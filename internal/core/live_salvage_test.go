// 直播中断自动收尾（学徒 2026-09-15 定：直播中断肯定要自动收尾）。
//
// 直播录制因故障终止时，不得停在"半成品 + 等用户处理"的状态：已录部分要
// 自动收尾成正式文件，但**必须**在状态与文案上与"完整录完"可区分，并且如实
// 报告产物时间轴上的缺口——否则就是把"产物不完整"藏起来，正是本项目
// 反复出现的头号缺陷形态。
package core

import (
	"encoding/json"
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

// useFastLive 把直播轮询/重试参数调到测试尺度，并在结束时还原。
func useFastLive(t *testing.T, maxEmptyPolls int) {
	t.Helper()
	saveRestoreState(t)
	oldLimiter := testStd.limiter
	oldInterval, oldEmpty := testStd.livePollInterval, testStd.liveMaxEmptyPolls
	oldRetries := testStd.retryLimit.Load()
	testStd.limiter = newResizableSem(1)
	testStd.livePollInterval = 5 * time.Millisecond
	testStd.liveMaxEmptyPolls = maxEmptyPolls
	testStd.retryLimit.Store(1) // 分片失败立即判定，别在测试里等重试退避
	t.Cleanup(func() {
		testStd.limiter = oldLimiter
		testStd.livePollInterval, testStd.liveMaxEmptyPolls = oldInterval, oldEmpty
		testStd.retryLimit.Store(oldRetries)
	})
}

// startLiveServer 起步进式直播源：第 n 次轮询返回 lists[n-1]（越界则重复最后一份）。
// bad 里的分片返回 500，用于模拟源站中途挂掉。
func startLiveServer(t *testing.T, lists []string, segs map[string]string, bad map[string]bool) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	n := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := n
		if i >= len(lists) {
			i = len(lists) - 1
		}
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, lists[i])
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		if bad[r.URL.Path] {
			http.Error(w, "upstream boom", http.StatusInternalServerError)
			return
		}
		body, ok := segs[r.URL.Path]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// startLiveTask 注册并启动一条指向 srv 的直播任务。
func startLiveTask(t *testing.T, id, m3u8URL string) (*taskEntry, string) {
	t.Helper()
	dir := t.TempDir()
	te := &taskEntry{rt: testStd, st: taskState{
		id: id, queued: true, stage: "排队中", started: time.Now(),
		m3u8URL: m3u8URL, filename: id + ".ts", saveDir: dir,
		finalPath: filepath.Join(dir, id+".ts"),
	}}
	testStd.tasks[te.st.id] = te
	go runDiskPipeline(te)
	return te, dir
}

// TestLiveFailureAutoFinalizes 直播分片批次失败 → 自动收尾：
// 已录部分必须变成正式文件，任务终态必须标成"中断"而不是干净完成。
func TestLiveFailureAutoFinalizes(t *testing.T) {
	useFastLive(t, 1<<20) // 不让"空轮询"抢先结束，失败必须来自分片下载
	srv := startLiveServer(t,
		[]string{
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n",
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n" +
				"#EXTINF:6.0,\nseg/2.ts\n#EXTINF:6.0,\nseg/3.ts\n",
		},
		map[string]string{"/seg/0.ts": "SEG-0", "/seg/1.ts": "SEG-1"},
		map[string]bool{"/seg/2.ts": true, "/seg/3.ts": true},
	)
	te, _ := startLiveTask(t, "livesalvage", srv.URL+"/live.m3u8")

	waitTaskState(t, te, func(s taskState) bool { return s.done }, "中断收尾")
	waitLimiterDrained(t)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.interrupted {
		t.Fatalf("中断收尾后 interrupted=false（stage=%q errorMsg=%q）", st.stage, st.errorMsg)
	}
	if st.errorMsg == "" {
		t.Fatal("errorMsg 为空：中断原因丢失，界面会把它显示成一次完整录完")
	}
	if !strings.Contains(st.stage, "中断") {
		t.Fatalf("stage=%q want 含「中断」——必须区别于完整录完的「已保存」", st.stage)
	}
	if st.paused {
		t.Fatal("中断收尾后 paused=true：任务又变成可恢复态")
	}
	data, err := os.ReadFile(st.finalPath)
	if err != nil {
		t.Fatalf("已录部分没有收尾成正式文件: %v", err)
	}
	if string(data) != "SEG-0SEG-1" {
		t.Fatalf("成品内容=%q want %q", data, "SEG-0SEG-1")
	}
	if _, err := os.Stat(st.finalPath + ".part"); !os.IsNotExist(err) {
		t.Fatal("收尾后残留 .part 半成品")
	}
}

