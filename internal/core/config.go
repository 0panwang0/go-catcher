// 运行时可调配置：最大并发下载任务数 / 单任务分片并发数 / 分片失败重试次数。
// 提供：resizableSem（替代原先固定容量的 sem channel，支持运行中改上限）、
// config 结构体读写 gocatcher_config.json、/config 端点应用与落盘。
package core

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	MaxConcurrent  int    `json:"maxConcurrent"`  // 同时下载的任务数
	SegConcurrency int    `json:"segConcurrency"` // 单个任务内分片并发数
	MaxRetries     int    `json:"maxRetries"`     // 分片/HTTP 失败重试次数
	Port           int    `json:"port"`           // 服务监听端口（改后需重启服务生效）
	UIView         string `json:"uiView"`         // 监控页任务列表详略：detail=卡片 / compact=单行（纯展示偏好，无运行时影响）
	Proxy          string `json:"proxy"`          // system=跟随系统代理（默认）；http://host:port 手动；direct/none/off = 直连
	// APIToken 本地 HTTP API 的访问令牌（首次启动生成后固定），用于挡住"任意网页
	// 驱动本机服务写文件/执行程序"。它不是调 API 的必要凭证意义上的秘密——
	// 本机进程读得到配置——但网页读不到，这正是要防的那一类。详见 auth.go。
	APIToken string `json:"apiToken,omitempty"`
}

func defaultConfig() appConfig {
	return appConfig{
		MaxConcurrent:  3,
		SegConcurrency: 10,
		MaxRetries:     3,
		Port:           DefaultPort,
		UIView:         "detail",
		Proxy:          "system", // 跟随 Windows 系统代理（Clash 等开箱即用），与包级默认一致
	}
}

// cfg / limiter / configPath / configInitOnce 均为 Runtime 字段（见 runtime.go）。

// ============================================================
// 运行时下载调参（/config 可热改）
// ------------------------------------------------------------
// 原先 segConcurrency / maxRetries 是裸 int 包级变量：applyConfigLocked 在
// cfgMu 下写，而下载热路径（分片并发、重试轮次）到处无锁读。-race 之所以全绿，
// 只是因为现有测试里没有"一边下载一边改配置"这条路径，不代表安全。
// 改用原子变量：读点无锁、写点原子，语义与取值范围完全不变。
// 现为 Runtime 字段（G1），读取入口是方法。
// ============================================================

// concurrencyNow / maxRetriesNow 是下载热路径的读取入口（无锁）。
func (r *Runtime) concurrencyNow() int { return int(r.segConcurrency.Load()) }
func (r *Runtime) maxRetriesNow() int  { return int(r.retryLimit.Load()) }

// setDownloadTuning 写入运行时下载调参。CLI 初始化与 /config 热更新都走这里。
// conc<=0 表示不改分片并发；retries<0 表示不改重试次数（CLI 未暴露该参数）。
func (r *Runtime) setDownloadTuning(conc, retries int) {
	if conc > 0 {
		r.segConcurrency.Store(int64(conc))
	}
	if retries >= 0 {
		r.retryLimit.Store(int64(retries))
	}
}

// getConfigPath 配置随 exe 存放（测试可通过直接赋值 r.configPath 覆盖）。
func (r *Runtime) getConfigPath() string {
	if r.configPath != "" {
		return r.configPath
	}
	if exe, err := os.Executable(); err == nil {
		r.configPath = filepath.Join(filepath.Dir(exe), "gocatcher_config.json")
	} else {
		r.configPath = "gocatcher_config.json"
	}
	return r.configPath
}

// loadConfig 启动时读取配置并应用（校验范围，越界/损坏回退默认）。
func (r *Runtime) loadConfig() {
	c := defaultConfig()
	data, err := os.ReadFile(r.getConfigPath())
	if err == nil {
		var fileC appConfig
		if json.Unmarshal(data, &fileC) == nil {
			// 返回值表示"发生过夹取"。这里按设计忽略：读盘路径对越界值做夹取
			// （而非回退默认），与 /config 端点用 clamped 提示用户的处理不同。
			_ = clamp(&fileC.MaxConcurrent, 1, 16)
			_ = clamp(&fileC.SegConcurrency, 1, 32)
			_ = clamp(&fileC.MaxRetries, 0, 10)
			// 旧配置文件没有 port 字段（反序列化为 0）：回退默认而不是夹取到 1
			if fileC.Port <= 0 || fileC.Port > 65535 {
				fileC.Port = c.Port
			}
			// uiView 同理：空串/非法值回退默认，只认 detail / compact
			if fileC.UIView != "compact" {
				fileC.UIView = c.UIView
			}
			// proxy：空串 = 旧配置文件没有该字段，回退默认（与端口同理）；
			// system / direct 关键字或 http://host:port 之外的非法值也回退默认
			fileC.Proxy = strings.TrimSpace(fileC.Proxy)
			if strings.EqualFold(fileC.Proxy, "system") {
				fileC.Proxy = "system"
			} else if fileC.Proxy == "" || (!isDirectStr(fileC.Proxy) && !validProxyAddr(fileC.Proxy)) {
				fileC.Proxy = c.Proxy
			}
			c = fileC
		}
	}
	r.cfg = c
	r.applyConfigLocked() // 无论默认还是读盘，都写入限制器与运行时变量
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

// validProxyAddr 校验代理地址：仅支持 http://host[:port]（dialTLSContext 走
// HTTP CONNECT 隧道，socks5/https 代理无法工作）。
func validProxyAddr(p string) bool {
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "http" && u.Host != ""
}

// applyConfigLocked 把 cfg 写入运行时变量（调用方须持 cfgMu）。
func (r *Runtime) applyConfigLocked() {
	r.limiter.setLimit(r.cfg.MaxConcurrent)
	r.setDownloadTuning(r.cfg.SegConcurrency, r.cfg.MaxRetries)
	r.setProxyAddr(r.cfg.Proxy)
}

// saveConfigLocked 把当前 cfg 原子写盘（调用方须持 cfgMu），返回写盘错误。
//
// 以前失败直接 return：GUI 是 windowsgui 子系统、没有控制台，写盘失败用户完全
// 看不到，以为设置改了、重启却发现回到旧值。/config 端点现在把错误回给前端。
func (r *Runtime) saveConfigLocked() error {
	data, err := json.MarshalIndent(r.cfg, "", "  ")
	if err != nil {
		return err
	}
	p := r.getConfigPath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	return nil
}

// initRuntimeConfig 服务启动时调用：建限制器并装载持久化配置。
// once 保护：GUI 外壳可能在 Engine.Start 之前就问端口，
// 这里保证配置只从磁盘装载一次，之后仅经 /config 端点修改。
func (r *Runtime) initRuntimeConfig() {
	r.configInitOnce.Do(func() {
		r.limiter = newResizableSem(r.cfg.MaxConcurrent)
		r.loadConfig()
		// 访问令牌在配置装载后立刻就绪（guard 每次都读它，不能等到第一次请求才生成）
		r.ensureAPIToken()
	})
}

// configuredPort 返回配置文件里的服务端口（引擎启动前用它拼监控页地址）。
func (r *Runtime) configuredPort() int {
	r.initRuntimeConfig()
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	return r.cfg.Port
}
