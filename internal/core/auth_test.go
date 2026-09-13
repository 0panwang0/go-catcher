// 本地 API 访问控制测试：guard（Host / 令牌 / CORS）、下载目录白名单、
// 「打开文件/文件夹」按任务 id 解析。
//
// 这些用例对应评审里的 P0 安全问题：修复前，任意网页都能用简单请求驱动本机服务
// 写任意路径、执行任意文件、做 SSRF。
package core

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setTestToken 覆盖当前令牌，Cleanup 时还原。
func setTestToken(t *testing.T, tok string) {
	t.Helper()
	testStd.cfgMu.Lock()
	old := testStd.cfg.APIToken
	testStd.cfg.APIToken = tok
	testStd.cfgMu.Unlock()
	t.Cleanup(func() {
		testStd.cfgMu.Lock()
		testStd.cfg.APIToken = old
		testStd.cfgMu.Unlock()
	})
}

// resetAllowedDirs 清空白名单，Cleanup 时还原。
func resetAllowedDirs(t *testing.T) {
	t.Helper()
	testStd.allowedDirMu.Lock()
	old := testStd.allowedDirs
	testStd.allowedDirs = map[string]bool{}
	testStd.allowedDirMu.Unlock()
	t.Cleanup(func() {
		testStd.allowedDirMu.Lock()
		testStd.allowedDirs = old
		testStd.allowedDirMu.Unlock()
	})
}

// guardedDo 发一个经过 guard 的请求；handler 恒返回 200，便于区分是 guard 拦下还是放行。
func guardedDo(t *testing.T, method, target, host string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, target, nil)
	if host != "" {
		r.Host = host
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	testStd.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("reached"))
	})).ServeHTTP(w, r)
	return w
}

func TestGuardRequiresToken(t *testing.T) {
	setTestToken(t, "secret-token")

	if w := guardedDo(t, "GET", "/status", "127.0.0.1:7891", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得到 %d", w.Code)
	}
	if w := guardedDo(t, "GET", "/status?t=wrong", "127.0.0.1:7891", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("错误令牌应 401，得到 %d", w.Code)
	}
	if w := guardedDo(t, "GET", "/status?t=secret-token", "127.0.0.1:7891", nil); w.Code != http.StatusOK {
		t.Fatalf("query 令牌应放行，得到 %d", w.Code)
	}
	if w := guardedDo(t, "GET", "/status", "127.0.0.1:7891", map[string]string{tokenHeader: "secret-token"}); w.Code != http.StatusOK {
		t.Fatalf("header 令牌应放行，得到 %d", w.Code)
	}
}

// TestGuardTokenFreePaths 探活/握手/页面本身不需要令牌，否则扩展和界面都无法自举。
func TestGuardTokenFreePaths(t *testing.T) {
	setTestToken(t, "secret-token")
	for _, p := range []string{"/health", "/svc/info", "/", "/settings"} {
		if w := guardedDo(t, "GET", p, "127.0.0.1:7891", nil); w.Code != http.StatusOK {
			t.Errorf("%s 应免令牌，得到 %d", p, w.Code)
		}
	}
	// 未列出的路径仍然要令牌（/log 含本机路径，同样必须鉴权）
	for _, p := range []string{"/config", "/log", "/status", "/download"} {
		if w := guardedDo(t, "GET", p, "127.0.0.1:7891", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s 应要令牌，得到 %d", p, w.Code)
		}
	}
}

// TestGuardFrameDeniedPages 注入令牌的页面必须禁止被 iframe 嵌套（点击劫持）：
// 它们免令牌且 HTML 里带着真令牌，任意网页透明 iframe 覆盖诱导点击，就能让
// 页面自身去 POST /config 改代理（流量经中间人）或改端口。Host 校验拦不住 ——
// iframe 发请求时 Host 就是本机地址。
func TestGuardFrameDeniedPages(t *testing.T) {
	setTestToken(t, "secret-token")
	for _, p := range []string{"/", "/settings"} {
		w := guardedDo(t, "GET", p, "127.0.0.1:7891", nil)
		if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s X-Frame-Options=%q want DENY", p, got)
		}
		if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
			t.Errorf("%s Content-Security-Policy=%q 应含 frame-ancestors 'none'", p, got)
		}
	}
}

