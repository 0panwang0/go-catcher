// 单任务下载管线与暂停/取消收尾。
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// runGuarded 在独立 goroutine 里跑 fn（= 下载管线），拦下任何漏网的 panic。
//
// 为什么必须有这一层：runDiskPipeline 由 `go` 启动、没有返回值，一旦 panic
// 就是整个进程消失——GUI 没了，其它正在下载/录制的任务也一起没了。fMP4 解析
// 那边已经做了边界校验 + 入口 recover，这里是最后一道，覆盖"没人想到的那条路"
// （畸形 init 段、收尾阶段的容器操作等）。
//
// 独立成函数而不是内联 defer：内联没法写测试，而"崩溃被拦住了"这件事必须可验。
//
// 已经在终态的任务不改写状态：panic 完全可能发生在收尾成功之后，
// 把一次成功保存改写成"失败"和崩溃一样糟（用户会以为文件没了）。
func runGuarded(te *taskEntry, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[core] FATAL: 任务 %s 内部错误（已拦截）: %v\n%s\n", te.st.id, r, debug.Stack())
			te.mu.Lock()
			finished := te.st.done
			te.mu.Unlock()
			if finished {
				return
			}
			failTask(te, fmt.Sprintf("内部错误（已拦截崩溃）: %v", r))
			te.rt.markDirty()
		}
	}()
	fn()
}

