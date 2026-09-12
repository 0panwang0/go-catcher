//go:build windows

package platform

import (
	"os"

	"golang.org/x/sys/windows"
)

// AttachParentConsole 把本（GUI 子系统）进程挂回父终端并重定向标准输出：
// 从 PowerShell/cmd 跑 CLI 或 --server 模式时日志可见；双击启动（无父终端）静默失败。
func AttachParentConsole() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	attach := kernel32.NewProc("AttachConsole")
	// ATTACH_PARENT_PROCESS = (DWORD)-1
	if r, _, _ := attach.Call(uintptr(^uintptr(0))); r == 0 {
		return
	}
	conout, err := windows.CreateFile(
		windows.StringToUTF16Ptr("CONOUT$"),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(conout), "CONOUT$")
	os.Stdout = f
	os.Stderr = f
}
