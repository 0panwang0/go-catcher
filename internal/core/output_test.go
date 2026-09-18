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

// TestIsAllowedDownloadExt 落盘前只放行白名单内的扩展名（可执行扩展名与 /openfile
// 组合即是本机代码执行；黑名单列不完，所以反过来列白名单）。
func TestIsAllowedDownloadExt(t *testing.T) {
	// 白名单：本服务会产出的媒体/字幕类型，含容器修正后的结果（.mp4 等）
	for _, e := range []string{".ts", ".mp4", ".mkv", ".webm", ".m4s", ".AAC", ".srt", ".vtt"} {
		if !isAllowedDownloadExt(e) {
			t.Errorf("%s 应被放行", e)
		}
	}
	// 可执行类一律拒绝；空扩展名由调用方单独处理（补 .ts），这里也必须为 false
	for _, e := range []string{
		".exe", ".BAT", ".ps1", ".lnk", ".scr", ".msi", ".dll", ".sys", ".js",
		// 黑名单容易漏掉的这几类，白名单天然覆盖
		".pif", ".msc", ".inf", ".settingcontent-ms", ".search-ms", ".diagcab", ".url", ".reg",
		"", ".txt",
	} {
		if isAllowedDownloadExt(e) {
			t.Errorf("%s 不应被放行", e)
		}
	}
}

// TestSanitizeFilenameStripsBidiControls 显示欺骗类控制符必须被剔除。
//
// 形态：U+202E（RLO）之后的文本整段反向显示，于是 "video_4pm.mp4" 在界面上
// 显示成 "video_mp4.exe" 之类；它们不可见，靠肉眼看名字发现不了。扩展名白名单
// 限制了真正的危害，但"看着叫 A 实际叫 B"这一半得在名字里堵掉。
// 扰动点：删掉 sanitizeFilename 里的 isBidiControl 分支，本条会红。
func TestSanitizeFilenameStripsBidiControls(t *testing.T) {
	for _, r := range []rune{
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // LRE/RLE/PDF/LRO/RLO
		0x2066, 0x2067, 0x2068, 0x2069, // LRI/RLI/FSI/PDI
		0x200E, 0x200F, 0x061C, // LRM/RLM/ALM
	} {
		in := "a" + string(r) + "b.mp4"
		if got := sanitizeFilename(in); got != "ab.mp4" {
			t.Errorf("U+%04X 应被剔除：sanitizeFilename(%q)=%q want %q", r, in, got, "ab.mp4")
		}
	}

	// 对抗场景：反向覆盖让末尾看起来是 .mp4，真实扩展名被藏起来
	spoofed := "video_4pm\u202Eexe.mp4"
	if got := sanitizeFilename(spoofed); strings.ContainsAny(got, "\u202A\u202E") {
		t.Fatalf("欺骗名未被清理: %q", got)
	}
}

// TestSanitizeFilenameKeepsEmojiJoiners 不能一刀切删所有 Cf（格式）类字符：
// 零宽连接符 U+200D 是 emoji 组合序列的一部分，剔掉会把正常标题拆坏
// ——那属于"为了安全把正常功能也弄坏"。
func TestSanitizeFilenameKeepsEmojiJoiners(t *testing.T) {
	const family = "👨\u200d👩\u200d👧"
	if got := sanitizeFilename(family + ".mp4"); got != family+".mp4" {
		t.Fatalf("含零宽连接符的标题被改坏：got %q want %q", got, family+".mp4")
	}
}

// TestNormalizeOutputStripsBidiControls CLI 的 -o 路径也要滤 bidi 控制符。
//
// sanitizeFilename 只有 server 一个调用点，CLI 的 -o 走 normalizeOutput —— 若这层
// 不滤，浮层兜底命令（-o 由页面标题派生、站点可控）的 U+202E 能一路进到文件名。
// 但只滤 bidi、**不做** sanitizeFilename 的整套清洗：带目录的 -o 会被毁掉。
// 扰动点：删掉 normalizeOutput 里的 isBidiControl 分支，本条会红。
func TestNormalizeOutputStripsBidiControls(t *testing.T) {
	rt := &Runtime{outputFile: "out/vid\u202Eeo.ts"}
	if got := rt.normalizeOutput(); got != "out/video.ts" {
		t.Fatalf("路径里的 bidi 控制符未被剔除: %q want %q", got, "out/video.ts")
	}
	rt = &Runtime{outputFile: "a\u202Ab.mp4"}
	if got := rt.normalizeOutput(); got != "ab.mp4" {
		t.Fatalf("U+202A 应被剔除: %q want %q", got, "ab.mp4")
	}
	// 零宽连接符不是 bidi 控制符，必须保留（防"顺手改成整体删 Cf"）
	rt = &Runtime{outputFile: "a\u200Db.mp4"}
	if got := rt.normalizeOutput(); got != "a\u200Db.mp4" {
		t.Fatalf("非 bidi 的 Cf 字符不应被删: %q", got)
	}
	// 无扩展名的原行为不回归
	rt = &Runtime{outputFile: "out/video"}
	if got := rt.normalizeOutput(); got != "out/video.ts" {
		t.Fatalf("无扩展名应补 .ts: %q", got)
	}
}
