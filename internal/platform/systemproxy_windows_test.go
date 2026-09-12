//go:build windows

package platform

import "testing"

// TestNormalizeSystemProxy 注册表 ProxyServer 的各种已知格式。
func TestNormalizeSystemProxy(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:7890", "http://127.0.0.1:7890"},                                          // 常见：单一地址全协议
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890"},                                   // 已带 scheme
		{"  127.0.0.1:7890  ", "http://127.0.0.1:7890"},                                      // 容忍空白
		{"localhost:7890", "http://localhost:7890"},                                          // 域名形式
		{"ftp=1.1.1.1:21;http=127.0.0.1:7890;https=127.0.0.1:7891", "http://127.0.0.1:7891"}, // 分协议：优先 https=
		{"ftp=1.1.1.1:21;http=127.0.0.1:7890", "http://127.0.0.1:7890"},                      // 分协议：无 https= 回退 http=
		{"ftp=1.1.1.1:21", ""},                                                               // 只配了不支持的协议 → 直连
		{"socks5://x:1080", ""},                                                              // 不支持的 scheme → 直连
		{"https://x:8443", ""},                                                               // TLS-to-proxy 不支持 → 直连
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeSystemProxy(c.in); got != c.want {
			t.Fatalf("NormalizeSystemProxy(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestValidProxyAddr 代理地址校验：仅 http://host[:port]（CONNECT 隧道）。
func TestValidProxyAddr(t *testing.T) {
	cases := []struct {
		p    string
		want bool
	}{
		{"http://127.0.0.1:7890", true},
		{"http://host:8080", true},
		{"http://x", true}, // 无端口也合法（默认 80）
		{"socks5://x:1080", false},
		{"https://x:8443", false},
		{"ftp://x:21", false},
		{"not a url", false},
		{"", false},
	}
	for _, c := range cases {
		if got := ValidProxyAddr(c.p); got != c.want {
			t.Fatalf("ValidProxyAddr(%q)=%v want %v", c.p, got, c.want)
		}
	}
}

// TestSystemProxyAddrRealRegistry 直读本机注册表：只验证「不因读取失败而
// panic / 返回值经规范化（空或合法 http 地址）」，不假设用户代理开关状态。
func TestSystemProxyAddrRealRegistry(t *testing.T) {
	got := RealSystemProxyAddr()
	if got == "" {
		return // 系统未启用代理：合法结果
	}
	if !ValidProxyAddr(got) {
		t.Fatalf("注册表读数未规范化: %q", got)
	}
}

// TestSystemProxyWarning 只有 system 模式 + 注册表启用了不可用协议时才提示。
func TestSystemProxyWarning(t *testing.T) {
	if w := SystemProxyWarning("direct"); w != "" {
		t.Fatalf("direct 模式不应提示: %q", w)
	}
	if w := SystemProxyWarning("http://manual:1"); w != "" {
		t.Fatalf("手动模式不应提示: %q", w)
	}
	// system 模式但注册表未启用 → 不提示（由真实注册表状态决定，只验证不 panic）
	_ = SystemProxyWarning("system")
}
