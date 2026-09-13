// /probe 预检接口测试：文本转发、Referer 透传、gzip 解压与参数/上游错误处理。
package core

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// setupProbeTest 测试期强制直连并重置共享客户端，避免默认代理干扰；
// 同时放行本机地址（上游是 httptest 起的 127.0.0.1 服务，默认会被 SSRF 防护挡掉）。
func setupProbeTest(t *testing.T) {
	t.Helper()
	oldProxy, oldClient := testStd.getProxyAddr(), testStd.sharedClient
	oldAllowLocal := testStd.probeAllowLocal
	testStd.setProxyAddr("direct")
	testStd.probeAllowLocal = true
	testStd.netMu.Lock()
	testStd.sharedClient = nil
	testStd.netMu.Unlock()
	t.Cleanup(func() {
		testStd.setProxyAddr(oldProxy)
		testStd.probeAllowLocal = oldAllowLocal
		testStd.netMu.Lock()
		testStd.sharedClient = oldClient
		testStd.netMu.Unlock()
	})
}

func probeRequest(t *testing.T, rawURL, referer string) *http.Request {
	t.Helper()
	q := url.Values{}
	q.Set("url", rawURL)
	q.Set("referer", referer)
	return httptest.NewRequest(http.MethodGet, "/probe?"+q.Encode(), nil)
}

func TestProbeForwardsPlaylist(t *testing.T) {
	setupProbeTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nseg0.ts\n"))
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	testEngine().handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", "https://page.example"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	// CORS 头已不在端点里设置：跨源读取权限统一由 auth.go 的 guard 按令牌决定
	if body := rec.Body.String(); !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("响应应包含 playlist 文本: %q", body)
	}
}

// TestValidProbeTargetBlocksInternal /probe 不能变成 SSRF 跳板：
// 环回、私网、链路本地、云元数据地址与内网主机名一律拒绝。
func TestValidProbeTargetBlocksInternal(t *testing.T) {
	// 不调用 setupProbeTest：本用例要的就是生产语义（不放行本机地址）
	blocked := []string{
		"http://127.0.0.1:7891/status",
		"http://localhost/x.m3u8",
		"http://[::1]/x.m3u8",
		"http://10.0.0.5/a.m3u8",
		"http://172.16.3.4/a.m3u8",
		"http://192.168.1.1/a.m3u8",
		"http://169.254.169.254/latest/meta-data/", // 云元数据
		"http://100.64.0.1/a.m3u8",                 // CGNAT
		"http://0.0.0.0/a.m3u8",
		"http://router.internal/a.m3u8",
		"http://nas.local/a.m3u8",
	}
	for _, raw := range blocked {
		if testStd.validProbeTarget(raw) {
			t.Errorf("应拒绝内网目标: %s", raw)
		}
	}
	if !testStd.validProbeTarget("https://cdn.example.com/live/index.m3u8") {
		t.Error("公网 https 目标应放行")
	}
}

func TestProbeForwardsReferer(t *testing.T) {
	setupProbeTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://live.example/room" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Write([]byte("#EXTM3U\n"))
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	testEngine().handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", "https://live.example/room"))
	if rec.Code != http.StatusOK {
		t.Fatalf("正确 Referer 应转发成功: status=%d", rec.Code)
	}
}

func TestProbeGunzipsBody(t *testing.T) {
	setupProbeTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write([]byte("#EXTM3U\n#EXTINF:4,\na.ts\n"))
		gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(buf.Bytes())
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	testEngine().handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "#EXTINF") {
		t.Fatalf("gzip 响应应解压: %q", body)
	}
}

// TestProbeMethodNotAllowed 方法校验由路由表的 methodGuard 提供（handler 内
// 不再重复实现）：POST /probe 必须 405 且带 Allow 头。走 newMux 才覆盖到这一层。
func TestProbeMethodNotAllowed(t *testing.T) {
	setupProbeTest(t)
	rec := httptest.NewRecorder()
	newMux(testEngine()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/probe?url=https://x.example/a.m3u8", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow=%q want GET", allow)
	}
}

func TestProbeBadParams(t *testing.T) {
	setupProbeTest(t)
	cases := []string{
		"",                          // 缺 url
		"ftp://cdn.example/a.m3u8",  // 非 http(s)
		"https://",                  // 无 host
		"not a url at all \x01\x02", // 解析失败/非法
	}
	for _, raw := range cases {
		q := url.Values{}
		q.Set("url", raw)
		req := httptest.NewRequest(http.MethodGet, "/probe?"+q.Encode(), nil)
		rec := httptest.NewRecorder()
		testEngine().handleProbe(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("url=%q status=%d want 400", raw, rec.Code)
		}
	}
}

func TestProbeUpstreamError(t *testing.T) {
	setupProbeTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	testEngine().handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", ""))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("上游 403 应映射 502: status=%d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "403") {
		t.Fatalf("响应应携带上游状态: %q", body)
	}
}

func TestProbeUpstreamUnavailable(t *testing.T) {
	setupProbeTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstream.Close() // 立即关闭，制造连接失败

	rec := httptest.NewRecorder()
	testEngine().handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", ""))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("上游不可达应映射 502: status=%d", rec.Code)
	}
}
