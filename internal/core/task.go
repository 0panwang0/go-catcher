// 任务模型与注册表：状态/并发槽/清理/JSON/意图常量。
package core

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

type taskState struct {
	id        string // 任务 ID
	queued    bool   // 已创建但在排队等待并发槽
	running   bool   // pipeline 是否正在跑
	paused    bool   // 已暂停（保留断点，可恢复）
	canceled  bool   // 已取消（.part 已清理，不可恢复）
	stage     string // "排队中"/"解析视频源"/"下载分片中"/"已保存"/"已暂停"/"已取消"/"失败"
	done      bool   // 整个任务结束（成功或失败或取消）
	finalPath string // 成功后的完整文件路径
	openPath  string // 当前真实存在的可打开文件（done 时为 finalPath；paused 时为 .part；其它为空）
	errorMsg  string // 失败原因
	// 本次任务的文件元信息（任务开始即填充，失败/成功后保留）
	m3u8URL  string // 来源 m3u8
	referer  string // 来源页（恢复下载时需要）
	filename string // 目标文件名（如 xxx.mp4）
	saveDir  string // 用户选的目标保存目录
	// 分片下载进度（运行时由 dlJob 的原子计数刷新）
	segDone  int64
	segTot   int64
	started  time.Time
	finished time.Time
}

// 任务是否还能恢复（暂停或意外中断，且还没下完）

func (t taskState) resumable() bool {
	return !t.done || (t.paused && !t.canceled)
}

const (
	intentNone = iota
	intentPause
	intentCancel
)

type taskEntry struct {
	mu     sync.Mutex
	st     taskState          // 对外状态快照（受 mu 保护）
	job    *dlJob             // 任务上下文（运行时 segDone/segTot 原子值直接读）
	cancel context.CancelFunc // 中断下载（暂停/取消共用，靠 intent 区分后续处理）
	intent int                // intentNone / intentPause / intentCancel
}

var (
	saveDirMu      sync.Mutex // 保护 defaultSaveDir
	defaultSaveDir string     // /pickdir 选中的目录，供后续 /download?mode=disk 使用
	tasksMu        sync.Mutex
	tasks          = map[string]*taskEntry{}
	// 并发槽位：用 resizableSem（config.go），运行中可调整上限
	seqID int64
)

// snapshot 返回某任务当前对外状态（segDone 实时从 job 原子取，stage/error 从缓存取）

func snapshot(te *taskEntry) taskState {
	te.mu.Lock()
	s := te.st
	te.mu.Unlock()
	if te.job != nil {
		s.segDone = te.job.segNow()
		s.segTot = te.job.segTotal()
	}
	// openPath：当前真实可打开的文件。done 用成品；paused/失败 用 .part 半成品（失败保留 .part 供重试）；其余空。
	if s.done && s.finalPath != "" && fileExists(s.finalPath) {
		s.openPath = s.finalPath
	} else if s.finalPath != "" && fileExists(s.finalPath+".part") {
		s.openPath = s.finalPath + ".part"
	} else {
		s.openPath = ""
	}
	return s
}

// fileExists 封装 os.Stat 的存在性判断

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// failTask 标记任务失败结束（保留文件元信息）

func failTask(te *taskEntry, msg string) {
	te.mu.Lock()
	te.st.running = false
	te.st.queued = false
	te.st.done = true
	te.st.stage = "失败"
	te.st.errorMsg = msg
	te.st.finalPath = ""
	te.mu.Unlock()
}

// newTaskID 生成自增任务 ID

func newTaskID() string {
	tasksMu.Lock()
	seqID++
	id := fmt.Sprintf("t%d", seqID)
	tasksMu.Unlock()
	return id
}

// pruneOldTasks 若已完成/失败任务超过上限，清理最老的几个，防止注册表无限增长。
// 运行中/排队中的任务永不清理。

const maxKeptTasks = 100

func pruneOldTasks() {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	// 统计已完成任务并按 started 排序保留最新的 maxKeptTasks 个
	type doneItem struct {
		id      string
		started time.Time
	}
	var dones []doneItem
	for id, te := range tasks {
		te.mu.Lock()
		finished := te.st.done
		st := te.st.started
		te.mu.Unlock()
		if finished {
			dones = append(dones, doneItem{id, st})
		}
	}
	if len(dones) <= maxKeptTasks {
		return
	}
	sort.Slice(dones, func(i, j int) bool { return dones[i].started.Before(dones[j].started) })
	for _, d := range dones[:len(dones)-maxKeptTasks] {
		delete(tasks, d.id)
	}
}

func taskStateJSON(t taskState) string {
	pct := 0.0
	if t.segTot > 0 {
		pct = float64(t.segDone) / float64(t.segTot) * 100
		if pct > 100 {
			pct = 100
		}
	}
	// 已完成但成品文件已不在磁盘（被移动/删除）：前端显示"已失效"而不是"完成 · 已保存"。
	// snapshot 已按磁盘实况计算 openPath（文件存在时 == finalPath），据此判断无需再 Stat 一次。
	fileMissing := t.done && t.finalPath != "" && t.openPath != t.finalPath
	return fmt.Sprintf(
		`{"id":%q,"queued":%v,"running":%v,"paused":%v,"canceled":%v,"stage":%q,"done":%v,"fileMissing":%v,"pct":%.1f,"segDone":%d,"segTot":%d,"finalPath":%q,"openPath":%q,"error":%q,"m3u8URL":%q,"referer":%q,"filename":%q,"saveDir":%q,"started":%q,"finished":%q}`,
		t.id, t.queued, t.running, t.paused, t.canceled, t.stage, t.done, fileMissing, pct,
		t.segDone, t.segTot, t.finalPath, t.openPath, t.errorMsg, t.m3u8URL, t.referer,
		t.filename, t.saveDir,
		t.started.Format(time.RFC3339), t.finished.Format(time.RFC3339))
}

// 列出所有任务状态：未结束的（含暂停）在前，已结束的在后；组内按开始时间倒序（新→旧）。
// 前端据此按天分组展示，最新的任务永远在最上面。

func listTasks() []taskState {
	tasksMu.Lock()
	entries := make([]*taskEntry, 0, len(tasks))
	for _, te := range tasks {
		entries = append(entries, te)
	}
	tasksMu.Unlock()
	out := make([]taskState, 0, len(entries))
	for _, te := range entries {
		out = append(out, snapshot(te))
	}
	sort.SliceStable(out, func(i, j int) bool {
		di := out[i].done
		dj := out[j].done
		if di != dj {
			return !di // 未结束（含暂停/排队/运行中）在前
		}
		if out[i].started.Equal(out[j].started) {
			return out[i].id < out[j].id
		}
		return out[i].started.After(out[j].started) // 新→旧
	})
	return out
}

// /status 返回任务状态：
//   - ?id=t3        -> 返回单个任务对象 JSON（不存在返回 404）
//   - 无 id          -> 返回 {"tasks":[ {...}, ... ]} 全部任务列表（供监控页）
