// HTTP 控制端点测试：取消任务在各状态下的收尾路径。
package core

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestHandleCancelFailedTaskDropsBitmap 失败任务转「取消」必须连位图一起删。
//
// 回归（P0-5）：这一支原先只 os.Remove(part)，位图留下 ⇒ 用户重新下载同一个视频
// 时会认领同一个文件名（uniquePath 只看 .part 在不在），新建的 0 字节 .part 配上
// 这个残留位图，被标记「已完成」的片就永远不会下载 ⇒ 成品中间是空洞、
// downloadDirect 却返回 nil、界面显示「已保存」。同一个函数里「任务还在跑时取消」
// 那一支一直是配对写的（见 TestHandleCancelPausedTask 里删掉的那对），只有这一支漏了。
//
// 判据必须钉住**位图也没了**：只钉 .part 消失对这条缺陷完全无感（原来的代码就
// 通过了任何"只查 .part"的断言）。顺带钉 .meta.tmp —— 这正是"必须走
// removeChunkMeta 而不是裸删"的理由。
func TestHandleCancelFailedTaskDropsBitmap(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "bad.ts")
	part, meta := writePart(t, final)
	tmp := meta + ".tmp"
	if err := os.WriteFile(tmp, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	te := &taskEntry{rt: testStd, st: taskState{
		id: "tf", done: true, stage: "失败", errorMsg: "HTTP 500",
		filename: "bad.ts", saveDir: dir, finalPath: final,
	}}
	testStd.tasks[te.st.id] = te

	callCancel(t, "tf")

	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("取消后 .part 应删除")
	}
	if _, err := os.Stat(meta); !os.IsNotExist(err) {
		t.Fatal("取消后 .part.meta 必须一并删除：残留位图会让「重新下载同一个视频」悄悄跳片、成品带洞却报成功")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("取消后 .part.meta.tmp 也要清（这就是位图删除必须走 removeChunkMeta 的原因）")
	}
	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.canceled || st.errorMsg != "" {
		t.Fatalf("失败任务未转取消: %+v", st)
	}
}

// TestResumeTreatsQueuedAsBusy 已受理但还在排队（running=false / queued=true）的
// 任务必须被 /resume 当成「忙」。
//
// 回归（P2-13）：守卫原先只判 running，而 pipeline 在等并发槽期间写的正是
// running=false / queued=true（pipeline.go 进 limiter.acquire 之前）⇒ 连发两次
// /resume 会各起一条 pipeline：两条各自 te.job = job（后者覆盖前者）、两个
// streamWriter 以 O_APPEND 写同一个 .part ⇒ 分片交错；te.cancel 也被覆盖，此后
// /pause 只能取消其中一条。槽满时（默认 MaxConcurrent=3）这个窗口长达整个排队时长，
// 不是微秒级竞态。同函数的直播分支判的是 running||queued —— 这条用例把点播分支
// 也钉到同一口径。
func TestResumeTreatsQueuedAsBusy(t *testing.T) {
	saveRestoreState(t)
	cases := []struct {
		name    string
		running bool
		queued  bool
	}{
		{"正在跑", true, false},
		{"已受理待排队", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := "tb-" + c.name
			te := &taskEntry{rt: testStd, st: taskState{
				id: id, stage: "排队中", errorMsg: "留着别动",
				filename: "v.ts", saveDir: t.TempDir(),
				running: c.running, queued: c.queued,
			}}
			testStd.tasks[id] = te
			t.Cleanup(func() { delete(testStd.tasks, id) })

			w := httptest.NewRecorder()
			testEngine().handleResume(w, httptest.NewRequest("GET", "/resume?id="+id, nil))
			if w.Code != 200 {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"running":true`) {
				t.Fatalf("忙任务应回 running:true，实际 %s", w.Body.String())
			}
			te.mu.Lock()
			st := te.st
			te.mu.Unlock()
			if st.stage != "排队中" || st.errorMsg != "留着别动" {
				t.Fatalf("忙任务被重新受理了（会起第二条 pipeline）: %+v", st)
			}
			// 忙任务的状态必须一点没动：done/canceled 不能被重置、queued 不能被清掉
			// （queued 一旦被清，下一次 /resume 就又放行了）。
			if st.queued != c.queued || st.done || st.canceled {
				t.Fatalf("忙任务的状态被改动了: queued=%v(want %v) done=%v canceled=%v",
					st.queued, c.queued, st.done, st.canceled)
			}
		})
	}
}
