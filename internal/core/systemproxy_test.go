//go:build windows

package core

import "testing"

// TestEffectiveProxySystemFollows system 模式每次解析都透传注册表读数
// （注入桩函数），手动/直连模式不碰注册表。
// 平台实现（platform.RealSystemProxyAddr）随平台层拆包移出，这里只验证
// Runtime 对 systemProxyAddrFn 注入点的行为。
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
