// 运行时配置与并发信号量测试：resizableSem 并发正确性、配置装载/回退/落盘。
package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestResizableSemBasic 基础 acquire/release 与计数。
func TestResizableSemBasic(t *testing.T) {
	l := newResizableSem(2)
	ctx := context.Background()
	if a, lim := l.current(); a != 0 || lim != 2 {
		t.Fatalf("current=%d,%d want 0,2", a, lim)
	}
	for i := 0; i < 2; i++ {
		if !l.acquire(ctx) {
			t.Fatalf("acquire %d 失败", i)
		}
	}
	if a, _ := l.current(); a != 2 {
		t.Fatalf("active=%d want 2", a)
	}
	l.release()
	if a, _ := l.current(); a != 1 {
		t.Fatalf("active=%d want 1", a)
	}
}

// TestResizableSemConcurrent 20 个协程抢 3 个槽位，峰值占用不得超过上限。
func TestResizableSemConcurrent(t *testing.T) {
	l := newResizableSem(3)
	ctx := context.Background()
	var wg sync.WaitGroup
	var active, maxActive int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !l.acquire(ctx) {
				t.Error("acquire 失败")
				return
			}
			a := atomic.AddInt64(&active, 1)
			for {
				cur := atomic.LoadInt64(&maxActive)
				if a <= cur || atomic.CompareAndSwapInt64(&maxActive, cur, a) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&active, -1)
			l.release()
		}()
	}
	wg.Wait()
	if m := atomic.LoadInt64(&maxActive); m > 3 {
		t.Fatalf("峰值占用 %d 超过上限 3", m)
	}
	if a, _ := l.current(); a != 0 {
		t.Fatalf("结束后 active=%d 应归零", a)
	}
}

