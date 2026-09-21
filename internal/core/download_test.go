// 直链下载断点续传测试：Range 续传、不支持回退、416 重下、失败保留 part。
package core

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// saveRestoreChunkSize 临时把直链片长调小（生产是 8MiB，测试文件没那么大）。
// 片长决定片数，进而决定走不走多片路径 —— 不调小的话小测试文件只会切出 1 片。
func saveRestoreChunkSize(t *testing.T, n int64) {
	t.Helper()
	old := testStd.chunkSizeBytes
	t.Cleanup(func() { testStd.chunkSizeBytes = old })
	testStd.chunkSizeBytes = n
}

// TestNewChunkMeta 固定片长切分与末片边界。
func TestNewChunkMeta(t *testing.T) {
	m := newChunkMeta(1000, 250)
	if m.Size != 250 || len(m.Done) != 4 {
		t.Fatalf("1000/250: size=%d n=%d want 250,4", m.Size, len(m.Done))
	}
	m2 := newChunkMeta(999, 250) // 末片 249
	if m2.Size != 250 || len(m2.Done) != 4 {
		t.Fatalf("999/250: size=%d n=%d want 250,4", m2.Size, len(m2.Done))
	}
	m3 := newChunkMeta(10, 4) // ceil(10/4)=3
	if m3.Size != 4 || len(m3.Done) != 3 {
		t.Fatalf("10/4: size=%d n=%d want 4,3", m3.Size, len(m3.Done))
	}
	m4 := newChunkMeta(1000, 1000) // 整片：不凑第二片
	if m4.Size != 1000 || len(m4.Done) != 1 {
		t.Fatalf("1000/1000: size=%d n=%d want 1000,1", m4.Size, len(m4.Done))
	}
	m5 := newChunkMeta(1000, 0) // 片长非法 → 退回默认片长
	if m5.Size != chunkSizeFixed || len(m5.Done) != 1 {
		t.Fatalf("片长非法应退回默认: size=%d n=%d", m5.Size, len(m5.Done))
	}
}

// TestChunkCount 片数计算：向上取整，且不因 total+size 溢出（total 是远端声明值）。
func TestChunkCount(t *testing.T) {
	cases := []struct {
		total, size, want int64
	}{
		{1000, 250, 4},
		{999, 250, 4},
		{1000, 333, 4}, // 3×333=999 < 1000
		{1, chunkSizeFixed, 1},
		{chunkSizeFixed, chunkSizeFixed, 1},
		{chunkSizeFixed + 1, chunkSizeFixed, 2},
		{1000, 0, 1}, // 片长非法 → 默认片长
		// 旧写法 (total+size-1)/size 在这里会溢出成负数
		{math.MaxInt64, chunkSizeFixed, math.MaxInt64/chunkSizeFixed + 1},
	}
	for _, c := range cases {
		if got := chunkCount(c.total, c.size); got != c.want {
			t.Errorf("chunkCount(%d, %d)=%d want %d", c.total, c.size, got, c.want)
		}
	}
}

// TestLoadChunkMetaRejectsInconsistent 位图与片划分不自洽时一律当没有位图。
//
// 短了会漏下尾部（成品短一截）、长了会在末尾请求越界区间（416 → 白删重下），
// 两者都比"重下"更糟，所以宁可不认。
func TestLoadChunkMetaRejectsInconsistent(t *testing.T) {
	part := filepath.Join(t.TempDir(), "x.mp4.part")
	cases := []struct {
		name string
		meta chunkMeta
		want bool
	}{
		{"自洽", chunkMeta{Total: 1000, Size: 250, Done: make([]bool, 4)}, true},
		{"长度短了", chunkMeta{Total: 1000, Size: 250, Done: make([]bool, 3)}, false},
		{"长度长了", chunkMeta{Total: 1000, Size: 250, Done: make([]bool, 5)}, false},
		{"Total 非正", chunkMeta{Total: 0, Size: 250, Done: make([]bool, 4)}, false},
		{"Size 非正", chunkMeta{Total: 1000, Size: 0, Done: make([]bool, 4)}, false},
		{"空位图", chunkMeta{Total: 1000, Size: 250}, false},
	}
	for _, c := range cases {
		if err := saveChunkMeta(part, &c.meta); err != nil {
			t.Fatalf("saveChunkMeta: %v", err)
		}
		_, ok := loadChunkMeta(part)
		if ok != c.want {
			t.Errorf("%s: loadChunkMeta ok=%v want %v", c.name, ok, c.want)
		}
	}
}

