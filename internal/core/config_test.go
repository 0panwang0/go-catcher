// 运行时配置与并发信号量测试：resizableSem 并发正确性、配置装载/回退/落盘。
package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	oldPath, oldLimiter, oldCfg := configPath, limiter, cfg
	oldConc, oldRetries := concurrency, maxRetries
	t.Cleanup(func() {
		configPath, limiter, cfg = oldPath, oldLimiter, oldCfg
		concurrency, maxRetries = oldConc, oldRetries
	})
	configPath = p
	limiter = newResizableSem(defaultConfig().MaxConcurrent)
}

// TestLoadConfigFromFile 合法配置读盘并应用到限制器与全局变量。
func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	os.WriteFile(p, []byte(`{"maxConcurrent":5,"segConcurrency":20,"maxRetries":7,"port":8899,"uiView":"compact"}`), 0644)
	saveRestoreConfig(t, p)

	loadConfig()
	if cfg.MaxConcurrent != 5 || cfg.SegConcurrency != 20 || cfg.MaxRetries != 7 || cfg.Port != 8899 || cfg.UIView != "compact" {
		t.Fatalf("cfg=%+v", cfg)
	}
	if a, lim := limiter.current(); lim != 5 || a != 0 {
		t.Fatalf("limiter=%d,%d want 0,5", a, lim)
	}
	if concurrency != 20 || maxRetries != 7 {
		t.Fatalf("concurrency=%d maxRetries=%d want 20,7", concurrency, maxRetries)
	}
}

// TestLoadConfigFallsBack 损坏 JSON / 越界值 / 非法 port 与 uiView 回退默认。
func TestLoadConfigFallsBack(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)

	os.WriteFile(p, []byte("{not json"), 0644)
	loadConfig()
	if cfg.MaxConcurrent != 3 || cfg.SegConcurrency != 10 || cfg.MaxRetries != 3 {
		t.Fatalf("损坏 JSON 应回退默认: %+v", cfg)
	}

	os.WriteFile(p, []byte(`{"maxConcurrent":99,"segConcurrency":0,"maxRetries":-1,"port":0,"uiView":"bogus"}`), 0644)
	loadConfig()
	if cfg.MaxConcurrent != 16 || cfg.SegConcurrency != 1 || cfg.MaxRetries != 0 {
		t.Fatalf("越界值应被 clamp: %+v", cfg)
	}
	if cfg.Port != DefaultPort {
		t.Fatalf("port=0 应回退默认 %d, got %d", DefaultPort, cfg.Port)
	}
	if cfg.UIView != "detail" {
		t.Fatalf("非法 uiView 应回退 detail, got %q", cfg.UIView)
	}
}

// TestSaveConfigLocked 落盘为可解析的 JSON，字段与 cfg 一致。
func TestSaveConfigLocked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gocatcher_config.json")
	saveRestoreConfig(t, p)

	cfg = appConfig{MaxConcurrent: 4, SegConcurrency: 12, MaxRetries: 2, Port: 7000, UIView: "compact"}
	saveConfigLocked()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	var got appConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("落盘 JSON 解析失败: %v", err)
	}
	if got != cfg {
		t.Fatalf("落盘=%+v want %+v", got, cfg)
	}
}