// TestLiveAutoFinalizeKeepsRecordedSegments 中断收尾不能把已录分片数清零——
// 否则界面上会显示"已录制 0 片"，用户看不出文件里到底有多少内容。
func TestLiveAutoFinalizeKeepsRecordedSegments(t *testing.T) {
	useFastLive(t, 1<<20)
	srv := startLiveServer(t,
		[]string{
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n",
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n" +
				"#EXTINF:6.0,\nseg/2.ts\n",
		},
		map[string]string{"/seg/0.ts": "SEG-0", "/seg/1.ts": "SEG-1"},
		map[string]bool{"/seg/2.ts": true},
	)
	te, _ := startLiveTask(t, "livesegcount", srv.URL+"/live.m3u8")

	waitTaskState(t, te, func(s taskState) bool { return s.done }, "中断收尾")
	waitLimiterDrained(t)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.segDone != 2 {
		t.Fatalf("中断收尾后 segDone=%d want 2（已落盘片数不得被清零）", st.segDone)
	}
	if st.segTot != 0 {
		t.Fatalf("直播 segTot=%d want 0（列表无限增长，没有总数）", st.segTot)
	}
	if !st.interrupted {
		t.Fatal("任务未被标记为中断收尾")
	}
}

// TestLiveInitFailureDoesNotFinalize 一片都没落盘时不收尾：
// 没有可保存的内容，硬保存只会给用户一个播不出画面的空壳。
func TestLiveInitFailureDoesNotFinalize(t *testing.T) {
	useFastLive(t, 1<<20)
	srv := startLiveServer(t,
		[]string{"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n"},
		map[string]string{},
		map[string]bool{"/seg/0.ts": true, "/seg/1.ts": true},
	)
	te, _ := startLiveTask(t, "livenocontent", srv.URL+"/live.m3u8")

	waitTaskState(t, te, func(s taskState) bool { return s.done }, "失败收尾")
	waitLimiterDrained(t)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.interrupted {
		t.Fatal("没有任何已落盘分片却标成了「中断已保存」")
	}
	if st.stage != "失败" {
		t.Fatalf("stage=%q want 失败（无内容可救）", st.stage)
	}
	if st.errorMsg == "" {
		t.Fatal("失败原因丢失")
	}
	if st.finalPath != "" {
		if _, err := os.Stat(st.finalPath); !os.IsNotExist(err) {
			t.Fatalf("无内容可用却产出了成品文件: %s", st.finalPath)
		}
	}
}

// TestLiveGapReportedAfterFailure 失败批次里没写进文件的那些分片，会在产物
// 时间轴上留一个真实空洞（fMP4 的 tfdt 前跳 / TS 的 PTS 前跳），必须如实报告。
func TestLiveGapReportedAfterFailure(t *testing.T) {
	useFastLive(t, 1<<20)
	srv := startLiveServer(t,
		[]string{
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n",
			"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n" +
				"#EXTINF:6.0,\nseg/2.ts\n#EXTINF:6.0,\nseg/3.ts\n",
		},
		map[string]string{"/seg/0.ts": "SEG-0", "/seg/1.ts": "SEG-1"},
		map[string]bool{"/seg/2.ts": true, "/seg/3.ts": true},
	)
	te, _ := startLiveTask(t, "livegap", srv.URL+"/live.m3u8")

	waitTaskState(t, te, func(s taskState) bool { return s.done }, "中断收尾")
	waitLimiterDrained(t)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.gapSeconds < 11 || st.gapSeconds > 13 {
		t.Fatalf("gapSeconds=%.1f want ≈12（2 片 × 6s 未写入）", st.gapSeconds)
	}
}

