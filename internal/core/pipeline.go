// 单任务下载管线与暂停/取消收尾。
package core

import (
	"context"
	"fmt"
	"os"
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

	// 1. fetch m3u8（含 master 选最高码率）
	m3u8Content, err := job.fetchM3U8()
	if err != nil {
		if ctx.Err() != nil {
			finishInterrupt(te)
			return
		}
		fail("获取 m3u8 失败: " + err.Error())
		return
	}
	// 2. 解析分片
	segURLs, err := job.parseTSSegments(m3u8Content)
	if err != nil {
		fail("解析分片失败: " + err.Error())
		return
	}
	fmt.Printf("[disk] id=%s segments: %d\n", st.id, len(segURLs))

	// 下载期间写 .part，写完后改名成正式文件（避免半成品被当成成品）
	partPath := st.finalPath + ".part"

	// 3. 流式下载：从断点继续，边下边写。写完的瞬间文件就是成品，无需合并阶段。
	from := int(st.segDone)
	next, derr := streamDownload(ctx, job, segURLs, from, partPath)

	te.mu.Lock()
	te.st.segDone = int64(next)
	te.st.segTot = int64(len(segURLs))
	te.mu.Unlock()

	if derr != nil {
		if ctx.Err() != nil {
			finishInterrupt(te)
			return
		}
		fail("下载分片失败: " + derr.Error())
		return
	}

	// 4. 落盘：.part → 正式文件
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
		}
		fmt.Printf("[disk] id=%s 已取消\n", id)
	} else {
		fmt.Printf("[disk] id=%s 已暂停，断点 %d\n", id, segDone)
	}
	markDirty()
}

// uniquePath 若 path 已存在则返回 "name (1).ext"、"name (2).ext"… 直到不冲突

// ============================================================
// 任务持久化
// 任务列表写入 exe 同目录的 gocatcher_state.json，
// 服务重启后能恢复历史任务（含未完成的，恢复后为「已暂停」可继续）。
// 写入用「临时文件 + rename」保证原子性，避免写一半崩掉留下坏文件。
// ============================================================
