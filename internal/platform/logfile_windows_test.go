//go:build windows

// 日志尾部读取 + 异步落盘器测试（GUI 无控制台时 /log 是排障唯一入口）。
package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTailLines(t *testing.T) {
	long := strings.Join([]string{"l1", "l2", "l3", "l4", "l5"}, "\n") + "\n"

	if got := tailLines(long, 2); got != "l4\nl5\n" {
		t.Fatalf("取最后 2 行 = %q", got)
	}
	// 行数不足时全给
	if got := tailLines("a\nb\n", 100); got != "a\nb\n" {
		t.Fatalf("行数不足应全给 = %q", got)
	}
	// n 非法时回退到 LogTailLines 上限
	if got := tailLines("a\nb\n", 0); got != "a\nb\n" {
		t.Fatalf("n=0 应回退上限 = %q", got)
	}
	// 空文件
	if got := tailLines("", 10); got != "" {
		t.Fatalf("空内容应返回空串 = %q", got)
	}
	// 无结尾换行
	if got := tailLines("a\nb", 5); got != "a\nb\n" {
		t.Fatalf("无尾换行应补上 = %q", got)
	}

	// 超大输入按行数截断，不裁剪行内容
	var big strings.Builder
	for i := 0; i < LogTailLines*3; i++ {
		big.WriteString("line\n")
	}
	got := tailLines(big.String(), LogTailLines)
	if n := strings.Count(got, "\n"); n != LogTailLines {
		t.Fatalf("应恰好返回 %d 行, got %d", LogTailLines, n)
	}
}

// TestStampLines 逐行加戳（只给首行加戳会让日志后半段失去时间信息）。
func TestStampLines(t *testing.T) {
	at := time.Date(2026, 9, 12, 9, 30, 5, 0, time.Local)
	got := string(stampLines([]byte("第一行\n第二行\n\n尾行无换行"), at))

	want := "2026-09-12 09:30:05 第一行\n" +
		"2026-09-12 09:30:05 第二行\n" +
		"\n" +
		"2026-09-12 09:30:05 尾行无换行"
	if got != want {
		t.Fatalf("加戳结果不对:\n got %q\nwant %q", got, want)
	}

	if s := string(stampLines(nil, at)); s != "" {
		t.Errorf("空输入应返回空: %q", s)
	}
}

// TestLogWriterDropsInsteadOfBlocking 队列满时必须丢弃并计数，绝不阻塞。
//
// 这是 P2-6 的核心判据：原实现里磁盘慢会把管道写满、进而阻塞所有 fmt.Printf
// （包括下载热路径）。这里不启动落盘 goroutine，队列填满后再投递——若 enqueue
// 是阻塞发送，本用例会直接挂住（超时失败），也就证明不了"不反压"。
func TestLogWriterDropsInsteadOfBlocking(t *testing.T) {
	w := &logWriter{ch: make(chan []byte, 1), done: make(chan struct{})}

	w.enqueue([]byte("占满队列"))
	w.enqueue([]byte("溢出1"))
	w.enqueue([]byte("溢出2"))

	if d := w.dropped.Load(); d != 2 {
		t.Fatalf("应丢弃 2 行，得到 %d", d)
	}
	if len(w.ch) != 1 {
		t.Fatalf("队列应恰好保留 1 行，得到 %d", len(w.ch))
	}
}

// TestLogWriterReportsDroppedLines 丢弃必须留下痕迹。
// 排障时「日志里没有」和「日志被丢了」是完全不同的结论。
func TestLogWriterReportsDroppedLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	w := &logWriter{
		path: p, maxBytes: 1 << 20, f: f,
		ch: make(chan []byte, 1), done: make(chan struct{}),
	}
	w.enqueue([]byte("a"))
	w.enqueue([]byte("b")) // 溢出
	w.enqueue([]byte("c")) // 溢出
	w.flushDroppedNotice()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "丢弃 2 行") {
		t.Fatalf("日志里应说明丢弃了多少行，得到: %q", data)
	}
}

