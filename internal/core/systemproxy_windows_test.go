//go:build windows

package core

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
		if got := normalizeSystemProxy(c.in); got != c.want {
			t.Fatalf("normalizeSystemProxy(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestEffectiveProxySystemFollows system 模式每次解析都透传注册表读数
// （注入 stub），手动/直连模式不碰注册表。
func TestEffectiveProxySystemFollows(t *testing.T) {
	oldFn, oldProxy := testStd.systemProxyAddrFn, testStd.getProxyAddr()
	t.Cleanup(func() {
		testStd.systemProxyAddrFn = oldFn
		testStd.setProxyAddr(oldProxy)
	})

	testStd.setProxyAddr("system")

	// 系统代理已启用：透传注册表读数
	testStd.systemProxyAddrFn = func() string { return "http://10.1.1.1:9999" }
	if got := testStd.effectiveProxy(); got != "http://10.1.1.1:9999" {
		t.Fatalf("system+已启用 effectiveProxy=%q", got)
	}
	if testStd.isDirectProxy() {
		t.Fatal("system+已启用 不应判定直连")
	}

	// 系统代理未启用：直连（Clash 关掉系统代理开关的场景）
	testStd.systemProxyAddrFn = func() string { return "" }
	if got := testStd.effectiveProxy(); got != "" {
		t.Fatalf("system+未启用 effectiveProxy=%q", got)
	}
	if !testStd.isDirectProxy() {
		t.Fatal("system+未启用 应判定直连")
	}

	// 手动模式：不经过 systemProxyAddr
	testStd.systemProxyAddrFn = func() string { t.Fatal("手动模式不应读系统代理"); return "" }
	testStd.setProxyAddr("http://manual:1")
	if got := testStd.effectiveProxy(); got != "http://manual:1" {
		t.Fatalf("手动模式 effectiveProxy=%q", got)
	}

	// 大小写不敏感的 System 也走注册表
	testStd.setProxyAddr("  SYSTEM ")
	testStd.systemProxyAddrFn = func() string { return "http://10.1.1.1:9999" }
	if got := testStd.effectiveProxy(); got != "http://10.1.1.1:9999" {
		t.Fatalf("SYSTEM（含空白）effectiveProxy=%q", got)
	}
}

// TestSystemProxyAddrRealRegistry 直读本机注册表：只验证「不因读取失败而
// panic / 返回值经规范化（空或合法 http 地址）」，不假设用户代理开关状态。
func TestSystemProxyAddrRealRegistry(t *testing.T) {
	got := realSystemProxyAddr()
	if got == "" {
		return // 系统未启用代理：合法结果
	}
	if !validProxyAddr(got) {
		t.Fatalf("注册表读数未规范化: %q", got)
	}
}