// TestResizableSemSetLimitRaise 提高上限应唤醒在 cond 上等待的协程。
func TestResizableSemSetLimitRaise(t *testing.T) {
	l := newResizableSem(1)
	ctx := context.Background()
	if !l.acquire(ctx) {
		t.Fatal("首槽失败")
	}
	got := make(chan bool, 1)
	go func() { got <- l.acquire(ctx) }()
	time.Sleep(20 * time.Millisecond) // 让等待者进入 cond.Wait
	l.setLimit(2)
	select {
	case v := <-got:
		if !v {
			t.Fatal("提高上限后 acquire 返回 false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("提高上限未唤醒等待者")
	}
	l.release()
	l.release()
}

// TestResizableSemSetLimitLower 降低上限后不再放行新任务，释放到新上限内再放行。
func TestResizableSemSetLimitLower(t *testing.T) {
	l := newResizableSem(3)
	ctx := context.Background()
	if !l.acquire(ctx) || !l.acquire(ctx) {
		t.Fatal("占槽失败")
	}
	l.setLimit(1) // 已占用 2 > 新上限 1
	if _, lim := l.current(); lim != 1 {
		t.Fatalf("limit=%d want 1", lim)
	}
	got := make(chan bool, 1)
	go func() { got <- l.acquire(ctx) }()
	select {
	case v := <-got:
		t.Fatalf("超限时 acquire 不应放行，got %v", v)
	case <-time.After(100 * time.Millisecond):
		// 正确：仍阻塞
	}
	l.release() // active 2→1，仍等于新上限 1，继续阻塞
	select {
	case v := <-got:
		t.Fatalf("active==limit 时 acquire 不应放行，got %v", v)
	case <-time.After(100 * time.Millisecond):
		// 正确：仍阻塞
	}
	l.release() // active 1→0 < limit，放行一个
	select {
	case v := <-got:
		if !v {
			t.Fatal("释放到上限内后 acquire 返回 false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("释放后未唤醒等待者")
	}
	l.release()
}

// TestResizableSemCancel ctx 取消应唤醒等待者且 acquire 返回 false（调用方不得 release）。
func TestResizableSemCancel(t *testing.T) {
	l := newResizableSem(1)
	ctx := context.Background()
	if !l.acquire(ctx) {
		t.Fatal("首槽失败")
	}
	ctx2, cancel := context.WithCancel(ctx)
	got := make(chan bool, 1)
	go func() { got <- l.acquire(ctx2) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case v := <-got:
		if v {
			t.Fatal("取消后 acquire 返回 true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消未唤醒等待者")
	}
	l.release()
}

// TestResizableSemPreCanceled ctx 已取消时 acquire 直接失败。
func TestResizableSemPreCanceled(t *testing.T) {
	l := newResizableSem(2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l.acquire(ctx) {
		t.Fatal("已取消的 ctx 不应拿到槽位")
	}
	if a, _ := l.current(); a != 0 {
		t.Fatalf("active=%d 应保持 0", a)
	}
}

// TestClamp 边界与越界标记。
func TestClamp(t *testing.T) {
	cases := []struct {
		v, lo, hi int
		want      int
		ok        bool
	}{
		{5, 1, 16, 5, true},
		{0, 1, 16, 1, false},
		{99, 1, 16, 16, false},
	}
	for _, c := range cases {
		v := c.v
		if ok := clamp(&v, c.lo, c.hi); v != c.want || ok != c.ok {
			t.Fatalf("clamp(%d)=%d ok=%v want %d ok=%v", c.v, v, ok, c.want, c.ok)
		}
	}
}

// saveRestoreConfig 保存并恢复配置相关全局，避免测试互相污染。
func saveRestoreConfig(t *testing.T, p string) {
	t.Helper()
	oldPath, oldLimiter, oldCfg := testStd.configPath, testStd.limiter, testStd.cfg
	oldConc, oldRetries, oldProxy := testStd.concurrencyNow(), testStd.maxRetriesNow(), testStd.getProxyAddr()
	t.Cleanup(func() {
		testStd.configPath, testStd.limiter, testStd.cfg = oldPath, oldLimiter, oldCfg
		testStd.setDownloadTuning(oldConc, oldRetries)
		testStd.setProxyAddr(oldProxy)
	})
	testStd.configPath = p
	testStd.limiter = newResizableSem(defaultConfig().MaxConcurrent)
}

// TestLoadConfigFromFile 合法配置读盘并应用到限制器与全局变量。
func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	os.WriteFile(p, []byte(`{"maxConcurrent":5,"segConcurrency":20,"maxRetries":7,"port":8899,"uiView":"compact","proxy":"http://192.168.1.2:8888"}`), 0644)
	saveRestoreConfig(t, p)

	testStd.loadConfig()
	if testStd.cfg.MaxConcurrent != 5 || testStd.cfg.SegConcurrency != 20 || testStd.cfg.MaxRetries != 7 || testStd.cfg.Port != 8899 || testStd.cfg.UIView != "compact" {
		t.Fatalf("testStd.cfg=%+v", testStd.cfg)
	}
	if a, lim := testStd.limiter.current(); lim != 5 || a != 0 {
		t.Fatalf("testStd.limiter=%d,%d want 0,5", a, lim)
	}
	if testStd.concurrencyNow() != 20 || testStd.maxRetriesNow() != 7 {
		t.Fatalf("concurrency=%d maxRetries=%d want 20,7", testStd.concurrencyNow(), testStd.maxRetriesNow())
	}
	if testStd.cfg.Proxy != "http://192.168.1.2:8888" || testStd.getProxyAddr() != "http://192.168.1.2:8888" {
		t.Fatalf("proxy testStd.cfg=%q 运行时=%q", testStd.cfg.Proxy, testStd.getProxyAddr())
	}
}

// TestLoadConfigFallsBack 损坏 JSON / 越界值 / 非法 port、uiView 与 proxy 回退默认。
func TestLoadConfigFallsBack(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)

	os.WriteFile(p, []byte("{not json"), 0644)
	testStd.loadConfig()
	if testStd.cfg.MaxConcurrent != 3 || testStd.cfg.SegConcurrency != 10 || testStd.cfg.MaxRetries != 3 {
		t.Fatalf("损坏 JSON 应回退默认: %+v", testStd.cfg)
	}

	os.WriteFile(p, []byte(`{"maxConcurrent":99,"segConcurrency":0,"maxRetries":-1,"port":0,"uiView":"bogus","proxy":"socks5://x:1080"}`), 0644)
	testStd.loadConfig()
	if testStd.cfg.MaxConcurrent != 16 || testStd.cfg.SegConcurrency != 1 || testStd.cfg.MaxRetries != 0 {
		t.Fatalf("越界值应被 clamp: %+v", testStd.cfg)
	}
	if testStd.cfg.Port != DefaultPort {
		t.Fatalf("port=0 应回退默认 %d, got %d", DefaultPort, testStd.cfg.Port)
	}
	if testStd.cfg.UIView != "detail" {
		t.Fatalf("非法 uiView 应回退 detail, got %q", testStd.cfg.UIView)
	}
	if testStd.cfg.Proxy != defaultConfig().Proxy {
		t.Fatalf("非法 proxy 应回退默认 %q, got %q", defaultConfig().Proxy, testStd.cfg.Proxy)
	}

	// 旧配置文件无 proxy 字段（空串）= 未配置过，回退默认而不是直连
	os.WriteFile(p, []byte(`{"proxy":""}`), 0644)
	testStd.loadConfig()
	if testStd.cfg.Proxy != defaultConfig().Proxy {
		t.Fatalf("空 proxy 应回退默认 %q, got %q", defaultConfig().Proxy, testStd.cfg.Proxy)
	}

	// direct 关键字显式直连，跨重启保持
	os.WriteFile(p, []byte(`{"proxy":"direct"}`), 0644)
	testStd.loadConfig()
	if testStd.cfg.Proxy != "direct" || testStd.getProxyAddr() != "direct" {
		t.Fatalf("direct 应保持直连: testStd.cfg=%q 运行时=%q", testStd.cfg.Proxy, testStd.getProxyAddr())
	}

	// system 关键字跟随系统代理（新默认），跨重启保持；大小写不敏感
	os.WriteFile(p, []byte(`{"proxy":"System"}`), 0644)
	testStd.loadConfig()
	if testStd.cfg.Proxy != "system" || testStd.getProxyAddr() != "system" {
		t.Fatalf("system 应保持跟随系统: testStd.cfg=%q 运行时=%q", testStd.cfg.Proxy, testStd.getProxyAddr())
	}
}

// TestSaveConfigLocked 落盘为可解析的 JSON，字段与 cfg 一致。
func TestSaveConfigLocked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)

	testStd.cfg = appConfig{MaxConcurrent: 4, SegConcurrency: 12, MaxRetries: 2, Port: 7000, UIView: "compact", Proxy: "direct"}
	testStd.saveConfigLocked()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	var got appConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("落盘 JSON 解析失败: %v", err)
	}
	if got != testStd.cfg {
		t.Fatalf("落盘=%+v want %+v", got, testStd.cfg)
	}
}

// postConfig 向 /config 提交 JSON 补丁，返回响应码。
func postConfig(t *testing.T, body string) int {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(body))
	testEngine().handleConfig(w, r)
	return w.Code
}

