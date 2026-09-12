//go:build windows

// 诊断日志落盘。
//
// GUI 模式编译为 windowsgui 子系统，没有控制台：下载引擎里大量的 fmt.Printf
// 诊断信息（重试、回退、规范化、落盘失败原因…）全部进了黑洞，排障只能靠猜。
// 这里把标准输出/错误复制一份到 exe 同目录的 gocatcher.log，并给每段输出加
// 时间戳；已有控制台时保持一致（控制台照旧打印，文件留档）。
package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// logMaxBytes 单个日志文件上限；超出后轮转为 gocatcher.log.1（覆盖上一份）。
const logMaxBytes = 2 << 20 // 2MB

// logQueueCap 待写盘的日志行数上限。
//
// 队列存在的意义：把"读管道"和"写磁盘"解耦。队列满即丢弃新日志并计数，
// 绝不阻塞调用方——诊断日志丢了只是排障少几条线索，而卡住下载热路径是故障。
const logQueueCap = 8192

// logMaxWriteErrors 连续写盘失败多少次后放弃写盘（仍继续排空队列）。
// 磁盘满/只读时反复重试毫无意义，只会把 CPU 烧在必然失败的 Write 上。
const logMaxWriteErrors = 3

// logCloseTimeout 退出前排空队列的最长等待。日志值得等，但不值得让用户
// 盯着一个关不掉的窗口——排不掉就放弃，剩下的行本来就是"磁盘跟不上"的产物。
const logCloseTimeout = 2 * time.Second

// logTailLines /log 端点回给前端的行数上限。
const logTailLines = 200

// activeLog 当前落盘器（进程级单例），供退出路径排空队列。
//
// ⚠️ 这里**刻意**用一个包级变量，是 G1「禁止新增包级可变变量」的明确例外：
// 日志是被 SetupFileLogging 劫持 os.Stdout 换来的**进程级**设施，天然不属于某个
// Runtime（core 可以多实例化，但一个进程只有一个 stdout）。G1 要防的是引擎状态
// 散落到包级导致无法多实例化，这条不在此列。除 SetupFileLogging /
// CloseFileLogging 外，任何代码都不该碰它。
var activeLog atomic.Pointer[logWriter]

// logFilePath 日志文件路径：exe 同目录（与 config/state 一致，双击启动必定可写）。
func logFilePath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "gocatcher.log")
	}
	return "gocatcher.log"
}

// logWriter 异步落盘器：调用方只往队列里塞，磁盘 IO 全在这个 goroutine 里做。
//
// 为什么必须异步（P2-6）：SetupFileLogging 把 os.Stdout 换成管道后，所有
// fmt.Printf 都变成"往管道写"，而管道缓冲区只有几十 KB。原实现由读端直接同步
// f.Write，磁盘一慢（网络盘、磁盘满、杀软扫描）管道就写满，于是下载热路径上的
// 进度/重试 printf 全部阻塞——日志把下载拖死，是典型的"监控把系统压垮"。
//
// 现在读端只做内存搬运，磁盘慢只会让队列变满、丢弃新日志（并计数）。
//
// ch 永不 close：Close 走 quit 通道，于是"关闭之后仍有人 enqueue"只会撞上
// select 的 default 分支被丢弃，不会 panic（往已关闭的 channel 发送会 panic）。
type logWriter struct {
	path      string
	maxBytes  int64
	f         *os.File // 仅 run goroutine 触碰（Close 也不碰，见 run 的 defer）
	written   int64    // 当前文件已写字节（仅 run goroutine 触碰）
	writeErr  int      // 连续写盘失败次数（仅 run goroutine 触碰）
	tee       []*os.File
	ch        chan []byte
	dropped   atomic.Int64
	closeOnce sync.Once
	quit      chan struct{}
	done      chan struct{}
}

