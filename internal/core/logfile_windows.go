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
	"time"
)

// logMaxBytes 单个日志文件上限；超出后轮转为 gocatcher.log.1（覆盖上一份）。
const logMaxBytes = 2 << 20 // 2MB

// logTailLines /log 端点回给前端的行数上限。
const logTailLines = 200

// logFilePath 日志文件路径：exe 同目录（与 config/state 一致，双击启动必定可写）。
func logFilePath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "gocatcher.log")
	}
	return "gocatcher.log"
}

// SetupFileLogging 把标准输出/错误 tee 一份到日志文件。
// 任何一步失败都静默放弃（只读介质、权限不足等），绝不因为日志问题影响启动。
func SetupFileLogging() {
	p := logFilePath()
	// 超过上限先轮转，避免日志无限膨胀
	if fi, err := os.Stat(p); err == nil && fi.Size() > logMaxBytes {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		f.Close()
		return
	}
	os.Stdout, os.Stderr = w, w
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				// 逐行加时间戳：一次 Write 通常含多行（banner、多行错误），
				// 只给首行加戳会让日志后半段失去时间信息。
				stamp := time.Now().Format("2006-01-02 15:04:05 ")
				var out strings.Builder
				for _, line := range strings.SplitAfter(string(buf[:n]), "\n") {
					if line == "" {
						continue
					}
					if line == "\n" {
						out.WriteString(line) // 纯空行不浪费前缀
						continue
					}
					out.WriteString(stamp)
					out.WriteString(line)
				}
				_, _ = f.Write([]byte(out.String()))
				// GUI 模式下 oldOut 是无控制台的无效句柄，写失败直接忽略
				_, _ = oldOut.Write(buf[:n])
				if oldErr != oldOut {
					_, _ = oldErr.Write(buf[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}()
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
