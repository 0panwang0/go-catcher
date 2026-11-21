// 直播 UI 的定点回归（B0-2 / B0-3）。
//
// 语义（学徒 2026-09-15 定，2026-09-20 补充文案口径）：
//   - 直播任务在界面上只给「停止」与「取消」，不给暂停/继续/重试；
//   - 中断收尾的任务显示成「已中断」而不是「失败」（文件其实已经保存了，
//     显示成失败会让用户以为白录了），并如实报出产物时间轴上的缺口。
//   - **按钮文案不带括号补充说明**（学徒 2026-09-20 拍板选 B 并追加此条）：
//     直播中断态的「停止」后面**不再跟**「（保存已录）」/「（保存已录部分）」。
//     "停止后已录部分会保存成文件"这件事由状态行那句完整句子承担
//     （stageText 的 `录制已中断 · 点「停止」保存已录部分`），按钮只留动作名。
//     ⚠️ 已知后果：这个标签与「直播运行中」的「⏹ 停止」逐字相同，两个态靠徽章
//     （「已中断」/「录制中」）与状态行区分——**不许靠往按钮上加字来"补救"**。
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
		// 终态：中断/失败/旧暂停态的直播只给「停止」+「取消」
		{"t.live && (t.done || t.paused || t.error)", "缺少直播终态分支：这些任务会掉进点播按钮，出现「重试」"},
		// 这里只钉"这个文案存在"（全页扫描）；**分支内**确实给了 stop 按钮由
		// TestLiveInterruptedStopHasNoParenthetical 用 spanBetween 另钉，别把两条混起来看。
		{"'⏹ 停止','stopTask('+id+')'", "卡片式终态没有「停止」入口"},
		{"'⏹','停止','stopTask('+id+')'", "紧凑式终态没有「停止」入口"},
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

// TestLiveInterruptedStopLabelHasNoParenthetical 直播中断态的「停止」不带括号补充说明。
//
// 学徒 2026-09-20 拍板：选 B（保留「停止」这个动作语义），并追加"不要（保存已录）
// 这种文案" —— 即按钮文案只留动作名，"停止后已录部分会保存成文件"这件事
// 交给状态行那句完整句子（stageText 的 `录制已中断 · 点「停止」保存已录部分`）。
//
// 为什么必须**分支内逐字**断言，而不是全页扫 `'⏹ 停止','stopTask('`：
// 后者在"运行中"分支里也有，于是"终态分支整个丢掉按钮"时照样绿 —— 那是假绿。
// 这里钉的是"那一行确实长这样"，改坏了（丢掉按钮 / 加回括号）必红。
//
// ⚠️ 复核提示：`'取消（删除文件）'` 是**简略模式的 title（悬停提示）**，不是可见标签，
// 且它是破坏性警告（点下去会删文件）而不是安慰性补充，**本断言不管它**——
// 有意保留。若哪天要一并去掉，先确认学徒的口径（2026-09-20 只点了「（保存已录）」）。
func TestLiveInterruptedStopLabelHasNoParenthetical(t *testing.T) {
	page := homePageHTML

	iActs := strings.Index(page, "function actions(t){")
	iRow := strings.Index(page, "function rowCard(t){")
	if iActs < 0 || iRow < 0 || iRow <= iActs {
		t.Fatal("找不到 actions()/rowCard() —— 页面结构变了，本测试的锚点要跟着改")
	}

	for _, c := range []struct{ where, src, want string }{
		{"详细模式·直播终态", page[iActs:iRow], "btn('primary','⏹ 停止','stopTask('+id+')')"},
		{"简略模式·直播终态", page[iRow:], "ibtn('primary','⏹','停止','stopTask('+id+')')"},
	} {
		reg, ok := spanBetween(c.src, "} else if (t.live &&", "} else if (t.done && !t.error) {")
		if !ok {
			t.Errorf("%s：取不到直播终态分支 —— 分支结构变了，测试锚点要跟着改", c.where)
			continue
		}
		if !strings.Contains(reg, c.want) {
			t.Errorf("%s：终态分支里的停止按钮不是 %q —— 学徒 2026-09-20 定："+
				"按钮文案不带括号补充说明，那句解释归状态行", c.where, c.want)
		}
	}

	// 反向兜底：括号版文案不许从别处（另一处的 title、注释…）溜回来。
	if strings.Contains(page, "（保存已录") {
		t.Error("web/index.html 又出现了「（保存已录…）」—— 学徒 2026-09-20 定：按钮文案不带括号补充说明")
	}
}

