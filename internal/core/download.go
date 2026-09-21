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
	"net/url"
	"os"
	"strconv"
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
	// segRefs 分片主机 → 浏览器实际发出的 Referer（host 含端口，键归一小写）。
	// 分片 CDN 防盗链可能只认解析站域名而非页面域名，抓分片时按 segURL 的 host
	// 精确匹配选用；空串是有效值（浏览器对该 host 没带 Referer，此时不设该头），
	// 只有未命中才回退到 referer。CLI / 直链任务恒为空。
	segRefs map[string]string
	saveDir string // 目标保存目录
	fname   string // 目标文件名（含扩展名）
	limit   int    // 分片上限（0 = 全部）
	// 分片进度（每任务独立原子计数）。
	// segDone 是"已写进缓冲区"的序号（给界面看，反映真实进度）；
	// flushed 是"已确认落盘"的断点（给断点持久化与恢复对齐用，见 swFlushBytes）。
	// 两者分开：进度要即时，断点必须保守。
	segDone int64
	// flushed 把"已落盘分片序号"与"已落盘绝对字节数"打包成一个不可变值，
	// 用 atomic.Pointer 一次性发布。
	//
	// 为什么必须是**一对**而不是两个独立原子值：HLS 的分片长度由源站决定、
	// 每片不同，序号推不出字节、字节也推不出序号，而恢复时两个都要用
	// （序号当续传起点、字节数当 .part 的对齐目标）。分开存会让读端拿到
	// 「新序号 + 旧字节数」这类错配组合：截断少了是重复写入（P0-4），
	// 截断多了是把用户已下的内容删掉。见 flushedMark。
	flushed atomic.Pointer[flushedMark]
	segTot  int64
	// 直链字节进度（直链任务专用；分片任务这两个字段恒 0，两者互斥）。
	// 直链没有"分片总数"这个分母，它的百分比只能按字节算 —— 见 setBytes。
	bytesDone int64
	bytesTot  int64
	// 供 server 模式刷新任务状态；CLI 模式为空
	progress func(stage string, segDone, segTot int64)

	// live 直播跟随模式：segTot 恒为 0（列表无限增长，前端按录制时长展示）
	live bool
	// encrypted 播放列表声明了 #EXT-X-KEY（METHOD 非 NONE）。
	// 落盘抽样校验要用它判断"密文长度"还是"明文长度"合理（见 validateOutput），
	// 而收尾可能发生在拿不到 playlistInfo 的地方（停止/中断路径），故记在这里。
	encrypted bool
	// gapMillis 产物时间轴上的缺口累计（毫秒）。直播只有一次机会：分片一旦从
	// 滑动窗口滚走就再也补不回来，产物里那段时间轴就是空的（fMP4 的 tfdt 前跳、
	// TS 的 PTS 前跳）。这里如实累加，收尾时写进任务状态供界面提示——不报缺口
	// 等于把"产物不完整"藏起来，正是本项目反复出现的头号缺陷形态。
	gapMillis int64
	// pre 容器探测时已拉取的首个分片（from==0 时直接复用，避免重复下载）
	pre []byte
	// 直播去重状态：media sequence 水位线（已录到的最大序号）。
	// 判定见 seenCovers；判定主依据就是它，没有别的判据。
	seenMu  sync.Mutex
	seenSeq uint64
	seenAny bool

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
	// partMode 直链 .part 的写入模式（partModeChunked / partModeStream），
	// 空 = 未知（升级前的旧任务）。它决定"文件大小能不能当续传起点"：
	// 分片模式用 WriteAt 稀疏写，文件中间可能有洞 ⇒ 大小 ≠ 有效字节数；
	// 单连接模式是连续前缀 ⇒ 可以。两种模式在磁盘上无法区分，必须记下来。
	partMode atomic.Value // string

	// restartMu / restartNote 恢复路径上的一次性提示（如"位图丢失，已从头下载"）。
	// 由 pipeline 在收尾前转写到任务状态 —— GUI 是 windowsgui 子系统、没有控制台，
	// 只打日志等于没提示，而"白下了一遍"必须让用户知道。
	restartMu   sync.Mutex
	restartNote string
}

// seenCovers 报告序号为 seq 的分片是否已录制（按 media sequence 水位线判定）。
//
// 为什么不用"URL 是否见过"：EVENT 列表只增不减，旧实现那个有界 URL 窗口会把
// 早期分片淘汰掉，于是它们被当成新分片重录一遍；不设界又解决不了内存与写盘
// 膨胀（旧实现只增不减，6 小时直播能攒下上万条 URL 常驻内存，且每次
// markDirty 都把它整个序列化进状态文件）。水位线对滑动窗口列表与 EVENT 列表
// 都正确，且是 O(1)。
//
// 跨会话不恢复：B0 之后直播只有「停止」「取消」两态，不存在"接着上次录"。
func (j *dlJob) seenCovers(seq uint64) bool {
	j.seenMu.Lock()
	defer j.seenMu.Unlock()
	return j.seenAny && seq <= j.seenSeq
}

// seenRecord 记录一个已录制分片：推进 media sequence 水位线（只前进，不回退）。
func (j *dlJob) seenRecord(seq uint64) {
	j.seenMu.Lock()
	defer j.seenMu.Unlock()
	if !j.seenAny || seq > j.seenSeq {
		j.seenSeq = seq
	}
	j.seenAny = true
}

