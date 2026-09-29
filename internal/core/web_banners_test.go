// 告警横幅的**通用**不变量（与消息来源无关；persist.notice 专属的那几条在
// web_persist_notice_test.go）。
//
// 页面是 HTML 字符串，编译期发现不了，故用结构扫描钉住 —— 参照
// web_stopped_empty_test.go 的做法。两条都是"写下了规则但没人守"的典型：
// 实现里的注释讲得很清楚，删掉那段实现却没有任何测试会红。
package core

import (
	"strings"
	"testing"
)

// 横幅内容没变时不得重写 innerHTML：poll 每 900ms 跑一轮，每轮重写会让节点闪烁，
// 还会把用户正在选中的文本打断。
//
// 判据方向：删掉 setBanners 开头那句「签名相同就 return」⇒ 本条必红。
func TestWebBannersSkipRebuildWhenUnchanged(t *testing.T) {
	body := jsFuncBody(t, homePageHTML, "function setBanners(")

	if !strings.Contains(body, "bannerSig") {
		t.Fatal("setBanners 没有内容签名：每轮 poll 都会重写 innerHTML（节点闪烁）")
	}
	guard := strings.Index(body, "sig === bannerSig")
	write := strings.Index(body, "innerHTML")
	if guard < 0 {
		t.Fatalf("setBanners 没有「内容未变则跳过」的比较: %q", body)
	}
	if write < 0 {
		t.Fatal("setBanners 里找不到 innerHTML 赋值（实现换了？本测试锚点要跟着改）")
	}
	// 顺序反了等于没省：先写 DOM 再比较，每轮照样重建。
	if guard > write {
		t.Error("内容比较排在写 innerHTML 之后：每轮仍会重建 DOM")
	}
}

// 页面级告警**只有一个出口**：连不上本机服务也必须落到 #banners 上，
// 不许再另开一行（旧的页脚 #errline 已退役，且它只认一种颜色）。
//
// 判据方向：把 poll 的 catch 分支改调别的函数（或删掉）⇒ 本条必红。
func TestWebPageLevelAlertsGoThroughBanners(t *testing.T) {
	if strings.Contains(homePageHTML, `id="errline"`) {
		t.Error("#errline 又回来了：页面级告警被拆成两个出口")
	}

	i := strings.Index(homePageHTML, "} catch(e) {")
	if i < 0 {
		t.Fatal("找不到 poll 的 catch 分支（连接失败那条告警的家）—— 结构变了，锚点要跟着改")
	}
	seg := homePageHTML[i:]
	if j := strings.Index(seg, "\n}"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "setBanners(") {
		t.Error("连不上本机服务时没有走横幅：这条告警到不了用户眼前")
	}
}
