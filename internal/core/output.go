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

// uniquePath 返回一个可用的输出路径，并**原子地把它认领下来**：
// 返回时 <结果>.part 一定已经存在（0 字节占位）。
//
// 两条判据缺一不可：
//  1. 磁盘检查：正式文件已存在，或对应的 .part 半成品已存在；
//  2. 原子认领：用 O_CREATE|O_EXCL 创建 <候选>.part，失败即换下一个候选。
//
// 为什么需要第 2 条（P2-8）：只有第 1 条时是 TOCTOU —— 两个并发 /download
// 在彼此都还没创建 .part 的瞬间会算出同一个路径，然后两个任务互相写同一个
// 文件、抢同一个成品名。EXCL 创建把"选中"和"占用"合成一个原子操作，
// 而且认领随 .part 的删除（取消任务）自动释放，不需要额外的登记表。
//
// 调用方直接往返回路径 + ".part" 写即可；写入方（writeInitSegment /
// newStreamWriter）都是 O_APPEND|O_CREATE，追加到 0 字节占位上与新建等价。
func uniquePath(p string) (string, error) {
	claim := func(cand string) (bool, error) {
		if _, err := os.Stat(cand); err == nil {
			return false, nil // 正式文件已存在
		}
		f, err := os.OpenFile(cand+".part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			if os.IsExist(err) {
				return false, nil // 已被占用（并发任务或磁盘上的残留半成品）
			}
			// 目录不存在 / 无权限：换个名字也一样失败，如实上报
			return false, fmt.Errorf("创建临时文件失败 %s: %w", cand+".part", err)
		}
		return true, f.Close()
	}

	if ok, err := claim(p); err != nil {
		return "", err
	} else if ok {
		return p, nil
	}

	dir := filepath.Dir(p)
	base := filepath.Base(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i <= maxNameAttempts; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		ok, err := claim(cand)
		if err != nil {
			return "", err
		}
		if ok {
			return cand, nil
		}
	}
	return "", fmt.Errorf("同名文件过多，无法为 %s 生成可用文件名", base)
}

// maxNameAttempts 找空闲文件名时的最大尝试次数（防"目录里躺着一万个同名文件"
// 时无限循环；正常情况第一两次就命中）。
const maxNameAttempts = 9999

// reclaimPath 修正扩展名（TS 内容不能存成 .ts 以外的错名、MP4 不能存成 .ts）
// 之后重新认领目标路径：新路径照旧原子占位，旧路径上那个 0 字节占位顺手清掉。
//
// 只在"任务刚开工、还没有任何下载内容"时调用（pipeline 里从断点 0 开始的两条
// 分支），所以清旧占位是安全的；这里仍只删 0 字节文件，万一调用点被挪到
// 已有数据的路径上，也只会留下一个多余文件而不是删掉真实进度。
func reclaimPath(saveDir, newName, oldFinalPath string) (string, error) {
	np, err := uniquePath(filepath.Join(saveDir, newName))
	if err != nil {
		return "", err
	}
	if oldFinalPath == "" || strings.EqualFold(oldFinalPath, np) {
		return np, nil
	}
	if fi, serr := os.Stat(oldFinalPath + ".part"); serr == nil && fi.Size() == 0 {
		_ = os.Remove(oldFinalPath + ".part")
	}
	return np, nil
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

// allowedDownloadExts /download 允许落盘的扩展名（白名单）。
//
// 为什么用白名单而不是黑名单（P2-10）：本服务把远端内容原样落盘，只要扩展名能被
// 系统当程序执行，配合 /openfile 就是一个本机代码执行原语。而黑名单天然列不完
// —— .pif / .msc / .inf / .settingcontent-ms / .search-ms / .diagcab… 以及将来
// 新增的关联类型都在外面。反过来看，"本服务会产出哪些格式"是有限且已知的，
// 所以列白名单更可靠：漏列的后果是"某个冷门格式下不了"，而不是"能落盘可执行文件"。
//
// 视频/音频部分与 container.go 的 containerRegistry 扩展名保持一致，否则修正确
// 扩展名的正常流程会被自己拦下。
var allowedDownloadExts = map[string]bool{
	// 视频容器
	".ts": true, ".m2ts": true, ".mts": true, ".mp4": true, ".m4v": true, ".m4s": true,
	".mov": true, ".flv": true, ".mkv": true, ".webm": true, ".avi": true, ".wmv": true,
	".mpg": true, ".mpeg": true, ".3gp": true,
	// 音频
	".m4a": true, ".aac": true, ".mp3": true, ".wav": true, ".flac": true, ".ogg": true,
	".oga": true, ".opus": true,
	// 字幕（同一次下载常见的伴生文件）
	".vtt": true, ".srt": true, ".ass": true, ".ssa": true,
}

// isAllowedDownloadExt 报告扩展名是否在 /download 的落盘白名单内（大小写不敏感）。
func isAllowedDownloadExt(ext string) bool {
	return allowedDownloadExts[strings.ToLower(strings.TrimSpace(ext))]
}

// ============================================================
// 下载监控页 + 打开文件夹
// 页面访问 http://127.0.0.1:7891/ 展示当前/最近一次下载的文件信息，
// 提供"打开所在文件夹"（Windows explorer 选中文件）。
// ============================================================
