// 网络层：uTLS 指纹 / 代理 CONNECT / HTTP client 与重试。
package core

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

type bufferedConn struct {
	r *bufio.Reader
	net.Conn
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

// dialTLSContext: 连接 Clash 代理 → CONNECT 隧道 → uTLS 伪造 Chrome 指纹握手
// 代理为空 / direct / none 时不走代理，直接连接（Clash 没开或访问国内资源时用）

func dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	proxy := effectiveProxy()
	if isDirectStr(proxy) {
		return dialDirect(ctx, addr)
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, err
	}

	// 1. TCP 连接到 Clash 代理
	conn, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("连接代理失败: %w", err)
	}

	// 2. 发送 HTTP CONNECT 建立隧道
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 CONNECT 失败: %w", err)
	}

	// 3. 读取 CONNECT 响应
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取 CONNECT 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 失败: %s", resp.Status)
	}

	// 4. uTLS 握手 — 伪造 Chrome 指纹
	host, _, _ := net.SplitHostPort(addr)
	bConn := &bufferedConn{r: br, Conn: conn}

	uConn := utls.UClient(bConn, &utls.Config{
		ServerName: host,
	}, utls.HelloCustom)

	// 获取 Chrome 指纹 spec，覆盖 ALPN 为 http/1.1 避免 HTTP/2 问题
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		bConn.Close()
		return nil, fmt.Errorf("构建 TLS spec 失败: %w", err)
	}
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
	if err := uConn.ApplyPreset(&spec); err != nil {
		bConn.Close()
		return nil, fmt.Errorf("应用 TLS spec 失败: %w", err)
	}

	if err := uConn.HandshakeContext(ctx); err != nil {
		bConn.Close()
		return nil, fmt.Errorf("TLS 握手失败: %w", err)
	}

	return uConn, nil
}

// effectiveProxy 返回本次连接实际使用的代理地址（空 = 直连）。
// "system" 模式每次建连现读注册表：Clash 开关系统代理、改端口即时跟随。
func effectiveProxy() string {
	p := strings.TrimSpace(getProxyAddr())
	if strings.EqualFold(p, "system") {
		return systemProxyAddr()
	}
	return p
}

// isDirectProxy 当前是否直连（含 system 模式下系统代理未启用的情况）。
func isDirectProxy() bool {
	return isDirectStr(effectiveProxy())
}

// isDirectStr 代理串的直连判定（空 / direct / none / off，不区分大小写）。
func isDirectStr(p string) bool {
	p = strings.TrimSpace(strings.ToLower(p))
	return p == "" || p == "direct" || p == "none" || p == "off"
}

// 不走代理时的直连 + uTLS 握手（指纹照旧伪造）

func dialDirect(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("直连失败: %w", err)
	}
	host, _, _ := net.SplitHostPort(addr)
	uConn := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
	if err := uConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS 握手失败: %w", err)
	}
	return uConn, nil
}

func newRequest(target, ref string) (*http.Request, error) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil, err
	}
	// 伪造浏览器请求头。Origin 从 Referer 的同源推导（浏览器里真实播放时就是这么发的）；
	// Referer 为空则不设 Origin。
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", ref)
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
		req.Header.Set("Origin", u.Scheme+"://"+u.Host)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Ch-Ua", `"Microsoft Edge";v="150", "Not?A_Brand";v="99", "Chromium";v="150"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	return req, nil
}

// sanitizeURLForError 把 URL 里不可打印的控制字符替换成 ?，并截断到可读长度。
// url.Parse 报错时会把整段原始 URL（可能含一大坨二进制）带进错误信息，
// 直接展示会把前端弹窗撑爆，这里用于生成安全可读的错误摘要。
func sanitizeURLForError(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] < 0x20 || b[i] == 0x7f {
			b[i] = '?'
		}
	}
	out := string(b)
	if len(out) > 160 {
		out = out[:160] + "…"
	}
	return out
}

// cleanURLParseErr 把 url.Parse 类报错里的原始 URL 换成消毒后的短摘要。
// Go 的报错形如 parse "<URL>": net/url: invalid control character in URL，
// 取引号后面的错误类型即可，URL 单独用 sanitizeURLForError 展示。
func cleanURLParseErr(err error, target string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, "\": "); i >= 0 {
		msg = msg[i+3:]
	}
	return fmt.Errorf("%s（链接: %s）", msg, sanitizeURLForError(target))
}

// 带重试的 HTTP GET

func httpGetWithRetry(target, ref string) ([]byte, int, error) {
	var lastErr error
	var lastStatus int
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := newRequest(target, ref)
		if err != nil {
			return nil, 0, cleanURLParseErr(err, target)
		}
		resp, err := getClient().Do(req)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: %w", attempt, err)
			time.Sleep(time.Duration(attempt*2) * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastStatus = resp.StatusCode
			// 打印响应头帮助调试
			if attempt == 1 {
				fmt.Printf("\n  HTTP %d, 响应头:\n", resp.StatusCode)
				for k, v := range resp.Header {
					fmt.Printf("    %s: %s\n", k, v)
				}
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d: HTTP %d", attempt, resp.StatusCode)
			time.Sleep(time.Duration(attempt*2) * time.Second)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: read body: %w", attempt, err)
			continue
		}
		// 显式解压：Go 在请求未显式声明 Accept-Encoding 时会自动解压 gzip，
		// 但自定义 Transport + uTLS 握手下个别 CDN 仍可能把压缩流原样返回。
		// 此处按 Content-Encoding 兜底（若 Go 已解压，该头会被移除，不会二次解压）。
		if enc := resp.Header.Get("Content-Encoding"); strings.Contains(enc, "gzip") {
			if gz, gerr := gzip.NewReader(bytes.NewReader(body)); gerr == nil {
				if ub, uerr := io.ReadAll(gz); uerr == nil {
					body = ub
				}
				gz.Close()
			}
		}
		return body, http.StatusOK, nil
	}
	return nil, lastStatus, lastErr
}