func runDiskPipeline(te *taskEntry) {
	acquired := false
	defer func() {
		// 释放并发槽，让排队的下一个任务进场
		if acquired {
			te.rt.limiter.release()
		}
	}()

	// 建可取消的 ctx（暂停/取消都通过它中断下载）。
	// 在排队前就建好，这样排队期间也能被暂停/取消打断。
	ctx, cancel := context.WithCancel(context.Background())
	te.mu.Lock()
	te.cancel = cancel
	te.intent = intentNone
	te.st.queued = true
	te.st.running = false
	te.st.stage = "排队中"
	te.mu.Unlock()
	defer cancel()
	te.rt.markDirty()

	// 等待并发槽位（超过当前上限的新任务在此排队；队列中可被暂停/取消打断）
	if !te.rt.limiter.acquire(ctx) {
		finishInterrupt(te)
		return
	}
	acquired = true

	te.mu.Lock()
	te.st.queued = false
	te.st.running = true
	te.st.paused = false
	te.st.stage = "解析视频源"
	st := te.st
	// 兼容旧版状态文件：失败任务曾把 finalPath 清空（v0.2 bug），重试/续传时
	// 按 filename+saveDir 拼回，否则 .part 会写到工作目录下的游离 ".part"。
	// 不走 uniquePath：.part 就在原路径上，查重反而会错开到 "xxx (1).ts"。
	if st.finalPath == "" && st.filename != "" {
		st.finalPath = filepath.Join(st.saveDir, st.filename)
		te.st.finalPath = st.finalPath
	}
	te.mu.Unlock()
	te.rt.markDirty()

	job := &dlJob{
		rt:      te.rt,
		id:      st.id,
		m3u8URL: st.m3u8URL,
		referer: st.referer,
		saveDir: st.saveDir,
		fname:   st.filename,
	}
	te.mu.Lock()
	te.job = job
	te.mu.Unlock()

	// 进度回调：每写入一批分片就刷新任务状态（持久化时以它为准）。
	// 跨分片规范化状态也在此同步回任务（暂停/重启续传沿用，一个不透明字节包）：
	// progress 由 streamWriter 在"已按序写入"后触发，状态与断点严格一致。
	job.progress = func(stage string, done, tot int64) {
		te.mu.Lock()
		te.st.stage = stage
		te.st.segDone = done
		te.st.segTot = tot
		if job.norm != nil {
			te.st.normState = job.norm.Snapshot()
		}
		te.mu.Unlock()
	}

	// 下载期间写 .part，写完后改名成正式文件（避免半成品被当成成品）
	partPath := st.finalPath + ".part"

	fail := func(msg string) {
		fmt.Printf("[disk] FAIL: %s\n", msg)
		failTask(te, msg)
		te.rt.markDirty()
	}

	// 先确保目标目录存在（用户可能在弹框里选了一个还没创建的子目录）
	if err := os.MkdirAll(st.saveDir, 0755); err != nil {
		fail("无法创建目录 " + st.saveDir + ": " + err.Error())
		return
	}

	// 1. fetch 播放列表（若 URL 返回的是 MP4 等直链媒体文件，isDirect=true）
	m3u8Content, baseURL, isDirect, err := job.fetchPlaylist(ctx)
	if err != nil {
		if ctx.Err() != nil {
			finishInterrupt(te)
			return
		}
		fail("获取 m3u8 失败: " + err.Error())
		return
	}

	// 直链文件（MP4 等）：整体流式下载，跳过分片解析
	if isDirect {
		// 输出扩展名跟随 URL（如 .mp4），避免 MP4 内容存成 .ts
		if ext := directExtFromURL(st.m3u8URL); ext != "" && !strings.HasSuffix(strings.ToLower(st.filename), ext) {
			st.filename = strings.TrimSuffix(st.filename, filepath.Ext(st.filename)) + ext
			nf, rerr := reclaimPath(st.saveDir, st.filename, st.finalPath)
			if rerr != nil {
				fail(rerr.Error())
				return
			}
			st.finalPath = nf
			st.filename = filepath.Base(st.finalPath)
			partPath = st.finalPath + ".part"
		}
		te.mu.Lock()
		// 重命名后的文件名/路径同步回任务状态：取消/暂停时按新路径清理 .part
		te.st.filename = st.filename
		te.st.finalPath = st.finalPath
		te.st.stage = "下载直链文件中"
		te.mu.Unlock()
		te.rt.markDirty()
		fmt.Printf("[disk] id=%s 直链文件下载 -> %s\n", st.id, st.finalPath)
		if err := job.downloadDirect(ctx, partPath); err != nil {
			if ctx.Err() != nil {
				finishInterrupt(te)
				return
			}
			fail("下载直链文件失败: " + err.Error())
			return
		}
		if err := moveFile(partPath, st.finalPath); err != nil {
			fail("保存文件失败: " + err.Error())
			return
		}
		te.mu.Lock()
		te.st.running = false
		te.st.done = true
		te.st.paused = false
		te.st.stage = "已保存"
		te.st.filename = st.filename
		te.st.finalPath = st.finalPath
		te.st.errorMsg = ""
		te.st.finished = time.Now()
		te.mu.Unlock()
		te.rt.markDirty()
		return
	}

	// 2. 解析播放列表（分片 / init 段 / ENDLIST → 直播 or 点播）
	pl := parsePlaylist(m3u8Content, baseURL)
	if len(pl.segments) == 0 {
		fail("播放列表中没有找到任何媒体分片（响应可能被加密或压缩）")
		return
	}
	// 播放列表语义校验（中途换 key / 字节范围分片 / 非 identity 密钥格式）：
	// 这些特性按当前实现硬跑都会静默产出损坏文件，宁可在这里失败
	if verr := validatePlaylist(pl); verr != nil {
		fail(verr.Error())
		return
	}
	// 加密流装配解密器（拉取 key 并按 METHOD 建解密器）；失败任务即失败。
	// 续传/重试路径同样会走到这里重建解密器（key 只存于播放列表声明中）。
	if kerr := job.ensureDecryptor(ctx, pl.key); kerr != nil {
		fail(kerr.Error())
		return
	}
	isLive := !pl.hasEndList
	job.encrypted = pl.key != nil
	fmt.Printf("[disk] id=%s segments: %d live=%v\n", st.id, len(pl.segments), isLive)

	// 断点：已写入的分片数（暂停/失败后恢复时从它继续）
	from := int(st.segDone)

	// 直播 + 已有断点：显式拒绝，不硬跑。
	// 直播流没有"接着上次录"这回事——断点期间的分片已经从滑动窗口滚走，
	// 继续追加只会产出时间轴带空洞的产物（fMP4 的 tfdt 前跳、TS 的 PTS 前跳）。
	// 落进这里的有两种任务：探测完成前就被暂停的（当时还不知道是直播），
	// 以及旧状态文件遗留下来的直播断点任务。
	if isLive && from > 0 {
		fail(fmt.Sprintf("直播流不支持从断点继续（已有 %d 个分片）：中间的内容已从列表滚走，"+
			"继续录制会在产物里留下时间轴空洞，请重新开始录制", from))
		return
	}

	// 3. 容器探测（仅全新下载时）：拉取首片 → 识别真实格式 → 修正扩展名 → 取 init 段。
	//    续传（from>0）时格式已定、扩展名已修正、init 已在文件头，全部跳过，
	//    但规范化仍需进行：用持久化的容器 ID 恢复（旧版本任务从 .part 头嗅探）。
	if from == 0 {
		container, cerr := probeContainer(ctx, job, &pl, pl.segments[0])
		if cerr != nil {
			if ctx.Err() != nil {
				finishInterrupt(te)
				return
			}
			fail(cerr.Error())
			return
		}
		// 输出扩展名跟随真实容器（fMP4 内容绝不能存成 .ts）
		if nf := correctExtName(st.filename, container); nf != st.filename {
			st.filename = nf
			np, rerr := reclaimPath(st.saveDir, st.filename, st.finalPath)
			if rerr != nil {
				fail(rerr.Error())
				return
			}
			st.finalPath = np
			st.filename = filepath.Base(st.finalPath)
			partPath = st.finalPath + ".part"
		}
		if werr := writeInitSegmentFor(ctx, job, &pl, partPath); werr != nil {
			if ctx.Err() != nil {
				finishInterrupt(te)
				return
			}
			fail(werr.Error())
			return
		}
		te.mu.Lock()
		te.st.filename = st.filename
		te.st.finalPath = st.finalPath
		te.st.containerID = container.ID
		if job.norm != nil {
			te.st.normState = job.norm.Snapshot()
		}
		te.mu.Unlock()
		te.rt.markDirty()
	} else {
		te.mu.Lock()
		normState := te.st.normState
		te.mu.Unlock()
		if rerr := restoreContainer(job, st.containerID, partPath, normState); rerr != nil {
			fail(rerr.Error())
			return
		}
	}

	// 4. 下载：直播跟随（循环拉取增量追加）vs 点播（一次性并发）
	//
	// job.live 与 st.live 必须在同一个临界区里写：st.live 是 /status 与前端
	// 分流的唯一判据。此前只设了 job.live，任务状态里的 live 恒为 false（P1-4）
	// ——前端的「录制中」徽章、无限进度条、录制时长分支全是死代码，
	// 直播被完全按点播渲染。
	//
	// 直播去重状态不跨会话恢复：B0 之后直播只有「停止」「取消」两态，
	// 不存在"接着上次录"（暂停期间的分片已从滑动窗口滚走）。
	te.mu.Lock()
	job.live = isLive
	te.st.live = isLive
	te.mu.Unlock()

	var next int
	if isLive {
		next, err = job.liveDownload(ctx, partPath, from)
	} else if from >= len(pl.segments) {
		// 断点已达/超过列表总数（含旧版本重复续传遗留的脏断点）：分片已齐，直接收尾。
		// streamDownload 未运行，job 计数需手动同步（snapshot 的进度读 job 原子值）
		next = from
		job.setSeg(int64(len(pl.segments)), int64(len(pl.segments)))
		// 断点同样要落盘值语义：下面 306 行会持久化 segFlushedNow 作为下次的 from
		job.setSegFlushed(int64(from))
	} else {
		// 点播续传只下剩余分片：segURLs[0] 对应写入序号 from，重复传全量会把
		// 整个列表重下一遍追加到断点后（内容重复 + segDone 超过 segTot）
		next, err = streamDownload(ctx, job, pl.segments[from:], from, pl.mediaSeq+uint64(from), partPath)
	}

	te.mu.Lock()
	// 持久化的断点必须是"确实落盘"的位置：212 行在下次启动时直接拿它当续传起点
	// from。next 是内存进度，可能领先磁盘（streamDownload 关闭时最终 Flush 失败，
	// 那条路径已经改用 Flushed 并回错误）。用 next 会让重启后从空洞之后继续
	// append —— 缺口永久留在产物里，而状态与日志一切正常（R9）。
	te.st.segDone = job.segFlushedNow()
	if isLive {
		te.st.segTot = 0 // 直播列表无限增长，进度由前端按录制时长展示
	} else {
		te.st.segTot = int64(len(pl.segments))
	}
	te.mu.Unlock()

	// 内存进度与落盘断点不等 = 尾部有分片没进文件（缓冲丢失或关闭时 Flush 失败）。
	// 正常情况下两者相等；不等就是上面那条断点回退条款在起作用，必须让它可见：
	// 少了的那几片永远补不回来（窗口滚走后），用户该知道产物短了。
	if flushed := job.segFlushedNow(); int(flushed) < next {
		fmt.Printf("[disk] WARN: id=%s 尾部 %d 片未落盘（内存进度 %d，落盘断点 %d）\n",
			st.id, int64(next)-flushed, next, flushed)
	}

	if err != nil {
		if ctx.Err() != nil {
			finishInterrupt(te)
			return
		}
		msg := "下载分片失败: " + err.Error()
		// 直播录制因故障终止（源站挂掉、列表拉取失败、响应变直链、key 轮换…）：
		// 已录部分自动收尾成正式文件，不停在半成品上等用户处理
		// （学徒 2026-09-15 定：直播中断肯定要自动收尾）。
		//
		// 三条边界：
		//   - 只救"已经写进 .part 的"内容；一片都没落盘时没有可保存的东西，
		//     硬保存只会给用户一个播不出画面的空壳；
		//   - 用户主动停止/取消不归这里管（ctx.Err() 分支走 finishInterrupt）；
		//   - 收尾内的抽样校验不通过时 finalizeRecording 返回 error，落回 failTask
		//     ——不能因为是直播就把坏数据当成品（那又是"产物坏了但日志正常"）。
		if job.live && job.segFlushedNow() > 0 {
			fmt.Printf("[disk] LIVE-FAIL: %s\n", msg)
			// reason 直接给 msg：stage 已经是「录制中断 · 已保存」，再加"录制中断:"
			// 前缀会让界面读成"录制中断 · 已保存（录制中断: xx）"。
			if serr := finalizeRecording(te, job, partPath, finalizeInterrupted, msg); serr == nil {
				return
			}
		}
		fail(msg)
		return
	}

	// 直播的「停止」/退出走这里：liveDownload 把 ctx 取消视作干净结束
	// （返回 nil error，见其末尾的注释），若不拦一下就会落到下面"正常完成"
	// 的收尾里——用户主动停止的录制会被显示成一次完整录完。
	if ctx.Err() != nil {
		finishInterrupt(te)
		return
	}

	// 5-6. 抽样校验 → 容器收尾 → .part 改名为正式文件。
	//      与「停止」「中断」路径共用同一个收尾函数：收尾代码一旦分叉，
	//      两条路的产物规则就再也对不齐了。
	if ferr := finalizeRecording(te, job, partPath, finalizeComplete, ""); ferr != nil {
		fail(ferr.Error())
		return
	}
}

