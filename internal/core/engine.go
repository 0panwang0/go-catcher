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
	mu       sync.Mutex
	port     int
	running  bool
	srv      *http.Server
	done     chan struct{} // Start 时创建，Stop 时关闭；无头模式据此退出
	initOnce sync.Once
}

func NewEngine(port int) *Engine {
	if port <= 0 {
		port = DefaultPort
	}
	return &Engine{port: port}
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
		initRuntimeConfig()
		loadState()
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindAddr, e.port))
	if err != nil {
		return fmt.Errorf("监听 %s:%d 失败（端口被占用？）: %w", bindAddr, e.port, err)
	}
	e.srv = &http.Server{Handler: newMux(e)}
	e.done = make(chan struct{})
	e.running = true
	go func() { _ = e.srv.Serve(ln) }()

	active, limit := limiter.current()
	fmt.Println("========================================")
	fmt.Println("  GoCatcher 本地下载服务")
	fmt.Printf("  监听地址: http://%s:%d\n", bindAddr, e.port)
	fmt.Printf("  最大并发下载: %d / %d 运行\n", active, limit)
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
	pauseAllTasks()
	waitTasksSettled(3 * time.Second)
	saveState() // 绕过 debounce，同步落盘一次，保证断点信息不丢
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

// Done 在服务停止后可读（无头模式 select 它决定进程退出）。未 Start 过时为 nil。
func (e *Engine) Done() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done
}

// pauseAllTasks 把所有进行中/排队中的任务转为「已暂停」。
// 与单个任务的 /pause 语义一致：intent=pause + cancel ctx，
// pipeline 的 finishInterrupt 收尾并保留 .part 断点。
func pauseAllTasks() {
	tasksMu.Lock()
	entries := make([]*taskEntry, 0, len(tasks))
	for _, te := range tasks {
		entries = append(entries, te)
	}
	tasksMu.Unlock()
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
func waitTasksSettled(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		tasksMu.Lock()
		busy := 0
		for _, te := range tasks {
			te.mu.Lock()
			if te.st.running || te.st.queued {
				busy++
			}
			te.mu.Unlock()
		}
		tasksMu.Unlock()
		if busy == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