// commitLiveWaterline 按"已确认落盘"的前缀推进直播去重水位线（P1-1）。
//
// 记账必须晚于落盘：旧实现是收集阶段就 seenRecord(seq)，也就是**下载之前**
// 就把分片记成"已录制"，下载失败也不会撤销——后续轮询据此把它们跳过，
// 产物时间轴上留一段空洞，而状态、日志都说一切正常。
//
// 用已落盘前缀（segFlushedNow）而不是 streamDownload 返回的 next：正常完成
// 两者相等，但"落盘断点"才是唯一有语义的判据（它就是给断点续传用的那个计数）。
func commitLiveWaterline(j *dlJob, seqs []uint64, startIdx int) {
	n := int(j.segFlushedNow()) - startIdx
	if n > len(seqs) {
		n = len(seqs)
	}
	if n < 0 {
		n = 0
	}
	for i := 0; i < n; i++ {
		j.seenRecord(seqs[i])
	}
}

// seenWatermark 返回 media sequence 水位线（已录到的最大序号，以及是否已建立）。
// 直播用它检测"窗口滚动把分片淘汰掉了"：本轮列表首片的序号若是水位线之后
// 一段距离，中间那些分片已经永远补不回来，产物时间轴上会留一个空洞。
func (j *dlJob) seenWatermark() (uint64, bool) {
	j.seenMu.Lock()
	defer j.seenMu.Unlock()
	return j.seenSeq, j.seenAny
}

// addGapSeconds 累加产物时间轴上的缺口时长（秒）。
func (j *dlJob) addGapSeconds(sec float64) {
	if sec <= 0 {
		return
	}
	atomic.AddInt64(&j.gapMillis, int64(sec*1000))
}

// gapSecondsNow 返回累计缺口时长（秒）。
func (j *dlJob) gapSecondsNow() float64 {
	return float64(atomic.LoadInt64(&j.gapMillis)) / 1000
}

// backfill 任务完成/暂停收尾：委托规范化状态的 Finish（无状态容器为 nil，静默跳过）。
func (j *dlJob) backfill(path string) error {
	if j.norm == nil {
		return nil
	}
	return j.norm.Finish(path)
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

// flushedMark 是"已确认落盘"的断点：分片序号 + 绝对字节数（一次性发布，见 dlJob.flushed）。
//
// 两个字段必须同一次读写（同源）：HLS 分片长度不一，序号与字节互不推导；
// 而恢复既要序号（续传起点）又要字节数（把 .part 对齐到账本位置）。
// 拆成两个原子值就会读到「新序号 + 旧字节数」这种自相矛盾的组合。
type flushedMark struct {
	segs  int64
	bytes int64
}

// segFlushedNow 返回"已确认落盘"的分片序号。持久化必须用它而不是 segNow()：
// segNow() 可能领先磁盘若干 MB，拿它当断点续传会在文件中间留下空洞。
func (j *dlJob) segFlushedNow() int64 { return j.flushedMarkNow().segs }

// flushedBytesNow 返回"已确认落盘"的绝对字节数（含 init 段与恢复时的 baseBytes）。
// 与 segFlushedNow 同源（同一次 Load）—— 两者永远描述同一个时刻。
func (j *dlJob) flushedBytesNow() int64 { return j.flushedMarkNow().bytes }

// flushedMarkNow 一次读出整对账本值；从未落盘过（nil）返回零值。
func (j *dlJob) flushedMarkNow() flushedMark {
	if m := j.flushed.Load(); m != nil {
		return *m
	}
	return flushedMark{}
}

// setFlushed 推进已落盘断点（streamWriter 每次 flush 成功后回调）。
// segs 与 bytes 必须在同一次调用里给出：它们描述的是同一个时刻的事实。
func (j *dlJob) setFlushed(segs, bytes int64) {
	j.flushed.Store(&flushedMark{segs: segs, bytes: bytes})
}

func (j *dlJob) segTotal() int64 { return atomic.LoadInt64(&j.segTot) }

// setBytes 更新直链下载的字节进度（原子）。tot 未知时传 0 —— 界面据此退回
// "下载中…"，而不是显示一个分母为 0 的假百分比。
//
// 直链进度为什么不用 setSeg：那个函数的语义是"分片序号"，界面会把它渲染成
// 「分片 3 / 20」。直链的段数只是内部并发实现（8MiB 一片、与并发数无关），
// 给用户看没有意义 —— 用户要的是"这个大文件下了多少"。
func (j *dlJob) setBytes(done, tot int64) {
	atomic.StoreInt64(&j.bytesDone, done)
	atomic.StoreInt64(&j.bytesTot, tot)
}

func (j *dlJob) bytesNow() int64 { return atomic.LoadInt64(&j.bytesDone) }

func (j *dlJob) bytesTotal() int64 { return atomic.LoadInt64(&j.bytesTot) }

// 直链 .part 的两种写入模式（见 dlJob.partMode）。
const (
	partModeChunked = "chunked" // 分片并发 + WriteAt 定位写（文件中间可能有洞）
	partModeStream  = "stream"  // 单连接连续追加（文件大小即有效字节数）
)

// setPartMode 记录 .part 的写入模式，并触发一次状态落盘。
//
// 为什么必须立刻落盘：这个值决定下次启动时"文件大小能不能当续传起点"，
// 而进程随时可能被杀 —— 只在收尾时写就来不及了（那正是 P0-4 直链版本的成因）。
func (j *dlJob) setPartMode(m string) {
	if j.partModeNow() == m {
		return
	}
	j.partMode.Store(m)
	if j.rt != nil {
		j.rt.markDirty()
	}
}

// partModeNow 返回当前记录的写入模式（空 = 未知，升级前的旧任务）。
func (j *dlJob) partModeNow() string {
	if v, ok := j.partMode.Load().(string); ok {
		return v
	}
	return ""
}

// setRestartNote 记下恢复路径上的一次性提示（见 dlJob.restartNote）。
func (j *dlJob) setRestartNote(note string) {
	j.restartMu.Lock()
	j.restartNote = note
	j.restartMu.Unlock()
}

// takeRestartNote 取走提示（pipeline 转写进任务状态后即清空）。
func (j *dlJob) takeRestartNote() string {
	j.restartMu.Lock()
	defer j.restartMu.Unlock()
	n := j.restartNote
	j.restartNote = ""
	return n
}

// ============================================================
// 流式下载：边下边写
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
	onWrite func(next int)                 // 每次推进后回调（刷新界面进度）
	onFlush func(flushed int, bytes int64) // 每次落盘后回调（推进可持久化断点，序号与字节同源）
	norm    func([]byte) []byte            // 写入前的规范化（nil = 原样写）

	// baseBytes 打开文件时文件里**已被记账**的字节数：续传时 = init 段 + 已确认
	// 落盘的分片字节（调用方按账本对齐 .part 后给），全新任务 = 0。
	// 它让 flushedBytes 报的是**绝对文件偏移**，恢复时能直接拿去 os.Truncate。
	baseBytes int64
	// flushedBytes 已确认落盘的绝对字节数（含 baseBytes），与 flushed 序号在同一次
	// 结算里推进 —— 账本成对，读端不会看到错配组合。
	flushedBytes int64
	// pendingBytes 已写进 bufio、尚未落盘的字节数。只有 flush 成功后才并入
	// flushedBytes：把"写进缓冲区"当成已落盘，正是 R9 那个断点虚高的坑。
	pendingBytes int64
}