// TestLiveGapDetectedOnWindowRollover 播放列表窗口滚动把分片淘汰掉时（轮询
// 间隔超过了窗口时长），那些分片同样永远补不回来，必须计入缺口。
func TestLiveGapDetectedOnWindowRollover(t *testing.T) {
	useFastLive(t, 1) // 第二轮之后无新分片即判定结束，好让任务自然收尾
	// 注意第一份列表会被 pipeline 的探测请求消费掉（fetchPlaylist 先跑一次），
	// 所以同内容的列表要出现两次：一次给探测，一次给 liveDownload 第一轮。
	const listSeq0 = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n"
	// 下一轮窗口已经滚到 seq=10：seq 2..9 这 8 片被淘汰
	const listSeq10 = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:10\n" +
		"#EXTINF:6.0,\nseg/10.ts\n#EXTINF:6.0,\nseg/11.ts\n"
	srv := startLiveServer(t,
		[]string{listSeq0, listSeq0, listSeq10},
		map[string]string{
			"/seg/0.ts": "SEG-0", "/seg/1.ts": "SEG-1",
			"/seg/10.ts": "SEG-10", "/seg/11.ts": "SEG-11",
		},
		nil,
	)
	te, _ := startLiveTask(t, "liverollover", srv.URL+"/live.m3u8")

	waitTaskState(t, te, func(s taskState) bool { return s.done }, "自然收尾")
	waitLimiterDrained(t)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.gapSeconds < 40 {
		t.Fatalf("gapSeconds=%.1f want ≥40（8 片 × 6s 已被窗口淘汰）", st.gapSeconds)
	}
	if st.errorMsg != "" {
		t.Fatalf("自然收尾不该带错误原因: %q", st.errorMsg)
	}
}

// seedStateFile 写一份状态文件供 loadState 恢复。
func seedStateFile(t *testing.T, tasks []persistedTask) {
	t.Helper()
	data, err := json.Marshal(stateFile{Version: stateVersion, Tasks: tasks})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testStd.getStatePath(), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestSalvageInterruptedLiveFinalizesPart 程序被强杀（任务管理器 / 断电）时
// 没有任何机会执行收尾，重启后必须补做——否则"直播中断必自动收尾"就留了一个
// "只能手动点停止"的口子。这里直接调补偿函数（同步）；Engine.Start 里以
// goroutine 启动它，不阻塞 GUI。
func TestSalvageInterruptedLiveFinalizesPart(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "live.ts")
	const want = "SEG-0SEG-1"
	if err := os.WriteFile(final+".part", []byte(want), 0644); err != nil {
		t.Fatal(err)
	}
	seedStateFile(t, []persistedTask{{
		ID: "t1", Filename: "live.ts", SaveDir: dir, FinalPath: final,
		M3u8URL: "https://example.com/live.m3u8",
		Stage:   "录制中断（程序退出）", Live: true, Paused: true, SegDone: 2,
	}})
	testStd.loadState()

	testStd.salvageInterruptedLive()

	te := testStd.findTask("t1")
	if te == nil {
		t.Fatal("任务未被恢复")
	}
	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.done || !st.interrupted {
		t.Fatalf("补偿收尾未生效: done=%v interrupted=%v stage=%q", st.done, st.interrupted, st.stage)
	}
	if st.segDone != 2 {
		t.Fatalf("segDone=%d want 2（已录片数必须保留）", st.segDone)
	}
	if st.paused {
		t.Fatal("补偿收尾后仍是待处理态")
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("补偿收尾没有产出成品文件: %v", err)
	}
	if string(data) != want {
		t.Fatalf("成品内容=%q want %q", data, want)
	}
	if _, err := os.Stat(final + ".part"); !os.IsNotExist(err) {
		t.Fatal("补偿收尾后残留 .part")
	}
}

