// 任务状态持久化到 gocatcher_state.json。
package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const stateVersion = 1

// persistedTask 落盘的字段（不含运行时对象）

type persistedTask struct {
	ID       string            `json:"id"`
	Filename string            `json:"filename"`
	SaveDir  string            `json:"saveDir"`
	M3u8URL  string            `json:"m3u8URL"`
	Referer  string            `json:"referer"`
	SegRefs  map[string]string `json:"segrefs,omitempty"`
	SegDone  int64             `json:"segDone"`
	SegTot   int64             `json:"segTot"`
	// FlushedBytes 已确认落盘的分片字节数（含 init 段）。旧状态文件（v0.5.0）
	// 没有这个字段，读到 0 ⇒ 由恢复时的对齐判据判为"无法对齐"，从 0 重下
	// （见 pipeline 的对齐判据），不是解析出一个半残状态。
	FlushedBytes int64     `json:"flushedBytes,omitempty"`
	Stage        string    `json:"stage"`
	Done         bool      `json:"done"`
	Paused       bool      `json:"paused"`
	Canceled     bool      `json:"canceled"`
	FinalPath    string    `json:"finalPath"`
	ErrorMsg     string    `json:"errorMsg"`
	Started      time.Time `json:"started"`
	Finished     time.Time `json:"finished"`
	Live         bool      `json:"live"` // 直播跟随任务（播放列表无 ENDLIST）
	// 中断收尾（已保存已录部分但没录完）与产物时间轴上的缺口时长。
	// 落盘是为了重启后界面仍能把"中断"与"完整录完"区分开。
	Interrupted bool            `json:"interrupted,omitempty"`
	GapSeconds  float64         `json:"gapSeconds,omitempty"`
	ContainerID string          `json:"containerID,omitempty"` // 探测到的容器 ID（续传恢复规范化）
	NormState   json.RawMessage `json:"normState,omitempty"`   // 跨分片状态字节（NormState.snapshot 导出，续传 restore 恢复）
	// PartMode 直链 .part 的写入模式（"chunked" / "stream"）。
	// 分片模式用 WriteAt 稀疏写 ⇒ 文件大小 ≠ 有效字节数，位图丢失时**不能**按
	// 大小续传；单连接模式是连续前缀 ⇒ 可以。两种模式在磁盘上无法区分，必须记。
	PartMode string `json:"partMode,omitempty"`
}

type stateFile struct {
	Version int             `json:"version"`
	Tasks   []persistedTask `json:"tasks"`
}

// stateMu / stateDirty / stateSaving / statePathOnce / statePath / stateSaveDelay
// / persistHealth 均为 Runtime 字段（见 runtime.go）。

// getStatePath 状态文件路径：exe 同目录（用户双击 bat 启动时必定可写）。
// 测试可直接赋值 r.statePath 覆盖（非空时优先）。

func (r *Runtime) getStatePath() string {
	if r.statePath != "" {
		return r.statePath
	}
	r.statePathOnce.Do(func() {
		if exe, err := os.Executable(); err == nil {
			r.statePath = filepath.Join(filepath.Dir(exe), "gocatcher_state.json")
			return
		}
		r.statePath = "gocatcher_state.json"
	})
	return r.statePath
}

// markDirty 标记状态已变更，触发一次延迟落盘（合并高频进度更新，避免疯狂写盘）

func (r *Runtime) markDirty() {
	r.stateMu.Lock()
	if r.stateSaving {
		r.stateDirty = true // 正在写，让它写完再来一轮
		r.stateMu.Unlock()
		return
	}
	r.stateSaving = true
	r.stateMu.Unlock()

	go func() {
		time.Sleep(r.stateSaveDelay) // debounce
		r.stateMu.Lock()
		r.stateDirty = false
		r.stateMu.Unlock()

		r.saveState()

		r.stateMu.Lock()
		r.stateSaving = false
		again := r.stateDirty
		r.stateMu.Unlock()
		if again {
			r.markDirty()
		}
	}()
}

// collectPersisted 快照所有任务（先取 tasks 快照再逐个加锁，避免锁嵌套死锁）