// finalizeOutcome 是收尾的起因，决定两件事：产物校验失败时是否保留 .part、
// 以及对外显示成「已完成」还是「已中断」。
//
// 为什么不是一个 interrupted bool：这两件事的取值并不总是一致。用户主动点
// 「停止」不该显示成"中断"（那是用户自己结束的），但它同样必须保留 .part——
// 验不过就删等于把用户录的东西销毁掉。用一个 bool 会逼出"要么误删、要么误标"。
type finalizeOutcome int

const (
	finalizeComplete    finalizeOutcome = iota // 正常下载/录制到自然结束
	finalizeStopped                            // 用户主动停止（他认为录到此为止）
	finalizeInterrupted                        // 非用户意愿中断（拉流失败、程序退出补偿）
)

// keepsPartOnInvalid 报告产物抽样校验失败时是否必须保留 .part。
// 只有"正常走完"才允许丢弃损坏的临时文件；其余路径那份半成品可能是用户仅有的东西。
func (o finalizeOutcome) keepsPartOnInvalid() bool { return o != finalizeComplete }

// interrupted 报告这条收尾要不要对外标成「已中断」。
func (o finalizeOutcome) interrupted() bool { return o == finalizeInterrupted }

// finalizeRecording 把 .part 收尾成正式文件：抽样校验 → 容器收尾（fMP4 回填
// 总时长 mehd/mvhd）→ 改名成成品 → 置任务终态。
//
// 三条路径共用（正常完成 / 用户停止 / 故障中断），差别只在 outcome。
//
// interrupted 那一条有两条硬约束，缺一条就是把缺陷藏起来：
//   - stage 必须与"完整录制"可区分（"录制中断 · 已保存"）；
//   - errorMsg 必须保留原因、不能清空——本项目头号缺陷形态就是
//     "产物不完整但日志与状态显示一切正常"。
func finalizeRecording(te *taskEntry, job *dlJob, partPath string, outcome finalizeOutcome, reason string) error {
	interrupted := outcome.interrupted()
	te.mu.Lock()
	finalPath, id, segTot := te.st.finalPath, te.st.id, te.st.segTot
	te.mu.Unlock()

	// 落盘前抽样校验：探测期守卫只看首片，这里对成品头/中段再确认一次。
	// 直播窗口大小不代表总量，体量合理性检查不适用（valSegs=0）。
	valSegs := 0
	if !job.live {
		valSegs = int(segTot)
	}
	if verr := validateOutput(job.container, partPath, ProbeInfo{
		Encrypted: job.encrypted,
		Segments:  valSegs,
		MinBytes:  int64(job.initLen),
	}); verr != nil {
		if outcome.keepsPartOnInvalid() {
			// 用户停止 / 故障中断：保留 .part，那可能是用户仅有的半成品，交给他自己处理
			return fmt.Errorf("%w（临时文件保留在 %s）", verr, partPath)
		}
		// 正常路径判定损坏就直接丢弃：重试只会重下出同样的坏数据，
		// 留着它只会让"重试"反复失败并占着断点。
		os.Remove(partPath)
		os.Remove(partPath + ".meta")
		return fmt.Errorf("%w（已丢弃损坏的临时文件）", verr)
	}

	if jerr := job.backfill(partPath); jerr != nil {
		fmt.Printf("[disk] WARN: 容器收尾处理失败: %v\n", jerr)
	}
	if err := moveFile(partPath, finalPath); err != nil {
		return fmt.Errorf("保存文件失败: %w", err)
	}
	if interrupted {
		fmt.Printf("[disk] id=%s 已保存（录制中断: %s）-> %s\n", id, reason, finalPath)
	} else if outcome == finalizeStopped {
		fmt.Printf("[disk] id=%s 已保存（用户停止录制）-> %s\n", id, finalPath)
	} else {
		fmt.Printf("[disk] id=%s 已保存 -> %s\n", id, finalPath)
	}

	te.mu.Lock()
	te.st.running = false
	te.st.queued = false
	te.st.paused = false
	te.st.canceled = false
	te.st.done = true
	te.st.finalPath = finalPath
	te.st.finished = time.Now()
	// 点播收尾把进度对齐总数；直播没有总数（segTot 恒 0），保留已落盘片数
	// ——否则界面上显示"已录制 0 片"（P2-2）。
	if segTot > 0 {
		te.st.segDone = segTot
	} else {
		te.st.segDone = job.segFlushedNow()
	}
	if interrupted {
		te.st.stage = "录制中断 · 已保存"
		te.st.errorMsg = reason
	} else if outcome == finalizeStopped {
		// 用户主动点「停止」：这是"录到这里收工"，不是故障。stage 写清是谁结束的，
		// errorMsg 必须留空——否则前端的"失败"统计会把它算进去（学徒 2026-09-15 定）。
		te.st.stage = "已保存（用户停止录制）"
		te.st.errorMsg = ""
	} else {
		te.st.stage = "已保存"
		te.st.errorMsg = ""
	}
	// interrupted 标记决定前端把这条记录显示成「已中断」还是「已完成」
	// （用户停止算完成：是他自己叫停的，不是出了故障）。
	// 缺口时长如实带上：滑动窗口滚走的分片补不回来，产物时间轴上那段确实是空的。
	te.st.interrupted = interrupted
	te.st.gapSeconds = job.gapSecondsNow()
	te.mu.Unlock()
	te.rt.markDirty()
	return nil
}

