//go:build windows

package platform

// 浏览器「原生消息宿主」（native messaging host）的登记与消息帧读写。
//
// 为什么需要这条通道：本程序与浏览器扩展之间走的是本地 HTTP 服务，而 HTTP 是
// 请求-响应协议，**客户端没有"启动服务端"的能力**——服务没在跑时扩展只能报错。
// 要让扩展自己把客户端拉起来，必须换一条由**浏览器负责启动进程**的通道：
// 扩展声明 nativeMessaging 权限并调 connectNative()，浏览器按注册表里登记的清单
// 启动清单 path 指向的可执行文件，再用 stdin/stdout 与它通信。
//
// 本文件只负责这条通道的两件"平台事"：
//  1. 登记：生成宿主清单 JSON，并在 HKCU 写入各浏览器读取该清单的注册表项；
//  2. 收发：native messaging 的 stdio 帧协议（4 字节小端长度 + JSON）。
//
// 业务侧（收到唤起请求后怎么确保服务在跑）在 internal/core/nativehost.go。
//
// 为什么写 HKCU 而不是 HKLM：不需要管理员权限；这是当前用户自己的浏览器配置，
// 卸载时删掉自己的键即可，不影响其他用户。
//
// 安全边界：这个能力的实质是"允许**指定**扩展启动本机程序"，因此清单里的
// allowed_origins 必须精确限定到扩展 ID（浏览器据此拒绝其它扩展的调用）；
// 宿主进程本身只做"确保本地服务在跑"，不执行扩展传来的任何命令。

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	// NativeHostName 宿主名。三处必须逐字一致：清单文件名、清单内容里的 name、
	// 注册表子键名——浏览器按注册表项名找清单，再校验清单里的 name 是否同名。
	NativeHostName = "com.gocatcher.browser_host"

	// TrayFlag 让客户端以"托盘常驻"形态启动：起服务 + 托盘就位，不弹主窗。
	TrayFlag = "--tray"

	// NativeHostFlag 让客户端以宿主形态启动（由浏览器拉起，走 stdio 帧协议）。
	NativeHostFlag = "--native-host"

	// nativeMessageMaxBytes 单条原生消息的接收上限。
	// 协议本身允许到 4 GiB，但这里只收自家的小消息；设上限是为了不让
	// 损坏或恶意输入逼我们分配巨大缓冲区。
	nativeMessageMaxBytes = 1 << 20
)

// nativeHostRegistryRoots 各浏览器读取宿主注册表项的位置。
// 扩展是标准 MV3（chrome.* 命名空间 + 通用权限），Chrome 与 Edge 都能装，
// 因此两个位置各写一份；都放 HKCU。
var nativeHostRegistryRoots = []string{
	`Software\Google\Chrome\NativeMessagingHosts`,
	`Software\Microsoft\Edge\NativeMessagingHosts`,
}

