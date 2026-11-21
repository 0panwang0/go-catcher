// CLI 参数解析测试。
package core

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestParseCLI 全参数解析与默认值回退。
// G1 后 ParseCLI 的默认值是内置字面量（不再读任何运行时状态）；
// RunCLI 会创建一次性 Runtime 并应用解析结果。
func TestParseCLI(t *testing.T) {
	o, err := ParseCLI([]string{
		"--url=https://x/a.m3u8", "--referer=https://ref.example",
		"-c", "5", "-o", "out.ts", "--limit", "3", "--server", "--port", "9999",
	})
	if err != nil {
		t.Fatalf("合法参数不应报错: %v", err)
	}
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
	o2, err2 := ParseCLI([]string{"--url=https://y/b.m3u8"})
	if err2 != nil {
		t.Fatalf("合法参数不应报错: %v", err2)
	}
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
		o, err := ParseCLI([]string{arg})
		if err != nil {
			t.Fatalf("%q 不应报错: %v", arg, err)
		}
		if !o.NativeHostMode {
			t.Fatalf("%q 未识别为宿主模式: %+v", arg, o)
		}
		// 必须停在宿主模式：不能被当成 CLI 直下（URL 为空会走用法提示）或其它角色
		if o.URL != "" || o.ServerMode || o.TrayMode || o.InstallNativeHost || o.UninstallNativeHost {
			t.Fatalf("%q 误落到其它模式: %+v", arg, o)
		}
	}

	// 显式开关仍然有效（手工排障用）
	if o, err := ParseCLI([]string{"--native-host"}); err != nil || !o.NativeHostMode {
		t.Fatalf("--native-host 未生效: o=%+v err=%v", o, err)
	}

	// 反向：命令行下载与服务模式不受影响
	for _, args := range [][]string{
		{"--url=https://example.com/a.m3u8"},
		{"--server", "--port", "7891"},
		{"--install-native-host"},
		{},
	} {
		o, err := ParseCLI(args)
		if err != nil {
			t.Fatalf("%v 不应报错: %v", args, err)
		}
		if o.NativeHostMode {
			t.Fatalf("%v 被误判为宿主模式: %+v", args, o)
		}
	}
}

// TestParseCLIUnknownFlagDoesNotExit：R10 的行为断言 —— 未知参数只返回错误，不自己退出。
//
// 为什么必须起子进程：这条断言的内容正是"**不**调用 os.Exit"。若在本进程里直接调
// ParseCLI("--definitely-not-a-flag")，一旦回归成 flag.ExitOnError，整个测试二进制
// 会被 os.Exit(2) 带走 —— 只能看到 "exit status 2"，包里其余用例一起消失（掩盖别的失败）。
// 放进子进程，退出码与 stderr 都成了可读的证据。
func TestParseCLIUnknownFlagDoesNotExit(t *testing.T) {
	const probeEnv = "GOCATCHER_CLI_UNKNOWN_FLAG_PROBE"
	if os.Getenv(probeEnv) == "1" {
		_, err := ParseCLI([]string{"--definitely-not-a-flag"})
		fmt.Fprintf(os.Stderr, "PROBE_RESULT err=%v", err)
		os.Exit(0) // 能走到这里就证明没有 ExitOnError
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestParseCLIUnknownFlagDoesNotExit$")
	cmd.Env = append(os.Environ(), probeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("未知参数导致进程异常退出（回归成 ExitOnError 了？）: err=%v, out=%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "PROBE_RESULT err=") {
		t.Fatalf("子进程没跑到探针点: %s", got)
	}
	if strings.Contains(got, "PROBE_RESULT err=<nil>") {
		t.Errorf("ParseCLI 对未知参数没有返回错误: %s", got)
	}
}

// TestMainReportsParseErrorAfterAttachingConsole：结构守卫（R10 的另一半）。
//
// ParseCLI 不再自己退出只是修了一半：错误得有人报，且必须在**挂好控制台之后**报，
// 否则提示依旧落进尚不存在的 stderr。这里钉住 main.go 里 AttachParentConsole 早于
// 报错分支（以 core.PrintUsage 为标志：报错分支在 switch 之前，是它的首次出现）。
func TestMainReportsParseErrorAfterAttachingConsole(t *testing.T) {
	body := strings.Join(sourceFuncBody(t, "../../cmd/go-catcher/main.go", "func main()"), "\n")
	iAttach := strings.Index(body, "AttachParentConsole")
	iUsage := strings.Index(body, "PrintUsage")
	if iAttach < 0 || iUsage < 0 {
		t.Fatalf("main.go 里找不到 AttachParentConsole(%d) / PrintUsage(%d)（改了结构要同步改本守卫）", iAttach, iUsage)
	}
	if iAttach > iUsage {
		t.Error("main.go 在挂控制台之前就报解析错误 —— 提示会落进不存在的控制台（R10）")
	}
}
