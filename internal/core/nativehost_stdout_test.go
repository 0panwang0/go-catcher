// 宿主模式的 stdout 是 native messaging 协议通道：启动路径上不许有任何诊断输出。
package core

import (
	"regexp"
	"strings"
	"testing"
)

// TestNoStdoutOnNativeHostStartupPath 宿主启动路径上的函数不得写 os.Stdout。
//
// 背景（P1-8，2026-09-24 实证）：宿主模式（--native-host）下 os.Stdout 是浏览器与
// 本进程之间的 **native messaging 协议通道** —— main.go 对宿主模式**刻意不调**
// platform.AttachParentConsole（那会把 os.Stdout 换成 CONOUT$），nativehost.go 也
// 不调 SetupFileLogging。往 stdout 写一行诊断，浏览器会把它当成 4 字节长度前缀解析
// ⇒ 帧错位 ⇒ 扩展侧"连上了但永远收不到响应"，而所有文件日志一切正常（本项目头号
// 缺陷形态）。
//
// 缺陷实例：auth.go 的 ensureAPIToken 用 fmt.Printf 报「令牌落盘失败」，而它正好在
// RunNativeHost 的启动路径上（RunNativeHost → configuredPort() → initRuntimeConfig()
// → ensureAPIToken）。触发条件是 exe 目录不可写（典型：装进 Program Files）且配置
// 缺失/损坏。nativehost.go 的注释里早就写下「诊断输出一律走 os.Stderr」这条约束，
// 但**没有任何守卫**守着它 —— 这条用例就是那个守卫。
//
// ⚠️ 逐**函数**钉，不是逐文件：nativehost.go 里 --install-native-host /
// --uninstall-native-host / --native-host-status 三个子命令、output.go 的 emitOutput
// 都有**合法**的 stdout 输出（它们不在宿主启动路径上，而且 main.go 对非宿主模式调了
// AttachParentConsole）。逐文件的守卫会把它们一起误杀。
//
// ⚠️ 结构断言看不见「值」：它只能证明这些函数里没有 fmt.Print*，不能证明日志真的
// 落到了该落的地方。所以末尾另有一条正向断言钉住修复本身。
//
// 守卫边界：只覆盖下面这张表。新加一条从 RunNativeHost 出发的调用链时请把新函数
// 补进来 —— 它抓不到"谁也没想到的那条链"。（newRuntime 是单行转发，自身无从打印，
// 故代之以它转发的 newRuntimeWithEntropy。）
func TestNoStdoutOnNativeHostStartupPath(t *testing.T) {
	hostStartup := []struct{ file, sig string }{
		{"nativehost.go", "func RunNativeHost("},
		{"config.go", "func (r *Runtime) configuredPort("},
		{"config.go", "func (r *Runtime) initRuntimeConfig("},
		{"config.go", "func (r *Runtime) loadConfig("},
		{"config.go", "func (r *Runtime) saveConfigLocked("},
		{"auth.go", "func (r *Runtime) ensureAPIToken("},
		{"auth.go", "func newAPIToken("},
		{"output.go", "func writeFileAtomic("},
		{"runtime.go", "func newRuntimeWithEntropy("},
	}
	// 只认真正的 stdout 写点：fmt.Fprintf(os.Stderr, ...) 是允许的（nativehost.go
	// 通篇用它报错），所以模式里不能让 Fprintf 命中。
	stdoutWrite := regexp.MustCompile(`fmt\.Print(f|ln)?\(`)
	for _, h := range hostStartup {
		for _, l := range sourceFuncBody(t, h.file, h.sig) {
			if stdoutWrite.MatchString(l) {
				t.Errorf("%s 的 %s 往 os.Stdout 写诊断，而它在宿主启动路径上 —— "+
					"那会毁掉 native messaging 协议（改用 platform.Logf）: %s",
					h.file, h.sig, strings.TrimSpace(l))
			}
		}
	}

	// 正向接线：ensureAPIToken 的落盘失败分支必须真的经 platform.Logf。
	// （光有上面那条负向断言，把整段删掉也只留下一条注释、照样全绿。）
	fix := strings.Join(sourceFuncBody(t, "auth.go", "func (r *Runtime) ensureAPIToken("), "\n")
	if !strings.Contains(fix, "platform.Logf(") {
		t.Error("ensureAPIToken 的落盘失败分支必须经 platform.Logf 报出（它绝不写 os.Stdout）")
	}
}
