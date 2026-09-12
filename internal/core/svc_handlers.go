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

// svcInfoResp /svc/info 的响应体（字段名是扩展的契约，改动需同步 server-api.js）。
type svcInfoResp struct {
	OK    bool   `json:"ok"`
	Exe   string `json:"exe"`
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// GET /svc/info → 服务自身信息：exe 绝对路径 + 监听端口 + 访问令牌。
// 扩展用它自动记录本机 exe 路径（兜底"复制命令到终端"需要完整路径，
// 但每台机器路径不同不能写死），服务跑过一次扩展就零配置。
//
// 这里把 token 一并交出去，是刻意的：它是扩展唯一的握手入口，而网页拿不到它——
// 本响应不带 CORS 头（guard 只对令牌校验通过的响应设 CORS 头）、且被 Host 校验
// 挡住 DNS rebinding，浏览器里只能得到一个 opaque 响应（连 body 都读不出）。
// 本机进程倒是能读到，但本机进程本来就能直接读配置文件，不在本层防御范围内
// （见 auth.go 文件头）。
func handleSvcInfo(w http.ResponseWriter, r *http.Request, e *Engine) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	writeJSON(w, http.StatusOK, svcInfoResp{OK: true, Exe: exe, Port: e.Port(), Token: e.rt.apiToken()})
}

// POST /svc/stop → 优雅停止引擎（先回一句确认再停）。
func handleSvcStop(w http.ResponseWriter, r *http.Request, e *Engine) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK       bool `json:"ok"`
		Stopping bool `json:"stopping"`
	}{true, true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	fmt.Println("[svc] 收到 /svc/stop，正在停止引擎")
	go e.Stop()
}
