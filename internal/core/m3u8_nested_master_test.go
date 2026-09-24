// 多级嵌套 master 校验回归。
//
// 缺陷形态：fetchPlaylist 只做一跳，第二跳拿到的东西只验了「首行是 #EXTM3U」。
// 若子播放列表本身又是 master，里面那一串地址其实是**下一级播放列表**，而
// parsePlaylist 会跳过所有以 # 开头的行、把这些地址当成分片逐个下载 ——
// 拼出来的成品是若干份 m3u8 文本，日志却一切正常。
package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNestedMasterIsRejected(t *testing.T) {
	const master = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nsub.m3u8\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 两跳都返回 master：子播放列表本身又是一个 master。
		w.Write([]byte(master))
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1)

	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/master.m3u8", referer: ""}
	_, _, _, err := j.fetchPlaylist(context.Background())
	if err == nil {
		t.Fatal("子播放列表又是 master 时必须显式拒绝（否则会把下一级 m3u8 当地址下回来）")
	}
	if !strings.Contains(err.Error(), "嵌套") {
		t.Fatalf("错误信息应说明是多级嵌套，实际: %v", err)
	}
}

// 配对：正常的一跳 master（子列表是媒体播放列表）必须照旧放行。
func TestSingleHopMasterStillAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "master.m3u8") {
			w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nsub.m3u8\n"))
			return
		}
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1)

	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/master.m3u8", referer: ""}
	content, base, isDirect, err := j.fetchPlaylist(context.Background())
	if err != nil {
		t.Fatalf("正常的一跳 master 不该报错: %v", err)
	}
	if isDirect || !strings.HasSuffix(base, "/sub.m3u8") {
		t.Fatalf("应返回子播放列表内容与子 URL，实际 base=%q isDirect=%v", base, isDirect)
	}
	if pl := parsePlaylist(content, base); len(pl.segments) != 1 {
		t.Fatalf("子播放列表应解析出 1 个分片，实际 %v", pl.segments)
	}
}
