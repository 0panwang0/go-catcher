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

	// embedKeyParam 内嵌豁免键的参数名：GUI 外壳把它拼进 iframe 地址，
	// 服务端据此认出"这是外壳自己的嵌套"（见 routeDef.frameGuard）。
	embedKeyParam = "e"
)

// 防嵌套端点由路由表声明（routeDef.frameGuard，见 server.go）：/ 与 /settings
// 返回「注入令牌的 HTML」且免令牌——任意网页用
// <iframe src="http://127.0.0.1:<port>/settings" style="opacity:0"> 透明覆盖，
// 诱导用户点击即可驱动页面自身的脚本 POST /config（把代理改成攻击者地址 → 流量
// 经中间人）或改端口（服务重启后失联）；监控页的 /openfile 按钮同样能被诱导点击。
// Host 校验拦不住 —— iframe 的 Host 就是本机地址，完全合法。
//
// 两个头都设：X-Frame-Options 是老浏览器/老 WebView 的兜底，CSP frame-ancestors
// 是现代浏览器的标准。
//
// 例外——GUI 外壳自己的嵌套必须放行（否则客户端直接白屏）：
// 桌面客户端的主窗是「外壳 HTML + iframe 内嵌监控页」两层，外壳用 WebView2 的
// SetHtml（等价 NavigateToString）加载，父文档是 opaque origin。CSP 的
// frame-ancestors 表达不了 opaque origin——'self'/'none' 都会把这个合法父窗口
// 一并拒掉，表现为窗口里只剩一个"禁止"图标。放行规则见 frameAllowed（embedKey
// + Sec-Fetch-Site 两条独立信号）。判断放在服务端而非外壳侧，是因为
// frame-ancestors 由被嵌页面自己声明，父窗口无法替它放宽。

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

// embedKey 生成本进程的内嵌豁免键。与 API 令牌同为 16 字节随机十六进制，
// 但用途完全不同（令牌管"谁能调用"，它管"谁可以当父窗口"），且**不落盘**——
// 只需在一个进程生命周期内保持稳定，重启即换新，无需跨启动一致。
func newEmbedKey() string { return newAPIToken() }

// embedAccepted 报告请求是否携带本进程的内嵌豁免键（见 routeDef.frameGuard 的例外说明）。
//
// 空键一律拒绝：embedKey 未初始化时（理论上只在 newRuntime 之前）不能让
// "参数缺失"和"键为空"凑成一次相等比较而放行。
func (r *Runtime) embedAccepted(req *http.Request) bool {
	got := strings.TrimSpace(req.URL.Query().Get(embedKeyParam))
	if got == "" || r.embedKey == "" {
		return false
	}
	// 定长比较，与令牌一致：不给"逐字节猜键"留时间差
	return subtle.ConstantTimeCompare([]byte(got), []byte(r.embedKey)) == 1
}

// frameAllowed 报告这次请求的嵌套是否来自 GUI 自己（见 routeDef.frameGuard）。
//
// 两条**独立且各自充分**的信号，命中任一条即放行——这不是冗余设计，而是两次
// 真实的踩坑各对应一条：
//
//  1. embedKey：外壳首次加载 iframe 时用。父文档是 opaque origin，浏览器给不出
//     任何"同源"信息，只能靠这把钥匙。
//  2. Sec-Fetch-Site: same-origin：框架**自己发起**的跳转——监控页的 ⚙ 走
//     location.href='/settings'，请求由 iframe 内的本服务文档发出，浏览器标注为同源。
//     只靠钥匙时这条链路没有钥匙可用，整页会被 DENY 挡住（点设置直接白屏）。
//     两条跳转同时把 location.search 带上了（见 web/*.html），所以即便某个 WebView
//     不发 Sec-Fetch-*，这条链路也仍有钥匙兜底。
//
// 为什么攻击者造不出这两条：
//   - embedKey 每进程随机、不落盘，只注入外壳 HTML。网页读不到外壳文档（跨源），
//     32 位十六进制猜不出来；比较走常量时间，不留逐字节试探的时间差。
//   - Sec-Fetch-* 是 Fetch 规范里的 forbidden header name（`Sec-` 前缀），页面
//     fetch/XHR 设上去会被浏览器丢弃，只能由浏览器自己填。它标 same-origin 的
//     前提是「发起文档确实位于本服务 origin」，网页做不到；DNS rebinding 想把
//     自己的页面搬进这个 origin 也不行，hostAllowed 会先把非本机 Host 判 403
//     —— 所以本函数必须在 Host 校验之后调用，顺序不能调。
//
// 最坏情况评估（万一两条同时被判为真）：被嵌套方仍然读不到页面内容——CORS 头只在
// tokenOK 时下发（见 guard），跨源 iframe 拿不到 DOM，也拿不到页面里注入的令牌。
// 所以防嵌套是第二道线，第一道是令牌；两道都失守才轮得到点击劫持（改代理 / 改端口），
// 而不是令牌泄漏。
func (r *Runtime) frameAllowed(req *http.Request) bool {
	if r.embedAccepted(req) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Sec-Fetch-Site")), "same-origin")
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

		// 注入令牌的页面默认禁止被 iframe 嵌套（点击劫持防护，见 routeDef.frameGuard）。
		// 用 routeFor 解析实际命中的路由项，而不是另立一份路径名单：未知路径会落到
		// catch-all "/"，于是同样继承防护（fail-safe），不会因漏登记而放行。
		// 放在 Host 校验之后：403 响应没必要再带这组头；也保证 frameAllowed 里
		// 那条 same-origin 判断建立在"Host 已确认本机"的前提之上。
		if rd, ok := routeFor(req.URL.Path); ok && rd.frameGuard && !r.frameAllowed(req) {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
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
