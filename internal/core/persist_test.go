// 任务状态持久化测试：saveState/loadState 往返、运行中→暂停转换、fmp4 状态恢复、取消清理。
package core

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// saveRestoreState 固定状态文件路径并保存/恢复 tasks 与 seqID 全局。
func saveRestoreState(t *testing.T) string {
	t.Helper()
	oldPath, oldTasks, oldSeq := statePath, tasks, seqID
	t.Cleanup(func() {
		statePath, tasks, seqID = oldPath, oldTasks, oldSeq
		stateDirty, stateSaving = false, false
	})
	getStatePath() // 触发 once，之后直接覆盖 statePath
	dir := t.TempDir()
	statePath = filepath.Join(dir, "gocatcher_state.json")
	tasks = map[string]*taskEntry{}
	seqID = 0
	return statePath
}

// TestSaveLoadRoundTrip 任务完整往返：done 任务原样恢复（含 fmp4 状态），
// running/queued 残留转「已暂停」，seqID 推进避免撞号。
func TestSaveLoadRoundTrip(t *testing.T) {
	p := saveRestoreState(t)

	initData, info := prepareInit(buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000}), true)
	if info.mehdOff < 0 {
		t.Fatal("buildInit 应解析出 mehd 位置")
	}
	_ = initData

	now := time.Now()
	tasks["t1"] = &taskEntry{st: taskState{
		id: "t1", done: true, finalPath: filepath.Join(t.TempDir(), "a.mp4"),
		filename: "a.mp4", m3u8URL: "https://x/a.m3u8", stage: "已保存",
		started: now, finished: now, containerID: "fmp4",
		fmp4Baseline: map[string]uint64{"1": 100}, fmp4End: map[string]uint64{"1": 9000},
		fmp4Init: info,
	}}
	tasks["t2"] = &taskEntry{st: taskState{
		id: "t2", running: true, stage: "下载分片中", filename: "b.ts",
		m3u8URL: "https://x/b.m3u8", started: now,
	}}
	tasks["t3"] = &taskEntry{st: taskState{
		id: "t3", queued: true, stage: "排队中", filename: "c.ts",
		m3u8URL: "https://x/c.m3u8", started: now,
	}}

	saveState()

	// 模拟进程重启：清空注册表后从磁盘恢复
	tasks = map[string]*taskEntry{}
	seqID = 0
	loadState()

	if len(tasks) != 3 {
		t.Fatalf("恢复任务数=%d want 3", len(tasks))
	}

	t1 := tasks["t1"]
	if !t1.st.done || t1.st.containerID != "fmp4" || t1.st.stage != "已保存" {
		t.Fatalf("t1 状态损坏: %+v", t1.st)
	}
	if !reflect.DeepEqual(t1.st.fmp4Baseline, map[string]uint64{"1": 100}) {
		t.Fatalf("fmp4Baseline=%v", t1.st.fmp4Baseline)
	}
	if t1.st.fmp4Init == nil || t1.st.fmp4Init.mehdOff != info.mehdOff {
		t.Fatalf("fmp4Init 未恢复: %+v", t1.st.fmp4Init)
	}

	for _, id := range []string{"t2", "t3"} {
		te := tasks[id]
		if !te.st.paused || te.st.running || te.st.queued || te.st.stage != "已暂停" {
			t.Fatalf("%s 残留应转暂停: %+v", id, te.st)
		}
		if !te.st.resumable() {
			t.Fatalf("%s 恢复后应可续传", id)
		}
	}

	if seqID != 3 {
		t.Fatalf("seqID=%d want 3（推进避免撞号）", seqID)
	}
	_ = p
}

// TestLoadStateCanceledClearsFinalPath 已取消任务清空 finalPath（防御旧版本状态残留）。
func TestLoadStateCanceledClearsFinalPath(t *testing.T) {
	p := saveRestoreState(t)
	os.WriteFile(p, []byte(`{"version":1,"tasks":[{"id":"t1","canceled":true,"done":true,"finalPath":"C:\\old\\x.mp4"}]}`), 0644)

	loadState()

	t1, ok := tasks["t1"]
	if !ok {
		t.Fatal("t1 未恢复")
	}
	if t1.st.finalPath != "" {
		t.Fatalf("canceled 任务 finalPath=%q 应被清空", t1.st.finalPath)
	}
}

// TestLoadStateSkipsMissingFile 状态文件不存在时不报错、不产生任务。
func TestLoadStateSkipsMissingFile(t *testing.T) {
	saveRestoreState(t)
	loadState() // 无文件，静默返回
	if len(tasks) != 0 {
		t.Fatalf("无状态文件应无任务, got %d", len(tasks))
	}
}

// TestLoadStateGarbageFile 损坏 JSON 忽略并保持空注册表。
func TestLoadStateGarbageFile(t *testing.T) {
	p := saveRestoreState(t)
	os.WriteFile(p, []byte("{broken"), 0644)
	loadState()
	if len(tasks) != 0 {
		t.Fatalf("损坏状态文件应被忽略, got %d", len(tasks))
	}
}

// TestCollectPersistedSorted 落盘列表按 ID 稳定排序（避免 map 遍历顺序污染文件内容）。
func TestCollectPersistedSorted(t *testing.T) {
	old := tasks
	t.Cleanup(func() { tasks = old })
	tasks = map[string]*taskEntry{
		"t3": {st: taskState{id: "t3", done: true}},
		"t1": {st: taskState{id: "t1", done: true}},
		"t2": {st: taskState{id: "t2", paused: true}},
	}
	got := collectPersisted()
	if len(got) != 3 || got[0].ID != "t1" || got[1].ID != "t2" || got[2].ID != "t3" {
		t.Fatalf("排序错误: %v", idsOfPT(got))
	}
}

func idsOfPT(list []persistedTask) []string {
	out := make([]string, len(list))
	for i, pt := range list {
		out[i] = pt.ID
	}
	return out
}
