// 回归测试：非规范裸值 #EXT-X-MAP:URI=init.mp4 被误拒（2026-09-14 评审 P2-3）。
//
// 缺陷形态：mapURIRe 只认带引号的 URI（`URI="init.mp4"`）。播放列表写裸值时
// hasMap/mapURI 一律取不到 ⇒ detectContainer 判不出 fMP4（hasMap=false 且首片是
// moof 而非 ftyp），任务以「识别为 fMP4 分片流，但播放列表没有 #EXT-X-MAP
// 初始化段」失败——**明明有 init 段却报"没有"**，用户无从下手。
//
// 同一份宽容度在 #EXT-X-KEY 上早就有了（keyURIRe / keyFormatRe 都认裸值），
// 只有 #EXT-X-MAP 漏了：三处属性解析必须对齐，否则就是"同类输入两条路径两种结果"。
package core

import "testing"

const mapTestBase = "https://cdn.example.com/v/pl.m3u8"

// TestParseMapURIBareValue 带引号与裸值两种形态必须解析出同一个 mapURI，
// 属性名大小写不敏感；值本身为空时仍然不算"有 init 段"（否则会去 fetch 空 URL）。
func TestParseMapURIBareValue(t *testing.T) {
	want := "https://cdn.example.com/v/init.mp4"
	ok := []struct{ name, line string }{
		{"带引号（规范）", `#EXT-X-MAP:URI="init.mp4"`},
		{"裸值（非规范）", `#EXT-X-MAP:URI=init.mp4`},
		{"裸值后跟其它属性", `#EXT-X-MAP:URI=init.mp4,BYTERANGE=1234@0`},
		{"裸值后跟空白再跟属性", `#EXT-X-MAP:URI=init.mp4 , BYTERANGE=1234@0`},
		{"属性名小写", `#EXT-X-MAP:uri=init.mp4`},
		{"带引号后跟其它属性", `#EXT-X-MAP:URI="init.mp4",BYTERANGE=1234@0`},
	}
	for _, c := range ok {
		pl := parsePlaylist("#EXTM3U\n"+c.line+"\n#EXTINF:6.0,\nseg0.m4s\n", mapTestBase)
		if !pl.hasMap || pl.mapURI != want {
			t.Errorf("%s: hasMap=%v mapURI=%q，want true / %q（裸值应与带引号解析出同一结果）",
				c.name, pl.hasMap, pl.mapURI, want)
		}
	}

	for _, line := range []string{
		`#EXT-X-MAP:URI=""`,
		`#EXT-X-MAP:URI=`,
		`#EXT-X-MAP:URI=,BYTERANGE=1234@0`,
		`#EXT-X-MAP:BYTERANGE=1234@0`,
	} {
		pl := parsePlaylist("#EXTM3U\n"+line+"\n#EXTINF:6.0,\nseg0.m4s\n", mapTestBase)
		if pl.hasMap || pl.mapURI != "" {
			t.Errorf("%s: 空值不应判为有 init 段，得到 hasMap=%v mapURI=%q",
				line, pl.hasMap, pl.mapURI)
		}
	}
}

// TestParseMapURIResolvesAgainstPlaylist 裸值同样要按播放列表 URL 解析相对路径，
// 否则会去请求进程工作目录下的 init.mp4（本地文件路径，必然失败）。
func TestParseMapURIResolvesAgainstPlaylist(t *testing.T) {
	pl := parsePlaylist(
		"#EXTM3U\n#EXT-X-MAP:URI=sub/init.mp4\n#EXTINF:6.0,\nseg0.m4s\n",
		"https://cdn.example.com/v/a/pl.m3u8")
	if got, want := pl.mapURI, "https://cdn.example.com/v/a/sub/init.mp4"; got != want {
		t.Fatalf("mapURI=%q want %q", got, want)
	}
}
