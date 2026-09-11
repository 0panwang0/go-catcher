// 文件名清洗与路径长度裁剪测试（Windows 可写性）。
package core

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeFilenameWindowsRules Windows 非法字符 / 结尾点空格 / 保留设备名。
// 视频标题来自网页 title，含 ":"、"?"、结尾 "." 都很常见，未清洗会直接写盘失败。
func TestSanitizeFilenameWindowsRules(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a.mp4:hidden", "a.mp4_hidden"},     // 冒号 = NTFS 备用数据流分隔符
		{"..\\..\\evil.ts", ".._.._evil.ts"}, // 路径穿越
		{"影片 <第1集>.ts", "影片 _第1集_.ts"},       // 尖括号
		{"a?b*c|d.ts", "a_b_c_d.ts"},         // 通配/管道
		{"tab\there.ts", "tabhere.ts"},       // 控制字符直接删
		{"name...", "name"},                  // 结尾点（Windows 会静默丢弃）
		{"name   ", "name"},                  // 结尾空格
		{"CON", "_CON"},                      // 保留设备名
		{"con.txt", "_con.txt"},              // 保留名带扩展名同样非法
		{"LPT1.ts", "_LPT1.ts"},              // 保留名（大写）
		{"NUL", "_NUL"},                      //
		{"COM10", "COM10"},                   // COM10 不是保留名（仅 1-9）
		{"CONSOLE.ts", "CONSOLE.ts"},         // 仅前缀相同不算保留名
		{"", "video.ts"},                     //
		{".", "video.ts"},                    //
		{"..", "video.ts"},                   //
		{"正常影片名.mp4", "正常影片名.mp4"},           // 合法名不动
	}
	for _, c := range cases {
		if got := sanitizeFilename(c.in); got != c.want {
			t.Errorf("sanitizeFilename(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestClipFilenameForDir 超长路径裁剪后仍 ≤ MAX_PATH，且不切碎 UTF-8 字符。
func TestClipFilenameForDir(t *testing.T) {
	dir := `C:\Users\someone\Videos\已下载的视频收藏`

	// 短名不动
	short := "影片.ts"
	if got := clipFilenameForDir(dir, short); got != short {
		t.Fatalf("短名不应被裁剪: %q", got)
	}

	// 超长中文名：裁剪后总长 ≤ MAX_PATH，且扩展名保留、无非法 UTF-8
	long := strings.Repeat("很长的影片标题", 30) + ".mp4"
	got := clipFilenameForDir(dir, long)
	if len(dir)+1+len(got) > maxPathLen {
		t.Fatalf("裁剪后仍然超长: dir=%d name=%d", len(dir), len(got))
	}
	if !strings.HasSuffix(got, ".mp4") {
		t.Fatalf("应保留扩展名: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("裁剪切碎了 UTF-8 字符: %q", got)
	}

	// 目录本身已极长：原样返回（交给系统给出明确错误，不静默改名）
	deep := `C:\` + strings.Repeat("verylongdirectoryname\\", 20)
	if got := clipFilenameForDir(deep, long); got != long {
		t.Fatalf("超长目录不应裁剪文件名: %q", got)
	}
}

// TestIsExecutableExt 落盘前拒绝可执行扩展名（与 /openfile 组合即代码执行）。
func TestIsExecutableExt(t *testing.T) {
	for _, e := range []string{".exe", ".BAT", ".ps1", ".lnk", ".scr"} {
		if !isExecutableExt(e) {
			t.Errorf("%s 应被判定为可执行", e)
		}
	}
	for _, e := range []string{".mp4", ".ts", ".m3u8", ""} {
		if isExecutableExt(e) {
			t.Errorf("%s 不应被判定为可执行", e)
		}
	}
}
