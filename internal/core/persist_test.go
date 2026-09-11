// 任务状态持久化测试：saveState/loadState 往返、运行中→暂停转换、规范化状态字节恢复、取消清理。
package core

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// saveRestoreState 固定状态文件路径并保存/恢复 tasks 与 seqID 全局。
// 去抖间隔调短并在清理前等待落盘 goroutine 结束，防止它泄漏到后续测试
// （拿着已恢复的全局 statePath/tasks 继续写，构成数据竞争）。
func saveRestoreState(t *testing.T) string {
	t.Helper()
	oldPath, oldTasks, oldSeq, oldDelay := testStd.statePath, testStd.tasks, testStd.seqID, testStd.stateSaveDelay
	testStd.stateSaveDelay = 5 * time.Millisecond
	t.Cleanup(func() {
		waitStateIdle(t)
		testStd.statePath, testStd.tasks, testStd.seqID, testStd.stateSaveDelay = oldPath, oldTasks, oldSeq, oldDelay
		testStd.stateDirty, testStd.stateSaving = false, false
	})
	testStd.getStatePath() // 触发 once，之后直接覆盖 testStd.statePath
	dir := t.TempDir()
	testStd.statePath = filepath.Join(dir, "gocatcher_state.json")
	testStd.tasks = map[string]*taskEntry{}
	testStd.seqID = 0
	return testStd.statePath
}

// waitStateIdle 轮询等待去抖保存 goroutine 完全结束（无脏标记、无在途保存）。
func waitStateIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		testStd.stateMu.Lock()
		idle := !testStd.stateDirty && !testStd.stateSaving
		testStd.stateMu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("等待状态落盘超时")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSaveLoadRoundTrip 任务完整往返：done 任务原样恢复（含规范化状态字节包），
// running/queued 残留转「已暂停」，seqID 推进避免撞号。
func TestSaveLoadRoundTrip(t *testing.T) {
	p := saveRestoreState(t)

	// normState 对持久化层是不透明字节包：schema 归容器实现自有测试
	// （internal/fmp4/normstate_test.go），这里只验证逐字节保真。
	// 约束仅为合法 JSON（persist 信封以 json.RawMessage 携带），
	// 内容结构 core 一无所知——用未知字段 + 嵌套值模拟任意容器格式。
	nsBytes := []byte(`{"opaque":{"baseline":[100,9000],"binary":[0,255,128]}}`)

	now := time.Now()
	testStd.tasks["t1"] = &taskEntry{rt: testStd, st: taskState{
		id: "t1", done: true, finalPath: filepath.Join(t.TempDir(), "a.mp4"),
		filename: "a.mp4", m3u8URL: "https://x/a.m3u8", stage: "已保存",
		started: now, finished: now, containerID: "fmp4",
		normState: nsBytes,
	}}
	testStd.tasks["t2"] = &taskEntry{rt: testStd, st: taskState{
		id: "t2", running: true, stage: "下载分片中", filename: "b.ts",
		m3u8URL: "https://x/b.m3u8", started: now,
	}}
	testStd.tasks["t3"] = &taskEntry{rt: testStd, st: taskState{
		id: "t3", queued: true, stage: "排队中", filename: "c.ts",
		m3u8URL: "https://x/c.m3u8", started: now,
	}}

	testStd.saveState()

	// 模拟进程重启：清空注册表后从磁盘恢复
	testStd.tasks = map[string]*taskEntry{}
	testStd.seqID = 0
	testStd.loadState()

	if len(testStd.tasks) != 3 {
		t.Fatalf("恢复任务数=%d want 3", len(testStd.tasks))
	}

	t1 := testStd.tasks["t1"]
	if !t1.st.done || t1.st.containerID != "fmp4" || t1.st.stage != "已保存" {
		t.Fatalf("t1 状态损坏: %+v", t1.st)
	}
	// 验证 normState 字节包内容保真（持久化层不解析内容，仅可能重排空白）
	if !jsonEqual(t1.st.normState, nsBytes) {
		t.Fatalf("normState 字节包往返失真: %s want %s", t1.st.normState, nsBytes)
	}

	for _, id := range []string{"t2", "t3"} {
		te := testStd.tasks[id]
		if !te.st.paused || te.st.running || te.st.queued || te.st.stage != "已暂停" {
			t.Fatalf("%s 残留应转暂停: %+v", id, te.st)
		}
		if !te.st.resumable() {
			t.Fatalf("%s 恢复后应可续传", id)
		}
	}

	if testStd.seqID != 3 {
		t.Fatalf("testStd.seqID=%d want 3（推进避免撞号）", testStd.seqID)
	}
	_ = p
}

