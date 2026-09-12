// 本地 HTTP API 的访问控制：Host 校验 + 访问令牌。
//
// 威胁模型：服务监听 127.0.0.1，但「只有本机能连」≠「安全」——浏览器里任意网页
// 都能对 http://127.0.0.1:<port> 发起简单请求（GET 不需要预检），而本服务同时具备
// 「按调用方给的绝对路径写文件」（/download）和「执行程序」（/openfile）两种能力，
// 串起来就是：任意网页 → 本机持久化代码执行。这道门必须自己设。
//
// 两层彼此独立、各自生效：
//
//  1. Host 校验：只认 127.0.0.1 / localhost / [::1]。
//     挡的是 DNS rebinding —— 攻击者域名解析到 127.0.0.1 时，页面与 127.0.0.1
//     在浏览器眼里成了「同源」，CORS 完全帮不上忙，只有 Host 头能分辨。
//
//  2. 访问令牌：除握手端点外，所有请求必须带 ?t=<token> 或 X-GoCatcher-Token。
//     网页拿不到 token —— 未通过校验的响应一律不带 CORS 头，浏览器里只是一个
//     opaque 响应，读不出内容；而带上令牌才有 CORS 头，扩展才能读到响应体。
//
// 为什么不做 Origin 白名单：扩展 content script 发出的请求带的是「页面 origin」
// （例如 https://www.xmfyy.com），与恶意网页无法区分；真正区分两者的正是 token。
package core

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// tokenPlaceholder 内嵌页面 HTML 里的令牌占位符：服务端返回页面时替换成真 token。
	tokenPlaceholder = "__GOCATCHER_TOKEN__"

	// tokenHeader 备用传递方式（curl / 脚本比塞进 query 更干净）。
	tokenHeader = "X-GoCatcher-Token"
)

// newAPIToken 生成 16 字节随机 token（32 位十六进制）。
func newAPIToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见；退化到时间戳弱得多，但比「没有令牌」强。
		return fmt.Sprintf("%016x", uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b)
}

// ensureAPIToken 保证配置里有一枚 token 并返回它（首次调用会落盘）。
//
// 之所以持久化而不是每次启动轮换：扩展是长期存在的客户端，token 一变就得重新握手，
// 而任何一次握手失败在用户眼里都是「下载突然不能用了」。持久化并不降低防护效果——
// 网页既读不到配置文件，也读不到未授权的响应体。
func (r *Runtime) ensureAPIToken() string {
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	if r.cfg.APIToken == "" {
		r.cfg.APIToken = newAPIToken()
		if err := r.saveConfigLocked(); err != nil {
			// 落盘失败不影响本次运行（token 已在内存里），但下次启动会换一枚，
			// 扩展需要重新握手——这里至少留一条痕迹，不要把失败彻底吞掉。
			fmt.Printf("[auth] 令牌落盘失败（下次启动将更换令牌）: %v\n", err)
		}
	}
	return r.cfg.APIToken
}

// apiToken 返回当前 token（空串 = 尚未初始化，此时一律拒绝）。
func (r *Runtime) apiToken() string {
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	return r.cfg.APIToken
}

// guard 包住整个 mux：先验 Host，再验令牌。
func (r *Runtime) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 禁止浏览器按内容猜类型：JSON 响应不能被当成脚本加载
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if !hostAllowed(req.Host) {
			http.Error(w, "forbidden: unexpected Host header", http.StatusForbidden)
			return
		}

		// 预检只声明「允许什么」，不泄露任何信息；真正的请求仍要过令牌。
		if req.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", originOrWildcard(req))
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", tokenHeader+", Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		tokenOK := r.tokenAccepted(req)
		if !tokenOK && !tokenFreePath(req.URL.Path) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized：缺少或错误的访问令牌"}`)
			return
		}
		// 令牌正确才给 CORS：扩展的 content script 需要读到响应体，
		// 而拿不到令牌的调用方也就拿不到这个头。
		//
		// 这里必须挂在 tokenOK 上，不能写成"非免令牌路径才给"——免令牌路径里
		// /svc/info 的响应体含令牌、/ 与 /settings 的 HTML 里注入了令牌，
		// 一旦它们带上 CORS 头，任意网页两行 fetch 就能把令牌读走，
		// 下面所有需要令牌的端点就全部失守了（见 tokenFreePath 的说明）。
		if tokenOK {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		next.ServeHTTP(w, req)
	})
}

// tokenFreePath 报告路径是否免令牌。从路由表派生（单一来源）：表里
// needToken=false 的路径精确匹配即豁免，未命中表项的路径一律要求令牌。
//
// 为什么必须精确匹配："/" 是 catch-all，未知路径（如 /nonexistent）也会
// 落到它上面；若把 catch-all 当成免令牌，任意网页 fetch 一个未知路径就能
// 拿到注入令牌的 HTML（handleHomePage 把令牌写进了页面），令牌防线失守。
// 因此豁免只认字面路径，新增免令牌端点请先确认它不含秘密、也拿不到 CORS 头
// （见 routeDefs 的说明）。
func tokenFreePath(p string) bool {
	for _, rd := range routeDefs {
		if rd.path == p {
			return !rd.needToken
		}
	}
	return false
}

// tokenAccepted 校验请求携带的令牌（query 或 header 任一）。
func (r *Runtime) tokenAccepted(req *http.Request) bool {
	got := requestToken(req)
	want := r.apiToken()
	if got == "" || want == "" {
		return false
	}
	// 定长比较，避免通过响应时间逐字节猜 token
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func requestToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(tokenHeader)); v != "" {
		return v
	}
	return strings.TrimSpace(r.URL.Query().Get("t"))
}

func originOrWildcard(r *http.Request) string {
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
		return o
	}
	return "*"
}

// hostAllowed 只接受本机地址的 Host 头（挡 DNS rebinding，见文件头说明）。
func hostAllowed(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// injectToken 把页面里的令牌占位符替换成真 token。
// 占位符不存在时原样返回（前端忘了放占位符不应该让页面打不开）。
func (r *Runtime) injectToken(page string) string {
	if !strings.Contains(page, tokenPlaceholder) {
		return page
	}
	return strings.ReplaceAll(page, tokenPlaceholder, r.apiToken())
}
