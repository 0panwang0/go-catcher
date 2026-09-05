// 单任务下载管线与暂停/取消收尾。
package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runDiskPipeline(te *taskEntry) {
	acquired := false
	defer func() {
		// 释放并发槽，让排队的下一个任务进场
		if acquired {
			limiter.release()
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
	markDirty()

	// 等待并发槽位（超过当前上限的新任务在此排队；队列中可被暂停/取消打断）
	if !limiter.acquire(ctx) {
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
	te.mu.Unlock()
	markDirty()

	job := &dlJob{
		id:      st.id,
		m3u8URL: st.m3u8URL,
		referer: st.referer,
		saveDir: st.saveDir,
		fname:   st.filename,
	}
	te.mu.Lock()
	te.job = job
	te.mu.Unlock()

	// 进度回调：每写入一批分片就刷新任务状态（持久化时以它为准）
	job.progress = func(stage string, done, tot int64) {
		te.mu.Lock()
		te.st.stage = stage
		te.st.segDone = done
		te.st.segTot = tot
		te.mu.Unlock()
	}

	// 规范化状态由容器工厂创建（见下方容器探测分支），此处仅声明回调。
	// 分片写入前的规范化回调：时间戳归一化 + NAL 封装转换（仅 fMP4 容器生效），
	// 内联 init（首片自带 ftyp/moov）在此补 mehd 占位；
	// 并把基准/结束时间/init 信息同步回任务状态（暂停/重启后续传仍沿用）
	job.normFn = func(d []byte) []byte {
		if job.container == nil || job.container.Normalize == nil || job.norm == nil {
			return d
		}
		// 含 moov 的分片（无 #EXT-X-MAP 时 init 内联在首片）→ 补 mehd 占位并记录回填位置；
		// 同时解析轨道类型，仅视频轨做 NAL→AVCC 转换（音频轨特征与 NAL 头有冲突，不可猜测）
		if normInitInfo(job.norm) == nil {
			nd2, info := prepareInit(d, true)
			if info.mehdOff >= 0 {
				d = nd2
				normSetInit(job.norm, info)
			}
			if len(info.trackVideo) > 0 {
				applyTrackVideo(job.norm, info.trackVideo)
			}
		}
		nd, err := job.container.Normalize(d, job.norm)
		if err != nil {
			fmt.Printf("[norm] 分片规范化失败（原样写入）: %v\n", err)
			return d
		}
		if len(nd) != len(d) {
			fmt.Printf("[norm] 分片规范化: %d → %d 字节\n", len(d), len(nd))
		}
		te.mu.Lock()
		te.st.fmp4Baseline = job.norm.snapshot()
		te.st.fmp4End = job.norm.endSnapshot()
		if info := normInitInfo(job.norm); info != nil {
			te.st.fmp4Init = info
		}
		te.mu.Unlock()
		return nd
	}

	fail := func(msg string) {
		fmt.Printf("[disk] FAIL: %s\n", msg)
		failTask(te, msg)
		markDirty()
	}

	// 先确保目标目录存在（用户可能在弹框里选了一个还没创建的子目录）
	if err := os.MkdirAll(st.saveDir, 0755); err != nil {
		fail("无法创建目录 " + st.saveDir + ": " + err.Error())
		return
	}

	// 1. fetch 播放列表（若 URL 返回的是 MP4 等直链媒体文件，isDirect=true）
	m3u8Content, baseURL, isDirect, err := job.fetchPlaylist()
	if err != nil {
		if ctx.Err() != nil {
			finishInterrupt(te)
			return
		}
		fail("获取 m3u8 失败: " + err.Error())
		return
	}

	// 下载期间写 .part，写完后改名成正式文件（避免半成品被当成成品）
	partPath := st.finalPath + ".part"

	// 直链文件（MP4 等）：整体流式下载，跳过分片解析
	if isDirect {
		// 输出扩展名跟随 URL（如 .mp4），避免 MP4 内容存成 .ts
		if ext := directExtFromURL(st.m3u8URL); ext != "" && !strings.HasSuffix(strings.ToLower(st.filename), ext) {
			st.filename = strings.TrimSuffix(st.filename, filepath.Ext(st.filename)) + ext
			st.finalPath = uniquePath(filepath.Join(st.saveDir, st.filename))
			st.filename = filepath.Base(st.finalPath)
			partPath = st.finalPath + ".part"
		}
		te.mu.Lock()
		// 重命名后的文件名/路径同步回任务状态：取消/暂停时按新路径清理 .part
		te.st.filename = st.filename
		te.st.finalPath = st.finalPath
		te.st.stage = "下载直链文件中"
		te.mu.Unlock()
		markDirty()
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
		markDirty()
		return
	}

	// 2. 解析播放列表（分片 / init 段 / ENDLIST → 直播 or 点播）
	pl := parsePlaylist(m3u8Content, baseURL)
	if len(pl.segments) == 0 {
		fail("播放列表中没有找到任何媒体分片（响应可能被加密或压缩）")
		return
	}
	isLive := !pl.hasEndList
	fmt.Printf("[disk] id=%s segments: %d live=%v\n", st.id, len(pl.segments), isLive)

	// 断点：已写入的分片数（暂停/失败后恢复时从它继续）
	from := int(st.segDone)

	// 3. 容器探测（仅全新下载时）：拉取首片 → 识别真实格式 → 修正扩展名 → 取 init 段。
	//    续传（from>0）时格式已定、扩展名已修正、init 已在文件头，全部跳过，
	//    但规范化仍需进行：用持久化的容器 ID 恢复（旧版本任务从 .part 头嗅探）。
	if from == 0 {
		pre, perr := fetchSegment(ctx, job, pl.segments[0])
		if perr != nil {
			if ctx.Err() != nil {
				finishInterrupt(te)
				return
			}
			fail("探测视频格式失败（首个分片）: " + perr.Error())
			return
		}
		job.pre = pre

		container := detectContainer(pre, pl.hasMap)
		job.container = container
		// 规范化状态由容器工厂创建（generic 等无状态容器为 nil）
		if container.NewState != nil {
			job.norm = container.NewState()
		}
		// 输出扩展名跟随真实容器（fMP4 内容绝不能存成 .ts）
		if container.Ext != "" && !strings.HasSuffix(strings.ToLower(st.filename), container.Ext) {
			st.filename = strings.TrimSuffix(st.filename, filepath.Ext(st.filename)) + container.Ext
			st.finalPath = uniquePath(filepath.Join(st.saveDir, st.filename))
			st.filename = filepath.Base(st.finalPath)
			partPath = st.finalPath + ".part"
		}
		if container.Init == InitFromMap {
			if pl.mapURI == "" {
				fail("识别为 fMP4 分片流，但播放列表没有 #EXT-X-MAP 初始化段，无法生成可播放文件")
				return
			}
			initData, ferr := fetchSegment(ctx, job, pl.mapURI)
			if ferr != nil {
				if ctx.Err() != nil {
					finishInterrupt(te)
					return
				}
				fail("获取 fMP4 init 段失败: " + ferr.Error())
				return
			}
			// 补 mehd 占位（fMP4 总时长声明）并记录回填位置与 timescale；
			// 同时解析轨道类型，仅视频轨做 NAL→AVCC 转换
			initData, info := prepareInit(initData, true)
			normSetInit(job.norm, info)
			if len(info.trackVideo) > 0 {
				applyTrackVideo(job.norm, info.trackVideo)
			}
			if werr := writeInitSegment(partPath, initData); werr != nil {
				fail("写入 init 段失败: " + werr.Error())
				return
			}
		}
		te.mu.Lock()
		te.st.filename = st.filename
		te.st.finalPath = st.finalPath
		te.st.containerID = container.ID
		if info := normInitInfo(job.norm); info != nil {
			te.st.fmp4Init = info
		}
		te.mu.Unlock()
		markDirty()
	} else {
		if c := findContainerByID(st.containerID); c != nil {
			job.container = c
			if c.NewState != nil {
				job.norm = c.NewState()
			}
		} else if head, herr := readHead(partPath, 4096); herr == nil && len(head) > 0 {
			job.container = detectContainer(head, true) // 旧版本任务：从 .part 文件头嗅探
			if job.container.NewState != nil {
				job.norm = job.container.NewState()
			}
		}
		// fMP4 续传：恢复 mehd 回填所需信息；旧版本任务（未持久化 init 信息）
		// 回退为从 .part 文件头重新解析（mehd 占位已在首次运行时写入）。
		// 只有声明了 Backfill（有收尾回填需求）的容器才需要 init 信息。
		if job.container != nil && job.container.Backfill != nil && normInitInfo(job.norm) == nil {
			if info := te.st.fmp4Init; info != nil {
				normSetInit(job.norm, info)
			} else if head, herr := readHead(partPath, 64<<10); herr == nil && len(head) > 8 {
				_, info := prepareInit(head, false)
				normSetInit(job.norm, info)
			}
		}
		if info := normInitInfo(job.norm); info != nil && len(info.trackVideo) > 0 {
			applyTrackVideo(job.norm, info.trackVideo)
		}
	}

	// 规范化状态统一恢复持久化的跨分片状态（tfdt 基准 + 每轨结束时间，断点续传沿用）
	if job.norm != nil {
		if len(st.fmp4Baseline) > 0 {
			job.norm.restore(st.fmp4Baseline)
		}
		if len(st.fmp4End) > 0 {
			job.norm.restoreEnd(st.fmp4End)
		}
	}

	// 4. 下载：直播跟随（循环拉取增量追加）vs 点播（一次性并发）
	te.mu.Lock()
	job.live = isLive
	if isLive {
		job.seen = make(map[string]bool)
		for _, u := range st.seen {
			job.seenAdd(u) // 断点恢复：跳过已录制分片
		}
	}
	te.mu.Unlock()

	var next int
	if isLive {
		next, err = job.liveDownload(ctx, partPath, from)
	} else {
		next, err = streamDownload(ctx, job, pl.segments, from, partPath)
	}

	te.mu.Lock()
	te.st.segDone = int64(next)
	if isLive {
		te.st.segTot = 0 // 直播列表无限增长，进度由前端按录制时长展示
	} else {
		te.st.segTot = int64(len(pl.segments))
	}
	te.mu.Unlock()

	if err != nil {
		if ctx.Err() != nil {
			finishInterrupt(te)
			return
		}
		fail("下载分片失败: " + err.Error())
		return
	}

	// 5. 落盘：先做容器收尾处理（fMP4 回填总时长 mehd/mvhd），再 .part → 正式文件
	if jerr := job.backfill(partPath); jerr != nil {
		fmt.Printf("[disk] WARN: 容器收尾处理失败: %v\n", jerr)
	}
	if err := moveFile(partPath, st.finalPath); err != nil {
		fail("保存文件失败: " + err.Error())
		return
	}
	fmt.Printf("[disk] id=%s 已保存 -> %s\n", st.id, st.finalPath)
	te.mu.Lock()
	te.st.running = false
	te.st.done = true
	te.st.paused = false
	te.st.stage = "已保存"
	te.st.finalPath = st.finalPath
	te.st.errorMsg = ""
	te.st.segDone = te.st.segTot
	te.st.finished = time.Now()
	te.mu.Unlock()
	markDirty()
}

// finishInterrupt 处理"下载被 ctx 中断"的收尾：按用户意图标记暂停或取消。
// 暂停保留 .part 和断点；取消删除 .part，任务不可恢复。

func finishInterrupt(te *taskEntry) {
	te.mu.Lock()
	intent := te.intent
	part := te.st.finalPath + ".part"
	id := te.st.id
	segDone := te.st.segDone
	te.st.running = false
	te.st.queued = false
	if intent == intentCancel {
		te.st.canceled = true
		te.st.done = true
		te.st.paused = false
		te.st.stage = "已取消"
		te.st.errorMsg = ""
		te.st.finalPath = "" // 已取消：没有任何成品文件，清掉避免"打开文件"指向不存在的路径
	} else {
		te.st.paused = true
		te.st.stage = "已暂停"
		te.st.finished = time.Now()
	}
	te.mu.Unlock()

	if intent == intentCancel {
		if part != "" {
			if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
				fmt.Printf("[disk] WARN: 删除临时文件失败 %s: %v\n", part, err)
			}
			if err := os.Remove(part + ".meta"); err != nil && !os.IsNotExist(err) {
				fmt.Printf("[disk] WARN: 删除分片位图失败 %s: %v\n", part+".meta", err)
			}
		}
		fmt.Printf("[disk] id=%s 已取消\n", id)
	} else {
		// 暂停时也做容器收尾处理（fMP4 回填已录部分的总时长，方便直接预览/拖动）；
		// 续传完成后会以新总时长再次回填
		if j := te.job; j != nil {
			if err := j.backfill(part); err != nil {
				fmt.Printf("[disk] WARN: 容器收尾处理失败: %v\n", err)
			}
		}
		fmt.Printf("[disk] id=%s 已暂停，断点 %d\n", id, segDone)
	}
	markDirty()
}

// uniquePath 若 path 已存在则返回 "name (1).ext"、"name (2).ext"… 直到不冲突

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
