// /probe：服务端代拉 m3u8 文本（带 Referer 的完整浏览器头 + 指纹伪装），
// 供浏览器扩展预检候选链接的画质/时长。
// 浏览器侧 fetch 对无 CORS 头的 CDN 只能拿到 opaque 空响应（"未预检"的根源），
// 预检必须与下载路径同能力，故由本地服务代理拉取。
package core

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const probeMaxBytes = 4 << 20 // playlist/master 体积很小，4MB 上限防御异常响应

const probeTimeout = 12 * time.Second // 预检是交互路径，快速失败优于长重试

// probeMaxRedirects 预检允许跟随的重定向跳数。
// 链路正常时 CDN 也就 1–2 跳，3 跳已是宽裕值；上限越小越不容易被拿来做跳板。
const probeMaxRedirects = 3

// probeClient 预检专用客户端：复用共享 Transport（连接池、代理、指纹握手完全一致），
// 只替换重定向策略。
//
// 为什么不改共享 client：下载路径必须跟随 CDN 重定向，给它加逐跳校验会误伤正常下载。
// 而 /probe 是外部输入（url 参数）驱动的，第一跳由 validProbeTarget 校验过，
// 跳转后的目标却从不过问 —— 公网 URL 302 到 169.254.169.254 即可绕过整个 SSRF 防护。
//
// 每次现取 Transport：代理变更时 getClient() 会重建 sharedClient，
// 这里跟着换才不会抱着旧代理的连接池。
func (r *Runtime) probeClient() *http.Client {
	base := r.getClient()
	return &http.Client{
		Transport:     base.Transport,
		CheckRedirect: r.checkProbeRedirect,
	}
}

// checkProbeRedirect 对每一次重定向的目标重新做一次 SSRF 校验（含跳数上限）。
func (r *Runtime) checkProbeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= probeMaxRedirects {
		return fmt.Errorf("重定向超过 %d 次", probeMaxRedirects)
	}
	if req.URL == nil {
		return fmt.Errorf("重定向目标缺失")
	}
	if !r.validProbeTarget(req.URL.String()) {
		return fmt.Errorf("重定向目标不被允许: %s", sanitizeURLForError(req.URL.String()))
	}
	return nil
}

func (e *Engine) handleProbe(w http.ResponseWriter, r *http.Request) {
	// 方法校验由路由表的 methodGuard 统一完成（routeDefs 里 /probe 只允许 GET）。
	// 这里不再手写一份：表驱动改造后残留的重复校验会让人怀疑"是不是漏挂了表"。
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
	resp, err := e.rt.probeClient().Do(req.WithContext(ctx))
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
// 127.0.0.1 服务，要验证转发/Referer/gzip 等行为就得放行环回地址。生产恒为 false。
//
// 它刻意只放行**环回**，不放行私网/链路本地/元数据地址 —— 否则"重定向到
// 169.254.169.254 必须被拒"这类用例会因为开关而假绿。

// validProbeTarget 只接受公网 http/https URL。
// 这里不只是在校验格式——/probe 会带着伪造的浏览器头、走用户的代理去请求目标，
// 不做限制就等于把「读内网服务 / 云元数据」的 SSRF 能力开放出去。
//
// 本函数同时是**重定向逐跳校验**的判据（见 checkProbeRedirect）：第一跳与后续跳
// 用同一把尺子，不然跳转一次就绕过去了。
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
	allowLoopback := r.probeAllowLocal
	if isLocalHostName(host) {
		return allowLoopback
	}
	if ip := net.ParseIP(host); ip != nil {
		if allowLoopback && ip.IsLoopback() {
			return true
		}
		return !isBlockedIP(ip)
	}
	// 域名：先解析一次，任一结果落在内网即拒绝。
	// 解析失败时放行——真正拨号时也会失败，报 502 比 400 更贴近事实。
	// 注意这里有 DNS TOCTOU（解析与拨号之间可能变），所以它只是第一道；
	// 挡外部调用方的兜底是 Host 校验 + 访问令牌（见 auth.go）。
	// 另注：走代理时出口在代理侧，这一层管不到——所以它挡的是"本机代为访问内网"，
	// 而不是"经由代理访问内网"，后者属于代理自身的策略范围。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host); err == nil {
		for _, a := range addrs {
			if allowLoopback && a.IP.IsLoopback() {
				continue
			}
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
// （自定义 Transport + 指纹伪装握手下个别 CDN 会把压缩流原样返回，同 httpGetPlaylist）。
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
