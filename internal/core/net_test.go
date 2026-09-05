// 网络层测试：带重试的 HTTP GET（成功/gzip 兜底解压/HTTP 状态失败）。
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
	t.Cleanup(func() { sharedClient, maxRetries = oldClient, oldRetries })
	sharedClient = c
	maxRetries = retries
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
