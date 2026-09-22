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

// TestNoUncappedBodyReadRemains 结构守卫：行为用例只证明"当前写法的上限效果"，
// 有人把调用换回无上限的读法时它们仍可能全绿（例如换个函数名但没设限）。
// 这条直接钉住"读远端响应体的地方不存在无上限读"。
//
// 覆盖两侧 —— 第七轮 P1-1 的播放列表侧与 E1 的媒体分片侧：
//   - net.go：播放列表读（httpGetWithRetry / httpGetPlaylist）+ gzip 解压
//   - download.go：媒体分片读（fetchSegment）
//
// 两侧都必须经 readCapped* 带 limit 读；无上限的 readAllWithIdleTimeout 应已删除。
func TestNoUncappedBodyReadRemains(t *testing.T) {
	// 无上限读函数一旦回来，多半是被当成"方便的工具"重新复用。扫整个包而不是
	// 只扫 net.go —— 它换个文件复活，只看 net.go 的守卫就漏了。
	//
	// 跳过 _test.go：测试里出现这个函数名是**断言文本**（本条守卫自己就写着它），
	// 不是调用点；生产代码才是要守的那一侧。
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("列举包内文件失败: %v", err)
	}
	var scanned int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		scanned++
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("读 %s 失败: %v", e.Name(), err)
		}
		if strings.Contains(string(b), "readAllWithIdleTimeout") {
			t.Errorf("%s 里又出现了无上限读函数 readAllWithIdleTimeout —— "+
				"它不做体积限制，任何持续输出的响应体都能把它灌到 OOM", e.Name())
		}
	}
	// 防退化：枚举若失效（工作目录不对、读不到文件），上面整段会静默变成恒真。
	if scanned < 10 {
		t.Fatalf("只扫到 %d 个非测试 .go 文件，枚举明显失效（本包生产文件远不止于此）", scanned)
	}

	src, err := os.ReadFile("net.go")
	if err != nil {
		t.Fatalf("读 net.go 失败: %v", err)
	}
	s := string(src)
	if strings.Contains(s, "io.ReadAll(gz)") {
		t.Error("gzip 解压又回到了无上限的 io.ReadAll(gz)：压缩率由对方决定，无上限就是解压炸弹")
	}
	if !strings.Contains(s, "readCappedBytes(gz, maxPlaylistBytes)") {
		t.Error("gzip 解压必须经 readCappedBytes 设限")
	}
	if n := strings.Count(s, "readCappedWithIdleTimeout("); n < 3 {
		t.Errorf("net.go 的 readCappedWithIdleTimeout 出现 %d 次，应含 1 处定义 + 2 处调用"+
			"（httpGetWithRetry / httpGetPlaylist）", n)
	}

	dlSrc, err := os.ReadFile("download.go")
	if err != nil {
		t.Fatalf("读 download.go 失败: %v", err)
	}
	if !strings.Contains(string(dlSrc),
		"readCappedWithIdleTimeout(resp.Body, resp.Body, j.rt.segBodyLimitNow()") {
		t.Error("fetchSegment 读分片必须经 readCappedWithIdleTimeout + segBodyLimitNow 设限：" +
			"空闲超时只挡「对方卡住」，挡不住「对方一直吐」")
	}
}

// ============================================================
// E1：媒体分片读路径的体积上限
// ============================================================

// saveRestoreSegBodyLimit 临时把分片读体上限调到测试造得出的大小。
//
// 生产值是 maxSegmentBytes（256 MiB），要触发超限得真造并传输 256 MiB ——
// 单测承受不了（-race 下更甚），所以这个上限必须可注入。
func saveRestoreSegBodyLimit(t *testing.T, n int64) {
	t.Helper()
	old := testStd.segBodyBytes
	t.Cleanup(func() { testStd.segBodyBytes = old })
	testStd.segBodyBytes = n
}

// TestSegmentBodyIsCapped 分片读体超限必须显式失败。
//
// 这是分片路径上原先唯一缺的那道闸：空闲超时管的是"对方卡住不动"，而持续
// 输出的源站永远不空闲 —— 无上限的读会一路吃到 OOM，且日志上一个错字都没有。
// 顺带钉住"超限不空耗重试"（同一个 URL 再试还是同一份超限内容）。
func TestSegmentBodyIsCapped(t *testing.T) {
	const limit = 4096
	saveRestoreSegBodyLimit(t, limit)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(bytes.Repeat([]byte("S"), int(limit)+1024))
	}))
	defer srv.Close()
	// 给足重试次数：超限必须在第一次就停，若继续重试 hits 会 > 1
	saveRestoreHTTP(t, srv.Client(), 5)

	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/seg.ts"}
	data, err := fetchSegment(context.Background(), j, srv.URL+"/seg.ts")
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("超限分片应被显式拒绝，得到 %d 字节 / err=%v", len(data), err)
	}
	if data != nil {
		t.Fatalf("超限不得返回（截断的）分片内容，got %d 字节：截断内容会被按正常"+
			"分片写进产物 —— 产物坏了而日志正常", len(data))
	}
	if hits != 1 {
		t.Fatalf("请求次数=%d，应为 1：同一个 URL 重试只会再拿回同一份超限内容", hits)
	}
}

// TestSegmentBodyUnderLimitPasses 上限内的分片必须原样通过：设限不得误伤。
//
// 单测上一条（超限被拒）挡不住"把上限设成 0 / 设得过小"这类改坏 —— 那种改法
// 会让上一条更绿。这条与它成对：一条管"该拒的拒了"，一条管"该放的放了"。
func TestSegmentBodyUnderLimitPasses(t *testing.T) {
	const limit = 4096
	saveRestoreSegBodyLimit(t, limit)

	payload := bytes.Repeat([]byte("S"), 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1)

	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/seg.ts"}
	data, err := fetchSegment(context.Background(), j, srv.URL+"/seg.ts")
	if err != nil {
		t.Fatalf("上限内的分片不该失败: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("分片内容被改动：got %d 字节 want %d", len(data), len(payload))
	}
}

// TestSegmentDefaultLimitIsGenerousAndFallback 生产默认上限必须"宽到不会误伤
// 合法分片"，且字段为 0 时回落到它（0 是"用默认"，不是"不许任何字节"）。
//
// 这条挡的是"把默认值调小"这类改坏：调小之后上面两条行为用例仍会全绿
// （它们用的是注入的小上限），但真实的高码率大分片会被当成超限拒掉 ——
// 产物坏了，日志却说"响应体超过体积上限"，判据看起来还挺合理。
func TestSegmentDefaultLimitIsGenerousAndFallback(t *testing.T) {
	// 合法分片大小跨三个数量级：音频片几十 KB，4K 高码率片可上百 MB。
	// 默认值必须高过后者。64 MiB 是保守下界（约等于 20 秒的 25 Mbps 片）。
	const floor = 64 << 20
	if maxSegmentBytes < floor {
		t.Errorf("maxSegmentBytes=%d 小于 %d：默认上限宽不到足以容纳合法的大分片，"+
			"真实高码率流会被误判成超限", maxSegmentBytes, floor)
	}

	old := testStd.segBodyBytes
	t.Cleanup(func() { testStd.segBodyBytes = old })
	testStd.segBodyBytes = 0
	if got := testStd.segBodyLimitNow(); got != maxSegmentBytes {
		t.Errorf("segBodyBytes=0 时应回落到 maxSegmentBytes(%d)，得到 %d", maxSegmentBytes, got)
	}
}
