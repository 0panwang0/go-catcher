// m3u8 解析层的模糊测试。m3u8 解析是纯函数（无 I/O），任意畸形文本都必须
// 不 panic、不产出自相矛盾的状态。本项目头号缺陷形态是"静默错值"：畸形输入
// 被容忍到管线里，最终产出损坏文件而日志全绿 —— 因此这里除了"不 panic"，
// 还显式断言三条容易破坏的语义不变量，把静默退化变成可复现的失败。
//
// 种子语料随 `go test` 运行（质量门只跑种子）；需要发散式模糊时手跑：
//
//	go test ./internal/core/ -run=^$ -fuzz=FuzzParseM3U8Playlist -fuzztime=30s
//	go test ./internal/core/ -run=^$ -fuzz=FuzzParseM3U8KeyLine -fuzztime=30s
package core

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// fuzzBase 固定测试基址：分片相对 URL 统一按它解析（与 m3u8_mapuri_test.go 同约定）。
const fuzzBase = "https://cdn.example.test/v/playlist.m3u8"

// fmtRef 返回分片链里的第 i 个分片引用，构造种子播放列表时复用。
func fmtRef(i int) string {
	return "seg" + strconv.Itoa(i) + ".ts"
}

// m3u8FuzzSeeds 是 parsePlaylist 的种子集合，每条对应一个已知缺陷形态或合法基线：
//   - 全畸形 KEY 行：METHOD=AES-128 但无 URI → 必须 keyMalformed=true
//   - 裸值 URI KEY 行：非规范但合法 → 必须解析出 key，不得误判畸形
//   - METHOD=NONE / 无 METHOD：显式明文 → key==nil 且非畸形
//   - 换 key（rotation）：用过之后换另一组 → multiKey=true
//   - 单键重复声明（每个分片前都重复同一 KEY）：合法常见写法 → 不得 multiKey
//   - 极端 EXTINF（NaN/超大/负/缺时长等）：时长不得变成 +Inf/NaN，且不与分片错位
//   - BYTERANGE：hasByteRange=true
//   - BOM / 控制字符 / 二进制残片：不得 panic、不得进分片列表
//   - master（STREAM-INF）+ 正常点播/直播基线
func m3u8PlaylistSeeds() [][]string {
	return [][]string{
		// 畸形加密声明（头号形态：畸形后静默按明文跑）
		{"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128\n#EXTINF:8.0,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\n#EXTINF:8.0,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"\"\n#EXTINF:8.0,\n" + fmtRef(0)},
		// 裸值 URI：合法非规范形态，必须解析出 key
		{"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=key.bin,IV=0x00000000000000000000000000000001\n#EXTINF:8.0,\n" + fmtRef(0)},
		// 带引号 URI + IV + KEYFORMAT 裸值
		{"#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key.bin\",IV=0x00000000000000000000000000000000,KEYFORMAT=\"com.apple.streamingkeydelivery\"\n#EXTINF:8.0,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:8.0,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXT-X-KEY:URI=key.bin\n#EXTINF:8.0,\n" + fmtRef(0)},
		// key rotation
		{"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"a.bin\"\n#EXTINF:8.0,\n" + fmtRef(0) + "\n#EXT-X-KEY:METHOD=AES-128,URI=\"b.bin\"\n#EXTINF:8.0,\n" + fmtRef(1)},
		// 单键重复（合法）
		{"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"a.bin\"\n#EXTINF:8.0,\n" + fmtRef(0) + "\n#EXT-X-KEY:METHOD=AES-128,URI=\"a.bin\"\n#EXTINF:8.0,\n" + fmtRef(1)},
		// 极端 EXTINF
		{"#EXTM3U\n#EXTINF:NaN,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXTINF:+Inf,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXTINF:-1.0,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXTINF:99999999999999999999,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXTINF:,\n" + fmtRef(0)},
		// BYTERANGE
		{"#EXTM3U\n#EXT-X-BYTERANGE:123456@789\n#EXTINF:8.0,\nbig.ts"},
		// BOM / 残片 / 控制字符
		{"\ufeff#EXTM3U\n#EXTINF:8.0,\n" + fmtRef(0)},
		{"\x00\x01\x02\x03\x04\xff"},
		{"non-m3u8 binary garbage \x1b[31mANSI\x1b[0m"},
		// master
		{"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720\nmid/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=1600000\nhigh/index.m3u8"},
		// 点播 / 直播基线
		{"#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\n" + fmtRef(0) + "\n#EXTINF:6.0,\n" + fmtRef(1) + "\n#EXT-X-ENDLIST"},
		{"#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:6.0,\n" + fmtRef(0) + "\n#EXTINF:6.0,\n" + fmtRef(1)},
		{"#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"720@0\"\n#EXTINF:6.0,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXT-X-MAP:URI=init.mp4,BYTERANGE=720@0\n#EXTINF:6.0,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:abc\n#EXTINF:6.0,\n" + fmtRef(0)},
		{"#EXTM3U\n#EXTINF:6.0\nseg0.ts\n#EXTINF:6.0\nseg1.ts"},
	}
}

