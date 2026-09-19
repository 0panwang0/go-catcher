// 分片 Referer 选用与解析的回归测试（白名单型防盗链把分片 403 掉的修复面）。
//
// 缺陷形态回顾：浏览器对分片「不带 Referer」这个信号，曾在这条链上被逐级吞掉
// （空值条目被丢弃 ⇒ segmentReferer 回退页面 Referer ⇒ 白名单型 CDN 见 Referer
// 即 403）。所以这里除了单测，还拿真 httptest 源站断言**实际发出去的请求头**——
// 逐级吞信号的缺陷，只看单测返回值是发现不了的。
package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseSegRefs(t *testing.T) {
	t.Run("空串返回 nil", func(t *testing.T) {
		m, err := parseSegRefs("")
		if err != nil || m != nil {
			t.Fatalf("empty => m=%v err=%v, want nil/nil", m, err)
		}
	})
	t.Run("空白串返回 nil", func(t *testing.T) {
		m, err := parseSegRefs("   ")
		if err != nil || m != nil {
			t.Fatalf("blank => m=%v err=%v, want nil/nil", m, err)
		}
	})
	t.Run("非法 JSON 报错", func(t *testing.T) {
		if _, err := parseSegRefs("{not-json"); err == nil {
			t.Fatal("invalid JSON should error")
		}
	})
	t.Run("合法 map 且键归一小写", func(t *testing.T) {
		m, err := parseSegRefs(`{"CDN.EXAMPLE:8443":"https://parser.example/"}`)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if m["cdn.example:8443"] != "https://parser.example/" {
			t.Fatalf("lowercased key lookup = %v", m)
		}
	})
	t.Run("空 host 条目被丢弃", func(t *testing.T) {
		m, err := parseSegRefs(`{"": "https://parser.example/", "   ": "x"}`)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(m) != 0 {
			t.Fatalf("empty-host entries should be dropped, got %v", m)
		}
	})
	// 空值 referer 不是「缺项」，是「浏览器对该 host 没带 Referer」，必须原样保留：
	// 丢掉它 segmentReferer 就会回退页面 Referer。
	//
	// 扰动（改回去这条必红）：把 parseSegRefs 的 `if h == "" {` 改回
	// `if h == "" || r == "" {`。
	t.Run("空值 referer 条目保留", func(t *testing.T) {
		m, err := parseSegRefs(`{"cdn.example:8443":""}`)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		ref, ok := m["cdn.example:8443"]
		if !ok {
			t.Fatalf("空值条目被丢弃了：%v", m)
		}
		if ref != "" {
			t.Fatalf("空值条目值应为空串，got %q", ref)
		}
	})
}

