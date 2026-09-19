package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// directMP4Stub 造一段"看起来是 MP4"的字节：合法 ftyp box 头 + 伪随机填充。
// 头必须过 isDirectMediaFile（按 magic 判定），否则整条直链路径都不会被走到，
// 测试就变成了"在测 m3u8 分支"还不自知。
func directMP4Stub(n int) []byte {
	head := []byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p',
		'm', 'p', '4', '2', 0x00, 0x00, 0x00, 0x00,
		'm', 'p', '4', '2', 'i', 's', 'o', 'm',
	}
	out := make([]byte, n)
	copy(out, head)
	for i := len(head); i < n; i++ {
		out[i] = byte(i * 31)
	}
	return out
}

// serveDirectRange 按 `bytes=start-end` 回 206；无 Range 头则回 200 全量。
func serveDirectRange(w http.ResponseWriter, r *http.Request, body []byte) {
	rg := r.Header.Get("Range")
	if !strings.HasPrefix(rg, "bytes=") {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
		return
	}
	var start, end int64
	spec := strings.TrimPrefix(rg, "bytes=")
	if i := strings.Index(spec, "-"); i >= 0 {
		start, _ = strconv.ParseInt(spec[:i], 10, 64)
		if spec[i+1:] == "" {
			end = int64(len(body)) - 1
		} else {
			end, _ = strconv.ParseInt(spec[i+1:], 10, 64)
		}
	}
	if end >= int64(len(body)) {
		end = int64(len(body)) - 1
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(body[start : end+1])
}

// startDirectTask 起一份独立运行时并提交一个直链任务，返回任务 ID 与状态条目。
//
// 用独立运行时而不是共享的 testStd：后者有并发槽，槽位被别的用例占着时任务会一直
// 停在"排队中"，等它收尾就成了不稳定用例（hardening_test.go 里记着这个坑）。
func startDirectTask(t *testing.T, rawURL, dir, filename string) (*Runtime, *taskEntry) {
	t.Helper()
	rt := newRuntime()
	rt.configPath = filepath.Join(t.TempDir(), "gocatcher_config.json")
	rt.initRuntimeConfig()
	rt.allowSaveDir(dir)

	q := url.Values{}
	q.Set("m3u8", rawURL)
	q.Set("mode", "disk")
	q.Set("dir", dir)
	q.Set("filename", filename)

	w := httptest.NewRecorder()
	(&Engine{rt: rt}).handleDownload(w, httptest.NewRequest(http.MethodGet, "/download?"+q.Encode(), nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("应 202，得到 %d body=%s", w.Code, w.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatalf("响应不是合法 JSON: %v body=%q", err, w.Body.String())
	}
	te := rt.findTask(started.ID)
	if te == nil {
		t.Fatal("任务未注册")
	}
	return rt, te
}

// TestDirectDownloadProgressReaches100 直链任务完成后 pct 必须是 100。
//
// 缺陷形态（学徒 2026-09-19 截图）：直链条目的进度条是满的、文字却是 **0.0%**。
// 根因是 pct 只由 segDone/segTot 推导，而直链没有分片 —— 收尾路径压根不碰这两个
// 字段，分母恒 0。断言落在 **DTO 的 pct** 上（前端读的就是它），而不是"有没有调
// setBytes"这类实现细节 —— 后者改个内部结构就会假绿。
//
// 两种服务器形态都覆盖，因为它们走的是两条不同代码路径：
//   - 单连接（忽略 Range、恒 200）—— 旧实现整段一次进度都不上报；
//   - 分片（支持 Range 且 > minChunkedSize）—— 旧实现上报的是片数。
func TestDirectDownloadProgressReaches100(t *testing.T) {
	const size = 3 << 20 // > minChunkedSize，支持 Range 时必然走分片
	body := directMP4Stub(size)

	for _, tc := range []struct {
		name    string
		useRng  bool
		chunked bool
	}{
		{"单连接", false, false},
		{"分片", true, false},
		{"无 Content-Length", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "video/mp4")
				if tc.chunked {
					// 先 Flush 再写 → chunked 编码，客户端拿不到总大小。
					// 这是"分母缺失"的形态：下载中只能显示"下载中…"，完成后的 100%
					// 全靠在收尾处按成品实际大小补 —— 收尾那段兜底只有这条用例有区分力。
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					w.Write(body)
					return
				}
				if !tc.useRng {
					// 恒 200：模拟不支持 Range 的服务器
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					w.Write(body)
					return
				}
				serveDirectRange(w, r, body)
			}))
			defer srv.Close()

			dir := t.TempDir()
			_, te := startDirectTask(t, srv.URL+"/movie.mp4", dir, "movie.mp4")
			waitTaskState(t, te, func(s taskState) bool { return s.done }, "直链任务收尾")

			dto := toTaskStateDTO(snapshot(te))
			if dto.Error != "" {
				t.Fatalf("直链任务不该失败: %s", dto.Error)
			}
			if dto.Pct != 100 {
				t.Fatalf("直链完成后 pct=%.1f want 100（学徒截图里的 0.0%% 就是这个缺陷）", dto.Pct)
			}
			// 直链不能借分片字段来凑百分比 —— 有值前端会渲染成「分片 N / M」
			if dto.SegDone != 0 || dto.SegTot != 0 {
				t.Fatalf("直链任务的 segDone/segTot=%d/%d 必须为 0（借分片字段会让界面显示「分片 N / M」）",
					dto.SegDone, dto.SegTot)
			}
			// 顺带确认"进度对了、文件却坏了"没被混过去
			got, err := os.ReadFile(filepath.Join(dir, "movie.mp4"))
			if err != nil {
				t.Fatalf("成品不存在: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("成品字节与源站不一致: got %d bytes want %d", len(got), len(body))
			}
		})
	}
}