// TestSalvageInterruptedLiveKeepsPartOnFailure 补偿收尾失败时必须保留 .part：
// 那可能是用户仅有的内容，删掉就等于把他录的东西弄丢了（本项目的红线是
// 宁可不保存，也不能悄悄销毁用户数据）。
func TestSalvageInterruptedLiveKeepsPartOnFailure(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "live.mp4")
	if err := os.WriteFile(final+".part", []byte("NOT-FMP4-DATA"), 0644); err != nil {
		t.Fatal(err)
	}
	seedStateFile(t, []persistedTask{{
		ID: "t1", Filename: "live.mp4", SaveDir: dir, FinalPath: final,
		M3u8URL: "https://example.com/live.m3u8",
		Stage:   "录制中断（程序退出）", Live: true, Paused: true, SegDone: 3,
		ContainerID: "fmp4-map", // 声明是 fMP4，内容却不是 → 抽样校验必然失败
	}})
	testStd.loadState()

	testStd.salvageInterruptedLive()

	te := testStd.findTask("t1")
	if te == nil {
		t.Fatal("任务未被恢复")
	}
	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.interrupted {
		t.Fatal("抽样校验不通过却当成中断收尾成功了")
	}
	if st.stage != "失败" {
		t.Fatalf("stage=%q want 失败", st.stage)
	}
	if _, err := os.Stat(final + ".part"); err != nil {
		t.Fatalf("补偿收尾失败却删掉了 .part（用户仅有的内容）: %v", err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatal("校验失败却产出了成品文件")
	}
}

// TestSalvageSkipsNonLiveTasks 点播任务的「已暂停」是可续的（.part 就是断点），
// 补偿收尾会把它 rename 成成品、把断点吃掉——绝不能碰。
func TestSalvageSkipsNonLiveTasks(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "vod.ts")
	if err := os.WriteFile(final+".part", []byte("SEG-0"), 0644); err != nil {
		t.Fatal(err)
	}
	seedStateFile(t, []persistedTask{{
		ID: "t1", Filename: "vod.ts", SaveDir: dir, FinalPath: final,
		M3u8URL: "https://example.com/vod.m3u8",
		Stage:   "已暂停", Paused: true, SegDone: 1, SegTot: 4,
	}})
	testStd.loadState()

	testStd.salvageInterruptedLive()

	te := testStd.findTask("t1")
	if te == nil {
		t.Fatal("任务未被恢复")
	}
	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.done || st.interrupted {
		t.Fatalf("点播暂停任务被补偿收尾了: done=%v interrupted=%v", st.done, st.interrupted)
	}
	if st.stage != "已暂停" {
		t.Fatalf("stage=%q want 已暂停（点播断点必须原样保留）", st.stage)
	}
	if _, err := os.Stat(final + ".part"); err != nil {
		t.Fatalf("点播任务的 .part 被动过: %v", err)
	}
}

