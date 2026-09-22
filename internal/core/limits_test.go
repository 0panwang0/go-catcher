// 读取上限与重试下限的回归（第七轮 P0-3 / P1-1）。
//
// 两条缺陷同一个成因的一半：**远端字节不可信，而"不可信"有两种表现** ——
//   - 数量上不可信（P1-1）：响应体可以无限大，也可以在很小体积里藏一个解压炸弹；
//   - 参数上不可信（P0-3）：用户能在设置页把重试次数设成 0，而 0 会让重试循环
//     一次都不进，httpGetWithRetry 于是返回"成功 + 空内容"。
//
// 两条都落在同一个判据上：**读进来的 / 没读到的，都不能被当成正常结果继续用。**
// 所以超限一律显式报错，绝不做"截断了也算成功"或"没尝试也算成功"。
package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ============================================================
// P0-3：重试次数为 0 不能变成"成功 + 空内容"
// ============================================================

// TestRetryLimitZeroKeepsAtLeastOneAttempt 设 0 后读取入口必须给出 ≥1。
func TestRetryLimitZeroKeepsAtLeastOneAttempt(t *testing.T) {
	old := testStd.maxRetriesNow()
	testStd.setDownloadTuning(-1, 0)
	t.Cleanup(func() { testStd.setDownloadTuning(-1, old) })

	if got := testStd.maxRetriesNow(); got < 1 {
		t.Fatalf("maxRetriesNow()=%d：0 的语义是「不重试」，不是「不尝试」；"+
			"夹取到 ≥1 才能让重试循环至少进一次", got)
	}
}

// TestRetryLimitZeroFailsInsteadOfReportingSuccess 服务端必失败时，重试次数 0
// 也必须**返回错误**，而不是 (nil, 0, nil)。
//
// 这是那条缺陷的可观测后果：四条循环都写成 `attempt := 1; attempt <= n; attempt++`，
// n=0 时一次都不执行，函数直接走到末尾 `return nil, lastStatus, lastErr` ——
// 而 lastErr 从未被赋值，于是返回 nil error + nil body。调用方把空体当
// 播放列表继续往下走，产物坏了而日志全绿。
func TestRetryLimitZeroFailsInsteadOfReportingSuccess(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 0)

	body, status, err := testStd.httpGetWithRetry(context.Background(), srv.URL, "")
	if err == nil {
		t.Fatalf("重试次数 0 时返回了成功（body=%d 字节 status=%d）："+
			"没尝试过就报成功，是把失败静默成空内容", len(body), status)
	}
	if len(body) != 0 {
		t.Fatalf("失败时不应带回内容，got %q", body)
	}
	if hits != 1 {
		t.Fatalf("请求次数=%d，应为 1（0 次重试 = 尝试一次）", hits)
	}
}

// ============================================================
// P1-1：播放列表读路径的体积上限
// ============================================================

// oversizePlaylistBody 造一份首行合法、总体超过上限的播放列表文本。
func oversizePlaylistBody() []byte {
	var buf bytes.Buffer
	buf.WriteString("#EXTM3U\n")
	line := append(bytes.Repeat([]byte("# pad"), 16), '\n') // 65 字节
	for int64(buf.Len()) <= maxPlaylistBytes+1024 {
		buf.Write(line)
	}
	return buf.Bytes()
}

// TestPlaylistReadIsCapped httpGetWithRetry 读超限响应必须报错，
// 不得把截断后的内容当成功返回。
func TestPlaylistReadIsCapped(t *testing.T) {
	body := oversizePlaylistBody()
	if int64(len(body)) <= maxPlaylistBytes {
		t.Fatalf("用例前提不成立：响应体 %d 字节未超上限 %d", len(body), maxPlaylistBytes)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1) // 1 次尝试；超限不必重试

	got, _, err := testStd.httpGetWithRetry(context.Background(), srv.URL, "")
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("超限响应应被显式拒绝，得到 %d 字节 / err=%v", len(got), err)
	}
	if len(got) != 0 {
		t.Fatalf("超限时不应返回（截断的）内容，got %d 字节", len(got))
	}
}