// TestLogWriterWritesStampedLines 异步落盘：Close 后内容应完整落到文件。
func TestLogWriterWritesStampedLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gocatcher.log")
	w, err := newLogWriter(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	w.enqueue([]byte("hello\nworld\n"))
	w.Close() // Close 会等队列写完，因此无需轮询等待

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "hello\n") || !strings.Contains(got, "world\n") {
		t.Fatalf("两行都应落盘: %q", got)
	}
	// 每一行都要有时间戳
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("应恰好两行: %q", got)
	}
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if len(line) < 20 || line[4] != '-' || line[13] != ':' {
			t.Fatalf("每行都应有时间戳前缀: %q", line)
		}
	}
}

// TestLogWriterRotatesAtRuntime 长跑会话也要轮转（原来只在启动时轮转一次，
// 于是跑几天就是一个几百 MB 的日志）。
func TestLogWriterRotatesAtRuntime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher.log")
	// 上限压到 200 字节，方便在测试里触发轮转
	w, err := newLogWriter(p, 200)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 60) + "\n"
	for i := 0; i < 10; i++ {
		w.enqueue([]byte(line))
	}
	w.Close()

	rotated := p + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("超过上限后应轮转出 %s: %v", rotated, err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 200 {
		t.Fatalf("轮转后当前日志应回到上限内，得到 %d 字节", fi.Size())
	}
	// 轮转不是丢数据：.1 里应该有内容
	rfi, err := os.Stat(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if rfi.Size() == 0 {
		t.Fatal("轮转文件不该是空的")
	}
}

// TestNewLogWriterSeedsSizeForExistingFile 启动时已有超限日志：第一次写入
// 就应轮转（原来只在 SetupFileLogging 里查一次，且轮转逻辑与运行期是两份）。
func TestNewLogWriterSeedsSizeForExistingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher.log")
	if err := os.WriteFile(p, []byte(strings.Repeat("old\n", 100)), 0644); err != nil {
		t.Fatal(err)
	}
	w, err := newLogWriter(p, 64)
	if err != nil {
		t.Fatal(err)
	}
	w.enqueue([]byte("fresh\n"))
	w.Close()

	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("已有超限日志应在首次写入时轮转: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "fresh") {
		t.Fatalf("新日志应写进轮转后的文件: %q", data)
	}
	if strings.Contains(string(data), "old") {
		t.Fatalf("旧内容应留在 .1 里: %q", data)
	}
}

// TestLogWriterStopsAfterWriteFailures 磁盘写不动时应该放弃写盘而不是反复重试。
// 用一个已关闭的句柄模拟：写入必然失败。
func TestLogWriterStopsAfterWriteFailures(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gocatcher.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	w := &logWriter{path: p, maxBytes: 1 << 20, f: f}

	for i := 0; i < logMaxWriteErrors; i++ {
		w.write([]byte("x\n"))
	}
	if w.f != nil {
		t.Fatal("连续写盘失败到上限后应停止写盘（置空句柄）")
	}
	// 再写不能 panic（f 为 nil 直接返回）
	w.write([]byte("y\n"))
}

// TestCloseFileLoggingFlushesQueue 退出前的排空。
// "刚打印完就 os.Exit"是最需要那条日志的场景，不能让它留在队列里消失。
func TestCloseFileLoggingFlushesQueue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gocatcher.log")
	w, err := newLogWriter(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	old := activeLog.Swap(w)
	t.Cleanup(func() { activeLog.Store(old) })

	w.enqueue([]byte("最后一行\n"))
	CloseFileLogging()

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "最后一行") {
		t.Fatalf("关闭前队列里的日志应已落盘: %q", data)
	}

	// 幂等：重复调用不能 panic（activeLog 已被换出，第二次是空操作）
	CloseFileLogging()
	// 关闭之后再有输出也不能 panic（ch 永不 close，落到 default 分支被丢弃）
	w.enqueue([]byte("关闭之后的输出\n"))
}
