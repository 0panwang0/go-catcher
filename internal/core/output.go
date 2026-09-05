// 成品输出：跨盘移动、文件唯一命名、文件名清洗。
// HLS 分片按序拼接本身就是可播放的原始流（TS/fMP4），直接落盘、不做任何封装转换。
package core

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// normalizeOutput 补齐输出文件扩展名（无扩展名默认 .ts）
func normalizeOutput() string {
	if filepath.Ext(outputFile) == "" {
		return outputFile + ".ts"
	}
	return outputFile
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

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	if name == "" {
		name = "video.ts"
	}
	return name
}

// ============================================================
// 下载监控页 + 打开文件夹
// 页面访问 http://127.0.0.1:7891/ 展示当前/最近一次下载的文件信息，
// 提供"打开所在文件夹"（Windows explorer 选中文件）。
// ============================================================