// finishInterrupt 处理"下载被 ctx 中断"的收尾：按用户意图标记暂停、停止或取消。
// 暂停保留 .part 和断点；停止把已录部分收尾成正式文件；取消删除 .part，任务不可恢复。

func finishInterrupt(te *taskEntry) {
	te.mu.Lock()
	intent := te.intent
	part := te.st.finalPath + ".part"
	id := te.st.id
	segDone := te.st.segDone
	te.st.running = false
	te.st.queued = false
	switch intent {
	case intentCancel:
		te.st.canceled = true
		te.st.done = true
		te.st.paused = false
		te.st.stage = "已取消"
		te.st.errorMsg = ""
		te.st.finalPath = "" // 已取消：没有任何成品文件，清掉避免"打开文件"指向不存在的路径
	case intentStop:
		// 直播停止：终态由 finishStop 按"有没有录到内容"决定
		te.st.paused = false
	default:
		te.st.paused = true
		te.st.stage = "已暂停"
		te.st.finished = time.Now()
	}
	te.mu.Unlock()

	switch intent {
	case intentCancel:
		if part != "" {
			if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
				fmt.Printf("[disk] WARN: 删除临时文件失败 %s: %v\n", part, err)
			}
			if err := os.Remove(part + ".meta"); err != nil && !os.IsNotExist(err) {
				fmt.Printf("[disk] WARN: 删除分片位图失败 %s: %v\n", part+".meta", err)
			}
		}
		fmt.Printf("[disk] id=%s 已取消\n", id)
	case intentStop:
		finishStop(te, id, part, segDone, finalizeStopped, "用户停止录制")
	default:
		// 暂停时也做容器收尾处理（fMP4 回填已录部分的总时长，方便直接预览/拖动）；
		// 续传完成后会以新总时长再次回填
		if j := te.jobRef(); j != nil {
			if err := j.backfill(part); err != nil {
				fmt.Printf("[disk] WARN: 容器收尾处理失败: %v\n", err)
			}
		}
		fmt.Printf("[disk] id=%s 已暂停，断点 %d\n", id, segDone)
	}
	te.rt.markDirty()
}