// TestPlaylistReadIsCappedOnPeekPath httpGetPlaylist 的"peek + 读剩余"路径同样有上限：
// 前 32KB 会被当直链媒体头试探一次，之后剩下的部分同样不能无限读。
func TestPlaylistReadIsCappedOnPeekPath(t *testing.T) {
	body := oversizePlaylistBody()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1)

	got, isDirect, _, err := testStd.httpGetPlaylist(context.Background(), srv.URL, "")
	if isDirect {
		t.Fatal("文本播放列表不该被判成直链媒体文件")
	}
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("超限响应应被显式拒绝，得到 %d 字节 / err=%v", len(got), err)
	}
}

// TestGzipBombIsRejected 解压后的体积同样受限。
//
// 这是 P1-1 里更狠的一半：压缩率完全由对方决定（1000:1 很平常），
// 只在压缩前设限挡不住"线上几十 KB、内存里几个 GB"。用例刻意断言压缩包本身
// **小于**上限，好让失败只可能来自解压后那道限。
func TestGzipBombIsRejected(t *testing.T) {
	var bomb bytes.Buffer
	gz := gzip.NewWriter(&bomb)
	zeros := make([]byte, 1<<20)
	for i := 0; i < 9; i++ { // 解压后 9 MiB，超过 8 MiB 上限
		if _, err := gz.Write(zeros); err != nil {
			t.Fatalf("构造压缩数据失败: %v", err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("构造压缩数据失败: %v", err)
	}
	if int64(bomb.Len()) >= maxPlaylistBytes {
		t.Fatalf("用例前提不成立：压缩包 %d 字节已不小于上限，无法证明是解压那侧拦下的", bomb.Len())
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(bomb.Bytes())
	}))
	defer srv.Close()
	// DisableCompression：模拟个别 CDN 把压缩流原样返回（与既有 gzip 兜底用例同一手法）
	saveRestoreHTTP(t, &http.Client{Transport: &http.Transport{DisableCompression: true}}, 1)

	got, _, err := testStd.httpGetWithRetry(context.Background(), srv.URL, "")
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("解压炸弹应被显式拒绝，得到 %d 字节 / err=%v", len(got), err)
	}
}

// TestNoUncappedPlaylistReadRemains 结构守卫：行为用例只证明"当前写法的上限效果"，
// 有人把调用换回无上限的读法时它们仍可能全绿（例如换个函数名但没设限）。
// 这条直接钉住"播放列表读路径不存在无上限读"。
func TestNoUncappedPlaylistReadRemains(t *testing.T) {
	src, err := os.ReadFile("net.go")
	if err != nil {
		t.Fatalf("读 net.go 失败: %v", err)
	}
	s := string(src)
	// 定义行本身不算调用点
	body := strings.ReplaceAll(s, "func readAllWithIdleTimeout(", "func <无上限读定义>")
	if strings.Contains(body, "readAllWithIdleTimeout(") {
		t.Error("net.go 的播放列表读路径又出现了无上限的 readAllWithIdleTimeout 调用")
	}
	if strings.Contains(s, "io.ReadAll(gz)") {
		t.Error("gzip 解压又回到了无上限的 io.ReadAll(gz)")
	}
	if !strings.Contains(s, "readCappedBytes(gz, maxPlaylistBytes)") {
		t.Error("gzip 解压必须经 readCappedBytes 设限")
	}
	if n := strings.Count(s, "readCappedWithIdleTimeout("); n < 3 {
		t.Errorf("readCappedWithIdleTimeout 出现 %d 次，应含 1 处定义 + 2 处调用"+
			"（httpGetWithRetry / httpGetPlaylist）", n)
	}
}
