// HTTP 端点：控制(暂停/恢复/取消/移除)与查询(状态/打开文件/夹)。
package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ============================================================
// JSON 响应辅助
// ------------------------------------------------------------
// 统一走 encoding/json，不再用 fmt.Fprintf 拼 JSON 串。拼串时 %q 走的是
// strconv.Quote，对控制字符会产出 \x01 这类 JSON 非法转义（JSON 只认 \u0001）。
// 而 error / dir / path 里都可能出现远端异常串或任意本机路径，一旦命中，
// 前端 JSON.parse 会整块抛错、任务列表整个渲染失败。
// ============================================================

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Printf("[http] 响应序列化失败: %v\n", err)
	}
}

// jsonError 以 JSON 形式返回错误（前端统一读 error 字段）。
func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{msg})
}

// actionResp 是控制/打开类端点的统一响应体。未涉及的字段用 omitempty 省略，
// 与改造前各个格式串产出的 JSON 形状保持一致（前端只做真值判断）。
type actionResp struct {
	OK        bool   `json:"ok,omitempty"`
	ID        string `json:"id,omitempty"`
	Paused    bool   `json:"paused,omitempty"`
	Running   bool   `json:"running,omitempty"`
	Resumed   bool   `json:"resumed,omitempty"`
	Canceled  bool   `json:"canceled,omitempty"`
	Removed   bool   `json:"removed,omitempty"`
	Path      string `json:"path,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

func (r *Runtime) findTask(id string) *taskEntry {
	r.tasksMu.Lock()
	defer r.tasksMu.Unlock()
	return r.tasks[id]
}

// handlePause 暂停任务：cancel 掉下载 ctx，pipeline 会把断点存下来

func (e *Engine) handlePause(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	te := e.rt.findTask(id)
	if te == nil {
		jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	te.mu.Lock()
	if te.st.done {
		te.mu.Unlock()
		jsonError(w, http.StatusBadRequest, "task already finished")
		return
	}
	if te.st.paused {
		te.mu.Unlock()
		writeJSON(w, http.StatusOK, actionResp{OK: true, ID: id, Paused: true})
		return
	}
	te.intent = intentPause
	te.st.stage = "暂停中"
	cancel := te.cancel
	te.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	writeJSON(w, http.StatusOK, actionResp{OK: true, ID: id, Paused: true})
}

// handleResume 恢复任务：从上次断点继续下载（重新走一遍 pipeline，会重新排队）

func (e *Engine) handleResume(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	te := e.rt.findTask(id)
	if te == nil {
		jsonError(w, http.StatusNotFound, "task not found")
		return
	}

	te.mu.Lock()
	// done 且无错误 = 正常完成或已取消（用户放弃），不可再续；
	// 失败任务（errorMsg 非空）done 也为 true，但必须允许重试
	if te.st.done && !te.st.paused && te.st.errorMsg == "" {
		te.mu.Unlock()
		jsonError(w, http.StatusBadRequest, "task already finished")
		return
	}
	if te.st.running && !te.st.paused {
		te.mu.Unlock()
		writeJSON(w, http.StatusOK, actionResp{OK: true, ID: id, Running: true})
		return
	}
	// 清掉旧意图，重新起一个 goroutine（会重新拿并发槽并接着断点下）
	te.intent = intentNone
	te.st.paused = false
	te.st.done = false
	te.st.canceled = false
	te.st.errorMsg = ""
	te.st.stage = "排队中"
	te.st.queued = true
	te.mu.Unlock()

	go runDiskPipeline(te)
	writeJSON(w, http.StatusOK, actionResp{OK: true, ID: id, Resumed: true})
}

// handleCancel 取消任务：中断下载并删除半截 .part 文件

func (e *Engine) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	te := e.rt.findTask(id)
	if te == nil {
		jsonError(w, http.StatusNotFound, "task not found")
		return
	}

	te.mu.Lock()
	if te.st.done {
		// 已结束的任务：成功/已取消的幂等返回；
		// 失败的转为"已取消"——用户语义：取消 = 不再要这个文件，
		// 无条件删除 .part（不管有没有进度），取消按钮也随之消失。
		part := ""
		if te.st.errorMsg != "" && !te.st.canceled && te.st.filename != "" {
			part = filepath.Join(te.st.saveDir, te.st.filename+".part")
			te.st.canceled = true
			te.st.stage = "已取消"
			te.st.errorMsg = ""
			te.st.finalPath = ""
		}
		te.mu.Unlock()
		if part != "" {
			_ = os.Remove(part)
			fmt.Printf("[disk] 失败任务转取消，删除 .part: %s\n", part)
			e.rt.markDirty()
		}
		writeJSON(w, http.StatusOK, actionResp{OK: true, ID: id, Canceled: true})
		return
	}
	te.intent = intentCancel
	te.st.stage = "取消中"
	cancel := te.cancel
	te.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// 二次确认：cancel() 后既没有 worker 在跑（排队等并发槽 / 已暂停——
	// 暂停任务的 cancel 早已随上一轮 pipeline 结束而失效），也没有 goroutine
	// 会执行 finishInterrupt。请求线程直接终态化，否则任务永远停在
	// 「取消中」——取消按钮点了没反应。仍在跑的交给 pipeline 自己收尾。
	te.mu.Lock()
	idle := !te.st.running && !te.st.done
	part := ""
	if te.st.finalPath != "" {
		part = te.st.finalPath + ".part"
	}
	te.mu.Unlock()
	if idle {
		// 与 finishInterrupt 的取消路径语义一致：不可恢复 + 清理半成品
		te.mu.Lock()
		te.st.canceled = true
		te.st.done = true
		te.st.paused = false
		te.st.running = false
		te.st.queued = false
		te.st.stage = "已取消"
		te.st.errorMsg = ""
		te.st.finalPath = ""
		te.mu.Unlock()
		if part != "" {
			_ = os.Remove(part)
			_ = os.Remove(part + ".meta")
		}
		e.rt.markDirty()
	}
	writeJSON(w, http.StatusOK, actionResp{OK: true, ID: id, Canceled: true})
}

// handleRemove 把任务从列表里彻底删掉（只影响展示，不碰磁盘文件）

func (e *Engine) handleRemove(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		jsonError(w, http.StatusBadRequest, "missing id")
		return
	}
	e.rt.tasksMu.Lock()
	te := e.rt.tasks[id]
	if te == nil {
		e.rt.tasksMu.Unlock()
		jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	te.mu.Lock()
	running := te.st.running || te.st.queued
	te.mu.Unlock()
	if running {
		e.rt.tasksMu.Unlock()
		jsonError(w, http.StatusBadRequest, "task is running, cancel it first")
		return
	}
	delete(e.rt.tasks, id)
	e.rt.tasksMu.Unlock()
	e.rt.markDirty()
	writeJSON(w, http.StatusOK, actionResp{OK: true, ID: id, Removed: true})
}

// handleOpenFile 用系统关联程序直接打开文件（区别于 /openfolder 只选中）

func (e *Engine) handleOpenFile(w http.ResponseWriter, r *http.Request) {
	target, status, msg := e.taskOpenPath(r.URL.Query().Get("id"))
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	// 直接 ShellExecute（x/sys/windows），不经过 cmd/powershell/explorer 子进程。
	// 避免 detached 服务进程 fork 外部 shell 后 GUI 无法送上交互桌面的问题。
	if err := shellOpen(target); err != nil {
		jsonError(w, http.StatusInternalServerError, "open file: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, actionResp{OK: true, Path: target})
}

// taskOpenPath 取任务当前可打开的文件（成品优先，其次 .part 半成品）。
// 返回 (路径, HTTP 状态, 错误信息)；状态为 0 表示成功。
//
// /openfile 只认任务 id、不再接受任意 path：旧签名等价于给任何网页一个
// "ShellExecute 任意文件"的原语（先 /download 落盘再打开即代码执行）。
// 砍掉参数面比事后加黑名单可靠。
func (e *Engine) taskOpenPath(id string) (string, int, string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", http.StatusBadRequest, "missing query param: id"
	}
	te := e.rt.findTask(id)
	if te == nil {
		return "", http.StatusNotFound, "task not found"
	}
	if p := snapshot(te).openPath; p != "" {
		return p, 0, ""
	}
	return "", http.StatusNotFound, "任务没有可打开的文件（可能已被移动或删除）"
}

// taskOpenDir 取任务可打开的目录：优先产物所在目录（.part 也适用），
// 其次是任务记录的保存目录。
func (e *Engine) taskOpenDir(id string) (string, int, string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", http.StatusBadRequest, "missing query param: id"
	}
	te := e.rt.findTask(id)
	if te == nil {
		return "", http.StatusNotFound, "task not found"
	}
	s := snapshot(te)
	if s.openPath != "" {
		if fi, err := os.Stat(s.openPath); err == nil {
			if fi.IsDir() {
				return s.openPath, 0, ""
			}
			return filepath.Dir(s.openPath), 0, ""
		}
	}
	if s.saveDir != "" {
		if fi, err := os.Stat(s.saveDir); err == nil && fi.IsDir() {
			return s.saveDir, 0, ""
		}
	}
	return "", http.StatusNotFound, "任务没有可打开的目录"
}

// pickDirResp /pickdir 的响应：cancelled 不带 omitempty，保证「取消」与
// 「选了目录」两种情况下字段都存在，前端判断不会因字段缺失而产生歧义。
type pickDirResp struct {
	Cancelled bool   `json:"cancelled"`
	Dir       string `json:"dir,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (e *Engine) handlePickDir(w http.ResponseWriter, r *http.Request) {
	dir, err := pickFolder(0, "选择视频保存文件夹")
	if err != nil {
		// 用户取消，err 带 cancelled 标记
		writeJSON(w, http.StatusOK, pickDirResp{Cancelled: true, Error: err.Error()})
		return
	}
	if dir == "" {
		writeJSON(w, http.StatusOK, pickDirResp{Cancelled: true})
		return
	}
	e.rt.saveDirMu.Lock()
	e.rt.defaultSaveDir = dir
	e.rt.saveDirMu.Unlock()
	// 用户亲手选过的目录才进白名单——/download 只接受白名单内的 dir
	e.rt.allowSaveDir(dir)
	fmt.Printf("[pickdir] selected: %s\n", dir)
	writeJSON(w, http.StatusOK, pickDirResp{Dir: dir})
}

