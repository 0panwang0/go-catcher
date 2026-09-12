// HTTP 路由装配与 /download 任务启动（监听与生命周期由 Engine 管理）。
package core

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// newMux 装配全部路由；传入 Engine 供 /svc/stop 触发停机与端点访问运行时状态。
func newMux(e *Engine) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/pickdir", e.handlePickDir)
	mux.HandleFunc("/status", e.handleStatus)
	mux.HandleFunc("/download", e.handleDownload)
	mux.HandleFunc("/probe", e.handleProbe) // 扩展预检：服务端代拉 m3u8 文本（绕浏览器 CORS 限制）
	mux.HandleFunc("/pause", e.handlePause)
	mux.HandleFunc("/resume", e.handleResume)
	mux.HandleFunc("/cancel", e.handleCancel)
	mux.HandleFunc("/remove", e.handleRemove)
	mux.HandleFunc("/openfolder", e.handleOpenFolder)
	mux.HandleFunc("/openfile", e.handleOpenFile)
	mux.HandleFunc("/config", e.handleConfig)
	mux.HandleFunc("/log", handleLog)                 // 最近日志（排障用；受令牌保护，可能含本机路径）
	mux.HandleFunc("/settings", e.handleSettingsPage) // 独立设置页（监控页 ⚙ 跳转进入）
	mux.HandleFunc("/", e.handleHomePage)             // 下载监控页
	registerSvcRoutes(mux, e)                         // /svc/stop（工具条"停止服务"按钮 / 扩展兜底）
	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok"))
}

// handleLog 返回日志文件最后若干行（纯文本）。
// GUI 是 windowsgui 子系统、没有控制台，这份日志是排障的唯一入口；
// 内容含本机路径，因此走令牌鉴权（不在 guard 的免鉴权白名单里）。
func handleLog(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	tail, err := LogTail(logTailLines)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, tail)
}

func (e *Engine) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	m3u8URL := q.Get("m3u8")
	refererParam := q.Get("referer")
	filename := strings.TrimSpace(q.Get("filename"))
	mode := strings.ToLower(q.Get("mode"))
	if filename == "" {
		filename = "video.ts"
	}
	if m3u8URL == "" {
		http.Error(w, "missing required query param: m3u8", http.StatusBadRequest)
		return
	}

	if mode != "disk" {
		// 多任务改造后仅支持 disk 落盘模式（旧 stream 模式已移除）
		http.Error(w, "only mode=disk is supported now", http.StatusBadRequest)
		return
	}

	saveDir := strings.TrimSpace(q.Get("dir"))
	if saveDir == "" {
		e.rt.saveDirMu.Lock()
		saveDir = e.rt.defaultSaveDir
		e.rt.saveDirMu.Unlock()
	}
	if saveDir == "" {
		http.Error(w, "disk mode requires dir (use /pickdir first)", http.StatusBadRequest)
		return
	}
	// dir 必须在「用户选过的目录」白名单内：否则配合攻击者可控的 m3u8，
	// 这就是一个「往任意绝对路径写文件」的原语（写进启动目录 = 持久化代码执行）。
	if !e.rt.isAllowedSaveDir(saveDir) {
		jsonError(w, http.StatusBadRequest, "dir 不在允许的下载目录内，请先通过 /pickdir 选择目录")
		return
	}
	fname := sanitizeFilename(filename)
	// 扩展名白名单（P2-10）：本服务把内容原样落盘，允许写可执行类扩展名就等于
	// 提供了一个"落盘可执行文件"的原语——与 /openfile 组合即为本机代码执行。
	// 黑名单列不完（.pif/.msc/.inf/.settingcontent-ms…），故改为白名单，
	// 只放行本服务确实会产出的媒体/字幕类型（见 allowedDownloadExts）。
	if ext := filepath.Ext(fname); ext != "" && !isAllowedDownloadExt(ext) {
		jsonError(w, http.StatusBadRequest,
			"不支持的输出扩展名（仅允许视频/音频/字幕类型）: "+ext)
		return
	}
	// 不强改扩展名：调用方给什么就用什么；完全没扩展名的（TS 原始流）补 .ts
	if filepath.Ext(fname) == "" {
		fname += ".ts"
	}

	// MAX_PATH 裁剪：目录 + 文件名（含 uniquePath 后缀与 .part/.meta）超长时
	// 先截短文件名，否则写盘会以"文件名或扩展名太长"失败
	fname = clipFilenameForDir(saveDir, fname)

	id := e.rt.newTaskID()
	// 目标路径在任务创建时就定死（.part 与正式文件都基于它），
	// 这样暂停/重启恢复后仍写回同一个文件，不会冒出 "xxx (1).mp4"。
	// uniquePath 会原子认领该路径（创建 0 字节 .part 占位），因此并发同名
	// 请求拿到的一定是不同的文件名（P2-8）。
	finalPath, err := uniquePath(filepath.Join(saveDir, fname))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 去重后可能与请求名不一致（同名已存在 -> "xxx (1).mp4"）。
	// 用真实落盘名回填 filename，让任务卡片显示与实际文件一致，多次下载也能区分。
	displayName := filepath.Base(finalPath)
	te := &taskEntry{rt: e.rt, intent: intentNone}
	te.mu.Lock()
	te.st = taskState{
		id: id, queued: true, running: false, stage: "排队中", started: time.Now(),
		m3u8URL: m3u8URL, referer: refererParam, filename: displayName, saveDir: saveDir,
		finalPath: finalPath,
	}
	te.mu.Unlock()

	e.rt.tasksMu.Lock()
	e.rt.tasks[id] = te
	e.rt.tasksMu.Unlock()
	e.rt.pruneOldTasks()

	// 启动 goroutine：先排队等并发槽，拿到后真正跑 pipeline
	go runDiskPipeline(te)

	// 统一走 writeJSON：%q 是 strconv.Quote，对控制字符会产出 \x01 这类
	// JSON 非法转义（见 handlers.go 顶部说明）。saveDir/fname 来自调用方，
	// 不保证不含控制字符。
	writeJSON(w, http.StatusAccepted, struct {
		Started  bool   `json:"started"`
		ID       string `json:"id"`
		Dir      string `json:"dir"`
		Filename string `json:"filename"`
	}{true, id, saveDir, fname})
}

// runDiskPipeline 跑单个下载任务：排队 → 解析 → 流式下载（边下边写）→ 落盘。
// 每个任务持有独立 dlJob（独立计数），可多个任务并行互不干扰；
// 任务状态写入 te.st / 回调，供 /status?id=xx 轮询与监控页展示。
//
// 写入策略：分片按序 append 到 <finalPath>.part，因此"下一个待写序号"就是断点。
// 支持三种中断：
//   - 暂停：cancel ctx → streamDownload 返回已写入序号 → 保留 .part，可从断点恢复
//   - 取消：同上，但额外删除 .part，任务终结
//   - 失败：保留 .part（下次可从断点重试），标记失败原因
