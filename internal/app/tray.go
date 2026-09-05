// VideoCatch 桌面客户端的"系统深色标题栏 + 自绘工具条 + 系统托盘 + 关窗缩托盘"实现。
//
// 关键设计：不去掉 WS_CAPTION。系统标题栏让 Windows 自己画（用 DWM 强制深色 + 自定义背景色），
// 窗口控件（最小化/最大化/关闭）是系统原生的，跟 Clash for Windows / VS Code 等桌面应用
// 视觉一致；内容区上方放一个紧凑的 32px 工具条放功能按钮，与下方背景同色无缝衔接。
//
// 其余：
//  1. 用 getlantern/systray 的 Register（不是 Run），让托盘与 webview 共用一个消息循环。
//  2. Win32 子类化（SetWindowLongPtr GWLP_WNDPROC）拦截主窗 WM_CLOSE → 缩托盘。
//     真退出只在托盘"退出"菜单触发：标志位 + 让 WM_CLOSE 透传给原 wndproc。
//  3. systray 库的托盘窗口（类名 "SystrayClass"）也被子类化：左键点托盘图标直接
//     显示主窗口，右键才弹菜单（库默认左/右键都弹菜单且无回调 API）。
//
// 不改 go-webview2 源码、不引 CGO 之外的额外依赖。
package app

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/getlantern/systray"
	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"video_catch/internal/core"
)

// Win32 常量
const (
	gwlpWndproc = uintptr(0xFFFFFFFFFFFFFC) // GWLP_WNDPROC = -4
	wmClose     = 0x0010
	swHide      = 0
	swShow      = 5 // SW_SHOW

	// systray 库 NOTIFYICON 回调消息（库内固定 WM_USER+1），lParam 是鼠标消息
	wmTrayCallback = 0x0401
	wmLButtonUp    = 0x0202

	// DWM 让 Windows 把标题栏/控件库渲染为深色：
	// DWMWA_USE_IMMERSIVE_DARK_MODE 原值 19，新值 20，Win10 1903 支持 19，Win11 用 20。
	// 这里两个都试（失败也无所谓，深色模式不生效时回退到默认）。
	dwmAttrImmersiveDarkOld = 19
	dwmAttrImmersiveDarkNew = 20
	// Win11 22H2+ 才支持 DWMWA_CAPTION_COLOR / DWMWA_TEXT_COLOR，不支持就跳过。
	dwmAttrCaptionColor = 35
	dwmAttrTextColor    = 36

	// 标题栏颜色：用 #202020，与 iframe 背景 #0b1220 自然分层（标题栏略浅）。
	// COLORREF 字节序是 0x00BBGGRR。
	captionColorBGR uint32 = 0x00202020
	textColorBGR    uint32 = 0x00E0E0E0
)

// 进程级状态
var (
	origWndProc  uintptr
	mainHwnd     uintptr
	exitingFlag  atomic.Bool
	trayOrigProc uintptr
)

// Win32 句柄
var (
	user32             = windows.NewLazySystemDLL("user32.dll")
	dwmapi             = windows.NewLazySystemDLL("dwmapi.dll")
	procSetWindowLong  = user32.NewProc("SetWindowLongPtrW")
	procGetWindowLong  = user32.NewProc("GetWindowLongPtrW")
	procCallWindowProc = user32.NewProc("CallWindowProcW")
	procShowWindow     = user32.NewProc("ShowWindow")
	procSetForeground  = user32.NewProc("SetForegroundWindow")
	procPostMessage    = user32.NewProc("PostMessageW")
	procFindWindow     = user32.NewProc("FindWindowW")
	procDwmSetAttr     = dwmapi.NewProc("DwmSetWindowAttribute")
)

// enableDarkTitleBar 通过 DWM 让 Windows 把窗口标题栏渲染成深色（Win10 1903+ / Win11）。
// 调用失败（版本太老）也不致命——浅色标题栏仍然可用。
//
// DwmSetWindowAttribute 签名：
//   HRESULT DwmSetWindowAttribute(HWND hwnd, DWORD dwAttribute, LPCVOID pvAttribute, DWORD cbAttribute);
func enableDarkTitleBar(hwnd uintptr) {
	// 1) 沉浸式深色（让标题栏文本/控件变成白色，Win11 用 attr=20，Win10 1903 用 attr=19）。
	var enabled int32 = 1
	procDwmSetAttr.Call(hwnd, uintptr(dwmAttrImmersiveDarkNew), uintptr(unsafe.Pointer(&enabled)), 4)
	procDwmSetAttr.Call(hwnd, uintptr(dwmAttrImmersiveDarkOld), uintptr(unsafe.Pointer(&enabled)), 4)
	// 2) 自定义标题栏背景色 + 文字色（仅 Win11 22H2+ 支持，失败也无副作用）。
	color := captionColorBGR
	procDwmSetAttr.Call(hwnd, uintptr(dwmAttrCaptionColor), uintptr(unsafe.Pointer(&color)), unsafe.Sizeof(color))
	color = textColorBGR
	procDwmSetAttr.Call(hwnd, uintptr(dwmAttrTextColor), uintptr(unsafe.Pointer(&color)), unsafe.Sizeof(color))
}