// finishStoppedTask 收尾一条已不在运行（失败或中断后静止）的直播任务。
// pipeline 已经退出，没有 ctx 可取消，直接按现有 .part 走收尾；
// 由 /stop 在"任务未运行"时排到 goroutine 里执行。
//
// 与 finishInterrupt 的 intentStop 分支有个关键区别：那条路上任务还在录，
// 是用户自己叫停的，算「已完成」；这里任务早就停了（拉流失败 / 程序退出），
// 用户点的「停止」只是"把已录部分保存下来"，中断原因不是他造成的，
// 所以保持「已中断」并沿用原原因——把它改写成"已完成"等于抹掉故障记录。
func finishStoppedTask(te *taskEntry) {
	te.mu.Lock()
	part := te.st.finalPath + ".part"
	id := te.st.id
	segDone := te.st.segDone
	reason := te.st.errorMsg
	te.mu.Unlock()
	if reason == "" {
		// 状态文件里的直播任务不带 errorMsg（中断原因是"上次程序退出"这件事本身）
		reason = "程序退出"
	}
	finishStop(te, id, part, segDone, finalizeInterrupted, reason)
}

// reopenJobForFinalize 为「没有 job 的任务」重建收尾所需的 dlJob。
//
// 正常运行时 job 由 pipeline 创建并挂在任务上；进程被强杀后重启，任务只剩
// 持久化字节。此时若直接拿 `.part` 收尾，容器与跨分片规范化状态都会缺席——
// fMP4 的 mehd/mvhd 回填整段跳过，产物总时长是 0（播放器拖不动、看不出录了多久）。
// 所以从 containerID / normState / `.part` 头部把这两样恢复回来。
//
// 返回 nil 表示"没有可收尾的内容"（finalPath 为空、或 `.part` 不存在/为空）。
func reopenJobForFinalize(te *taskEntry) *dlJob {
	te.mu.Lock()
	id, finalPath := te.st.id, te.st.finalPath
	segDone, segTot := te.st.segDone, te.st.segTot
	live, containerID := te.st.live, te.st.containerID
	filename, saveDir := te.st.filename, te.st.saveDir
	normState := te.st.normState
	te.mu.Unlock()

	if finalPath == "" && filename != "" {
		// 兼容旧版状态文件（曾有版本把失败任务的 finalPath 清空）：按
		// filename+saveDir 拼回，否则 .part 找不到，用户录的东西就成了磁盘上
		// 无人认领的孤儿。与 pipeline 启动时的兜底同一套规则。
		finalPath = filepath.Join(saveDir, filename)
	}
	if finalPath == "" {
		return nil
	}
	part := finalPath + ".part"
	// 0 字节的 .part 是 uniquePath 留下的占位（认领文件名），不是半成品
	if !nonEmptyFile(part) {
		return nil
	}

	job := &dlJob{rt: te.rt, id: id, live: live}
	// 进度计数一并对齐已录片数：snapshot 优先读 job 的原子值，
	// 不设就会把已录片数显示成 0。
	job.setSeg(segDone, segTot)
	job.setSegFlushed(segDone)
	// 收尾路径**有意忽略** restoreContainer 的错误：用户要的是把已录内容保住，
	// 状态不可用只影响时间轴刻度（mehd 回填），不该让他丢掉整份录像。
	// 续传路径（runDownload）才是必须拒绝的地方。
	_ = restoreContainer(job, containerID, part, normState)
	return job
}

