// 直链下载断点续传测试：Range 续传、不支持回退、416 重下、失败保留 part。
package core

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rangeServer 构造支持 Range 的直链服务器：带 Range 返回 206 及余下内容。
func rangeServer(content []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if strings.HasPrefix(rng, "bytes=") {
			var start int
			fmt.Sscanf(rng, "bytes=%d-", &start)
			if start >= len(content) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start:])
			return
		}
		w.Write(content)
	}))
}

// TestDownloadDirectBasic 首次下载：无 part、200 全量写入。
func TestDownloadDirectBasic(t *testing.T) {
	content := []byte("hello-catcher")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)

	dir := t.TempDir()
	part := filepath.Join(dir, "x.mp4.part")
	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("downloadDirect: %v", err)
	}
	data, _ := os.ReadFile(part)
	if string(data) != "hello-catcher" {
		t.Fatalf("结果=%q", data)
	}
}

// TestDownloadDirectResume part 已存在时带 Range 请求，206 追加后文件完整。
func TestDownloadDirectResume(t *testing.T) {
	content := []byte("0123456789abcdef")
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=") {
			w.Header().Set("Content-Range", "bytes 5-15/16")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[5:])
			return
		}
		w.Write(content)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)

	dir := t.TempDir()
	part := filepath.Join(dir, "x.mp4.part")
	os.WriteFile(part, []byte("01234"), 0644) // 已下 5 字节

	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("downloadDirect: %v", err)
	}
	data, _ := os.ReadFile(part)
	if string(data) != "0123456789abcdef" {
		t.Fatalf("续传拼接结果=%q want 完整内容", data)
	}
	if gotRange != "bytes=5-" {
		t.Fatalf("Range 头=%q want bytes=5-", gotRange)
	}
}

// TestDownloadDirectNoRangeFallback 服务器忽略 Range 返回 200：截断旧 part 全量重下。
func TestDownloadDirectNoRangeFallback(t *testing.T) {
	content := []byte("server-full-content")
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Write(content) // 始终 200 全量（模拟不支持 Range 的服务器）
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)

	dir := t.TempDir()
	part := filepath.Join(dir, "x.mp4.part")
	os.WriteFile(part, []byte("stale-old-data"), 0644)

	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("downloadDirect: %v", err)
	}
	data, _ := os.ReadFile(part)
	if string(data) != "server-full-content" {
		t.Fatalf("200 回退应全量覆盖, got %q", data)
	}
	if gotRange != "bytes=14-" {
		t.Fatalf("Range 头=%q（探测应带偏移，旧 part 14 字节）", gotRange)
	}
}

// TestDownloadDirect416Retry part 越界（服务器内容已变）：416 后丢 part 全量重下。
func TestDownloadDirect416Retry(t *testing.T) {
	content := []byte("new-content")
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=") {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Write(content)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)

	dir := t.TempDir()
	part := filepath.Join(dir, "x.mp4.part")
	os.WriteFile(part, []byte("very-long-stale-part"), 0644) // 20 字节 > 新文件长度

	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("downloadDirect: %v", err)
	}
	data, _ := os.ReadFile(part)
	if string(data) != "new-content" {
		t.Fatalf("416 后应全量重下, got %q", data)
	}
	if hits != 2 {
		t.Fatalf("hits=%d want 2（416 + 全量）", hits)
	}
}

// TestDownloadDirectCancelKeepsPart 下载中取消：.part 保留已写内容，供任务恢复续传。
func TestDownloadDirectCancelKeepsPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < 200; i++ {
			if _, err := w.Write(make([]byte, 500)); err != nil {
				return
			}
			fl.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)

	dir := t.TempDir()
	part := filepath.Join(dir, "x.mp4.part")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	err := job.downloadDirect(ctx, part)
	if err == nil {
		t.Fatal("取消应返回错误")
	}
	fi, statErr := os.Stat(part)
	if statErr != nil {
		t.Fatalf("取消后 .part 应保留: %v", statErr)
	}
	if fi.Size() == 0 {
		t.Fatal("已写内容不应为空")
	}
}

// saveRestoreSeg 保存并恢复任务内分片并发数全局。
func saveRestoreSeg(t *testing.T, n int) {
	t.Helper()
	old := testStd.concurrencyNow()
	t.Cleanup(func() { testStd.setDownloadTuning(old, -1) })
	testStd.setDownloadTuning(n, -1)
}

