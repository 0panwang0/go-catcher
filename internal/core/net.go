// 网络层：uTLS 指纹 / 代理 CONNECT / HTTP client 与重试。
package core

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
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
//
// proxyAddr / sharedClient / netMu 均为 Runtime 字段（见 runtime.go），
// 访问入口是方法 getProxyAddr / setProxyAddr / getClient。

// dialProxyTunnel 连上配置的代理并向 addr 建立 CONNECT 隧道。
// 返回的 *bufferedConn 复用读取 CONNECT 响应时用的 bufio.Reader —— 响应之后
// 代理可能已经把目标数据一起发过来了，新建 Reader 会把这部分丢掉。
func (r *Runtime) dialProxyTunnel(ctx context.Context, addr string) (*bufferedConn, error) {
	proxyURL, err := url.Parse(r.effectiveProxy())
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
	return &bufferedConn{r: br, Conn: conn}, nil
}

// dialContext 处理明文 http://（及非 TLS 的 TCP）目标。
//
// http.Transport 只在目标是 https 时才调 DialTLSContext；明文请求会落到默认
// 拨号器上，也就是**完全绕过用户配置的代理** —— 需要代理的用户遇到 http CDN
// 会直连失败，甚至把真实 IP 暴露出去。这里补上：直连配置走直连，否则建立
// CONNECT 隧道（隧道内是明文 HTTP，不做 TLS 握手）。
func (r *Runtime) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if r.isDirectProxy() {
		return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, addr)
	}
	return r.dialProxyTunnel(ctx, addr)
}

func (r *Runtime) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if r.isDirectProxy() {
		return dialDirect(ctx, addr)
	}

	bConn, err := r.dialProxyTunnel(ctx, addr)
	if err != nil {
		return nil, err
	}

	// 4. uTLS 握手 — 伪造 Chrome 指纹
	host, _, _ := net.SplitHostPort(addr)

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
func (r *Runtime) effectiveProxy() string {
	p := strings.TrimSpace(r.getProxyAddr())
	if strings.EqualFold(p, "system") {
		return r.systemProxyAddr()
	}
	return p
}

// isDirectProxy 当前是否直连（含 system 模式下系统代理未启用的情况）。
func (r *Runtime) isDirectProxy() bool {
	return isDirectStr(r.effectiveProxy())
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

func (r *Runtime) newRequest(target, ref string) (*http.Request, error) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil, err
	}
	// 伪造浏览器请求头。Origin 从 Referer 的同源推导（浏览器里真实播放时就是这么发的）；
	// Referer 为空则不设 Origin。
	req.Header.Set("User-Agent", r.userAgent)
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

