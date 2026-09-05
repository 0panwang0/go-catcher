// 服务自我控制端点 /svc/*：停止服务。
// 单进程架构里这不是进程退出——Engine.Stop 只停下载服务（任务转暂停、断点落盘、关监听），
// GUI 外壳继续活着显示降级面板；无头 --server 模式则由 Done() 通道触发进程退出。
package core

import (
	"fmt"
	"net/http"
	"os"
)

func registerSvcRoutes(mux *http.ServeMux, e *Engine) {
	mux.HandleFunc("/svc/stop", func(w http.ResponseWriter, r *http.Request) {
		handleSvcStop(w, r, e)
	})
	mux.HandleFunc("/svc/info", func(w http.ResponseWriter, r *http.Request) {
		handleSvcInfo(w, r, e)
	})
}

// 通用 CORS + JSON 头
func svcJSON(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
}

// GET /svc/info → 服务自身信息：exe 绝对路径 + 监听端口。
// 扩展用它自动记录本机 exe 路径（兜底"复制命令到终端"需要完整路径，
// 但每台机器路径不同不能写死），服务跑过一次扩展就零配置。
func handleSvcInfo(w http.ResponseWriter, r *http.Request, e *Engine) {
	svcJSON(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	fmt.Fprintf(w, `{"ok":true,"exe":%q,"port":%d}`, exe, e.Port())
}

// POST /svc/stop → 优雅停止引擎（先回一句确认再停）。
func handleSvcStop(w http.ResponseWriter, r *http.Request, e *Engine) {
	svcJSON(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fmt.Fprintf(w, `{"ok":true,"stopping":true}`)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	fmt.Println("[svc] 收到 /svc/stop，正在停止引擎")
	go e.Stop()
}
