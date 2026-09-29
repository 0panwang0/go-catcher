// S1/S2：/status 的 persist.notice 必须在页面上被消费，且**不得**被套用落盘失败的
// 固定文案。
//
// 后端改对（notice 活得过落盘）只是上半句；第四层不消费就等于没修 —— 与 R2 的
// fileMissing / gapSeconds"后端给了、前端零消费"完全同一形态。页面是 HTML 字符串，
// 编译期发现不了，用扫描钉不变量（参照 web_stopped_empty_test.go）。
package core

import (
	"strings"
	"testing"
)

func TestWebConsumesPersistNotice(t *testing.T) {
	page := homePageHTML

	i := strings.Index(page, "function poll()")
	if i < 0 {
		t.Fatal("找不到 poll() —— 页面结构变了，本测试锚点要跟着改")
	}
	seg := page[i:]
	if j := strings.Index(seg, "\nfunction el("); j > 0 {
		seg = seg[:j]
	}

	if !strings.Contains(seg, "d.persist.notice") {
		t.Error("poll() 没有读 d.persist.notice：后端给的留档通知到不了用户眼前（S1 只修了一半）")
	}
	// notice 必须**原样**推成一条独立消息，而不是被塞进"断点状态写入磁盘失败"那句
	// —— S2 的缺陷正是把两种不同语义硬编码成同一句话。
	if !strings.Contains(seg, "pmsg.push(d.persist.notice)") {
		t.Error("poll() 没有把 notice 原样推入消息列表：会被硬编码文案覆盖（S2）")
	}
	// 反向：notice 不该被"ok === false"这个条件挡住 —— 留档时 ok 恰恰是 true。
	if strings.Contains(seg, "d.persist.ok === false && d.persist.notice") {
		t.Error("notice 被挂在 ok===false 下：留档场景 ok 为 true，通知永远不显示")
	}
}
