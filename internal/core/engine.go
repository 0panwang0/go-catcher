// Engine：本地下载服务的进程内生命周期。
// 单进程架构的核心抽象——GUI 外壳、无头 --server 模式、监控页里的 /svc/stop
// 都通过同一组方法控制服务，可反复 Start/Stop（配置与历史任务只在首次 Start 装载一次）。
package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultPort 监控页 / Edge 扩展默认端口
const DefaultPort = 7891

type Engine struct {
	// rt 是本引擎的运行时状态（配置/任务表/代理客户端等，见 runtime.go）。
	// 每个 Engine 独有一份，互不共享 —— internal/core 因此可多实例化。
	rt *Runtime

	mu       sync.Mutex
	override int // --port 命令行覆盖；0 = 跟随 gocatcher_config.json
	port     int // 本次实际监听的端口（供 Port() 与日志使用）
	running  bool
	srv      *http.Server
	done     chan struct{} // Start 时创建，Stop 时关闭；无头模式据此退出
	initOnce sync.Once
}

// NewEngine 创建引擎。port 为命令行覆盖值（--port），0 表示跟随配置文件。
func NewEngine(port int) *Engine {
	return &Engine{override: port, rt: newRuntime()}
}

// Start 启动 HTTP 服务（幂等：已在运行直接返回 nil）。
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return nil
	}
	e.initOnce.Do(func() {
		// 先建限制器并装载持久化配置（并发数在恢复任务前就绪），再恢复历史任务
		e.rt.initRuntimeConfig()
		e.rt.loadState()
	})
	// 端口解析：--port 覆盖 > 配置文件；端口只在本方法和 Port() 里读，改动即时生效于下次 Start
	port := e.override
	if port <= 0 {
		port = e.rt.configuredPort()
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", e.rt.bindAddr, port))
	if err != nil {
		return fmt.Errorf("监听 %s:%d 失败（端口被占用？）: %w", e.rt.bindAddr, port, err)
	}
	e.port = port
	// guard 包在最外层：Host 校验 + 访问令牌（见 auth.go）。
	// 服务只监听 127.0.0.1，但浏览器里的任意网页都能打到 127.0.0.1，
	// 而本服务具备"写任意路径"和"执行程序"两种能力——这道门必须自己设。
	e.srv = &http.Server{Handler: e.rt.guard(newMux(e))}
	e.done = make(chan struct{})
	e.running = true
	go func() { _ = e.srv.Serve(ln) }()

	active, limit := e.rt.limiter.current()
	fmt.Println("========================================")
	fmt.Println("  GoCatcher 本地下载服务")
	fmt.Printf("  监听地址: http://%s:%d\n", e.rt.bindAddr, port)
	fmt.Printf("  最大并发下载: %d / %d 运行\n", active, limit)
	fmt.Printf("  访问令牌: %s\n", e.rt.ensureAPIToken())
	if w := e.rt.systemProxyWarning(); w != "" {
		// 静默降级成直连必须说出来：用户以为走了代理、实际暴露真实 IP，
		// 不提示的话排查时完全看不出问题在哪。
		fmt.Printf("  [!] %s\n", w)
	}
	fmt.Println("========================================")
	return nil
}

// Stop 优雅停机：进行中任务转暂停（保留断点）→ 状态落盘 → 关监听。
// 幂等；不退出进程——进程退出由调用方（GUI 退出路径 / 无头模式的 Done 通道）决定。
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	srv := e.srv
	done := e.done
	e.mu.Unlock()

	fmt.Println("[svc] 正在停止下载服务…")
	e.rt.pauseAllTasks()
	if !e.rt.waitTasksSettled(3 * time.Second) {
		// 超时：仍有 pipeline 没收尾。断点安全（persist 用的是"已落盘"计数，
		// 见 swFlushBytes），但缓冲里可能还有没写出的小尾巴，提示一下。
		fmt.Println("[svc] 警告：部分任务 3 秒内未收尾，断点按已落盘位置保存")
	}
	e.rt.saveState() // 绕过 debounce，同步落盘一次，保证断点信息不丢
	// /pickdir 的文件夹对话框可能一直挂着，Shutdown 带超时兜底
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	_ = srv.Close()
	close(done)
	fmt.Println("[svc] 下载服务已停止")
}

func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// Port 返回服务当前监听的端口；未运行时返回下次 Start 将使用的端口
// （--port 覆盖优先）。GUI 外壳据此更新 iframe 地址，端口改动重启服务后自动跟上。
func (e *Engine) Port() int {
	e.mu.Lock()
	running, port, override := e.running, e.port, e.override
	e.mu.Unlock()
	if running {
		return port
	}
	if override > 0 {
		return override
	}
	return e.rt.configuredPort()
}

// Done 在服务停止后可读（无头模式 select 它决定进程退出）。未 Start 过时为 nil。
func (e *Engine) Done() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done
}

// pauseAllTasks 把所有进行中/排队中的任务转为「已暂停」。
// 与单个任务的 /pause 语义一致：intent=pause + cancel ctx，
// pipeline 的 finishInterrupt 收尾并保留 .part 断点。
func (r *Runtime) pauseAllTasks() {
	r.tasksMu.Lock()
	entries := make([]*taskEntry, 0, len(r.tasks))
	for _, te := range r.tasks {
		entries = append(entries, te)
	}
	r.tasksMu.Unlock()
	for _, te := range entries {
		te.mu.Lock()
		if te.st.done || te.st.paused {
			te.mu.Unlock()
			continue
		}
		if te.intent == intentNone {
			te.intent = intentPause
		}
		te.st.stage = "暂停中"
		cancel := te.cancel
		te.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

// waitTasksSettled 等所有任务脱离 running/queued（最多 max）。
// 返回是否在时限内全部收尾。
func (r *Runtime) waitTasksSettled(max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		r.tasksMu.Lock()
		busy := 0
		for _, te := range r.tasks {
			te.mu.Lock()
			if te.st.running || te.st.queued {
				busy++
			}
			te.mu.Unlock()
		}
		r.tasksMu.Unlock()
		if busy == 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
