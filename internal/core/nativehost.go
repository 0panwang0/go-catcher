// 原生消息宿主：由浏览器按注册表清单拉起，职责只有一个——确保本地服务在跑。
//
// 为什么需要它：扩展与客户端之间本来走本地 HTTP 服务，而 HTTP 是请求-响应协议，
// **客户端没有"启动服务端"的能力**——服务没在跑时扩展除了报错什么也做不了。
// 原生消息通道把"启动进程"这件事交给浏览器（它按注册表清单去起清单里 path 指向的
// 程序），于是扩展就能主动把客户端唤起来。
//
// 宿主自身**不下载任何东西、不执行扩展传来的命令**：收到唤起请求 → 探一次
// /health → 没在跑就拉起托盘形态的客户端 → 等服务就绪 → 回响应。之后扩展仍走
// 原有的 HTTP 通道，宿主可以随浏览器一起退出。
//
// 登记（注册表 + 清单文件）与帧协议在 internal/platform/nativehost_windows.go。
package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/0panwang0/go-catcher/internal/platform"
)

// 宿主等待服务就绪的参数。
const (
	// nativeHostWaitTotal 拉起客户端后的总等待时长。客户端要初始化窗口/托盘并起
	// HTTP 服务，冷启动实测在数秒内；给足余量但不无限等——扩展侧还有自己的超时。
	nativeHostWaitTotal = 20 * time.Second
	// nativeHostProbeInterval 两次探测之间的间隔。
	nativeHostProbeInterval = 300 * time.Millisecond
	// nativeHostProbeTimeout 单次探测的超时。
	nativeHostProbeTimeout = 800 * time.Millisecond
)

// nativeHostRequest 扩展发来的请求。目前只有一种类型；留 type 字段是为了
// 将来加新指令时不必改协议格式。
type nativeHostRequest struct {
	Type string `json:"type"`
}

// nativeHostResponse 回给扩展的响应。
type nativeHostResponse struct {
	OK      bool   `json:"ok"`
	Port    int    `json:"port,omitempty"`
	Started bool   `json:"started,omitempty"` // true = 本次请求把客户端拉起来了
	Error   string `json:"error,omitempty"`
}

// nativeHostExtensionIDs 返回允许调用本宿主的扩展来源。
//
// 与 edge_extension/manifest.json 的 key 字段一一对应：清单里固定了 key，扩展 ID
// 才不会因机器而异，allowed_origins 也才写得死。有测试读 manifest.json 反算 ID
// 与本函数比对，防止两边漂移（一边改了另一边忘改，症状是"唤起永远失败且没有报错"）。
//
// 抽成函数而非包级切片：切片是可变类型，包级可变状态在 G1 里是被禁止的。
func nativeHostExtensionIDs() []string {
	return []string{"jodjnfkmjplkofpgmjlmiiadnkofiopk"}
}

// EnsureNativeHost 幂等登记原生消息宿主，返回清单路径。
// 客户端启动时自动调用；--install-native-host 手动走同一条路径。
func EnsureNativeHost(exePath string) (string, error) {
	return platform.EnsureNativeHostRegistered(exePath, nativeHostExtensionIDs())
}

// RunNativeHost 跑原生消息宿主循环，返回进程退出码。
//
// 循环直到 stdin 关闭（浏览器断开连接或关闭窗口）。宿主是浏览器拉起的子进程，
// 生命周期就该由浏览器决定，正常退出时不留后台残留。
//
// ⚠️ 本模式**绝不能**调用 platform.AttachParentConsole 或 SetupFileLogging：
// 前者会把 os.Stdout 换成 CONOUT$，后者会把它换成日志管道，都会直接毁掉
// stdin/stdout 这条协议通道（表现为扩展侧"连上了但永远收不到响应"）。
// 因此本模式下的诊断输出一律走 os.Stderr。
func RunNativeHost() int {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "native host: 取可执行文件路径失败: %v\n", err)
		return 1
	}
	port := newRuntime().configuredPort()

	for {
		body, rErr := platform.ReadNativeMessage(os.Stdin)
		if rErr != nil {
			// EOF = 浏览器侧断开（关窗/退出），正常收尾；其它读错误才需要留痕
			if rErr != io.EOF && rErr != io.ErrUnexpectedEOF {
				fmt.Fprintf(os.Stderr, "native host: 读消息失败: %v\n", rErr)
			}
			return 0
		}
		resp := handleNativeHostRequest(exePath, port, body)
		data, mErr := json.Marshal(resp)
		if mErr != nil {
			data = []byte(`{"ok":false,"error":"响应序列化失败"}`)
		}
		if wErr := platform.WriteNativeMessage(os.Stdout, data); wErr != nil {
			fmt.Fprintf(os.Stderr, "native host: 写消息失败: %v\n", wErr)
			return 1
		}
	}
}