// TestDirectDownloadReportsProgressWhileRunning 下载**过程中**就要有字节级进度。
//
// 学徒 2026-09-19 定的：直链下载中按字节显示百分比，而不是只写"下载中…"。
// 判据是"全程至少出现过一次 0 < pct < 100" —— 只看完成值的话，
// 一个"只在收尾时把 100 塞进去"的实现也能过，那等于没做下载中进度。
func TestDirectDownloadReportsProgressWhileRunning(t *testing.T) {
	const size = 3 << 20
	body := directMP4Stub(size)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 纯单连接形态：忽略 Range、恒 200，好让进度只能来自字节上报
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		flusher, _ := w.(http.Flusher)
		const step = 64 << 10
		for off := 0; off < len(body); off += step {
			end := off + step
			if end > len(body) {
				end = len(body)
			}
			if _, err := w.Write(body[off:end]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond) // 拉开窗口，让测试能采到中间态
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, te := startDirectTask(t, srv.URL+"/movie.mp4", dir, "movie.mp4")

	var seen []float64
	deadline := time.Now().Add(20 * time.Second)
	sawPartial := false
	for time.Now().Before(deadline) {
		dto := toTaskStateDTO(snapshot(te))
		if dto.Pct > 0 && dto.Pct < 100 {
			sawPartial = true
			seen = append(seen, dto.Pct)
			break
		}
		te.mu.Lock()
		done := te.st.done
		te.mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "直链任务收尾")

	if !sawPartial {
		t.Fatalf("下载全程没出现过 0<pct<100 —— 直链缺字节级进度上报（采到的中间值：%v）", seen)
	}
	if dto := toTaskStateDTO(snapshot(te)); dto.Pct != 100 {
		t.Fatalf("收尾后 pct=%.1f want 100", dto.Pct)
	}
}

// TestResponseTotalBytes 分母来源的三个分支都要有明确行为：
// 206 认 Content-Range 的 "/total"，200 认 Content-Length（加回起始偏移），
// 都没有则返回 0（分母未知 → 界面退回"下载中…"，由收尾补 100）。
func TestResponseTotalBytes(t *testing.T) {
	mk := func(hdr map[string]string, cl int64) *http.Response {
		h := http.Header{}
		for k, v := range hdr {
			h.Set(k, v)
		}
		return &http.Response{Header: h, ContentLength: cl}
	}

	cases := []struct {
		name   string
		resp   *http.Response
		offset int64
		want   int64
	}{
		{"206 用 Content-Range 总量", mk(map[string]string{"Content-Range": "bytes 0-0/1048576"}, 1), 0, 1048576},
		{"206 续传偏移不影响总量", mk(map[string]string{"Content-Range": "bytes 512-1023/4096"}, 4096), 512, 4096},
		{"200 用 Content-Length", mk(nil, 4096), 0, 4096},
		{"两个头都没有 => 0", mk(nil, -1), 0, 0},
		{"Content-Range 畸形 => 退回 Content-Length", mk(map[string]string{"Content-Range": "bytes 0-0/*"}, 512), 0, 512},
	}
	for _, c := range cases {
		if got := responseTotalBytes(c.resp, c.offset); got != c.want {
			t.Errorf("%s: responseTotalBytes=%d want %d", c.name, got, c.want)
		}
	}
}

// TestBytesOfChunksClamps 末片不满时不能把进度算过头（会得到 >100% 再被夹掉）。
func TestBytesOfChunksClamps(t *testing.T) {
	for _, c := range []struct {
		done      int
		size, tot int64
		want      int64
	}{
		{0, 8 << 20, 10 << 20, 0},
		{1, 8 << 20, 10 << 20, 8 << 20},
		{2, 8 << 20, 10 << 20, 10 << 20}, // 2×8MiB > 10MiB，必须夹到总量
		{3, 4, 10, 10},
	} {
		if got := bytesOfChunks(c.done, c.size, c.tot); got != c.want {
			t.Errorf("bytesOfChunks(%d,%d,%d)=%d want %d", c.done, c.size, c.tot, got, c.want)
		}
	}
}