func (e *Engine) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id != "" {
		e.rt.tasksMu.Lock()
		te := e.rt.tasks[id]
		e.rt.tasksMu.Unlock()
		if te == nil {
			jsonError(w, http.StatusNotFound, "task not found")
			return
		}
		// 单对象：兼容旧字段（无 tasks 包裹），直接返回对象
		writeJSON(w, http.StatusOK, toTaskStateDTO(snapshot(te)))
		return
	}

	// 无 id：返回列表 + 持久化健康状态（前端据此提示"断点可能没存住"）
	all := e.rt.listTasks()
	dtos := make([]taskStateDTO, 0, len(all))
	for _, t := range all {
		dtos = append(dtos, toTaskStateDTO(t))
	}
	writeJSON(w, http.StatusOK, struct {
		Tasks   []taskStateDTO `json:"tasks"`
		Persist persistDTO     `json:"persist"`
	}{dtos, e.rt.persistDTOOf()})
}

func (e *Engine) handleHomePage(w http.ResponseWriter, r *http.Request) {
	// "/" 在 ServeMux 里是 catch-all：所有未命中更具体模式的路径都会进来，
	// 必须精确匹配 "/" 才渲染首页（方法已由路由表的 methodGuard 限定 GET）。
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// 令牌注入：页面本身免鉴权（网页读不到它的 body，见 auth.go），
	// 页面里的脚本拿着注入的令牌才能调 /status、/openfile 等。
	fmt.Fprint(w, e.rt.injectToken(homePageHTML))
}

