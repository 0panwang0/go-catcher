// 直播语义（学徒 2026-09-15 定）：直播任务只有「停止」与「取消」两态，
// 没有暂停/继续/重试——暂停期间的流已经从列表里滚走，续录只会让产物在
// 时间轴上留一个空洞。
//
// 本文件同时锁住复核时实测到的两个历史缺陷：
//   - P1-4：taskState.live 全链路无人赋值 → /status 的 live 恒 false，
//     前端 11 处直播分支不可达（「录制中」徽章、无限进度条、录制时长），
//     而 B0 的全部整改判据都是它；
//   - P2-2：直播收尾把 segDone 归零（segTot 恒为 0），界面上"已录制 0 片"。
package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newStoppableLiveTask 起一条真实直播任务：源列表固定 2 片且永不 ENDLIST，
// 流水线录完这 2 片后停在轮询里，等测试主动停止。
// 返回任务与已录分片的期望内容。
func newStoppableLiveTask(t *testing.T, id string) (*taskEntry, string) {
	t.Helper()
	saveRestoreState(t)
	oldLimiter := testStd.limiter
	oldInterval, oldEmpty := testStd.livePollInterval, testStd.liveMaxEmptyPolls
	testStd.limiter = newResizableSem(1)
	testStd.livePollInterval = 5 * time.Millisecond
	testStd.liveMaxEmptyPolls = 1 << 20 // 不自动判定"直播结束"，由测试主动停止
	t.Cleanup(func() {
		testStd.limiter = oldLimiter
		testStd.livePollInterval, testStd.liveMaxEmptyPolls = oldInterval, oldEmpty
	})

	segBody := map[string]string{"/seg/0.ts": "SEG-0", "/seg/1.ts": "SEG-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n")
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := segBody[r.URL.Path]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	te := &taskEntry{rt: testStd, st: taskState{
		id: id, queued: true, stage: "排队中", started: time.Now(),
		m3u8URL: srv.URL + "/live.m3u8", filename: id + ".ts", saveDir: dir,
		finalPath: filepath.Join(dir, id+".ts"),
	}}
	testStd.tasks[te.st.id] = te
	go runDiskPipeline(te)
	return te, "SEG-0SEG-1"
}

