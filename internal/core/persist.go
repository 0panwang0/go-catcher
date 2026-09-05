// 任务状态持久化到 gocatcher_state.json。
package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const stateVersion = 1

// persistedTask 落盘的字段（不含运行时对象）

type persistedTask struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	SaveDir   string    `json:"saveDir"`
	M3u8URL   string    `json:"m3u8URL"`
	Referer   string    `json:"referer"`
	SegDone   int64     `json:"segDone"`
	SegTot    int64     `json:"segTot"`
	Stage     string    `json:"stage"`
	Done      bool      `json:"done"`
	Paused    bool      `json:"paused"`
	Canceled  bool      `json:"canceled"`
	FinalPath string    `json:"finalPath"`
	ErrorMsg  string    `json:"errorMsg"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Live      bool              `json:"live"`             // 直播跟随任务（播放列表无 ENDLIST）
	SeenURLs  []string          `json:"seenURLs,omitempty"` // 直播已录分片 URL 窗口（断点恢复去重）
	ContainerID string          `json:"containerID,omitempty"` // 探测到的容器 ID（续传恢复规范化）
	FMP4Baseline map[string]uint64 `json:"fmp4Baseline,omitempty"` // fMP4 tfdt 基准（续传沿用）
	FMP4End      map[string]uint64 `json:"fmp4End,omitempty"`      // fMP4 各轨结束时间（续传继续累计，完成回填 mehd）
	FMP4Init     *persistFMP4InitInfo  `json:"fmp4Init,omitempty"`     // fMP4 init 段解析结果（回填位置与 timescale）
}

type stateFile struct {
	Version int             `json:"version"`
	Tasks   []persistedTask `json:"tasks"`
}

var (
	stateMu       sync.Mutex
	stateDirty    bool
	stateSaving   bool
	statePathOnce sync.Once
	statePath     string
)

// getStatePath 状态文件路径：exe 同目录（用户双击 bat 启动时必定可写）

func getStatePath() string {
	statePathOnce.Do(func() {
		if exe, err := os.Executable(); err == nil {
			statePath = filepath.Join(filepath.Dir(exe), "gocatcher_state.json")
			return
		}
		statePath = "gocatcher_state.json"
	})
	return statePath
}

// markDirty 标记状态已变更，触发一次延迟落盘（合并高频进度更新，避免疯狂写盘）

func markDirty() {
	stateMu.Lock()
	if stateSaving {
		stateDirty = true // 正在写，让它写完再来一轮
		stateMu.Unlock()
		return
	}
	stateSaving = true
	stateMu.Unlock()

	go func() {
		time.Sleep(1200 * time.Millisecond) // debounce：进度每帧都变，1.2s 存一次足够
		stateMu.Lock()
		stateDirty = false
		stateMu.Unlock()

		saveState()

		stateMu.Lock()
		stateSaving = false
		again := stateDirty
		stateMu.Unlock()
		if again {
			markDirty()
		}
	}()
}

// collectPersisted 快照所有任务（先取 tasks 快照再逐个加锁，避免锁嵌套死锁）

func collectPersisted() []persistedTask {
	tasksMu.Lock()
	entries := make([]*taskEntry, 0, len(tasks))
	for _, te := range tasks {
		entries = append(entries, te)
	}
	tasksMu.Unlock()

	out := make([]persistedTask, 0, len(entries))
	for _, te := range entries {
		s := snapshot(te)
		pt := persistedTask{
			ID: s.id, Filename: s.filename, SaveDir: s.saveDir,
			M3u8URL: s.m3u8URL, Referer: s.referer,
			SegDone: s.segDone, SegTot: s.segTot, Stage: s.stage,
			Done: s.done, Paused: s.paused, Canceled: s.canceled,
			FinalPath: s.finalPath, ErrorMsg: s.errorMsg,
			Started: s.started, Finished: s.finished,
			Live: s.live,
		}
		if s.live {
			pt.SeenURLs = te.seenURLs()
		}
		pt.ContainerID = s.containerID
		pt.FMP4Baseline = s.fmp4Baseline
		pt.FMP4End = s.fmp4End
		pt.FMP4Init = s.fmp4Init.persist()
		out = append(out, pt)
	}
	// 稳定的排序，避免 map 遍历顺序导致文件内容每次都变
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// seenURLs 取直播任务当前已录制分片 URL 窗口（运行时以 job.seen 为准）。
func (te *taskEntry) seenURLs() []string {
	te.mu.Lock()
	defer te.mu.Unlock()
	if te.job == nil || !te.job.live {
		return nil
	}
	return te.job.seenSnapshot()
}

// saveState 把任务快照写入 gocatcher_state.json（临时文件 + rename 原子替换）。
func saveState() {
	list := collectPersisted()
	data, err := json.MarshalIndent(stateFile{Version: stateVersion, Tasks: list}, "", "  ")
	if err != nil {
		fmt.Printf("[state] 序列化失败: %v\n", err)
		return
	}
	p := getStatePath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		fmt.Printf("[state] 写入失败: %v\n", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		fmt.Printf("[state] 替换状态文件失败: %v\n", err)
	}
}

// loadState 启动时恢复历史任务。
// 上次进程退出时仍在跑/排队/暂停的任务，一律置为「已暂停」——
// 它们都保留了 .part 和断点，用户点恢复即可接着下。

func loadState() {
	p := getStatePath()
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
	resumed := 0
	for _, pt := range sf.Tasks {
		te := &taskEntry{intent: intentNone}
		te.st = taskState{
			id: pt.ID, stage: pt.Stage, done: pt.Done, paused: pt.Paused,
			canceled: pt.Canceled, finalPath: pt.FinalPath, errorMsg: pt.ErrorMsg,
			m3u8URL: pt.M3u8URL, referer: pt.Referer, filename: pt.Filename,
			saveDir: pt.SaveDir, segDone: pt.SegDone, segTot: pt.SegTot,
			started: pt.Started, finished: pt.Finished,
			live: pt.Live, seen: pt.SeenURLs,
			containerID: pt.ContainerID, fmp4Baseline: pt.FMP4Baseline,
			fmp4End: pt.FMP4End,
		}
		if pt.FMP4Init != nil {
			te.st.fmp4Init = &fmp4InitInfo{}
			te.st.fmp4Init.restore(pt.FMP4Init)
		}
		// 已取消的任务没有任何成品文件，清掉 finalPath（防御旧版本状态文件残留）
		if pt.Canceled {
			te.st.finalPath = ""
		}
		// 上次没跑完的（含 running/queued 残留）统一变成可恢复的暂停态
		if !te.st.done && !te.st.canceled {
			te.st.paused = true
			te.st.running = false
			te.st.queued = false
			te.st.stage = "已暂停"
			resumed++
		}
		tasks[te.st.id] = te
		// 推进 ID 序号，避免新任务和历史任务撞号
		var n int64
		if _, err := fmt.Sscanf(te.st.id, "t%d", &n); err == nil && n > seqID {
			seqID = n
		}
	}
	if len(sf.Tasks) > 0 {
		fmt.Printf("[state] 已恢复 %d 个历史任务（其中 %d 个可继续下载）\n", len(sf.Tasks), resumed)
	}
}

// ============================================================
// 任务控制：暂停 / 恢复 / 取消 / 移除
// ============================================================

// findTask 按 id 取任务
