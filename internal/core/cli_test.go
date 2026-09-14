// CLI 参数解析测试。
package core

import "testing"

// TestParseCLI 全参数解析与默认值回退。
// G1 后 ParseCLI 的默认值是内置字面量（不再读任何运行时状态）；
// RunCLI 会创建一次性 Runtime 并应用解析结果。
func TestParseCLI(t *testing.T) {
	o := ParseCLI([]string{
		"--url=https://x/a.m3u8", "--referer=https://ref.example",
		"-c", "5", "-o", "out.ts", "--limit", "3", "--server", "--port", "9999",
	})
	if o.URL != "https://x/a.m3u8" {
		t.Fatalf("URL=%q", o.URL)
	}
	if o.Referer != "https://ref.example" {
		t.Fatalf("Referer=%q", o.Referer)
	}
	if o.Concurrency != 5 || o.Output != "out.ts" || o.Limit != 3 {
		t.Fatalf("o=%+v", o)
	}
	if !o.ServerMode || o.Port != 9999 {
		t.Fatalf("server/port 解析错误: %+v", o)
	}

	// 未指定参数回退内置默认
	o2 := ParseCLI([]string{"--url=https://y/b.m3u8"})
	if o2.Proxy != "system" || o2.Output != "output.ts" || o2.Concurrency != defaultConfig().SegConcurrency {
		t.Fatalf("默认值错误: %+v", o2)
	}
	if o2.ServerMode || o2.Limit != 0 || o2.Port != 0 {
		t.Fatalf("默认零值错误: %+v", o2)
	}
}

// TestParseCLIBrowserHostInvocation 浏览器拉起宿主时不会带 --native-host：
// 宿主清单的 path 只能写可执行文件，浏览器把调用方 origin 当第一个参数传进来。
// 认不出这个形态 ⇒ 进程打印用法后立刻退出 ⇒ 浏览器侧"宿主已退出"，扩展永远唤不起程序。
// 2026-09-14 用户实测到的"启动不了"就是这个回归，故锁死。
func TestParseCLIBrowserHostInvocation(t *testing.T) {
	const id = "jodjnfkmjplkofpgmjlmiiadnkofiopk"

	for _, arg := range []string{
		"chrome-extension://" + id + "/",
		"chrome-extension://" + id,       // 无尾斜杠
		"CHROME-EXTENSION://" + id + "/", // 大小写不敏感
	} {
		o := ParseCLI([]string{arg})
		if !o.NativeHostMode {
			t.Fatalf("%q 未识别为宿主模式: %+v", arg, o)
		}
		// 必须停在宿主模式：不能被当成 CLI 直下（URL 为空会走用法提示）或其它角色
		if o.URL != "" || o.ServerMode || o.TrayMode || o.InstallNativeHost || o.UninstallNativeHost {
			t.Fatalf("%q 误落到其它模式: %+v", arg, o)
		}
	}

	// 显式开关仍然有效（手工排障用）
	if o := ParseCLI([]string{"--native-host"}); !o.NativeHostMode {
		t.Fatalf("--native-host 未生效: %+v", o)
	}

	// 反向：命令行下载与服务模式不受影响
	for _, args := range [][]string{
		{"--url=https://example.com/a.m3u8"},
		{"--server", "--port", "7891"},
		{"--install-native-host"},
		{},
	} {
		o := ParseCLI(args)
		if o.NativeHostMode {
			t.Fatalf("%v 被误判为宿主模式: %+v", args, o)
		}
	}
}
