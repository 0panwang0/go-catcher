// HTTP 控制端点测试：取消任务在各状态下的收尾路径。
package core

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// callCancel 调用 /cancel?id=<id>，断言 HTTP 200。
func callCancel(t *testing.T, id string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/cancel?id="+id, nil)
	testEngine().handleCancel(w, r)
	if w.Code != 200 {
		t.Fatalf("cancel HTTP %d: %s", w.Code, w.Body.String())
	}
}

// writePart 在 finalPath 旁生成 .part 半成品与 .meta 位图，返回两文件路径。
func writePart(t *testing.T, finalPath string) (part, meta string) {
	t.Helper()
	part = finalPath + ".part"
	meta = part + ".meta"
	if err := os.WriteFile(part, []byte("half"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(`{"total":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	return part, meta
}

// TestHandleCancelPausedTask 已暂停任务没有 worker goroutine（cancel 早已随
// 上一轮 pipeline 结束而失效），取消必须在请求线程直接终态化。
// 回归：曾出现点取消没反应、任务永远卡在「取消中」。
func TestHandleCancelPausedTask(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "video.ts")
	part, meta := writePart(t, final)

	// 真实暂停态：cancel func 存在但 ctx 已死（pipeline 退出时 defer 调用过）
	_, cancelFn := context.WithCancel(context.Background())
	cancelFn()

	te := &taskEntry{rt: testStd, cancel: cancelFn, st: taskState{
		id: "tp", paused: true, stage: "已暂停",
		filename: "video.ts", saveDir: dir, finalPath: final,
		segDone: 100, segTot: 200,
	}}
	testStd.tasks[te.st.id] = te

	callCancel(t, "tp")

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.canceled || !st.done || st.paused || st.stage != "已取消" || st.finalPath != "" {
		t.Fatalf("取消后状态异常: %+v", st)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal(".part 未删除")
	}
	if _, err := os.Stat(meta); !os.IsNotExist(err) {
		t.Fatal(".part.meta 未删除")
	}
}

// TestHandleCancelQueuedTask 排队等并发槽的任务（还没起 worker）：请求线程
// 直接终态化并清理半成品，不依赖 goroutine 收尾。
func TestHandleCancelQueuedTask(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "q.ts")
	part, _ := writePart(t, final)

	te := &taskEntry{rt: testStd, st: taskState{
		id: "tq", queued: true, stage: "排队中",
		filename: "q.ts", saveDir: dir, finalPath: final,
	}}
	testStd.tasks[te.st.id] = te

	callCancel(t, "tq")

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.canceled || !st.done || st.queued || st.stage != "已取消" {
		t.Fatalf("取消后状态异常: %+v", st)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal(".part 未删除")
	}
}

// TestHandleCancelRunningTaskDelegatesToWorker 运行中任务：请求线程只发
// cancel 信号不直接改状态，由 worker（pipeline goroutine）走 finishInterrupt。
func TestHandleCancelRunningTaskDelegatesToWorker(t *testing.T) {
	saveRestoreState(t)
	ctx, cancelFn := context.WithCancel(context.Background())
	te := &taskEntry{rt: testStd, cancel: cancelFn, st: taskState{id: "tr", running: true, stage: "下载分片中"}}
	testStd.tasks[te.st.id] = te

	workerDone := make(chan struct{})
	go func() { // 模拟 pipeline：等 ctx 取消后走标准收尾
		defer close(workerDone)
		<-ctx.Done()
		finishInterrupt(te)
	}()

	callCancel(t, "tr")
	<-workerDone

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.canceled || !st.done || st.stage != "已取消" {
		t.Fatalf("worker 收尾后状态异常: %+v", st)
	}
}

// TestHandleCancelDoneTask 已完成任务：幂等返回，不转取消、不碰成品文件。
func TestHandleCancelDoneTask(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "ok.ts")
	if err := os.WriteFile(final, []byte("done"), 0644); err != nil {
		t.Fatal(err)
	}

	te := &taskEntry{rt: testStd, st: taskState{
		id: "td", done: true, stage: "已保存",
		filename: "ok.ts", saveDir: dir, finalPath: final,
	}}
	testStd.tasks[te.st.id] = te

	callCancel(t, "td")

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.canceled || st.stage != "已保存" || st.finalPath != final {
		t.Fatalf("完成任务被误转取消: %+v", st)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("成品文件被误删: %v", err)
	}
}
