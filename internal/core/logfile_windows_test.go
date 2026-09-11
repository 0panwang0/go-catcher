//go:build windows

// 日志尾部读取测试（GUI 无控制台时 /log 是排障唯一入口）。
package core

import (
	"strings"
	"testing"
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
	// n 非法时回退到 logTailLines 上限
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
	for i := 0; i < logTailLines*3; i++ {
		big.WriteString("line\n")
	}
	got := tailLines(big.String(), logTailLines)
	if n := strings.Count(got, "\n"); n != logTailLines {
		t.Fatalf("应恰好返回 %d 行, got %d", logTailLines, n)
	}
}