// TestGuardRejectsForeignHost DNS rebinding 防护：Host 不是本机地址一律 403，
// 即使令牌正确——这条挡的是"攻击者域名解析到 127.0.0.1 后与页面同源"。
func TestGuardRejectsForeignHost(t *testing.T) {
	setTestToken(t, "secret-token")
	for _, h := range []string{"evil.example:7891", "evil.example", "", "127.0.0.1.evil.example:7891"} {
		if w := guardedDo(t, "GET", "/status?t=secret-token", h, nil); w.Code != http.StatusForbidden {
			t.Errorf("Host=%q 应 403，得到 %d", h, w.Code)
		}
	}
	for _, h := range []string{"127.0.0.1:7891", "localhost:7891", "[::1]:7891"} {
		if w := guardedDo(t, "GET", "/status?t=secret-token", h, nil); w.Code != http.StatusOK {
			t.Errorf("Host=%q 应放行，得到 %d", h, w.Code)
		}
	}
}

// TestGuardCORSOnlyWithToken 未通过令牌校验的响应绝不能带 CORS 头，
// 否则网页能读到响应体，令牌形同虚设。
//
// 必须枚举全部路径，不能只挑一个代表：这条不变量第一次被漏掉，正是因为
// 只采样了 /status——一个"恰好行为正确"的路径，而真正出事的是 /svc/info
// 这类免令牌端点，它们的响应体里就有令牌。
func TestGuardCORSOnlyWithToken(t *testing.T) {
	setTestToken(t, "secret-token")
	evil := map[string]string{"Origin": "https://evil.example"}

	// 需要令牌的路径：401，且不带 CORS 头
	for _, p := range []string{"/status", "/config", "/log", "/download", "/probe", "/svc/stop"} {
		w := guardedDo(t, "GET", p, "127.0.0.1:7891", evil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s 无令牌应 401，得到 %d", p, w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s（未授权）不应带 CORS 头，得到 %q", p, got)
		}
	}

	// 免令牌路径：放行，但绝不能带 CORS 头——它们是"不含秘密"才被豁免的，
	// 而 /svc/info 的响应体里就有令牌、/ 与 /settings 的 HTML 里注入了令牌。
	for _, p := range []string{"/health", "/svc/info", "/", "/settings"} {
		if !tokenFreePath(p) {
			t.Fatalf("%s 不在免令牌清单里，本用例已失效", p)
		}
		w := guardedDo(t, "GET", p, "127.0.0.1:7891", evil)
		if w.Code != http.StatusOK {
			t.Errorf("%s 应免令牌放行，得到 %d", p, w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s（免令牌）绝不能带 CORS 头，否则响应体里的令牌会被任意网页读走；得到 %q", p, got)
		}
	}

	// 带对令牌：必须带 CORS 头，否则扩展的 content script 读不到响应体
	w := guardedDo(t, "GET", "/status?t=secret-token", "127.0.0.1:7891", map[string]string{"Origin": "https://page.example"})
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("授权响应应带 CORS 头，得到 %q", got)
	}
	// 免令牌路径带上正确令牌时同样可以拿到 CORS 头（扩展靠 /svc/info?t= 兜底重握手），
	// 这不再是泄露——令牌本身就是通行证。
	w = guardedDo(t, "GET", "/svc/info?t=secret-token", "127.0.0.1:7891", evil)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("带令牌的 /svc/info 应带 CORS 头，得到 %q", got)
	}
}