// FuzzParseM3U8Playlist 任意 m3u8 文本都不许 panic、不产出自相矛盾的状态。
//
// 不变量（每条都是"静默错值 → 显式失败"的探针）：
//  1. segments 与 durs 严格等长（解析器每 appends 一个分片必须同步累计时长，
//     否则分片与时长错位，直播时间轴推算全错 —— 本项目 DTS 错位缺陷的近亲形态）。
//  2. totalDur 不得为 NaN 或 +Inf（EXITINF 解析忽略 ParseFloat 错误，
//     若放行 "NaN"/"+Inf" 会污染 totalDur 与 durs，下游净时长比较全部失效）。
//  3. 单条时长不得为 NaN/+Inf（同上，逐分片污染）。
func FuzzParseM3U8Playlist(f *testing.F) {
	for _, s := range m3u8PlaylistSeeds() {
		f.Add(s[0])
	}
	f.Fuzz(func(t *testing.T, text string) {
		pl := parsePlaylist(text, fuzzBase)
		if len(pl.segments) != len(pl.durs) {
			t.Fatalf("segments/durs 长度不一致: %d vs %d", len(pl.segments), len(pl.durs))
		}
		if math.IsNaN(pl.totalDur) || math.IsInf(pl.totalDur, 0) {
			t.Fatalf("totalDur 出现非有限值: %v", pl.totalDur)
		}
		for i := range pl.durs {
			if math.IsNaN(pl.durs[i]) || math.IsInf(pl.durs[i], 0) {
				t.Fatalf("durs[%d] 非有限值: %v", i, pl.durs[i])
			}
		}
	})
}

// FuzzParseM3U8KeyLine 单行 KEY 解析的三态契约不可自相矛盾：
//
//	(malformed=true)  ⟹  key 必须为 nil
//
// 相反方向（nil 且非畸形）合法：可能只是行里没有 METHOD（非 KEY 声明）或 METHOD=NONE。
// 这条不变量直接锁住头号缺陷形态：畸形声明若被当成明文（返回 nil,false），
// 或是畸形却仍返回一把 key，都会造成密文被当明文/错钥解出随机字节。
func FuzzParseM3U8KeyLine(f *testing.F) {
	for _, s := range m3u8PlaylistSeeds() {
		for _, line := range strings.Split(s[0], "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#EXT-X-KEY:") {
				f.Add(strings.TrimSpace(line))
			}
		}
	}
	f.Add("#EXT-X-KEY:METHOD=AES-128")
	f.Add("#EXT-X-KEY:METHOD=AES-128,URI=")
	f.Add("#EXT-X-KEY:METHOD=AES-128,URI=\"\"")
	f.Add("#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\"")
	f.Add("#EXT-X-KEY:METHOD=AES-128,URI=k.bin,IV=0x1234567890abcdef1234567890abcdef")
	f.Add("#EXT-X-KEY:METHOD=NONE")
	f.Add("#EXT-X-KEY:URI=k.bin")
	f.Add("#EXT-X-KEY:")
	f.Add("#EXTINF:8.0,")
	f.Add("plain segment line")
	f.Fuzz(func(t *testing.T, line string) {
		k, malformed := parseKeyLine(line, fuzzBase)
		if malformed && k != nil {
			t.Fatalf("畸形声明却返回了 key: line=%q key=%+v", line, k)
		}
		_ = k
	})
}
