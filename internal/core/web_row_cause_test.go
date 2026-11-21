// 简略模式也要说清「为什么中断 / 为什么失败」（学徒 2026-09-20 23:50 选方案 A）。
//
// 缺陷形态（学徒 09-20 23:47 截图抓到）：详细模式的 stageText() 早就把
// "为什么中断、缺口多久"写清楚了，但简略模式 rowCard() 里除了 restartNote
// 那一个 ⚠，原因**一概不渲染** —— 同一个任务，展开卡片能读到原因，折叠成
// 一行就只剩徽章「已中断」。又一次头号缺陷形态：**行为是对的，界面把事实藏起来了**。
//
// 方案 A 的三条落地要点，每条对应下面一组断言（各自都有让它可以变红的扰动）：
//
//	① 原因文本**同源**：stageText() 与 rowCard() 都调 causeText()——两处各拼一份
//	   就会出现"改一处漏一处"，而这次的两处本来就曾经不一致；
//	② ⚠ 的条件必须覆盖"有原因"，不只是 restartNote（退回旧写法必红）；
//	③ 全文同时挂 ⚠ 与**整行**——鼠标落在行内任意处都能看到，不用对准 12px 的图标。
//
// 页面是 HTML 字符串、前端没有运行时测试，所以用扫描钉住（参照 web_live_ui_test.go）。
package core

import (
	"strings"
	"testing"
)

// rowCauseRegions 取出本组测试要断言的四个区域。任一锚点取不到时直接 Fatal ——
// 锚点失效必须显式失败，不能退化成"什么都没检查、然后全绿"。
//
// 区域按函数逐一闭合（stage 收到 liveTitle 为止），否则相邻函数会被卷进来，
// 断言就在别人的代码里找命中了。
func rowCauseRegions(t *testing.T) (cause, stage, rowNote, row string) {
	t.Helper()
	page := homePageHTML

	iCause := strings.Index(page, "function causeText(t){")
	iStage := strings.Index(page, "function stageText(t){")
	iLive := strings.Index(page, "function liveTitle(t){")
	iNote := strings.Index(page, "function noteRow(t){")
	iRowNote := strings.Index(page, "function rowNote(t){")
	iRow := strings.Index(page, "function rowCard(t){")
	iScroll := strings.Index(page, "// —— 名称滚动")
	if iCause < 0 || iStage < 0 || iLive < 0 || iNote < 0 || iRowNote < 0 || iRow < 0 || iScroll < 0 {
		t.Fatalf("页面结构变了，锚点取不到（cause=%d stage=%d live=%d noteRow=%d rowNote=%d rowCard=%d scroll=%d）",
			iCause, iStage, iLive, iNote, iRowNote, iRow, iScroll)
	}
	if !(iCause < iStage && iStage < iLive && iLive < iNote && iNote < iRowNote && iRowNote < iRow && iRow < iScroll) {
		t.Fatalf("函数次序变了（期望 causeText → stageText → liveTitle → noteRow → rowNote → rowCard → 名称滚动），本测试锚点要跟着改")
	}
	return page[iCause:iStage], page[iStage:iLive], page[iRowNote:iRow], page[iRow:iScroll]
}

