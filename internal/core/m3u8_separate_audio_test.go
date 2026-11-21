// 独立音频轨道（#EXT-X-MEDIA:TYPE=AUDIO）校验回归。
//
// 缺陷形态：pickHighestBitrateM3U8 只看 #EXT-X-STREAM-INF 的 BANDWIDTH，完全不
// 认识 #EXT-X-MEDIA。当 master 把音频单独切成一条播放列表、变体用 AUDIO 属性引用
// 它时，下回来的成品**没有声音**，日志一切正常。
//
// 判据刻意收窄（见 masterHasSeparateAudioTrack）：必须「被选中变体引用了它」+
// 「是音频」+「带 URI」三者同时成立才拒，否则会误伤 muxed 音频、空 URI 的组与字幕轨。
package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMasterSeparateAudioDetected 判据本身的分支表（纯函数，逐支验）。
func TestMasterSeparateAudioDetected(t *testing.T) {
	const audioWithURI = `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="a",URI="audio/a.m3u8"`
	const audioNoURI = `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="a"`
	cases := []struct {
		name    string
		media   string
		variant string
		want    bool
	}{
		{"被引用 + 带 URI ⇒ 拒", audioWithURI, `#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO="aud"`, true},
		{"音频被 mux 进变体（URI 缺省）⇒ 放行", audioNoURI, `#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO="aud"`, false},
		{"变体没写 AUDIO 属性 ⇒ 放行", audioWithURI, `#EXT-X-STREAM-INF:BANDWIDTH=1000000`, false},
		{"引用的是别的组 ⇒ 放行", audioWithURI, `#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO="other"`, false},
		{
			"只有字幕轨 ⇒ 放行",
			`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="sub",URI="sub/s.m3u8"`,
			`#EXT-X-STREAM-INF:BANDWIDTH=1000000,SUBTITLES="sub"`, false,
		},
		{
			"裸值写法（非规范播放列表）⇒ 同样要拒",
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=aud,URI=audio/a.m3u8`,
			`#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=aud`, true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			content := "#EXTM3U\n" + c.media + "\n" + c.variant + "\nmedia.m3u8\n"
			if got := masterHasSeparateAudioTrack(content, c.variant); got != c.want {
				t.Fatalf("masterHasSeparateAudioTrack=%v want %v", got, c.want)
			}
		})
	}
}

// TestSeparateAudioStreamIsRejected 端到端：走 fetchPlaylist 时必须报错。
func TestSeparateAudioStreamIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="a",URI="audio/a.m3u8"` + "\n" +
			`#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO="aud"` + "\nvideo/v.m3u8\n"))
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 1)

	j := &dlJob{rt: testStd, m3u8URL: srv.URL + "/master.m3u8", referer: ""}
	_, _, _, err := j.fetchPlaylist(context.Background())
	if err == nil {
		t.Fatal("音频在独立轨道上的流必须显式拒绝（否则成品无声）")
	}
	if !strings.Contains(err.Error(), "EXT-X-MEDIA") {
		t.Fatalf("错误信息应点名 EXT-X-MEDIA，实际: %v", err)
	}
}