func (r *Runtime) collectPersisted() []persistedTask {
	r.tasksMu.Lock()
	entries := make([]*taskEntry, 0, len(r.tasks))
	for _, te := range r.tasks {
		entries = append(entries, te)
	}
	r.tasksMu.Unlock()

	out := make([]persistedTask, 0, len(entries))
	for _, te := range entries {
		s := snapshot(te)
		// 断点必须用"已确认落盘"的那一对账本值，不能用界面上那个（R9）：
		// segDone 反映的是"已写进缓冲区"，可能领先磁盘若干 MB，拿它当续传起点
		// 会在文件中间留空洞。序号与字节数必须**同源取**（一次 Load）——
		// 分开取会让恢复拿到错配的一对，进而把 .part 截到错误位置。
		segDone, flushedBytes := s.segDone, s.flushedBytes
		if job := te.jobRef(); job != nil {
			m := job.flushedMarkNow()
			segDone, flushedBytes = m.segs, m.bytes
		}
		pt := persistedTask{
			ID: s.id, Filename: s.filename, SaveDir: s.saveDir,
			M3u8URL: s.m3u8URL, Referer: s.referer, SegRefs: s.segRefs,
			SegDone: segDone, SegTot: s.segTot, FlushedBytes: flushedBytes,
			Stage: s.stage,
			Done:  s.done, Paused: s.paused, Canceled: s.canceled,
			FinalPath: s.finalPath, ErrorMsg: s.errorMsg,
			Started: s.started, Finished: s.finished,
			Live:        s.live,
			Interrupted: s.interrupted,
			GapSeconds:  s.gapSeconds,
		}
		pt.ContainerID = s.containerID
		// 直链的写入模式优先取 job 的实时值：它要到下载开始时才确定
		// （见 downloadDirect / setPartMode），而 setPartMode 会自己触发落盘。
		pt.PartMode = s.partMode
		if job := te.jobRef(); job != nil {
			if m := job.partModeNow(); m != "" {
				pt.PartMode = m
			}
		}
		if len(s.normState) > 0 {
			pt.NormState = json.RawMessage(s.normState)
		}
		out = append(out, pt)
	}
	// 稳定的排序，避免 map 遍历顺序导致文件内容每次都变
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ============================================================
// 持久化健康状态（R4）
// ------------------------------------------------------------
// saveState 失败以前只 fmt.Printf：GUI 是 windowsgui 子系统、没有控制台，
// 用户完全看不到 —— 以为断点存了、重启却发现进度回退。/status 现在带上这份
// 健康信息，前端能给出一次明显提示。
// ============================================================

// persistHealth（Runtime 字段）记录最近一次落盘结果（R4）。
// saveState 失败以前只 fmt.Printf：GUI 是 windowsgui 子系统、没有控制台，
// 用户完全看不到 —— 以为断点存了、重启却发现进度回退。/status 现在带上这份
// 健康信息，前端能给出一次明显提示。

func (r *Runtime) recordPersistResult(err error) {
	r.persistHealth.mu.Lock()
	if err != nil {
		r.persistHealth.lastErr = err.Error()
	} else {
		r.persistHealth.lastErr = ""
		r.persistHealth.lastOK = time.Now()
	}
	r.persistHealth.mu.Unlock()
}

// persistStatus 返回 (最近一次落盘错误, 最近一次落盘成功时间)。
func (r *Runtime) persistStatus() (string, time.Time) {
	r.persistHealth.mu.Lock()
	defer r.persistHealth.mu.Unlock()
	return r.persistHealth.lastErr, r.persistHealth.lastOK
}

// persistDTO 是 /status 里暴露的持久化健康信息。
type persistDTO struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	LastOK string `json:"lastOK,omitempty"`
}

func (r *Runtime) persistDTOOf() persistDTO {
	errMsg, lastOK := r.persistStatus()
	d := persistDTO{OK: errMsg == "", Error: errMsg}
	if !lastOK.IsZero() {
		d.LastOK = lastOK.Format(time.RFC3339)
	}
	return d
}

// saveState 把任务快照写入 gocatcher_state.json（临时文件 + rename 原子替换）。
// 失败会记进 persistHealth，由 /status 暴露给前端提示用户。
func (r *Runtime) saveState() {
	list := r.collectPersisted()
	data, err := json.MarshalIndent(stateFile{Version: stateVersion, Tasks: list}, "", "  ")
	if err != nil {
		r.recordPersistResult(fmt.Errorf("序列化失败: %w", err))
		fmt.Printf("[state] 序列化失败: %v\n", err)
		return
	}
	p := r.getStatePath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		r.recordPersistResult(fmt.Errorf("写入失败: %w", err))
		fmt.Printf("[state] 写入失败: %v\n", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		r.recordPersistResult(fmt.Errorf("替换状态文件失败: %w", err))
		fmt.Printf("[state] 替换状态文件失败: %v\n", err)
		return
	}
	r.recordPersistResult(nil)
}