// TestRowNoteCoversCauseNotJustRestartNote 简略模式的 ⚠ 必须覆盖"有原因"，不只是重下提示。
//
// 这条是本轮的核心：改之前 rowCard 里唯一的原因通道是 `t.restartNote ?`，
// 于是中断/失败的任务在简略模式里只有一个「已中断」徽章。
func TestRowNoteCoversCauseNotJustRestartNote(t *testing.T) {
	cause, stage, rowNoteSrc, row := rowCauseRegions(t)

	// ① 同源：两个出口最终都必须落到 causeText()。链条是
	//   详细模式 stageText → causeText；简略模式 rowCard → rowNote → causeText。
	// 每一环单独钉住 —— 断在任何一环，症状都是"其中一处看不到原因"。
	if !strings.Contains(stage, "causeText(t)") {
		t.Error("详细模式的 stageText() 没调 causeText() —— 两处原因文案会各自漂移")
	}
	if !strings.Contains(row, "rowNote(t)") {
		t.Error("简略模式的 rowCard() 没接上 rowNote() —— 折叠成一行后又看不到中断/失败原因了")
	}

	// ② rowNote() 必须同时消费两个来源（拼接，不是二选一）
	if !strings.Contains(rowNoteSrc, "t.restartNote") {
		t.Error("rowNote() 没有消费 restartNote —— 恢复提示在简略模式消失了")
	}
	if !strings.Contains(rowNoteSrc, "causeText(t)") {
		t.Error("rowNote() 没有消费 causeText() —— 中断/失败原因在简略模式消失了")
	}

	// 反向：rowCard 的 ⚠ 不许退回"只认 restartNote"的旧写法（这正是缺陷的原始形态）
	if strings.Contains(row, `t.restartNote ? '<span class="rowwarn"`) {
		t.Error("rowCard() 的 ⚠ 又只认 restartNote 了 —— 中断/失败原因会重新一个字都看不到")
	}

	// ④ 原因文本必须覆盖三类事实：中断态、失败原因、时间轴缺口
	for _, c := range []struct{ frag, why string }{
		{"t.interrupted", "causeText() 没有区分中断态 —— 会把中断拼成失败"},
		{"t.error", "causeText() 没有读失败原因"},
		{"gapTail(t)", "causeText() 没有带出时间轴缺口 —— 缺了多久会被藏起来"},
	} {
		if !strings.Contains(cause, c.frag) {
			t.Errorf("causeText() 缺少 %q —— %s", c.frag, c.why)
		}
	}
}

// TestRowCauseTitleIsOnBothIconAndRow 全文必须同时挂 ⚠ **与整行**。
//
// 学徒选 A 时定的就是这个：鼠标落在行内任意处都能读到全文，不用对准那个 12px 的小图标。
// 只挂 ⚠ 等于把可发现性全押在一个小图标上；只挂整行则"⚠ 提示这里有话说"的暗示失效。
//
// 用出现次数（恰好 2 次）而不是 Contains：少一处要红，多一处也说明还有别的拼接来源在乱挂。
func TestRowCauseTitleIsOnBothIconAndRow(t *testing.T) {
	_, _, _, row := rowCauseRegions(t)

	const want = `title="'+esc(note)+'"`
	if got := strings.Count(row, want); got != 2 {
		t.Errorf("简略模式把原因挂进 title 的地方有 %d 处，期望恰好 2 处（⚠ 一处 + 整行一处）—— "+
			"少一处就有鼠标落不到的角落，多一处说明还有第二个拼接近似文案的来源", got)
	}

	// ⚠ 元素本身必须还在（否则"⚠ 提示这里有话说"的暗示没了，只剩悬停才发现的 title）
	if !strings.Contains(row, `'<span class="rowwarn" title="'+esc(note)+'">⚠</span>'`) {
		t.Error("简略模式缺少 ⚠ 标记元素 —— 整行 title 是隐形的，用户不会知道这里有话要说")
	}

	// 反向：不许出现"只把 restartNote 挂上去、原因不挂"的降级写法
	if strings.Contains(row, `title="'+esc(t.restartNote)+'"`) {
		t.Error("简略模式的 title 只挂了 restartNote —— 中断/失败原因不会出现在悬停里")
	}
}

// TestRowCauseDetailStaysEscaped 原因文本必须经 esc() 再进 HTML。
//
// 原因里可能带源站返回的字符串（URL、错误消息），直接内插进 title/属性就是注入面。
// 这条钉的是"两个出口都转义"，不是"某个函数写了 esc"。
func TestRowCauseDetailStaysEscaped(t *testing.T) {
	cause, stage, _, row := rowCauseRegions(t)

	// 详细模式出口转义（stageText 把纯文本包进 span）
	if !strings.Contains(stage, "esc(cause)") {
		t.Error("stageText() 没有把 causeText() 的结果转义就内插 —— 原因里可能带源站返回的字符串")
	}
	// 简略模式出口转义（⚠ 与整行 title 两处）
	if strings.Count(row, "esc(note)") < 2 {
		t.Error("简略模式的 title 没有转义原因文本 —— 属性内插是注入面")
	}
	// causeText 自身返回纯文本：不许夹带标签，否则会被 esc() 转成可见的 &lt;span&gt;
	if strings.Contains(cause, "<span") {
		t.Error("causeText() 返回了带标签的 HTML —— 它约定返回纯文本，转义后会变成可见的尖括号")
	}
}
