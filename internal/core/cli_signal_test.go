// R9：CLI 的 Ctrl+C 必须覆盖每一条下载路径。
//
// 缺陷形态：`signal.NotifyContext` 原先只在 isLive 分支里注册，点播与直链收到的
// SIGINT 无人接管 —— 走 Go 默认处置把进程直接杀掉，RunCLI 里的 defer 一个都不跑。
// 修法的**全部内容**就是"提到三条下载路径之前，且只注册一次"，所以断言钉住这两件事。
package core

import (
	"strings"
	"testing"
)

// TestRunCLIRegistersInterruptBeforeAllDownloadPaths：结构守卫（R9）。
//
// 位置判据取"早于每一个下载入口"，而不是早于某句 `if isLive`：
// 后者在 RunCLI 里出现多次（试片提示 / 下载分支 / 校验分支），取首个等于没钉住。
func TestRunCLIRegistersInterruptBeforeAllDownloadPaths(t *testing.T) {
	body := strings.Join(sourceFuncBody(t, "cli.go", "func RunCLI(o CLIOptions) int"), "\n")

	const marker = "signal.NotifyContext"
	if n := strings.Count(body, marker); n != 1 {
		t.Fatalf("%s 在 RunCLI 里出现 %d 次，应为 1 次（多份订阅冗余且易误读）", marker, n)
	}
	iNotify := strings.Index(body, marker)

	for _, entry := range []string{"job.downloadDirect(", "streamDownload(", "job.liveDownload("} {
		iEntry := strings.Index(body, entry)
		if iEntry < 0 {
			t.Fatalf("RunCLI 里找不到 %s（改了结构就要同步改本守卫）", entry)
		}
		if iNotify > iEntry {
			t.Errorf("%s 注册在 %s 之后 —— 该路径的 Ctrl+C 不会被接收（R9）", marker, entry)
		}
	}

	// 注册了却仍把 context.Background() 传给下载路径 = 等于没接。
	// RunCLI 里唯一允许出现它的地方就是 NotifyContext 那一行。
	if n := strings.Count(body, "context.Background()"); n != 1 {
		t.Errorf("RunCLI 里 context.Background() 出现 %d 次，只应在 signal.NotifyContext 那行出现 1 次（其余路径必须用可取消 ctx，R9）", n)
	}
}
