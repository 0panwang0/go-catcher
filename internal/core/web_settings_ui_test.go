// 设置页代理提示的定点回归（B2-F3 / P1-3）。
//
// 背景：系统代理启用了、但协议不受支持时（如 socks5），引擎逐连接静默降级为直连。
// 用户以为流量走了代理，实际目标站点看到的是真实 IP。启动日志里那行 [!] 早就有，
// 缺的是界面——syncSysHint() 当时是空实现，且调用点传的是 systemProxy，
// 服务端返回的 systemProxyWarning 从来没被读过。
//
// 与 web_live_ui_test.go 同理：页面是 HTML 字符串，编译期发现不了，前端又没有运行时
// 测试，所以用扫描把不变量钉住（参照 embedpage_test.go 的做法）。
// 断言只取不含缩进的短片段：这里钉的是"有没有这个行为"，不是排版。
package core

import (
	"strings"
	"testing"
)

func TestSettingsUISurfacesSystemProxyWarning(t *testing.T) {
	page := settingsPageHTML

	mustHave := []struct{ frag, why string }{
		{"d.systemProxyWarning", "设置页没读服务端的 systemProxyWarning —— 静默降级在界面上永远不会出现"},
		{"function syncSysHint()", "缺少提示渲染函数"},
		{"warnhint", "警示没有独立样式：降级提示混在普通说明里，等于没说"},
		{"跟随系统：当前系统代理", "跟到了代理却不回显地址，用户没法确认真的跟上了"},
		{"跟随系统：系统未启用代理，当前等价于直连", "系统没开代理时不说清楚，用户会以为流量走了代理"},
		{"proxyMode !== 'system'", "提示没按模式收敛：直连/手动模式下也会显示（那两种走哪儿由地址栏说明）"},
	}
	for _, c := range mustHave {
		if !strings.Contains(page, c.frag) {
			t.Errorf("web/settings.html 缺少 %q —— %s", c.frag, c.why)
		}
	}

	// 反向断言：切模式必须重算提示。原来 syncSysHint 只在载入时调一次，点「跟随系统」
	// 提示仍是旧的。压掉空白再比对，免得后面纯缩进调整把这行断言误伤。
	flat := strings.Join(strings.Fields(page), " ")
	if !strings.Contains(flat, "(proxyMode === 'manual') ? '' : 'none'; syncSysHint();") {
		t.Error("web/settings.html 的 syncProxySeg() 没有重算提示：切换代理模式后提示不会更新")
	}
	// 反向断言：注册表里的 ProxyServer 是不可信输入，禁止把字符串拼成 HTML 塞进 DOM。
	// 盯的是"真的往 DOM 里塞 HTML"的写法，而不是 innerHTML 这个词——注释里说明
	// 为什么不用它是正常的（本页就有一处），放行裸词会把说明文字也判成违规。
	for _, bad := range []string{".innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(page, bad) {
			t.Errorf("web/settings.html 出现 %q：注册表值是不可信输入，拼 HTML 字符串等于给自己挖 XSS", bad)
		}
	}
}