// TestRestoreContainerSniffsTSWithoutMapHint 旧版状态文件没有 containerID 时，
// 续传与收尾只能靠 .part 头部嗅探容器。嗅探以前硬编码"播放列表有 #EXT-X-MAP"
// 这一前提，于是**任何**开头不是 ftyp 的内容都被判成 fmp4-map —— 一个 TS
// 半成品会被 fMP4 校验器判成"内容不符合容器特征"，续传和收尾双双白白失败。
func TestRestoreContainerSniffsTSWithoutMapHint(t *testing.T) {
	dir := t.TempDir()
	tsPart := filepath.Join(dir, "v.ts.part")
	if err := os.WriteFile(tsPart, fakeTSPlain(0), 0644); err != nil {
		t.Fatal(err)
	}
	job := &dlJob{rt: testStd}
	restoreContainer(job, "", tsPart, nil)
	if job.container == nil || job.container.ID != "ts" {
		t.Fatalf("TS 半成品嗅探结果=%v want ts（不能靠 fMP4 校验器兜底）", job.container)
	}

	// 反向：fMP4 的 .part 头部就是 init 段（ftyp 开头），按魔数必须仍然认得出来，
	// 否则这个修复就把 fMP4 续传弄坏了。
	mp4Part := filepath.Join(dir, "v.mp4.part")
	head := append([]byte{0, 0, 0, 0x18}, []byte("ftypiso5")...)
	if err := os.WriteFile(mp4Part, append(head, make([]byte, 256)...), 0644); err != nil {
		t.Fatal(err)
	}
	job2 := &dlJob{rt: testStd}
	restoreContainer(job2, "", mp4Part, nil)
	if job2.container == nil || job2.container.ID != "fmp4" {
		t.Fatalf("fMP4 半成品嗅探结果=%v want fmp4", job2.container)
	}
}

// callStop 调用 /stop?id=<id>，断言 HTTP 200。
func callStop(t *testing.T, id string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/stop?id="+id, nil)
	testEngine().handleStop(w, r)
	if w.Code != 200 {
		t.Fatalf("stop HTTP %d: %s", w.Code, w.Body.String())
	}
}

// TestStoppedRestoredLiveSavesPart 重启后恢复出来的直播任务（没有 job）被用户
// 点「停止」时，必须把 .part 里的内容保存成正式文件。
//
// 回归：finishStop 原先只看 `job == nil` 就判"无内容"，于是把用户已经录到磁盘
// 上的内容**整个删掉**——重启后点一次「停止」，录到的东西就没了。判据必须落在
// ".part 里有没有内容"上，而不是"当前有没有 job"上。
func TestStoppedRestoredLiveSavesPart(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "live.ts")
	const want = "SEG-0SEG-1"
	if err := os.WriteFile(final+".part", []byte(want), 0644); err != nil {
		t.Fatal(err)
	}
	seedStateFile(t, []persistedTask{{
		ID: "t1", Filename: "live.ts", SaveDir: dir, FinalPath: final,
		M3u8URL: "https://example.com/live.m3u8",
		Stage:   "录制中断（程序退出）", Live: true, Paused: true, SegDone: 2,
	}})
	testStd.loadState()

	callStop(t, "t1")

	te := testStd.findTask("t1")
	if te == nil {
		t.Fatal("任务未被恢复")
	}
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "停止收尾")

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.interrupted {
		t.Fatalf("重启后点停止应走中断收尾: interrupted=%v stage=%q", st.interrupted, st.stage)
	}
	if st.segDone != 2 {
		t.Fatalf("segDone=%d want 2", st.segDone)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("点「停止」把用户录到的内容删了（成品文件不存在）: %v", err)
	}
	if string(data) != want {
		t.Fatalf("成品内容=%q want %q", data, want)
	}
}

