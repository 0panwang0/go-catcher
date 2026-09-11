// 流式下载核心：dlJob 上下文、streamWriter 按序落盘、分片并发下载。
package core

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type dlJob struct {
	// rt 是任务所属运行时（创建任务时注入）：分片并发/重试参数、共享 HTTP
	// 客户端、直播轮询参数、伪造 UA 等都经它读取，不再依赖包级全局。
	rt      *Runtime
	id      string // 任务 ID（server 模式分配；CLI 模式为空）
	m3u8URL string
	referer string
	saveDir string // 目标保存目录
	fname   string // 目标文件名（含扩展名）
	limit   int    // 分片上限（0 = 全部）
	// 分片进度（每任务独立原子计数）。
	// segDone 是"已写进缓冲区"的序号（给界面看，反映真实进度）；
	// segFlushed 是"已确认落盘"的序号（给断点持久化用，见 swFlushBytes 注释）。
	// 两者分开：进度要即时，断点必须保守。
	segDone    int64
	segFlushed int64
	segTot     int64
	// 供 server 模式刷新任务状态；CLI 模式为空
	progress func(stage string, segDone, segTot int64)

	// live 直播跟随模式：segTot 恒为 0（列表无限增长，前端按录制时长展示）
	live bool
	// pre 容器探测时已拉取的首个分片（from==0 时直接复用，避免重复下载）
	pre []byte
	// seen 直播已下载分片 URL 集合（去重 + 断点恢复时跳过已录分片）
	seenMu sync.Mutex
	seen   map[string]bool

	// container 探测到的容器（决定分片写入前是否需要规范化）
	container *Container
	// norm 容器规范化状态（fMP4 的 tfdt 基准 + 每轨结束时间 + 轨道类型 +
	// init 段解析结果，由 container.NewState 创建，跨分片/跨批次/断点续传共用；
	// 无状态容器为 nil）
	norm NormState
	// decryptor 分片解密器（nil = 明文流），由 ensureDecryptor 按播放列表
	// 的 #EXT-X-KEY 装配；decKeyID 已装配解密器的 key 指纹（直播轮询幂等去重）
	decryptor SegDecryptor
	decKeyID  string
	// preURL 容器探测预取分片的 URL（streamDownload 复用 pre 前核对，
	// 防直播列表滚动后首片张冠李戴写错内容）
	preURL string
	// initLen 本次实际写入 .part 头的 init 段长度（0 = 无 init 段或续传）。
	// 落盘校验用它兜住"只落了 init 段、分片一个没写"的假成功。
	initLen int
}

// seenHas / seenAdd / seenSnapshot 直播分片去重与持久化窗口。
func (j *dlJob) seenHas(u string) bool {
	j.seenMu.Lock()
	defer j.seenMu.Unlock()
	return j.seen[u]
}

// backfill 任务完成/暂停收尾：委托规范化状态的 Finish（无状态容器为 nil，静默跳过）。
func (j *dlJob) backfill(path string) error {
	if j.norm == nil {
		return nil
	}
	return j.norm.Finish(path)
}

func (j *dlJob) seenAdd(u string) {
	j.seenMu.Lock()
	defer j.seenMu.Unlock()
	j.seen[u] = true
}

