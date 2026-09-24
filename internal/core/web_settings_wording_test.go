// 设置页「改端口生效方式」的定点回归（需求单 v0.6.0 特性 1）。
//
// 背景：托盘菜单里的「启动 / 停止服务」被去掉后，端口改动的生效方式只剩"重启客户端"。
// 设置页原来两处文案都写着"托盘右键切换一次"——指向一个不再存在的操作，用户照做只会扑空。
// 文案是 HTML 字符串，编译期发现不了，所以用扫描钉住（参照 web_settings_ui_test.go）。
//
// 断言只取不含缩进的短片段：钉的是"有没有这个说法"，不是排版。
package core

import (
	"strings"
	"testing"
)

// abolishedTrayEntries 托盘菜单里已删掉的两个入口文案，页面里不许再提。
var abolishedTrayEntries = []string{"启动 / 停止服务", "在浏览器打开监控"}

// TestSettingsPageSaysRestartClientForPort 端口生效方式的说法。
func TestSettingsPageSaysRestartClientForPort(t *testing.T) {
	page := settingsPageHTML

	// 两处必须各自改到：只断言"页面里含新说法"会漏掉另一处没改的情况。
	for _, c := range []struct{ where, frag string }{
		{"端口说明", "改动端口需重启客户端生效"},
		{"保存后的提示", "需重启客户端生效：请退出并重新打开客户端"},
	} {
		if !strings.Contains(page, c.frag) {
			t.Errorf("web/settings.html 的%s未改成「重启客户端」——仍指向已删除的托盘菜单项（缺 %q）",
				c.where, c.frag)
		}
	}

	// 反向断言：页面里不该再有「托盘」。放行裸词是有意的——本页与托盘已无任何正当关联
	//（原先唯一的两处正是本次改掉的），再出现即意味着又指向了已删入口。
	if strings.Contains(page, "托盘") {
		t.Error("web/settings.html 仍提到「托盘」：托盘启停菜单已删除，指向它的文案会说谎")
	}
}

// TestNoServiceToggleEntryInWebPages 监控页与设置页都不许出现已删入口的文案。
func TestNoServiceToggleEntryInWebPages(t *testing.T) {
	for _, c := range []struct{ name, page string }{
		{"web/index.html", homePageHTML},
		{"web/settings.html", settingsPageHTML},
	} {
		for _, w := range abolishedTrayEntries {
			if strings.Contains(c.page, w) {
				t.Errorf("%s 出现已删除的入口文案 %q（v0.6.0 特性 1）", c.name, w)
			}
		}
	}
}