// TestUserStopOnRunningLiveMarksCompleted 用户对着"正在录制"的直播点「停止」，
// 语义是"录到这里收工"——必须标「已完成」，不能标「已中断」。
//
// 学徒 2026-09-15 的判断：中断是"非用户意愿"的（源站断流、程序退出），
// 用户自己叫停不该显示成出了故障。区别于 TestStoppedRestoredLiveSavesPart：
// 那条路上任务早就停了，用户点的「停止」只是"保存已录部分"，仍按中断记。
func TestUserStopOnRunningLiveMarksCompleted(t *testing.T) {
	useFastLive(t, 1<<20) // 不让"空轮询"抢先结束
	srv := startLiveServer(t,
		[]string{"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n"},
		map[string]string{"/seg/0.ts": "SEG-0", "/seg/1.ts": "SEG-1"},
		nil,
	)
	te, _ := startLiveTask(t, "livestop", srv.URL+"/live.m3u8")

	// 等到真有分片落盘再停：否则会走"无内容"分支，测不到收尾。
	waitTaskState(t, te, func(s taskState) bool {
		j := te.jobRef()
		return s.running && j != nil && j.segFlushedNow() >= 2
	}, "录到 2 片")

	callStop(t, "livestop")
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "停止收尾")
	waitLimiterDrained(t)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.interrupted {
		t.Fatalf("用户主动停止被标成「已中断」（stage=%q）——那是故障态，不该用在用户自己叫停上", st.stage)
	}
	if st.errorMsg != "" {
		t.Fatalf("用户停止留下了错误原因 %q：前端会把它算进「失败」统计", st.errorMsg)
	}
	if !strings.Contains(st.stage, "用户停止") {
		t.Fatalf("stage=%q want 含「用户停止录制」——得说清是谁结束的", st.stage)
	}
	if st.paused {
		t.Fatal("停止收尾后 paused=true：任务又变成可恢复态")
	}
	data, err := os.ReadFile(st.finalPath)
	if err != nil {
		t.Fatalf("停止后没有产出成品文件: %v", err)
	}
	if string(data) != "SEG-0SEG-1" {
		t.Fatalf("成品内容=%q want %q", data, "SEG-0SEG-1")
	}
	if _, err := os.Stat(st.finalPath + ".part"); !os.IsNotExist(err) {
		t.Fatal("收尾后残留 .part 半成品")
	}
}

// TestUserStopKeepsPartWhenValidationFails 用户点「停止」但产物抽样校验不通过时，
// 必须保留 .part。
//
// 这是个容易写错的地方：把"用户停止"实现成 finalizeRecording(..., false, ...) 会
// 顺带让它走"正常路径"的校验失败分支 —— 那里会 os.Remove(.part)。结果是"用户点一下
// 停止，磁盘上没录完的东西被销毁"。所以保留 .part 的判据必须是 outcome != complete，
// 不能跟"显示成已完成还是已中断"共用一个布尔。
func TestUserStopKeepsPartWhenValidationFails(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "live.mp4")
	if err := os.WriteFile(final+".part", []byte("NOT-FMP4-DATA"), 0644); err != nil {
		t.Fatal(err)
	}
	te := &taskEntry{rt: testStd, st: taskState{
		id: "livestop-bad", live: true, running: true, stage: "录制直播中",
		m3u8URL:  "https://example.com/live.m3u8",
		filename: "live.mp4", saveDir: dir, finalPath: final,
	}}
	job := &dlJob{rt: testStd, id: "livestop-bad", live: true}
	job.setSeg(3, 0)
	job.setSegFlushed(3)
	restoreContainer(job, "fmp4-map", final+".part", nil) // 声明 fMP4，内容不是 → 校验必失败
	te.job = job
	te.intent = intentStop
	testStd.tasks[te.st.id] = te

	finishInterrupt(te)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if _, err := os.Stat(final + ".part"); err != nil {
		t.Fatalf("用户停止 + 校验失败时删掉了 .part（那是用户仅有的内容）: %v", err)
	}
	if st.interrupted {
		t.Fatal("抽样校验不通过却当成收尾成功了")
	}
	if !st.done || st.errorMsg == "" {
		t.Fatalf("校验失败必须标失败并留下原因: done=%v errorMsg=%q", st.done, st.errorMsg)
	}
}

