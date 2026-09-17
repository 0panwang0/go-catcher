// 网络层测试：带重试的 HTTP GET（成功/gzip 兜底解压/HTTP 状态失败）、
// m3u8 格式校验（HTML 网页拒绝/BOM 容忍/直链媒体放行），
// 以及代理隧道 CONNECT 的读超时与超时清除。
package core

import (
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// saveRestoreHTTP 覆盖 sharedClient 与 maxRetries 全局，测试结束恢复。
func saveRestoreHTTP(t *testing.T, c *http.Client, retries int) {
	t.Helper()
	oldClient, oldRetries := testStd.sharedClient, testStd.maxRetriesNow()
	testStd.netMu.Lock()
	testStd.sharedClient = c
	testStd.netMu.Unlock()
	testStd.setDownloadTuning(-1, retries)
	t.Cleanup(func() {
		testStd.netMu.Lock()
		testStd.sharedClient = oldClient
		testStd.netMu.Unlock()
		testStd.setDownloadTuning(-1, oldRetries)
	})
}

// TestHTTPGetWithRetryGzipFallback 服务器返回 gzip 且 Transport 不自动解压时，
// httpGetWithRetry 的 Content-Encoding 兜底解压分支生效。
func TestHTTPGetWithRetryGzipFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write([]byte("hello-catcher"))
		gz.Close()
	}))
	defer srv.Close()
	// DisableCompression：模拟个别 CDN 把压缩流原样返回的场景
	saveRestoreHTTP(t, &http.Client{Transport: &http.Transport{DisableCompression: true}}, 2)
	body, status, err := testStd.httpGetWithRetry(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("httpGetWithRetry: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200", status)
	}
	if string(body) != "hello-catcher" {
		t.Fatalf("body=%q want 已解压的 hello-catcher", body)
	}
}

// TestHTTPGetWithRetryStatusFailure 非 200 重试耗尽后返回状态码与错误。
func TestHTTPGetWithRetryStatusFailure(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1) // 1 次尝试，避免长时间 sleep

	_, status, err := testStd.httpGetWithRetry(context.Background(), srv.URL, "")
	if status != http.StatusNotFound {
		t.Fatalf("status=%d want 404", status)
	}
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err=%v want 含 HTTP 404", err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d want 1（maxRetries=1）", hits)
	}
}

// TestHTTPGetWithRetrySuccess 正常 200 直接返回。
func TestHTTPGetWithRetrySuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("plain"))
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)

	body, status, err := testStd.httpGetWithRetry(context.Background(), srv.URL, "")
	if err != nil || status != http.StatusOK || string(body) != "plain" {
		t.Fatalf("body=%q status=%d err=%v", body, status, err)
	}
}

// TestHTTPGetWithRetryBadURL 非法 URL 直接报错不发起请求。
func TestHTTPGetWithRetryBadURL(t *testing.T) {
	saveRestoreHTTP(t, &http.Client{}, 3)
	_, _, err := testStd.httpGetWithRetry(context.Background(), "://bad url", "")
	if err == nil {
		t.Fatal("非法 URL 应报错")
	}
}

// TestIsM3U8Playlist m3u8 文本校验：#EXTM3U 首行（含 BOM/前导空行容忍），
// HTML/空内容一律拒绝。
func TestIsM3U8Playlist(t *testing.T) {
	valid := []string{
		"#EXTM3U\n#EXTINF:1,\ns.ts\n",
		"#EXTM3U",                         // 无换行的最简形式
		"\ufeff#EXTM3U\n#EXTINF:1,\n",     // BOM
		"\n\n#EXTM3U\n#EXTINF:1,\ns.ts\n", // 前导空行
	}
	for _, s := range valid {
		if !isM3U8Playlist([]byte(s)) {
			t.Fatalf("%q 应判定为 m3u8", s)
		}
	}
	invalid := []string{
		"",                           // 空
		"\ufeff",                     // 仅 BOM
		"<!DOCTYPE html>\n<html>...", // HTML 页面
		"random text",                // 普通文本
	}
	for _, s := range invalid {
		if isM3U8Playlist([]byte(s)) {
			t.Fatalf("%q 不应判定为 m3u8", s)
		}
	}
}

