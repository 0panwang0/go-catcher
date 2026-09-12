// 加固回归：2026-09-11 评审的 P2 批次（唯一命名原子认领 / 方法校验 /
// JSON 响应统一 / 0 字节占位不当作可打开文件）。
package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestUniquePathConcurrentClaims 并发认领同名目标必须各得其所（P2-8）。
// 修复前只有磁盘检查，两个并发 /download 在彼此都还没创建 .part 的瞬间会拿到
// 同一个路径，然后互相写同一个文件、抢同一个成品名。
func TestUniquePathConcurrentClaims(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "v.ts")

	const n = 8
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := uniquePath(target)
			if err != nil {
				t.Errorf("第 %d 个认领失败: %v", i, err)
				return
			}
			got[i] = p
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, p := range got {
		if p == "" {
			t.Fatalf("第 %d 个认领为空", i)
		}
		if seen[p] {
			t.Fatalf("路径被重复认领: %s（并发任务会互相写同一个文件）", p)
		}
		seen[p] = true
		if !fileExists(p + ".part") {
			t.Errorf("%s 返回时应已创建 .part 占位", p)
		}
	}
	if !seen[target] {
		t.Errorf("首个认领者应拿到原始路径 %s，实际得到 %v", target, got)
	}
}

// TestUniquePathReportsUnusableDir 目录不可用时如实报错，不能靠"换个名字"绕过去
// （否则退化成在同一个坏目录里空转到尝试次数上限）。
func TestUniquePathReportsUnusableDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created")
	if _, err := uniquePath(filepath.Join(missing, "v.ts")); err == nil {
		t.Fatal("目录不存在时应报错")
	}
}

// TestUniquePathStillAvoidsExistingFiles 保留原有语义：正式文件已存在就换名。
func TestUniquePathStillAvoidsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "v.ts")
	if err := os.WriteFile(target, []byte("done"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := uniquePath(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "v (1).ts") {
		t.Fatalf("已有成品时应让位，得到 %q", got)
	}
}

// TestSnapshotIgnoresEmptyPart 排队中的任务只有一个 0 字节占位，
// 不该被报成"可打开的半成品"。
func TestSnapshotIgnoresEmptyPart(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "v.ts")
	te := &taskEntry{rt: testStd, st: taskState{id: "q", queued: true, finalPath: final}}

	if err := os.WriteFile(final+".part", nil, 0644); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(te).openPath; got != "" {
		t.Fatalf("0 字节占位不该当作可打开文件，得到 %q", got)
	}

	if err := os.WriteFile(final+".part", []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(te).openPath; got != final+".part" {
		t.Fatalf("有内容的 .part 应可打开，得到 %q", got)
	}
}

// TestHandlersRejectWrongMethod 端点只接受约定的方法（P2-2）。
// 跨源"简单请求"只限 GET/POST/HEAD，方法校验能挡掉 <img src>、<form> 这类
// 不需要 CORS 就能发出去的噪声请求。方法约束收敛在路由表（routeDefs）里由
// methodGuard 统一执行，本测试遍历整张表，保证任何端点都不会漏声明方法。
func TestHandlersRejectWrongMethod(t *testing.T) {
	setTestToken(t, "method-token")
	h := testStd.guard(newMux(testEngine()))

	candidate := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete}
	for _, rd := range routeDefs {
		// 挑一个不在该端点方法集里的方法作反面样本
		var bad string
		for _, m := range candidate {
			if !slices.Contains(rd.methods, m) {
				bad = m
				break
			}
		}
		if bad == "" {
			continue // 端点声明了全部候选方法（当前不存在）
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(bad, rd.path, nil)
		r.Host = "127.0.0.1:7891"
		if rd.needToken {
			q := r.URL.Query()
			q.Set("t", "method-token")
			r.URL.RawQuery = q.Encode()
		}
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s 应 405，得到 %d", bad, rd.path, w.Code)
		}
		if got := w.Header().Get("Allow"); got != strings.Join(rd.methods, ", ") {
			t.Errorf("%s 的 Allow 应为 %q，得到 %q", rd.path, strings.Join(rd.methods, ", "), got)
		}
	}
}