// finishStop 收尾一条被「停止」的直播录制。
//
// 有已落盘分片就保存成正式文件；一片都没录到则按无内容处理（清掉临时文件），
// 否则用户会拿到一个只有 init 段、播不出画面的空壳。
//
// outcome 由调用方给：还在录时停止 = finalizeStopped（用户主动收工），
// 已经静止时停止 = finalizeInterrupted（沿用原中断原因），见 finishStoppedTask。
func finishStop(te *taskEntry, id, part string, segDone int64, outcome finalizeOutcome, reason string) {
	// 启动补偿收尾可能正在收尾同一个（重启后恢复出来的）任务，见 finalizeMu。
	te.finalizeMu.Lock()
	defer te.finalizeMu.Unlock()

	// 拿到锁后必须重新确认终态：等锁期间对方可能已经收尾完了。
	// 不复检的后果不是"白做一遍"，而是把结果改坏——.part 已被改名，下一轮的
	// 抽样校验读不到文件头直接放行，直到 moveFile 才失败，一条"已保存"的
	// 记录被改写成「失败」，用户看到文件失败、实际文件好好地躺在磁盘上。
	if snapshot(te).done {
		return
	}

	job := te.jobRef()
	if job == nil {
		// 没有 job 不等于没有内容：重启后恢复出来的直播任务正是这个形态
		// （pipeline 早已不在，job 为空但 .part 里有用户录到的东西）。
		// 不重建就直接走下面的"无内容"分支，会把 .part 删掉——那是把用户
		// 录的东西销毁掉，本项目红线。
		job = reopenJobForFinalize(te)
	}
	if job == nil || job.segFlushedNow() == 0 {
		if part != "" {
			if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
				fmt.Printf("[disk] WARN: 删除临时文件失败 %s: %v\n", part, err)
			}
			if err := os.Remove(part + ".meta"); err != nil && !os.IsNotExist(err) {
				fmt.Printf("[disk] WARN: 删除分片位图失败 %s: %v\n", part+".meta", err)
			}
		}
		te.mu.Lock()
		te.st.done = true
		te.st.paused = false
		te.st.canceled = false
		te.st.stage = "已停止（无内容）"
		te.st.finalPath = ""
		te.st.errorMsg = ""
		te.st.finished = time.Now()
		te.mu.Unlock()
		fmt.Printf("[disk] id=%s 已停止（无内容可保存）\n", id)
		return
	}
	if err := finalizeRecording(te, job, part, outcome, reason); err != nil {
		fmt.Printf("[disk] FAIL: 停止收尾失败: %v\n", err)
		failTask(te, err.Error())
		return
	}
	fmt.Printf("[disk] id=%s 已停止并保存（已录 %d 片）\n", id, segDone)
}

