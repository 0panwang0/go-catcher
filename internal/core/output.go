// 成品输出：mp4/ffmpeg 重封装、跨盘移动、文件唯一命名、文件名清洗。
package core

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func wantMP4() bool {
	ext := strings.ToLower(filepath.Ext(outputFile))
	return ext == ".mp4" || ext == ""
}

// 补齐输出文件扩展名

func normalizeOutput() string {
	if filepath.Ext(outputFile) == "" {
		return outputFile + ".mp4"
	}
	return outputFile
}

// 查找可用的 ffmpeg：--ffmpeg 指定 > PATH > Windows 常见安装位置

func findFFmpeg() string {
	if ffmpegPath != "" {
		if _, err := os.Stat(ffmpegPath); err == nil {
			return ffmpegPath
		}
		fmt.Printf("  [!] 指定的 ffmpeg 不存在: %s\n", ffmpegPath)
		return ""
	}
	for _, name := range []string{"ffmpeg", "ffmpeg.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	// 常见安装位置兜底
	globs := []string{
		`%LOCALAPPDATA%\Microsoft\WinGet\Links\ffmpeg.exe`,
		`%LOCALAPPDATA%\Programs\ffmpeg\bin\ffmpeg.exe`,
		`%USERPROFILE%\scoop\apps\ffmpeg\current\bin\ffmpeg.exe`,
		`C:\ProgramData\chocolatey\bin\ffmpeg.exe`,
		`C:\ffmpeg\bin\ffmpeg.exe`,
		`D:\ffmpeg\bin\ffmpeg.exe`,
	}
	if vol := filepath.VolumeName(mustGetwd()); vol != "" {
		globs = append(globs, filepath.Join(vol, `\ffmpeg\bin\ffmpeg.exe`))
	}
	for _, g := range globs {
		matches, err := filepath.Glob(os.ExpandEnv(g))
		if err != nil {
			continue
		}
		if len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

func mustGetwd() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}

// 无损重封装：TS -> MP4，不重新编码（秒级完成）

func remuxToMP4(src, dst, ffmpeg string) error {
	cmd := exec.Command(ffmpeg,
		"-y",
		"-fflags", "+genpts", // 重新生成时间戳，避免拼接处跳秒
		"-i", src,
		"-c", "copy", // 视频音频全部直通，不重新编码
		"-movflags", "+faststart", // moov 前置，边下边播/拖拽更顺
		"-loglevel", "error",
		dst,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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

// 把流式下载好的 .part 落盘为最终文件：能 remux 就 remux，否则直接改名

func emitOutput(partPath, finalPath string) error {
	// -o 可能带子目录（如 out/video.mp4），先保证目录存在
	if dir := filepath.Dir(finalPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建输出目录失败: %w", err)
		}
	}

	if !wantMP4() || noRemux {
		// 用户明确要 .ts（或关闭了封装），直接搬过去
		if err := moveFile(partPath, finalPath); err != nil {
			return err
		}
		fmt.Printf("输出 -> %s\n", finalPath)
		return nil
	}

	ffmpeg := findFFmpeg()
	if ffmpeg == "" {
		if err := moveFile(partPath, finalPath); err != nil {
			return err
		}
		fmt.Printf("输出 -> %s\n", finalPath)
		fmt.Println("  [i] 未安装 ffmpeg，直接输出 TS 流（PotPlayer/VLC 可播）")
		return nil
	}

	fmt.Printf("  使用 ffmpeg 无损重封装: %s\n", ffmpeg)
	if err := remuxToMP4(partPath, finalPath, ffmpeg); err != nil {
		fmt.Printf("  [!] 重封装失败: %v，回退为直接输出\n", err)
		if rerr := moveFile(partPath, finalPath); rerr != nil {
			return rerr
		}
		fmt.Printf("输出 -> %s\n", finalPath)
		return nil
	}
	fmt.Printf("输出 -> %s (真 MP4 容器)\n", finalPath)
	return nil
}

// ============================================================
// main
// ============================================================

// uniquePath 若 path 已存在（或对应的 .part 半成品已存在）则返回
// "name (1).ext"、"name (2).ext"… 直到不冲突。
// 必须同时检查 .part：下载期间磁盘上只有 <name>.mp4.part（<name>.mp4 尚未 rename 出来），
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
		name = "video.mp4"
	}
	return name
}

// ============================================================
// 下载监控页 + 打开文件夹
// 页面访问 http://127.0.0.1:7891/ 展示当前/最近一次下载的文件信息，
// 提供"打开所在文件夹"（Windows explorer 选中文件）。
// ============================================================