// TestFetchPlaylistRejectsHTML 解析页 URL（查询参数尾部伪装 .m3u8）拉回 HTML
// 时，fetchPlaylist 必须立即失败且不空耗重试——回归：曾把 HTML 行当分片解析，
// 探测分片 404、任务卡住 20+ 分钟才失败。
func TestFetchPlaylistRejectsHTML(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("<!DOCTYPE html>\n<html><body>parser page</body></html>"))
	}))
	defer srv.Close()
	// maxRetries=3：若误走重试路径会请求 3 次，校验应首次即失败
	saveRestoreHTTP(t, srv.Client(), 3)

	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/play/?url=https://x.com/a.m3u8", referer: ""}
	_, _, _, err := j.fetchPlaylist(context.Background())
	if err == nil || !strings.Contains(err.Error(), "不是 m3u8 播放列表") {
		t.Fatalf("err=%v want 指向非 m3u8 的明确错误", err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d want 1（校验失败不应重试）", hits)
	}
}

// TestFetchPlaylistAcceptsBOM 带 BOM 的合法播放列表正常通过校验并解析出分片。
func TestFetchPlaylistAcceptsBOM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("\ufeff#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1)

	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/index.m3u8", referer: ""}
	content, base, isDirect, err := j.fetchPlaylist(context.Background())
	if err != nil {
		t.Fatalf("fetchPlaylist: %v", err)
	}
	if isDirect {
		t.Fatal("文本播放列表不应判定为直链媒体")
	}
	if base != srv.URL+"/index.m3u8" {
		t.Fatalf("base=%q", base)
	}
	pl := parsePlaylist(content, base)
	if len(pl.segments) != 1 || pl.segments[0] != srv.URL+"/seg0.ts" {
		t.Fatalf("segments=%v want 1 个绝对分片", pl.segments)
	}
	if !pl.hasEndList {
		t.Fatal("应识别 #EXT-X-ENDLIST（点播）")
	}
}

// TestFetchPlaylistMasterSubNotPlaylist master 指向的子列表返回网页时失败，
// 不把 HTML 当子播放列表继续走管线。
func TestFetchPlaylistMasterSubNotPlaylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "master.m3u8") {
			w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nsub.m3u8\n"))
			return
		}
		w.Write([]byte("<html>not a playlist</html>"))
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1)

	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/master.m3u8", referer: ""}
	_, _, _, err := j.fetchPlaylist(context.Background())
	if err == nil || !strings.Contains(err.Error(), "子播放列表") {
		t.Fatalf("err=%v want 子播放列表校验失败", err)
	}
}

// TestSetProxyAddrSwapsClient 换代理必须整体换新共享客户端：连接池只关
// 空闲连接，归还回来的旧代理连接会被进行中任务的下一个分片继续借走。
// 置 nil 重建后 getClient 返回新实例；同值重复设置不动连接池。
func TestSetProxyAddrSwapsClient(t *testing.T) {
	oldProxy, oldClient := testStd.getProxyAddr(), testStd.sharedClient
	t.Cleanup(func() {
		testStd.netMu.Lock()
		testStd.sharedClient = oldClient
		testStd.netMu.Unlock()
		testStd.setProxyAddr(oldProxy)
	})

	// 预置一个「旧代理」客户端
	old := &http.Client{}
	testStd.netMu.Lock()
	testStd.sharedClient = old
	testStd.netMu.Unlock()

	testStd.setProxyAddr("http://10.0.0.1:8080")
	if testStd.getProxyAddr() != "http://10.0.0.1:8080" {
		t.Fatalf("proxy=%q", testStd.getProxyAddr())
	}
	if testStd.getClient() == old {
		t.Fatal("换代理后应重建客户端，旧实例不得复用")
	}

	// 同值重复设置：客户端保持原实例（配置页保存其它字段也走这里）
	cur := testStd.getClient()
	testStd.setProxyAddr("http://10.0.0.1:8080")
	if testStd.getClient() != cur {
		t.Fatal("代理未变化时不应重建客户端（白白丢连接池重握手）")
	}
}