// newStreamWriter 以追加模式打开 path（不存在则创建），从 startIdx 开始写。
// 追加模式是断点续传的关键：恢复时直接从上次断点继续往后写。
//
// baseBytes 是调用方按账本对齐后、文件里已被记账的字节数。传错的后果是
// flush 回调报出错误的绝对偏移（进而把 .part 截到错误位置），所以调用方
// 必须保证"传到这里的 baseBytes == 文件当前长度"（见 pipeline 的对齐判据）。
func newStreamWriter(path string, startIdx int, baseBytes int64, onWrite func(int), onFlush func(int, int64), norm func([]byte) []byte) (*streamWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &streamWriter{
		next:         startIdx,
		flushed:      startIdx,
		buf:          make(map[int][]byte),
		w:            bufio.NewWriterSize(f, 8<<20),
		f:            f,
		onWrite:      onWrite,
		onFlush:      onFlush,
		norm:         norm,
		baseBytes:    baseBytes,
		flushedBytes: baseBytes,
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
		if n, err := sw.w.Write(d); err != nil {
			return err
		} else {
			sw.pendingBytes += int64(n)
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
	sw.settleLocked()
}

// settleLocked 在缓冲**成功落盘之后**结算账本（调用方须持 mu）。
//
// 结算动作必须晚于 flush：pendingBytes 记的是"已写进 bufio"的字节，缓冲区
// 没落盘时它们不在文件里；拿它当断点会让恢复从文件末尾之后的位置续写，
// 中间那段永远缺失（R9）。序号与字节数在同一次结算里推进，读端永远拿到
// 自洽的一对（见 dlJob.flushed）。
func (sw *streamWriter) settleLocked() {
	if sw.pendingBytes == 0 && sw.flushed == sw.next {
		return
	}
	sw.flushedBytes += sw.pendingBytes
	sw.pendingBytes = 0
	sw.flushed = sw.next
	if sw.onFlush != nil {
		sw.onFlush(sw.flushed, sw.flushedBytes)
	}
}

// Next 返回内存断点（下一个待写序号）。它反映"已受理的下载进度"，可能领先
// 磁盘若干 MB（bufio 缓冲还没 flush），**不能**拿它当续传起点 —— 见 Flushed。
func (sw *streamWriter) Next() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.next
}

// Flushed 返回已确认落盘的断点：所有 < Flushed() 的分片字节都已写进文件。
// 持久化续传起点只能用这个值。用 Next() 会在两种情况下把断点推到磁盘之外：
// 缓冲里的数据还没 flush，或关闭时的最终 Flush 失败（Close 只在成功时才推进
// flushed）。断点虚高的后果是重启后从空洞之后继续追加 —— 缺口永久留在产物里，
// 而状态与日志一切正常（R9 / 本项目头号缺陷形态）。
func (sw *streamWriter) Flushed() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.flushed
}

// FlushedBytes 返回已确认落盘的**绝对字节数**（含 baseBytes），与 Flushed()
// 描述同一时刻。恢复时按它对 .part 做对齐（truncate 到账本位置），
// 所以它必须来自写入层自己的结算，不能拿 os.Stat().Size() 代替。
func (sw *streamWriter) FlushedBytes() int64 {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.flushedBytes
}

// Close 冲刷缓冲并关闭文件。保存断点前必须先调用，否则尾部数据会丢。

