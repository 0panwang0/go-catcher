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

// persistedOmissions 列出 taskState 里**故意**不落盘的字段及理由。
//
// 它存在的意义是把隐式决定变成显式登记：taskState → persistedTask → taskStateDTO
// 是三层手工同步的结构，加一个字段要在六处改（两个 struct、两个转换函数、以及
// 前端契约），而 Go 对"具名字面量漏写字段"是静默的（补零值、编译期不报错）。
// 于是"忘了持久化"与"想过、决定不持久化"在代码里长得一模一样 —— 前者会让
// 该字段的重启后丢失且无任何症状（形态①：静默丢值）。
//
// 加字段时的规矩：要么在 collectPersisted / loadState 里接线（两侧都要），
// 要么在这里写明为什么不留。TestTaskStatePersistenceIsExplicit 会拦下三处都漏的。
//
// 只读注册表，运行期不要改写。
var persistedOmissions = map[string]string{
	"queued":      "重启后排队位无意义：loadState 把没跑完的一律重建成可恢复态",
	"running":     "同上：上次进程留下的 running 必然是脏值，载入时统一置 false",
	"openPath":    "每次 snapshot 按磁盘实况现算（文件可能已被移走/删掉），落盘值必然陈旧",
	"bytesDone":   "直链进度由 downloadDirect 从 .part 实际大小重新取（setBytes），落盘值只会在恢复瞬间显示错值",
	"bytesTot":    "同上：总长下载时才探到，恢复时重新探，不依赖旧状态",
	"restartNote": "恢复时的一次性提示，本次进程内有效（见 taskState.restartNote 的注释）",
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
//
// 全程持 stateSaveMu：debounce goroutine 与 Engine.Stop 是两条独立调用路径，而
// writeFileAtomic 的临时名是固定的 ⇒ 两个写者会互踩出非法 JSON（见 runtime.go）。
func (r *Runtime) saveState() {
	r.stateSaveMu.Lock()
	defer r.stateSaveMu.Unlock()

	// 进程级拦截：CLI 的 Runtime 没有持久化语义（从不 loadState），写下去只会把
	// 空表覆盖进与 GUI 共用的状态文件（R1）。这与下面的"载入失败"是两回事 ——
	// 那条管"这次载入失败"，这条管"本 Runtime 永远不写"。放在唯一写盘咽喉上，
	// 所以任何到达 saveState 的路径（debounce goroutine / Engine.Stop）都被拦下。
	if r.persistDisabled {
		return
	}

	if r.stateOverwriteBlocked {
		// 载入时一个任务都没恢复出来（文件损坏 / 读不出 / 版本不认识），此刻的
		// tasks 是空表。写下去 = 拿空表覆盖用户仅有的那份文件。宁可这次不落盘
		// （A-1）——**保住原文件**比"本次会话的进度能存下来"重要得多。
		// loadState 已把原因记进 persistHealth，/status 会显示给用户。
		return
	}

	list := r.collectPersisted()
	data, err := json.MarshalIndent(stateFile{Version: stateVersion, Tasks: list}, "", "  ")
	if err != nil {
		r.recordPersistResult(fmt.Errorf("序列化失败: %w", err))
		fmt.Printf("[state] 序列化失败: %v\n", err)
		return
	}
	// 原子写：临时文件 + Sync + rename（见 writeFileAtomic）—— 就地写会在断电/强杀时
	// 留下半份 JSON，而载入侧对不符版本/解析失败是整份丢弃，历史任务会一起没。
	if err := writeFileAtomic(r.getStatePath(), data, 0644); err != nil {
		r.recordPersistResult(fmt.Errorf("写入失败: %w", err))
		fmt.Printf("[state] 写入失败: %v\n", err)
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
			// 读不出来（被别的程序占用 / 权限不足 / 盘拔了）：**不能当成"没有历史"**
			// —— 后续任何一次 saveState 都会拿空表覆盖它。先改名留档，改不动就本轮不落盘。
			r.quarantineUnreadableState(p, fmt.Sprintf("读取失败: %v", err))
		}
		return
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		// 内容确实坏了（截断 / 非法 JSON）。留档后**允许继续写新状态**：留档那份还能
		// 人工抢救，"被空表覆盖掉"就没有任何余地了。
		r.quarantineUnreadableState(p, fmt.Sprintf("解析失败: %v", err))
		return
	}
	// 版本校验（R7）：只写不读的版本号等于没有。未来版本（> stateVersion）的
	// 字段语义未知，宁可放弃恢复也不要解析出一个半残状态，让用户在界面上看到
	// 「任务凭空消失」；0（旧文件没有该字段）按当前结构尽力解析。
	//
	// ⚠️ 分支处置与上面两种**不同**（A-1）：这里的文件本身是好的，只是当前程序
	// 不认识它（用户降级了版本）。既不能改名挪走、更不能覆盖 —— 那等于把新版本
	// 写下的历史弄丢。所以走"本轮不落盘"这条路。
	if sf.Version > stateVersion {
		r.blockStateOverwrite(fmt.Sprintf("状态文件版本 %d 高于当前支持的 %d（请升级程序）", sf.Version, stateVersion))
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

// blockStateOverwrite 让本次进程彻底不再写状态文件，并把原因交给 /status（A-1）。
// 用在"文件本身可能是好的、只是这次读不了/不认识"的场合 —— 此时宁可不落盘。
//
// ⚠️ 这里**不调 recordPersistResult(nil)**，而且 saveState 在置位后直接 return、
// 也不会清它 ⇒ 这条不健康状态会一直挂在 /status 上直到进程退出。这正是要的效果：
// 用户得看见"这次没载入历史、也没敢覆盖原文件"。
func (r *Runtime) blockStateOverwrite(why string) {
	r.stateSaveMu.Lock()
	r.stateOverwriteBlocked = true
	r.stateSaveMu.Unlock()
	r.recordPersistResult(fmt.Errorf("未载入历史（%s）：本次不覆盖原状态文件", why))
	fmt.Printf("[state] %s；本次不落盘，以免覆盖原文件\n", why)
}

// quarantineUnreadableState 把读不出/坏掉的状态文件改名留档，然后允许正常落盘。
// 留档失败 ⇒ 退回 blockStateOverwrite（保命优先：宁可本次不保存，也不覆盖）。
//
// 备份名带时间戳：多次损坏各留一份（也避开"上一次的只读备份挡路"这种 rename 失败）。
func (r *Runtime) quarantineUnreadableState(p, why string) {
	bak := fmt.Sprintf("%s.%s.corrupt", p, time.Now().Format("20060102-150405"))
	if err := os.Rename(p, bak); err != nil {
		r.blockStateOverwrite(fmt.Sprintf("%s，且留档失败: %v", why, err))
		return
	}
	r.recordPersistResult(fmt.Errorf("原状态文件%s，已留档为 %s", why, filepath.Base(bak)))
	fmt.Printf("[state] 原状态文件%s，已留档为 %s（本次从空列表开始）\n", why, filepath.Base(bak))
}

// ============================================================
// 任务控制：暂停 / 恢复 / 取消 / 移除
// ============================================================

// findTask 按 id 取任务
