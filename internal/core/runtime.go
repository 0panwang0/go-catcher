// Runtime：引擎全部可变运行状态的唯一载体（G1 去全局化）。
//
// 历史背景：internal/core 曾经有十几个包级可变变量（cfg、limiter、tasks、
// sharedClient、defaultSaveDir、persistHealth、CLI 参数……）散布在各个文件里。
// 包级全局意味着：同进程只能有一个引擎实例、internal/core 无法被当库引用、
// 任何新代码都能绕过锁直接摸状态。现在它们全部收进 Runtime，由持有者决定生命周期：
//
//   - Engine（GUI/无头服务）在 NewEngine 时创建自己的 Runtime；
//   - CLI 直下模式在 RunCLI 里创建一次性 Runtime；
//   - 两者互不干扰，理论上同进程可以并存多个 Engine。
//
// 仍保留在包级的只有**只读注册表**（containerRegistry / keyMethodRegistry /
// 编译期正则）与真正的常量——它们不可变，无并发问题。
//
// 载体链：Engine.rt / taskEntry.rt / dlJob.rt。深层代码（下载分片、持久化）
// 一律通过最近持有者的 rt 字段访问状态，不允许新增包级可变变量。
package core

import (
	"crypto/rand"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0panwang0/go-catcher/internal/platform"
)

// defaultUserAgent 请求伪造的浏览器 UA（CLI 与 GUI 共用）。
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0"

// Runtime 聚合引擎全部可变状态。字段按职责分区；并发保护策略见各组注释。
type Runtime struct {
	// ---- 访问控制 ----
	// embedKey 内嵌豁免键：构造时生成一次、此后只读，故不加锁（见 auth.go）。
	embedKey string

	// entropyErr 熵源（crypto/rand）不可用的原因，受 cfgMu 保护。
	// 非 nil 时令牌与内嵌键都拿不到，引擎必须拒绝启动（见 Engine.Start）：
	// 退化出来的"随机数"是可预测的，等于没有访问控制。
	entropyErr error

	// ---- 配置（原 config.go 包级变量）----
	// cfg 由 cfgMu 保护；segConcurrency/retryLimit 是热路径原子读。
	cfgMu          sync.Mutex
	cfg            appConfig
	configPath     string
	configInitOnce sync.Once
	limiter        *resizableSem
	segConcurrency atomic.Int64 // 单任务内分片并发数
	retryLimit     atomic.Int64 // 分片/HTTP 失败重试次数

	// ---- 任务表（原 task.go 包级变量）----
	tasksMu        sync.Mutex
	tasks          map[string]*taskEntry
	seqID          int64
	saveDirMu      sync.Mutex
	defaultSaveDir string // /pickdir 选中的目录，供后续 /download?mode=disk 使用

	// ---- 下载目录白名单（原 savedir.go 包级变量）----
	allowedDirMu sync.RWMutex
	allowedDirs  map[string]bool // 归一化后的绝对路径

	// ---- 状态持久化（原 persist.go 包级变量）----
	stateMu        sync.Mutex
	stateDirty     bool
	stateSaving    bool
	statePathOnce  sync.Once
	statePath      string
	stateSaveDelay time.Duration
	persistHealth  struct {
		mu      sync.Mutex
		lastErr string
		lastOK  time.Time
	}

	// ---- 网络（原 net.go 包级变量）----
	// 代理运行时可经 /config 修改；修改时整体换新 client（见 setProxyAddr）。
	netMu        sync.Mutex
	proxyAddr    string
	sharedClient *http.Client

	// ---- 系统代理探测钩子（原 systemproxy_windows.go 的函数变量）----
	// 默认读 WinINET 注册表；测试注入桩函数。
	systemProxyAddrFn func() string

	// ---- /probe SSRF 防护的测试开关（原 probe.go 包级变量）----
	// 单测的上游是 httptest 起的 127.0.0.1 服务，需放行环回地址；生产恒为 false。
	// 只放行环回，不放行私网/链路本地/元数据地址（见 probe.go 的说明）。
	probeAllowLocal bool

	// ---- CLI / 引擎运行参数（原 cli.go 包级变量）----
	userAgent  string
	outputFile string
	referer    string
	limit      int
	bindAddr   string

	// ---- 直播跟随轮询参数（原 download.go 包级变量，测试可调短）----
	livePollInterval  time.Duration
	liveMaxEmptyPolls int

	// ---- 直链分片参数 ----
	// chunkSizeBytes 目标片长；0 = 用 chunkSizeFixed（生产值）。
	// 仅供测试调小以覆盖多片/续传路径，未暴露到 /config。
	chunkSizeBytes int64

	// ---- 媒体分片读体参数 ----
	// segBodyBytes 单个分片读体上限；0 = 用 maxSegmentBytes（生产值，256 MiB）。
	// 仅供测试调小以覆盖超限路径 —— 生产值下要造 256 MiB 响应才能触发，
	// 那条路没法在单测里真跑。未暴露到 /config。
	segBodyBytes int64
}

// newRuntime 创建一份带默认值的运行时。Engine 与 CLI 各持一份，互不共享。
func newRuntime() *Runtime { return newRuntimeWithEntropy(rand.Reader) }

// newRuntimeWithEntropy 用指定熵源创建运行时。
// 生产恒走 crypto/rand（newRuntime）；注入失败 reader 的入口只给测试用——
// 覆盖"熵源不可用必须拒绝启动"这条路径。
func newRuntimeWithEntropy(rnd io.Reader) *Runtime {
	r := &Runtime{
		cfg:               defaultConfig(),
		tasks:             map[string]*taskEntry{},
		allowedDirs:       map[string]bool{},
		stateSaveDelay:    1200 * time.Millisecond, // 进度去抖：合并高频更新避免疯狂写盘
		proxyAddr:         "system",                // 默认跟随 Windows 系统代理
		userAgent:         defaultUserAgent,
		outputFile:        "output.ts",
		bindAddr:          "127.0.0.1",
		livePollInterval:  3 * time.Second,
		liveMaxEmptyPolls: 25,
		chunkSizeBytes:    chunkSizeFixed,
		segBodyBytes:      maxSegmentBytes,
	}
	// 内嵌豁免键：每次运行新键，GUI 外壳的 iframe 豁免凭据。熵源故障时留空
	// 并记下原因——空键下 embedAccepted 一律拒绝（fail-closed），不静默放行。
	if key, err := newEmbedKey(rnd); err != nil {
		r.entropyErr = err
	} else {
		r.embedKey = key
	}
	r.systemProxyAddrFn = platform.RealSystemProxyAddr
	// 原包级 init()：热路径原子变量的默认值
	r.segConcurrency.Store(10)
	r.retryLimit.Store(3)
	return r
}