// nativeHostManifest 宿主清单（浏览器规定的字段，多一个少一个都会导致连接失败）。
type nativeHostManifest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Path           string   `json:"path"`
	Type           string   `json:"type"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// NativeHostManifestPath 清单文件路径：<exe 目录>/native-host/<宿主名>.json。
// 与 gocatcher_config.json 同放 exe 旁边——本程序是绿色版，可变数据都跟着 exe 走。
func NativeHostManifestPath(exePath string) string {
	return filepath.Join(filepath.Dir(exePath), "native-host", NativeHostName+".json")
}

// buildNativeHostManifest 组装清单内容（扩展 ID → chrome-extension://<id>/ 形式）。
func buildNativeHostManifest(exePath string, extensionIDs []string) nativeHostManifest {
	origins := make([]string, 0, len(extensionIDs))
	for _, id := range extensionIDs {
		if id = strings.TrimSpace(id); id != "" {
			origins = append(origins, "chrome-extension://"+id+"/")
		}
	}
	return nativeHostManifest{
		Name:           NativeHostName,
		Description:    "GoCatcher 本地下载服务宿主（仅负责确保服务在运行）",
		Path:           exePath,
		Type:           "stdio",
		AllowedOrigins: origins,
	}
}

// EnsureNativeHostRegistered 幂等登记宿主，返回清单路径。
//
// 只在"确实需要改"时才落盘 / 写注册表：本函数每次客户端启动都会调用，
// 无脑重写会让注册表键的写入时间每次都变（部分安全软件会因此反复报警），
// 也会让浏览器的原生消息宿主缓存反复失效。
//
// extensionIDs 为空时直接报错而不是"登记一个谁都不能用的宿主"：
// allowed_origins 为空的清单等于把这条通道敞着，宁可失败。
func EnsureNativeHostRegistered(exePath string, extensionIDs []string) (string, error) {
	if len(extensionIDs) == 0 {
		return "", errors.New("未提供扩展来源，拒绝登记原生消息宿主")
	}
	if abs, err := filepath.Abs(exePath); err == nil {
		exePath = abs
	}
	manifestPath, err := ensureManifestFile(exePath, extensionIDs)
	if err != nil {
		return "", err
	}
	for _, root := range nativeHostRegistryRoots {
		if err := ensureRegistryValue(root, manifestPath); err != nil {
			return manifestPath, err
		}
	}
	return manifestPath, nil
}

// ensureManifestFile 内容不变则不重写清单文件。
func ensureManifestFile(exePath string, extensionIDs []string) (string, error) {
	want, err := json.MarshalIndent(buildNativeHostManifest(exePath, extensionIDs), "", "  ")
	if err != nil {
		return "", err
	}
	want = append(want, '\n')
	p := NativeHostManifestPath(exePath)
	if cur, err := os.ReadFile(p); err == nil && string(cur) == string(want) {
		return p, nil // 内容一致，不动文件
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return "", fmt.Errorf("创建宿主清单目录失败: %w", err)
	}
	if err := os.WriteFile(p, want, 0644); err != nil {
		return "", fmt.Errorf("写入宿主清单失败: %w", err)
	}
	return p, nil
}

// ensureRegistryValue 让某个浏览器位置指向 manifestPath（已一致则跳过）。
func ensureRegistryValue(root, manifestPath string) error {
	if cur, ok := readNativeHostRegistry(root); ok && strings.EqualFold(cur, manifestPath) {
		return nil
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, root+`\`+NativeHostName, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("创建宿主注册表项失败 (%s): %w", root, err)
	}
	defer k.Close()
	// 宿主注册表项的值就是清单文件的绝对路径（默认值，名称是空串）
	if err := k.SetStringValue("", manifestPath); err != nil {
		return fmt.Errorf("写入宿主注册表值失败 (%s): %w", root, err)
	}
	return nil
}

// readNativeHostRegistry 读某个浏览器位置登记的清单路径，第二个返回值为是否存在。
func readNativeHostRegistry(root string) (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, root+`\`+NativeHostName, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue("")
	if err != nil {
		return "", false
	}
	return v, true
}

// NativeHostRegistration 一条浏览器位置的登记现状。
type NativeHostRegistration struct {
	Root string // 注册表位置
	Path string // 该位置登记的清单路径（空 = 未登记）
}

// QueryNativeHost 查询宿主登记现状，按 nativeHostRegistryRoots 的顺序返回
// （有序，便于排障输出逐行对照）。
func QueryNativeHost() []NativeHostRegistration {
	out := make([]NativeHostRegistration, 0, len(nativeHostRegistryRoots))
	for _, root := range nativeHostRegistryRoots {
		v, _ := readNativeHostRegistry(root)
		out = append(out, NativeHostRegistration{Root: root, Path: v})
	}
	return out
}

// UnregisterNativeHost 注销宿主：删除注册表项与清单文件，并顺手清掉因此变空的
// 父键。不动 exe、不动用户配置、不动下载记录。
func UnregisterNativeHost(exePath string) error {
	var problems []string
	for _, root := range nativeHostRegistryRoots {
		if err := registry.DeleteKey(registry.CURRENT_USER, root+`\`+NativeHostName); err != nil &&
			!errors.Is(err, registry.ErrNotExist) {
			problems = append(problems, fmt.Sprintf("%s: %v", root, err))
			continue
		}
		// 父键是本程序建的（浏览器没装时）就一并清掉；里面还有别的宿主时
		// DeleteKey 会因非空失败，忽略即可。
		_ = registry.DeleteKey(registry.CURRENT_USER, root)
	}
	if err := os.Remove(NativeHostManifestPath(exePath)); err != nil && !os.IsNotExist(err) {
		problems = append(problems, fmt.Sprintf("清单文件: %v", err))
	}
	if len(problems) > 0 {
		return fmt.Errorf("注销未完全成功: %s", strings.Join(problems, "; "))
	}
	return nil
}

// ============================================================
// native messaging 帧协议
// ------------------------------------------------------------
// 每条消息 = 4 字节小端序长度（不含自身）+ 该长度的 UTF-8 JSON。
// 浏览器侧由 Chromium 实现，这里是与之对接的另一半，格式写错会静默无响应。
// ============================================================

// ReadNativeMessage 读一条原生消息体。
func ReadNativeMessage(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n > nativeMessageMaxBytes {
		return nil, fmt.Errorf("原生消息长度超限: %d 字节", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// WriteNativeMessage 写一条原生消息体。
func WriteNativeMessage(w io.Writer, body []byte) error {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// LaunchTrayClient 以"托盘常驻"形态启动客户端。
//
// 用 ShellExecute 而不是 exec.Command：配 DETACHED_PROCESS 启动的子进程拿不到
// 交互桌面，托盘与窗口都出不来；ShellExecute 用调用方自己的 token 与
// window station 去激活目标进程，落在同一个交互桌面上（理由同 shell.go）。
//
// 不等待进程结束：客户端是长驻的，而宿主进程由浏览器拉起、会随浏览器退出，
// 不能把客户端的生命周期挂到宿主身上。
func LaunchTrayClient(exePath string) error {
	return shellExec("open", exePath, TrayFlag)
}