func TestSegmentReferer(t *testing.T) {
	j := &dlJob{
		referer: "https://page.example/",
		segRefs: map[string]string{"cdn.example:8443": "https://parser.example/"},
	}

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"host 命中返回分片 Referer", "http://cdn.example:8443/seg-1.ts", "https://parser.example/"},
		{"host 大小写不敏感", "HTTP://CDN.EXAMPLE:8443/seg-1.ts", "https://parser.example/"},
		{"host 未命中回退页面 Referer", "http://other-cdn.example/a.ts", "https://page.example/"},
		{"空 URL 回退页面 Referer", "", "https://page.example/"},
		{"非法 URL 回退页面 Referer", "%gh%zz", "https://page.example/"},
	}
	for _, c := range cases {
		if got := j.segmentReferer(c.url); got != c.want {
			t.Errorf("%s: segmentReferer(%q) = %q, want %q", c.name, c.url, got, c.want)
		}
	}

	t.Run("segRefs 为空时始终回退", func(t *testing.T) {
		empty := &dlJob{referer: "https://page.example/", segRefs: nil}
		if got := empty.segmentReferer("http://cdn.example:8443/seg-1.ts"); got != "https://page.example/" {
			t.Fatalf("nil segRefs => %q, want page referer", got)
		}
	})
	// 命中空值 = 浏览器对该 host 没带 Referer ⇒ 返回空串（调用方据此整头不设），
	// 绝不能回落页面 Referer。
	//
	// 扰动（改回去这条必红）：把 segmentReferer 的 `if !ok { return j.referer }`
	// 改回 `if !ok || ref == "" { return j.referer }`。
	t.Run("空值条目命中并返回空", func(t *testing.T) {
		gap := &dlJob{referer: "https://page.example/", segRefs: map[string]string{"cdn.example:8443": ""}}
		if got := gap.segmentReferer("http://cdn.example:8443/seg-1.ts"); got != "" {
			t.Fatalf("空值条目 => %q, want 空串（不设 Referer）", got)
		}
	})
	// 浏览器 `new URL().host` 会消掉默认端口，Go 的 url.Host 不会 —— 目标 URL 里
	// 显式写了 :443 / :80 时，必须仍能命中浏览器上报的无端口键，否则白名单型 CDN
	// 上照样 403（修复形同没生效）。
	//
	// 扰动（改回去这条必红）：删掉 segmentReferer 里的默认端口兜底分支。
	t.Run("显式默认端口回落到无端口条目", func(t *testing.T) {
		bare := &dlJob{referer: "https://page.example/", segRefs: map[string]string{"cdn.example": ""}}
		if got := bare.segmentReferer("https://cdn.example:443/seg-1.ts"); got != "" {
			t.Fatalf(":443 与无端口同 origin => %q, want 空串", got)
		}
		if got := bare.segmentReferer("http://cdn.example:80/seg-1.ts"); got != "" {
			t.Fatalf(":80 与无端口同 origin => %q, want 空串", got)
		}
		// 反向也要守：非默认端口是另一个 host，不得套用无端口条目。
		if got := bare.segmentReferer("https://cdn.example:8443/seg-1.ts"); got != "https://page.example/" {
			t.Fatalf(":8443 是独立 host，不该套用无端口条目 => %q, want 页面 Referer", got)
		}
		// IPv6 去端口后要保留方括号（浏览器 new URL().host 就是 "[::1]"）。
		ipv6 := &dlJob{referer: "https://page.example/", segRefs: map[string]string{"[::1]": "https://parser.example/"}}
		if got := ipv6.segmentReferer("https://[::1]:443/seg-1.ts"); got != "https://parser.example/" {
			t.Fatalf("IPv6 去端口应保留方括号 => %q, want 解析站 Referer", got)
		}
	})
}

// segReqRecord 记录源站实际收到的分片请求（present 区分「头不存在」与「头存在但空」）。
type segReqRecord struct {
	path    string
	referer string
	present bool
}

// newRefererProbeCDN 起一个仿「白名单型防盗链」的源站：路径以 /noref 开头时 Referer
// 非空即 403（真机观察：分片带上页面 Referer 必被拒），以 /ref 开头只认解析站域名，
// 其余路径只认页面域名。每次请求实际收到的 Referer 都会被记下，且区分「头不存在」
// 与「头存在但为空」。
func newRefererProbeCDN(t *testing.T) (*httptest.Server, func() []segReqRecord) {
	t.Helper()
	var mu sync.Mutex
	var seen []segReqRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vals, present := r.Header["Referer"]
		got := ""
		if len(vals) > 0 {
			got = vals[0]
		}
		mu.Lock()
		seen = append(seen, segReqRecord{path: r.URL.Path, referer: got, present: present})
		mu.Unlock()

		switch {
		case strings.HasPrefix(r.URL.Path, "/noref"):
			if got != "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// .m3u8 路径回播放列表内容（供 playlist 链路用）：master 里带
			// EXT-X-STREAM-INF 会触发 master → 子列表的第二跳。
			switch {
			case strings.HasSuffix(r.URL.Path, "/master.m3u8"):
				w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nsub.m3u8\n"))
			case strings.HasSuffix(r.URL.Path, ".m3u8"):
				w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\nseg-1.ts\n#EXT-X-ENDLIST\n"))
			default:
				w.Write([]byte("noref-ok"))
			}
		case strings.HasPrefix(r.URL.Path, "/ref"):
			if got != "https://parser.example/" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Write([]byte("ref-ok"))
		default:
			if got != "https://page.example/watch/1" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Write([]byte("pageref-ok"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []segReqRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]segReqRecord(nil), seen...)
	}
}

