// 管线最外层兜底（P1-2 第三层）的回归测试。
//
// runDiskPipeline 由 `go` 启动、没有返回值：漏网的 panic 就是整个进程消失——
// GUI 没了，其它正在下载/录制的任务一起没。这一层因此必须可测。
package core

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// newGuardTestTask 造一个在跑的任务条目（runGuarded 只依赖 te.st / te.rt）。
func newGuardTestTask(id string) *taskEntry {
	return &taskEntry{rt: testStd, st: taskState{id: id, running: true, stage: "下载分片中"}}
}

// TestRunGuardedRecoversPanic 管线内 panic → 该任务标记失败，进程继续活着。
func TestRunGuardedRecoversPanic(t *testing.T) {
	saveRestoreState(t)
	te := newGuardTestTask("tpanic")
	testStd.tasksMu.Lock()
	testStd.tasks[te.st.id] = te
	testStd.tasksMu.Unlock()

	runGuarded(te, func() { panic("boom") })

	// 能走到下一行本身就是核心断言：进程没被 panic 带走。
	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.done {
		t.Fatal("panic 后任务没进终态：并发槽不会释放，任务永远卡在「下载中」")
	}
	if st.running || st.queued {
		t.Fatalf("终态仍在运行: running=%v queued=%v", st.running, st.queued)
	}
	if !strings.Contains(st.errorMsg, "已拦截") {
		t.Fatalf("errorMsg=%q 未说明这是被拦截的内部错误", st.errorMsg)
	}
}

// TestRunGuardedKeepsFinishedState 已收尾的任务不再被改写：
// panic 可能发生在收尾成功之后，把一次成功保存改成「失败」和崩溃一样糟
// （用户会以为文件没了，其实好好地躺在磁盘上）。
func TestRunGuardedKeepsFinishedState(t *testing.T) {
	saveRestoreState(t)
	te := newGuardTestTask("tdone")
	te.mu.Lock()
	te.st.done = true
	te.st.running = false
	te.st.stage = "已保存"
	te.mu.Unlock()

	runGuarded(te, func() { panic("late boom") })

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.stage != "已保存" || st.errorMsg != "" {
		t.Fatalf("已收尾任务被改写: stage=%q errorMsg=%q", st.stage, st.errorMsg)
	}
}

// TestRunGuardedRunsFn 正常路径原样透传（不改变既有行为）。
func TestRunGuardedRunsFn(t *testing.T) {
	ran := false
	runGuarded(newGuardTestTask("tok"), func() { ran = true })
	if !ran {
		t.Fatal("runGuarded 没有执行传入的 fn")
	}
}

// TestPipelineLaunchIsGuarded 钉住两个生产入口都走 runGuarded。
// 少了这道扫描，谁把 `go runDiskPipeline(te)` 写回去都能全绿通过——
// 只有真遇到畸形输入崩溃时才暴露，而那时进程已经没了。
func TestPipelineLaunchIsGuarded(t *testing.T) {
	for _, f := range []string{"server.go", "handlers.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		if bytes.Contains(src, []byte("go runDiskPipeline(")) {
			t.Errorf("%s 里仍有裸 go runDiskPipeline：panic 会带走整个进程", f)
		}
		if !bytes.Contains(src, []byte("go runGuarded(te, func() { runDiskPipeline(te) })")) {
			t.Errorf("%s 未通过 runGuarded 启动管线", f)
		}
	}
}
