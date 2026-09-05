// 运行时可调配置：最大并发下载任务数 / 单任务分片并发数 / 分片失败重试次数。
// 提供：resizableSem（替代原先固定容量的 sem channel，支持运行中改上限）、
// config 结构体读写 gocatcher_config.json、/config 端点应用与落盘。
package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// ============================================================
// resizableSem — 可在运行时调整上限的信号量
// ------------------------------------------------------------
// 原 pipeline 用固定 channel（make(chan struct{}, maxConcurrent)）当并发槽，
// maxConcurrent 是 const，启动后无法改。换成计数器+条件变量实现，
// setLimit() 可随时改上限且对已在等待的任务即时生效。
// ============================================================

type resizableSem struct {
	mu     sync.Mutex
	cond   *sync.Cond
	limit  int
	active int
}

func newResizableSem(n int) *resizableSem {
	l := &resizableSem{limit: n}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// current 返回 (已占用, 上限)
func (l *resizableSem) current() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.limit
}

// acquire 阻塞直到拿到槽位；ctx 被取消则返回 false（未占用，调用方不得 release）。
func (l *resizableSem) acquire(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if ctx != nil && ctx.Err() != nil {
		return false
	}
	// 起一个 watcher：ctx 取消时广播，让在 cond 上等待的协程醒来重新判断
	stop := make(chan struct{})
	stopped := false
	defer func() {
		if !stopped {
			stopped = true
			close(stop)
		}
	}()
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				l.cond.Broadcast() // 唤醒等待者自查 ctx
			case <-stop:
			}
		}()
	}

	for l.active >= l.limit {
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		l.cond.Wait() // 会释放 l.mu 等待，被唤醒后重新加锁
	}
	l.active++
	if !stopped {
		stopped = true
		close(stop)
	}
	return true
}

// release 释放一个槽位。只应在 acquire 成功返回 true 后调用。
func (l *resizableSem) release() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.cond.Broadcast()
	l.mu.Unlock()
}

// setLimit 调整上限（即时生效：降低后不再放行新任务，提高后唤醒等待者）。
func (l *resizableSem) setLimit(n int) {
	l.mu.Lock()
	l.limit = n
	l.cond.Broadcast()
	l.mu.Unlock()
}

// ============================================================
// 配置值
// ============================================================

type appConfig struct {
	MaxConcurrent  int `json:"maxConcurrent"`  // 同时下载的任务数
	SegConcurrency int `json:"segConcurrency"` // 单个任务内分片并发数
	MaxRetries     int `json:"maxRetries"`     // 分片/HTTP 失败重试次数
}

func defaultConfig() appConfig {
	return appConfig{
		MaxConcurrent:  3,
		SegConcurrency: 10,
		MaxRetries:     3,
	}
}

var (
	cfgMu      sync.Mutex // 保护 cfg 与对全局变量的写入
	cfg        = defaultConfig()
	configPath string
	limiter    *resizableSem
)

// getStatePath 类似逻辑：配置随 exe 存放
func getConfigPath() string {
	if configPath != "" {
		return configPath
	}
	if exe, err := os.Executable(); err == nil {
		configPath = filepath.Join(filepath.Dir(exe), "gocatcher_config.json")
	} else {
		configPath = "gocatcher_config.json"
	}
	return configPath
}

// loadConfig 启动时读取配置并应用（校验范围，越界/损坏回退默认）。
func loadConfig() {
	c := defaultConfig()
	data, err := os.ReadFile(getConfigPath())
	if err == nil {
		var fileC appConfig
		if json.Unmarshal(data, &fileC) == nil {
			clamp(&fileC.MaxConcurrent, 1, 16)
			clamp(&fileC.SegConcurrency, 1, 32)
			clamp(&fileC.MaxRetries, 0, 10)
			c = fileC
		}
	}
	cfg = c
	applyConfigLocked() // 无论默认还是读盘，都写入限制器与全局变量
}

// clamp 把 v 约束到 [lo,hi]，越界返回 true（表示需采用默认）
func clamp(v *int, lo, hi int) bool {
	ok := true
	if *v < lo {
		*v = lo
		ok = false
	}
	if *v > hi {
		*v = hi
		ok = false
	}
	return ok
}

// applyConfigLocked 把 cfg 写入全局运行时变量（调用方须持 cfgMu）。
func applyConfigLocked() {
	limiter.setLimit(cfg.MaxConcurrent)
	concurrency = cfg.SegConcurrency
	maxRetries = cfg.MaxRetries
}

// saveConfig 把当前 cfg 原子写盘（调用方须持 cfgMu）。
func saveConfigLocked() {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	p := getConfigPath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// initRuntimeConfig 服务启动时调用：建限制器并装载持久化配置。
func initRuntimeConfig() {
	if limiter == nil {
		limiter = newResizableSem(cfg.MaxConcurrent)
	}
	loadConfig()
}