// stopTaskViaHandler 直接调用 /stop 端点（不经 HTTP 栈）。
func stopTaskViaHandler(t *testing.T, te *taskEntry) {
	t.Helper()
	rec := httptest.NewRecorder()
	testEngine().handleStop(rec, httptest.NewRequest(http.MethodGet, "/stop?id="+te.st.id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/stop 返回 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestLiveFlagExposedInStatus P1-4：直播任务必须对外暴露 live=true。
// 修复前 taskState.live 从无写入点，/status 恒 false。
func TestLiveFlagExposedInStatus(t *testing.T) {
	te, _ := newStoppableLiveTask(t, "liveflag")
	waitTaskState(t, te, func(s taskState) bool { return s.segDone >= 2 }, "录到 2 片")

	st := snapshot(te)
	if !st.live {
		t.Fatal("直播任务 live=false：前端所有直播分支不可达（P1-4）")
	}
	if dto := toTaskStateDTO(st); !dto.Live {
		t.Fatal("DTO.live=false：/status 未暴露直播标记")
	}
	if st.segTot != 0 {
		t.Fatalf("直播 segTot=%d want 0（列表无限增长，进度按录制时长展示）", st.segTot)
	}

	stopTaskViaHandler(t, te)
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "停止收尾")
	waitLimiterDrained(t)
	if got := snapshot(te).segDone; got != 2 {
		t.Fatalf("收尾后 segDone=%d want 2（P2-2：直播进度被清零）", got)
	}
}

// TestStopLiveFinalizesFile 用户对着"正在录制"的直播点「停止」：已录部分收尾成
// 正式文件，并按"用户主动结束"记（学徒 2026-09-15 定：他自己叫停的不算故障，
// 界面显示「已完成」并把谁结束的说清楚）。
//
// 三条必须同时成立，缺一条就会骗人：
//   - interrupted=false：不是故障态；
//   - stage 写明"用户停止录制"：光说"已保存"分不清是录完了还是被叫停了；
//   - errorMsg 留空：那不是错误，留着会被前端算进「失败」统计。
//
// 「已中断」留给非用户意愿的中断：见 TestSalvageInterruptedLive* 与
// TestStopIdleLiveFinalizesExistingPart（任务早就停了，用户点的「停止」只是保存）。
func TestStopLiveFinalizesFile(t *testing.T) {
	te, want := newStoppableLiveTask(t, "livestop")
	waitTaskState(t, te, func(s taskState) bool { return s.segDone >= 2 }, "录到 2 片")
	stopTaskViaHandler(t, te)
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "停止收尾")
	waitLimiterDrained(t)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.interrupted {
		t.Fatalf("用户主动停止被标成中断态（stage=%q）：「中断」是非用户意愿的语义", st.stage)
	}
	if !strings.Contains(st.stage, "用户停止") {
		t.Fatalf("stage=%q want 含「用户停止录制」——必须说清是谁结束的", st.stage)
	}
	if st.errorMsg != "" {
		t.Fatalf("errorMsg=%q want 空：用户停止不是错误，留着会被算进「失败」统计", st.errorMsg)
	}
	if st.paused {
		t.Fatal("停止后 paused=true：任务又变回了可恢复态")
	}
	data, err := os.ReadFile(st.finalPath)
	if err != nil {
		t.Fatalf("成品文件不存在: %v", err)
	}
	if string(data) != want {
		t.Fatalf("成品内容=%q want %q", data, want)
	}
	if _, err := os.Stat(st.finalPath + ".part"); !os.IsNotExist(err) {
		t.Fatal("停止后残留 .part 半成品")
	}
}

// TestStopRejectsNonLive 点播任务不能用 /stop：它的"停"语义是暂停（保留断点可续），
// 混用会让用户以为点播也能"停一下再来"，而产物其实是半成品。
func TestStopRejectsNonLive(t *testing.T) {
	saveRestoreState(t)
	te := &taskEntry{rt: testStd, st: taskState{id: "vodstop", stage: "下载分片中", running: true}}
	testStd.tasks[te.st.id] = te

	rec := httptest.NewRecorder()
	testEngine().handleStop(rec, httptest.NewRequest(http.MethodGet, "/stop?id=vodstop", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("点播 /stop 返回 %d want 400", rec.Code)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "非直播") {
		t.Fatalf("错误文案不可行动: %s", msg)
	}
	te.mu.Lock()
	stage, intent := te.st.stage, te.intent
	te.mu.Unlock()
	if stage != "下载分片中" || intent != intentNone {
		t.Fatalf("被拒的 /stop 改动了任务: stage=%q intent=%d", stage, intent)
	}
}

// TestPauseRejectsLiveAndResumeFinalizesIt 直播任务的这两条控制入口语义不同：
//
//   - 「暂停」仍然是续录入口，必须拒绝（空档期的流已经从列表滚走）；
//   - 「继续」不再是拒绝，而是**收尾保存**（学徒 2026-09-20 定：直播点继续 =
//     直接完成任务）。分流放在后端，任何入口（GUI / CLI / 扩展）调 resume 都走它。
func TestPauseRejectsLiveAndResumeFinalizesIt(t *testing.T) {
	saveRestoreState(t)
	te := &taskEntry{rt: testStd, st: taskState{id: "liveblock", live: true, stage: "录制中", running: true}}
	testStd.tasks[te.st.id] = te
	e := testEngine()

	rec := httptest.NewRecorder()
	e.handlePause(rec, httptest.NewRequest(http.MethodGet, "/pause?id=liveblock", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("直播 /pause 返回 %d want 400", rec.Code)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "停止") {
		t.Fatalf("pause 错误文案应引导到「停止」: %s", msg)
	}
	te.mu.Lock()
	paused, intent := te.st.paused, te.intent
	te.mu.Unlock()
	if paused || intent != intentNone {
		t.Fatalf("被拒的 pause 改动了任务状态: paused=%v intent=%d", paused, intent)
	}

	rec = httptest.NewRecorder()
	e.handleResume(rec, httptest.NewRequest(http.MethodGet, "/resume?id=liveblock", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("直播 /resume 应分流到收尾（200），实际 %d: %s", rec.Code, rec.Body.String())
	}
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "直播 resume 收尾")
	te.mu.Lock()
	stage, errMsg := te.st.stage, te.st.errorMsg
	te.mu.Unlock()
	if errMsg != "" {
		t.Fatalf("没有内容可保存不是错误，errorMsg=%q", errMsg)
	}
	if !strings.Contains(stage, "无内容") {
		t.Fatalf("stage=%q 应为「无内容」终态（本用例里 .part 不存在）", stage)
	}
}

// TestStopOrPauseAllTasksSplitsByLive GUI 退出前按类型分流：
// 直播走「停止」（收尾成正式文件），点播走「暂停」（保留断点下次续）。
// 留一个"下次接着录"的直播半成品，只会制造一个带时间轴空洞的断档文件。
func TestStopOrPauseAllTasksSplitsByLive(t *testing.T) {
	saveRestoreState(t)
	liveCtx, liveCancel := context.WithCancel(context.Background())
	vodCtx, vodCancel := context.WithCancel(context.Background())
	defer vodCancel()

	live := &taskEntry{rt: testStd, st: taskState{id: "q1", live: true, running: true, stage: "录制中"}, cancel: liveCancel}
	vod := &taskEntry{rt: testStd, st: taskState{id: "q2", running: true, stage: "下载分片中"}, cancel: vodCancel}
	testStd.tasks[live.st.id], testStd.tasks[vod.st.id] = live, vod

	testStd.stopOrPauseAllTasks()

	live.mu.Lock()
	liveIntent, liveStage := live.intent, live.st.stage
	live.mu.Unlock()
	vod.mu.Lock()
	vodIntent, vodStage := vod.intent, vod.st.stage
	vod.mu.Unlock()

	if liveIntent != intentStop {
		t.Fatalf("直播任务 intent=%d want intentStop", liveIntent)
	}
	if vodIntent != intentPause {
		t.Fatalf("点播任务 intent=%d want intentPause", vodIntent)
	}
	if !strings.Contains(liveStage, "停止") || !strings.Contains(vodStage, "暂停") {
		t.Fatalf("stage 文案未分流: live=%q vod=%q", liveStage, vodStage)
	}
	if liveCtx.Err() == nil {
		t.Fatal("直播任务的取消函数未被调用")
	}
	if vodCtx.Err() == nil {
		t.Fatal("点播任务的取消函数未被调用（暂停同样要先中断下载）")
	}
}

// TestStopIdleLiveFinalizesExistingPart 直播任务失败后处于静止态（pipeline 已退出），
// 此时点「停止」仍要能保存已录部分——否则界面上会留一个点了没反应的按钮。
func TestStopIdleLiveFinalizesExistingPart(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "idle.ts")
	const want = "SEG-0SEG-1"
	if err := os.WriteFile(final+".part", []byte(want), 0644); err != nil {
		t.Fatal(err)
	}

	job := &dlJob{rt: testStd, live: true}
	// 账本成对给：2 片、12 字节（= 下面 want 的长度）。收尾判据要用字节数
	// 区分"只落了 init 段的空壳"与"真录到分片"，两个字段都必须设。
	job.setFlushed(2, int64(len(want)))
	te := &taskEntry{rt: testStd, job: job, st: taskState{
		id: "idlelive", live: true, stage: "失败", errorMsg: "下载分片失败: boom",
		filename: "idle.ts", saveDir: dir, finalPath: final, segDone: 2,
	}}
	testStd.tasks[te.st.id] = te

	rec := httptest.NewRecorder()
	testEngine().handleStop(rec, httptest.NewRequest(http.MethodGet, "/stop?id=idlelive", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/stop 返回 %d: %s", rec.Code, rec.Body.String())
	}
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "静止态停止收尾")

	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("成品文件不存在: %v", err)
	}
	if string(data) != want {
		t.Fatalf("成品内容=%q want %q", data, want)
	}
	if _, err := os.Stat(final + ".part"); !os.IsNotExist(err) {
		t.Fatal("停止后残留 .part")
	}
}

// TestLiveRestartWithPartFinalizesInsteadOfResuming 直播任务重启恢复时，只要盘上已
// 有内容就必须**收尾保存**，绝不进入 liveDownload 续录。
//
// 学徒 2026-09-20 定的语义：直播点「继续」= 直接完成任务。为什么不能接着录——
// 断点/程序退出期间的分片已经从滑动窗口滚走，接着录只能在产物时间轴上留一段空洞。
//
// 判据为什么不能只看 from>0（旧实现）：运行中不落盘时 from 恒为 0（P0-1），
// 守卫于是根本不触发，writeInitSegment 见 .part 非空就跳过写 init、
// streamDownload 用 O_APPEND 从**已有内容的末尾**续写 —— 产物变成两段录制拼接
// （探针实测 SEG0|SEG1|SEG2|SEG0|SEG1|SEG2|），而直播录像不可重录。
//
// 三个断言各钉一件事：
//   - 源站请求数 == 0：没有重新下载（不是"下完了才保存"）；
//   - 成品内容 == 原 .part：已录内容一字不差地保住了；
//   - 中断原因保留、interrupted 为真：不把中断改写成"已完成"（等于抹掉故障记录）。
func TestLiveRestartWithPartFinalizesInsteadOfResuming(t *testing.T) {
	saveRestoreState(t)
	oldLimiter := testStd.limiter
	testStd.limiter = newResizableSem(1)
	t.Cleanup(func() { testStd.limiter = oldLimiter })

	// 分片请求数必须为 0（核心：没有续录）；列表请求最多 1 次 ——
	// 判定 isLive 必须先拉一次播放列表（pl.hasEndList），那是识别直播所需的，
	// 不是"又去轮询录制"。两次以上说明真的进了 liveDownload 的轮询循环。
	var playlistHits, segHits int64
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&playlistHits, 1)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n")
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&segHits, 1)
		fmt.Fprint(w, "SEG")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	const want = "SEG-A|SEG-B|"
	dir := t.TempDir()
	final := filepath.Join(dir, "livebp.ts")
	if err := os.WriteFile(final+".part", []byte(want), 0644); err != nil {
		t.Fatal(err)
	}
	// 模拟"程序退出后重启恢复出来的直播任务"：live 已持久化、账本与 .part 一致。
	te := &taskEntry{rt: testStd, st: taskState{
		id: "livebp", queued: true, stage: "录制中断（程序异常退出）", started: time.Now(),
		m3u8URL: srv.URL + "/live.m3u8", filename: "livebp.ts", saveDir: dir,
		finalPath: final, live: true,
		segDone: 2, flushedBytes: int64(len(want)),
	}}
	testStd.tasks[te.st.id] = te
	go runDiskPipeline(te)
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "直播重启收尾")
	waitLimiterDrained(t)

	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("成品文件不存在（已录的直播内容被丢弃）: %v", err)
	}
	if string(data) != want {
		t.Fatalf("成品内容=%q want %q", data, want)
	}
	if n := atomic.LoadInt64(&segHits); n != 0 {
		t.Fatalf("直播重启恢复不应下载任何分片（实际 %d 次）：说明又去续录了，产物会变成两段录制拼接", n)
	}
	if n := atomic.LoadInt64(&playlistHits); n > 1 {
		t.Fatalf("播放列表被请求 %d 次（>1 说明进了 liveDownload 的轮询循环）", n)
	}
	te.mu.Lock()
	msg, stage, interrupted := te.st.errorMsg, te.st.stage, te.st.interrupted
	te.mu.Unlock()
	if msg == "" {
		t.Fatal("中断原因必须保留：把它改写成'已完成'等于抹掉故障记录")
	}
	if !interrupted {
		t.Fatalf("中断收尾必须标 interrupted（stage=%q），否则界面会把这次录制显示成完整录完", stage)
	}
	if _, err := os.Stat(final + ".part"); !os.IsNotExist(err) {
		t.Fatal("收尾后残留 .part")
	}
}

