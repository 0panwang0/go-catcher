// 单任务下载管线与暂停/取消收尾。
package core

import (
	"context"
	"errors"
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
	// 中途换 key 的流按当前实现会解错前面的分片：宁可失败也不产出损坏文件
	if kerr := ensureSingleKey(pl); kerr != nil {
		fail(kerr.Error())
		return
	}
	// 加密流装配解密器（拉取 key 并按 METHOD 建解密器）；失败任务即失败。
	// 续传/重试路径同样会走到这里重建解密器（key 只存于播放列表声明中）。
	if kerr := job.ensureDecryptor(ctx, pl.key); kerr != nil {
		fail(kerr.Error())
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
			st.finalPath = uniquePath(filepath.Join(st.saveDir, st.filename))
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
		restoreContainer(job, st.containerID, partPath, normState)
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
	} else if from >= len(pl.segments) {
		// 断点已达/超过列表总数（含旧版本重复续传遗留的脏断点）：分片已齐，直接收尾。
		// streamDownload 未运行，job 计数需手动同步（snapshot 的进度读 job 原子值）
		next = from
		job.setSeg(int64(len(pl.segments)), int64(len(pl.segments)))
	} else {
		// 点播续传只下剩余分片：segURLs[0] 对应写入序号 from，重复传全量会把
		// 整个列表重下一遍追加到断点后（内容重复 + segDone 超过 segTot）
		next, err = streamDownload(ctx, job, pl.segments[from:], from, pl.mediaSeq+uint64(from), partPath)
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

	// 5. 落盘前抽样校验：探测期守卫只看首片，这里对成品头/中段再确认一次。
	//    判定损坏时直接丢弃 .part —— 重试只会重下出同样的坏数据，
	//    留着它只会让"重试"反复失败并占着断点。
	valSegs := len(pl.segments)
	if isLive {
		valSegs = 0 // 直播窗口大小不代表总量，体量合理性检查不适用
	}
	if verr := validateOutput(job.container, partPath, ProbeInfo{
		Encrypted: pl.key != nil,
		Segments:  valSegs,
		MinBytes:  int64(job.initLen),
	}); verr != nil {
		os.Remove(partPath)
		os.Remove(partPath + ".meta")
		fail(verr.Error() + "（已丢弃损坏的临时文件）")
		return
	}

	// 6. 落盘：先做容器收尾处理（fMP4 回填总时长 mehd/mvhd），再 .part → 正式文件
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
	te.rt.markDirty()
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
	te.rt.markDirty()
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
// 随后恢复跨分片规范化状态（无持久化字节时从 .part 头部重新解析 init，幂等）。
func restoreContainer(job *dlJob, containerID, partPath string, normState []byte) {
	if c := findContainerByID(containerID); c != nil {
		job.container = c
		if c.NewState != nil {
			job.norm = c.NewState()
		}
	} else if head, err := readHead(partPath, 4096); err == nil && len(head) > 0 {
		job.container = detectContainer(head, true)
		if job.container.NewState != nil {
			job.norm = job.container.NewState()
		}
	}
	if job.norm == nil {
		return
	}
	if len(normState) > 0 {
		job.norm.Restore(normState)
	} else if head, err := readHead(partPath, 64<<10); err == nil && len(head) > 8 {
		job.norm.Normalize(head)
	}
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