func (sw *streamWriter) Close() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	err := sw.w.Flush()
	if err == nil {
		// 尾部数据已落盘，断点可以安全推进到 next
		sw.settleLocked()
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
// 分片模式下 ctx 取消或失败会保留 .part 与 .meta（位图记下已落盘的片），
// 任务恢复后只重下缺失片。片长固定为 chunkSizeFixed、与并发数解耦，
// 否则片数恒等于并发数、中断时位图上一个 true 都来不及有（详见该常量说明）。
func (j *dlJob) downloadDirect(ctx context.Context, outPath string) error {
	// 模式 1：分片续传（位图恢复）
	if m, ok := loadChunkMeta(outPath); ok {
		j.setPartMode(partModeChunked)
		return j.downloadChunked(ctx, outPath, m)
	}

	// 模式 2：全新下载且服务器支持 Range → 分片下载（小文件不值得分片）
	if partFileOffset(outPath) == 0 {
		if total, ok := j.probeRange(ctx); ok {
			// 探到总量就先挂上分母：进度条从 0% 起步，不必等第一片下完才有数
			j.setBytes(0, total)
			if total > minChunkedSize {
				if m, ok := j.newChunkPlan(total); ok {
					if err := saveChunkMeta(outPath, m); err != nil {
						return err
					}
					j.setPartMode(partModeChunked)
					return j.downloadChunked(ctx, outPath, m)
				}
				// 片数超限（total 来自 Content-Range、不可信）：退回单连接，不建位图
			}
		}
	}

	// 模式 3：单连接续传
	offset := partFileOffset(outPath)
	// 模式守卫（P0-4 的直链版本）：上次是分片模式、但位图已经丢了（落盘失败、
	// 被外部删除，或上次退出时还没写盘）。此时文件是 WriteAt 稀疏写的产物 ——
	// 大小 = 最大已写区间末端，**中间可能有洞** —— 按大小续传会从文件末尾往后
	// append，那些洞永远补不上：成品能播，但其中几段是坏数据，日志一切正常。
	//
	// 判据必须取"上次记录的模式"，不能取"现在能不能读到位图"：位图丢失本身
	// 就是要防的形态，拿它当判据等于没有守卫。
	if offset > 0 && j.partModeNow() == partModeChunked {
		fmt.Printf("[direct] 上次为分片模式但位图已丢失，临时文件可能存在空洞：清空重下\n")
		j.setRestartNote("续传位图丢失，已从头下载")
		if err := os.Truncate(outPath, 0); err != nil {
			return err
		}
		offset = 0
	}
	if offset == 0 {
		j.setPartMode(partModeStream)
	}
	for attempt := 0; ; attempt++ {
		req, err := j.rt.newRequest(j.m3u8URL, j.segmentReferer(j.m3u8URL))
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
			// 分母尽力而为：206 从 Content-Range 取总量，200 用 Content-Length。
			// 都拿不到就保持 0（界面退回"下载中…"）—— 收尾会按成品实际大小补上，
			// 所以"直链完成后仍 0%"这条路径已经封死。
			tot := responseTotalBytes(resp, offset)
			j.setBytes(offset, tot)
			cpErr, closeErr := streamToFile(resp.Body, outPath, offset, func(written int64) {
				j.setBytes(offset+written, tot)
			})
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

// minChunkedSize 低于该大小的直链不分片（并发与位图文件的开销都不划算）。
const minChunkedSize = 1 << 20

// chunkSizeFixed 直链分片的目标片长。
//
// 刻意**不**按并发数均分（旧实现 size = ceil(total/workers)）：那样片数恒等于并发数
// （默认 10），十片等长、齐头并进，中断时往往一片都没跑完 —— 位图上一个 true 都
// 来不及有，注释里承诺的「只下载未完成的分片」等于没做（4GB 下到 50% 中断，一片
// 未完成，重下 100%）。固定片长让位图粒度与并发解耦：4GB / 8MiB = 512 片，下到
// 一半中断能真省下约 2GB。片数远多于并发时由 sem 限流，天然支持。
const chunkSizeFixed = 8 << 20 // 8 MiB

// maxChunkCount 位图允许的最大片数。
// total 来自 Content-Range（远端字节，不可信），片长固定后片数与 total 成正比 ——
// 一个离奇的 total 会撑出巨大的位图。8TiB 以内正常可用，超限则退回单连接。
const maxChunkCount = 1 << 20

// chunkMeta 直链分片下载的断点位图，JSON 持久化在 <part>.meta。
// Done[i] 标记第 i 片是否已完整落盘；恢复时只重下未完成片。
type chunkMeta struct {
	Total int64  `json:"total"`
	Size  int64  `json:"size"`
	Done  []bool `json:"done"`
}

func chunkMetaPath(partPath string) string { return partPath + ".meta" }

// chunkCount 按片长切分 total 字节得到的片数（向上取整；用除余避免 total+size 溢出）。
func chunkCount(total, size int64) int64 {
	if size < 1 {
		size = chunkSizeFixed
	}
	n := total / size
	if total%size != 0 {
		n++
	}
	return n
}

func loadChunkMeta(partPath string) (*chunkMeta, bool) {
	data, err := os.ReadFile(chunkMetaPath(partPath))
	if err != nil {
		return nil, false
	}
	var m chunkMeta
	if json.Unmarshal(data, &m) != nil || m.Total <= 0 || m.Size <= 0 || len(m.Done) == 0 {
		return nil, false
	}
	// 位图必须与片划分自洽：短了会漏下尾部（成品短一截），长了会在末尾请求越界区间
	// （416 → 判为"内容已变" → 白删重下）。不自洽一律当没有位图 —— 宁可重下，
	// 也不能按错位图拼出坏文件。
	want := chunkCount(m.Total, m.Size)
	if want != int64(len(m.Done)) || want > maxChunkCount {
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

// 位图落盘节流：每完成这么多片、或距上次落盘达到这么久，就写一次盘。
// 逐片写盘在 512 片的文件上是白烧 IO；太懒又会在中断时丢掉最近的完成片（白下）。
// 因此「结束」路径（失败/中断）必须显式 Flush 一次 —— 见 downloadChunked。
const (
	chunkMetaFlushEvery = 8
	chunkMetaFlushGap   = time.Second
)

// chunkMetaStore 位图的并发安全记账 + 节流持久化。
//
// 记账时机是硬约束：MarkDone 只能在分片数据**真正落盘之后**调用（downloadRange
// 成功返回）。先记账后落盘会在中断时留下"位图说完成了、盘上却没有"的空洞 ——
// 恢复时那片被跳过，成品里就是一段坏数据。
type chunkMetaStore struct {
	partPath  string
	mu        sync.Mutex
	m         *chunkMeta
	done      int
	stale     bool
	since     int       // 距上次落盘的完成片数
	flushedAt time.Time // 上次落盘时刻
}

func newChunkMetaStore(partPath string, m *chunkMeta) *chunkMetaStore {
	s := &chunkMetaStore{partPath: partPath, m: m, flushedAt: time.Now()}
	for _, d := range m.Done {
		if d {
			s.done++
		}
	}
	return s
}

// MarkDone 标记第 idx 片已完成（调用前该片数据必须已落盘），按节流策略落盘。
// 返回当前完成数与总片数，供进度上报。
func (s *chunkMetaStore) MarkDone(idx int) (done, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < 0 || idx >= len(s.m.Done) || s.m.Done[idx] {
		return s.done, len(s.m.Done)
	}
	s.m.Done[idx] = true
	s.done++
	s.since++
	if s.since >= chunkMetaFlushEvery || time.Since(s.flushedAt) >= chunkMetaFlushGap {
		s.flushLocked()
	}
	return s.done, len(s.m.Done)
}

// MarkStale 服务器内容已变（HTTP 416）：整份位图失效。
func (s *chunkMetaStore) MarkStale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stale = true
}

func (s *chunkMetaStore) Stale() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stale
}

func (s *chunkMetaStore) Done() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

func (s *chunkMetaStore) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m.Done)
}

// Pending 尚未完成的分片下标（升序）。
func (s *chunkMetaStore) Pending() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.m.Done)-s.done)
	for i, d := range s.m.Done {
		if !d {
			out = append(out, i)
		}
	}
	return out
}

