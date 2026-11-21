// 空列表页的空态（2026-09-29 学徒在四版布局稿里选定方案 C，见 `docs/ui-empty-preview.html`）：
// **吃满列表区** + 下载图标 + 主副两行文案。
//
// 页面是 HTML 字符串，编译期发现不了，故用结构扫描钉不变量（参照 web_stopped_empty_test.go）。
// 两条容易踩的：
//   - 撑满挂在 `#list:has(> .empty)` 上 —— 若无条件把 `#list` 变 flex，非空态的子项之间
//     不再折叠 margin，卡片与日期分割线的间距会整体变大（静默的视觉回归，没人会报）。
//   - `.empty` 必须是纵向 flex —— 少了它，图标与两行文案会被摆成一行（真机渲染当场抓到过：
//     第一版只断"有没有图标"，挤成一行照样绿）。
package core

import (
	"strings"
	"testing"
)

// cssRuleBody 取一条 CSS 规则的声明块（`sel` 之后到其后的第一个 `}`）。
func cssRuleBody(t *testing.T, page, sel string) string {
	t.Helper()
	i := strings.Index(page, sel)
	if i < 0 {
		t.Fatalf("页面里找不到 CSS 规则 %q（结构变了，锚点要跟着改）", sel)
	}
	seg := page[i+len(sel):]
	j := strings.Index(seg, "}")
	if j < 0 {
		t.Fatalf("规则 %q 没有结束花括号", sel)
	}
	return seg[:j]
}

// emptyStateHTML 取 render() 里拼空态的那段（`'<div class="empty">'` 到 `</div>';`）。
func emptyStateHTML(t *testing.T) string {
	t.Helper()
	const start = `'<div class="empty">'`
	i := strings.Index(homePageHTML, start)
	if i < 0 {
		t.Fatalf("找不到空态拼接的起点 %s（结构变了，锚点要跟着改）", start)
	}
	seg := homePageHTML[i:]
	end := strings.Index(seg, `</div>';`)
	if end < 0 {
		t.Fatal(`找不到空态拼接的结尾 </div>';`)
	}
	return seg[:end]
}

// 撑满的整条链：body / .wrap 竖向拉伸 → 空态时 #list 变 flex → .empty 用 flex:1 吃掉剩余高度。
func TestWebEmptyStateFillsListArea(t *testing.T) {
	if !strings.Contains(cssRuleBody(t, homePageHTML, "body{"), "display:flex") {
		t.Error("body 不是竖向 flex：.wrap 吃不到剩余高度，空态框撑不满")
	}
	if !strings.Contains(cssRuleBody(t, homePageHTML, ".wrap{"), "flex:1") {
		t.Error(".wrap 没有 flex:1：不会吃满视口高度")
	}
	if !strings.Contains(cssRuleBody(t, homePageHTML, "#list:has(> .empty){"), "display:flex") {
		t.Error("空态时 #list 没有变成 flex：.empty 的 flex:1 不起作用，框撑不满")
	}
	if !strings.Contains(cssRuleBody(t, homePageHTML, ".empty{"), "flex:1") {
		t.Error(".empty 没有 flex:1：空态框撑不满列表区")
	}
}

// 回归守卫：`#list` 的**无条件**规则里不许出现 display:flex ——
// 非空态一旦也被 flex 化，子项之间不再折叠 margin，卡片与日期分割线的间距会整体变大。
// 撑满只能挂在 `:has(> .empty)` 上。
func TestWebListOnlyFlexesWhenEmpty(t *testing.T) {
	body := cssRuleBody(t, homePageHTML, "#list{")
	if strings.Contains(body, "display:flex") {
		t.Errorf("#list 被无条件 flex 化：非空态间距会变（不再折叠 margin）: %q", body)
	}
	if !strings.Contains(homePageHTML, "#list:has(> .empty){") {
		t.Error("找不到 #list:has(> .empty)：空态撑满的挂钩没了（或写成了别的形态）")
	}
}

// 空态内部的形态：图标最上、主文案居中、副文案在下 —— 靠 .empty 的 flex-direction:column。
// 少了 column（或被改成 row），三段会挤成一行，而"有没有图标"这类断言照样绿。
func TestWebEmptyStateStacksVertically(t *testing.T) {
	if !strings.Contains(cssRuleBody(t, homePageHTML, ".empty{"), "flex-direction:column") {
		t.Error(".empty 不是纵向 flex：图标与两行文案会被摆成一行")
	}
	seg := emptyStateHTML(t)
	ic := strings.Index(seg, `class="emic"`)
	mt := strings.Index(seg, `class="emt"`)
	ms := strings.Index(seg, `class="ems"`)
	if ic < 0 || mt < 0 || ms < 0 {
		t.Fatalf("空态三段不全（emic=%d emt=%d ems=%d）—— 锚点要跟着改", ic, mt, ms)
	}
	if !(ic < mt && mt < ms) {
		t.Errorf("空态三段顺序不对：emic=%d emt=%d ems=%d，期望 图标 < 主文案 < 副文案", ic, mt, ms)
	}
}

// 文案分主副两行；副文案仍是原来那句（别顺手改词 —— 它与扩展侧/文档里的说法是一套）。
func TestWebEmptyStateWordingSplit(t *testing.T) {
	seg := emptyStateHTML(t)
	if !strings.Contains(seg, ">暂无下载任务<") {
		t.Error("主文案不是「暂无下载任务」")
	}
	if !strings.Contains(seg, "到视频页点「下载该视频」开始") {
		t.Error("副文案变了（应为「到视频页点「下载该视频」开始」）")
	}
	if strings.Contains(seg, "暂无下载任务。<br>") {
		t.Error("文案仍是旧的 `<br>` 单块写法：主副没有分层")
	}
}
