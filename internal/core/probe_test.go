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

// setupProbeTest 测试期强制直连并重置共享客户端，避免默认代理干扰。
func setupProbeTest(t *testing.T) {
	t.Helper()
	oldProxy, oldClient := getProxyAddr(), sharedClient
	setProxyAddr("direct")
	netMu.Lock()
	sharedClient = nil
	netMu.Unlock()
	t.Cleanup(func() {
		setProxyAddr(oldProxy)
		netMu.Lock()
		sharedClient = oldClient
		netMu.Unlock()
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
	handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", "https://page.example"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("ACAO=%q want *", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("响应应包含 playlist 文本: %q", body)
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
	handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", "https://live.example/room"))
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
	handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "#EXTINF") {
		t.Fatalf("gzip 响应应解压: %q", body)
	}
}

func TestProbeMethodNotAllowed(t *testing.T) {
	setupProbeTest(t)
	rec := httptest.NewRecorder()
	handleProbe(rec, httptest.NewRequest(http.MethodPost, "/probe?url=https://x.example/a.m3u8", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", rec.Code)
	}
}

func TestProbeBadParams(t *testing.T) {
	setupProbeTest(t)
	cases := []string{
		"",                            // 缺 url
		"ftp://cdn.example/a.m3u8",    // 非 http(s)
		"https://",                    // 无 host
		"not a url at all \x01\x02",   // 解析失败/非法
	}
	for _, raw := range cases {
		q := url.Values{}
		q.Set("url", raw)
		req := httptest.NewRequest(http.MethodGet, "/probe?"+q.Encode(), nil)
		rec := httptest.NewRecorder()
		handleProbe(rec, req)
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
	handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", ""))
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
	handleProbe(rec, probeRequest(t, upstream.URL+"/live.m3u8", ""))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("上游不可达应映射 502: status=%d", rec.Code)
	}
}
