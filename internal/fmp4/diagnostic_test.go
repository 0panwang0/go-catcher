// P3-7 回归：诊断输出不得落到进程 stdout。
//
// fmp4 是纯库，调用方的 stdout 未必是给人看的地方：
//   - 原生消息宿主模式下它就是与浏览器通信的协议通道，写诊断等于破坏协议；
//   - 无控制台的宿主里写进去就是黑洞，还查不出"为什么排障日志是空的"。
//
// 所以库内诊断只能走注入的出口（NewStateWithLogger）；没注入即丢弃。
// 这条约束靠"扫描源码找 fmt.Print"是守不住的，必须由测试钉住行为：
// 把 os.Stdout 换成管道，跑遍所有会产生诊断的路径，断言一个字节都没写。
//
// 覆盖范围说明：normalizeFMP4Segment 的 recover 兜底分支（`[norm] 规范化内部错误`）
// 用的是同一个 logDiagnostic，但触发它需要一个真能 panic 的输入——解析层已全部加固，
// 构造不出来，故由 FuzzNormalize 兜底，这里不重复。
package fmp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout 把进程 stdout 换成管道；返回值在跑完被测代码后调一次，
// 负责还原 os.Stdout 并给出期间写入的全部内容。
//
// os.Stdout 是包级变量，每个测试都同步独占它——本包没有 t.Parallel。
func captureStdout(t *testing.T) func() string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	return func() string {
		os.Stdout = old
		_ = w.Close() // 关掉写端，读端 Read 才会返回
		return <-done
	}
}

// shortMehdFile 写一个"声明长度只有 12 字节的 mehd"文件：装得下 verflags、
// 装不下 duration 字段，触发 backfillDurations 的 `[disk] WARN` 分支。
// 文件尾部跟着 4 个哨兵字节，用来确认没被写到盒外。
func shortMehdFile(t *testing.T) string {
	t.Helper()
	short := make([]byte, 12)
	binary.BigEndian.PutUint32(short, 12)
	copy(short[4:], "mehd")
	path := filepath.Join(t.TempDir(), "short-mehd.bin")
	if err := os.WriteFile(path, append(short, 0xAA, 0xBB, 0xCC, 0xDD), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// mehdInfoFor 构造一个"可回填"的 init 解析结果（实际回填会被短盒校验挡下）。
// mvhdOff 必须显式给 -1：零值 0 是合法偏移，漏给会额外触发一条 mvhd 位置的 WARN
// （生产侧由 prepareInit / restore 保证，见 fmp4InitInfo 的注释）。
func mehdInfoFor() *fmp4InitInfo {
	return &fmp4InitInfo{movieTS: 1000, trackTS: map[uint32]uint32{1: 90000}, mehdOff: 0, mvhdOff: -1}
}

// TestLibraryNeverWritesStdout 不注入口时，所有会产生诊断的路径都必须静默
// （而不是"写成 stdout 让调用方自己接"）。
func TestLibraryNeverWritesStdout(t *testing.T) {
	path := shortMehdFile(t)
	take := captureStdout(t)

	// 1) 畸形 moof：traf 字段区截断 → parseMoof 报错 → 诊断（应丢弃）
	if _, err := NewState().Normalize(malformedTrunMoof()); err != nil {
		t.Fatalf("畸形分片不应上抛错误: %v", err)
	}
	// 2) 畸形 moof：tfhd 声明与盒内布局不符 → 同上
	if _, err := NewState().Normalize(shortTfhdMoof()); err != nil {
		t.Fatalf("畸形分片不应上抛错误: %v", err)
	}
	// 3) 截断的 init 段：install 路径（pipeline 的 writeInitSegmentFor 走这里）
	init := buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000})
	if _, err := NewState().Normalize(append([]byte{}, init[:len(init)/3]...)); err != nil {
		t.Fatalf("截断 init 不应上抛错误: %v", err)
	}
	// 4) 收尾回填：短盒 → `[disk] WARN`（应丢弃）
	st := NewState()
	st.end[1] = 90000
	if err := backfillDurations(path, mehdInfoFor(), st); err != nil {
		t.Fatalf("短盒应跳过而非报错: %v", err)
	}

	if got := take(); got != "" {
		t.Fatalf("库往 stdout 写了 %d 字节：\n%s", len(got), got)
	}
}

// TestDiagnosticGoesToInjectedLogger 注入出口后诊断恰好一条、且仍不碰 stdout。
//
// "恰好一条"是刻意的强断言：诊断重复输出会让日志被同一条警告刷屏，
// 而排障时最需要的是能一眼数清"这个分片出过几次问题"。
func TestDiagnosticGoesToInjectedLogger(t *testing.T) {
	var lines []string
	take := captureStdout(t)
	st := NewStateWithLogger(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	in := malformedTrunMoof()
	out, err := st.Normalize(in)
	if err != nil {
		t.Fatalf("不应上抛错误: %v", err)
	}
	if got := take(); got != "" {
		t.Fatalf("注入出口后仍写了 stdout：%s", got)
	}
	if len(lines) != 1 {
		t.Fatalf("诊断条数=%d want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "[norm]") {
		t.Fatalf("诊断缺 [norm] 前缀: %q", lines[0])
	}
	// 诊断归诊断，放行语义不变：畸形输入原样返回
	if !bytes.Equal(out, in) {
		t.Fatalf("结构异常的分片应原样放行：%d 字节 → %d 字节", len(in), len(out))
	}
}

// TestBackfillDiagnosticGoesToInjectedLogger `[disk] WARN` 分支同样只走注入出口。
func TestBackfillDiagnosticGoesToInjectedLogger(t *testing.T) {
	path := shortMehdFile(t)
	var lines []string
	take := captureStdout(t)
	st := NewStateWithLogger(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	st.end[1] = 90000

	if err := backfillDurations(path, mehdInfoFor(), st); err != nil {
		t.Fatalf("短盒应跳过而非报错: %v", err)
	}
	if got := take(); got != "" {
		t.Fatalf("写了 stdout：%s", got)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "[disk]") {
		t.Fatalf("诊断=%v want 一条含 [disk] 的警告", lines)
	}
	// 校验失败就不该动文件：哨兵字节必须原样
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, []byte{0xAA, 0xBB, 0xCC, 0xDD}) {
		t.Fatalf("盒外字节被改写: %x", got)
	}
}

// TestLogDiagnosticNilSafe nil 接收者与未注入口都不得 panic：recover 兜底里
// n 可能本身就是 nil（调用方传了空状态），那时再 panic 就没人接得住了。
func TestLogDiagnosticNilSafe(t *testing.T) {
	var nilState *normState
	nilState.logDiagnostic("nil 接收者 %d", 1)
	NewState().logDiagnostic("出口为 nil 时丢弃 %d", 2)
}
