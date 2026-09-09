// 失败重试 E2E 回归：failTask 曾清空 finalPath，重试的 pipeline 拿空路径
// 拼出相对 ".part"——续传数据全部写到工作目录下的游离文件、收尾 moveFile
// 必失败，用户表现为「点重试永远不行，只有暂停后重来才行」（暂停路径
// finishInterrupt 从不清 finalPath，所以暂停续传一直正常）。
// 回归点：失败保留 finalPath；旧状态文件遗留的空 finalPath 在 pipeline 内
// 按 filename+saveDir 自愈。
package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitTaskState 轮询等待任务到达目标状态（f 返回 true），超时报当前状态。
func waitTaskState(t *testing.T, te *taskEntry, f func(taskState) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		te.mu.Lock()
		st := te.st
		te.mu.Unlock()
		if f(st) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待任务%s超时: %+v", what, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitLimiterDrained 等并发槽全部归还。done 状态置位后 pipeline goroutine 只剩
// 延迟 release 一步（defer 里读全局 limiter），槽归零即证明 goroutine 已完全
// 退出——之后 t.Cleanup 恢复全局 limiter 才不会与该读构成数据竞争。
func waitLimiterDrained(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if active, _ := limiter.current(); active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待并发槽归还超时（pipeline goroutine 未退出）")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRetryAfterFailureKeepsFinalPath(t *testing.T) {
	saveRestoreState(t)
	oldLimiter, oldConc, oldRetries := limiter, concurrency, maxRetries
	limiter = newResizableSem(2)
	concurrency, maxRetries = 4, 1 // 单次尝试即失败，缩短测试耗时
	t.Cleanup(func() { limiter, concurrency, maxRetries = oldLimiter, oldConc, oldRetries })

	var mu sync.Mutex
	broken := true // 首片成功（留断点），其余 404；修复后全部 200
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n"+
			"#EXTINF:6.0,\nseg/0\n#EXTINF:6.0,\nseg/1\n"+
			"#EXTINF:6.0,\nseg/2\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b := broken
		mu.Unlock()
		if b && r.URL.Path != "/seg/0" {
			http.Error(w, "boom", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "SEG-%s", strings.TrimPrefix(r.URL.Path, "/seg/"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	final := filepath.Join(dir, "vod.ts")
	te := &taskEntry{st: taskState{
		id: "tr", queued: true, stage: "排队中", started: time.Now(),
		m3u8URL: srv.URL + "/vod.m3u8", filename: "vod.ts", saveDir: dir, finalPath: final,
	}}
	tasks[te.st.id] = te

	// 第一轮：分片 1/2 失败 → 任务失败，但首片已写（断点 = 1）
	go runDiskPipeline(te)
	waitTaskState(t, te, func(s taskState) bool { return s.done && s.errorMsg != "" }, "失败")
	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.finalPath != final {
		t.Fatalf("失败后 finalPath=%q want %q（重试靠它定位 .part）", st.finalPath, final)
	}
	part, err := os.ReadFile(final + ".part")
	if err != nil || string(part) != "SEG-0" {
		t.Fatalf("失败后 .part 内容异常: %q err=%v", part, err)
	}

	// 模拟旧版状态文件（失败时 finalPath 被清空）+ 服务恢复，然后重试
	te.mu.Lock()
	te.st.finalPath = ""
	te.mu.Unlock()
	mu.Lock()
	broken = false
	mu.Unlock()

	w := httptest.NewRecorder()
	handleResume(w, httptest.NewRequest("GET", "/resume?id=tr", nil))
	if w.Code != 200 {
		t.Fatalf("resume HTTP %d: %s", w.Code, w.Body.String())
	}
	waitTaskState(t, te, func(s taskState) bool { return s.done && s.errorMsg == "" }, "重试完成")

	te.mu.Lock()
	st = te.st
	te.mu.Unlock()
	if st.finalPath != final {
		t.Fatalf("重试后 finalPath=%q want %q（空 finalPath 应自愈拼回）", st.finalPath, final)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("读成品: %v", err)
	}
	if want := "SEG-0SEG-1SEG-2"; string(data) != want {
		t.Fatalf("成品=%q want %q（断点续传不应重复/丢失）", data, want)
	}
	if _, err := os.Stat(final + ".part"); err == nil {
		t.Fatal("收尾后 .part 仍存在（应已改名为成品）")
	}
	if _, err := os.Stat(".part"); err == nil {
		t.Fatal("工作目录出现游离 .part（finalPath 为空时的病征）")
	}

	// 排空并发槽：两轮 pipeline goroutine 连延迟 release 都执行完后再进
	// cleanup，避免其读全局 limiter 与 cleanup 恢复全局变量竞争。
	waitLimiterDrained(t)
}