// ============================================================
// 容器装配（GUI 任务与 CLI 共用）
// ------------------------------------------------------------
// 这两条路径原先各自实现了一遍「探测首片 → 识别容器 → 写 init 段」，
// 差异直接变成了产物差异：CLI 从不给 job.container / job.norm 赋值，
// 于是同一份 fMP4 源在 CLI 下不做分片时间戳归一化、不写 mehd 占位、
// 收尾也不回填总时长。统一到这里，两边行为由同一段代码决定。
// ============================================================

// probeContainer 拉取首个分片 → 解密 → 识别容器 → 执行探测期守卫。
// 成功后 job.container / job.norm / job.pre / job.preURL 均已就位。
// 返回的错误已带上下文；调用方需先查 ctx.Err() 以区分「用户中断」与真实失败。
func probeContainer(ctx context.Context, job *dlJob, pl *playlistInfo, firstSegURL string) (*Container, error) {
	pre, err := fetchSegment(ctx, job, firstSegURL)
	if err != nil {
		return nil, fmt.Errorf("探测视频格式失败（首个分片）: %w", err)
	}
	// 加密流：探测分片先解密（容器魔数在密文上看不出，识别必须基于明文）
	if pre, err = job.decryptSegmentIfAny(pl.mediaSeq, pre); err != nil {
		return nil, fmt.Errorf("解密首个分片失败: %w", err)
	}
	job.pre = pre
	job.preURL = firstSegURL

	container := detectContainer(pre, pl.hasMap)
	// 探测期守卫（含加密流误判）：尽早失败，不要下完几个 GB 才发现存的是密文
	if err := guardGenericMedia(container, pl.key, pre); err != nil {
		return nil, err
	}
	job.container = container
	// 规范化状态由容器工厂创建（generic 等无状态容器为 nil）
	if container.NewState != nil {
		job.norm = container.NewState()
	}
	return container, nil
}

