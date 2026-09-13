// 回归测试：加密声明解析失败不得静默降级为明文管线（2026-09-13 评审 P1-1）。
//
// 缺陷形态：keyURIRe 只认带引号的 URI（`URI="k.ts"`）。播放列表写
// `METHOD=AES-128,URI=k.ts`（非规范裸值）时 parseKeyLine 返回 nil，于是
// pl.key=nil —— 与「明文流」在后续所有判断里完全无法区分：validatePlaylist
// 放行、不装解密器、ProbeInfo.Encrypted=false 让 generic 守卫也不设防。
// 结果是密文被当明文拼进成品、全链路日志正常 —— 本项目自认的头号缺陷形态。
package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestParseKeyLineMalformedURI 声明了加密 METHOD 但 URI 缺失/为空 → 必须报畸形
// （返回 (nil, true)），而不是与「无效 KEY 行 / METHOD=NONE」共用 nil 这一个出口。
func TestParseKeyLineMalformedURI(t *testing.T) {
	base := "https://cdn.example.com/v/pl.m3u8"
	malformed := []struct{ name, line string }{
		{"URI 属性整个缺失", `#EXT-X-KEY:METHOD=AES-128`},
		{"URI 为空引号值", `#EXT-X-KEY:METHOD=AES-128,URI=""`},
		{"URI 裸值但为空（逗号紧跟）", `#EXT-X-KEY:METHOD=AES-128,URI=,IV=0x00000000000000000000000000000001`},
	}
	for _, c := range malformed {
		k, bad := parseKeyLine(c.line, base)
		if k != nil || !bad {
			t.Errorf("%s: 应报畸形（key=nil, malformed=true），得到 key=%v malformed=%v", c.name, k, bad)
		}
	}

	// 这两种都不是畸形：前者显式明文，后者根本不是一条 KEY 声明
	for _, line := range []string{
		`#EXT-X-KEY:METHOD=NONE`,
		`#EXT-X-KEY:IV=0x00000000000000000000000000000001`,
	} {
		k, bad := parseKeyLine(line, base)
		if k != nil || bad {
			t.Errorf("%s: 不应报畸形，得到 key=%v malformed=%v", line, k, bad)
		}
	}
}

// TestParseKeyLineBareURI 非规范裸值 URI 现在也认（与 KEYFORMAT 的宽容度对齐）：
// URI 宽容之后，就不需要「解析失败 → 当明文」这条降级路径了。
func TestParseKeyLineBareURI(t *testing.T) {
	base := "https://cdn.example.com/v/pl.m3u8"
	k, bad := parseKeyLine(
		`#EXT-X-KEY:METHOD=AES-128,URI=k.ts,IV=0x00000000000000000000000000000001`, base)
	if bad {
		t.Fatal("裸值 URI 不应被判为畸形")
	}
	if k == nil {
		t.Fatal("裸值 URI 应被解析出 KeyInfo")
	}
	if k.Method != "AES-128" || k.URI != "https://cdn.example.com/v/k.ts" {
		t.Errorf("解析结果=%+v want AES-128 / https://cdn.example.com/v/k.ts", *k)
	}
	if len(k.IV) != 16 {
		t.Errorf("IV 应解析出 16 字节，得到 %d", len(k.IV))
	}
}

// TestValidatePlaylistRejectsMalformedKey 端到端回归：整条流不能因为一条畸形
// 加密声明而落进明文管线；同时确认裸值 URI 的合法加密流不被误伤。
func TestValidatePlaylistRejectsMalformedKey(t *testing.T) {
	pl := parsePlaylist(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-KEY:METHOD=AES-128
#EXTINF:6.0,
seg0.ts
#EXTINF:6.0,
seg1.ts
#EXT-X-ENDLIST
`, "https://cdn.example.com/v/pl.m3u8")
	if pl.key != nil {
		t.Fatalf("畸形 KEY 不应产出 KeyInfo，得到 %+v", pl.key)
	}
	if !pl.keyMalformed {
		t.Fatal("应标记 keyMalformed")
	}
	if err := validatePlaylist(pl); err == nil {
		t.Fatal("畸形加密声明必须被拒绝（否则密文会被当明文拼进成品）")
	}

	bare := parsePlaylist(`#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI=k.ts
#EXTINF:6.0,
seg0.ts
#EXT-X-ENDLIST
`, "https://cdn.example.com/v/pl.m3u8")
	if err := validatePlaylist(bare); err != nil {
		t.Errorf("裸值 URI 的合法加密流不应被拒: %v", err)
	}
}

// TestLiveFollowRejectsMalformedKey 直播路径同样要过完整语义校验：只在点播
// 路径调 validatePlaylist，会让直播成为绕过口（畸形声明被当明文一路跟录）。
func TestLiveFollowRejectsMalformedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-KEY:METHOD=AES-128\n#EXTINF:6.0,\nseg/0.ts\n")
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "live.ts")
	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/live.m3u8", live: true, seen: make(map[string]bool)}
	if _, err := j.liveDownload(context.Background(), out, 0); err == nil {
		t.Fatal("直播遇到畸形加密声明必须报错，不能当明文继续录")
	}
}
