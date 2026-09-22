// #EXT-X-MAP 的 BYTERANGE 必须与分片行的 #EXT-X-BYTERANGE 同等对待（第七轮 P1-6）。
//
// 缺陷形态：**同一个特性，项目里两套态度**。分片行级的 #EXT-X-BYTERANGE 早就
// 显式拒绝（ensureNoByteRange），MAP 行级的同一个属性却无人过问 —— 于是
//
//	#EXT-X-MAP:URI="init.mp4",BYTERANGE="720@1000"
//
// 会被按"整个 init.mp4"下载。偏移非 0 时拿到的根本不是 init 段，fMP4 的容器识别
// 与轨道信息全都跟着错，而日志一行异常都没有（本项目头号缺陷形态：产物坏了但日志正常）。
package core

import (
	"strings"
	"testing"
)

// TestMapByteRangeIsRejected 带 BYTERANGE 的 #EXT-X-MAP 必须显式拒绝。
func TestMapByteRangeIsRejected(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"带引号", `#EXT-X-MAP:URI="init.mp4",BYTERANGE="720@0"`},
		{"裸值", `#EXT-X-MAP:URI=init.mp4,BYTERANGE=720@0`},
		{"BYTERANGE 在前", `#EXT-X-MAP:BYTERANGE="720@0",URI="init.mp4"`},
		{"偏移非 0", `#EXT-X-MAP:URI="init.mp4",BYTERANGE="720@1000"`},
	}
	for _, c := range cases {
		pl := parsePlaylist("#EXTM3U\n"+c.line+"\n#EXTINF:6.0,\nseg0.ts\n", "https://cdn.x/v/")
		if !pl.hasMap {
			t.Errorf("%s: 用例前提不成立：没解析出 init 段 URI（%s）", c.name, c.line)
			continue
		}
		if !pl.mapByteRange {
			t.Errorf("%s: 未识别出 MAP 的 BYTERANGE 属性（%s）", c.name, c.line)
			continue
		}
		err := validatePlaylist(pl)
		if err == nil {
			t.Errorf("%s: 带 BYTERANGE 的 init 段应被拒绝，validatePlaylist 放行了（%s）", c.name, c.line)
			continue
		}
		if !strings.Contains(err.Error(), "EXT-X-BYTERANGE") {
			t.Errorf("%s: 错误信息应点明是哪个特性，got %v", c.name, err)
		}
	}
}

// TestMapWithoutByteRangeStillAccepted 不误伤：MAP 行没有 BYTERANGE 时必须放行。
// 这条防的是"收得过头"——把任何 MAP 行都当不支持，正常 fMP4 流会全被拒掉。
func TestMapWithoutByteRangeStillAccepted(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"普通带引号", `#EXT-X-MAP:URI="init.mp4"`},
		{"普通裸值", `#EXT-X-MAP:URI=init.mp4`},
		// URI 是 quoted-string，查询串里出现同样的词**不是**属性，
		// 整行正则匹配会在这里误判 —— 这条用例专门钉住"只认属性位置"。
		{"查询串里出现该词", `#EXT-X-MAP:URI="https://x/i.mp4?BYTERANGE=0"`},
		// 引号内的逗号不是属性分隔符
		{"URI 含逗号", `#EXT-X-MAP:URI="a,b.mp4"`},
	}
	for _, c := range cases {
		pl := parsePlaylist("#EXTM3U\n"+c.line+"\n#EXTINF:6.0,\nseg0.ts\n", "https://cdn.x/v/")
		if !pl.hasMap {
			t.Errorf("%s: 应解析出 init 段 URI（%s），got hasMap=%v", c.name, c.line, pl.hasMap)
			continue
		}
		if pl.mapByteRange {
			t.Errorf("%s: 误判成带 BYTERANGE（%s）—— 正常流会被拒掉", c.name, c.line)
			continue
		}
		if err := validatePlaylist(pl); err != nil {
			t.Errorf("%s: 不该被拒绝（%s），got %v", c.name, c.line, err)
		}
	}
}

// TestMapByteRangeWithoutURIStillAccepted MAP 声明了 BYTERANGE 却没给 URI 时，
// 没有 init 段可取，不该因为一行畸形声明拒掉整个流（fMP4 流另有"缺 init 段"的
// 判定在管；TS 流则本来就该忽略这行）。
func TestMapByteRangeWithoutURIStillAccepted(t *testing.T) {
	pl := parsePlaylist("#EXTM3U\n#EXT-X-MAP:BYTERANGE=\"720@0\"\n#EXTINF:6.0,\nseg0.ts\n",
		"https://cdn.x/v/")
	if pl.hasMap || pl.mapURI != "" {
		t.Fatalf("没有 URI 就不该判为有 init 段：hasMap=%v mapURI=%q", pl.hasMap, pl.mapURI)
	}
	if pl.mapByteRange {
		t.Fatal("没有 URI 时不该置 mapByteRange：会把一行畸形声明放大成整条流被拒")
	}
	if err := validatePlaylist(pl); err != nil {
		t.Fatalf("不该被拒绝，got %v", err)
	}
}