// seenSnapshot 返回已见分片 URL（稳定排序，用于持久化断点恢复）。
func (j *dlJob) seenSnapshot() []string {
	j.seenMu.Lock()
	defer j.seenMu.Unlock()
	out := make([]string, 0, len(j.seen))
	for u := range j.seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
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

// segFlushedNow 返回"已确认落盘"的断点。持久化必须用它而不是 segNow()：
// segNow() 可能领先磁盘若干 MB，拿它当断点续传会在文件中间留下空洞。
func (j *dlJob) segFlushedNow() int64 { return atomic.LoadInt64(&j.segFlushed) }

// setSegFlushed 推进已落盘断点（streamWriter 每次 flush 后回调）。
func (j *dlJob) setSegFlushed(n int64) {
	atomic.StoreInt64(&j.segFlushed, n)
}

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
// 内存有界：同时在飞的分片数 = concurrencyNow()，乱序到达的暂存在 buf。
// ============================================================

// streamWriter 按严格顺序把到达的分片 append 到目标文件。

// swFlushBytes 是 streamWriter 的主动刷新阈值：缓冲区攒够这么多字节就 flush 一次。
//
// 为什么不能只靠 bufio 自动刷（R9）：进度计数会被当成断点持久化，而它记录的
// 是"已写进缓冲区"而非"已落盘"。Engine.Stop 在 waitTasksSettled(3s) 超时后
// 直接落盘，此时计数可能领先磁盘最多一个缓冲区（8MB）；重启续传从该断点往后
// append，中间这段就永久缺失 —— 文件错位甚至直接损坏。
// 因此把"可持久化断点"（flushed）与"界面进度"（next）分开，并且 flushed
// 只在 flush 成功后才推进。
const swFlushBytes = 1 << 20 // 1MB

type streamWriter struct {
	mu      sync.Mutex
	next    int            // 下一个待写入缓冲区的分片序号（= 内存断点）
	flushed int            // 已确认落盘的断点：所有 < flushed 的分片字节都已 flush
	buf     map[int][]byte // 乱序到达、暂时还不能写的数据
	w       *bufio.Writer
	f       *os.File
	onWrite func(next int)      // 每次推进后回调（刷新界面进度）
	onFlush func(flushed int)   // 每次落盘后回调（推进可持久化断点）
	norm    func([]byte) []byte // 写入前的规范化（nil = 原样写）
}

// newStreamWriter 以追加模式打开 path（不存在则创建），从 startIdx 开始写。
// 追加模式是断点续传的关键：恢复时直接从上次断点继续往后写。

func newStreamWriter(path string, startIdx int, onWrite, onFlush func(int), norm func([]byte) []byte) (*streamWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &streamWriter{
		next:    startIdx,
		flushed: startIdx,
		buf:     make(map[int][]byte),
		w:       bufio.NewWriterSize(f, 8<<20),
		f:       f,
		onWrite: onWrite,
		onFlush: onFlush,
		norm:    norm,
	}, nil
}

// submit 交出一个分片的数据：先缓存，再把从 next 起的连续数据一次性写出。
// 规范化在锁内按写入顺序执行（fMP4 的 tfdt 基准必须由首个写入的分片确立）。

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
		if sw.norm != nil {
			d = sw.norm(d)
		}
		if _, err := sw.w.Write(d); err != nil {
			return err
		}
		sw.next++
	}
	// 攒够阈值就落盘，并把"可持久化断点"推进到 flush 之后的位置。
	// 界面进度始终报 next（真实进度），断点只报 flushed（保守值）。
	sw.flushLocked()
	if sw.onWrite != nil {
		sw.onWrite(sw.next)
	}
	return nil
}

// flushLocked 在缓冲达到阈值时落盘并推进 flushed（调用方须持 mu）。
func (sw *streamWriter) flushLocked() {
	if sw.w.Buffered() < swFlushBytes {
		return
	}
	if err := sw.w.Flush(); err != nil {
		return // 写入错误由下一次 Write 暴露，这里不吞掉断点推进即可
	}
	if sw.flushed != sw.next {
		sw.flushed = sw.next
		if sw.onFlush != nil {
			sw.onFlush(sw.flushed)
		}
	}
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
	err := sw.w.Flush()
	if err == nil {
		// 尾部数据已落盘，断点可以安全推进到 next
		sw.flushed = sw.next
		if sw.onFlush != nil {
			sw.onFlush(sw.flushed)
		}
		err = sw.f.Close()
		return err
	}
	sw.f.Close()
	return err
}

