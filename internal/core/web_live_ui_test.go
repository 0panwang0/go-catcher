// 直播 UI 的定点回归（B0-2 / B0-3）。
//
// 语义（学徒 2026-09-15 定）：
//   - 直播任务在界面上只给「停止」与「取消」，不给暂停/继续/重试；
//   - 中断收尾的任务显示成「已中断」而不是「失败」（文件其实已经保存了，
//     显示成失败会让用户以为白录了），并如实报出产物时间轴上的缺口。
//
// 这类回归编译期发现不了（页面是 HTML 字符串），前端又没有运行时测试，
// 所以用扫描把不变量钉住——参照 embedpage_test.go 的做法。
// 断言只取不含缩进的短片段：这里钉的是"有没有这个行为"，不是排版。
package core

import (
	"strings"
	"testing"
)

func TestLiveUIOffersStopNotResume(t *testing.T) {
	page := homePageHTML

	mustHave := []struct{ frag, why string }{
		{"function stopTask(id)", "缺少直播停止入口：服务端已提供 /stop 但页面调不到"},
		{"/stop?id=", "stopTask 没有打到 /stop 端点"},
		// 运行中：直播给「停止」，点播给「暂停」（卡片式与紧凑式各一处）
		{"'⏹ 停止','stopTask('+id+')'", "卡片式运行态没把直播的「暂停」换成「停止」"},
		{"'⏹','停止','stopTask('+id+')'", "紧凑式运行态没把直播的「暂停」换成「停止」"},
		// 终态：中断/失败/旧暂停态的直播只给「停止（保存已录）」+「取消」
		{"t.live && (t.done || t.paused || t.error)", "缺少直播终态分支：这些任务会掉进点播按钮，出现「重试」"},
		{"停止（保存已录）", "卡片式终态没有「保存已录」的停止入口"},
		{"停止（保存已录部分）", "紧凑式终态没有「保存已录部分」的停止入口"},
		{"已中断", "中断收尾的任务没有独立徽章，会被显示成「失败」"},
		{"录制中断", "中断态文案缺失：产物不完整必须如实说出来"},
		// B0-3：中断标记与缺口提示必须来自服务端字段，不能靠 error 字符串猜
		{"if(t.interrupted)", "前端没有消费服务端的 interrupted 标记（会退回按 error 猜）"},
		{"function gapText(t)", "缺少缺口提示：时间轴上的空洞会被完全藏起来"},
		{"t.gapSeconds", "gapText 没有读服务端的 gapSeconds"},
		{"gapText(t)", "缺口提示写好了却没有在任务行里显示"},
	}
	for _, c := range mustHave {
		if !strings.Contains(page, c.frag) {
			t.Errorf("web/index.html 缺少 %q —— %s", c.frag, c.why)
		}
	}

	// 反向断言：这句承诺与实现矛盾（暂停期间的流已从列表滚走），必须彻底删掉
	if strings.Contains(page, "可继续录制") {
		t.Error("web/index.html 仍含「可继续录制」：直播暂停后无法续录，这句承诺是错的")
	}
	// 反向断言：中断态不能再靠 "done && error && live" 这种字符串语义判断——
	// 它区分不出"失败"（没有产物）与"中断已保存"（产物在磁盘上）
	if strings.Contains(page, "t.done && t.error && t.live") {
		t.Error("web/index.html 仍在用 done&&error&&live 判断中断态，应改用服务端的 interrupted 字段")
	}
}

// TestLiveUIRendersNoFakeProgress 直播卡片不得画"假进度条"。
//
// 学徒 2026-09-15 报的原始现象：录制中的进度条只有一半、样式奇怪。根因是
// `.bar>i.indet` 写死 width:42%，只让内部渐变来回跑（动的是 background-position，
// 色块本身不动），于是永远像一条卡在 40% 的坏进度条。直播本来就没有总时长，
// 套"进度条"这个隐喻本身就是错的：运行中应当画全宽活性带，终态画全宽实色条。
func TestLiveUIRendersNoFakeProgress(t *testing.T) {
	page := homePageHTML

	mustHave := []struct{ frag, why string }{
		{".bar.live", "直播运行中缺少活性带"},
		{"flowstripes", "活性带没有流动动画，看着像一条死掉的空条"},
		{"<div class=\"metric\">", "直播卡片缺少单行指标行（左「已录时长」右「片数」）"},
		{"function liveTitle(t)", "缺少直播指标文案函数"},
		{"function stageRow(t)", "stageText 可能返回空串，空行会留出多余间距"},
		// 指标行的右侧（片数）与 stageText 左边（时长）不能再互相重复
		{"t.segDone+' 片'", "直播片数文案变了，请确认指标行右侧仍显示片数"},
		{"if (!key) return '日期未知'", "日期分组没兜住无效时间戳（会渲染成 NaN月NaN日）"},
	}
	for _, c := range mustHave {
		if !strings.Contains(page, c.frag) {
			t.Errorf("web/index.html 缺少 %q —— %s", c.frag, c.why)
		}
	}

	mustNotHave := []struct{ frag, why string }{
		{".bar>i.indet", "直播仍在用 42% 定宽的假进度条（永远像卡在半路）"},
		{".rowstrip i.indet", "简略模式仍在用 42% 定宽的假进度条"},
		{".indet{width:42%", "还有写死 42% 宽度的假进度条样式"},
		{"keyframes indet", "indet 动画已无人使用（只让渐变跑、色块不动的假动效）"},
		{"录制直播中 · 已录 ", "直播录制中的状态行又把时长/片数说了一遍（指标行已经说过）"},
		{"已录制 '+t.segDone+' 个分片", "旧的重复片数文案回来了"},
	}
	for _, c := range mustNotHave {
		if strings.Contains(page, c.frag) {
			t.Errorf("web/index.html 仍含 %q —— %s", c.frag, c.why)
		}
	}

	// 顶部统计：直播运行中/排队中不得算进"中断"。这个判据漏掉 paused 时，
	// 每条正在录的直播都会被统计成"中断"。
	if !strings.Contains(page, "!t.canceled && !!t.paused") {
		t.Error("liveInterrupted 少了 paused 条件：正在录制的直播会被统计成「中断」")
	}
}
