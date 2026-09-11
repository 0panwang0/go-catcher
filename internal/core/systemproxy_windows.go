//go:build windows

// 系统代理读取：Clash 等工具的「系统代理」开关写的 WinINET 注册表设置
// （HKCU\...\Internet Settings 的 ProxyEnable/ProxyServer）。
// system 代理模式每次建连现读注册表，开关/改端口即时跟随。
package core

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// systemProxyReading 一次注册表读取的结果。
type systemProxyReading struct {
	server  string // ProxyServer 原始值
	enabled bool   // ProxyEnable != 0 且读取成功
}

// readSystemProxy 读当前用户的 WinINET 代理设置。
func readSystemProxy() systemProxyReading {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return systemProxyReading{}
	}
	defer k.Close()
	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return systemProxyReading{}
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return systemProxyReading{}
	}
	return systemProxyReading{server: server, enabled: true}
}

// realSystemProxyAddr 读当前用户 WinINET 系统代理（Runtime.systemProxyAddrFn
// 的默认实现；测试可向 Runtime 注入桩函数）。
// 返回规范化 http://host:port；未启用/读取失败返回 ""（= 直连）。
func realSystemProxyAddr() string {
	r := readSystemProxy()
	if !r.enabled {
		return ""
	}
	return normalizeSystemProxy(r.server)
}

// systemProxyWarning 报告"系统代理已启用、但本引擎用不了"的原因（空 = 无需提示）。
//
// socks5:// 之类的协议无法走 HTTP CONNECT 隧道，normalizeSystemProxy 会返回空串
// ——效果是静默直连。用户以为走了代理、实际暴露真实 IP，这种"安静的降级"必须
// 显式说出来，否则排查时完全看不出问题在哪。
func (rt *Runtime) systemProxyWarning() string {
	if !strings.EqualFold(strings.TrimSpace(rt.getProxyAddr()), "system") {
		return "" // 手动/直连模式与系统代理无关
	}
	r := readSystemProxy()
	if !r.enabled {
		return ""
	}
	if normalizeSystemProxy(r.server) != "" {
		return "" // 解析成功，没有可提示的
	}
	return systemProxyNotice(r.server)
}

// systemProxyNotice 解释 ProxyServer 为何不可用；无法归因时返回通用提示。
func systemProxyNotice(server string) string {
	s := strings.TrimSpace(server)
	if s == "" {
		return ""
	}
	const rule = "本工具只支持 http:// 代理（socks5 等无法走 CONNECT 隧道），已按直连处理"
	if !strings.Contains(s, "=") {
		if i := strings.Index(s, "://"); i > 0 {
			return fmt.Sprintf("系统代理协议为 %s，%s", strings.ToLower(s[:i]), rule)
		}
		return ""
	}
	var protos []string
	hasHTTP := false
	for _, kv := range strings.Split(s, ";") {
		eq := strings.Index(kv, "=")
		if eq <= 0 {
			continue
		}
		p := strings.ToLower(strings.TrimSpace(kv[:eq]))
		protos = append(protos, p)
		if p == "http" || p == "https" {
			hasHTTP = true
		}
	}
	if !hasHTTP && len(protos) > 0 {
		return fmt.Sprintf("系统代理只配置了 %s，%s", strings.Join(protos, "/"), rule)
	}
	return ""
}

// normalizeSystemProxy 规范化 ProxyServer 值：可能是 "host:port"、
// "http://host:port" 或 "ftp=h:21;http=h:8080;https=h:7890" 按协议分设格式。
// 本引擎只发 HTTPS 请求，优先取 https=，回退 http=，再回退整串。
func normalizeSystemProxy(server string) string {
	s := strings.TrimSpace(server)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "=") {
		parts := map[string]string{}
		for _, kv := range strings.Split(s, ";") {
			if eq := strings.Index(kv, "="); eq > 0 {
				parts[strings.ToLower(strings.TrimSpace(kv[:eq]))] = strings.TrimSpace(kv[eq+1:])
			}
		}
		switch {
		case parts["https"] != "":
			s = parts["https"]
		case parts["http"] != "":
			s = parts["http"]
		default:
			return ""
		}
	}
	// 已带 scheme 的只认 http://（socks5:// 等无法走 CONNECT 隧道 → 直连）；
	// 裸 host:port 补 http:// 前缀
	if strings.Contains(s, "://") {
		if !strings.HasPrefix(strings.ToLower(s), "http://") {
			return ""
		}
	} else {
		s = "http://" + s
	}
	if !validProxyAddr(s) {
		return ""
	}
	return s
}
