// 任务状态机测试：resumable/snapshot/failTask/ID 生成/JSON/清理/列表排序。
package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTaskStateResumable 恢复条件判定。
func TestTaskStateResumable(t *testing.T) {
	cases := []struct {
		name string
		st   taskState
		want bool
	}{
		{"未结束可恢复", taskState{done: false}, true},
		{"暂停可恢复", taskState{done: true, paused: true, canceled: false}, true},
		{"取消不可恢复", taskState{done: true, canceled: true}, false},
		{"完成不可恢复", taskState{done: true}, false},
	}
	for _, c := range cases {
		if got := c.st.resumable(); got != c.want {
			t.Fatalf("%s: resumable=%v want %v", c.name, got, c.want)
		}
	}
}

// TestNewTaskID 自增与并发安全。
func TestNewTaskID(t *testing.T) {
	old := testStd.seqID
	t.Cleanup(func() { testStd.seqID = old })
	testStd.seqID = 0
	if id := testStd.newTaskID(); id != "t1" {
		t.Fatalf("newTaskID=%q want t1", id)
	}
	if id := testStd.newTaskID(); id != "t2" {
		t.Fatalf("newTaskID=%q want t2", id)
	}
}

// TestSnapshotOpenPath openPath 按磁盘实况计算：成品 → .part → 空。
func TestSnapshotOpenPath(t *testing.T) {
	dir := t.TempDir()

	final := filepath.Join(dir, "a.mp4")
	os.WriteFile(final, []byte("x"), 0644)
	te := &taskEntry{rt: testStd, st: taskState{done: true, finalPath: final}}
	if s := snapshot(te); s.openPath != final {
		t.Fatalf("成品存在 openPath=%q want %q", s.openPath, final)
	}

	part := filepath.Join(dir, "b.ts.part")
	os.WriteFile(part, []byte("x"), 0644)
	te2 := &taskEntry{rt: testStd, st: taskState{finalPath: filepath.Join(dir, "b.ts")}}
	if s := snapshot(te2); s.openPath != part {
		t.Fatalf(".part 存在 openPath=%q want %q", s.openPath, part)
	}

	te3 := &taskEntry{rt: testStd, st: taskState{finalPath: filepath.Join(dir, "none.ts")}}
	if s := snapshot(te3); s.openPath != "" {
		t.Fatalf("都不存在 openPath=%q want 空", s.openPath)
	}
}

// TestSnapshotReadsJobProgress 有 job 时进度取原子值而非缓存。
func TestSnapshotReadsJobProgress(t *testing.T) {
	job := &dlJob{rt: testStd}
	job.setSeg(7, 10)
	te := &taskEntry{rt: testStd, st: taskState{id: "t1"}, job: job}
	s := snapshot(te)
	if s.segDone != 7 || s.segTot != 10 {
		t.Fatalf("segDone=%d segTot=%d want 7,10", s.segDone, s.segTot)
	}
}

// TestFailTask 失败状态转换：保留元信息与 finalPath（重试的 pipeline 靠它定位 .part）。
func TestFailTask(t *testing.T) {
	te := &taskEntry{rt: testStd, st: taskState{running: true, queued: true, finalPath: "x.mp4"}}
	failTask(te, "网络错误")
	s := te.st
	if !s.done || s.running || s.queued || s.stage != "失败" || s.errorMsg != "网络错误" || s.finalPath != "x.mp4" {
		t.Fatalf("failTask 后状态异常: %+v", s)
	}
}

// TestTaskStateJSON 进度百分比与文件失效标记。
func TestTaskStateJSON(t *testing.T) {
	started := time.Date(2026, 9, 6, 10, 0, 0, 0, time.Local)
	s := taskState{
		id: "t9", stage: "下载分片中", segDone: 5, segTot: 10,
		started: started, done: true, finalPath: "x.mp4", openPath: "y.mp4",
	}
	raw, err := json.Marshal(toTaskStateDTO(s))
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	j := string(raw)
	for _, want := range []string{`"id":"t9"`, `"pct":50`, `"stage":"下载分片中"`, `"fileMissing":true`, `"done":true`} {
		if !strings.Contains(j, want) {
			t.Fatalf("JSON 缺少 %s: %s", want, j)
		}
	}
}

