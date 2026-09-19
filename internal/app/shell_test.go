// GUI 外壳 HTML 的定点回归：只测"外壳是否把内嵌豁免键带上了"这一件事。
package app

import (
	"strings"
	"testing"
)

// TestShellHTMLInjectsEmbedKey 外壳的 iframe 地址必须带上内嵌豁免键。
//
// 服务端对 / 与 /settings 默认发 X-Frame-Options: DENY 防点击劫持，只有带对键的
// 嵌套才放行；而外壳经 SetHtml 加载、父文档是 opaque origin，永远是"被拒"的那一侧。
// 漏注入的代价是客户端主窗里只剩一个禁止图标——编译、启动、日志全部正常，
// 排查成本极高（这条正是踩过一次后补的）。
func TestShellHTMLInjectsEmbedKey(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	got := shellHTML(key)

	if strings.Contains(got, embedKeyPlaceholder) {
		t.Fatal("占位符未被替换：外壳拿不到键，内嵌的监控页会被 DENY")
	}
	if !strings.Contains(got, "var EMBED_KEY='"+key+"'") {
		t.Error("外壳里没有把键注入 EMBED_KEY 变量")
	}
	// 真正生效的是 iframe 地址，所以键必须出现在拼接 URL 的那一处
	if !strings.Contains(got, "'/?e='+EMBED_KEY") {
		t.Error("iframe 地址未拼接内嵌豁免键，监控页会被 X-Frame-Options 拒掉（主窗白屏）")
	}
}