// TestLiveRestartWithBreakpointButNoPartStopsCleanly 带断点（from>0）但盘上没有
// .part：同样不能续录 —— 硬跑会从 from 开始往空文件里写，产物前 from 片直接缺失。
// 该形态走"无内容"终态：任务结束、不产出成品、不留错误提示（用户没做错什么）。
func TestLiveRestartWithBreakpointButNoPartStopsCleanly(t *testing.T) {
	saveRestoreState(t)
	oldLimiter := testStd.limiter
	testStd.limiter = newResizableSem(1)
	t.Cleanup(func() { testStd.limiter = oldLimiter })

	// 分片请求必须为 0；列表请求最多 1 次（判定 isLive 所需，见上一个用例）。
	var playlistHits, segHits int64
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&playlistHits, 1)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n")
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&segHits, 1)
		fmt.Fprint(w, "SEG")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	te := &taskEntry{rt: testStd, st: taskState{
		id: "livebp2", queued: true, stage: "已暂停", started: time.Now(),
		m3u8URL: srv.URL + "/live.m3u8", filename: "livebp2.ts", saveDir: dir,
		finalPath: filepath.Join(dir, "livebp2.ts"),
		live:      true,
		segDone:   2, // 有断点，但磁盘上什么都没有
	}}
	testStd.tasks[te.st.id] = te
	go runDiskPipeline(te)
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "无内容收尾")
	waitLimiterDrained(t)

	if n := atomic.LoadInt64(&segHits); n != 0 {
		t.Fatalf("不应下载任何分片（实际 %d 次）", n)
	}
	if n := atomic.LoadInt64(&playlistHits); n > 1 {
		t.Fatalf("播放列表被请求 %d 次（>1 说明进了轮询循环）", n)
	}
	te.mu.Lock()
	msg, stage, finalPath := te.st.errorMsg, te.st.stage, te.st.finalPath
	te.mu.Unlock()
	if msg != "" {
		t.Fatalf("没有内容可保存不是错误，errorMsg=%q", msg)
	}
	if !strings.Contains(stage, "无内容") {
		t.Fatalf("stage=%q 应说明没有内容可保存（用户才知道为什么没有文件）", stage)
	}
	if finalPath != "" {
		if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
			t.Fatal("无内容路径不该产出成品文件")
		}
	}
}