// newLogWriter 打开日志文件并启动落盘 goroutine。
// maxBytes<=0 时回落到 logMaxBytes（避免调用方传 0 导致每写一行就轮转）。
func newLogWriter(path string, maxBytes int64) (*logWriter, error) {
	if maxBytes <= 0 {
		maxBytes = logMaxBytes
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	var written int64
	if fi, err := f.Stat(); err == nil {
		written = fi.Size()
	}
	w := &logWriter{
		path:     path,
		maxBytes: maxBytes,
		f:        f,
		written:  written,
		ch:       make(chan []byte, logQueueCap),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go w.run()
	return w, nil
}

// enqueue 投递一段原始输出（非阻塞）。
//
// 用 select+default 而不是阻塞发送：这是"绝不反压调用方"的实现点。
// 队列满时丢弃并计数，由 flushDroppedNotice 在日志里留一条说明——排障时
// 「日志里没有」和「日志被丢了」是完全不同的结论，不能让人自己猜。
func (w *logWriter) enqueue(raw []byte) {
	select {
	case w.ch <- raw:
	default:
		w.dropped.Add(1)
	}
}

// pump 从管道读原始输出并复制入队；只做内存搬运，不碰磁盘也不碰控制台。
func (w *logWriter) pump(r *os.File) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			// Read 会复用 buf，必须复制一份再入队
			w.enqueue(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

// run 落盘主循环：队列 → 时间戳 → 文件 + 控制台 tee。
func (w *logWriter) run() {
	defer func() {
		if w.f != nil {
			_ = w.f.Close()
		}
		close(w.done)
	}()
	for {
		select {
		case raw := <-w.ch:
			w.emit(raw)
		case <-w.quit:
			// 退出前排空：非阻塞地把队列里剩下的写完再收工
			for {
				select {
				case raw := <-w.ch:
					w.emit(raw)
				default:
					return
				}
			}
		}
	}
}

// emit 写一条输出：文件拿加戳版，控制台 tee 拿原样版。
//
// tee 放在这里（而不是 pump 里）是刻意的：控制台阻塞只该拖慢落盘，
// 不该把管道堵回去连累 fmt.Printf。
func (w *logWriter) emit(raw []byte) {
	w.flushDroppedNotice()
	w.write(stampLines(raw, time.Now()))
	// GUI 模式下这些句柄是无效的，写失败直接忽略；有控制台时保持一致。
	for _, t := range w.tee {
		_, _ = t.Write(raw)
	}
}

// write 追加一段已加戳的文本，必要时先轮转。写盘失败累计到上限后放弃（置空 f）。
func (w *logWriter) write(b []byte) {
	if w.f == nil || len(b) == 0 {
		return
	}
	// 预判"写完这一笔会不会超限"，超了就先把当前文件轮转掉，这样单文件
	// 大小始终受 maxBytes 约束（含 written>0 的判断，避免单笔就超限时无限轮转）。
	if w.written > 0 && w.written+int64(len(b)) > w.maxBytes {
		w.rotate()
		if w.f == nil {
			return
		}
	}
	n, err := w.f.Write(b)
	w.written += int64(n)
	if err != nil {
		w.writeErr++
		if w.writeErr >= logMaxWriteErrors {
			_ = w.f.Close()
			w.f = nil
		}
		return
	}
	w.writeErr = 0
}

// rotate 轮转当前日志：关闭 → 改名为 .1 → 重新打开。
// 必须先把文件关掉再改名：Windows 上重命名一个仍被打开的句柄会失败。
func (w *logWriter) rotate() {
	_ = w.f.Close()
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		// 改名失败（被别的进程占着）就继续往原文件追加，总比停止记录好
		if f, oerr := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); oerr == nil {
			w.f, w.written = f, 0
			return
		}
		w.f = nil
		return
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		w.f = nil
		return
	}
	w.f, w.written = f, 0
}

// flushDroppedNotice 把"队列满丢弃了多少行"补写进日志。
func (w *logWriter) flushDroppedNotice() {
	d := w.dropped.Swap(0)
	if d == 0 {
		return
	}
	w.write(stampLines([]byte(fmt.Sprintf(
		"[日志] 输出速度超过写盘速度，已丢弃 %d 行（gocatcher.log 不完整）\n", d)), time.Now()))
}

// Close 停止落盘并尽量排空队列（最多等 logCloseTimeout）。
func (w *logWriter) Close() {
	w.closeOnce.Do(func() { close(w.quit) })
	select {
	case <-w.done:
	case <-time.After(logCloseTimeout):
	}
}

// stampLines 给每一行加时间戳前缀。
//
// 逐行加戳而不是整段加一个：一次 Write 常含多行（横幅、多行错误），
// 只给首行加戳会让日志后半段失去时间信息。纯空行不加前缀（省得刷屏）。
func stampLines(b []byte, now time.Time) []byte {
	stamp := now.Format("2006-01-02 15:04:05 ")
	var out strings.Builder
	out.Grow(len(b) + 32)
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if line == "" {
			continue
		}
		if line == "\n" {
			out.WriteString(line)
			continue
		}
		out.WriteString(stamp)
		out.WriteString(line)
	}
	return []byte(out.String())
}

// SetupFileLogging 把标准输出/错误 tee 一份到日志文件。
// 任何一步失败都静默放弃（只读介质、权限不足等），绝不因为日志问题影响启动。
func SetupFileLogging() {
	w, err := newLogWriter(logFilePath(), logMaxBytes)
	if err != nil {
		return
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	r, pw, err := os.Pipe()
	if err != nil {
		w.Close()
		return
	}
	os.Stdout, os.Stderr = pw, pw
	w.tee = append(w.tee, oldOut)
	if oldErr != oldOut {
		w.tee = append(w.tee, oldErr)
	}
	activeLog.Store(w)
	go w.pump(r)
}

// CloseFileLogging 退出前调用：把队列里剩余的日志写盘后关闭。
//
// 为什么必须有它：异步化之后"刚打印完就 os.Exit"的那几行会留在队列里——而
// 启动失败时打印的错误行恰恰是最需要留下的那几条。平时队列几乎是空的
// （写盘比输出快得多），所以这里通常瞬间返回。
func CloseFileLogging() {
	if w := activeLog.Swap(nil); w != nil {
		w.Close()
	}
}

// LogTail 返回日志文件最后最多 n 行。/log 端点用它把日志送到前端。
func LogTail(n int) (string, error) {
	data, err := os.ReadFile(logFilePath())
	if err != nil {
		return "", fmt.Errorf("读取日志失败: %w", err)
	}
	return tailLines(string(data), n), nil
}

// tailLines 取文本最后最多 n 行（n<=0 或超上限时取 logTailLines 行）。
func tailLines(s string, n int) string {
	if n <= 0 || n > logTailLines {
		n = logTailLines
	}
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}
