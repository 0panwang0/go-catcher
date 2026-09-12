// 任务模型与注册表：状态/并发槽/清理/JSON/意图常量。
package core

import (
	"context"
	"fmt"
	"math"
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
	filename string // 目标文件名（如 xxx.ts）
	saveDir  string // 用户选的目标保存目录
	// 分片下载进度（运行时由 dlJob 的原子计数刷新）
	segDone  int64
	segTot   int64
	started  time.Time
	finished time.Time

	// live 直播跟随任务（播放列表无 ENDLIST，列表不断增长）
	live bool
	// seen 直播已录制分片 URL 窗口（断点恢复时跳过已录分片；运行时以 job.seen 为准）
	seen []string

	// containerID 探测到的容器 ID（断点续传恢复规范化等格式相关行为）
	containerID string
	// normState 跨分片规范化状态的持久化字节（NormState.snapshot 导出；
	// 续传时 restore 恢复：tfdt 基准/结束时间/init 信息全包）
	normState []byte
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
	// rt 是任务所属的运行时（创建任务时注入）。pipeline / 持久化 / 控制端点
	// 通过它访问 limiter、任务表、状态落盘等运行时状态。
	rt     *Runtime
	mu     sync.Mutex
	st     taskState          // 对外状态快照（受 mu 保护）
	job    *dlJob             // 任务上下文（运行时 segDone/segTot 原子值直接读）；指针本身必须经 jobRef 在 mu 内取
	cancel context.CancelFunc // 中断下载（暂停/取消共用，靠 intent 区分后续处理）
	intent int                // intentNone / intentPause / intentCancel
}

// saveDirMu / defaultSaveDir / tasksMu / tasks / seqID 均为 Runtime 字段（见 runtime.go）。

// jobRef 取当前任务上下文（持 te.mu）。
//
// te.job 由 pipeline 在启动时写入，写点是持锁的；而 /status 每 900ms 轮询一次
// snapshot、后台还会周期性 collectPersisted，它们都在另一个 goroutine 里。
// 指针字段本身也必须走锁——裸读是一个真实的 data race（`-race` 在没有
// "边跑边读状态"的用例时抓不到，不代表安全）。
//
// 拿到返回值后可以安全使用：dlJob 的进度计数是 atomic/自带锁的，
// 这里保护的只是"指针有没有被换掉"。
func (te *taskEntry) jobRef() *dlJob {
	te.mu.Lock()
	defer te.mu.Unlock()
	return te.job
}

// snapshot 返回某任务当前对外状态（segDone 实时从 job 原子取，stage/error 从缓存取）

