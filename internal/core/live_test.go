// 直播跟随与容器注册表测试。
package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParsePlaylist 验证分片/init 段/ENDLIST/时长解析与相对路径解析。
func TestParsePlaylist(t *testing.T) {
	base := "https://cdn.example.com/v/a/index.m3u8"
	m3u := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:8\n#EXT-X-MAP:URI=\"init.mp4\"\n" +
		"#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.5,\nseg/1.ts\n#EXT-X-ENDLIST\n"
	pl := parsePlaylist(m3u, base)
	if len(pl.segments) != 2 {
		t.Fatalf("segments=%d want 2", len(pl.segments))
	}
	if pl.segments[0] != "https://cdn.example.com/v/a/seg/0.ts" {
		t.Fatalf("seg0=%q 相对路径解析错误", pl.segments[0])
	}
	if !pl.hasMap || pl.mapURI != "https://cdn.example.com/v/a/init.mp4" {
		t.Fatalf("map: hasMap=%v mapURI=%q", pl.hasMap, pl.mapURI)
	}
	if !pl.hasEndList {
		t.Fatal("应识别为点播（含 ENDLIST）")
	}
	if pl.totalDur != 12.5 {
		t.Fatalf("totalDur=%v want 12.5", pl.totalDur)
	}

	// 无 ENDLIST = 直播/事件流
	pl2 := parsePlaylist("#EXTM3U\n#EXTINF:4.0,\na.ts\n", base)
	if pl2.hasEndList {
		t.Fatal("无 ENDLIST 却判为点播")
	}
	if pl2.totalDur != 4 {
		t.Fatalf("totalDur=%v want 4", pl2.totalDur)
	}

	// 绝对 URL 分片不被改写
	pl3 := parsePlaylist("#EXTM3U\nhttps://other.example/x.ts\n", base)
	if len(pl3.segments) != 1 || pl3.segments[0] != "https://other.example/x.ts" {
		t.Fatalf("绝对 URL 分片解析错误: %v", pl3.segments)
	}
}

// TestDetectContainer 验证各格式魔数识别与扩展名/init 策略。
func TestDetectContainer(t *testing.T) {
	tsData := make([]byte, 376)
	tsData[0] = 0x47
	tsData[188] = 0x47

	cases := []struct {
		name     string
		data     []byte
		hasMap   bool
		wantID   string
		wantExt  string
		wantInit InitPolicy
	}{
		{"ts", tsData, false, "ts", ".ts", InitNone},
		{"fmp4-inline", []byte("\x00\x00\x00\x18ftypisom"), false, "fmp4", ".mp4", InitNone},
		{"fmp4-map", []byte("\x00\x00\x00\x18moofDATA"), true, "fmp4", ".mp4", InitFromMap},
		{"fmp4-moof-nomap", []byte("\x00\x00\x00\x18moofDATA"), false, "fmp4", ".mp4", InitFromMap},
		{"flv", []byte("FLV\x01\x05\x00\x00\x00\x09"), false, "flv", ".flv", InitNone},
		{"webm", append([]byte("\x1a\x45\xdf\xa3"), []byte("webm")...), false, "webm", ".webm", InitNone},
		{"mkv", append([]byte("\x1a\x45\xdf\xa3"), []byte("matroska")...), false, "mkv", ".mkv", InitNone},
		{"aac", []byte{0xff, 0xf1, 0x50, 0x80}, false, "aac", ".aac", InitNone},
		{"mp3-id3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00"), false, "mp3", ".mp3", InitNone},
		{"mp3-frame", []byte{0xff, 0xfb, 0x90, 0x00}, false, "mp3", ".mp3", InitNone},
		{"wav", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), false, "wav", ".wav", InitNone},
		{"ogg", []byte("OggS\x00\x02"), false, "ogg", ".ogg", InitNone},
		{"avi", []byte("RIFF\x00\x00\x00\x00AVI LIST"), false, "avi", ".avi", InitNone},
		{"generic", []byte("GENERIC-BINARY"), false, "generic", "", InitNone},
		{"map-with-ftyp-inline", []byte("\x00\x00\x00\x18ftypiso6"), true, "fmp4", ".mp4", InitNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectContainer(c.data, c.hasMap)
			if got.ID != c.wantID || got.Ext != c.wantExt || got.Init != c.wantInit {
				t.Fatalf("detectContainer(%s)=%+v want id=%s ext=%s init=%v",
					c.name, *got, c.wantID, c.wantExt, c.wantInit)
			}
		})
	}
}

