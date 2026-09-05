// VideoCatch 桌面客户端（GUI 外壳）。
//
// 单进程架构：下载服务不再是独立进程，由本进程内的 core.Engine 承载。
//   - 主窗 = iframe 内嵌 7891 监控页（任务列表/暂停/继续/设置）铺满窗口，无工具条——
//     服务随程序启动自动运行、退出自动停止，窗口内不放冗余控件；
//     服务被手动停止时外壳自动切换为居中降级提示。
//   - 系统托盘：左键点图标直接显示主窗口，右键弹菜单（启停服务/浏览器打开/退出）。
//     点窗口 X 缩到托盘（WM_CLOSE 子类化拦）。
//   - 退出客户端（托盘菜单）= 优雅停服务（进行中任务转暂停、断点落盘）后进程结束；
//     点 X 只是缩到托盘，下载继续。
package app

import (
	"os"
	"os/exec"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"video_catch/internal/core"
)

// monitorURL 监控页地址（与 core.DefaultPort 对应；外壳 HTML 内的硬编码同源）。
const monitorURL = "http://127.0.0.1:7891"

func openBrowser() error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", monitorURL+"/")
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

	eng := core.NewEngine(core.DefaultPort)

	// 打开客户端即自动拉起下载服务（沿用旧双进程版行为，浏览器扩展依赖 7891 常驻）。
	// 失败不阻断 GUI：外壳轮询显示"未运行"，用户点"启动服务"能看到具体错误。
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
		title, _ := windows.UTF16PtrFromString("VideoCatch")
		text, _ := windows.UTF16PtrFromString("无法创建窗口（需要 Edge/WebView2 运行时）")
		windows.MessageBox(0, text, title, windows.MB_ICONERROR)
		os.Exit(1)
	}
	defer w.Destroy()

	// 外壳唯一需要的绑定：服务是否在运行（决定显示 iframe 还是降级提示）。
	// 启停控制都在托盘菜单；服务随程序启动自动运行、退出自动停止。
	w.Bind("vc_running", func() bool { return eng.Running() })

	// 加载外壳：iframe 内嵌 7891 监控页铺满窗口，无工具条。
	// 外壳的 JS 自己轮询 vc_running 决定显示 iframe 还是降级提示，无需 Go 端切换。
	w.SetHtml(shellHTML())

	// 注册托盘 + 子类化主窗(拦 WM_CLOSE 缩托盘)
	initTray(w, eng)

	w.Run()

	// w.Run() 返回 = 客户端真实退出（托盘菜单"退出客户端"；点 X 只是缩托盘不会走到这）。
	// 单进程架构：退出前优雅停掉下载服务。
	eng.Stop()
}
