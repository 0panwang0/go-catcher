// 路由表完整性测试（评审 P2-3）：表驱动路由的护栏。
//
// 表驱动把「mux 装配 / handler 内方法检查 / auth 免令牌白名单」三处收敛成
// 一处，代价是这三类约束的正确性全部押在 routeDefs 一张表上——必须用测试
// 把它钉死：路径唯一、方法约束非空、免令牌集合恰为四个已知端点。
package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRouteTableIntegrity 路由表的结构性不变量。
func TestRouteTableIntegrity(t *testing.T) {
	seen := map[string]bool{}
	for _, rd := range routeDefs {
		if rd.path == "" {
			t.Error("存在空路径的路由表项")
		}
		if seen[rd.path] {
			t.Errorf("路由路径重复: %s", rd.path)
		}
		seen[rd.path] = true
		if len(rd.methods) == 0 {
			t.Errorf("%s 未声明方法约束（表驱动下方法检查只在这里发生）", rd.path)
		}
		if rd.new == nil {
			t.Errorf("%s 缺少 handler 工厂", rd.path)
		}
	}

	// 免令牌集合必须恰为这四个：多一个 = 给网页多一个读响应体的口子，
	// 少一个 = 扩展/界面无法自举（见 auth.go tokenFreePath 的说明）。
	want := map[string]bool{"/health": true, "/svc/info": true, "/": true, "/settings": true}
	for _, rd := range routeDefs {
		if rd.needToken {
			if want[rd.path] {
				t.Errorf("%s 声明需要令牌，却出现在豁免集合中", rd.path)
			}
			continue
		}
		if !want[rd.path] {
			t.Errorf("%s 意外免令牌（needToken=false）", rd.path)
		}
		delete(want, rd.path)
	}
	if len(want) != 0 {
		t.Errorf("以下应免令牌的端点缺失: %v", want)
	}

	// 全部端点都能被 newMux 挂上（handler 工厂不 panic）
	mux := newMux(testEngine())
	for _, rd := range routeDefs {
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, rd.path, nil)); pattern == "" {
			t.Errorf("%s 未注册进 mux", rd.path)
		}
	}
}

// TestMethodGuardRejects 方法不符时回 405 并带 Allow 头（走完整 guard+mux 链路）。
func TestMethodGuardRejects(t *testing.T) {
	setTestToken(t, "mg-token")
	h := testStd.guard(newMux(testEngine()))

	do := func(method, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, target, nil)
		r.Host = "127.0.0.1:7891"
		h.ServeHTTP(w, r)
		return w
	}

	cases := []struct {
		name   string
		method string
		target string
		allow  string
	}{
		{"POST /status", http.MethodPost, "/status?t=mg-token", http.MethodGet},
		{"DELETE /status", http.MethodDelete, "/status?t=mg-token", http.MethodGet},
		{"GET /svc/stop（只认 POST）", http.MethodGet, "/svc/stop?t=mg-token", http.MethodPost},
		{"PUT /config（只认 GET/POST）", http.MethodPut, "/config?t=mg-token", "GET, POST"},
		{"DELETE /health", http.MethodDelete, "/health", http.MethodGet},
	}
	for _, c := range cases {
		w := do(c.method, c.target)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 应 405，得到 %d", c.name, w.Code)
		}
		if got := w.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s 的 Allow 应为 %q，得到 %q", c.name, c.allow, got)
		}
	}

	// 免令牌端点照样受方法约束：POST /health 不会因为免令牌就放行
	if w := do(http.MethodPost, "/health"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /health 应 405（免令牌 ≠ 免方法约束），得到 %d", w.Code)
	}

	// 不带令牌的 POST /status：guard 先拦（401），不会落到方法检查
	if w := do(http.MethodPost, "/status"); w.Code != http.StatusUnauthorized {
		t.Errorf("无令牌 POST /status 应 401（guard 先于 methodGuard），得到 %d", w.Code)
	}

	// 带令牌的未知路径落到 catch-all：404 而不是渲染首页（tokenFreePath 精确匹配）
	w := do(http.MethodGet, "/nonexistent?t=mg-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("带令牌的未知路径应 404（不渲染首页），得到 %d", w.Code)
	}
	// 未知路径无令牌：401（精确路径才豁免，catch-all 不算）
	if w := do(http.MethodGet, "/nonexistent"); w.Code != http.StatusUnauthorized {
		t.Errorf("无令牌未知路径应 401（免令牌只认字面路径），得到 %d", w.Code)
	}

	// 方法不符的 405 响应体是 JSON（前端统一读 error 字段）
	w = do(http.MethodPost, "/status?t=mg-token")
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("405 响应体应为 JSON error，得到 %s", w.Body.String())
	}
}