// TestSegmentRefsChainOnWire 把 /download 的 segrefs 参数走完整条链
// （query JSON → parseSegRefs → segmentReferer → 真正发出去的请求头）并断言线序结果。
//
// 三个子用例分别守三个方向，任一处被改回去就会红：
//  1. 空值条目 —— 分片请求**不得**出现 Referer 头（扰动：parseSegRefs / segmentReferer /
//     net.go 的 `if ref != ""` 三处任改一处）；
//  2. 非空条目 —— 必须原样发出解析站 Referer（扰动：让 segmentReferer 恒返回 ""）；
//  3. 未命中条目 —— 仍回退页面 Referer（扰动：把 `if !ok { return j.referer }` 改成恒返回 ""）。
func TestSegmentRefsChainOnWire(t *testing.T) {
	srv, seen := newRefererProbeCDN(t)
	saveRestoreHTTP(t, srv.Client(), 1)
	host := strings.TrimPrefix(srv.URL, "http://")

	jobWith := func(raw string) *dlJob {
		segRefs, err := parseSegRefs(raw)
		if err != nil {
			t.Fatalf("parseSegRefs(%q): %v", raw, err)
		}
		return &dlJob{rt: testStd, referer: "https://page.example/watch/1", segRefs: segRefs}
	}
	fetch := func(j *dlJob, path string) (string, error) {
		body, err := fetchSegment(context.Background(), j, srv.URL+path)
		return string(body), err
	}

	t.Run("空值条目：分片请求不带 Referer 头", func(t *testing.T) {
		body, err := fetch(jobWith(`{"`+host+`":""}`), "/noref/seg-1.png")
		if err != nil {
			t.Fatalf("白名单型 CDN 收到页面 Referer 会 403，这里应成功: %v", err)
		}
		if body != "noref-ok" {
			t.Fatalf("body=%q", body)
		}
		got := seen()
		if len(got) == 0 {
			t.Fatal("分片请求没到达源站")
		}
		// 断言「头不存在」而非「非空」：空值头在真机上也能过，但浏览器发的是
		// 整头缺失，实现必须与浏览器一致 —— 这条比源站判据更严，正是要钉的点。
		if last := got[len(got)-1]; last.present {
			t.Fatalf("分片请求仍带 Referer 头（值=%q）：空值条目没能抑制页面 Referer", last.referer)
		}
	})

	t.Run("非空条目：原样发出解析站 Referer", func(t *testing.T) {
		body, err := fetch(jobWith(`{"`+host+`":"https://parser.example/"}`), "/ref/seg-2.ts")
		if err != nil {
			t.Fatalf("解析站 Referer 应被采用: %v", err)
		}
		if body != "ref-ok" {
			t.Fatalf("body=%q", body)
		}
		if last := seen()[len(seen())-1]; last.referer != "https://parser.example/" {
			t.Fatalf("线上 Referer=%q want 解析站域名", last.referer)
		}
	})

	t.Run("未命中条目：回退页面 Referer", func(t *testing.T) {
		body, err := fetch(jobWith(`{"other-cdn.example":"https://parser.example/"}`), "/pageref/seg-3.ts")
		if err != nil {
			t.Fatalf("未命中应回退页面 Referer: %v", err)
		}
		if body != "pageref-ok" {
			t.Fatalf("body=%q", body)
		}
		if last := seen()[len(seen())-1]; last.referer != "https://page.example/watch/1" {
			t.Fatalf("线上 Referer=%q want 页面域名", last.referer)
		}
	})
}