// TestDownloadDirectChunked 大文件走并发分片：探测 + N 路 Range，文件完整、meta 删除。
func TestDownloadDirectChunked(t *testing.T) {
	content := bytes.Repeat([]byte("abcde"), 512*1024) // 2.5MB
	const chunk = 512 << 10                            // 512KiB/片 → 5 片
	var ranges int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "bytes=0-0" { // 能力探测
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[:1])
			return
		}
		var start, end int
		fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
		atomic.AddInt32(&ranges, 1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start : end+1])
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)
	saveRestoreSeg(t, 4)
	saveRestoreChunkSize(t, chunk)

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
	if got := atomic.LoadInt32(&ranges); got != 5 {
		t.Fatalf("完整分片请求数=%d want 5（2.5MB / 512KiB）", got)
	}
	if _, err := os.Stat(part + ".meta"); !os.IsNotExist(err) {
		t.Fatal("完成后 .meta 应删除")
	}
}

// TestDownloadDirectChunkedResume 位图续传：已完成的片一个请求都不发。
func TestDownloadDirectChunkedResume(t *testing.T) {
	content := bytes.Repeat([]byte("xyz"), 512*1024) // 1.5MB
	const chunk = 256 << 10                          // 256KiB/片 → 6 片
	var fullRanges int32
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
	saveRestoreChunkSize(t, chunk)

	dir := t.TempDir()
	part := filepath.Join(dir, "big.mp4.part")
	total := int64(len(content))
	m := newChunkMeta(total, chunk)
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
	if got := atomic.LoadInt32(&fullRanges); got != 4 {
		t.Fatalf("完整分片请求数=%d want 4（6 片里前 2 片已完成）", got)
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

// TestChunkedBitmapSurvivesFailureAndResumes 分片下载中途失败：
//  1. 位图必须落盘并记下已完成的片（修复前位图只在创建时写过一次，全是 false，
//     于是"只下载未完成的分片"从未生效 —— 中断即全量重下）；
//  2. 恢复时只请求缺失片，已完成的片一个请求都不发。
func TestChunkedBitmapSurvivesFailureAndResumes(t *testing.T) {
	content := bytes.Repeat([]byte("m"), 5*(256<<10)) // 1.25MB（须 > minChunkedSize 才分片）
	const chunk = 256 << 10                           // 256KiB/片 → 5 片
	var (
		mu       sync.Mutex
		ranges         = map[int]int{} // 片下标 → 完整分片请求次数
		failLast int32 = 1
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[:1])
			return
		}
		var start, end int
		fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
		idx := start / chunk
		if idx == 4 && atomic.LoadInt32(&failLast) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		ranges[idx]++
		mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start : end+1])
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1) // 不重试，失败即返回
	saveRestoreSeg(t, 1)                // 串行
	saveRestoreChunkSize(t, chunk)

	dir := t.TempDir()
	part := filepath.Join(dir, "big.mp4.part")
	job := &dlJob{rt: testStd, m3u8URL: srv.URL}

	// ① 末片恒 500：整体失败，但位图必须留住已完成的前 4 片
	if err := job.downloadDirect(context.Background(), part); err == nil {
		t.Fatal("末片 500 应报错")
	}
	m, ok := loadChunkMeta(part)
	if !ok {
		t.Fatal("失败后位图必须存在（留给续传用）")
	}
	if len(m.Done) != 5 || !m.Done[0] || !m.Done[1] || !m.Done[2] || !m.Done[3] || m.Done[4] {
		t.Fatalf("位图应精确记录已完成的前 4 片: %v", m.Done)
	}
	if fi, err := os.Stat(part); err != nil || fi.Size() != 4*chunk {
		t.Fatalf("已落盘字节数=%v want %d", fi, 4*chunk)
	}

	// ② 源站恢复：只应请求缺失的末片
	atomic.StoreInt32(&failLast, 0)
	mu.Lock()
	ranges = map[int]int{}
	mu.Unlock()
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("续传: %v", err)
	}
	data, _ := os.ReadFile(part)
	if !bytes.Equal(data, content) {
		t.Fatalf("续传后内容不完整: len=%d want %d", len(data), len(content))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 1 || ranges[4] == 0 {
		t.Fatalf("续传应只请求缺失的末片，实际请求了 %v", ranges)
	}
	if _, err := os.Stat(part + ".meta"); !os.IsNotExist(err) {
		t.Fatal("全部完成后 .meta 应删除")
	}
}