func snapshot(te *taskEntry) taskState {
	te.mu.Lock()
	s := te.st
	job := te.job
	te.mu.Unlock()
	if job != nil {
		s.segDone = job.segNow()
		s.segTot = job.segTotal()
	}
	// openPath：当前真实可打开的文件。done 用成品；paused/失败 用 .part 半成品（失败保留 .part 供重试）；其余空。
	// .part 要求非空：uniquePath 会为刚创建的任务留下一个 0 字节占位（认领文件名），
	// 那不是"可打开的半成品"。
	if s.done && s.finalPath != "" && fileExists(s.finalPath) {
		s.openPath = s.finalPath
	} else if s.finalPath != "" && nonEmptyFile(s.finalPath+".part") {
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

// nonEmptyFile 报告路径是存在且非空的文件。用来区分"真正下到一半的半成品"
// 与 uniquePath 留下的 0 字节占位（任务还在排队，没有任何可打开的内容）。
func nonEmptyFile(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// failTask 标记任务失败结束（保留 .part 与文件元信息，供"重试"断点续传）。
// finalPath 绝不能清：重试的 pipeline 靠它定位 .part，清了会把续传数据
// 写到工作目录下的游离 ".part" 且收尾必失败（暂停路径 finishInterrupt 就不清）。
func failTask(te *taskEntry, msg string) {
	te.mu.Lock()
	te.st.running = false
	te.st.queued = false
	te.st.done = true
	te.st.stage = "失败"
	te.st.errorMsg = msg
	te.mu.Unlock()
}

// newTaskID 生成自增任务 ID

func (r *Runtime) newTaskID() string {
	r.tasksMu.Lock()
	r.seqID++
	id := fmt.Sprintf("t%d", r.seqID)
	r.tasksMu.Unlock()
	return id
}

// pruneOldTasks 若已完成/失败任务超过上限，清理最老的几个，防止注册表无限增长。
// 运行中/排队中的任务永不清理。

const maxKeptTasks = 100

func (r *Runtime) pruneOldTasks() {
	r.tasksMu.Lock()
	defer r.tasksMu.Unlock()
	// 统计已完成任务并按 started 排序保留最新的 maxKeptTasks 个
	type doneItem struct {
		id      string
		started time.Time
	}
	var dones []doneItem
	for id, te := range r.tasks {
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
		delete(r.tasks, d.id)
	}
}

// taskStateDTO 是 /status 对外暴露的任务状态字段集（字段名即前端契约，改动需同步 web/）。
//
// 为什么用 encoding/json 而不是 fmt.Sprintf 拼串：errorMsg 的来源是远端异常串
// （HTTP 状态、URL、响应头），可能含控制字符；而 %q 走 strconv.Quote，会产生
// \x01 这类 JSON 非法转义（JSON 只认 \u0001）。一旦命中，前端 JSON.parse 直接
// 抛错、任务列表整块渲染失败。
type taskStateDTO struct {
	ID          string  `json:"id"`
	Queued      bool    `json:"queued"`
	Running     bool    `json:"running"`
	Paused      bool    `json:"paused"`
	Canceled    bool    `json:"canceled"`
	Stage       string  `json:"stage"`
	Done        bool    `json:"done"`
	Live        bool    `json:"live"`
	FileMissing bool    `json:"fileMissing"`
	Pct         float64 `json:"pct"`
	SegDone     int64   `json:"segDone"`
	SegTot      int64   `json:"segTot"`
	FinalPath   string  `json:"finalPath"`
	OpenPath    string  `json:"openPath"`
	Error       string  `json:"error"`
	M3U8URL     string  `json:"m3u8URL"`
	Referer     string  `json:"referer"`
	Filename    string  `json:"filename"`
	SaveDir     string  `json:"saveDir"`
	Started     string  `json:"started"`
	Finished    string  `json:"finished"`
}

// toTaskStateDTO 计算派生字段（pct / fileMissing）并转换结构。
func toTaskStateDTO(t taskState) taskStateDTO {
	pct := 0.0
	if t.segTot > 0 {
		pct = float64(t.segDone) / float64(t.segTot) * 100
		if pct > 100 {
			pct = 100
		}
		// 保留一位小数（沿用原 %.1f 的精度），顺带避免浮点尾数让前端签名抖动
		pct = math.Round(pct*10) / 10
	}
	// 已完成但成品文件已不在磁盘（被移动/删除）：前端显示"已失效"而不是"完成 · 已保存"。
	// snapshot 已按磁盘实况计算 openPath（文件存在时 == finalPath），据此判断无需再 Stat 一次。
	// 失败/取消任务不算失效：它们的成品本来就不存在（openPath 指向 .part），语义是"失败"。
	fileMissing := t.done && t.errorMsg == "" && !t.canceled && t.finalPath != "" && t.openPath != t.finalPath
	return taskStateDTO{
		ID: t.id, Queued: t.queued, Running: t.running, Paused: t.paused,
		Canceled: t.canceled, Stage: t.stage, Done: t.done, Live: t.live,
		FileMissing: fileMissing, Pct: pct, SegDone: t.segDone, SegTot: t.segTot,
		FinalPath: t.finalPath, OpenPath: t.openPath, Error: t.errorMsg,
		M3U8URL: t.m3u8URL, Referer: t.referer, Filename: t.filename, SaveDir: t.saveDir,
		Started:  t.started.Format(time.RFC3339),
		Finished: t.finished.Format(time.RFC3339),
	}
}

// 列出所有任务状态：未结束的（含暂停）在前，已结束的在后；组内按开始时间倒序（新→旧）。
// 前端据此按天分组展示，最新的任务永远在最上面。

func (r *Runtime) listTasks() []taskState {
	r.tasksMu.Lock()
	entries := make([]*taskEntry, 0, len(r.tasks))
	for _, te := range r.tasks {
		entries = append(entries, te)
	}
	r.tasksMu.Unlock()
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
