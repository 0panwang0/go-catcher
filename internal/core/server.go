// HTTP 路由装配与 /download 任务启动（监听与生命周期由 Engine 管理）。
package core

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// newMux 装配全部路由；传入 Engine 供 /svc/stop 触发停机。
func newMux(e *Engine) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/pickdir", handlePickDir)
	mux.HandleFunc("/status", handleStatus)
	mux.HandleFunc("/download", handleDownload)
	mux.HandleFunc("/pause", handlePause)
	mux.HandleFunc("/resume", handleResume)
	mux.HandleFunc("/cancel", handleCancel)
	mux.HandleFunc("/remove", handleRemove)
	mux.HandleFunc("/openfolder", handleOpenFolder)
	mux.HandleFunc("/openfile", handleOpenFile)
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) { handleConfig(w, r, e) })
	mux.HandleFunc("/settings", handleSettingsPage) // 独立设置页（监控页 ⚙ 跳转进入）
	mux.HandleFunc("/", handleHomePage) // 下载监控页
	registerSvcRoutes(mux, e)           // /svc/stop（工具条"停止服务"按钮 / 扩展兜底）
	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte("ok"))
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		saveDirMu.Lock()
		saveDir = defaultSaveDir
		saveDirMu.Unlock()
	}
	if saveDir == "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Error(w, "disk mode requires dir (use /pickdir first)", http.StatusBadRequest)
		return
	}
	fname := sanitizeFilename(filename)
	// 不强改扩展名：调用方给什么就用什么；完全没扩展名的（TS 原始流）补 .ts
	if filepath.Ext(fname) == "" {
		fname += ".ts"
	}

	id := newTaskID()
	// 目标路径在任务创建时就定死（.part 与正式文件都基于它），
	// 这样暂停/重启恢复后仍写回同一个文件，不会冒出 "xxx (1).mp4"
	finalPath := uniquePath(filepath.Join(saveDir, fname))
	// 去重后可能与请求名不一致（同名已存在 -> "xxx (1).mp4"）。
	// 用真实落盘名回填 filename，让任务卡片显示与实际文件一致，多次下载也能区分。
	displayName := filepath.Base(finalPath)
	te := &taskEntry{intent: intentNone}
	te.mu.Lock()
	te.st = taskState{
		id: id, queued: true, running: false, stage: "排队中", started: time.Now(),
		m3u8URL: m3u8URL, referer: refererParam, filename: displayName, saveDir: saveDir,
		finalPath: finalPath,
	}
	te.mu.Unlock()

	tasksMu.Lock()
	tasks[id] = te
	tasksMu.Unlock()
	pruneOldTasks()

	// 启动 goroutine：先排队等并发槽，拿到后真正跑 pipeline
	go runDiskPipeline(te)

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"started":true,"id":%q,"dir":%q,"filename":%q}`, id, saveDir, fname)
}

// runDiskPipeline 跑单个下载任务：先排队等并发槽，拿到后执行完整 pipeline 并把合并文件落到 saveDir。
// 每个任务持有独立 dlJob（独立临时目录/独立计数），可多个任务并行互不干扰。
// 任务状态写入 te.st / 回调，供 /status?id=xx 轮询与监控页展示。
// runDiskPipeline 跑单个下载任务：排队 → 解析 → 流式下载（边下边写）→ 落盘。
//
// 写入策略：分片按序 append 到 <finalPath>.part，因此"下一个待写序号"就是断点。
// 支持三种中断：
//   - 暂停：cancel ctx → streamDownload 返回已写入序号 → 保留 .part，可从断点恢复
//   - 取消：同上，但额外删除 .part，任务终结
//   - 失败：保留 .part（下次可从断点重试），标记失败原因
