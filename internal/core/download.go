// 流式下载核心：dlJob 上下文、streamWriter 按序落盘、分片并发下载。
package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type dlJob struct {
	id      string // 任务 ID（server 模式分配；CLI 模式为空）
	m3u8URL string
	referer string
	saveDir string // 目标保存目录
	fname   string // 目标文件名（含扩展名）
	limit   int    // 分片上限（0 = 全部）
	// 分片进度（每任务独立原子计数）
	segDone int64
	segTot  int64
	// 供 server 模式刷新任务状态；CLI 模式为空
	progress func(stage string, segDone, segTot int64)
}

// setSeg 更新本任务分片进度计数，并回调 progress(若设了)

func (j *dlJob) setSeg(done, tot int64) {
	atomic.StoreInt64(&j.segDone, done)
	atomic.StoreInt64(&j.segTot, tot)
	if j.progress != nil {
		j.progress("下载分片中", done, tot)
	}
}

func (j *dlJob) segNow() int64 { return atomic.LoadInt64(&j.segDone) }

func (j *dlJob) segTotal() int64 { return atomic.LoadInt64(&j.segTot) }

// ============================================================
// 流式下载：边下边写（IDM 同款机制）
// ------------------------------------------------------------
// 旧实现是「先把全部分片下载到临时目录 → 再统一合并」。在 SMB 网络盘上，
// 几千个小文件逐个 open/read/close，合并阶段要跑好几分钟。
//
// 新实现：按序派发分片 → 并发下载到内存 → 由 streamWriter 按严格顺序
// append 到目标 .part 文件。最后一个分片写完的瞬间文件就是成品，
// 完全没有独立的合并阶段。
//
// 额外收益：next 之前的数据都已落盘，next 天然就是断点，
// 暂停 / 崩溃 / 重启后都能从 next 继续 append（断点续传）。
// 内存有界：同时在飞的分片数 = concurrency，乱序到达的暂存在 buf。
// ============================================================

// streamWriter 按严格顺序把到达的分片 append 到目标文件。

type streamWriter struct {
	mu      sync.Mutex
	next    int            // 下一个待写入的分片序号（= 断点）
	buf     map[int][]byte // 乱序到达、暂时还不能写的数据
	w       *bufio.Writer
	f       *os.File
	onWrite func(next int) // 每次推进后回调（用来刷新进度）
}

// newStreamWriter 以追加模式打开 path（不存在则创建），从 startIdx 开始写。
// 追加模式是断点续传的关键：恢复时直接从上次断点继续往后写。

func newStreamWriter(path string, startIdx int, onWrite func(int)) (*streamWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &streamWriter{
		next:    startIdx,
		buf:     make(map[int][]byte),
		w:       bufio.NewWriterSize(f, 8<<20),
		f:       f,
		onWrite: onWrite,
	}, nil
}

// submit 交出一个分片的数据：先缓存，再把从 next 起的连续数据一次性写出。

func (sw *streamWriter) submit(idx int, data []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if idx < sw.next {
		return nil // 已写过的序号，丢弃
	}
	sw.buf[idx] = data
	for {
		d, ok := sw.buf[sw.next]
		if !ok {
			break
		}
		delete(sw.buf, sw.next)
		if _, err := sw.w.Write(d); err != nil {
			return err
		}
		sw.next++
	}
	if sw.onWrite != nil {
		sw.onWrite(sw.next)
	}
	return nil
}

// Next 返回当前断点（下一个待写序号），暂停时用它作为续传起点。

func (sw *streamWriter) Next() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.next
}

// Close 冲刷缓冲并关闭文件。保存断点前必须先调用，否则尾部数据会丢。

func (sw *streamWriter) Close() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if err := sw.w.Flush(); err != nil {
		sw.f.Close()
		return err
	}
	return sw.f.Close()
}

// fetchSegment 带重试地把单个分片下载到内存。ctx 取消时立刻返回。

func fetchSegment(ctx context.Context, j *dlJob, segURL string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := newRequest(segURL, j.referer)
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		resp, err := sharedClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: %w", attempt, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d: HTTP %d", attempt, resp.StatusCode)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: read body: %w", attempt, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

// streamDownload 并发下载 segURLs[from:] 并按序写入 outPath。
// 返回已写入的分片数（即续传断点）与错误。暂停/取消时照样返回断点，
// 便于上层保存进度后从断点恢复。

func streamDownload(ctx context.Context, j *dlJob, segURLs []string, from int, outPath string) (int, error) {
	initSharedClient()
	total := len(segURLs)
	j.setSeg(int64(from), int64(total))

	sw, err := newStreamWriter(outPath, from, func(next int) {
		j.setSeg(int64(next), int64(total))
		fmt.Printf("\r  下载进度: %d / %d   ", next, total)
	})
	if err != nil {
		return from, err
	}
	defer func() {
		if cerr := sw.Close(); cerr != nil {
			fmt.Printf("\n  [!] 关闭输出文件失败: %v\n", cerr)
		}
	}()

	cc := concurrency
	if cc < 1 {
		cc = 1
	}

	var (
		wg       sync.WaitGroup
		flySem   = make(chan struct{}, cc) // 同时在飞的分片数（= 内存占用上限）
		errMu    sync.Mutex
		firstErr error
		stopped  = make(chan struct{}) // 首个错误/中断后停止派发新分片
	)

	setErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			close(stopped)
		}
		errMu.Unlock()
	}

dispatch:
	for i := from; i < total; i++ {
		// 出错或收到中断信号就不再派发，但已在飞的 goroutine 会自然结束
		select {
		case <-ctx.Done():
			setErr(ctx.Err())
			break dispatch
		case <-stopped:
			break dispatch
		default:
		}
		select {
		case <-ctx.Done():
			setErr(ctx.Err())
			break dispatch
		case <-stopped:
			break dispatch
		case flySem <- struct{}{}:
		}

		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-flySem }()
			defer func() {
				if r := recover(); r != nil {
					setErr(fmt.Errorf("分片 %d panic: %v", idx, r))
				}
			}()

			data, err := fetchSegment(ctx, j, segURLs[idx])
			if err != nil {
				if ctx.Err() != nil {
					setErr(ctx.Err())
				} else {
					setErr(fmt.Errorf("分片 %d 下载失败: %w", idx, err))
				}
				return
			}
			if err := sw.submit(idx, data); err != nil {
				setErr(fmt.Errorf("分片 %d 写入失败: %w", idx, err))
			}
		}(i)
	}

	wg.Wait()
	fmt.Println()

	next := sw.Next()
	j.setSeg(int64(next), int64(total))

	errMu.Lock()
	e := firstErr
	errMu.Unlock()
	if e != nil {
		return next, e
	}
	return next, nil
}