// TestLoadStateCanceledClearsFinalPath 已取消任务清空 finalPath（防御旧版本状态残留）。
func TestLoadStateCanceledClearsFinalPath(t *testing.T) {
	p := saveRestoreState(t)
	os.WriteFile(p, []byte(`{"version":1,"tasks":[{"id":"t1","canceled":true,"done":true,"finalPath":"C:\\old\\x.mp4"}]}`), 0644)

	testStd.loadState()

	t1, ok := testStd.tasks["t1"]
	if !ok {
		t.Fatal("t1 未恢复")
	}
	if t1.st.finalPath != "" {
		t.Fatalf("canceled 任务 finalPath=%q 应被清空", t1.st.finalPath)
	}
}

// TestLoadStateSkipsMissingFile 状态文件不存在时不报错、不产生任务。
func TestLoadStateSkipsMissingFile(t *testing.T) {
	saveRestoreState(t)
	testStd.loadState() // 无文件，静默返回
	if len(testStd.tasks) != 0 {
		t.Fatalf("无状态文件应无任务, got %d", len(testStd.tasks))
	}
}

// TestLoadStateGarbageFile 损坏 JSON 忽略并保持空注册表。
func TestLoadStateGarbageFile(t *testing.T) {
	p := saveRestoreState(t)
	os.WriteFile(p, []byte("{broken"), 0644)
	testStd.loadState()
	if len(testStd.tasks) != 0 {
		t.Fatalf("损坏状态文件应被忽略, got %d", len(testStd.tasks))
	}
}

// TestCollectPersistedSorted 落盘列表按 ID 稳定排序（避免 map 遍历顺序污染文件内容）。
func TestCollectPersistedSorted(t *testing.T) {
	old := testStd.tasks
	t.Cleanup(func() { testStd.tasks = old })
	testStd.tasks = map[string]*taskEntry{
		"t3": {st: taskState{id: "t3", done: true}},
		"t1": {st: taskState{id: "t1", done: true}},
		"t2": {st: taskState{id: "t2", paused: true}},
	}
	got := testStd.collectPersisted()
	if len(got) != 3 || got[0].ID != "t1" || got[1].ID != "t2" || got[2].ID != "t3" {
		t.Fatalf("排序错误: %v", idsOfPT(got))
	}
}

func idsOfPT(list []persistedTask) []string {
	out := make([]string, len(list))
	for i, pt := range list {
		out[i] = pt.ID
	}
	return out
}

// jsonEqual 比较两段 JSON 字节内容是否等价（忽略空白差异）：
// 持久化信封以 MarshalIndent 写出会重排内嵌 RawMessage 的缩进。
func jsonEqual(a, b []byte) bool {
	var ca, cb bytes.Buffer
	if err := json.Compact(&ca, a); err != nil {
		return false
	}
	if err := json.Compact(&cb, b); err != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// TestLoadStateRejectsFutureVersion 未来版本的状态文件必须整体跳过恢复：
// 字段语义未知，硬解析会得到半残状态，表现为"任务凭空消失/字段错乱"。
func TestLoadStateRejectsFutureVersion(t *testing.T) {
	p := saveRestoreState(t)
	os.WriteFile(p, []byte(`{"version":99,"tasks":[{"id":"t1","done":true,"filename":"a.ts"}]}`), 0644)

	testStd.loadState()

	if len(testStd.tasks) != 0 {
		t.Fatalf("未来版本应跳过恢复, got %d 个任务", len(testStd.tasks))
	}
}

// TestLoadStateAcceptsLegacyVersion 无 version 字段（0）的旧文件按当前结构尽力解析。
func TestLoadStateAcceptsLegacyVersion(t *testing.T) {
	p := saveRestoreState(t)
	os.WriteFile(p, []byte(`{"tasks":[{"id":"t7","done":true,"filename":"old.ts","stage":"已保存"}]}`), 0644)

	testStd.loadState()

	if _, ok := testStd.tasks["t7"]; !ok {
		t.Fatal("旧版（无 version）状态文件应仍可恢复")
	}
}

// TestPersistHealthRecordsFailure 落盘失败要能被 /status 看到（GUI 无控制台）。
func TestPersistHealthRecordsFailure(t *testing.T) {
	p := saveRestoreState(t)
	// 把状态路径指到一个不可能写入的位置（父级是文件而非目录）
	blocker := filepath.Join(filepath.Dir(p), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	testStd.statePath = filepath.Join(blocker, "nested", "gocatcher_state.json")

	testStd.saveState()
	if d := testStd.persistDTOOf(); d.OK || d.Error == "" {
		t.Fatalf("落盘失败后 persist 状态应为不健康: %+v", d)
	}

	testStd.statePath = p
	testStd.saveState()
	if d := testStd.persistDTOOf(); !d.OK {
		t.Fatalf("落盘成功后 persist 状态应恢复健康: %+v", d)
	}
}
