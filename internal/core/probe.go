// /probe：服务端代拉 m3u8 文本（带 Referer 的完整浏览器头 + uTLS 指纹），
// 供浏览器扩展预检候选链接的画质/时长。
// 浏览器侧 fetch 对无 CORS 头的 CDN 只能拿到 opaque 空响应（"未预检"的根源），
// 预检必须与下载路径同能力，故由本地服务代理拉取。
package core

import (
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const probeMaxBytes = 4 << 20 // playlist/master 体积很小，4MB 上限防御异常响应

const probeTimeout = 12 * time.Second // 预检是交互路径，快速失败优于长重试

func (e *Engine) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	referer := strings.TrimSpace(r.URL.Query().Get("referer"))
	if !e.rt.validProbeTarget(target) {
		http.Error(w, "invalid url param", http.StatusBadRequest)
		return
	}

	req, err := e.rt.newRequest(target, referer)
	if err != nil {
		http.Error(w, cleanURLParseErr(err, target).Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	resp, err := e.rt.getClient().Do(req.WithContext(ctx))
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream status "+resp.Status, http.StatusBadGateway)
		return
	}
	body, err := readProbeBody(resp)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(body)
}

// probeAllowLocal（Runtime 字段）仅供测试：单测的上游都是 httptest 起的
// 127.0.0.1 服务，要验证转发/Referer/gzip 等行为就得放行本机地址。生产恒为 false。

// validProbeTarget 只接受公网 http/https URL。
// 这里不只是在校验格式——/probe 会带着伪造的浏览器头、走用户的代理去请求目标，
// 不做限制就等于把「读内网服务 / 云元数据」的 SSRF 能力开放出去。
func (r *Runtime) validProbeTarget(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if r.probeAllowLocal {
		return true
	}
	if isLocalHostName(host) {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !isBlockedIP(ip)
	}
	// 域名：先解析一次，任一结果落在内网即拒绝。
	// 解析失败时放行——真正拨号时也会失败，报 502 比 400 更贴近事实。
	// 注意这里有 DNS TOCTOU（解析与拨号之间可能变），所以它只是第一道；
	// 挡外部调用方的兜底是 Host 校验 + 访问令牌（见 auth.go）。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host); err == nil {
		for _, a := range addrs {
			if isBlockedIP(a.IP) {
				return false
			}
		}
	}
	return true
}

// isLocalHostName 报告主机名是否指向本机/非公网（mDNS、内网 TLD 等）。
func isLocalHostName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	switch h {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".home.arpa"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// isBlockedIP 报告 IP 是否属于不该由 /probe 代为访问的范围：
// 环回、私网（含 IPv6 ULA）、链路本地、组播、未指定，以及运营商级 NAT 段。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// 100.64.0.0/10（CGNAT，RFC 6598）不在 IsPrivate 覆盖范围内
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

// readProbeBody 读取响应体并按 Content-Encoding 兜底解压 gzip
// （自定义 Transport + uTLS 握手下个别 CDN 会把压缩流原样返回，同 httpGetPlaylist）。
func readProbeBody(resp *http.Response) ([]byte, error) {
	rd := io.Reader(io.LimitReader(resp.Body, probeMaxBytes))
	if enc := resp.Header.Get("Content-Encoding"); strings.Contains(enc, "gzip") {
		if gz, err := gzip.NewReader(resp.Body); err == nil {
			defer gz.Close()
			rd = io.LimitReader(gz, probeMaxBytes)
		}
	}
	return io.ReadAll(rd)
}