// writeInitSegmentFor 按容器策略拉取 #EXT-X-MAP 初始化段，规范化后写入 .part 头。
// 非 fMP4(#EXT-X-MAP) 容器为空操作；重复调用安全（.part 已有内容时不写）。
func writeInitSegmentFor(ctx context.Context, job *dlJob, pl *playlistInfo, partPath string) error {
	c := job.container
	if c == nil || c.Init != InitFromMap {
		return nil
	}
	if pl.mapURI == "" {
		return errors.New("识别为 fMP4 分片流，但播放列表没有 #EXT-X-MAP 初始化段，无法生成可播放文件")
	}
	initData, err := fetchSegment(ctx, job, pl.mapURI)
	if err != nil {
		return fmt.Errorf("获取 fMP4 init 段失败: %w", err)
	}
	nd := initData
	if job.norm != nil {
		// 补 mehd 占位并解析轨道类型与回填位置（幂等）
		if nd, err = job.norm.Normalize(initData); err != nil {
			return fmt.Errorf("处理 fMP4 init 段失败: %w", err)
		}
	}
	if err := writeInitSegment(partPath, nd); err != nil {
		return fmt.Errorf("写入 init 段失败: %w", err)
	}
	job.initLen = len(nd)
	return nil
}

// restoreContainer 断点续传（from>0）时恢复格式相关行为：
// 优先按持久化的容器 ID 找回条目，旧版本任务回退为从 .part 文件头嗅探；
// 随后恢复跨分片规范化状态。
//
// 返回错误表示「续传不安全」：容器依赖跨分片状态（fMP4 的 tfdt 基准，判据是
// NewState != nil），而状态没能恢复。此时新分片会从头计时、与已录内容在时间轴上
// 重叠 —— 产物能播、内容是错的，属本项目头号缺陷形态，所以显式拒绝而不硬跑。
// 无状态容器（TS 等）恒返回 nil：它们续传本来就不依赖外部状态。
//
// 落到"状态不可用"的两种情形：旧版本任务文件里没有状态字节；字节版本不符或损坏
// （快照版本号见 fmp4.normPersistVersion）。从 .part 重建 fMP4 基准需要扫全文件，
// 不在本轮范围 —— 让用户重新开始，好过悄悄拼出坏时间轴。
func restoreContainer(job *dlJob, containerID, partPath string, normState []byte) error {
	if c := findContainerByID(containerID); c != nil {
		job.container = c
		if c.NewState != nil {
			job.norm = c.NewState()
		}
	} else if head, err := readHead(partPath, 4096); err == nil && len(head) > 0 {
		// 嗅探必须 onMagic=true 等价地"只看魔数"，不能假设播放列表有 #EXT-X-MAP：
		// 这里的入参是磁盘上的半成品，拿不到播放列表。传 hasMap=true 会让
		// **任何**开头不含 ftyp 的内容都被判成 fmp4-map —— 一个 .ts 半成品
		// 会被 fMP4 校验器判成"内容不符合容器特征"，续传与收尾都白白失败。
		// 反过来，fMP4 的 .part 头部就是 init 段（必带 ftyp），按魔数同样认得出来。
		job.container = detectContainer(head, false)
		if job.container.NewState != nil {
			job.norm = job.container.NewState()
		}
	}
	if job.norm == nil {
		return nil // 无状态容器：续传不依赖跨分片状态
	}
	// 空字节、版本不符、损坏都归这一处：Restore 全返回 false。
	// 不再单列"字节为空"的分支 —— 它与 Restore(nil)=false 等价，
	// 多一条路径只是多一处可能写错的地方（反向验证抓到过）。
	if !job.norm.Restore(normState) {
		return fmt.Errorf("断点续传状态不可用（旧版本任务文件、版本不符或已损坏），" +
			"继续追加会让新分片与已录内容在时间轴上重叠，请重新开始下载")
	}
	return nil
}

// correctExtName 按容器输出扩展名修正文件名（fMP4 内容绝不能存成 .ts）。
// 无需修正时原样返回。
func correctExtName(fname string, c *Container) string {
	if c == nil || c.Ext == "" || strings.HasSuffix(strings.ToLower(fname), c.Ext) {
		return fname
	}
	return strings.TrimSuffix(fname, filepath.Ext(fname)) + c.Ext
}

// readHead 读取文件开头最多 n 字节（续传时嗅探 .part 文件头识别容器）。
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	m, err := f.Read(b)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return b[:m], nil
}

// ============================================================
// 任务持久化
// 任务列表写入 exe 同目录的 gocatcher_state.json，
// 服务重启后能恢复历史任务（含未完成的，恢复后为「已暂停」可继续）。
// 写入用「临时文件 + rename」保证原子性，避免写一半崩掉留下坏文件。
// ============================================================
