// R5 / R6：CLI 单次下载的输出路径问题。
//
// R5：`-o` 支持带子目录，但建目录原先只发生在最后一步 emitOutput —— 而 HLS 路径
// 第 3 步就要 writeInitSegmentFor（O_CREATE，不建父目录）⇒ `-o out/video.ts` 必然失败。
// R6：直链下载的输出扩展名必须跟随 URL（.mp4 不能存成默认的 .ts）。
package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureOutputDirCreatesParent：成对用例之一 —— 带子目录时必须真把父目录建出来
// （判据直接取"目录存在"，删掉 MkdirAll 必红）。
//
// 为什么直调 ensureOutputDir 而不跑整个 RunCLI：RunCLI 会对 URL 发真实请求 +
// 重试退避（十几秒），本用例只关心建目录这件事，没必要拖慢整个测试套。
func TestEnsureOutputDirCreatesParent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sub", "nested", "video.ts")

	if err := ensureOutputDir(out); err != nil {
		t.Fatalf("ensureOutputDir 返回错误: %v", err)
	}
	fi, err := os.Stat(filepath.Dir(out))
	if err != nil || !fi.IsDir() {
		t.Fatalf("没有创建 -o 的父目录（-o 带子目录会失败，R5）: err=%v", err)
	}
}

// TestEnsureOutputDirPlainNameNoOp：成对用例之二 —— 纯文件名（无目录）时 filepath.Dir
// 返回 "."，不得误建。缺这条则"任何输入都 MkdirAll 一堆怪目录"测不出来。
func TestEnsureOutputDirPlainNameNoOp(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	before, _ := os.ReadDir(cwd)

	if err := ensureOutputDir("video.ts"); err != nil {
		t.Fatalf("纯文件名不应报错: %v", err)
	}
	after, _ := os.ReadDir(cwd)
	if len(after) != len(before) {
		t.Fatalf("纯文件名被误建了目录：前 %d 项，后 %d 项", len(before), len(after))
	}
}

// TestRunCLIUsesEnsureOutputDir：接线断言。只测纯函数的话，RunCLI 里那行调用被
// 删掉照样全绿（R5 的原始形态正是"建目录排在太后面"）—— 必须钉住"RunCLI 真的调了它"。
func TestRunCLIUsesEnsureOutputDir(t *testing.T) {
	body := strings.Join(sourceFuncBody(t, "cli.go", "func RunCLI(o CLIOptions) int"), "\n")
	if !strings.Contains(body, "ensureOutputDir") {
		t.Error("RunCLI 没有调用 ensureOutputDir —— -o 带子目录会在写 init 段时失败（R5）")
	}
}

// TestCorrectedDirectNameFollowsURL：直链输出的扩展名规则（纯函数）。
func TestCorrectedDirectNameFollowsURL(t *testing.T) {
	cases := []struct{ in, url, want string }{
		{"output.ts", "https://cdn.x/v.mp4", "output.mp4"},
		{"output.ts", "https://cdn.x/v.MP4", "output.mp4"}, // 大小写不敏感
		{"my.video.ts", "https://cdn.x/a/v.mkv", "my.video.mkv"},
		{"already.mp4", "https://cdn.x/v.mp4", "already.mp4"},  // 已是目标扩展名 ⇒ 不动
		{"output.ts", "https://cdn.x/index.m3u8", "output.ts"}, // 认不出的后缀 ⇒ 不动
		{"output.ts", "https://cdn.x/video", "output.ts"},      // 无后缀 ⇒ 不动
	}
	for _, c := range cases {
		if got := correctedDirectName(c.in, c.url); got != c.want {
			t.Errorf("correctedDirectName(%q, %q) = %q, want %q", c.in, c.url, got, c.want)
		}
	}
}

// TestRunCLIDirectBranchUsesCorrectedName：接线断言。只验纯函数的话，
// CLI 直链分支里那行调用被删掉照样全绿（R6 的原始形态就是"规则写好了没人调"）。
func TestRunCLIDirectBranchUsesCorrectedName(t *testing.T) {
	body := strings.Join(sourceFuncBody(t, "cli.go", "func RunCLI(o CLIOptions) int"), "\n")
	if !strings.Contains(body, "correctedDirectName") {
		t.Error("CLI 直链分支没有调用 correctedDirectName —— mp4 会存成 .ts（R6）")
	}
}
