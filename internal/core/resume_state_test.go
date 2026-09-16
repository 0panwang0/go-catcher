// 续传状态可用性测试：容器依赖跨分片状态时（fMP4 的 tfdt 基准），
// 状态拿不到就必须显式拒绝续传 —— 否则新分片从头计时、与已录内容在时间轴上
// 重叠，产出"能播但内容是错的"文件（本项目头号缺陷形态）。
package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 合法的 fMP4 续传快照（v=1 + 非空 baseline）。这是跨版本持久化契约，
// 内容全为字面量：fmp4 包若改了字段名，这里会红，提醒"旧任务的快照将失效"。
const validFMP4Snapshot = `{"v":1,"baseline":{"1":1000},"end":{"1":96000}}`

func TestRestoreContainerRejectsUnusableState(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "x.mp4.part")
	// 头部是真 fMP4 分片特征（moof），供无 containerID 时的嗅探路径识别
	if err := os.WriteFile(part, []byte("\x00\x00\x00\x18moofDATA"), 0644); err != nil {
		t.Fatalf("准备 .part: %v", err)
	}

	cases := []struct {
		name        string
		containerID string
		state       []byte
		wantErr     bool
	}{
		{"fMP4 无状态字节", "fmp4", nil, true},
		{"fMP4-map 无状态字节", "fmp4-map", nil, true},
		{"fMP4 空字节", "fmp4", []byte{}, true},
		{"fMP4 旧格式快照（无版本号）", "fmp4", []byte(`{"baseline":{"1":1000}}`), true},
		{"fMP4 未知版本", "fmp4", []byte(`{"v":99,"baseline":{"1":1000}}`), true},
		{"fMP4 版本对但没有基准", "fmp4", []byte(`{"v":1,"end":{"1":96000}}`), true},
		{"fMP4 损坏字节", "fmp4", []byte("{broken"), true},
		{"fMP4 合法快照", "fmp4", []byte(validFMP4Snapshot), false},
		{"fMP4-map 合法快照", "fmp4-map", []byte(validFMP4Snapshot), false},
		{"嗅探到 fMP4 但无状态", "", nil, true},
		{"TS 无状态字节（无状态容器）", "ts", nil, false},
	}
	for _, c := range cases {
		job := &dlJob{rt: testStd}
		err := restoreContainer(job, c.containerID, part, c.state)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
		if c.wantErr && err != nil {
			// 提示要能让用户知道该怎么办，而不是一句"失败"
			if !strings.Contains(err.Error(), "重新开始") {
				t.Errorf("%s: 错误信息应给出重新开始的出路: %q", c.name, err.Error())
			}
			continue
		}
		// 无论成功失败，容器与状态对象都要建好：收尾路径（忽略错误的那条）
		// 仍要靠它们回填时长、写 init 段。
		if job.container == nil {
			t.Errorf("%s: 容器应被识别", c.name)
		}
	}
}

// TestRestoreContainerTSHasNoState 无状态容器不持有状态对象 —— 这是
// "续传是否需要状态"的判据本身（NewState == nil）。
func TestRestoreContainerTSHasNoState(t *testing.T) {
	job := &dlJob{rt: testStd}
	if err := restoreContainer(job, "ts", filepath.Join(t.TempDir(), "x.ts.part"), nil); err != nil {
		t.Fatalf("TS 续传不应报错: %v", err)
	}
	if job.container == nil || job.container.ID != "ts" {
		t.Fatalf("应识别为 ts 容器: %+v", job.container)
	}
	if job.norm != nil {
		t.Fatal("TS 是无状态容器，不应创建状态对象")
	}
}
