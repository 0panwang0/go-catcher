// 成品输出：跨盘移动、文件唯一命名、文件名清洗。
// HLS 分片按序拼接本身就是可播放的原始流（TS/fMP4），直接落盘、不做任何封装转换。
package core

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// normalizeOutput 补齐输出文件扩展名（无扩展名默认 .ts）
func (r *Runtime) normalizeOutput() string {
	if filepath.Ext(r.outputFile) == "" {
		return r.outputFile + ".ts"
	}
	return r.outputFile
}

// moveFile 把 src 移到 dst：优先同卷 Rename，跨卷(不同盘)时回退为 复制+删除。
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// emitOutput 把流式下载好的 .part 落盘为最终文件（保证目录存在 + 移动 + 打日志）。
func emitOutput(partPath, finalPath string) error {
	// -o 可能带子目录（如 out/video.ts），先保证目录存在
	if dir := filepath.Dir(finalPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建输出目录失败: %w", err)
		}
	}
	if err := moveFile(partPath, finalPath); err != nil {
		return err
	}
	fmt.Printf("输出 -> %s\n", finalPath)
	return nil
}

// uniquePath 若 path 已存在（或对应的 .part 半成品已存在）则返回
// "name (1).ext"、"name (2).ext"… 直到不冲突。
// 必须同时检查 .part：下载期间磁盘上只有 <name>.ext.part（<name>.ext 尚未 rename 出来），
// 若不查 .part，两个并发同标题任务会分到同一个 finalPath，互相写同一个 .part 冲突。
func uniquePath(p string) string {
	occupied := func(cand string) bool {
		if _, err := os.Stat(cand); err == nil {
			return true // 正式文件已存在
		}
		if _, err := os.Stat(cand + ".part"); err == nil {
			return true // 半成品正在被写入
		}
		return false
	}
	if !occupied(p) {
		return p
	}
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if !occupied(cand) {
			return cand
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// /pickdir 弹 Windows 原生"选择文件夹"框，返回绝对路径并记为默认下载目录

// windowsReservedNames Windows 保留设备名：这些名字（不分大小写、带不带扩展名
// 都一样，CON.txt 同样打不开）不能作为文件名，否则写盘直接失败。
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// maxPathLen Windows 传统 MAX_PATH 上限（不含结尾 NUL）。
const maxPathLen = 260

// sanitizeFilename 把调用方给的文件名清洗成 Windows 上一定可写的名字：
//   - 非法字符 < > : " / \ | ? * 与控制字符 → 下划线（删除）
//   - 结尾的点与空格：Windows 会静默丢弃，自己先去掉，避免"以为叫 A. 实际叫 A"
//   - 保留设备名（CON/PRN/NUL/COM1…）加前缀规避
//
// 冒号额外重要：它在 NTFS 上是「备用数据流」分隔符（a.mp4:hidden 会写进 ADS）。
func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '_'
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" || name == "." || name == ".." {
		name = "video.ts"
	}
	if isWindowsReservedName(name) {
		name = "_" + name
	}
	return name
}

// isWindowsReservedName 判断名字（忽略扩展名）是否为 Windows 保留设备名。
func isWindowsReservedName(name string) bool {
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	return windowsReservedNames[strings.ToUpper(strings.TrimSpace(stem))]
}

// clipFilenameForDir 把文件名裁剪到「dir + 文件名」不超过 MAX_PATH。
// 超长路径在未开启长路径支持的系统上会以"文件名或扩展名太长"失败，
// 而视频标题动辄几十个汉字，很容易撞上。优先保留扩展名（输出格式靠它体现）。
func clipFilenameForDir(dir, name string) string {
	// 预留 uniquePath 的 " (10)" 后缀与 .part/.meta 后缀
	const reserve = 16
	budget := maxPathLen - len(dir) - 1 - reserve
	if budget < 8 {
		// 目录本身已极长：无法在 MAX_PATH 内表达，原样返回，让系统给出明确错误
		return name
	}
	if len(name) <= budget {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if len(ext) >= budget/2 {
		// 扩展名异常长（非法输入）：整体按预算截断，不强留扩展名
		ext, stem = "", name
	}
	keep := budget - len(ext)
	if keep > len(stem) {
		keep = len(stem)
	}
	// 按字节截断可能切断 UTF-8 多字节字符（中文标题常见）→ 回退到字符边界
	for keep > 0 && keep < len(stem) && !utf8.RuneStart(stem[keep]) {
		keep--
	}
	if keep == 0 {
		return name[len(name)-budget:]
	}
	return stem[:keep] + ext
}

// isExecutableExt 报告扩展名是否属于「可被系统当程序执行」的那一类。
// 用于拒绝调用方要求把远端内容落盘成可执行文件（见 handleDownload）。
func isExecutableExt(ext string) bool {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case ".exe", ".com", ".bat", ".cmd", ".ps1", ".psm1", ".vbs", ".vbe",
		".js", ".jse", ".wsf", ".wsh", ".scr", ".msi", ".msp", ".cpl",
		".lnk", ".url", ".reg", ".hta", ".jar", ".dll", ".sys":
		return true
	}
	return false
}

// ============================================================
// 下载监控页 + 打开文件夹
// 页面访问 http://127.0.0.1:7891/ 展示当前/最近一次下载的文件信息，
// 提供"打开所在文件夹"（Windows explorer 选中文件）。
// ============================================================