// 子类化的 wndproc：只拦 WM_CLOSE 缩托盘，其它全部透传给 go-webview2 原 wndproc。
func subclassWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	if msg == wmClose {
		if exitingFlag.Load() {
			restoreWndProc()
			procCallWindowProc.Call(origWndProc, hwnd, msg, wp, lp)
			return 0
		}
		procShowWindow.Call(hwnd, swHide)
		systray.SetTooltip("VideoCatch 下载客户端（已最小化到托盘 · 点此恢复）")
		return 0
	}
	r, _, _ := procCallWindowProc.Call(origWndProc, hwnd, msg, wp, lp)
	return r
}

func restoreWndProc() {
	if mainHwnd == 0 || origWndProc == 0 {
		return
	}
	procSetWindowLong.Call(mainHwnd, gwlpWndproc, origWndProc)
	origWndProc = 0
}

func installSubclass(hwnd uintptr) {
	mainHwnd = hwnd
	origWndProc, _, _ = procGetWindowLong.Call(hwnd, gwlpWndproc)
	if origWndProc == 0 {
		return
	}
	cb := windows.NewCallback(subclassWndProc)
	procSetWindowLong.Call(hwnd, gwlpWndproc, cb)
	// 让系统标题栏变深色
	enableDarkTitleBar(hwnd)
}

// 显示主窗（托盘菜单/左键托盘图标调用）
func showMainWindow() {
	if mainHwnd == 0 {
		return
	}
	procShowWindow.Call(mainHwnd, swShow)
	procSetForeground.Call(mainHwnd)
	systray.SetTooltip("VideoCatch 下载客户端")
}

// ============================================================
// 托盘窗口子类化：左键点托盘图标 → 显示主窗口；右键 → 弹菜单
// （getlantern/systray 无点击回调 API，只能从外面拦它的 wndProc）
// ============================================================

// subclassTrayWnd 子类化 systray 库内部的隐藏托盘窗口（类名固定 "SystrayClass"）。
// Register() 同步完成托盘窗口创建，所以此刻 FindWindow 一定能找到。
func subclassTrayWnd() {
	cls, _ := windows.UTF16PtrFromString("SystrayClass")
	hwnd, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(cls)), 0)
	if hwnd == 0 {
		return
	}
	orig, _, _ := procGetWindowLong.Call(hwnd, gwlpWndproc)
	if orig == 0 {
		return
	}
	trayOrigProc = orig
	procSetWindowLong.Call(hwnd, gwlpWndproc, windows.NewCallback(trayWndProc))
}

// trayWndProc 只拦"左键点击托盘图标"（库回调消息 WM_USER+1 + lParam=WM_LBUTTONUP），
// 显示主窗口后吞掉消息（不透传就不会弹菜单）；其余全部透传给库原 wndProc。
func trayWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	if msg == wmTrayCallback && lp == wmLButtonUp {
		showMainWindow()
		return 0
	}
	r, _, _ := procCallWindowProc.Call(trayOrigProc, hwnd, msg, wp, lp)
	return r
}

// 真退出
func realQuit() {
	exitingFlag.Store(true)
	if mainHwnd != 0 {
		procPostMessage.Call(mainHwnd, wmClose, 0, 0)
	}
	systray.Quit()
}

// initTray 在 webview 主窗创建之后、w.Run() 之前调用一次。
// systray.Register 立即返回（不阻塞），托盘窗口的 wndproc 注册后由 w.Run 的消息循环分发。
func initTray(w webview2.WebView, eng *core.Engine) {
	systray.Register(func() {
		systray.SetIcon(iconPNG)
		systray.SetTooltip("VideoCatch 下载客户端")

		mShow := systray.AddMenuItem("📺 显示主窗口", "恢复 VideoCatch 主窗口")
		systray.AddSeparator()
		mToggle := systray.AddMenuItem("⏯ 启动 / 停止服务", "按当前状态切换下载服务")
		systray.AddSeparator()
		mBrowse := systray.AddMenuItem("↗ 在浏览器打开监控", "用默认浏览器打开 http://127.0.0.1:7891/")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("❌ 退出客户端", "退出客户端并停止下载服务")

		go func() {
			for {
				select {
				case <-mShow.ClickedCh:
					w.Dispatch(func() { showMainWindow() })
				case <-mToggle.ClickedCh:
					if eng.Running() {
						eng.Stop()
						systray.SetTooltip("VideoCatch 下载客户端 · 服务已停止")
					} else if err := eng.Start(); err != nil {
						systray.SetTooltip(fmt.Sprintf("VideoCatch: 启动失败 %v", err))
					} else {
						systray.SetTooltip("VideoCatch 下载客户端 · 服务运行中")
					}
				case <-mBrowse.ClickedCh:
					_ = openBrowser()
				case <-mQuit.ClickedCh:
					w.Dispatch(func() { realQuit() })
					return
				}
			}
		}()
	}, nil)

	// 子类化主窗（保留 WS_CAPTION，让系统画深色标题栏 + 原生窗口控件）
	hwnd := uintptr(w.Window())
	w.Dispatch(func() { installSubclass(hwnd) })

	// 子类化托盘窗口：左键图标显示主窗口，右键弹菜单。
	// Register() 已同步创建好托盘窗口，此刻在主线程直接子类化即可。
	subclassTrayWnd()
}
