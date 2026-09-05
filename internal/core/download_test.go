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
	job := &dlJob{m3u8URL: srv.URL}
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

	job := &dlJob{m3u8URL: srv.URL}
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

	job := &dlJob{m3u8URL: srv.URL}
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

	job := &dlJob{m3u8URL: srv.URL}
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

	job := &dlJob{m3u8URL: srv.URL}
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
	old := concurrency
	t.Cleanup(func() { concurrency = old })
	concurrency = n
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
	job := &dlJob{m3u8URL: srv.URL}
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

	job := &dlJob{m3u8URL: srv.URL}
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
	job := &dlJob{m3u8URL: srv.URL}
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
	job := &dlJob{m3u8URL: srv.URL}
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
