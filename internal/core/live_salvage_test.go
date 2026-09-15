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

// TestLoadStateMarksLiveAsInterrupted 重启后未完成的直播任务必须标成"中断"，
// 不能混进点播那条"已暂停，可继续下载"的路——直播没有"接着录"这回事。
func TestLoadStateMarksLiveAsInterrupted(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	data, err := json.Marshal(stateFile{Version: stateVersion, Tasks: []persistedTask{
		{ID: "t1", Filename: "live.ts", SaveDir: dir, M3u8URL: "https://example.com/live.m3u8",
			Stage: "录制中", Live: true, SegDone: 5},
		{ID: "t2", Filename: "vod.ts", SaveDir: dir, M3u8URL: "https://example.com/vod.m3u8",
			Stage: "下载分片中", SegDone: 3},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testStd.getStatePath(), data, 0644); err != nil {
		t.Fatal(err)
	}

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