// TestNewChunkPlanRejectsHugeTotal 片数超限时不规划分片（调用方退回单连接）：
// total 来自 Content-Range、不可信，别让一个离奇的值撑出巨大位图。
func TestNewChunkPlanRejectsHugeTotal(t *testing.T) {
	saveRestoreChunkSize(t, 1) // 1 字节/片 → 片数 = total
	job := &dlJob{rt: testStd}
	if _, ok := job.newChunkPlan(maxChunkCount); !ok {
		t.Fatal("恰好达到上限仍应规划分片")
	}
	if _, ok := job.newChunkPlan(maxChunkCount + 1); ok {
		t.Fatal("超过上限必须放弃分片")
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
	sw, err := newStreamWriter(path, 0, 0, func(int) {}, func(f int, _ int64) {
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

// TestStreamWriterFlushedLagsMemoryBreakpoint 内存断点（Next）与落盘断点（Flushed）
// 必须能区分开：缓冲里的字节还没进文件时 Next 已经前进，Flushed 不能动。
// 持久化续传起点只能用 Flushed —— 用 Next 就是"断点领先磁盘"（R9）。
// 旧版 Next 的注释写着"暂停时用它作为续传起点"，那正是缺陷的写法。
func TestStreamWriterFlushedLagsMemoryBreakpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lag.ts")
	sw, err := newStreamWriter(path, 0, 0, func(int) {}, func(int, int64) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 远小于 1MB 刷新阈值：数据留在 bufio 缓冲里，还没落盘
	for i := 0; i < 3; i++ {
		if err := sw.submit(i, bytes.Repeat([]byte{byte(i + 1)}, 100)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if got := sw.Next(); got != 3 {
		t.Fatalf("Next()=%d want 3（内存进度）", got)
	}
	if got := sw.Flushed(); got != 0 {
		t.Fatalf("Flushed()=%d want 0（缓冲还没落盘，断点不得前进）", got)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Fatalf("文件大小=%v err=%v want 0（数据还在缓冲里）", fi, err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if got := sw.Flushed(); got != 3 {
		t.Fatalf("Close 成功后 Flushed()=%d want 3", got)
	}
}

// TestFinishStreamKeepsDiskBreakpointOnCloseFailure 关闭输出流时最终 Flush 失败
// （磁盘满 / 句柄失效）：尾部若干 MB 并没有进文件，此时断点绝不能推进到内存
// 进度。上层会把它当续传起点持久化（pipeline 的 st.segDone → 下次的 from），
// 直播还用 segFlushedNow 推进去重水位线 —— 断点虚高就是"产物里留下永久空洞、
// 而状态与日志一切正常"。关闭失败还必须作为错误交回调用方，不能只打一行警告
// 就继续（"失败被降级成警告"是本项目点名的缺陷形态）。
func TestFinishStreamKeepsDiskBreakpointOnCloseFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fail.ts")
	j := &dlJob{rt: testStd}
	sw, err := newStreamWriter(path, 0, 0, func(int) {}, func(int, int64) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 三片小数据留在缓冲里（< 1MB 阈值），随后把底层文件直接关掉，
	// 让 Close 的最终 Flush 必然失败。
	for i := 0; i < 3; i++ {
		if err := sw.submit(i, bytes.Repeat([]byte{byte(i + 1)}, 100)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	memNext := sw.Next()
	if err := sw.f.Close(); err != nil {
		t.Fatal(err)
	}
	closeErr := sw.Close()
	if closeErr == nil {
		t.Fatal("底层文件已关闭，Close 必须报错（否则本用例根本没走到失败路径）")
	}

	got, err := finishStream(sw, j, 0, closeErr)
	if err == nil {
		t.Fatal("关闭失败必须交回调用方，不能吞掉")
	}
	if got != 0 || j.segFlushedNow() != 0 {
		t.Fatalf("续传起点=%d segFlushedNow=%d want 0（尾部未落盘，断点不得前进；内存进度 %d）",
			got, j.segFlushedNow(), memNext)
	}
	if j.segNow() != int64(memNext) {
		t.Fatalf("界面进度 segNow=%d want %d（仍报真实内存进度）", j.segNow(), memNext)
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
