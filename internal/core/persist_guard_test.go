// 状态文件的"不许静默覆盖"与"落盘必须串行"（A-1 / A-2）。
//
// 缺陷形态（两段拼起来才成灾）：
// ① loadState 对「读失败 / JSON 损坏 / 版本过高」三种情况一律**静默 return**，
// 不置任何标记 —— 此时 r.tasks 是"一个都没恢复"的空表；
// ② Engine.Stop 无条件 saveState（"绕过 debounce，同步落盘一次，保证断点不丢"）。
// 于是用户历史被一份空表**永久覆盖**。GUI 是 windowsgui 子系统、没有控制台，
// 那几行 Printf 没人看得到：**数据丢了但日志正常**。
// 讽刺的是 persist.go 的注释恰好写着"宁可放弃恢复也不要让用户看到任务凭空消失"
// —— 它只做了前半（这次看不见），没做后半（下次也看不见）。
//
// 另一段（A-2）：saveState 有两个互不认识的调用者（debounce goroutine 与 Stop），
// 而 writeFileAtomic 的临时名是**固定的** `<path>.tmp` ⇒ 两个写者会互踩，落成
// 非法 JSON —— 而非法 JSON 正好触发①，接成同一个后果。
package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestLoadStateUnknownVersionDoesNotWipeHistory（A-1 核心）：
// 文件是好的、只是当前程序不认识（版本过高）—— 既不许改名挪走，也不许覆盖。
//
// 处置必须与"文件坏了"分开：坏的可以留档后继续，好的必须原地不动。合成一条
// "载入失败就留档"会把用户降级期间的历史挪到 .corrupt 里，升回新版本也用不上。
func TestLoadStateUnknownVersionDoesNotWipeHistory(t *testing.T) {
	p := saveRestoreState(t)
	orig := []byte(`{"version":99,"tasks":[{"id":"t1","done":true,"filename":"a.ts"}]}`)
	if err := os.WriteFile(p, orig, 0644); err != nil {
		t.Fatal(err)
	}

	testStd.loadState()

	// 1) 原文件必须**原样**留在原处（既没被挪走、也没被改写）
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("原状态文件被挪走或删了（那是把好文件当垃圾处理）: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatalf("原状态文件被改写:\n got=%s\nwant=%s", got, orig)
	}
	if n := len(globCorrupt(t, p)); n != 0 {
		t.Fatalf("好的文件不该被留档为 .corrupt（留了 %d 份）", n)
	}

	// 2) 模拟 Engine.Stop 的无条件 saveState：必须被拦住，一个字都不许写
	testStd.tasks["tNEW"] = &taskEntry{rt: testStd, st: taskState{id: "tNEW", done: true, filename: "new.ts", stage: "已保存"}}
	testStd.saveState()
	got2, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got2, orig) {
		t.Fatalf("载入失败后仍写了盘（历史被空表覆盖）: err=%v content=%s", err, got2)
	}

	// 3) 必须留下可被 /status 看到的不健康记录（GUI 无控制台，日志等于没写）
	if d := testStd.persistDTOOf(); d.OK || d.Error == "" {
		t.Fatalf("未载入历史时 /status 应报不健康，实际: %+v", d)
	}
}

// TestLoadStateGarbageIsQuarantinedThenWritable（A-1 的"另一条腿"）：
// 文件确实坏了 ⇒ 留档（还能人工抢救），主路径空出来让程序继续工作。
// 只有"拦住覆盖"是不够的：那会让用户在坏文件面前**永远存不了新进度**。
func TestLoadStateGarbageIsQuarantinedThenWritable(t *testing.T) {
	p := saveRestoreState(t)
	broken := []byte("{broken")
	if err := os.WriteFile(p, broken, 0644); err != nil {
		t.Fatal(err)
	}

	testStd.loadState()

	// 主路径应已空出（可以正常写新状态）
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("坏文件应被改名留档，主路径该空出来: %v", err)
	}
	// 留档必须存在且内容一字不差
	corrupt := globCorrupt(t, p)
	if len(corrupt) != 1 {
		t.Fatalf("应留下恰好一份 .corrupt 留档，实际 %v", corrupt)
	}
	if b, _ := os.ReadFile(corrupt[0]); !bytes.Equal(b, broken) {
		t.Fatalf("留档内容被改动（那不是留档）: %s", b)
	}

	// 之后必须真的能落盘（不能一直卡在"拒绝覆盖"）
	testStd.tasks["t1"] = &taskEntry{rt: testStd, st: taskState{id: "t1", done: true, filename: "a.ts", stage: "已保存"}}
	testStd.saveState()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("留档后应能正常落盘: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil || len(sf.Tasks) != 1 {
		t.Fatalf("留档后的状态文件不合法: err=%v content=%s", err, data)
	}
}

// globCorrupt 列出 p 的 .corrupt 留档。
func globCorrupt(t *testing.T, p string) []string {
	t.Helper()
	m, err := filepath.Glob(p + ".*.corrupt")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestSaveStateHoldsMutex（A-2 结构守卫）：
// 固定临时名要求同路径**单写者**。这条钉住"saveState 全程持 stateSaveMu"——
// 行为测试（下一条）只能概率性抓到互踩，结构守卫才能确定性地拦住"有人把锁去掉"。
func TestSaveStateHoldsMutex(t *testing.T) {
	body := strings.Join(sourceFuncBody(t, "persist.go", "func (r *Runtime) saveState()"), "\n")
	if !strings.Contains(body, "stateSaveMu.Lock()") {
		t.Error("saveState 没有持 stateSaveMu —— debounce 与 Stop 两个写者会互踩出非法 JSON")
	}
	if !strings.Contains(body, "stateOverwriteBlocked") {
		t.Error("saveState 没有检查 stateOverwriteBlocked —— 载入失败后会把历史覆盖成空表")
	}
}

// TestSaveStateConcurrentStaysValid（A-2 行为）：
// 并发调用 saveState，最终文件必须仍是**完整合法**的 JSON。
// 固定临时名 + 无互斥时，写者会互相截断/覆盖 ⇒ 解析失败 ⇒ 下次启动走"空表"。
func TestSaveStateConcurrentStaysValid(t *testing.T) {
	p := saveRestoreState(t)

	const nTasks = 200
	for i := 0; i < nTasks; i++ {
		id := fmt.Sprintf("t%03d", i)
		testStd.tasks[id] = &taskEntry{rt: testStd, st: taskState{
			id: id, done: true, filename: id + ".ts", stage: "已保存", m3u8URL: "https://x/" + id + ".m3u8",
		}}
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				testStd.saveState()
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("状态文件读不出: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("并发落盘产出非法 JSON（两个写者互踩了临时文件）: %v", err)
	}
	if len(sf.Tasks) != nTasks {
		t.Fatalf("任务数=%d want %d（内容被半份覆盖）", len(sf.Tasks), nTasks)
	}
}
