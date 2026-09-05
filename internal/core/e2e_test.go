package core

import (
	"encoding/binary"
	"os"
	"testing"
)

// TestValidateMehdE2E 端到端验证：真实录制文件 → 拆 init → 插 mehd → 规范化累计
// → 回填总时长。手动运行：GOCATCHER_SAMPLE=路径 go test -run TestValidateMehdE2E ./internal/core
func TestValidateMehdE2E(t *testing.T) {
	p := os.Getenv("GOCATCHER_SAMPLE")
	if p == "" {
		t.Skip("设置 GOCATCHER_SAMPLE 指向真实 fMP4 文件后手动运行")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// 1. 拆 init：从文件头到第一个 moof/styp 之前
	initEnd := 0
	pos := 0
	for pos+8 <= len(data) {
		sz, _, typ, ok := boxHeader(data, pos)
		if !ok {
			break
		}
		if typ == "moof" || typ == "styp" {
			initEnd = pos
			break
		}
		pos += sz
	}
	if initEnd == 0 {
		t.Fatal("文件开头未找到 init（首 box 即 moof/styp？）")
	}
	init := data[:initEnd]
	frags := data[initEnd:]
	t.Logf("init=%d 分片=%d", len(init), len(frags))

	// 2. 模拟新管线：init 补 mehd 占位
	newInit, info := prepareInit(init, true)
	if info.mehdOff < 0 {
		t.Fatal("init 无 moov/mvex，无法补 mehd")
	}
	t.Logf("mehdOff=%d movieTS=%d trackTS=%v mvhdOff=%d", info.mehdOff, info.movieTS, info.trackTS, info.mvhdOff)

	// 3. 分片规范化（累计每轨结束时间）——旧文件已归一化，此步幂等
	st := newNormState()
	normFrags, err := normalizeFMP4Segment(frags, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("end=%v", st.endSnapshot())

	// 4. 组装并回填
	out := append(append([]byte{}, newInit...), normFrags...)
	tmp := p + ".mehd.mp4"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		t.Fatal(err)
	}
	st.setInit(info)
	if err := backfillDurations(tmp, st); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(tmp)
	if v := binary.BigEndian.Uint64(got[info.mehdOff+12:]); v == 0 {
		t.Fatal("mehd duration 仍为 0")
	} else {
		t.Logf("mehd duration=%d（%.3f 秒）", v, float64(v)/float64(info.movieTS))
	}
	if info.mvhdOff >= 0 {
		off := info.mvhdOff + 24
		if v := binary.BigEndian.Uint32(got[off:]); v != 0 {
			t.Logf("mvhd duration=%d", v)
		}
	}
	t.Logf("已写出 %s", tmp)
}