// Flush 强制落盘（无未落盘改动时跳过）。
func (s *chunkMetaStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

// flushLocked 在持锁状态下写盘。写失败不中断下载：位图丢了只是下次多下几片，
// 数据本身是对的，不该因此把任务判失败。
// （诊断输出沿用本文件既有的 fmt.Printf 风格，待统一日志设施时一并收。）
func (s *chunkMetaStore) flushLocked() {
	if s.since == 0 {
		return
	}
	s.since = 0
	s.flushedAt = time.Now()
	if err := saveChunkMeta(s.partPath, s.m); err != nil {
		fmt.Printf("[direct] 分片位图落盘失败: %v\n", err)
	}
}

// newChunkMeta 按固定片长切分：Size = size（末片可能不足），片数 = ceil(total/size)。
func newChunkMeta(total, size int64) *chunkMeta {
	if size < 1 {
		size = chunkSizeFixed
	}
	return &chunkMeta{Total: total, Size: size, Done: make([]bool, chunkCount(total, size))}
}

// newChunkPlan 为 total 字节的直链规划分片。片数超过 maxChunkCount 时不规划
// （调用方退回单连接）—— Content-Range 是不可信输入，别让一个离奇的 total
// 撑出巨大位图。
func (j *dlJob) newChunkPlan(total int64) (*chunkMeta, bool) {
	size := j.rt.chunkSizeNow()
	if chunkCount(total, size) > maxChunkCount {
		return nil, false
	}
	return newChunkMeta(total, size), true
}

// probeRange 探测服务器是否支持 Range 并取得文件总大小。
// 发 Range: bytes=0-0，仅当 206 且 Content-Range 给出明确总大小时确认可用。
func (j *dlJob) probeRange(ctx context.Context) (int64, bool) {
	req, err := j.rt.newRequest(j.m3u8URL, j.segmentReferer(j.m3u8URL))
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
// 失败的分片在片内重试（maxRetriesNow() 次）；整体出错时把位图落盘后保留，
// 供下次续传只下缺失片。全部完成后删除 .meta（分片状态失效）。
func (j *dlJob) downloadChunked(ctx context.Context, outPath string, m *chunkMeta) error {
	f, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	// 用 closeOnce 而不是裸 defer：stale 分支必须先关句柄再删文件（Windows 上
	// 删除被打开的文件会失败），裸 defer 会让 Close 走第二遍并返回 ErrClosed。
	var closeOnce sync.Once
	closeFile := func() { closeOnce.Do(func() { _ = f.Close() }) }
	defer closeFile()

	store := newChunkMetaStore(outPath, m)
	total := m.Total
	// 直链进度按字节上报，不是片数（见 setBytes 的说明）。已完成片数 × 片长，
	// 换算后夹到 total —— 末片通常不满，直接乘会算过头。
	j.setBytes(bytesOfChunks(store.Done(), m.Size, total), total)

	var wg sync.WaitGroup
	sem := make(chan struct{}, j.rt.concurrencyNow())
	for _, idx := range store.Pending() {
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
				if errors.Is(err, errChunkStale) {
					store.MarkStale()
				}
				fmt.Printf("[direct] 分片 %d-%d 失败: %v\n", start, end, err)
				return
			}
			// 数据已落盘（downloadRange 用 WriteAt 直写，返回即已交给内核），
			// 这时才允许记账 —— 反过来会留下"位图说完成了、盘上却是空洞"的假状态。
			done, _ := store.MarkDone(idx)
			j.setBytes(bytesOfChunks(done, m.Size, total), total)
		}(idx)
	}
	wg.Wait()

	if store.Stale() {
		// 服务器内容已变：先关句柄（Windows 上打开中的文件删不掉），再丢弃 part/meta，
		// 下次任务全量重下
		closeFile()
		os.Remove(outPath)
		os.Remove(chunkMetaPath(outPath))
		return errChunkStale
	}
	if store.Done() != store.Total() {
		// 有分片失败/被取消：位图必须落盘再退出，否则本次已完成的片下次白下一遍
		// （这正是「中断即全量重下」的根因）。
		store.Flush()
		return fmt.Errorf("分片下载未完成")
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
		req, err := j.rt.newRequest(j.m3u8URL, j.segmentReferer(j.m3u8URL))
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
			if !sleepCtx(ctx, time.Duration(attempt-1)*300*time.Millisecond) {
				return ctx.Err()
			}
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
			if !sleepCtx(ctx, time.Duration(attempt-1)*300*time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		// 按 expected 限长：服务器忽略 Range 结束偏移、把整份文件回给一个分片请求时
		// （行为不当的 CDN / 中间层），多出的字节会被 WriteAt 写进下一个分片的区域
		// 并把它污染（邻片若已 done 就再也不会被重写，成品里那段是坏数据）。
		// 限长后 n 不可能超过 expected，多出的部分直接丢弃。
		n, cpErr := copyWithIdleTimeout(
			&offsetWriter{f: f, off: start, end: end},
			limitReadCloser(resp.Body, expected, resp.Body),
			transferIdleTimeout)
		resp.Body.Close()
		if cpErr != nil {
			if ctx.Err() != nil {
				return cpErr
			}
			if attempt >= j.rt.maxRetriesNow() {
				return cpErr
			}
			attempt++
			if !sleepCtx(ctx, time.Duration(attempt-1)*300*time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		if n != expected {
			if attempt >= j.rt.maxRetriesNow() {
				return fmt.Errorf("分片 %d-%d 收到 %d 字节, 期望 %d", start, end, n, expected)
			}
			attempt++
			if !sleepCtx(ctx, time.Duration(attempt-1)*300*time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		return nil
	}
}

// offsetWriter 把 Write 转发为固定偏移的 WriteAt（并发分片各自写自己的区间）。
// end 是本次分片可写的最后一个字节偏移：写入会被截断在该位置，保证任何情况下
// 溢出的字节都不会落到下一个分片的区域（调用方已用 limitReadCloser 在源头限长，
// 这一层是纵深防御）。
type offsetWriter struct {
	f   *os.File
	off int64
	end int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	if w.off > w.end {
		return 0, nil // 已写满本次区间，多余的直接丢弃
	}
	if remain := w.end - w.off + 1; int64(len(p)) > remain {
		p = p[:remain]
	}
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
//
// onWrite 非 nil 时，每写一块回调一次"本次流累计写入的字节数"（不含 offset）。
// 直链没有分片可数，进度只能按字节来（见 dlJob.setBytes）；加 offset 是调用方的事。
func streamToFile(body io.ReadCloser, outPath string, offset int64, onWrite func(int64)) (cpErr, closeErr error) {
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
	var dst io.Writer = f
	if onWrite != nil {
		dst = &countingWriter{w: f, onWrite: onWrite}
	}
	_, cpErr = copyWithIdleTimeout(dst, body, transferIdleTimeout)
	closeErr = f.Close()
	return
}

// countingWriter 累计写入字节数并回调（直链下载的字节级进度）。
// 不加锁：调用方 copyWithIdleTimeout 是单 goroutine 顺序写。
type countingWriter struct {
	w       io.Writer
	onWrite func(int64)
	n       int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.onWrite(c.n)
	return n, err
}

// bytesOfChunks 把"已完成片数"换算成字节数，并夹到 total 以内
// （末片通常不满一整片，直接乘会算过头）。
func bytesOfChunks(done int, size, total int64) int64 {
	n := int64(done) * size
	if n > total {
		n = total
	}
	return n
}

// responseTotalBytes 尽力给出这份响应对应的**完整文件**总字节数，用作直链进度分母：
//   - 206 → Content-Range 的 "/total"（唯一权威来源）；
//   - 200 → Content-Length（调用方在 200 时已把 offset 归零，所以相加同时也对）。
//
// 拿不到返回 0：分母未知时界面退回"下载中…"，完成后由收尾按成品大小补上。
func responseTotalBytes(resp *http.Response, offset int64) int64 {
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if total, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil && total > 0 {
				return total
			}
		}
	}
	if resp.ContentLength > 0 {
		return offset + resp.ContentLength
	}
	return 0
}

// segmentReferer 返回抓取 targetURL 应携带的 Referer：分片 CDN（以及部分站的播放
// 列表 CDN）防盗链可能只认解析站域名（浏览器实际发出的那个 Referer），而不是页面
// 域名。命中 segRefs（按 host 精确匹配）则**原样采用**它 —— 包括命中空串，空串表
// 示浏览器对该 host 没带 Referer，带页面 Referer 反而会被 CDN 判 403；只有未命中
// 才回退到 j.referer（CLI / 直链任务 segRefs 恒空，等于始终走这条）。
//
// segRefs 的键是扩展侧 `new URL().host` 的形态，查表键必须同形，否则命中不了。
func (j *dlJob) segmentReferer(targetURL string) string {
	if len(j.segRefs) == 0 {
		return j.referer
	}
	u, err := url.Parse(targetURL)
	if err != nil || u.Host == "" {
		return j.referer
	}
	// host 大小写不敏感，而 url.Parse 不归一化 host。
	host := strings.ToLower(u.Host)
	if ref, ok := j.segRefs[host]; ok {
		return ref
	}
	// 同上「键必须同形」：浏览器 `new URL().host` 会**消掉默认端口**（https:443 /
	// http:80 都省掉），而 Go 的 url.Host 原样保留 —— m3u8 里显式写成 :443 / :80
	// 的目标因此对不上，会退回页面 Referer（白名单型 CDN 上就是 403）。
	// 端口为空或恰为 scheme 默认端口时，去掉端口再查一次（此时与浏览器视角等价）。
	//
	// 非默认端口不兜底：:8443 是另一个 origin，浏览器也会带着端口上报。
	// 用字符串裁剪而不是 u.Hostname()，为的是保留 IPv6 的方括号 —— 浏览器
	// new URL("http://[::1]:443/").host 给的是 "[::1]"，而 Hostname() 给 "::1"。
	if p := u.Port(); p != "" && p == defaultPortOf(u.Scheme) {
		if ref, ok := j.segRefs[strings.TrimSuffix(host, ":"+p)]; ok {
			return ref
		}
	}
	return j.referer
}

// defaultPortOf 返回 scheme 的默认端口 —— 即浏览器 URL 规范化时会从 host 里省掉的
// 那个。非 http(s) 返回空串（此时不做去端口查表）。
func defaultPortOf(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// fetchSegment 带重试地把单个分片下载到内存。ctx 取消时立刻返回。

func fetchSegment(ctx context.Context, j *dlJob, segURL string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= j.rt.maxRetriesNow(); attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := j.rt.newRequest(segURL, j.segmentReferer(segURL))
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
			if !sleepCtx(ctx, time.Duration(attempt)*time.Second) {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d: HTTP %d", attempt, resp.StatusCode)
			if !sleepCtx(ctx, time.Duration(attempt)*time.Second) {
				return nil, ctx.Err()
			}
			continue
		}
		data, err := readAllWithIdleTimeout(resp.Body, transferIdleTimeout)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: read body: %w", attempt, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !sleepCtx(ctx, time.Duration(attempt)*time.Second) {
				return nil, ctx.Err()
			}
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
	// 账本成对给出：序号用本轮起点，字节数沿用恢复时的对齐结果（baseBytes）。
	// 后续 writer 的每次 flush 都会用"同一时刻的序号 + 绝对字节数"覆盖它。
	baseBytes := j.flushedBytesNow()
	j.setFlushed(int64(startIdx), baseBytes)

	sw, err := newStreamWriter(outPath, startIdx, baseBytes, func(next int) {
		j.setSeg(int64(next), dispTot)
		if j.live {
			fmt.Printf("\r  已录制分片: %d   ", next)
		} else {
			fmt.Printf("\r  下载进度: %d / %d   ", next, startIdx+total)
		}
	}, func(flushed int, bytes int64) {
		// 断点只在真实落盘后推进（R9）：Engine.Stop 可能在 pipeline 收尾前
		// 就落盘状态；若断点领先磁盘字节，续传会在文件中间留下空洞。
		// 字节数与序号同源（一次回调），读端不会看到错配的一对。
		j.setFlushed(int64(flushed), bytes)
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
	var closeErr error
	closeSW := func() {
		if closed {
			return
		}
		closed = true
		if cerr := sw.Close(); cerr != nil {
			// 关闭失败 = 尾部数据没进文件。记下来交给调用方，不降级成一行警告
			// （见 finishStream 的说明）。
			closeErr = cerr
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

	// 先冲刷关闭、再定稿断点：返回值会被上层当成续传起点持久化（pipeline 的
	// from），必须保证它对应的字节已经真正落在 .part 里（R9）。见 finishStream。
	closeSW()
	next, ferr := finishStream(sw, j, dispTot, closeErr)

	errMu.Lock()
	e := firstErr
	errMu.Unlock()
	if ferr != nil {
		return next, ferr
	}
	if e != nil {
		return next, e
	}
	return next, nil
}

// finishStream 关闭输出流后把"可持久化断点"定稿，返回续传起点（写入序号）。
//
// 断点只能是 Flushed()（确实落盘的字节数），不能是 Next()（内存进度）：关闭时
// 若最终 Flush 失败，尾部若干 MB 并没有进文件，而 Next() 已经领先它们。上层拿
// 返回值当续传起点持久化（pipeline 的 te.st.segDone → 下次启动的 from），直播还会
// 用 segFlushedNow 推进去重水位线 —— 断点虚高就是"产物里留下永久空洞、而状态与
// 日志一切正常"（本项目头号缺陷形态）。界面进度仍报 Next()：那是给用户看的真实
// 下载进度，与"能从哪里续"是两件事。
//
// closeErr 由 closeSW 转交（它可能已经被 defer 调用过）：关闭失败是真实故障，
// 一路交回调用方走失败路径，不吞成一行警告。
func finishStream(sw *streamWriter, j *dlJob, dispTot int64, closeErr error) (int, error) {
	next, flushed := sw.Next(), sw.Flushed()
	j.setSeg(int64(next), dispTot)
	// 序号与字节数必须取自同一次结算（都经 sw 的锁读，见 FlushedBytes）。
	j.setFlushed(int64(flushed), sw.FlushedBytes())
	if closeErr != nil {
		return flushed, closeErr
	}
	return next, nil
}

// ============================================================
// 直播跟随模式：循环拉取播放列表，增量下载新分片
// ------------------------------------------------------------
// 直播/事件流的播放列表没有 #EXT-X-ENDLIST，且列表不断增长（旧分片会被
// 滚动淘汰）。跟随模式每 livePollInterval 轮询一次播放列表，用 media sequence
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
			return next, nil // 用户中断（停止/取消/退出）：干净返回，收尾意图由上层 finishInterrupt 决定
		}
		content, base, isDirect, err := j.fetchPlaylist(ctx)
		if err != nil {
			return next, err
		}
		if isDirect {
			return next, fmt.Errorf("直播播放列表响应变为直链媒体，无法继续跟随录制")
		}
		cur := parsePlaylist(content, base)
		// 同一轮窗口内换 key（key rotation）无法安全解密：显式失败。
		// 跨轮换 key 是支持的 —— 每轮按本轮声明的 key 解密本轮分片。
		//
		// 校验必须走 validatePlaylist 这个唯一入口，不能只查 ensureSingleKey：
		// 畸形加密声明（METHOD 非 NONE 但 URI 解析不出）、#EXT-X-BYTERANGE、
		// 非 identity 的 KEYFORMAT 在点播路径都会被它挡下，直播若只做简易校验
		// 就成了绕过口 —— 那几类恰好都是「产物坏了但日志正常」的静默损坏。
		if kerr := validatePlaylist(cur); kerr != nil {
			return next, kerr
		}
		// 每轮轮询重新装配解密器（幂等：key 未变不重拉）；key 轮换时按新 key 解密后续分片
		if kerr := j.ensureDecryptor(ctx, cur.key); kerr != nil {
			return next, kerr
		}

		// 窗口滚动检测：上一轮录到的最大序号与本轮列表首片之间若断开，说明中间
		// 那些分片已经被滑动窗口淘汰、永远补不回来了（轮询间隔超过窗口时长时发生）。
		// 产物时间轴上那一段就是空的，必须计入缺口——漏报等于把"产物不完整"藏起来。
		if wm, ok := j.seenWatermark(); ok && cur.mediaSeq > wm+1 {
			missing := int(cur.mediaSeq - wm - 1)
			sec := estimateMissingSeconds(cur, missing)
			j.addGapSeconds(sec)
			fmt.Printf("[live] 窗口已滚动：%d 个分片（约 %.1fs）被淘汰，产物时间轴将出现空洞\n", missing, sec)
		}

		var newSegs []string
		var newDurs []float64 // 与本批分片一一对应的 EXTINF，失败时用来算缺口
		var pendSeqs []uint64 // 本批分片的 media sequence：落盘确认后才提交给水位线
		firstNewPos := -1     // 本批首个新分片在当前列表中的位置（派生 IV 的序号基准）
		for pos, u := range cur.segments {
			// 按 media sequence 判重（列表位置 + MEDIA-SEQUENCE），而不是"URL 是否
			// 见过"：见 seenCovers。判重只看水位线，提交在落盘之后（见
			// commitLiveWaterline）——在这里记会把没下成的分片也算成已录制。
			seq := cur.mediaSeq + uint64(pos)
			if j.seenCovers(seq) {
				continue
			}
			pendSeqs = append(pendSeqs, seq)
			newSegs = append(newSegs, u)
			newDurs = append(newDurs, durAt(cur, pos))
			if firstNewPos < 0 {
				firstNewPos = pos
			}
		}

		if len(newSegs) > 0 {
			empty = 0
			startIdx := next
			n, derr := streamDownload(ctx, j, newSegs, next, cur.mediaSeq+uint64(firstNewPos), outPath)
			next = n
			// 落盘之后再记账（P1-1）：只有确认写进文件的前缀才算"已录制"。
			commitLiveWaterline(j, pendSeqs, startIdx)
			if derr != nil {
				if ctx.Err() != nil {
					return next, nil
				}
				// 这批里"本该录到、却没写进文件"的分片，在产物时间轴上就是一段
				// 真实空洞（源站故障时它们大概率已跟着窗口滚走）。用户主动停止
				// （ctx 取消）不算缺口——那是"录到哪算哪"，不是中间少了一段。
				written := int(j.segFlushedNow()) - startIdx
				if written < 0 {
					written = 0
				}
				if written < len(newDurs) {
					j.addGapSeconds(sumDurs(newDurs[written:]))
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

// durAt 取播放列表里第 pos 个分片的 #EXTINF 时长（越界或该片未声明时长返回 0）。
func durAt(pl playlistInfo, pos int) float64 {
	if pos < 0 || pos >= len(pl.durs) {
		return 0
	}
	return pl.durs[pos]
}

// sumDurs 求一组分片时长的和（缺口时长）。
func sumDurs(ds []float64) float64 {
	var s float64
	for _, d := range ds {
		s += d
	}
	return s
}

// estimateMissingSeconds 估算被窗口淘汰的 n 个分片占用的时长。
//
// 那些分片已经取不到了（它们从列表里消失才叫"被淘汰"），拿不到各自的 EXTINF，
// 只能用本轮列表的平均片长作系数。宁可粗报也不漏报：缺口的价值就在于
// 让用户知道"这个文件少了一段"。
func estimateMissingSeconds(pl playlistInfo, n int) float64 {
	if n <= 0 || len(pl.segments) == 0 {
		return 0
	}
	return pl.totalDur / float64(len(pl.segments)) * float64(n)
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
