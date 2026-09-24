// GoCatcher 桌面客户端（GUI 外壳）。
//
// 单进程架构：下载服务不再是独立进程，由本进程内的 core.Engine 承载。
//   - 主窗 = iframe 内嵌本地监控页（任务列表/暂停/继续/设置）铺满窗口，无工具条——
//     服务随程序启动自动运行、退出自动停止，窗口内不放冗余控件；
//     服务没起来时外壳自动切换为居中降级提示。
//   - 系统托盘：左键点图标显示主窗口，右键弹菜单，只有「显示主窗口」与「退出客户端」
//     两项——服务启停与"在浏览器打开监控"两个入口已去掉（后者与主窗口功能重复）。
//     点窗口 X 缩到托盘（WM_CLOSE 子类化拦）。
//   - 退出客户端（托盘菜单）= 优雅停服务（进行中任务转暂停、断点落盘）后进程结束；
//     点 X 只是缩到托盘，下载继续。
package app

import (
	"fmt"
	"os"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"github.com/0panwang0/go-catcher/internal/core"
	"github.com/0panwang0/go-catcher/internal/platform"
)

// Run 启动 GUI 客户端（trayOnly=true 时为"托盘常驻"形态），阻塞直到用户真正退出。
//
// trayOnly 用于被浏览器原生消息宿主拉起的场景：用户只是点了一下"下载该视频"，
// 不该被突然弹出的窗口打断——服务照常起、托盘照常就位，主窗建好即收起，
// 想看进度时从托盘点开即可。
func Run(trayOnly bool) {
	// 单实例：已有实例则唤起它的窗口，本进程直接退出
	if !acquireSingleInstance() {
		activateExistingWindow()
		return
	}
	enablePerMonitorDPI()

	// 登记浏览器原生消息宿主（幂等）：登记过，扩展才能把本程序唤起来。
	// 失败不阻断 GUI——这只是"扩展能不能自动唤起"的附加能力，不该因为它没登记上
	// 就让客户端起不来；原因写进文件日志供排障。
	if exe, err := os.Executable(); err == nil {
		if _, rErr := core.EnsureNativeHost(exe); rErr != nil {
			fmt.Printf("[app] 登记浏览器原生消息宿主失败（扩展将无法自动唤起本程序）: %v\n", rErr)
		}
	}

	// 打开客户端即自动拉起下载服务（沿用旧双进程版行为，浏览器扩展依赖本地服务常驻）。
	// 失败不阻断 GUI：外壳轮询切到"未运行"降级面板，面板里给出原因与处理方式。
	// 端口不再固定 7891：跟随 gocatcher_config.json（可在监控页设置里改）。
	eng := core.NewEngine(0)
	if err := eng.Start(); err != nil {
		// 启动失败不阻断 GUI（外壳会切到"服务未运行"降级面板，提示重启客户端），
		// 但必须留下原因：GUI 是 windowsgui 子系统、没有控制台，端口被占用这类
		// 错误如果不落到文件日志，用户只会看到"服务未运行"而完全无从下手。
		fmt.Printf("[app] 下载服务启动失败: %v\n", err)
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: false,
		WindowOptions: webview2.WindowOptions{
			Title:  mainWindowTitle,
			IconId: 1, // 窗口类图标取 exe 资源 #1（winres/winres.json），否则 go-webview2 用通用 IDI_APPLICATION
			Width:  1080,
			Height: 780,
			Center: true,
		},
	})
	if w == nil {
		title, _ := windows.UTF16PtrFromString("GoCatcher")
		text, _ := windows.UTF16PtrFromString("无法创建窗口（需要 Edge/WebView2 运行时）")
		windows.MessageBox(0, text, title, windows.MB_ICONERROR)
		// 与 cmd/go-catcher/main.go 的同类退出路径一致：os.Exit 不跑 defer，
		// 日志是异步写盘的，不显式排空就会把最后那几行诊断一起带走——
		// 而"窗口都建不出来"恰恰是最需要留痕的时刻。
		platform.CloseFileLogging()
		os.Exit(1)
	}
	defer w.Destroy()

	if trayOnly {
		// 窗口在 go-webview2 内部创建时就已经显示出来了（该库没有"创建即隐藏"的
		// 选项，也不接受调用方传入自己的窗口句柄），因此这里只能建好立刻收起。
		// 代价是被唤起的那一次会闪过一瞬空白窗口，换来的是之后全程不打扰用户。
		hideWindow(uintptr(w.Window()))
	}

	// 外壳需要的两个绑定：服务是否在运行（iframe vs 降级提示）、当前端口
	// （设置里改端口并重启客户端后，外壳据此把 iframe 切到新地址）。
	// 服务随程序启动自动运行、退出自动停止，没有单独的启停入口。
	w.Bind("vc_running", func() bool { return eng.Running() })
	w.Bind("vc_port", func() int { return eng.Port() })

	// 加载外壳：iframe 内嵌监控页铺满窗口，无工具条。
	// 外壳的 JS 轮询 vc_running/vc_port 自行决定 iframe 地址与降级提示，无需 Go 端切换。
	// 必须带上本引擎的内嵌豁免键：监控页/设置页默认 DENY 防点击劫持，而外壳的父文档
	// 是 opaque origin，不带键会被一并拒掉（主窗白屏，只剩"禁止"图标）。
	w.SetHtml(shellHTML(eng.EmbedKey()))

	// 注册托盘 + 子类化主窗(拦 WM_CLOSE 缩托盘)
	initTray(w)

	w.Run()

	// w.Run() 返回 = 客户端真实退出（托盘菜单"退出客户端"；点 X 只是缩托盘不会走到这）。
	// 单进程架构：退出前优雅停掉下载服务。
	eng.Stop()
}
