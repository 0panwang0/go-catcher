// 网络层测试：带重试的 HTTP GET（成功/gzip 兜底解压/HTTP 状态失败）
// 与 m3u8 格式校验（HTML 网页拒绝/BOM 容忍/直链媒体放行）。
package core

import (
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// saveRestoreHTTP 覆盖 sharedClient 与 maxRetries 全局，测试结束恢复。
func saveRestoreHTTP(t *testing.T, c *http.Client, retries int) {
	t.Helper()
	oldClient, oldRetries := sharedClient, maxRetries
	netMu.Lock()
	sharedClient = c
	netMu.Unlock()
	maxRetries = retries
	t.Cleanup(func() {
		netMu.Lock()
		sharedClient = oldClient
		netMu.Unlock()
		maxRetries = oldRetries
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
	body, status, err := httpGetWithRetry(srv.URL, "")
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

	_, status, err := httpGetWithRetry(srv.URL, "")
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

	body, status, err := httpGetWithRetry(srv.URL, "")
	if err != nil || status != http.StatusOK || string(body) != "plain" {
		t.Fatalf("body=%q status=%d err=%v", body, status, err)
	}
}

// TestHTTPGetWithRetryBadURL 非法 URL 直接报错不发起请求。
func TestHTTPGetWithRetryBadURL(t *testing.T) {
	saveRestoreHTTP(t, &http.Client{}, 3)
	_, _, err := httpGetWithRetry("://bad url", "")
	if err == nil {
		t.Fatal("非法 URL 应报错")
	}
}

// TestIsM3U8Playlist m3u8 文本校验：#EXTM3U 首行（含 BOM/前导空行容忍），
// HTML/空内容一律拒绝。
func TestIsM3U8Playlist(t *testing.T) {
	valid := []string{
		"#EXTM3U\n#EXTINF:1,\ns.ts\n",
		"#EXTM3U",                        // 无换行的最简形式
		"\ufeff#EXTM3U\n#EXTINF:1,\n",    // BOM
		"\n\n#EXTM3U\n#EXTINF:1,\ns.ts\n", // 前导空行
	}
	for _, s := range valid {
		if !isM3U8Playlist([]byte(s)) {
			t.Fatalf("%q 应判定为 m3u8", s)
		}
	}
	invalid := []string{
		"",                       // 空
		"\ufeff",                 // 仅 BOM
		"<!DOCTYPE html>\n<html>...", // HTML 页面
		"random text",            // 普通文本
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

	j := &dlJob{m3u8URL: srv.URL + "/play/?url=https://x.com/a.m3u8", referer: ""}
	_, _, _, err := j.fetchPlaylist()
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

	j := &dlJob{m3u8URL: srv.URL + "/index.m3u8", referer: ""}
	content, base, isDirect, err := j.fetchPlaylist()
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

	j := &dlJob{m3u8URL: srv.URL + "/master.m3u8", referer: ""}
	_, _, _, err := j.fetchPlaylist()
	if err == nil || !strings.Contains(err.Error(), "子播放列表") {
		t.Fatalf("err=%v want 子播放列表校验失败", err)
	}
}

// TestSetProxyAddrSwapsClient 换代理必须整体换新共享客户端：连接池只关
// 空闲连接，归还回来的旧代理连接会被进行中任务的下一个分片继续借走。
// 置 nil 重建后 getClient 返回新实例；同值重复设置不动连接池。
func TestSetProxyAddrSwapsClient(t *testing.T) {
	oldProxy, oldClient := getProxyAddr(), sharedClient
	t.Cleanup(func() {
		netMu.Lock()
		sharedClient = oldClient
		netMu.Unlock()
		setProxyAddr(oldProxy)
	})

	// 预置一个「旧代理」客户端
	old := &http.Client{}
	netMu.Lock()
	sharedClient = old
	netMu.Unlock()

	setProxyAddr("http://10.0.0.1:8080")
	if getProxyAddr() != "http://10.0.0.1:8080" {
		t.Fatalf("proxy=%q", getProxyAddr())
	}
	if getClient() == old {
		t.Fatal("换代理后应重建客户端，旧实例不得复用")
	}

	// 同值重复设置：客户端保持原实例（配置页保存其它字段也走这里）
	cur := getClient()
	setProxyAddr("http://10.0.0.1:8080")
	if getClient() != cur {
		t.Fatal("代理未变化时不应重建客户端（白白丢连接池重握手）")
	}
}
