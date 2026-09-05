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
// proxyAddr 为空 / direct / none 时不走代理，直接连接（Clash 没开或访问国内资源时用）

func dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if isDirectProxy() {
		return dialDirect(ctx, addr)
	}

	proxyURL, err := url.Parse(proxyAddr)
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

// 判断是否跳过代理

func isDirectProxy() bool {
	p := strings.TrimSpace(strings.ToLower(proxyAddr))
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
	if sharedClient == nil {
		initSharedClient()
	}
	var lastErr error
	var lastStatus int
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := newRequest(target, ref)
		if err != nil {
			return nil, 0, cleanURLParseErr(err, target)
		}
		resp, err := sharedClient.Do(req)
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

// httpGetPlaylist 获取 m3u8 播放列表；若响应是直链媒体文件（MP4 等），
// 只读取开头一小段识别后即返回（isDirect=true），由上层改为流式整体下载，
// 避免把整个大文件读进内存。文本播放列表则读完剩余部分一并返回。
func httpGetPlaylist(target, ref string) (body []byte, isDirect bool, status int, err error) {
	if sharedClient == nil {
		initSharedClient()
	}
	const peekLen = 32 << 10 // 32KB：足够判断文本播放列表与二进制媒体头
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, rerr := newRequest(target, ref)
		if rerr != nil {
			return nil, false, 0, cleanURLParseErr(rerr, target)
		}
		resp, rerr := sharedClient.Do(req)
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
		return append(peek, rest...), false, http.StatusOK, nil
	}
	return nil, false, 0, lastErr
}

// ============================================================
// m3u8 解析
// ============================================================

// 从 URL 中提取 base path（用于拼接相对路径，保留尾部 /）

var sharedClient *http.Client

func initSharedClient() {
	if sharedClient != nil {
		return
	}
	transport := &http.Transport{
		// 不走环境 HTTP(S)_PROXY——代理只由显式 --proxy flag 控制（经 DialTLSContext）
		Proxy:               nil,
		DialTLSContext:      dialTLSContext,
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     90 * time.Second,
	}
	sharedClient = &http.Client{
		Transport: transport,
		Timeout:   90 * time.Second,
	}
}

// dlJob：单个下载任务的全部状态。同一时刻可存在多个 dlJob 并行跑。