// handleNativeHostRequest 把一条请求分派到具体处理逻辑。
func handleNativeHostRequest(exePath string, port int, body []byte) nativeHostResponse {
	var req nativeHostRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nativeHostResponse{Error: "请求不是合法 JSON"}
	}
	switch req.Type {
	case "ensure-server":
		return ensureLocalService(exePath, port)
	default:
		return nativeHostResponse{Error: "不支持的消息类型: " + req.Type}
	}
}

// ensureLocalService 确保本地服务在跑：已在跑直接返回，否则拉起客户端并等它就绪。
func ensureLocalService(exePath string, port int) nativeHostResponse {
	if probeLocalService(port, nativeHostProbeTimeout) {
		// 已经在跑就不重复启动——多数唤起都走这一支（客户端常驻托盘）
		return nativeHostResponse{OK: true, Port: port}
	}
	if err := platform.LaunchTrayClient(exePath); err != nil {
		return nativeHostResponse{Error: fmt.Sprintf("启动客户端失败: %v", err)}
	}
	if waitForLocalService(port, nativeHostWaitTotal) {
		return nativeHostResponse{OK: true, Port: port, Started: true}
	}
	// 拉起来了但服务没通，常见原因是客户端已在运行、而它内部的服务被手动停掉了
	// （单实例逻辑会让新进程直接退出并把已有窗口拉到前台）。给出可操作的提示，
	// 别让用户对着"唤起失败"猜。
	return nativeHostResponse{
		Error: "客户端已启动，但本地服务未在预期时间内就绪；若客户端已在运行，请从托盘菜单启动服务",
	}
}

// probeLocalService 探一次本地服务的健康端点。
//
// 显式关掉代理：环回地址必须直连。Go 的 ProxyFromEnvironment 虽然会跳过环回，
// 但这里不想依赖那个隐含行为——跟随系统代理的用户一旦配了全局代理，走错路径的
// 探测会给出假阴性，让扩展误以为服务没起来，于是反复拉起客户端。
func probeLocalService(port int, timeout time.Duration) bool {
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	client := &http.Client{Timeout: timeout, Transport: tr}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "ok"
}

// waitForLocalService 轮询等待服务就绪，超时返回 false。
//
// 这里用 time.Sleep 而不是工程统一的 sleepCtx：宿主进程的存活期完全由浏览器决定
// （stdin 一关就退出），没有可取消的上层 ctx；总等待时长有硬上限，不会卡死。
func waitForLocalService(port int, total time.Duration) bool {
	deadline := time.Now().Add(total)
	for {
		if probeLocalService(port, nativeHostProbeTimeout) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(nativeHostProbeInterval)
	}
}

// RunInstallNativeHost 登记原生消息宿主（客户端启动时会自动做，此命令用于手动修复）。
func RunInstallNativeHost() int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("取可执行文件路径失败: %v\n", err)
		return 1
	}
	manifest, err := EnsureNativeHost(exe)
	if err != nil {
		fmt.Printf("登记原生消息宿主失败: %v\n", err)
		return 1
	}
	fmt.Println("原生消息宿主已登记")
	fmt.Println("  清单文件:", manifest)
	fmtPractRegistrations()
	return 0
}

// RunUninstallNativeHost 注销原生消息宿主（只删注册表项与清单文件）。
func RunUninstallNativeHost() int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("取可执行文件路径失败: %v\n", err)
		return 1
	}
	if err := platform.UnregisterNativeHost(exe); err != nil {
		fmt.Printf("注销失败: %v\n", err)
		return 1
	}
	fmt.Println("原生消息宿主已注销（注册表项与清单文件已删除，程序与配置未改动）")
	return 0
}

// RunNativeHostStatus 打印宿主登记现状（排障用）。
func RunNativeHostStatus() int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("取可执行文件路径失败: %v\n", err)
		return 1
	}
	want := platform.NativeHostManifestPath(exe)
	fmt.Println("原生消息宿主登记状态")
	fmt.Println("  期望清单路径:", want)
	fmtPractRegistrations()
	return 0
}

// fmtPractRegistrations 逐行打印各浏览器位置的登记现状（与期望路径对照）。
func fmtPractRegistrations() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	want := platform.NativeHostManifestPath(exe)
	fmt.Println("  允许的扩展来源:", strings.Join(nativeHostExtensionIDs(), ", "))
	for _, r := range platform.QueryNativeHost() {
		switch {
		case r.Path == "":
			fmt.Printf("  [ ] %s —— 未登记\n", r.Root)
		case strings.EqualFold(r.Path, want):
			fmt.Printf("  [x] %s —— 已登记\n", r.Root)
		default:
			fmt.Printf("  [!] %s —— 指向其它路径: %s\n", r.Root, r.Path)
		}
	}
}
