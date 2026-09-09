//go:build windows

// 系统代理读取：Clash 等工具的「系统代理」开关写的 WinINET 注册表设置
// （HKCU\...\Internet Settings 的 ProxyEnable/ProxyServer）。
// system 代理模式每次建连现读注册表，开关/改端口即时跟随。
package core

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// systemProxyAddr 读当前用户 WinINET 系统代理。变量形式便于测试注入。
// 返回规范化 http://host:port；未启用/读取失败返回 ""（= 直连）。
var systemProxyAddr = func() string {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return ""
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return ""
	}
	return normalizeSystemProxy(server)
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