func (r *Runtime) httpGetWithRetry(ctx context.Context, target, ref string) ([]byte, int, error) {
	var lastErr error
	var lastStatus int
	for attempt := 1; attempt <= r.maxRetriesNow(); attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, lastStatus, err
		}
		req, err := r.newRequest(target, ref)
		if err != nil {
			return nil, 0, cleanURLParseErr(err, target)
		}
		req = req.WithContext(ctx)
		resp, err := r.getClient().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, lastStatus, ctx.Err()
			}
			lastErr = fmt.Errorf("attempt %d: %w", attempt, err)
			if !sleepCtx(ctx, time.Duration(attempt*2)*time.Second) {
				return nil, lastStatus, ctx.Err()
			}
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
			if !sleepCtx(ctx, time.Duration(attempt*2)*time.Second) {
				return nil, lastStatus, ctx.Err()
			}
			continue
		}
		// 读体带空闲超时（与分片下载同一套）：只设 ResponseHeaderTimeout 时，
		// 服务端把响应头发完就停住会让读取永久挂住。
		body, err := readAllWithIdleTimeout(resp.Body, transferIdleTimeout)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: read body: %w", attempt, err)
			if ctx.Err() != nil {
				return nil, lastStatus, ctx.Err()
			}
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
func (r *Runtime) httpGetPlaylist(ctx context.Context, target, ref string) (body []byte, isDirect bool, status int, err error) {
	const peekLen = 32 << 10 // 32KB：足够判断文本播放列表与二进制媒体头
	var lastErr error
	for attempt := 1; attempt <= r.maxRetriesNow(); attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, false, 0, err
		}
		req, rerr := r.newRequest(target, ref)
		if rerr != nil {
			return nil, false, 0, cleanURLParseErr(rerr, target)
		}
		req = req.WithContext(ctx)
		resp, rerr := r.getClient().Do(req)
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, false, 0, ctx.Err()
			}
			lastErr = fmt.Errorf("attempt %d: %w", attempt, rerr)
			if !sleepCtx(ctx, time.Duration(attempt*2)*time.Second) {
				return nil, false, 0, ctx.Err()
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d: HTTP %d", attempt, resp.StatusCode)
			if !sleepCtx(ctx, time.Duration(attempt*2)*time.Second) {
				return nil, false, 0, ctx.Err()
			}
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
		// peek/rest 都带空闲超时，closer 恒为 resp.Body：gzip.Reader.Close 不关
		// 底层连接，而空闲超时靠 Close 从另一 goroutine 中断阻塞读。
		var peekBuf bytes.Buffer
		if _, rerr = copyWithIdleTimeout(&peekBuf, limitReadCloser(rd, peekLen, resp.Body), transferIdleTimeout); rerr != nil {
			if gz != nil {
				gz.Close()
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d: read body: %w", attempt, rerr)
			continue
		}
		peek := peekBuf.Bytes()
		if isDirectMediaFile(peek, resp.Header.Get("Content-Type")) {
			if gz != nil {
				gz.Close()
			}
			resp.Body.Close()
			return peek, true, http.StatusOK, nil
		}
		rest, rerr := readAllWithIdleTimeout(&limitedReadCloser{Reader: rd, Closer: resp.Body}, transferIdleTimeout)
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

func (r *Runtime) getProxyAddr() string {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	return r.proxyAddr
}

// setProxyAddr 更新代理并整体换新共享客户端。只关空闲连接不够：连接池里
// 归还回来的旧代理连接会被进行中任务的下一个分片继续借走，一路用到下载
// 结束。置 nil 重建后，所有新发起的请求（含进行中任务的后续分片）立即走
// 新代理；仅保存瞬间在飞的请求（每任务最多分片并发数个）持旧 client 引用
// 在旧代理上收尾。值未变化时不动连接池——applyConfigLocked 对任何配置
// 保存都会调用，不能因改个并发数就迫使所有任务重握 TLS。
func (r *Runtime) setProxyAddr(v string) {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	if v == r.proxyAddr {
		return
	}
	r.proxyAddr = v
	if r.sharedClient != nil {
		old := r.sharedClient
		r.sharedClient = nil // 下次 getClient() 重建，走新代理
		old.CloseIdleConnections()
	}
}

// systemProxyAddr 读系统代理（经可注入钩子，测试可替换）。
func (r *Runtime) systemProxyAddr() string {
	if r.systemProxyAddrFn == nil {
		return ""
	}
	return r.systemProxyAddrFn()
}

// getClient 取共享 HTTP 客户端（首次调用时构建）。
//
// 这里刻意**不设** http.Client.Timeout：它计的是「从发起请求到读完整个响应体」，
// 大文件直链在慢链路上必然超过任何固定值（此前 90s 就是这么把下载掐死的）。
// 超时按阶段拆开：
//   - TLSHandshakeTimeout    TLS 握手
//   - ResponseHeaderTimeout  等响应头（服务端"收下请求就不吭声"的场景）
//   - IdleConnTimeout        连接池内空闲连接回收
//   - 响应体读取用「空闲超时」（见 copyWithIdleTimeout）：只要还在持续收到
//     数据就不算超时，连续无数据到达才判卡死
func (r *Runtime) getClient() *http.Client {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	if r.sharedClient == nil {
		r.sharedClient = &http.Client{
			Transport: &http.Transport{
				// 不走环境 HTTP(S)_PROXY——代理只由设置页 / --proxy 控制（经 DialTLSContext）
				Proxy:                 nil,
				DialContext:           r.dialContext,    // 明文 http://（Transport 不会为它调 DialTLSContext）
				DialTLSContext:        r.dialTLSContext, // https:// 走 uTLS 指纹
				MaxIdleConns:          200,
				MaxIdleConnsPerHost:   50,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		}
	}
	return r.sharedClient
}

// ============================================================
// 传输层超时
// ------------------------------------------------------------
// 固定总时长超时（如旧的 Client.Timeout=90s）与"大文件 + 慢链路"天然冲突：
// 前者解决不了，后者必然被误杀。真正要防的是"连接卡死"——TCP 半开、
// 服务端收下请求后不再发送。判据是"有没有新数据到达"，而不是"总共花了多久"。
// ============================================================

// transferIdleTimeout 响应体读取的空闲超时（连续该时长无数据到达即判卡死）。
const transferIdleTimeout = 60 * time.Second

// errTransferStalled 传输空闲超时。调用方需先查 ctx.Err() 以区分"用户暂停"。
var errTransferStalled = fmt.Errorf(
	"传输中断：连续 %s 无数据到达（连接卡死或服务端已停止响应）", transferIdleTimeout)

// copyWithIdleTimeout 把 body 拷到 dst，读空闲超过 idle 即关闭 body 中断传输。
// 返回已写字节数与错误；正常情况下 io.EOF 归零为 nil。
//
// body.Close() 是 net/http 官方支持的"从另一 goroutine 中断阻塞读"手段：
// 关闭后阻塞中的 Read 会立刻返回错误，连接同时被作废（不会把半个连接还回池）。
func copyWithIdleTimeout(dst io.Writer, body io.ReadCloser, idle time.Duration) (int64, error) {
	var stalled atomic.Bool
	timer := time.AfterFunc(idle, func() {
		stalled.Store(true)
		body.Close()
	})
	defer timer.Stop()

	buf := make([]byte, 128<<10)
	var total int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			timer.Reset(idle) // 有数据到达 → 重新计时
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if rerr != nil {
			switch {
			case errors.Is(rerr, io.EOF):
				return total, nil
			case stalled.Load():
				return total, errTransferStalled
			default:
				return total, rerr
			}
		}
	}
}

// readAllWithIdleTimeout 读整个 body 到内存，带空闲超时（小分片用）。
func readAllWithIdleTimeout(body io.ReadCloser, idle time.Duration) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := copyWithIdleTimeout(&buf, body, idle); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sleepCtx 可中断的退避等待：ctx 结束立刻返回 false，调用方据此提前退出，
// 而不是把一轮最长数秒的 sleep 白等完（暂停/取消要等好几秒才生效）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// limitedReadCloser 限制读取上限，Close 作用于指定的 closer。
//
// 为什么不是 io.NopCloser(io.LimitReader(...))：copyWithIdleTimeout 靠 body.Close()
// 从另一 goroutine 中断阻塞读（关闭后阻塞中的 Read 会立刻返回错误）。NopCloser 的
// Close 是空操作，空闲超时会因此失效、连接卡死时读永久阻塞。closer 传底层
// resp.Body 即可——即便上层套了 gzip.Reader，中断底层连接同样能让 gzip 读报错。
type limitedReadCloser struct {
	io.Reader
	io.Closer
}

// limitReadCloser 把 r 截断为最多可读 n 字节，Close 关闭 closer。
func limitReadCloser(r io.Reader, n int64, closer io.Closer) io.ReadCloser {
	return &limitedReadCloser{Reader: io.LimitReader(r, n), Closer: closer}
}

// dlJob：单个下载任务的全部状态。同一时刻可存在多个 dlJob 并行跑。