// TestTaskStateJSONEscapesControlChars 远端异常串里的控制字符必须被转义成
// JSON 合法形式（\u00XX）。旧实现用 %q 会产出 \x01，前端 JSON.parse 直接抛错。
func TestTaskStateJSONEscapesControlChars(t *testing.T) {
	s := taskState{id: "t1", stage: "失败", done: true, errorMsg: "HTTP 403\x01\x1b[0m <script>"}
	raw, err := json.Marshal(toTaskStateDTO(s))
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	if bytes.ContainsAny(raw, "\x01\x1b") {
		t.Fatalf("输出含未转义控制字符: %q", raw)
	}
	var back struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, raw)
	}
	if back.Error != s.errorMsg {
		t.Fatalf("error 往返不一致: %q", back.Error)
	}
}

// TestPruneOldTasks 超过上限清理最老已完成任务，运行中永不清理。
func TestPruneOldTasks(t *testing.T) {
	old := testStd.tasks
	t.Cleanup(func() { testStd.tasks = old })
	testStd.tasks = map[string]*taskEntry{}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < maxKeptTasks+10; i++ {
		id := fmt.Sprintf("t%03d", i)
		testStd.tasks[id] = &taskEntry{rt: testStd, st: taskState{done: true, started: base.Add(time.Duration(i) * time.Minute)}}
	}
	testStd.tasks["running"] = &taskEntry{rt: testStd, st: taskState{done: false, started: base}}

	testStd.pruneOldTasks()

	if len(testStd.tasks) != maxKeptTasks+1 {
		t.Fatalf("清理后 len=%d want %d", len(testStd.tasks), maxKeptTasks+1)
	}
	if _, ok := testStd.tasks["running"]; !ok {
		t.Fatal("运行中任务被误清理")
	}
	if _, ok := testStd.tasks["t000"]; ok {
		t.Fatal("最老任务 t000 未被清理")
	}
	if _, ok := testStd.tasks["t009"]; ok {
		t.Fatal("t009 应被清理（早于保留窗口）")
	}
	if _, ok := testStd.tasks["t010"]; !ok {
		t.Fatal("t010 应保留")
	}
	if _, ok := testStd.tasks["t109"]; !ok {
		t.Fatal("最新任务应保留")
	}
}

// TestListTasksOrder 未结束在前、组内按开始时间新→旧、同时刻按 id 升序。
func TestListTasksOrder(t *testing.T) {
	old := testStd.tasks
	t.Cleanup(func() { testStd.tasks = old })
	now := time.Now()
	testStd.tasks = map[string]*taskEntry{
		"t1": {st: taskState{id: "t1", done: true, started: now.Add(-3 * time.Hour)}},
		"t2": {st: taskState{id: "t2", paused: true, done: false, started: now.Add(-2 * time.Hour)}},
		"t3": {st: taskState{id: "t3", done: true, started: now.Add(-1 * time.Hour)}},
		"t4": {st: taskState{id: "t4", done: true, started: now.Add(-1 * time.Hour)}}, // 与 t3 同时刻
	}
	got := testStd.listTasks()
	wantOrder := []string{"t2", "t3", "t4", "t1"} // t3/t4 同时刻按 id 升序
	if len(got) != 4 {
		t.Fatalf("len=%d want 4", len(got))
	}
	for i, w := range wantOrder {
		if got[i].id != w {
			t.Fatalf("顺序[%d]=%s want %s (全部=%v)", i, got[i].id, w, idsOf(got))
		}
	}
}

func idsOf(list []taskState) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.id
	}
	return out
}