// loadState 启动时恢复历史任务。
// 上次进程退出时仍在跑/排队/暂停的点播任务一律置为「已暂停」——它们保留了
// .part 与断点，用户点恢复即可接着下；直播任务置为「录制中断」——它没有
// "接着录"这回事（暂停期间的分片已从滑动窗口滚走）。

func (r *Runtime) loadState() {
	p := r.getStatePath()
	data, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("[state] 读取失败: %v\n", err)
		}
		return
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		fmt.Printf("[state] 解析失败（忽略）: %v\n", err)
		return
	}
	// 版本校验（R7）：只写不读的版本号等于没有。未来版本（> stateVersion）的
	// 字段语义未知，宁可放弃恢复也不要解析出一个半残状态，让用户在界面上看到
	// 「任务凭空消失」；0（旧文件没有该字段）按当前结构尽力解析。
	if sf.Version > stateVersion {
		fmt.Printf("[state] 状态文件版本 %d 高于当前支持的 %d，已跳过恢复（请升级程序）\n", sf.Version, stateVersion)
		return
	}
	resumed := 0
	for _, pt := range sf.Tasks {
		te := &taskEntry{rt: r, intent: intentNone}
		te.st = taskState{
			id: pt.ID, stage: pt.Stage, done: pt.Done, paused: pt.Paused,
			canceled: pt.Canceled, finalPath: pt.FinalPath, errorMsg: pt.ErrorMsg,
			m3u8URL: pt.M3u8URL, referer: pt.Referer, segRefs: pt.SegRefs, filename: pt.Filename,
			saveDir: pt.SaveDir, segDone: pt.SegDone, segTot: pt.SegTot,
			flushedBytes: pt.FlushedBytes,
			started:      pt.Started, finished: pt.Finished,
			live:        pt.Live,
			interrupted: pt.Interrupted, gapSeconds: pt.GapSeconds,
			containerID: pt.ContainerID,
			partMode:    pt.PartMode,
		}
		if len(pt.NormState) > 0 {
			te.st.normState = append([]byte(nil), pt.NormState...)
		}
		// 已取消的任务没有任何成品文件，清掉 finalPath（防御旧版本状态文件残留）
		if pt.Canceled {
			te.st.finalPath = ""
		}
		// 上次没跑完的（含 running/queued 残留）统一变成可恢复态。
		// 直播是例外：它没有"接着录"这回事（暂停期间的分片已从滑动窗口滚走），
		// 标成中断态等用户点「停止」收尾，界面据此只给「停止」「取消」。
		if !te.st.done && !te.st.canceled {
			te.st.running = false
			te.st.queued = false
			te.st.paused = true
			if te.st.live {
				// 「异常」二字不能省：能走到这里的直播任务只可能是"上次进程没机会收尾"
				// （强杀 / 断电）——正常退出会由 stopOrPauseAllTasks 收尾成「已完成」。
				te.st.stage = "录制中断（程序异常退出）"
			} else {
				te.st.stage = "已暂停"
				resumed++
			}
		}
		r.tasks[te.st.id] = te
		// 历史任务的保存目录一并进白名单：重启后用户点"重试"仍写回原目录，
		// 否则 /download 的白名单校验会把恢复路径也一起挡掉。
		r.allowSaveDir(pt.SaveDir)
		// 推进 ID 序号，避免新任务和历史任务撞号
		var n int64
		if _, err := fmt.Sscanf(te.st.id, "t%d", &n); err == nil && n > r.seqID {
			r.seqID = n
		}
	}
	// 恢复出来的历史任务同样受 maxKeptTasks 约束：pruneOldTasks 原先只在新建任务
	// 时被调用，状态文件里堆积的旧完成任务无人清理（长期使用后列表无限增长）。
	r.pruneOldTasks()
	if len(sf.Tasks) > 0 {
		fmt.Printf("[state] 已恢复 %d 个历史任务（其中 %d 个可继续下载）\n", len(sf.Tasks), resumed)
	}
}

// ============================================================
// 任务控制：暂停 / 恢复 / 取消 / 移除
// ============================================================

// findTask 按 id 取任务
