// S1/S2：/status 的 persist.notice 必须在页面上被消费、**原样透传**，
// 并渲染成**告警横幅**（而不是塞进页脚一行、或被套用落盘失败的固定文案）。
//
// 后端改对（notice 活得过落盘）只是上半句；第四层不消费就等于没修 —— 与 R2 的
// fileMissing / gapSeconds"后端给了、前端零消费"完全同一形态。页面是 HTML 字符串，
// 编译期发现不了，用扫描钉不变量（参照 web_stopped_empty_test.go）。
package core

import (
	"strings"
	"testing"
)

// jsFuncBody 从页面里切出某个 JS 函数体（`function name(` 到下一个顶格 `}`）。
// 找不到就 Fatal —— 切分失效的断言会恒真，必须当场炸掉。
func jsFuncBody(t *testing.T, page, signature string) string {
	t.Helper()
	i := strings.Index(page, signature)
	if i < 0 {
		t.Fatalf("页面里找不到 %q（结构变了，本测试锚点要跟着改）", signature)
	}
	seg := page[i:]
	if j := strings.Index(seg, "\n}"); j >= 0 {
		return seg[:j]
	}
	t.Fatalf("页面里 %q 找不到顶格的结束花括号", signature)
	return ""
}

// pollSeg 返回 poll() 的函数体（到下一个顶格 function 为止）。
func pollSeg(t *testing.T) string {
	t.Helper()
	i := strings.Index(homePageHTML, "function poll()")
	if i < 0 {
		t.Fatal("找不到 poll() —— 页面结构变了，本测试锚点要跟着改")
	}
	seg := homePageHTML[i:]
	if j := strings.Index(seg, "\nfunction el("); j > 0 {
		seg = seg[:j]
	}
	return seg
}

func TestWebConsumesPersistNotice(t *testing.T) {
	seg := pollSeg(t)

	if !strings.Contains(seg, "d.persist.notice") {
		t.Error("poll() 没有读 d.persist.notice：后端给的留档通知到不了用户眼前（S1 只修了一半）")
	}
	// notice 必须**原样**当成一条独立横幅的正文，而不是被塞进"断点状态写入磁盘失败"那句
	// —— S2 的缺陷正是把两种不同语义硬编码成同一句话。
	if !strings.Contains(seg, "text:d.persist.notice") {
		t.Error("poll() 没有把 notice 原样作为横幅正文：会被硬编码文案覆盖（S2）")
	}
	// 反向：notice 不该被"ok === false"这个条件挡住 —— 留档时 ok 恰恰是 true。
	if strings.Contains(seg, "d.persist.ok === false && d.persist.notice") {
		t.Error("notice 被挂在 ok===false 下：留档场景 ok 为 true，通知永远不显示")
	}
	// 接线：poll() 必须真的把收集到的消息交给横幅渲染函数。
	if !strings.Contains(seg, "setBanners(pmsg)") {
		t.Error("poll() 没有调用 setBanners(pmsg)：消息收集了却没渲染（R2 同型：后端给了、前端零消费）")
	}
}

// notice 是"需要用户知晓的一次性事件"，视觉上必须与"落盘失败"区分开 ——
// 否则又回到"两类语义一个长相"的老样子。
func TestWebPersistNoticeIsWarnTier(t *testing.T) {
	seg := pollSeg(t)
	if !strings.Contains(seg, "{kind:'warn', text:d.persist.notice}") {
		t.Error("persist.notice 没被标成 warn 档：与落盘失败（err）同色就分不出轻重")
	}
}

// 横幅必须落在**标题正下方**（学徒 2026-09-29 拍板的 B 方案），而不是页脚。
func TestWebBannersSitUnderTitle(t *testing.T) {
	page := homePageHTML
	hd := strings.Index(page, `class="hd"`)
	bn := strings.Index(page, `id="banners"`)
	st := strings.Index(page, `id="stats"`)
	if hd < 0 || bn < 0 || st < 0 {
		t.Fatalf("锚点找不到：hd=%d banners=%d stats=%d（页面结构变了）", hd, bn, st)
	}
	if !(hd < bn && bn < st) {
		t.Errorf("横幅不在标题正下方：hd=%d banners=%d stats=%d，期望 hd < banners < stats", hd, bn, st)
	}
	if strings.Contains(page, `id="errline"`) {
		t.Error("页脚 errline 应已退役（告警统一走横幅），它仍在页面上")
	}
}

// 多条告警必须**各占一张卡**（纵向堆叠），不许再用分隔符拼成一行 ——
// 这正是学徒打回的那版（"都太丑了"）。
func TestWebBannersStackPerItem(t *testing.T) {
	body := jsFuncBody(t, homePageHTML, "function setBanners(")
	if !strings.Contains(body, "list.map(") {
		t.Error("横幅没有逐条渲染：多条告警会被拼成一行")
	}
	if !strings.Contains(body, `class="bn `) {
		t.Error("横幅没有用 .bn 卡片模板（左侧竖色条 + 圆角卡片）")
	}
	if strings.Contains(body, "join('　｜　')") || strings.Contains(body, `join("　｜　")`) {
		t.Error("横幅仍在用全角竖线拼接多条消息：那是被学徒打回的旧形态")
	}
	// 空则整块不占位（CSS 里 #banners:empty{display:none}）。
	if !strings.Contains(homePageHTML, "#banners:empty{display:none}") {
		t.Error("#banners 空态没有隐藏：没有告警时会留下一块空白")
	}
}

// 文案里带文件名/路径（gocatcher_state.json.<时间戳>.corrupt 等），
// 拼进 innerHTML 前必须转义 —— 与扩展侧 xss-guard 守同一件事。
func TestWebBannerEscapesText(t *testing.T) {
	body := jsFuncBody(t, homePageHTML, "function setBanners(")
	if !strings.Contains(body, "esc(b.text)") {
		t.Error("横幅正文没经 esc() 转义就拼进 innerHTML：文案含文件名/路径，是不可信输入")
	}
}