// TestNewChunkMeta 分片均分与末片边界。
func TestNewChunkMeta(t *testing.T) {
	m := newChunkMeta(1000, 4)
	if m.Size != 250 || len(m.Done) != 4 {
		t.Fatalf("1000/4: size=%d n=%d want 250,4", m.Size, len(m.Done))
	}
	m2 := newChunkMeta(999, 4) // 末片 249
	if m2.Size != 250 || len(m2.Done) != 4 {
		t.Fatalf("999/4: size=%d n=%d want 250,4", m2.Size, len(m2.Done))
	}
	m3 := newChunkMeta(10, 3) // ceil(10/3)=4，3 片
	if m3.Size != 4 || len(m3.Done) != 3 {
		t.Fatalf("10/3: size=%d n=%d want 4,3", m3.Size, len(m3.Done))
	}
	m4 := newChunkMeta(100, 1)
	if m4.Size != 100 || len(m4.Done) != 1 {
		t.Fatalf("100/1: size=%d n=%d want 100,1", m4.Size, len(m4.Done))
	}
}

// TestDownloadDirectChunked 大文件走并发分片：探测 + N 路 Range，文件完整、meta 删除。
func TestDownloadDirectChunked(t *testing.T) {
	content := bytes.Repeat([]byte("abcde"), 512*1024) // 2.5MB > 1MB 阈值
	srv := rangeServer(content)
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)
	saveRestoreSeg(t, 4)

	dir := t.TempDir()
	part := filepath.Join(dir, "big.mp4.part")
	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("downloadDirect: %v", err)
	}
	data, err := os.ReadFile(part)
	if err != nil {
		t.Fatalf("读取结果: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Fatalf("分片拼接损坏: len=%d want %d", len(data), len(content))
	}
	if _, err := os.Stat(part + ".meta"); !os.IsNotExist(err) {
		t.Fatal("完成后 .meta 应删除")
	}
}

// TestDownloadDirectChunkedResume 位图续传：已完成片跳过，只重下未完成片。
func TestDownloadDirectChunkedResume(t *testing.T) {
	content := bytes.Repeat([]byte("xyz"), 512*1024) // 1.5MB
	var fullRanges int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if strings.HasPrefix(rng, "bytes=") {
			if rng == "bytes=0-0" {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(content)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(content[:1])
				return
			}
			var start, end int
			fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
			atomic.AddInt32(&fullRanges, 1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start : end+1])
			return
		}
		w.Write(content)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)
	saveRestoreSeg(t, 4)

	dir := t.TempDir()
	part := filepath.Join(dir, "big.mp4.part")
	total := int64(len(content))
	m := newChunkMeta(total, 4)
	// 预写前 2 片并标记完成，模拟中断后的位图
	os.WriteFile(part, content[:2*m.Size], 0644)
	m.Done[0], m.Done[1] = true, true
	if err := saveChunkMeta(part, m); err != nil {
		t.Fatalf("saveChunkMeta: %v", err)
	}

	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("downloadDirect: %v", err)
	}
	data, _ := os.ReadFile(part)
	if !bytes.Equal(data, content) {
		t.Fatalf("续传拼接损坏: len=%d want %d", len(data), len(content))
	}
	if got := atomic.LoadInt32(&fullRanges); got != 2 {
		t.Fatalf("完整分片请求数=%d want 2（只下未完成片）", got)
	}
	if _, err := os.Stat(part + ".meta"); !os.IsNotExist(err) {
		t.Fatal("完成后 .meta 应删除")
	}
}

// TestDownloadDirectSmallFile 小文件不触发分片：探测后走单连接全量。
func TestDownloadDirectSmallFile(t *testing.T) {
	content := bytes.Repeat([]byte("ab"), 250*1024) // 500KB < 1MB 阈值
	var mainReqRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[:1])
			return
		}
		if strings.HasPrefix(rng, "bytes=") {
			var start, end int
			fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start : end+1])
			return
		}
		mainReqRange = ""
		w.Write(content)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)

	dir := t.TempDir()
	part := filepath.Join(dir, "small.mp4.part")
	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("downloadDirect: %v", err)
	}
	data, _ := os.ReadFile(part)
	if !bytes.Equal(data, content) {
		t.Fatalf("小文件损坏: len=%d want %d", len(data), len(content))
	}
	if mainReqRange != "" {
		t.Fatalf("小文件应走单连接（主请求无 Range），got %q", mainReqRange)
	}
}

