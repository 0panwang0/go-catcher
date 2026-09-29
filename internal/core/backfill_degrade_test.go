// R4：容器收尾（fMP4 回填 mehd/mvhd 总时长）失败时，成品**已合法可播**，
// 任务必须仍是「已完成」且 errorMsg 留空，只在 stage 里如实说明"时长信息缺失"。
//
// 缺陷形态（原状）：backfill 失败只 fmt.Printf 一行，随后照样置 done=true /
// stage="已保存" / errorMsg="" —— 文件其实"播放器预知不了时长、不能拖动进度条"
// （见 internal/fmp4/initseg.go 文件头），属功能性退化却报成功。
//
// 修法的两条边界（都靠本用例钉住）：
//   - 不能判成失败：那会让用户去点「取消」把可播文件删掉（本项目红线）；
//   - 不能写 errorMsg：前端 badge 先判 t.error（web/index.html 的 badge），
//     会把已完成的记录显示成「失败」，还算进"失败"统计。
package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// normFinishStub 是 NormState 的测试桩：Finish 按 fail 决定成功 / 失败。
// NormState 是接口（container.go），所以这里能真注入失败 —— 不用为了造场景去
// 拼一个装不下 duration 的 mehd 文件。
type normFinishStub struct{ fail bool }

func (n normFinishStub) Snapshot() []byte                   { return nil }
func (n normFinishStub) Restore([]byte) bool                { return false }
func (n normFinishStub) Normalize(b []byte) ([]byte, error) { return b, nil }
func (n normFinishStub) Finish(string) error {
	if n.fail {
		return fmt.Errorf("模拟：mehd 装不下 duration")
	}
	return nil
}

// runFinalizeWithNorm 跑一次点播正常收尾，返回收尾后的任务快照。
func runFinalizeWithNorm(t *testing.T, fail bool) taskState {
	t.Helper()
	dir := t.TempDir()
	final := filepath.Join(dir, "out.ts")
	part := final + ".part"
	if err := os.WriteFile(part, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	rt := newRuntime()
	rt.statePath = filepath.Join(dir, "state.json")
	rt.persistDisabled = true // 本用例不关心落盘
	te := &taskEntry{rt: rt, st: taskState{id: "t1", finalPath: final, filename: "out.ts"}}
	job := &dlJob{rt: rt, norm: normFinishStub{fail: fail}}
	if err := finalizeRecording(te, job, part, finalizeCause{outcome: finalizeComplete}); err != nil {
		t.Fatalf("收尾不应返回错误（文件已落盘）: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("成品必须保留: %v", err)
	}
	return snapshot(te)
}

func TestFinalizeBackfillFailureIsDegradeNotFail(t *testing.T) {
	got := runFinalizeWithNorm(t, true)
	if !got.done {
		t.Error("收尾失败后任务仍应是「已完成」（文件可播，只是时长信息缺失）")
	}
	if got.errorMsg != "" {
		t.Errorf("errorMsg 必须留空（否则前端显示「失败」并计入失败统计）: %q", got.errorMsg)
	}
	if !strings.Contains(got.stage, "时长信息缺失") {
		t.Errorf("stage 应如实说明时长信息缺失，实际: %q", got.stage)
	}
}

// 成对的另一半：收尾成功时 stage 不得带"时长信息缺失" —— 少了这条，
// 把降级说明无条件拼上去也能让上一条"绿"。
func TestFinalizeBackfillSuccessKeepsPlainStage(t *testing.T) {
	got := runFinalizeWithNorm(t, false)
	if !got.done {
		t.Error("正常收尾后任务应为已完成")
	}
	if strings.Contains(got.stage, "时长信息缺失") {
		t.Errorf("收尾成功时 stage 不该出现降级说明，实际: %q", got.stage)
	}
	if got.stage != "已保存" {
		t.Errorf("点播正常收尾 stage 应为「已保存」，实际: %q", got.stage)
	}
}
