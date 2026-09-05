// GoCatcher 桌面客户端（GUI 外壳）。
//
// 单进程架构：下载服务不再是独立进程，由本进程内的 core.Engine 承载。
//   - 主窗 = iframe 内嵌本地监控页（任务列表/暂停/继续/设置）铺满窗口，无工具条——
//     服务随程序启动自动运行、退出自动停止，窗口内不放冗余控件；
//     服务被手动停止时外壳自动切换为居中降级提示。
//   - 系统托盘：左键点图标直接显示主窗口，右键弹菜单（启停服务/浏览器打开/退出）。
//     点窗口 X 缩到托盘（WM_CLOSE 子类化拦）。
//   - 退出客户端（托盘菜单）= 优雅停服务（进行中任务转暂停、断点落盘）后进程结束；
//     点 X 只是缩到托盘，下载继续。
package app

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"github.com/0panwang0/go-catcher/internal/core"
)

// monitorBase 监控页地址前缀（端口跟随引擎/配置，运行时动态取）。
func monitorURL(eng *core.Engine) string {
	return fmt.Sprintf("http://127.0.0.1:%d", eng.Port())
}

func openBrowser(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = hiddenProcAttr()
	return cmd.Start()
}

// Run 启动 GUI 客户端，阻塞直到用户真正退出。
func Run() {
	// 单实例：已有实例则唤起它的窗口，本进程直接退出
	if !acquireSingleInstance() {
		activateExistingWindow()
		return
	}
	enablePerMonitorDPI()

	// 打开客户端即自动拉起下载服务（沿用旧双进程版行为，浏览器扩展依赖本地服务常驻）。
	// 失败不阻断 GUI：外壳轮询显示"未运行"，用户从托盘重启能看到具体错误。
	// 端口不再固定 7891：跟随 gocatcher_config.json（可在监控页设置里改）。
	eng := core.NewEngine(0)
	_ = eng.Start()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: false,
		WindowOptions: webview2.WindowOptions{
			Title:  mainWindowTitle,
			Width:  1080,
			Height: 780,
			Center: true,
		},
	})
	if w == nil {
		title, _ := windows.UTF16PtrFromString("GoCatcher")
		text, _ := windows.UTF16PtrFromString("无法创建窗口（需要 Edge/WebView2 运行时）")
		windows.MessageBox(0, text, title, windows.MB_ICONERROR)
		os.Exit(1)
	}
	defer w.Destroy()

	// 外壳需要的两个绑定：服务是否在运行（iframe vs 降级提示）、当前端口
	// （设置里改端口 + 托盘重启服务后，外壳据此把 iframe 切到新地址）。
	// 启停控制都在托盘菜单；服务随程序启动自动运行、退出自动停止。
	w.Bind("vc_running", func() bool { return eng.Running() })
	w.Bind("vc_port", func() int { return eng.Port() })

	// 加载外壳：iframe 内嵌监控页铺满窗口，无工具条。
	// 外壳的 JS 轮询 vc_running/vc_port 自行决定 iframe 地址与降级提示，无需 Go 端切换。
	w.SetHtml(shellHTML())

	// 注册托盘 + 子类化主窗(拦 WM_CLOSE 缩托盘)
	initTray(w, eng)

	w.Run()

	// w.Run() 返回 = 客户端真实退出（托盘菜单"退出客户端"；点 X 只是缩托盘不会走到这）。
	// 单进程架构：退出前优雅停掉下载服务。
	eng.Stop()
}
