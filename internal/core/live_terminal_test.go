// 直播终态文案（学徒 2026-09-22 定，方案 A「只改文案」）。
//
// 直播有两条"自然结束"的出口，判据强度完全不同，卡片文案必须分开：
//   - 播放列表出现 #EXT-X-ENDLIST —— 源站白纸黑字声明结束，**确证**；
//   - 连续 liveMaxEmptyPolls 次无新分片 —— 只是"列表不再增长"，**推断**
//     （runtime.go 默认 3s × 25 次 = 75 秒；源站编码卡顿、CDN 慢一拍都会撞上），
//     产物末尾可能被截断。
//
// 改前两条出口都落到 finalizeComplete 且不带任何说明，stage 逐字都是「已保存」，
// 用户分不出哪次是录完的、哪次是猜完的 —— 本项目头号缺陷形态（产物可能不完整，
// 而状态与日志显示一切正常）。
//
// 本文件同时钉住"只动文案"这条边界：徽章口径（interrupted / errorMsg / done）
// 不许被这次改动带偏，点播的「已保存」也不许多出括号。
package core

import (
	"strings"
	"testing"
)

const (
	// liveListNoEnd 一份普通的直播列表：没有 ENDLIST，源站侧面表现就是"还在播"。
	liveListNoEnd = "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n" +
		"#EXTINF:6.0,\nseg/1.ts\n"
	// liveListEnd 同一份列表补上 ENDLIST：源站声明直播结束。
	liveListEnd = liveListNoEnd + "#EXT-X-ENDLIST\n"
)

func liveTestSegs() map[string]string {
	return map[string]string{"/seg/0.ts": "SEG-0", "/seg/1.ts": "SEG-1"}
}

// TestLiveEndListStageText 源站声明结束：确证路径的文案必须点明"播放列表已结束"，
// 且不能混进推断结束的措辞。
func TestLiveEndListStageText(t *testing.T) {
	useFastLive(t, 1<<20) // 不让空轮询抢先结束：这次结束必须来自 ENDLIST
	srv := startLiveServer(t,
		[]string{liveListNoEnd, liveListEnd},
		liveTestSegs(), nil)

	te, _ := startLiveTask(t, "livendl", srv.URL+"/live.m3u8")
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "ENDLIST 收尾")
	waitLimiterDrained(t)

	st := snapshot(te)
	if !st.done || st.interrupted || st.errorMsg != "" {
		t.Fatalf("ENDLIST 结束应是干净的完成态: done=%v interrupted=%v errorMsg=%q",
			st.done, st.interrupted, st.errorMsg)
	}
	if !strings.Contains(st.stage, "播放列表已结束") {
		t.Fatalf("stage=%q want 含「播放列表已结束」（源站声明结束是确证）", st.stage)
	}
	if strings.Contains(st.stage, "停止更新") {
		t.Fatalf("stage=%q 把 ENDLIST 说成了推断结束", st.stage)
	}
}

// TestLiveInferredEndStageText 列表不再增长：推断路径的文案必须与确证路径可区分，
// 并且明说末尾可能不全 —— 那才是这条出口的真实不确定性。
func TestLiveInferredEndStageText(t *testing.T) {
	useFastLive(t, 3) // 连续 3 次空轮询即判定结束
	srv := startLiveServer(t, []string{liveListNoEnd}, liveTestSegs(), nil)

	te, _ := startLiveTask(t, "liveinf", srv.URL+"/live.m3u8")
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "推断结束收尾")
	waitLimiterDrained(t)

	st := snapshot(te)
	// 徽章口径不变：方案 A 只改文案，产物仍是"存下来能播"的完成态。
	if !st.done || st.interrupted || st.errorMsg != "" {
		t.Fatalf("推断结束仍应是完成态（徽章口径不变）: done=%v interrupted=%v errorMsg=%q",
			st.done, st.interrupted, st.errorMsg)
	}
	if st.stage == "已保存" {
		t.Fatal("stage 恰好是「已保存」：推断结束与确证结束又混成同一句了（这正是本次要修的缺陷）")
	}
	if !strings.Contains(st.stage, "播放列表停止更新") {
		t.Fatalf("stage=%q want 含「播放列表停止更新」", st.stage)
	}
	if !strings.Contains(st.stage, "末尾可能不全") {
		t.Fatalf("stage=%q want 明说「末尾可能不全」——推断结束的产物就是可能被截断", st.stage)
	}
}

// TestTwoLiveEndsHaveDistinctStageText 两条出口的卡片文案必须真的不同。
// 分开断言关键字还不够——两处都写成同一个字符串时单看每条都"含关键字"。
func TestTwoLiveEndsHaveDistinctStageText(t *testing.T) {
	endListStage := stageAfterLiveEnd(t, "livendl2", []string{liveListNoEnd, liveListEnd}, 1<<20)
	inferredStage := stageAfterLiveEnd(t, "liveinf2", []string{liveListNoEnd}, 3)

	if endListStage == inferredStage {
		t.Fatalf("两种结束方式的 stage 逐字相同（%q）：用户分不出哪次是录完的、哪次是猜完的", endListStage)
	}
}

// stageAfterLiveEnd 跑一条真直播任务到终态，返回它的 stage。
func stageAfterLiveEnd(t *testing.T, id string, lists []string, maxEmpty int) string {
	t.Helper()
	useFastLive(t, maxEmpty)
	srv := startLiveServer(t, lists, liveTestSegs(), nil)
	te, _ := startLiveTask(t, id, srv.URL+"/live.m3u8")
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "直播收尾 "+id)
	waitLimiterDrained(t)
	return snapshot(te).stage
}

// TestVodCompleteStageTextStaysPlain 点播完成态不受这次改动影响：
// reason 只有直播会给，点播必须还是干脆的「已保存」（多出一对括号就是回归）。
func TestVodCompleteStageTextStaysPlain(t *testing.T) {
	useFastLive(t, 1<<20)
	// 首份列表就带 ENDLIST ⇒ isLive=false，走点播一次性下载
	srv := startLiveServer(t,
		[]string{"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXT-X-ENDLIST\n"},
		map[string]string{"/seg/0.ts": "SEG-0"}, nil)

	te, _ := startLiveTask(t, "vodplain", srv.URL+"/live.m3u8")
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "点播完成")
	waitLimiterDrained(t)

	st := snapshot(te)
	if st.live {
		t.Fatal("首份列表带 ENDLIST 的任务被当成了直播")
	}
	if st.stage != "已保存" {
		t.Fatalf("stage=%q want 恰好「已保存」：点播不该带上直播的结束说明", st.stage)
	}
	if st.interrupted || st.errorMsg != "" {
		t.Fatalf("点播完成态被污染: interrupted=%v errorMsg=%q", st.interrupted, st.errorMsg)
	}
}
