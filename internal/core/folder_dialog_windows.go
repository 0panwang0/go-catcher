//go:build windows

package core

// Windows 原生"选择文件夹"对话框（IFileOpenDialog + FOS_PICKFOLDERS）。
// 纯 COM 手工调用，不依赖第三方库，返回用户选中的绝对路径。
// 用户取消返回 ("", error)，error 含 cancelled 字样。

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32            = windows.NewLazySystemDLL("ole32.dll")
	procCoInitialize = ole32.NewProc("CoInitialize")
	procCoUninit     = ole32.NewProc("CoUninitialize")
	procCoCreateInst = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFr  = ole32.NewProc("CoTaskMemFree")
	shell32          = windows.NewLazySystemDLL("shell32.dll")
	procSHCreateItem = shell32.NewProc("SHCreateItemFromParsingName")
)

// CLSID_FileOpenDialog {DC1C5A9C-E88A-4DDE-A5A1-60F82A20AEF7}
var clsidFileOpenDialog = windows.GUID{
	Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE,
	Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7},
}

// IID_IFileOpenDialog {D57C7288-D4AD-4768-BE02-9D969532D960}
var iidIFileOpenDialog = windows.GUID{
	Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768,
	Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60},
}

// IID_IShellItem {43826D1E-E718-42EE-BC55-A1E261C37BFE}
var iidIShellItem = windows.GUID{
	Data1: 0x43826D1E, Data2: 0xE718, Data3: 0x42EE,
	Data4: [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE},
}

const (
	FOS_PICKFOLDERS   = 0x00000020
	SIGDN_FILESYSPATH = 0x80058000
)

// COM 接口 vtable 调用辅助：跳一个 method index，返回 HRESULT
func comCall3(iface uintptr, idx int, a1, a2, a3 uintptr) (hr uintptr) {
	vt := *(*uintptr)(unsafe.Pointer(iface)) // vtable 指针
	method := *(*uintptr)(unsafe.Pointer(vt + uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	r0, _, _ := syscall.SyscallN(method, iface, a1, a2, a3)
	return r0
}

func comCall2(iface uintptr, idx int, a1, a2 uintptr) (hr uintptr) {
	vt := *(*uintptr)(unsafe.Pointer(iface))
	method := *(*uintptr)(unsafe.Pointer(vt + uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	r0, _, _ := syscall.SyscallN(method, iface, a1, a2)
	return r0
}

func comCall1(iface uintptr, idx int, a1 uintptr) (hr uintptr) {
	vt := *(*uintptr)(unsafe.Pointer(iface))
	method := *(*uintptr)(unsafe.Pointer(vt + uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	r0, _, _ := syscall.SyscallN(method, iface, a1)
	return r0
}

func comCall0(iface uintptr, idx int) (hr uintptr) {
	vt := *(*uintptr)(unsafe.Pointer(iface))
	method := *(*uintptr)(unsafe.Pointer(vt + uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	r0, _, _ := syscall.SyscallN(method, iface)
	return r0
}

func hrErr(hr uintptr) error {
	if hr == 0 { // S_OK
		return nil
	}
	// 0x800704C7 = ERROR_CANCELLED (用户取消)
	if hr&0xFFFFFFFF == 0x800704C7 {
		return fmt.Errorf("cancelled")
	}
	return fmt.Errorf("COM HRESULT 0x%08X", uint32(hr))
}

// pickFolder 弹原生文件夹选择框，返回绝对路径。用户取消返回 ("", error 含 cancelled)。
func pickFolder(ownerHwnd uintptr, title string) (string, error) {
	// STA COM 对象必须固定在同一个 OS 线程创建和使用。
	// Go goroutine 默认会在线程间迁移，会导致 CoInitialize 的线程与
	// 后续 COM 调用线程不一致 → 崩溃或 HRESULT 错误。锁线程规避。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 初始化 COM
	r, _, _ := procCoInitialize.Call(0) // COINIT_APARTMENTTHREADED=0
	// S_OK(0)、S_FALSE(1)、RPC_E_CHANGED_MODE(0x80010106) 都不算致命；只有 FAILED 才需要跳过
	initHr := uint32(r)
	if initHr != 0 && initHr != 1 && initHr != 0x80010106 {
		return "", fmt.Errorf("CoInitialize failed 0x%08X", initHr)
	}
	defer procCoUninit.Call()

	// CoCreateInstance(CLSID_FileOpenDialog, nil, CLSCTX_INPROC_SERVER=1, IID_IFileOpenDialog, &ptr)
	var dialogPtr uintptr
	hr, _, _ := procCoCreateInst.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)),
		0,
		1, // CLSCTX_INPROC_SERVER
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)),
		uintptr(unsafe.Pointer(&dialogPtr)),
	)
	if hr != 0 {
		return "", fmt.Errorf("CoCreateInstance failed 0x%08X", uint32(hr))
	}
	defer comCall0(dialogPtr, 2) // Release

	// SetOptions(FOS_PICKFOLDERS) — vtable index 9
	if err := hrErr(comCall1(dialogPtr, 9, FOS_PICKFOLDERS)); err != nil {
		return "", err
	}

	// SetTitle(title) — vtable index 17（utf16 指针）
	if title != "" {
		titlePtr, err := windows.UTF16PtrFromString(title)
		if err == nil {
			if err := hrErr(comCall1(dialogPtr, 17, uintptr(unsafe.Pointer(titlePtr)))); err != nil {
				return "", err
			}
		}
	}

	// Show(ownerHwnd) — vtable index 3（来自 IModalWindow）。阻塞直到用户选择/取消。
	showHr := comCall1(dialogPtr, 3, ownerHwnd)
	if showHr == 0x800704C7 {
		return "", fmt.Errorf("cancelled") // 用户点取消
	}
	if err := hrErr(showHr); err != nil {
		return "", err
	}

	// GetResult(&shellItem) — vtable index 20（IFileDialog::GetResult 一个参数）
	var itemPtr uintptr
	if err := hrErr(comCall1(dialogPtr, 20, uintptr(unsafe.Pointer(&itemPtr)))); err != nil {
		return "", err
	}
	if itemPtr == 0 {
		return "", fmt.Errorf("no result")
	}
	defer comCall0(itemPtr, 2) // Release shellItem

	// IShellItem::GetDisplayName(SIGDN_FILESYSPATH, &name) — vtable index 5
	var namePtr uintptr
	if err := hrErr(comCall2(itemPtr, 5, SIGDN_FILESYSPATH, uintptr(unsafe.Pointer(&namePtr)))); err != nil {
		return "", err
	}
	defer procCoTaskMemFr.Call(namePtr) // 释放 CoTaskMemAlloc 出来的字符串

	if namePtr == 0 {
		return "", fmt.Errorf("empty path")
	}
	path := windows.UTF16ToString((*[1 << 20]uint16)(unsafe.Pointer(namePtr))[:])
	return path, nil
}
