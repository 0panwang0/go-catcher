// Windows 弹窗辅助：用 golang.org/x/sys/windows 的 ShellExecute 激活关联程序。
// 不经过 cmd.exe / powershell / explorer 子进程，彻底绕开命令行解析歧义
// 与"detached 服务进程无法把 GUI 子进程送上交互桌面"的问题。
// ShellExecute 用调用进程自己的 token & window station/desktop 去激活应用，
// 只要服务是被用户的 bat/进程拉起（同一登录会话），就能正常弹窗。
package core

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

const swShownormal int32 = 1

// shellOpen 用系统关联程序打开 file（文件→默认播放器/编辑器；目录→资源管理器）。
func shellOpen(file string) error {
	return shellExec("open", file, "")
}

// shellReveal 打开 file 所在文件夹并在资源管理器里选中它。
// 目录：直接打开；文件：交给 explorer.exe /select,<path> 定位并选中。
func shellReveal(file string) error {
	if fi, err := os.Stat(file); err == nil && !fi.IsDir() {
		return shellExec("", "explorer.exe", "/select,"+file)
	}
	return shellExec("open", file, "")
}

func shellExec(verb, file, args string) error {
	vp, _ := syscall.UTF16PtrFromString(verb)
	fp, _ := syscall.UTF16PtrFromString(file)
	ap, _ := syscall.UTF16PtrFromString(args)
	return windows.ShellExecute(0, vp, fp, ap, nil, swShownormal)
}