// downloadDirect 直链文件（MP4 等）整体下载：流式把响应体写入 outPath。
// 三种模式自动选择：
//  1. 分片续传：.part.meta 位图存在 → 只下载未完成的分片
//  2. 分片下载：全新文件且服务器支持 Range（Content-Range 给出总大小）→ 并发分片
//  3. 单连接续传：其余情况 → Range 追加（206）/ 全量覆盖（200）/ 丢弃重下（416）
//
// 分片模式下 ctx 取消或失败保留 .part 与 .meta，任务恢复后从位图断点继续。
func (j *dlJob) downloadDirect(ctx context.Context, outPath string) error {
	// 模式 1：分片续传（位图恢复）
	if m, ok := loadChunkMeta(outPath); ok {
		return j.downloadChunked(ctx, outPath, m)
	}

	// 模式 2：全新下载且服务器支持 Range → 分片下载（小文件不值得分片）
	if partFileOffset(outPath) == 0 {
		if total, ok := j.probeRange(ctx); ok && total > minChunkedSize {
			m := newChunkMeta(total, j.rt.concurrencyNow())
			if err := saveChunkMeta(outPath, m); err != nil {
				return err
			}
			return j.downloadChunked(ctx, outPath, m)
		}
	}

	// 模式 3：单连接续传
	offset := partFileOffset(outPath)
	for attempt := 0; ; attempt++ {
		req, err := j.rt.newRequest(j.m3u8URL, j.referer)
		if err != nil {
			return cleanURLParseErr(err, j.m3u8URL)
		}
		req = req.WithContext(ctx)
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		resp, err := j.rt.getClient().Do(req)
		if err != nil {
			return err
		}
		switch resp.StatusCode {
		case http.StatusPartialContent, http.StatusOK:
			if resp.StatusCode == http.StatusOK {
				offset = 0 // 200 = 不支持 Range，全量覆盖
			}
			cpErr, closeErr := streamToFile(resp.Body, outPath, offset)
			resp.Body.Close()
			if cpErr != nil {
				return cpErr // 保留 .part，任务恢复后续传
			}
			return closeErr
		case http.StatusRequestedRangeNotSatisfiable:
			resp.Body.Close()
			if offset > 0 && attempt == 0 {
				os.Remove(outPath)
				offset = 0
				continue
			}
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		default:
			resp.Body.Close()
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
}

// minChunkedSize 低于该大小的直链不分片（并发开销不划算）。
const minChunkedSize = 1 << 20

// chunkMeta 直链分片下载的断点位图，JSON 持久化在 <part>.meta。
// Done[i] 标记第 i 片是否已完整落盘；恢复时只重下未完成片。
type chunkMeta struct {
	Total int64  `json:"total"`
	Size  int64  `json:"size"`
	Done  []bool `json:"done"`
}

func chunkMetaPath(partPath string) string { return partPath + ".meta" }

func loadChunkMeta(partPath string) (*chunkMeta, bool) {
	data, err := os.ReadFile(chunkMetaPath(partPath))
	if err != nil {
		return nil, false
	}
	var m chunkMeta
	if json.Unmarshal(data, &m) != nil || m.Total <= 0 || m.Size <= 0 || len(m.Done) == 0 {
		return nil, false
	}
	return &m, true
}

func saveChunkMeta(partPath string, m *chunkMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(chunkMetaPath(partPath), data, 0644)
}

// newChunkMeta 按并发数均分文件：每片大小 = ceil(total/workers)。
func newChunkMeta(total int64, workers int) *chunkMeta {
	if workers < 1 {
		workers = 1
	}
	size := (total + int64(workers) - 1) / int64(workers)
	n := int((total + size - 1) / size)
	return &chunkMeta{Total: total, Size: size, Done: make([]bool, n)}
}

// probeRange 探测服务器是否支持 Range 并取得文件总大小。
// 发 Range: bytes=0-0，仅当 206 且 Content-Range 给出明确总大小时确认可用。
func (j *dlJob) probeRange(ctx context.Context) (int64, bool) {
	req, err := j.rt.newRequest(j.m3u8URL, j.referer)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := j.rt.getClient().Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, false
	}
	cr := resp.Header.Get("Content-Range")
	var total int64
	if !strings.HasPrefix(cr, "bytes ") || !strings.Contains(cr, "/") {
		return 0, false
	}
	if _, err := fmt.Sscanf(cr, "bytes 0-0/%d", &total); err != nil || total <= 0 {
		return 0, false
	}
	return total, true
}

// errChunkStale 分片请求 416：服务器内容已变，分片位图全部失效，需丢弃重下。
var errChunkStale = errors.New("服务器内容已变化（HTTP 416）")

// downloadChunked 并发下载未完成分片：每片独立 Range 请求，WriteAt 按偏移落盘。
// 失败的分片在片内重试（maxRetriesNow() 次）；整体出错时保留位图供下次续传。
// 全部完成后删除 .meta（分片状态失效）。
func (j *dlJob) downloadChunked(ctx context.Context, outPath string, m *chunkMeta) error {
	f, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	total := m.Total
	var mu sync.Mutex
	stale := false
	done := 0
	for _, d := range m.Done {
		if d {
			done++
		}
	}
	if j.progress != nil {
		j.progress("下载直链文件中", int64(done), int64(len(m.Done)))
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, j.rt.concurrencyNow())
	for i, d := range m.Done {
		if d {
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := int64(idx) * m.Size
			end := start + m.Size - 1
			if end >= total {
				end = total - 1
			}
			if err := j.downloadRange(ctx, f, start, end); err != nil {
				mu.Lock()
				if errors.Is(err, errChunkStale) {
					stale = true
				}
				fmt.Printf("[direct] 分片 %d-%d 失败: %v\n", start, end, err)
				mu.Unlock()
				return
			}
			mu.Lock()
			m.Done[idx] = true
			done++
			if j.progress != nil {
				j.progress("下载直链文件中", int64(done), int64(len(m.Done)))
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if stale {
		// 服务器内容已变：关闭句柄后丢弃 part/meta，下次任务全量重下
		f.Close()
		os.Remove(outPath)
		os.Remove(chunkMetaPath(outPath))
		return errChunkStale
	}
	for _, d := range m.Done {
		if !d {
			// 有分片失败：保留 .part/.meta，任务恢复后续传
			return fmt.Errorf("分片下载未完成")
		}
	}
	os.Remove(chunkMetaPath(outPath))
	return nil
}

// downloadRange 单个分片下载：Range 请求 + WriteAt 写偏移，片内重试。
// 服务器 416（内容已变）返回 errChunkStale，由调用方统一清理分片状态。
func (j *dlJob) downloadRange(ctx context.Context, f *os.File, start, end int64) error {
	expected := end - start + 1
	attempt := 1
	for {
		req, err := j.rt.newRequest(j.m3u8URL, j.referer)
		if err != nil {
			return err
		}
		req = req.WithContext(ctx)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := j.rt.getClient().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			if attempt >= j.rt.maxRetriesNow() {
				return err
			}
			attempt++
			time.Sleep(time.Duration(attempt-1) * 300 * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			resp.Body.Close()
			return errChunkStale
		}
		if resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			if attempt >= j.rt.maxRetriesNow() {
				return fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			attempt++
			time.Sleep(time.Duration(attempt-1) * 300 * time.Millisecond)
			continue
		}
		n, cpErr := copyWithIdleTimeout(&offsetWriter{f: f, off: start}, resp.Body, transferIdleTimeout)
		resp.Body.Close()
		if cpErr != nil {
			if ctx.Err() != nil {
				return cpErr
			}
			if attempt >= j.rt.maxRetriesNow() {
				return cpErr
			}
			attempt++
			time.Sleep(time.Duration(attempt-1) * 300 * time.Millisecond)
			continue
		}
		if n != expected {
			if attempt >= j.rt.maxRetriesNow() {
				return fmt.Errorf("分片 %d-%d 收到 %d 字节, 期望 %d", start, end, n, expected)
			}
			attempt++
			time.Sleep(time.Duration(attempt-1) * 300 * time.Millisecond)
			continue
		}
		return nil
	}
}

// offsetWriter 把 Write 转发为固定偏移的 WriteAt（并发分片各自写自己的区间）。
type offsetWriter struct {
	f   *os.File
	off int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
}

// partFileOffset 返回 .part 已写字节数（续传起点）。
func partFileOffset(outPath string) int64 {
	if fi, err := os.Stat(outPath); err == nil && fi.Size() > 0 {
		return fi.Size()
	}
	return 0
}

// streamToFile 把 body 写入 outPath。offset=0 时截断覆盖（全量下载），
// offset>0 时追加（续传）。返回写错误与关闭错误。
// 读取带空闲超时：直链整体下载不能设固定总时长（大文件必然超），
// 但连接卡死必须能中断。
func streamToFile(body io.ReadCloser, outPath string, offset int64) (cpErr, closeErr error) {
	var f *os.File
	var err error
	if offset > 0 {
		// 续传：不截断，Seek 到末尾追加（Windows 上 O_APPEND 仅授予追加权限，Truncate 会被拒）
		f, err = os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			return err, nil
		}
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			f.Close()
			return err, nil
		}
	} else {
		f, err = os.Create(outPath)
		if err != nil {
			return err, nil
		}
	}
	_, cpErr = copyWithIdleTimeout(f, body, transferIdleTimeout)
	closeErr = f.Close()
	return
}

// fetchSegment 带重试地把单个分片下载到内存。ctx 取消时立刻返回。

func fetchSegment(ctx context.Context, j *dlJob, segURL string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= j.rt.maxRetriesNow(); attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := j.rt.newRequest(segURL, j.referer)
		if err != nil {
			return nil, cleanURLParseErr(err, segURL)
		}
		req = req.WithContext(ctx)
		resp, err := j.rt.getClient().Do(req)
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
		data, err := readAllWithIdleTimeout(resp.Body, transferIdleTimeout)
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

// streamDownload 并发下载 segURLs（一个连续批次）并按序写入 outPath。
// startIdx 是 segURLs[0] 对应的写入序号（点播=断点 from；直播=当前已写分片数）。
// seqBase 是 segURLs[0] 的 media sequence（播放列表 MEDIA-SEQUENCE + 其在列表中的
// 位置；密钥无显式 IV 时按它派生 IV，明文流无意义传 0）。
// 返回批次结束后的下一个写入序号（= startIdx+len(segURLs)）与错误。
// 暂停/取消时返回已实际写入的序号，便于上层保存进度后从断点恢复。
// 首个分片若已被容器探测预取（j.pre 非空且 URL 吻合，startIdx==0 时）直接复用。
func streamDownload(ctx context.Context, j *dlJob, segURLs []string, startIdx int, seqBase uint64, outPath string) (int, error) {
	total := len(segURLs)
	// 直播列表无限增长：进度只报已写入数，segTot 恒为 0（前端按录制时长展示）。
	// 点播 segTot 必须用绝对总数（断点 startIdx + 本批 total）：segDone 是跨批次
	// 累计的绝对写入数，若只报本批长度，续传时分片数会显示成「1771 / 654」。
	dispTot := int64(startIdx + total)
	if j.live {
		dispTot = 0
	}
	j.setSeg(int64(startIdx), dispTot)
	j.setSegFlushed(int64(startIdx))

	sw, err := newStreamWriter(outPath, startIdx, func(next int) {
		j.setSeg(int64(next), dispTot)
		if j.live {
			fmt.Printf("\r  已录制分片: %d   ", next)
		} else {
			fmt.Printf("\r  下载进度: %d / %d   ", next, startIdx+total)
		}
	}, func(flushed int) {
		// 断点只在真实落盘后推进（R9）：Engine.Stop 可能在 pipeline 收尾前
		// 就落盘状态；若断点领先磁盘字节，续传会在文件中间留下空洞。
		j.setSegFlushed(int64(flushed))
	}, func(d []byte) []byte {
		// 写入前规范化：委托容器状态对象（fMP4 时间戳归一化 + NAL 封装转换 +
		// 内联 init 消费）；无状态容器原样写，规范化失败降级原样写
		if j.norm == nil {
			return d
		}
		nd, err := j.norm.Normalize(d)
		if err != nil {
			fmt.Printf("[norm] 分片规范化失败（原样写入）: %v\n", err)
			return d
		}
		if len(nd) != len(d) {
			fmt.Printf("[norm] 分片规范化: %d → %d 字节\n", len(d), len(nd))
		}
		return nd
	})
	if err != nil {
		return startIdx, err
	}
	closed := false
	closeSW := func() {
		if closed {
			return
		}
		closed = true
		if cerr := sw.Close(); cerr != nil {
			fmt.Printf("\n  [!] 关闭输出文件失败: %v\n", cerr)
		}
	}
	defer closeSW()

	cc := j.rt.concurrencyNow()
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
	for i := 0; i < total; i++ {
		idx := startIdx + i
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
		go func(idx int, u string) {
			defer wg.Done()
			defer func() { <-flySem }()
			defer func() {
				if r := recover(); r != nil {
					setErr(fmt.Errorf("分片 %d panic: %v", idx, r))
				}
			}()

			var data []byte
			var err error // 闭包局部错误：并发写函数级 err 会数据竞争
			if startIdx == 0 && i == 0 && j.pre != nil && j.preURL == u {
				data = j.pre // 容器探测时已下载（含解密），直接复用
			} else {
				data, err = fetchSegment(ctx, j, u)
				if err != nil {
					if ctx.Err() != nil {
						setErr(ctx.Err())
					} else {
						setErr(fmt.Errorf("分片 %d 下载失败: %w", idx, err))
					}
					return
				}
				if data, err = j.decryptSegmentIfAny(seqBase+uint64(i), data); err != nil {
					setErr(fmt.Errorf("分片 %d 解密失败: %w", idx, err))
					return
				}
			}
			if err := sw.submit(idx, data); err != nil {
				setErr(fmt.Errorf("分片 %d 写入失败: %w", idx, err))
			}
		}(idx, segURLs[i])
	}

	wg.Wait()
	fmt.Println()

	// 先冲刷关闭、再取断点：返回的 next 会被上层当成续传起点持久化，
	// 必须保证它对应的字节已经真正落在 .part 里（R9）。
	closeSW()
	next := sw.Next()
	j.setSeg(int64(next), dispTot)
	j.setSegFlushed(int64(next))

	errMu.Lock()
	e := firstErr
	errMu.Unlock()
	if e != nil {
		return next, e
	}
	return next, nil
}

// ============================================================
// 直播跟随模式：循环拉取播放列表，增量下载新分片
// ------------------------------------------------------------
// 直播/事件流的播放列表没有 #EXT-X-ENDLIST，且列表不断增长（旧分片会被
// 滚动淘汰）。跟随模式每 livePollInterval 轮询一次播放列表，用 seen 集合
// 去重，只下载新增分片并 append 到 .part。用户暂停/取消即停止；
// 播放列表出现 ENDLIST（直播自然结束）或长时间无新分片（死流兜底）也停止。
//
// 返回 (已写分片数, 错误)：暂停/取消时返回当前断点与 nil，由上层按意图收尾。
// ============================================================

// livePollInterval / liveMaxEmptyPolls 为 Runtime 字段（约 75s/25 次兜底，
// 防死流/直播结束但无 ENDLIST 时挂死；测试可调短）。

func (j *dlJob) liveDownload(ctx context.Context, outPath string, from int) (int, error) {
	next := from
	empty := 0
	for {
		if err := ctx.Err(); err != nil {
			return next, nil // 暂停/取消：把断点交还上层
		}
		content, base, isDirect, err := j.fetchPlaylist()
		if err != nil {
			return next, err
		}
		if isDirect {
			return next, fmt.Errorf("直播播放列表响应变为直链媒体，无法继续跟随录制")
		}
		cur := parsePlaylist(content, base)
		// 同一轮窗口内换 key（key rotation）无法安全解密：显式失败。
		// 跨轮换 key 是支持的 —— 每轮按本轮声明的 key 解密本轮分片。
		if kerr := ensureSingleKey(cur); kerr != nil {
			return next, kerr
		}
		// 每轮轮询重新装配解密器（幂等：key 未变不重拉）；key 轮换时按新 key 解密后续分片
		if kerr := j.ensureDecryptor(ctx, cur.key); kerr != nil {
			return next, kerr
		}

		var newSegs []string
		firstNewPos := -1 // 本批首个新分片在当前列表中的位置（派生 IV 的序号基准）
		for pos, u := range cur.segments {
			if !j.seenHas(u) {
				j.seenAdd(u)
				newSegs = append(newSegs, u)
				if firstNewPos < 0 {
					firstNewPos = pos
				}
			}
		}

		if len(newSegs) > 0 {
			empty = 0
			n, derr := streamDownload(ctx, j, newSegs, next, cur.mediaSeq+uint64(firstNewPos), outPath)
			next = n
			if derr != nil {
				if ctx.Err() != nil {
					return next, nil
				}
				return next, derr
			}
			if cur.hasEndList {
				fmt.Println("[live] 播放列表出现 ENDLIST，直播录制自然结束")
				return next, nil
			}
		} else {
			if cur.hasEndList {
				fmt.Println("[live] 播放列表出现 ENDLIST，直播录制自然结束")
				return next, nil
			}
			empty++
			if empty >= j.rt.liveMaxEmptyPolls {
				fmt.Printf("[live] 连续 %d 次轮询无新分片，判定直播结束\n", empty)
				return next, nil
			}
		}

		select {
		case <-ctx.Done():
			return next, nil
		case <-time.After(j.rt.livePollInterval):
		}
	}
}

// writeInitSegment 把 fMP4 init 段写入 .part 文件头。
// 仅在文件为空（或不存在）时写：断点续传时文件已有 init，绝不能重复写。
func writeInitSegment(path string, data []byte) error {
	fi, err := os.Stat(path)
	if err == nil && fi.Size() > 0 {
		return nil // 已有内容（续传残留），init 已在头部
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
