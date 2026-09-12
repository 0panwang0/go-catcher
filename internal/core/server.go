// HTTP 路由装配与 /download 任务启动（监听与生命周期由 Engine 管理）。
package core

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// routeDef 一条路由表项：路径、允许的方法、是否要求令牌、handler 工厂。
// 端点的一切约束都收敛在这里——新增端点 = 加一行表项，不再需要同时改
// mux 装配、handler 内的方法检查、auth 的免令牌白名单三处（评审 P2-3）。
// needToken 默认语义为要求令牌；false 仅限响应体不含秘密的端点。
type routeDef struct {
	path      string
	methods   []string
	needToken bool
	new       func(*Engine) http.HandlerFunc
}

// routeDefs 全部端点一览表。免令牌端点只有 4 个：探活 / 握手 / 两个页面。
// 新增免令牌端点前必须想清楚：它的响应体是否含秘密（令牌）、是否会被 CORS
// 读到（豁免成立的前提见 auth.go 的说明）。
var routeDefs = []routeDef{
	{"/health", []string{http.MethodGet}, false, func(*Engine) http.HandlerFunc { return handleHealth }},
	{"/svc/info", []string{http.MethodGet}, false, func(e *Engine) http.HandlerFunc { return e.handleSvcInfo }},
	{"/", []string{http.MethodGet}, false, func(e *Engine) http.HandlerFunc { return e.handleHomePage }},
	{"/settings", []string{http.MethodGet}, false, func(e *Engine) http.HandlerFunc { return e.handleSettingsPage }},

	{"/pickdir", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handlePickDir }},
	{"/status", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handleStatus }},
	{"/download", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handleDownload }},
	{"/probe", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handleProbe }},
	{"/pause", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handlePause }},
	{"/resume", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handleResume }},
	{"/cancel", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handleCancel }},
	{"/remove", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handleRemove }},
	{"/openfolder", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handleOpenFolder }},
	{"/openfile", []string{http.MethodGet}, true, func(e *Engine) http.HandlerFunc { return e.handleOpenFile }},
	{"/config", []string{http.MethodGet, http.MethodPost}, true, func(e *Engine) http.HandlerFunc { return e.handleConfig }},
	{"/log", []string{http.MethodGet}, true, func(*Engine) http.HandlerFunc { return handleLog }},
	{"/svc/stop", []string{http.MethodPost}, true, func(e *Engine) http.HandlerFunc { return e.handleSvcStop }},
}

// newMux 装配全部路由。鉴权（Host / 令牌 / CORS）由 engine.go 在 mux 外层
// 的 guard 统一完成；方法校验在这里按路由表包一层，handler 内不再各自写。
func newMux(e *Engine) *http.ServeMux {
	mux := http.NewServeMux()
	for _, rd := range routeDefs {
		mux.HandleFunc(rd.path, methodGuard(rd, rd.new(e)))
	}
	return mux
}

// methodGuard 校验请求方法：不在表内的方法回 405 并带 Allow 头。
// 方法校验本身不构成防线（真正的防线是令牌），但它能挡掉 <img src>、<form>
// 这类不需要 CORS 就发出的噪声请求，也让端点的契约明确。
func methodGuard(rd routeDef, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, m := range rd.methods {
			if r.Method == m {
				h(w, r)
				return
			}
		}
		w.Header().Set("Allow", strings.Join(rd.methods, ", "))
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok"))
}

// handleLog 返回日志文件最后若干行（纯文本）。
// GUI 是 windowsgui 子系统、没有控制台，这份日志是排障的唯一入口；
// 内容含本机路径，因此走令牌鉴权（不在 guard 的免鉴权白名单里）。
func handleLog(w http.ResponseWriter, r *http.Request) {
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
