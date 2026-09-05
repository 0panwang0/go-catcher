//go:build windows

// Windows 进程/窗口辅助：Per-Monitor DPI、隐藏子进程、单实例互斥。
package app

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 主窗口标题（webview2 创建窗口与 FindWindow 唤起共用）
const mainWindowTitle = "GoCatcher 下载客户端"

func hiddenProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW：不弹子进程控制台
	}
}

// enablePerMonitorDPI 在创建任何窗口前声明 Per-Monitor V2 DPI 感知。
// go-webview2 自己不做 DPI 声明，进程默认 DPI-unaware 时整窗被系统
// 位图拉伸（缩放屏幕上字体明显发糊，和浏览器直接打开不一致），必须在此提前设置。
func enablePerMonitorDPI() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	proc := user32.NewProc("SetProcessDpiAwarenessContext")
	if proc.Find() != nil {
		return
	}
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	proc.Call(^uintptr(3))
}

// ============================================================
// 单实例：命名互斥量。第二个实例发现已有实例时唤起其主窗口后退出，
// 避免双开抢 7891 端口、出现两个托盘图标。
// ============================================================

var instanceMutex windows.Handle

func acquireSingleInstance() bool {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	createMutex := kernel32.NewProc("CreateMutexW")
	name, _ := windows.UTF16PtrFromString("GoCatcher_SingleInstance")
	r1, _, e1 := createMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if r1 == 0 {
		return true // 创建失败不阻止启动（比拒绝运行体验好）
	}
	if errno, ok := e1.(syscall.Errno); ok && errno == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(windows.Handle(r1))
		return false
	}
	instanceMutex = windows.Handle(r1)
	return true
}

// activateExistingWindow 唤起已运行实例的主窗口（可能是隐藏到托盘状态）。
func activateExistingWindow() {
	title, _ := windows.UTF16PtrFromString(mainWindowTitle)
	hwnd, _, _ := procFindWindow.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	procShowWindow.Call(hwnd, 9) // SW_RESTORE：从隐藏/最小化恢复
	procSetForeground.Call(hwnd)
}