// TestWriteInitSegment init 段只在文件为空时写入，重复调用不叠加。
func TestWriteInitSegment(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.mp4.part")
	init := []byte("FTYP-INIT-DATA")
	if err := writeInitSegment(p, init); err != nil {
		t.Fatalf("writeInitSegment: %v", err)
	}
	if err := writeInitSegment(p, []byte("SHOULD-NOT-APPEND")); err != nil {
		t.Fatalf("第二次 writeInitSegment: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(init) {
		t.Fatalf("init 段被重复写入: %q", data)
	}
}

// TestContainerBackfillRegistered 收尾处理已注册化：fMP4 容器（两条目 +
// findContainerByID）必须带 Backfill 回调，无收尾需求的容器（generic）必须为 nil。
func TestContainerBackfillRegistered(t *testing.T) {
	if c := findContainerByID("fmp4"); c == nil || c.Backfill == nil {
		t.Fatalf("findContainerByID(fmp4)=%+v want Backfill 非 nil", c)
	}
	if c := findContainerByID("generic"); c == nil || c.Backfill != nil {
		t.Fatalf("findContainerByID(generic)=%+v want Backfill nil", c)
	}
	// 探测路径：内联 init / #EXT-X-MAP 两种 fmp4 形态都要带收尾回调
	for _, d := range [][]byte{
		[]byte("\x00\x00\x00\x18ftypisom"),
		[]byte("\x00\x00\x00\x18moofDATA"),
	} {
		c := detectContainer(d, true)
		if c.ID != "fmp4" || c.Backfill == nil {
			t.Fatalf("detectContainer 命中 %s: Backfill 应为非 nil", c.ID)
		}
	}
	if c := detectContainer([]byte("GENERIC-BINARY"), false); c.Backfill != nil {
		t.Fatal("generic 容器 Backfill 应为 nil")
	}
}

// TestLiveFollow 直播跟随：播放列表逐轮增长，第二轮出现 ENDLIST，
// 验证增量下载、去重、顺序拼接与自然结束。
func TestLiveFollow(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		if n <= 1 {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n")
		} else {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n"+
				"#EXTINF:6.0,\nseg/2.ts\n#EXTINF:6.0,\nseg/3.ts\n#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "SEG-%s", strings.TrimPrefix(r.URL.Path, "/seg/"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldI, oldE := livePollInterval, liveMaxEmptyPolls
	livePollInterval = 5 * time.Millisecond
	liveMaxEmptyPolls = 3
	defer func() { livePollInterval, liveMaxEmptyPolls = oldI, oldE }()

	out := filepath.Join(t.TempDir(), "live.ts")
	j := &dlJob{m3u8URL: srv.URL + "/live.m3u8", live: true, seen: make(map[string]bool)}

	next, err := j.liveDownload(context.Background(), out, 0)
	if err != nil {
		t.Fatalf("liveDownload: %v", err)
	}
	if next != 4 {
		t.Fatalf("next=%d want 4", next)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "SEG-0.tsSEG-1.tsSEG-2.tsSEG-3.ts"
	if string(data) != want {
		t.Fatalf("拼接结果=%q want %q", data, want)
	}
}

// TestLiveFollowStop 直播跟随中途取消：ctx 取消后返回断点，文件保留已录分片。
func TestLiveFollowStop(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg/0.ts\n#EXTINF:6.0,\nseg/1.ts\n")
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "SEG-%s", strings.TrimPrefix(r.URL.Path, "/seg/"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldI := livePollInterval
	livePollInterval = 50 * time.Millisecond
	defer func() { livePollInterval = oldI }()

	out := filepath.Join(t.TempDir(), "live.ts")
	j := &dlJob{m3u8URL: srv.URL + "/live.m3u8", live: true, seen: make(map[string]bool)}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond) // 等第一轮分片写盘后再停
		cancel()
	}()
	next, err := j.liveDownload(ctx, out, 0)
	if err != nil {
		t.Fatalf("liveDownload 应为干净停止: %v", err)
	}
	if next != 2 {
		t.Fatalf("next=%d want 2（第一轮已录 2 片后停止）", next)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "SEG-0.tsSEG-1.ts" {
		t.Fatalf("文件内容=%q", data)
	}
}

// TestVODFMP4 点播 fMP4：播放列表带 #EXT-X-MAP，验证 init 段写入 + 首片预取复用。
func TestVODFMP4(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n"+
			"#EXTINF:6.0,\nseg0.m4s\n#EXTINF:6.0,\nseg1.m4s\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "FTYP-MOOV-INIT")
	})
	mux.HandleFunc("/seg0.m4s", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "\x00\x00\x00\x10moofDATA0")
	})
	mux.HandleFunc("/seg1.m4s", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "\x00\x00\x00\x10moofDATA1")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	j := &dlJob{m3u8URL: srv.URL + "/vod.m3u8"}
	content, base, isDirect, err := j.fetchPlaylist()
	if err != nil || isDirect {
		t.Fatalf("fetchPlaylist: isDirect=%v err=%v", isDirect, err)
	}
	pl := parsePlaylist(content, base)
	if len(pl.segments) != 2 {
		t.Fatalf("segments=%d", len(pl.segments))
	}
	if !pl.hasMap || pl.mapURI == "" {
		t.Fatalf("未解析出 #EXT-X-MAP: %+v", pl)
	}

	// 容器探测：moof 开头 + hasMap → fMP4 / InitFromMap
	pre, err := fetchSegment(ctx, j, pl.segments[0])
	if err != nil {
		t.Fatalf("fetchSegment: %v", err)
	}
	c := detectContainer(pre, pl.hasMap)
	if c.Ext != ".mp4" || c.Init != InitFromMap {
		t.Fatalf("容器=%+v want .mp4/InitFromMap", *c)
	}

	out := filepath.Join(t.TempDir(), "video.mp4.part")
	initData, err := fetchSegment(ctx, j, pl.mapURI)
	if err != nil {
		t.Fatalf("fetch init: %v", err)
	}
	if err := writeInitSegment(out, initData); err != nil {
		t.Fatalf("writeInitSegment: %v", err)
	}
	j.pre = pre

	next, err := streamDownload(ctx, j, pl.segments, 0, out)
	if err != nil {
		t.Fatalf("streamDownload: %v", err)
	}
	if next != 2 {
		t.Fatalf("next=%d want 2", next)
	}
	data, _ := os.ReadFile(out)
	want := "FTYP-MOOV-INIT" + "\x00\x00\x00\x10moofDATA0" + "\x00\x00\x00\x10moofDATA1"
	if string(data) != want {
		t.Fatalf("文件=%q want %q", data, want)
	}
}
