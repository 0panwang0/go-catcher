// 任务状态并发读写：te.job 指针必须在 te.mu 内访问。
//
// 背景（2026-09-11 评审 P1-1）：pipeline 启动时在 te.mu 内写 te.job，
// 而 /status 每 900ms 轮询 snapshot、后台还会周期性 collectPersisted、
// 暂停收尾走 finishInterrupt —— 这三处原先都在锁外裸读 te.job。
// 指针字段的读写同样受 -race 管辖，这是一个真实的 data race。
//
// 这个用例不依赖真实下载：直接把"持锁写 / 并发读"这两个动作放大到高频，
// 让 race detector 有机会命中。修复前跑 -race 必报 DATA RACE。
package core

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTaskJobPointerLocked(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	te := &taskEntry{rt: testStd, st: taskState{
		id: "trace", filename: "v.ts", saveDir: dir,
		finalPath: filepath.Join(dir, "v.ts"), started: time.Now(),
	}}
	testStd.tasksMu.Lock()
	testStd.tasks[te.st.id] = te
	testStd.tasksMu.Unlock()

	const iters = 2000
	var wg sync.WaitGroup

	// 写方：等价于 pipeline.go 里 `te.mu.Lock(); te.job = job; te.mu.Unlock()`。
	// 真实代码只写一次，这里反复替换以放大竞争窗口。
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				te.mu.Lock()
				te.job = &dlJob{rt: testStd, id: te.st.id}
				te.mu.Unlock()
			}
		}()
	}

	// 读方：/status 的 snapshot + 后台持久化的 collectPersisted（都会碰 te.job）。
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				if s := snapshot(te); s.id != "trace" {
					t.Errorf("snapshot 返回了错误的 id: %q", s.id)
					return
				}
				_ = testStd.collectPersisted()
			}
		}()
	}

	wg.Wait()

	// 顺带确认 jobRef 本身可用（暂停收尾路径 finishInterrupt 就是这么取的）。
	if job := te.jobRef(); job == nil || job.id != "trace" {
		t.Fatalf("jobRef 应返回刚写入的 job，得到 %v", job)
	}
}
