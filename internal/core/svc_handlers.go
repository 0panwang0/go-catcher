// 服务自我控制端点 /svc/*：停止服务。
// 单进程架构里这不是进程退出——Engine.Stop 只停下载服务（任务转暂停、断点落盘、关监听），
// GUI 外壳继续活着显示降级面板；无头 --server 模式则由 Done() 通道触发进程退出。
package core

import (
	"fmt"
	"net/http"
)

func registerSvcRoutes(mux *http.ServeMux, e *Engine) {
	mux.HandleFunc("/svc/stop", func(w http.ResponseWriter, r *http.Request) {
		handleSvcStop(w, r, e)
	})
}

// 通用 CORS + JSON 头
func svcJSON(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
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