// TestPlaylistUsesSegmentRefererOnWire 守「播放列表也按 host 选用 Referer」：
// 有的站把 m3u8 / 子播放列表也放在白名单型 CDN 上，一律用页面 Referer 会被 403。
// 这条链路不经过 fetchSegment（而是 httpGetPlaylist / httpGetWithRetry），
// 只在分片路径上钉断言是发现不了的。
//
// master → 子列表两跳都要覆盖（fetchPlaylist 里的第二次请求就是第二跳）。
//
// 扰动（改回去必红）：把 m3u8.go 里两处 `j.segmentReferer(...)` 改回 `j.referer`。
func TestPlaylistUsesSegmentRefererOnWire(t *testing.T) {
	srv, seen := newRefererProbeCDN(t)
	saveRestoreHTTP(t, srv.Client(), 1)
	host := strings.TrimPrefix(srv.URL, "http://")

	segRefs, err := parseSegRefs(`{"` + host + `":""}`)
	if err != nil {
		t.Fatalf("parseSegRefs: %v", err)
	}
	j := &dlJob{
		rt:      testStd,
		m3u8URL: srv.URL + "/noref/master.m3u8",
		referer: "https://page.example/watch/1",
		segRefs: segRefs,
	}

	content, base, isDirect, err := j.fetchPlaylist(context.Background())
	if err != nil {
		t.Fatalf("白名单型 CDN 收到页面 Referer 会 403：playlist 两跳都应成功，得到 %v", err)
	}
	if isDirect {
		t.Fatal("m3u8 内容不该被判成直链媒体")
	}
	if !strings.Contains(content, "seg-1.ts") {
		t.Fatalf("应拿到子播放列表内容，got %q", content)
	}
	if want := srv.URL + "/noref/sub.m3u8"; base != want {
		t.Fatalf("base=%q want %q", base, want)
	}

	// 两跳都必须真到达源站，且都**不得**出现 Referer 头 —— 「头不存在」比
	// 「头为空」更严，与浏览器 no-referrer 时的行为一致。
	var listHits int
	for _, rec := range seen() {
		if !strings.HasSuffix(rec.path, ".m3u8") {
			continue
		}
		listHits++
		if rec.present {
			t.Fatalf("playlist 请求 %s 仍带 Referer 头（值=%q）", rec.path, rec.referer)
		}
	}
	if listHits != 2 {
		t.Fatalf("master 与 sub 两跳都应到达源站，实际 %d 次", listHits)
	}
}

// assertNoReferer 断言 from 之后的每个请求都**没有** Referer 头 —— 是「整头缺失」，
// 不是「头存在但值为空」。
func assertNoReferer(t *testing.T, seen func() []segReqRecord, from int, what string) {
	t.Helper()
	recs := seen()
	if len(recs) <= from {
		t.Fatalf("%s 的请求没到达源站（修复后应能发出）", what)
	}
	for _, rec := range recs[from:] {
		if rec.present {
			t.Fatalf("%s 的请求 %s 仍带 Referer 头（值=%q）", what, rec.path, rec.referer)
		}
	}
}

// TestDirectLinkRequestsUseSegmentReferer 覆盖直链模式的三个请求点
// （probeRange / downloadRange / downloadDirect 的单连接续传）：它们和 playlist、
// 分片一样经 segmentReferer 按 host 选用 Referer —— 直链同样可能落在白名单型
// CDN 上，一律用页面 Referer 就 403。
//
// 断言看的是**源站收到的请求头**，不看返回值：源站不支持 Range，这几个调用本来
// 就返回失败/零值，返回值对区分缺陷没有信息量（和 §「只看返回值发现不了吞信号」
// 是同一个道理）。
//
// 扰动（改回去必红）：把 download.go 里这三处 `j.segmentReferer(j.m3u8URL)`
// 改回 `j.referer`。
func TestDirectLinkRequestsUseSegmentReferer(t *testing.T) {
	srv, seen := newRefererProbeCDN(t)
	saveRestoreHTTP(t, srv.Client(), 1)
	host := strings.TrimPrefix(srv.URL, "http://")

	segRefs, err := parseSegRefs(`{"` + host + `":""}`)
	if err != nil {
		t.Fatalf("parseSegRefs: %v", err)
	}
	newJob := func() *dlJob {
		return &dlJob{
			rt:      testStd,
			m3u8URL: srv.URL + "/noref/direct.bin",
			referer: "https://page.example/watch/1",
			segRefs: segRefs,
		}
	}
	// 带超时：万一源站行为与预期不符，宁可让调用方尽快返回，也别把测试挂住。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("probeRange", func(t *testing.T) {
		before := len(seen())
		if _, ok := newJob().probeRange(ctx); ok {
			t.Fatal("源站不支持 Range，不该探测成功")
		}
		assertNoReferer(t, seen, before, "probeRange")
	})

	t.Run("downloadRange", func(t *testing.T) {
		f, ferr := os.CreateTemp(t.TempDir(), "seg-*.part")
		if ferr != nil {
			t.Fatalf("CreateTemp: %v", ferr)
		}
		defer f.Close()
		before := len(seen())
		_ = newJob().downloadRange(ctx, f, 0, 15) // 源站回 200 而非 206，必然失败
		assertNoReferer(t, seen, before, "downloadRange")
	})

	t.Run("downloadDirect 单连接", func(t *testing.T) {
		before := len(seen())
		_ = newJob().downloadDirect(ctx, filepath.Join(t.TempDir(), "out.bin"))
		assertNoReferer(t, seen, before, "downloadDirect")
	})
}