func (e *Engine) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/settings" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, e.rt.injectToken(settingsPageHTML))
}

// handleOpenFolder 打开任务产物所在文件夹并选中该文件。只接受任务 id，
// 路径从任务记录推导（同 /openfile，砍掉"任意路径"的参数面）。
func (e *Engine) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	target, status, msg := e.taskOpenDir(r.URL.Query().Get("id"))
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	// shellReveal：文件→explorer /select 打开父目录并选中；目录→直接打开。
	if err := shellReveal(target); err != nil {
		jsonError(w, http.StatusInternalServerError, "reveal in explorer: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, actionResp{OK: true, Path: target})
}

// handleConfig GET 返回当前配置；POST 按补丁语义更新——只应用请求里出现的字段，缺省字段保持现值，
// 这样 {uiView:"compact"} 之类的局部更新不会顺带把并发/重试清零。
// 并发/重试即时生效；端口只写入配置，下次服务启动（托盘重启 / 重开程序）才生效——
// 服务正跑着时改端口，响应带 restartRequired=true 提示前端弹"需重启服务"。
// configResp /config 的响应体。
//   - systemProxy：当前检测到的 Windows 系统代理（设置页「跟随系统」模式回显）
//   - clamped：请求里有值越界、已被夹取到合法区间（前端据此提示用户）
//   - warning：配置未能落盘（R4：以前写失败被静默吞掉，用户以为改了其实没存）
type configResp struct {
	MaxConcurrent   int    `json:"maxConcurrent"`
	SegConcurrency  int    `json:"segConcurrency"`
	MaxRetries      int    `json:"maxRetries"`
	Port            int    `json:"port"`
	UIView          string `json:"uiView"`
	Proxy           string `json:"proxy"`
	SystemProxy     string `json:"systemProxy"`
	RestartRequired bool   `json:"restartRequired"`
	Clamped         bool   `json:"clamped,omitempty"`
	Warning         string `json:"warning,omitempty"`
	// SystemProxyWarning 系统代理已启用但协议不被支持（如 socks5）时会静默降级为
	// 直连——把原因回给前端，避免用户以为走了代理实际直连（有隐私风险）。
	SystemProxyWarning string `json:"systemProxyWarning,omitempty"`
}

func (e *Engine) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeCfg := func(restartRequired, clamped bool, saveErr error) {
		e.rt.cfgMu.Lock()
		cur := e.rt.cfg
		e.rt.cfgMu.Unlock()
		resp := configResp{
			MaxConcurrent:      cur.MaxConcurrent,
			SegConcurrency:     cur.SegConcurrency,
			MaxRetries:         cur.MaxRetries,
			Port:               cur.Port,
			UIView:             cur.UIView,
			Proxy:              cur.Proxy,
			SystemProxy:        e.rt.systemProxyAddr(),
			SystemProxyWarning: e.rt.systemProxyWarning(),
			RestartRequired:    restartRequired,
			Clamped:            clamped,
		}
		if saveErr != nil {
			resp.Warning = "配置未能写入磁盘（" + saveErr.Error() + "），本次修改仅在内存生效"
		}
		writeJSON(w, http.StatusOK, resp)
	}
	if r.Method == http.MethodGet {
		writeCfg(false, false, nil)
		return
	}
	// 到这里只有 POST：方法约束由路由表的 methodGuard 统一拦截（GET/POST 之外一律 405）。
	// 指针字段区分「未提供」与「显式为零」（如 maxRetries:0 是合法值）
	var in struct {
		MaxConcurrent  *int    `json:"maxConcurrent"`
		SegConcurrency *int    `json:"segConcurrency"`
		MaxRetries     *int    `json:"maxRetries"`
		Port           *int    `json:"port"`
		UIView         *string `json:"uiView"`
		Proxy          *string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad json body: "+err.Error())
		return
	}
	// 代理先于锁校验：非法值 400 拒绝，不落任何配置
	if in.Proxy != nil {
		p := strings.TrimSpace(*in.Proxy)
		switch {
		case p == "":
			p = "direct" // 空输入语义 = 直连；落盘为显式 direct，重载时不被当旧配置回退默认
		case strings.EqualFold(p, "system"):
			p = "system"
		case isDirectStr(p), validProxyAddr(p):
		default:
			jsonError(w, http.StatusBadRequest, "代理地址无效：应为 http://host:port、system 跟随系统或 direct 直连")
			return
		}
		in.Proxy = &p
	}
	e.rt.cfgMu.Lock()
	// clamped 记录"用户给的值越界、已被夹取"——把 clamp 的返回值真正用起来，
	// 而不是丢掉（此前 6 处调用全部忽略返回值，语义悬空）。
	clamped := false
	if in.MaxConcurrent != nil {
		if !clamp(in.MaxConcurrent, 1, 16) {
			clamped = true
		}
		e.rt.cfg.MaxConcurrent = *in.MaxConcurrent
	}
	if in.SegConcurrency != nil {
		if !clamp(in.SegConcurrency, 1, 32) {
			clamped = true
		}
		e.rt.cfg.SegConcurrency = *in.SegConcurrency
	}
	if in.MaxRetries != nil {
		if !clamp(in.MaxRetries, 0, 10) {
			clamped = true
		}
		e.rt.cfg.MaxRetries = *in.MaxRetries
	}
	portChanged := false
	if in.Port != nil {
		if !clamp(in.Port, 1, 65535) {
			clamped = true
		}
		portChanged = *in.Port != e.rt.cfg.Port
		e.rt.cfg.Port = *in.Port
	}
	if in.UIView != nil && (*in.UIView == "detail" || *in.UIView == "compact") {
		e.rt.cfg.UIView = *in.UIView
	}
	if in.Proxy != nil {
		e.rt.cfg.Proxy = *in.Proxy
	}
	e.rt.applyConfigLocked()
	saveErr := e.rt.saveConfigLocked()
	out := e.rt.cfg
	e.rt.cfgMu.Unlock()
	if saveErr != nil {
		fmt.Printf("[config] 落盘失败: %v\n", saveErr)
	}
	fmt.Printf("[config] 已更新: 并发任务=%d 分片并发=%d 重试=%d 端口=%d 视图=%s 代理=%s\n",
		out.MaxConcurrent, out.SegConcurrency, out.MaxRetries, out.Port, out.UIView, out.Proxy)
	writeCfg(e.Running() && portChanged, clamped, saveErr)
}