// TestSvcInfoJSONWellFormed /svc/info 走 encoding/json（P2-1）：
// %q 是 strconv.Quote，对控制字符会产出 JSON 非法转义，前端整块解析失败。
//
// 用独立运行时并在设令牌前显式初始化配置：handleSvcInfo 里的 e.Port() 会走到
// initRuntimeConfig()，它从磁盘重装配置（sync.Once，首次调用生效）。若那次装载
// 发生在设令牌之后，令牌就被换掉了——断言会拿到一枚随机新令牌而不是 "tok-abc"。
// 这是调用顺序问题，不是 handler 缺陷（生产里 initRuntimeConfig 在 mux 开始服务
// 之前就完成了），所以由用例自己把顺序固定下来，而不是放宽断言。
func TestSvcInfoJSONWellFormed(t *testing.T) {
	rt := newRuntime()
	rt.configPath = filepath.Join(t.TempDir(), "gocatcher_config.json")
	rt.initRuntimeConfig() // 先完成装载，之后的赋值不会再被覆盖

	rt.cfgMu.Lock()
	rt.cfg.APIToken = "tok-abc"
	rt.cfgMu.Unlock()

	w := httptest.NewRecorder()
	(&Engine{rt: rt}).handleSvcInfo(w, httptest.NewRequest(http.MethodGet, "/svc/info", nil))

	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type 应为 JSON，得到 %q", ct)
	}
	var got struct {
		OK    bool   `json:"ok"`
		Exe   string `json:"exe"`
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v body=%q", err, w.Body.String())
	}
	if !got.OK || got.Token != "tok-abc" {
		t.Fatalf("握手字段不对: %+v", got)
	}
}

// TestDownloadResponseJSONWellFormed 任务启动响应也必须是合法 JSON
// （扩展据此取 id 轮询进度）；同时确认任务创建即认领 .part 占位。
//
// 用独立运行时：共享 testStd 时任务要先抢并发槽，若槽位被别的用例占着，
// 任务会一直停在"排队中"，等它收尾就成了不稳定的等待。m3u8 故意给一个解析
// 必然失败的地址（httpGetPlaylist 对网络错误会按 attempt*2 秒退避重试，
// 用连不上的地址会让这个用例白等十几秒），任务立刻以"获取 m3u8 失败"收尾。
func TestDownloadResponseJSONWellFormed(t *testing.T) {
	rt := newRuntime()
	rt.configPath = filepath.Join(t.TempDir(), "gocatcher_config.json")
	rt.initRuntimeConfig()
	e := &Engine{rt: rt}

	dir := t.TempDir()
	rt.allowSaveDir(dir)

	q := url.Values{}
	q.Set("m3u8", "://no-scheme")
	q.Set("mode", "disk")
	q.Set("dir", dir)
	q.Set("filename", "a.ts")

	w := httptest.NewRecorder()
	e.handleDownload(w, httptest.NewRequest(http.MethodGet, "/download?"+q.Encode(), nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("应 202，得到 %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Started  bool   `json:"started"`
		ID       string `json:"id"`
		Dir      string `json:"dir"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v body=%q", err, w.Body.String())
	}
	if !got.Started || got.ID == "" || got.Filename != "a.ts" {
		t.Fatalf("启动字段不对: %+v", got)
	}
	// 认领的 .part 占位已存在（P2-8：任务创建即占用文件名）
	if !fileExists(filepath.Join(dir, "a.ts.part")) {
		t.Error("任务创建后应已认领 .part 占位")
	}

	// 等任务收尾（解析失败，毫秒级），避免留下仍在跑的 goroutine
	te := rt.findTask(got.ID)
	if te == nil {
		t.Fatal("任务未注册")
	}
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "任务失败收尾")
}
