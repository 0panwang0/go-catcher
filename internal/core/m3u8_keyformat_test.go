// #EXT-X-KEY 的 KEYFORMAT 校验回归。
//
// 缺陷形态：parseKeyLine 只读 METHOD/URI/IV，KEYFORMAT 被整个忽略。DRM 流的
// METHOD 往往仍写着 AES-128，于是一路放行，把"密钥系统响应体"当 16 字节裸密钥
// 用 —— 产物是能播但花屏/无声的文件，日志一切正常。这是最隐蔽的一类损坏，
// 必须显式拒绝。
//
// 用例里的 KEYFORMAT / URI 均为中性构造值，不代表任何具体 DRM 产品。
package core

import (
	"strings"
	"testing"
)

// TestParseKeyLineReadsKeyFormat KEYFORMAT 的三种写法（带引号 / 裸值 / 缺省）。
func TestParseKeyLineReadsKeyFormat(t *testing.T) {
	base := "https://cdn.example.com/v/pl.m3u8"
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			"带引号（规范写法）",
			`#EXT-X-KEY:METHOD=AES-128,URI="k.bin",KEYFORMAT="com.example.drm"`,
			"com.example.drm",
		},
		{
			"裸值（非规范播放列表）",
			`#EXT-X-KEY:METHOD=AES-128,URI="k.bin",KEYFORMAT=identity,KEYFORMATVERSIONS="1"`,
			"identity",
		},
		{
			"缺省即 identity（空串表示）",
			`#EXT-X-KEY:METHOD=AES-128,URI="k.bin"`,
			"",
		},
		{
			"商业 DRM 的 urn 形态",
			`#EXT-X-KEY:METHOD=AES-128,URI="data:text/plain;base64,AAAA",KEYFORMAT="urn:uuid:11111111-2222-4333-8444-555555555555"`,
			"urn:uuid:11111111-2222-4333-8444-555555555555",
		},
	}
	for _, c := range cases {
		k, _ := parseKeyLine(c.line, base)
		if k == nil {
			t.Fatalf("%s: 应解析出 KeyInfo，得到 nil（line=%s）", c.name, c.line)
		}
		if k.KeyFormat != c.want {
			t.Errorf("%s: KeyFormat=%q want %q", c.name, k.KeyFormat, c.want)
		}
	}
}

// TestEnsureIdentityKeyFormat identity 的等价写法都要放行，别把正常流误杀。
func TestEnsureIdentityKeyFormat(t *testing.T) {
	ok := []string{"", "identity", "IDENTITY", " identity "}
	for _, f := range ok {
		if !isIdentityKeyFormat(f) {
			t.Errorf("KEYFORMAT=%q 应视为 identity", f)
		}
	}
	bad := []string{
		"com.example.drm",
		"urn:uuid:11111111-2222-4333-8444-555555555555",
		"com.example.drm.alt",
	}
	for _, f := range bad {
		if isIdentityKeyFormat(f) {
			t.Errorf("KEYFORMAT=%q 不应视为 identity", f)
		}
	}
}

// TestValidatePlaylistRejectsNonIdentityKeyFormat 端到端：解析 → 校验必须失败，
// 且报错里带上 KEYFORMAT 值（用户要知道是哪种 DRM 拦下的）。
func TestValidatePlaylistRejectsNonIdentityKeyFormat(t *testing.T) {
	const pl = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-KEY:METHOD=AES-128,URI="https://drm.example.com/license",KEYFORMAT="com.example.drm",KEYFORMATVERSIONS="1"
#EXTINF:6.0,
seg0.ts
#EXTINF:6.0,
seg1.ts
#EXT-X-ENDLIST
`
	got := parsePlaylist(pl, "https://cdn.example.com/v/pl.m3u8")
	if len(got.segments) != 2 {
		t.Fatalf("分片解析数量不对: %d", len(got.segments))
	}
	err := validatePlaylist(got)
	if err == nil {
		t.Fatal("非 identity 的 KEYFORMAT 必须被拒绝（否则会把 license 响应当密钥用）")
	}
	if !strings.Contains(err.Error(), "com.example.drm") {
		t.Errorf("报错应带上 KEYFORMAT 值，得到: %v", err)
	}
}

// TestValidatePlaylistAcceptsIdentityAndPlain 明文流与 identity 加密流都要放行。
func TestValidatePlaylistAcceptsIdentityAndPlain(t *testing.T) {
	plain := parsePlaylist(`#EXTM3U
#EXTINF:6.0,
seg0.ts
#EXT-X-ENDLIST
`, "https://cdn.example.com/v/pl.m3u8")
	if err := validatePlaylist(plain); err != nil {
		t.Errorf("明文流不应被拒: %v", err)
	}

	identity := parsePlaylist(`#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="k.bin",KEYFORMAT="identity",KEYFORMATVERSIONS="1"
#EXTINF:6.0,
seg0.ts
#EXT-X-ENDLIST
`, "https://cdn.example.com/v/pl.m3u8")
	if err := validatePlaylist(identity); err != nil {
		t.Errorf("identity 加密流不应被拒: %v", err)
	}
}

// TestKeyFormatDoesNotTriggerFalseRotation 重复声明同一条 key 是合法写法，
// 且「裸写」与「显式 identity」是同一件事——不能因为写法不同就判成 key rotation
// （那会把正常流拒掉，比漏判更糟：误伤）。
func TestKeyFormatDoesNotTriggerFalseRotation(t *testing.T) {
	bare, _ := parseKeyLine(`#EXT-X-KEY:METHOD=AES-128,URI="k.bin"`, "https://cdn.example.com/v/pl.m3u8")
	explicit, _ := parseKeyLine(`#EXT-X-KEY:METHOD=AES-128,URI="k.bin",KEYFORMAT="identity"`, "https://cdn.example.com/v/pl.m3u8")
	if !sameKey(bare, explicit) {
		t.Error("缺省 KEYFORMAT 与显式 identity 应视为同一条 key")
	}
	if bare.fingerprint() != explicit.fingerprint() {
		t.Errorf("指纹应一致（否则会无谓重建解密器）: %q vs %q",
			bare.fingerprint(), explicit.fingerprint())
	}

	// 真的换了 KEYFORMAT 才算不同
	alt, _ := parseKeyLine(`#EXT-X-KEY:METHOD=AES-128,URI="k.bin",KEYFORMAT="com.example.drm.alt"`, "https://cdn.example.com/v/pl.m3u8")
	if sameKey(bare, alt) {
		t.Error("KEYFORMAT 不同不应视为同一条 key")
	}

	// 首条裸写、后续重复时写 identity：不能判成 rotation
	pl := `#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="k.bin"
#EXTINF:6.0,
seg0.ts
#EXT-X-KEY:METHOD=AES-128,URI="k.bin",KEYFORMAT="identity"
#EXTINF:6.0,
seg1.ts
#EXT-X-ENDLIST
`
	if err := validatePlaylist(parsePlaylist(pl, "https://cdn.example.com/v/pl.m3u8")); err != nil {
		t.Errorf("同一把 key 的两种等价写法不应判成 key rotation: %v", err)
	}
}
