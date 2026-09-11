// 下载目录白名单：/download 的 dir 参数只允许落在「用户通过 /pickdir 选过」的目录内。
//
// 不加约束时 dir 可以是任意绝对路径，配合攻击者可控的 m3u8/分片内容，就等于
// 「任意目录写文件」——写进启动目录即是持久化代码执行。所以这里不做「黑名单目录」，
// 而是反过来：只认白名单（用户亲手选过的目录及其子目录），别的 400。
package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// allowedDirMu / allowedDirs 均为 Runtime 字段（见 runtime.go）。

// allowSaveDir 把一个目录登记进白名单（/pickdir 选中时、或恢复历史任务时）。
func (r *Runtime) allowSaveDir(dir string) {
	key := canonicalDir(dir)
	if key == "" {
		return
	}
	r.allowedDirMu.Lock()
	r.allowedDirs[key] = true
	r.allowedDirMu.Unlock()
}

// isAllowedSaveDir 报告 dir 能否用作下载目录：必须已存在、是目录，
// 且等于某个已登记目录或其后代（子目录没有逃逸风险，父目录才有）。
func (r *Runtime) isAllowedSaveDir(dir string) bool {
	key := canonicalDir(dir)
	if key == "" {
		return false
	}
	fi, err := os.Stat(key)
	if err != nil || !fi.IsDir() {
		return false
	}
	r.allowedDirMu.RLock()
	defer r.allowedDirMu.RUnlock()
	for allowed := range r.allowedDirs {
		if key == allowed || isSubPath(allowed, key) {
			return true
		}
	}
	return false
}

// canonicalDir 归一化目录：绝对路径 + 解析符号链接 + （Windows 上）统一小写。
// 归一化是为了让「同一条路径的不同写法」能比较，例如
// C:\Users\me\Videos 与 c:/users/me/videos/../videos。
func canonicalDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	// EvalSymlinks 失败（路径不存在等）时退回 abs：登记时有 Stat 兜底
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs) // Windows 文件系统大小写不敏感
	}
	return abs
}

// isSubPath 报告 child 是否位于 parent 之下（不含 parent 自身）。
func isSubPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
