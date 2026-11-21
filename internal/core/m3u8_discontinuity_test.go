// #EXT-X-DISCONTINUITY 校验回归。
//
// 缺陷形态：解析器此前完全不认识这个标签（全项目零处理），于是带时间轴断点的
// 播放列表被当成一条连续时间轴顺序拼接 —— 断点之后的分片落在错误的时间位置，
// 产物"能播但时间轴错"，日志一切正常。与 #EXT-X-BYTERANGE 同源：跑得完、不报错、
// 产物不对。
//
// 覆盖两种写法：#EXT-X-DISCONTINUITY（断点本身）与 #EXT-X-DISCONTINUITY-SEQUENCE
// （断点序列号，出现即说明有断点）。
package core

import (
	"strings"
	"testing"
)

func TestDiscontinuityIsRejected(t *testing.T) {
	base := "https://cdn.example.com/v/pl.m3u8"
	cases := []struct {
		name string
		text string
	}{
		{
			"断点标签（点播）",
			"#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\ns0.ts\n" +
				"#EXT-X-DISCONTINUITY\n#EXTINF:6.0,\ns1.ts\n#EXT-X-ENDLIST\n",
		},
		{
			"断点序列号（v6+ 写法）",
			"#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:3\n#EXTINF:6.0,\ns0.ts\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pl := parsePlaylist(c.text, base)
			if !pl.hasDiscontinuity {
				t.Fatal("夹具本意是含断点，但解析结果没标记 —— 先查夹具，别让下面的断言在虚构状态上通过")
			}
			err := validatePlaylist(pl)
			if err == nil {
				t.Fatal("含 #EXT-X-DISCONTINUITY 的播放列表必须显式拒绝（断点后时间轴会错位）")
			}
			if !strings.Contains(err.Error(), "DISCONTINUITY") {
				t.Fatalf("错误信息应点名 DISCONTINUITY，实际: %v", err)
			}
		})
	}
}

// 配对：不含断点的正常播放列表必须照旧放行（判据不能扩大化）。
func TestNoDiscontinuityStillAccepted(t *testing.T) {
	base := "https://cdn.example.com/v/pl.m3u8"
	vod := parsePlaylist("#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+
		"#EXTINF:6.0,\ns0.ts\n#EXTINF:6.0,\ns1.ts\n#EXT-X-ENDLIST\n", base)
	if err := validatePlaylist(vod); err != nil {
		t.Fatalf("普通点播列表不该被拒绝: %v", err)
	}
	live := parsePlaylist("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:6.0,\ns0.ts\n", base)
	if err := validatePlaylist(live); err != nil {
		t.Fatalf("普通直播列表不该被拒绝: %v", err)
	}
}