// TestConfigProxyPatch 设置页提交代理：合法值即时生效并落盘；
// 空输入归一化为 direct；非法值 400 拒绝且不落任何配置。
func TestConfigProxyPatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)
	testStd.cfg = defaultConfig()

	if code := postConfig(t, `{"proxy":"http://10.0.0.1:8080"}`); code != 200 {
		t.Fatalf("合法代理 HTTP %d", code)
	}
	if testStd.cfg.Proxy != "http://10.0.0.1:8080" || testStd.getProxyAddr() != "http://10.0.0.1:8080" {
		t.Fatalf("代理未即时生效: testStd.cfg=%q 运行时=%q", testStd.cfg.Proxy, testStd.getProxyAddr())
	}
	var saved appConfig
	data, _ := os.ReadFile(p)
	if err := json.Unmarshal(data, &saved); err != nil || saved.Proxy != "http://10.0.0.1:8080" {
		t.Fatalf("代理未落盘: %s err=%v", data, err)
	}

	// 空输入 = 直连，落盘为显式 direct（重载时不会被当旧配置回退默认）
	if code := postConfig(t, `{"proxy":""}`); code != 200 {
		t.Fatalf("空代理 HTTP %d", code)
	}
	if testStd.cfg.Proxy != "direct" || !testStd.isDirectProxy() {
		t.Fatalf("空输入应归一化为 direct: testStd.cfg=%q", testStd.cfg.Proxy)
	}

	// system 模式合法：切回跟随系统代理
	if code := postConfig(t, `{"proxy":"system"}`); code != 200 {
		t.Fatalf("system 模式 HTTP %d", code)
	}
	if testStd.cfg.Proxy != "system" || testStd.getProxyAddr() != "system" {
		t.Fatalf("system 未生效: testStd.cfg=%q 运行时=%q", testStd.cfg.Proxy, testStd.getProxyAddr())
	}

	// 非法值 400：不修改任何配置
	if code := postConfig(t, `{"proxy":"socks5://x:1080","port":1234}`); code != 400 {
		t.Fatalf("非法代理应 400, got %d", code)
	}
	if testStd.cfg.Proxy != "system" || testStd.cfg.Port != defaultConfig().Port {
		t.Fatalf("非法值不应落任何配置: %+v", testStd.cfg)
	}
}

// TestConfigProxyGet GET /config 返回 proxy 与 systemProxy 字段（设置页回显用）。
// systemProxy 注入 stub 保证断言不依赖本机真实注册表状态。
func TestConfigProxyGet(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)
	testStd.cfg = appConfig{Proxy: "http://127.0.0.1:7890"}
	oldSys := testStd.systemProxyAddrFn
	testStd.systemProxyAddrFn = func() string { return "http://127.0.0.1:7890" }
	t.Cleanup(func() { testStd.systemProxyAddrFn = oldSys })

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/config", nil)
	testEngine().handleConfig(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `"proxy":"http://127.0.0.1:7890"`) {
		t.Fatalf("GET 响应缺 proxy 字段: %s", body)
	}
	if !strings.Contains(body, `"systemProxy":"http://127.0.0.1:7890"`) {
		t.Fatalf("GET 响应缺 systemProxy 字段: %s", body)
	}
}

// TestRuntimeTuningConcurrentAccess 配置热更新与下载热路径的无锁读并发时不得
// 出现数据竞争。旧实现把 segConcurrency/maxRetries 存成裸 int 包级变量：
// applyConfigLocked 在 cfgMu 下写，而下载路径到处无锁读 —— 本测试在 -race 下
// 会直接报 DATA RACE。
func TestRuntimeTuningConcurrentAccess(t *testing.T) {
	oldConc, oldRetries := testStd.concurrencyNow(), testStd.maxRetriesNow()
	t.Cleanup(func() { testStd.setDownloadTuning(oldConc, oldRetries) })

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if testStd.concurrencyNow() <= 0 || testStd.maxRetriesNow() < 0 {
						t.Errorf("读到非法运行参数")
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 500; i++ {
		testStd.setDownloadTuning(1+i%16, i%11)
	}
	close(stop)
	wg.Wait()
}
