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

// TestResizableSemBroadcastHoldsLock（新-2 结构守卫）。
//
// 缺陷形态：取消分支在**不持 l.mu** 的情况下 Broadcast。合法（sync.Cond 允许），
// 但不安全 —— 检查 `ctx.Err()` 与 `cond.Wait()` 不在同一临界区，取消恰好落在两者
// 之间时，广播发生在 `notifyListAdd` 之前、无人可唤醒，随后 Wait 永久睡下去：
// 任务卡在「排队中」，而 /resume 又因为它还占着队位不放行。
//
// 行为用例（上面的 Cancel / 下面的压力版）都只能**概率性**撞到这个窗口 —— 它只有
// 几十纳秒宽。所以这条结构守卫才是主判据：它确定性地钉住"取消方持同一把锁广播"。
func TestResizableSemBroadcastHoldsLock(t *testing.T) {
	body := strings.Join(sourceFuncBody(t, "config.go",
		"func (l *resizableSem) acquire(ctx context.Context) bool"), "\n")

	i := strings.Index(body, "ctx.Done()")
	if i < 0 {
		t.Fatal("找不到 ctx.Done() 分支 —— 切分失效，本断言会恒真")
	}
	branch := body[i:]

	iLock := strings.Index(branch, "l.mu.Lock()")
	iBc := strings.Index(branch, "l.cond.Broadcast()")
	if iLock < 0 || iBc < 0 {
		t.Fatal("取消分支里看不到 l.mu.Lock() / l.cond.Broadcast() —— 切分失效")
	}
	if iLock > iBc {
		t.Error("Broadcast 出现在 l.mu.Lock() 之前 —— 等于没持锁，会丢失唤醒")
	}
	// 防退化：Broadcast 之后必须 Unlock（否则是"锁住不还"）
	if !strings.Contains(branch[iBc:], "l.mu.Unlock()") {
		t.Error("取消分支广播后没有 l.mu.Unlock() —— 会把锁一直握在手里，等待者全醒不来")
	}
}

// TestResizableSemNoLostWakeup（新-2 行为，压力版）。
//
// 与 TestResizableSemCancel 的区别：**取消前不 sleep**，让"取消"与"主协程进 Wait"
// 的先后随机化，多轮重复以争取撞进那个窄窗口。修前它**可能**挂死，修后必须恒不挂。
// 这里断言的是"任何一轮都不许卡住"—— 挂死是最坏的用户可见后果（任务永远停在排队中）。
func TestResizableSemNoLostWakeup(t *testing.T) {
	const rounds = 3000
	var doneRounds int
	deadline := time.Now().Add(6 * time.Second)
	for i := 0; i < rounds && time.Now().Before(deadline); i++ {
		l := newResizableSem(1)
		if !l.acquire(context.Background()) {
			t.Fatal("首槽失败")
		}
		ctx, cancel := context.WithCancel(context.Background())
		got := make(chan bool, 1)
		go func() { got <- l.acquire(ctx) }()
		cancel()
		select {
		case v := <-got:
			if v {
				t.Fatalf("第 %d 轮：取消后仍拿到槽位", i)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("第 %d 轮：取消后 500ms 未返回 —— 丢失唤醒（检查与 Wait 分离）", i)
		}
		l.release()
		doneRounds++
	}
	if doneRounds < 50 {
		t.Fatalf("只跑了 %d 轮（下限 50）—— 压力不足等于没测", doneRounds)
	}
	t.Logf("压力轮次: %d", doneRounds)
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

// TestLoadConfigMissingIntFieldsFallBackToDefault（A-7）：
// 整型键**缺失**要回退默认，而不是被夹到下限。
//
// 缺陷形态：读盘路径对三个整型一律 clamp ⇒ 缺字段（= 反序列化成 0）被夹成 1/1/0。
// 旧配置或手改删过项的配置就会命中：并发变 1、重试变 0（任何一次失败直接放弃），
// 观感是"下载突然极慢/一失败就停"，而且**没有任何提示**（GUI 无控制台）。
// 同函数里 port / uiView / proxy 三处本来就是"空值回退默认"，只有这三个不是 ——
// 同一段代码两套语义，说明是漏改而不是有意设计。
func TestLoadConfigMissingIntFieldsFallBackToDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)
	def := defaultConfig()

	// 只写 port：三个整型键全缺
	os.WriteFile(p, []byte(`{"port":8899}`), 0644)
	testStd.loadConfig()
	if testStd.cfg.MaxConcurrent != def.MaxConcurrent ||
		testStd.cfg.SegConcurrency != def.SegConcurrency ||
		testStd.cfg.MaxRetries != def.MaxRetries {
		t.Fatalf("整型键缺失应回退默认 %d/%d/%d，实际 %d/%d/%d",
			def.MaxConcurrent, def.SegConcurrency, def.MaxRetries,
			testStd.cfg.MaxConcurrent, testStd.cfg.SegConcurrency, testStd.cfg.MaxRetries)
	}
	if testStd.cfg.Port != 8899 {
		t.Fatalf("port 键在，应保留 8899，got %d", testStd.cfg.Port)
	}

	// 反向：键在且越界仍要夹取（不能把两种处置混成一条"一律回退默认"）
	// —— 这里同时验"同一次读盘里，键在的夹取、键不在的回退默认"。
	os.WriteFile(p, []byte(`{"maxConcurrent":999}`), 0644)
	testStd.loadConfig()
	if testStd.cfg.MaxConcurrent != 16 {
		t.Fatalf("键在且越界应夹到 16，got %d", testStd.cfg.MaxConcurrent)
	}
	if testStd.cfg.SegConcurrency != def.SegConcurrency || testStd.cfg.MaxRetries != def.MaxRetries {
		t.Fatalf("同一次读盘里缺失的项仍应回退默认 %d/%d，实际 %d/%d",
			def.SegConcurrency, def.MaxRetries, testStd.cfg.SegConcurrency, testStd.cfg.MaxRetries)
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
					// 判据是"读到可用的运行参数"：并发 ≠ 0、重试次数 ≥ 1。
					// 这里**必须用读取入口**（而不是裸字段）：重试次数的存储值可以是 0
					// （设置页允许），下限夹取发生在 maxRetriesNow() 里 ——
					// 写成 `< 0` 的话，夹取被删掉也不会红，等于没查。
					if testStd.concurrencyNow() <= 0 || testStd.maxRetriesNow() < 1 {
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