// isM3U8Playlist 校验响应体是否为 m3u8 播放列表文本：首个非空行以 #EXTM3U
// 开头（RFC 8216 规定其必须为首行；容忍 BOM 与前导空行）。
// 解析页 URL 形如 https://parser.com/play/?url=….m3u8（查询参数尾部伪装成
// .m3u8），拉回的是 HTML 网页——不校验直接进 parsePlaylist 会把每行 HTML
// 当分片 URL 解析，任务表现为探测分片 404 / 长时间卡住后失败。
func isM3U8Playlist(body []byte) bool {
	s := strings.TrimPrefix(string(body), "\ufeff")
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, "#EXTM3U")
	}
	return false
}

// httpGetPlaylist 获取 m3u8 播放列表；若响应是直链媒体文件（MP4 等），
// 只读取开头一小段识别后即返回（isDirect=true），由上层改为流式整体下载，
// 避免把整个大文件读进内存。文本播放列表则读完剩余部分一并返回。
func httpGetPlaylist(target, ref string) (body []byte, isDirect bool, status int, err error) {
	const peekLen = 32 << 10 // 32KB：足够判断文本播放列表与二进制媒体头
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, rerr := newRequest(target, ref)
		if rerr != nil {
			return nil, false, 0, cleanURLParseErr(rerr, target)
		}
		resp, rerr := getClient().Do(req)
		if rerr != nil {
			lastErr = fmt.Errorf("attempt %d: %w", attempt, rerr)
			time.Sleep(time.Duration(attempt*2) * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d: HTTP %d", attempt, resp.StatusCode)
			time.Sleep(time.Duration(attempt*2) * time.Second)
			continue
		}

		rd := io.Reader(resp.Body)
		var gz io.ReadCloser
		if enc := resp.Header.Get("Content-Encoding"); strings.Contains(enc, "gzip") {
			if gz, rerr = gzip.NewReader(resp.Body); rerr == nil {
				rd = gz
			} else {
				gz = nil // 解压失败就按原样读（极少见）
			}
		}
		peek, rerr := io.ReadAll(io.LimitReader(rd, peekLen))
		if rerr != nil {
			if gz != nil {
				gz.Close()
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d: read body: %w", attempt, rerr)
			continue
		}
		if isDirectMediaFile(peek, resp.Header.Get("Content-Type")) {
			if gz != nil {
				gz.Close()
			}
			resp.Body.Close()
			return peek, true, http.StatusOK, nil
		}
		rest, rerr := io.ReadAll(rd)
		if gz != nil {
			gz.Close()
		}
		resp.Body.Close()
		if rerr != nil {
			lastErr = fmt.Errorf("attempt %d: read body: %w", attempt, rerr)
			continue
		}
		full := append(peek, rest...)
		if !isM3U8Playlist(full) {
			// 200 但内容不是播放列表（网页/解析页）：重试同样结果，直接失败并
			// 给出可行动的错误，不再空耗 maxRetries 轮
			return nil, false, 0, fmt.Errorf(
				"URL 指向的不是 m3u8 播放列表（响应为网页内容，请确认选择 .m3u8 直链，而非播放页/解析页链接）")
		}
		return full, false, http.StatusOK, nil
	}
	return nil, false, 0, lastErr
}

// ============================================================
// m3u8 解析
// ============================================================

// 从 URL 中提取 base path（用于拼接相对路径，保留尾部 /）

var (
	netMu        sync.Mutex // 保护 proxyAddr 与 sharedClient（代理运行时可经 /config 修改）
	proxyAddr    = "system" // 默认跟随 Windows 系统代理（Clash 开箱即用）
	sharedClient *http.Client
)

func getProxyAddr() string {
	netMu.Lock()
	defer netMu.Unlock()
	return proxyAddr
}

// setProxyAddr 更新代理并整体换新共享客户端。只关空闲连接不够：连接池里
// 归还回来的旧代理连接会被进行中任务的下一个分片继续借走，一路用到下载
// 结束。置 nil 重建后，所有新发起的请求（含进行中任务的后续分片）立即走
// 新代理；仅保存瞬间在飞的请求（每任务最多分片并发数个）持旧 client 引用
// 在旧代理上收尾。值未变化时不动连接池——applyConfigLocked 对任何配置
// 保存都会调用，不能因改个并发数就迫使所有任务重握 TLS。
func setProxyAddr(v string) {
	netMu.Lock()
	defer netMu.Unlock()
	if v == proxyAddr {
		return
	}
	proxyAddr = v
	if sharedClient != nil {
		old := sharedClient
		sharedClient = nil // 下次 getClient() 重建，走新代理
		old.CloseIdleConnections()
	}
}

// getClient 取共享 HTTP 客户端（首次调用时构建）。
func getClient() *http.Client {
	netMu.Lock()
	defer netMu.Unlock()
	if sharedClient == nil {
		sharedClient = &http.Client{
			Transport: &http.Transport{
				// 不走环境 HTTP(S)_PROXY——代理只由设置页 / --proxy 控制（经 DialTLSContext）
				Proxy:               nil,
				DialTLSContext:      dialTLSContext,
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 90 * time.Second,
		}
	}
	return sharedClient
}

// dlJob：单个下载任务的全部状态。同一时刻可存在多个 dlJob 并行跑。