// TestSnapshotExposesGapWhileRecording 录制途中出现的缺口必须当场就能从 /status
// 看到。等到收尾才第一次告诉用户，他可能已经白录了几个小时。
func TestSnapshotExposesGapWhileRecording(t *testing.T) {
	te := &taskEntry{rt: testStd, st: taskState{id: "livegap", live: true, running: true, stage: "录制直播中"}}
	job := &dlJob{rt: testStd, id: "livegap", live: true}
	job.setSeg(4, 0)
	job.addGapSeconds(12)
	te.job = job

	if got := snapshot(te).gapSeconds; got < 11 || got > 13 {
		t.Fatalf("snapshot gapSeconds=%.1f want ≈12（运行中的缺口没暴露给前端）", got)
	}
	if got := toTaskStateDTO(snapshot(te)).GapSeconds; got < 11 || got > 13 {
		t.Fatalf("DTO gapSeconds=%.1f want ≈12（前端拿不到缺口就报不出来）", got)
	}
}

// TestSalvageAndStopDoNotDoubleFinalize 补偿收尾与用户手点「停止」可能同时落到
// 同一个恢复出来的任务上。两条收尾并发时，后到的那个会因为 .part 已被改名而
// 抽样校验失败，把一次成功的保存改写成「失败」——用户的文件其实在磁盘上好好的。
func TestSalvageAndStopDoNotDoubleFinalize(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "live.ts")
	if err := os.WriteFile(final+".part", []byte("SEG-0SEG-1"), 0644); err != nil {
		t.Fatal(err)
	}
	seedStateFile(t, []persistedTask{{
		ID: "t1", Filename: "live.ts", SaveDir: dir, FinalPath: final,
		M3u8URL: "https://example.com/live.m3u8",
		Stage:   "录制中断（程序退出）", Live: true, Paused: true, SegDone: 2,
	}})
	testStd.loadState()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); testStd.salvageInterruptedLive() }()
	go func() { defer wg.Done(); finishStoppedTask(testStd.findTask("t1")) }()
	wg.Wait()

	te := testStd.findTask("t1")
	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if st.stage == "失败" {
		t.Fatalf("两条收尾路径互相踩踏，成功保存被改写成失败: %+v", st)
	}
	if !st.done || !st.interrupted {
		t.Fatalf("收尾未完成: done=%v interrupted=%v stage=%q", st.done, st.interrupted, st.stage)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("成品文件不存在: %v", err)
	}
	if string(data) != "SEG-0SEG-1" {
		t.Fatalf("成品内容=%q", data)
	}
}

// TestLoadStateMarksLiveAsInterrupted 重启后未完成的直播任务必须标成"中断"，
// 不能混进点播那条"已暂停，可继续下载"的路——直播没有"接着录"这回事。
func TestLoadStateMarksLiveAsInterrupted(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	seedStateFile(t, []persistedTask{
		{ID: "t1", Filename: "live.ts", SaveDir: dir, M3u8URL: "https://example.com/live.m3u8",
			Stage: "录制中", Live: true, SegDone: 5},
		{ID: "t2", Filename: "vod.ts", SaveDir: dir, M3u8URL: "https://example.com/vod.m3u8",
			Stage: "下载分片中", SegDone: 3},
	})

	testStd.loadState()

	live := testStd.findTask("t1")
	if live == nil {
		t.Fatal("直播任务未被恢复")
	}
	live.mu.Lock()
	liveStage, livePaused, liveDone := live.st.stage, live.st.paused, live.st.done
	live.mu.Unlock()
	if !strings.Contains(liveStage, "中断") {
		t.Fatalf("直播任务恢复后 stage=%q want 含「中断」", liveStage)
	}
	if !livePaused {
		t.Fatal("直播任务恢复后应为待处理态（前端据此只给「停止」「取消」）")
	}
	if liveDone {
		t.Fatal("直播任务恢复后不该直接是终态——已录部分还没收尾")
	}

	vod := testStd.findTask("t2")
	if vod == nil {
		t.Fatal("点播任务未被恢复")
	}
	vod.mu.Lock()
	vodStage, vodPaused := vod.st.stage, vod.st.paused
	vod.mu.Unlock()
	if vodStage != "已暂停" || !vodPaused {
		t.Fatalf("点播任务恢复后 stage=%q paused=%v want 已暂停/true", vodStage, vodPaused)
	}
}
