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

func findTask(id string) *taskEntry {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	return tasks[id]
}

// handlePause 暂停任务：cancel 掉下载 ctx，pipeline 会把断点存下来

func handlePause(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	te := findTask(id)
	if te == nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}
	te.mu.Lock()
	if te.st.done {
		te.mu.Unlock()
		http.Error(w, `{"error":"task already finished"}`, http.StatusBadRequest)
		return
	}
	if te.st.paused {
		te.mu.Unlock()
		fmt.Fprintf(w, `{"ok":true,"id":%q,"paused":true}`, id)
		return
	}
	te.intent = intentPause
	te.st.stage = "暂停中"
	cancel := te.cancel
	te.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	fmt.Fprintf(w, `{"ok":true,"id":%q,"paused":true}`, id)
}

// handleResume 恢复任务：从上次断点继续下载（重新走一遍 pipeline，会重新排队）

func handleResume(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	te := findTask(id)
	if te == nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	te.mu.Lock()
	if te.st.done && !te.st.paused {
		te.mu.Unlock()
		http.Error(w, `{"error":"task already finished"}`, http.StatusBadRequest)
		return
	}
	if te.st.running && !te.st.paused {
		te.mu.Unlock()
		fmt.Fprintf(w, `{"ok":true,"id":%q,"running":true}`, id)
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
	fmt.Fprintf(w, `{"ok":true,"id":%q,"resumed":true}`, id)
}

// handleCancel 取消任务：中断下载并删除半截 .part 文件

func handleCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	te := findTask(id)
	if te == nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	te.mu.Lock()
	if te.st.done {
		// 已结束的任务（成功/失败/已取消）无需再取消
		te.mu.Unlock()
		fmt.Fprintf(w, `{"ok":true,"id":%q,"canceled":true}`, id)
		return
	}
	te.intent = intentCancel
	te.st.stage = "取消中"
	cancel := te.cancel
	queued := te.st.queued && !te.st.running
	part := te.st.finalPath + ".part"
	te.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// 排队中的任务没有 goroutine 在跑（或正卡在等并发槽），
	// cancel 只能打断 sem 等待、不会走 finishInterrupt，这里兜底清理
	if queued {
		te.mu.Lock()
		te.st.canceled = true
		te.st.done = true
		te.st.paused = false
		te.st.running = false
		te.st.queued = false
		te.st.stage = "已取消"
		te.mu.Unlock()
		if part != "" {
			_ = os.Remove(part)
		}
		markDirty()
	}
	fmt.Fprintf(w, `{"ok":true,"id":%q,"canceled":true}`, id)
}

// handleRemove 把任务从列表里彻底删掉（只影响展示，不碰磁盘文件）

func handleRemove(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
		return
	}
	tasksMu.Lock()
	te := tasks[id]
	if te == nil {
		tasksMu.Unlock()
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}
	if te != nil {
		te.mu.Lock()
		running := te.st.running || te.st.queued
		te.mu.Unlock()
		if running {
			tasksMu.Unlock()
			http.Error(w, `{"error":"task is running, cancel it first"}`, http.StatusBadRequest)
			return
		}
	}
	delete(tasks, id)
	tasksMu.Unlock()
	markDirty()
	fmt.Fprintf(w, `{"ok":true,"id":%q,"removed":true}`, id)
}

// handleOpenFile 用系统关联程序直接打开文件（区别于 /openfolder 只选中）

func handleOpenFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("path"))
	if target == "" {
		http.Error(w, "missing query param: path", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(target) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	fi, err := os.Stat(target)
	if err != nil {
		http.Error(w, "path does not exist: "+err.Error(), http.StatusNotFound)
		return
	}
	if fi.IsDir() {
		http.Error(w, "path is a directory, use /openfolder", http.StatusBadRequest)
		return
	}
	// 直接 ShellExecute（x/sys/windows），不经过 cmd/powershell/explorer 子进程。
	// 避免 detached 服务进程 fork 外部 shell 后 GUI 无法送上交互桌面的问题。
	if err := shellOpen(target); err != nil {
		http.Error(w, "open file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, `{"ok":true,"path":%q}`, target)
}

func handlePickDir(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	dir, err := pickFolder(0, "选择视频保存文件夹")
	if err != nil {
		// 用户取消，err 带 cancelled 标记
		fmt.Fprintf(w, `{"cancelled":true,"error":%q}`, err.Error())
		return
	}
	if dir == "" {
		fmt.Fprintf(w, `{"cancelled":true}`)
		return
	}
	saveDirMu.Lock()
	defaultSaveDir = dir
	saveDirMu.Unlock()
	fmt.Printf("[pickdir] selected: %s\n", dir)
	fmt.Fprintf(w, `{"cancelled":false,"dir":%q}`, dir)
}

// taskStateJSON 把任务状态序列化为 JSON 对象字符串片段

func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	id := r.URL.Query().Get("id")
	if id != "" {
		tasksMu.Lock()
		te := tasks[id]
		tasksMu.Unlock()
		if te == nil {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		t := snapshot(te)
		// 单对象：兼容旧字段（无 tasks 包裹），直接返回对象
		fmt.Fprintf(w, `%s`, taskStateJSON(t))
		return
	}

	// 无 id：返回列表
	all := listTasks()
	fmt.Fprintf(w, `{"tasks":[`)
	for i, t := range all {
		if i > 0 {
			fmt.Fprintf(w, ",")
		}
		fmt.Fprintf(w, `%s`, taskStateJSON(t))
	}
	fmt.Fprintf(w, `]}`)
}

// 清洗文件名：去掉路径分隔符和引号，避免破坏 Content-Disposition 解析

func handleHomePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, homePageHTML)
}

// /openfolder?path=<文件绝对路径> 打开所在文件夹并选中该文件。
// 校验 path 为绝对路径且指向已存在文件/目录，避免被当任意命令执行。

func handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("path"))
	if target == "" {
		http.Error(w, "missing query param: path", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(target) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(target); err != nil {
		http.Error(w, "path does not exist: "+err.Error(), http.StatusNotFound)
		return
	}
	// shellReveal：文件→explorer /select 打开父目录并选中；目录→直接打开。
	if err := shellReveal(target); err != nil {
		http.Error(w, "reveal in explorer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, `{"ok":true,"path":%q}`, target)
}

// handleConfig GET 返回当前配置，POST 更新配置（越界自动夹取后应用并落盘）。
// 并发/重试即时生效；端口只写入配置，下次服务启动（托盘重启 / 重开程序）才生效——
// 服务正跑着时改端口，响应带 restartRequired=true 提示前端弹"需重启服务"。
func handleConfig(w http.ResponseWriter, r *http.Request, e *Engine) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeCfg := func(restartRequired bool) {
		cfgMu.Lock()
		cur := cfg
		cfgMu.Unlock()
		fmt.Fprintf(w, `{"maxConcurrent":%d,"segConcurrency":%d,"maxRetries":%d,"port":%d,"restartRequired":%t}`,
			cur.MaxConcurrent, cur.SegConcurrency, cur.MaxRetries, cur.Port, restartRequired)
	}
	if r.Method == http.MethodGet {
		writeCfg(false)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in appConfig
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// 校验范围：越界夹取到合法区间
	clamp(&in.MaxConcurrent, 1, 16)
	clamp(&in.SegConcurrency, 1, 32)
	clamp(&in.MaxRetries, 0, 10)

	cfgMu.Lock()
	// 端口：0/缺失 = 保持现值（兼容只改并发的旧调用方），越界夹取到 1-65535
	if in.Port == 0 {
		in.Port = cfg.Port
	} else {
		clamp(&in.Port, 1, 65535)
	}
	portChanged := in.Port != cfg.Port
	cfg = in
	applyConfigLocked()
	saveConfigLocked()
	cfgMu.Unlock()
	fmt.Printf("[config] 已更新: 并发任务=%d 分片并发=%d 重试=%d 端口=%d\n",
		in.MaxConcurrent, in.SegConcurrency, in.MaxRetries, in.Port)
	writeCfg(e.Running() && portChanged)
}