// TestSleepCtx 退避等待必须可中断：暂停/取消不能等一整轮 sleep 跑完才生效
// （旧实现用裸 time.Sleep，最坏要等数秒）。
func TestSleepCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if sleepCtx(ctx, 5*time.Second) {
		t.Fatal("ctx 取消后 sleepCtx 应返回 false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("应在取消后立即返回，实际等了 %s", elapsed)
	}
	if !sleepCtx(context.Background(), 5*time.Millisecond) {
		t.Fatal("ctx 未取消时应睡满并返回 true")
	}
}

// silentProxy 起一个"只 accept、不回话"的假代理，返回它的地址与收尾函数。
// 用来复现"代理接受 TCP 但永不回应 CONNECT"这一类半死代理。
func silentProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假代理失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	conn := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		c.Read(make([]byte, 512)) // 收下 CONNECT 请求，然后什么都不回
		conn <- c
	}()
	t.Cleanup(func() {
		select {
		case c := <-conn:
			c.Close()
		default:
		}
	})
	return ln.Addr().String()
}

// useProxy 把共享运行时临时指向给定代理，测试结束恢复。
func useProxy(t *testing.T, addr string) {
	t.Helper()
	old := testStd.getProxyAddr()
	testStd.setProxyAddr("http://" + addr)
	t.Cleanup(func() { testStd.setProxyAddr(old) })
}

// TestDialProxyTunnelConnectReadTimeout 代理 accept 后不回 CONNECT 响应时，
// dialProxyTunnel 必须及时失败。
//
// 回归的缺陷形态：这次读原先没有任何读超时（调用方的 ctx 只作用于 Dial），
// 半死代理会让任务永久停在"连接中"，且不响应暂停/取消。
// 扰动点：删掉 conn.SetReadDeadline 那几行，本条会以"没有返回"失败。
func TestDialProxyTunnelConnectReadTimeout(t *testing.T) {
	useProxy(t, silentProxy(t))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	type outcome struct {
		err   error
		spent time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		_, derr := testStd.dialProxyTunnel(ctx, "example.com:443")
		done <- outcome{err: derr, spent: time.Since(start)}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("代理不回 CONNECT 响应时应返回错误")
		}
		if !strings.Contains(got.err.Error(), "CONNECT") {
			t.Fatalf("err=%v want 说明 CONNECT 阶段失败", got.err)
		}
		if got.spent > 3*time.Second {
			t.Fatalf("应在 300ms 的 ctx 期限附近失败，实际耗时 %s", got.spent)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dialProxyTunnel 未返回：读 CONNECT 响应缺少超时（旧实现永久阻塞在这条读上）")
	}
}

// TestDialProxyTunnelClearsReadDeadline 隧道建立后读超时必须被清掉。
//
// 这条连接接着要承载 TLS 握手与整个下载，分片之间长时间无数据是常态；
// 把 CONNECT 那一次的期限留给它，等于给所有传输埋了一个中途自断的雷。
// 扰动点：删掉建立隧道后那次 SetReadDeadline(time.Time{})，本条会读到超时错误。
func TestDialProxyTunnelClearsReadDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假代理失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	const payload = "TUNNEL-DATA"
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		c.Read(make([]byte, 512))
		c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		// 建隧道后静默超过调用方给的期限，再发数据
		time.Sleep(600 * time.Millisecond)
		c.Write([]byte(payload))
	}()
	useProxy(t, ln.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	bc, err := testStd.dialProxyTunnel(ctx, "example.com:443")
	if err != nil {
		t.Fatalf("隧道应当建立成功: %v", err)
	}
	defer bc.Close()

	// 不自己设 deadline：要验的正是"CONNECT 那一次的期限有没有留在连接上"
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(bc, got); err != nil || string(got) != payload {
		t.Fatalf("隧道建立后读超时应已清除：got=%q err=%v", got, err)
	}
}