// TestLiveUIRendersNoFakeProgress 直播卡片不得画"假进度条"。
//
// 学徒 2026-09-15 报的原始现象：录制中的进度条只有一半、样式奇怪。根因是
// `.bar>i.indet` 写死 width:42%，只让内部渐变来回跑（动的是 background-position，
// 色块本身不动），于是永远像一条卡在 40% 的坏进度条。直播本来就没有总时长，
// 套"进度条"这个隐喻本身就是错的：运行中应当画全宽活性带，终态画全宽实色条。
//
// 学徒 2026-09-16 追加：第一版活性带用"铺满的等距硬边流动纹"解决"不能像进度条"，
// 但形态本身丑（10px 高的条上就是一排脏虚线）。指定形态改为两条圆头胶囊 ——
// 一长一短、一快一慢、各自轮回，相位错开半程。判据不变：任何一帧都不能出现
// "左端贴死、右端是推进边界"的实心块，且两端都可以是空的。
// 卡片的 ::before/::after 与简略行的 .rowstrip.live 必须同机制同参数，
// 否则同一时刻两处位置对不上。
func TestLiveUIRendersNoFakeProgress(t *testing.T) {
	page := homePageHTML

	mustHave := []struct{ frag, why string }{
		{".bar.live", "直播运行中缺少活性带"},
		// 学徒 2026-09-16 指定的形态：两条圆头胶囊，一长一短、一快一慢，各自轮回。
		// 旧实现是"硬边等距流动纹"，在 10px 高的条上就是一排脏虚线。
		{".bar.live::before", "卡片活性带缺少长胶囊（::before）"},
		{".bar.live::after", "卡片活性带缺少短胶囊（::after）"},
		{"@keyframes rollLong", "缺少长胶囊的轮回动画"},
		{"@keyframes rollShort", "缺少短胶囊的轮回动画"},
		{"animation:rollLong 2.2s linear infinite", "长胶囊的周期变了（越长越快是这套形态的关键）"},
		{"animation:rollShort 3.6s linear infinite", "短胶囊的周期变了（必须比长条慢，才有错落感）"},
		{"border-radius:99px", "胶囊不是圆头（两端没做圆角）"},
		// 简略行必须与卡片同机制、同参数：同一时刻两处位置才对得上
		{".rowstrip.live::before", "简略行的活性带没有跟卡片同机制（伪元素缺失）"},
		{".rowstrip.live::after", "简略行缺少短胶囊"},
		// 墙钟相位：列表每 900ms 整表重建，动画会重启，不接续就是"每次从左边重来"
		{"function bandPhase()", "缺少墙钟相位函数：列表重建后胶囊每次从零开始"},
		{"var BAND_CYCLE_MS = 39600", "相位周期不是两条胶囊周期的最小公倍数，长/短条不会同时接续"},
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
		{"keyframes flowstripes", "旧的等距硬边流动纹回来了（10px 高的条上就是一排脏虚线）"},
		{"background-size:16px 100%", "卡片活性带回退成等距重复纹"},
		{"background-size:8px 100%", "简略行活性带回退成等距重复纹"},
		// 简略行的贴底定位：一旦在这里覆写 position，细条会退回文档流、
		// 变成行内 flex 项，把文件名整段挤掉（09-16 学徒截图抓到过）。
		{".rowstrip.live{position:", "简略行活性带覆写了 position，会把文件名挤掉"},
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
