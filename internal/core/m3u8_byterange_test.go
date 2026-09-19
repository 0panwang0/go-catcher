// #EXT-X-BYTERANGE 回归（2026-09-11 评审 P1-4）。
//
// 单个文件按字节区间切片的 HLS（多行分片指向同一个 URL，只靠 BYTERANGE 区分）
// 在"一行分片 = 一个完整 URL"的语义下会解析出 N 个相同 URL，于是把整个文件下
// N 遍再顺序拼接：体积放大 N 倍、时间轴错位，而每份内容都是合法 TS，
// validateOutput 发现不了。当前版本不支持，必须显式拒绝。
package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePlaylistDetectsByteRange(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:6\n" +
		"#EXTINF:6.0,\n#EXT-X-BYTERANGE:1024@0\nseg.ts\n" +
		"#EXTINF:6.0,\n#EXT-X-BYTERANGE:1024@1024\nseg.ts\n" +
		"#EXT-X-ENDLIST\n"
	pl := parsePlaylist(body, "https://cdn.example.com/v/")
	if !pl.hasByteRange {
		t.Fatal("应识别出 #EXT-X-BYTERANGE")
	}
	// 不校验的话，两行会解析成同一个 URL（这正是错解的根源）
	if len(pl.segments) != 2 || pl.segments[0] != pl.segments[1] {
		t.Fatalf("前提不成立：期望两行解析出同一个 URL，得到 %v", pl.segments)
	}
	if err := ensureNoByteRange(pl); err == nil {
		t.Fatal("字节范围分片必须显式拒绝，不能静默按错误语义处理")
	}

	// 对照：普通播放列表不能被误伤
	plain := parsePlaylist("#EXTM3U\n#EXTINF:6.0,\na.ts\n#EXTINF:6.0,\nb.ts\n#EXT-X-ENDLIST\n",
		"https://cdn.example.com/v/")
	if plain.hasByteRange {
		t.Fatal("普通播放列表被误判为字节范围分片")
	}
	if err := ensureNoByteRange(plain); err != nil {
		t.Fatalf("普通播放列表不应报错: %v", err)
	}
}

// TestRunCLIRejectsByteRangePlaylist 确认这道守卫真的接进了下载路径，
// 而不是只在解析函数里留了个没人读的标记。
func TestRunCLIRejectsByteRangePlaylist(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/br.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:6\n"+
			"#EXTINF:6.0,\n#EXT-X-BYTERANGE:376@0\nseg.ts\n"+
			"#EXTINF:6.0,\n#EXT-X-BYTERANGE:376@376\nseg.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/seg.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(mkTSSegment(2)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "br.ts")
	if code := RunCLI(CLIOptions{URL: srv.URL + "/br.m3u8", Proxy: "direct", Output: out, Concurrency: 2}); code == 0 {
		t.Fatal("含 #EXT-X-BYTERANGE 的流应退出码非 0")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("拒绝时不应产出文件，实际: %v", err)
	}
}