func TestGuardOptionsPreflight(t *testing.T) {
	setTestToken(t, "secret-token")
	w := guardedDo(t, http.MethodOptions, "/download", "127.0.0.1:7891", map[string]string{"Origin": "chrome-extension://abc"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("预检应 204，得到 %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Fatal("预检应声明允许的请求头")
	}
}

func TestInjectToken(t *testing.T) {
	setTestToken(t, "tok-123")
	page := `<script>var API_TOKEN = "__GOCATCHER_TOKEN__";</script>`
	got := testStd.injectToken(page)
	if got != `<script>var API_TOKEN = "tok-123";</script>` {
		t.Fatalf("令牌未注入: %q", got)
	}
	// 没有占位符时原样返回（前端漏放占位符不该让页面打不开）
	if got := testStd.injectToken("<html></html>"); got != "<html></html>" {
		t.Fatalf("无占位符应原样返回: %q", got)
	}
}

// TestSaveDirAllowlist /download 的 dir 只认用户选过的目录及其子目录。
func TestSaveDirAllowlist(t *testing.T) {
	resetAllowedDirs(t)
	root := t.TempDir()
	sub := filepath.Join(root, "子目录")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	if testStd.isAllowedSaveDir(root) {
		t.Fatal("未登记目录不应放行")
	}
	testStd.allowSaveDir(root)
	if !testStd.isAllowedSaveDir(root) {
		t.Fatal("已登记目录应放行")
	}
	if !testStd.isAllowedSaveDir(sub) {
		t.Fatal("已登记目录的子目录应放行")
	}
	if testStd.isAllowedSaveDir(outside) {
		t.Fatal("白名单外目录不应放行")
	}
	if testStd.isAllowedSaveDir(filepath.Join(root, "不存在")) {
		t.Fatal("不存在的目录不应放行")
	}
	if testStd.isAllowedSaveDir(filepath.Join(root, "..", filepath.Base(outside))) {
		t.Fatal("通过 .. 绕出的路径不应放行")
	}
}

func TestTaskOpenPathAndDir(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "v.ts")
	if err := os.WriteFile(final, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	te := &taskEntry{rt: testStd, st: taskState{
		id: "top", done: true, stage: "已保存",
		filename: "v.ts", saveDir: dir, finalPath: final,
	}}
	testStd.tasks[te.st.id] = te

	if p, status, msg := testEngine().taskOpenPath("top"); status != 0 || p != final {
		t.Fatalf("taskOpenPath 应返回成品路径: %q status=%d msg=%s", p, status, msg)
	}
	if d, status, msg := testEngine().taskOpenDir("top"); status != 0 || d != dir {
		t.Fatalf("taskOpenDir 应返回保存目录: %q status=%d msg=%s", d, status, msg)
	}
	if _, status, _ := testEngine().taskOpenPath(""); status != http.StatusBadRequest {
		t.Fatalf("缺 id 应 400，得到 %d", status)
	}
	if _, status, _ := testEngine().taskOpenPath("nope"); status != http.StatusNotFound {
		t.Fatalf("未知 id 应 404，得到 %d", status)
	}
	// 未完成的空任务没有可打开的文件
	empty := &taskEntry{rt: testStd, st: taskState{id: "tempty", paused: true, saveDir: dir}}
	testStd.tasks[empty.st.id] = empty
	if _, status, _ := testEngine().taskOpenPath("tempty"); status != http.StatusNotFound {
		t.Fatalf("无产物应 404，得到 %d", status)
	}
}

// TestHandleDownloadRejectsForeignDir 修复前 dir 可以是任意绝对路径，
// 配合攻击者控制的 m3u8 即为"任意目录写文件"。
func TestHandleDownloadRejectsForeignDir(t *testing.T) {
	resetAllowedDirs(t)
	dir := t.TempDir()
	q := url.Values{}
	q.Set("m3u8", "https://cdn.example.com/a.m3u8")
	q.Set("mode", "disk")
	q.Set("dir", dir)
	q.Set("filename", "a.ts")

	w := httptest.NewRecorder()
	testEngine().handleDownload(w, httptest.NewRequest(http.MethodGet, "/download?"+q.Encode(), nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("白名单外 dir 应 400，得到 %d", w.Code)
	}
}

// TestHandleDownloadRejectsExecutableFilename 服务把远端内容原样落盘，
// 允许写 .exe/.bat 就等于提供一个"落盘可执行文件"的原语。
func TestHandleDownloadRejectsExecutableFilename(t *testing.T) {
	resetAllowedDirs(t)
	dir := t.TempDir()
	testStd.allowSaveDir(dir)

	for _, name := range []string{"evil.exe", "run.bat", "x.ps1", "a.lnk", "d.dll"} {
		q := url.Values{}
		q.Set("m3u8", "https://cdn.example.com/a.m3u8")
		q.Set("mode", "disk")
		q.Set("dir", dir)
		q.Set("filename", name)

		w := httptest.NewRecorder()
		testEngine().handleDownload(w, httptest.NewRequest(http.MethodGet, "/download?"+q.Encode(), nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("filename=%s 应 400，得到 %d", name, w.Code)
		}
	}
}

// TestSanitizeFilenameStripsADS 冒号在 NTFS 上是备用数据流分隔符，必须去掉。
func TestSanitizeFilenameStripsADS(t *testing.T) {
	if got := sanitizeFilename("a.mp4:hidden"); got != "a.mp4_hidden" {
		t.Fatalf("应去掉冒号: %q", got)
	}
	if got := sanitizeFilename("..\\..\\evil.ts"); got != ".._.._evil.ts" {
		t.Fatalf("应去掉路径分隔符: %q", got)
	}
}

// TestMuxEndToEnd 走完整路由（newMux + guard），确认各攻击面在真实链路上被挡住。
// 比单测 guard 更进一步：证明端点自己也没有绕过鉴权/校验。
func TestMuxEndToEnd(t *testing.T) {
	setTestToken(t, "e2e-token")
	resetAllowedDirs(t)
	saveRestoreState(t)

	h := testStd.guard(newMux(testEngine()))

	do := func(target string, headers map[string]string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Host = "127.0.0.1:7891"
		for k, v := range headers {
			if strings.EqualFold(k, "Host") {
				r.Host = v // Host 在 net/http 里由 r.Host 表示，不是普通 header
				continue
			}
			r.Header.Set(k, v)
		}
		h.ServeHTTP(w, r)
		return w
	}

	// 1) 无令牌：除探活外一律 401
	if w := do("/status", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("/status 无令牌应 401，得到 %d", w.Code)
	}
	if w := do("/health", nil); w.Code != http.StatusOK {
		t.Errorf("/health 应免令牌，得到 %d", w.Code)
	}
	// 2) DNS rebinding：Host 不是本机 → 403（哪怕带对了令牌）
	if w := do("/status?t=e2e-token", map[string]string{"Host": "evil.example"}); w.Code != http.StatusForbidden {
		t.Errorf("外部 Host 应 403，得到 %d", w.Code)
	}
	// 3) 带令牌：正常放行
	if w := do("/status?t=e2e-token", nil); w.Code != http.StatusOK {
		t.Errorf("/status 带令牌应 200，得到 %d body=%s", w.Code, w.Body.String())
	}
	// 4) /openfile 只认任务 id：未知 id → 404，并且不再接受 path 参数
	if w := do("/openfile?t=e2e-token&id=nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("未知 id 应 404，得到 %d", w.Code)
	}
	if w := do("/openfile?t=e2e-token&path=C%3A%5CWindows%5CSystem32%5Ccalc.exe", nil); w.Code != http.StatusBadRequest {
		t.Errorf("只带 path（旧签名）应 400，得到 %d", w.Code)
	}
	// 5) /download 的白名单校验
	dir := t.TempDir()
	q := url.Values{}
	q.Set("t", "e2e-token")
	q.Set("m3u8", "https://cdn.example.com/a.m3u8")
	q.Set("mode", "disk")
	q.Set("dir", dir)
	if w := do("/download?"+q.Encode(), nil); w.Code != http.StatusBadRequest {
		t.Errorf("白名单外 dir 应 400，得到 %d", w.Code)
	}
	// 6) /probe 不能当 SSRF 跳板
	if w := do("/probe?t=e2e-token&url=http%3A%2F%2F169.254.169.254%2Flatest%2Fmeta-data%2F", nil); w.Code != http.StatusBadRequest {
		t.Errorf("云元数据地址应 400，得到 %d", w.Code)
	}
}