// TestDownloadDirectChunk416 分片下载中服务器 416（内容已变）：丢弃 part/meta 并报错。
func TestDownloadDirectChunk416(t *testing.T) {
	content := bytes.Repeat([]byte("q"), 2<<20) // 2MB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[:1])
			return
		}
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)
	saveRestoreSeg(t, 2)

	dir := t.TempDir()
	part := filepath.Join(dir, "big.mp4.part")
	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	err := job.downloadDirect(context.Background(), part)
	if err == nil {
		t.Fatal("416 应报错")
	}
	if _, serr := os.Stat(part); !os.IsNotExist(serr) {
		t.Fatal("416 后 .part 应删除（内容已变）")
	}
	if _, merr := os.Stat(part + ".meta"); !os.IsNotExist(merr) {
		t.Fatal("416 后 .meta 应删除")
	}
}

// TestStreamWriterFlushBreakpointNotAhead 可持久化断点（onFlush 回调值）永远不得
// 超过 .part 文件里真实存在的字节数（R9）。旧实现把 bufio 缓冲里的数据也算进
// 断点，Engine.Stop 超时落盘后重启续传会在文件中间留下空洞。
func TestStreamWriterFlushBreakpointNotAhead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.ts")
	const (
		segCount = 8
		segSize  = 300 << 10 // 8 × 300KB = 2.4MB，足以跨过 1MB 刷新阈值
	)
	flushed := 0
	sw, err := newStreamWriter(path, 0, func(int) {}, func(f int) {
		if f > flushed {
			flushed = f
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < segCount; i++ {
		if err := sw.submit(i, bytes.Repeat([]byte{byte(i + 1)}, segSize)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() < int64(flushed)*segSize {
			t.Fatalf("断点 %d 声称已落盘，但文件仅 %d 字节（应 ≥ %d）", flushed, fi.Size(), flushed*segSize)
		}
		if flushed > i+1 {
			t.Fatalf("断点 %d 超过已提交分片数 %d", flushed, i+1)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != segCount*segSize {
		t.Fatalf("最终文件大小 %d want %d", fi.Size(), segCount*segSize)
	}
	if flushed != segCount {
		t.Fatalf("Close 后断点应推进到 %d, got %d", segCount, flushed)
	}
}

// TestDownloadRangeLimitsOversizedResponse 缺陷形态回归：服务器忽略 Range 的
// 结束偏移、把整份文件回给一个分片请求时（行为不当的 CDN / 中间层），多出的
// 字节绝不能写进相邻分片的区域 —— 邻片若已标记完成就不会再被重写，成品里
// 那段永久是坏数据，而抽样校验未必命中。
func TestDownloadRangeLimitsOversizedResponse(t *testing.T) {
	content := bytes.Repeat([]byte{0xAB}, 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 故意无视 Range 的结束偏移：无论要哪一段，整个文件回过去
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content)
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1)

	path := filepath.Join(t.TempDir(), "x.bin")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	job := &dlJob{rt: testStd, m3u8URL: srv.URL}
	// 请求 [0,99]：期望 100 字节，服务器回 4096
	if err := job.downloadRange(context.Background(), f, 0, 99); err != nil {
		t.Fatalf("前 100 字节内容正确，不应判失败: %v", err)
	}
	head := make([]byte, 100)
	if _, err := f.ReadAt(head, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(head, content[:100]) {
		t.Fatal("分片写入内容应等于源文件前 100 字节")
	}
	// 文件必须恰好在 [0,99] 结束：多出的 3996 字节一旦落盘就会占住邻片区域
	if fi, _ := f.Stat(); fi.Size() != 100 {
		t.Fatalf("文件大小=%d want 100（服务器多回的字节不得落盘）", fi.Size())
	}
}

// TestOffsetWriterTruncatesAtEnd offsetWriter 必须把写入截断在区间末尾：
// 即便上游忘了限长，越界字节也不能落进邻片区域（纵深防御）。
func TestOffsetWriterTruncatesAtEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.bin")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := &offsetWriter{f: f, off: 10, end: 19} // 只允许写 [10,19]
	n, err := w.Write(bytes.Repeat([]byte{0xEE}, 100))
	if err != nil || n != 10 {
		t.Fatalf("越界写入应被截断到区间末尾：n=%d err=%v want n=10", n, err)
	}
	buf := make([]byte, 20)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if buf[i] != 0 {
			t.Fatalf("区间起点之前不应被写：偏移 %d = %#x", i, buf[i])
		}
	}
	for i := 10; i < 20; i++ {
		if buf[i] != 0xEE {
			t.Fatalf("区间内应被写：偏移 %d = %#x", i, buf[i])
		}
	}
	if fi, _ := f.Stat(); fi.Size() != 20 {
		t.Fatalf("文件大小=%d want 20（越界字节不得落盘）", fi.Size())
	}
	if n2, err2 := w.Write([]byte("more")); err2 != nil || n2 != 0 {
		t.Fatalf("写满后应安全丢弃：n=%d err=%v", n2, err2)
	}
}
