// fMP4 分片规范化测试：时间戳归一化 + 裸 NAL→AVCC。
package core

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
)

// ---- 合成 fMP4 分片（视频轨 trackID=1 + 音频轨 trackID=2）----

func box4(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

func fullBox4(typ string, flags uint32, payload []byte) []byte {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], flags)
	return box4(typ, append(h[:], payload...))
}

func tfhdBox(trackID uint32) []byte {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], trackID)
	return fullBox4("tfhd", 0x020000, p[:]) // default-base-is-moof
}

func tfdtBox(v uint64) []byte {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], uint32(v))
	return fullBox4("tfdt", 0, p[:])
}

// trunBox flags=0x201：data-offset + 逐样本 size。
func trunBox(sizes []uint32, dataOff int32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p, uint32(len(sizes)))
	binary.BigEndian.PutUint32(p[4:], uint32(dataOff))
	for _, s := range sizes {
		var sb [4]byte
		binary.BigEndian.PutUint32(sb[:], s)
		p = append(p, sb[:]...)
	}
	return fullBox4("trun", 0x201, p)
}

// buildSegment 构造 moof(video traf + audio traf) + mdat 的合成分片。
// rawV=true 时视频样本为 Annex-B 裸 NAL（start code 分隔），否则为已封装的 AVCC。
func buildSegment(tfdtV, tfdtA uint64, rawV bool) []byte {
	var v0, v1 []byte
	if rawV {
		// v0: NAL1=65 11 22 33 (4B) + NAL2=67 aa bb (3B)
		v0 = []byte{0, 0, 0, 1, 0x65, 0x11, 0x22, 0x33, 0, 0, 1, 0x67, 0xaa, 0xbb}
		v1 = []byte{0, 0, 1, 0x65, 0x44, 0x55, 0x66, 0x77} // NAL(5B)
	} else {
		v0 = []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33, 0, 0, 0, 3, 0x67, 0xaa, 0xbb}
		v1 = []byte{0, 0, 0, 5, 0x65, 0x44, 0x55, 0x66, 0x77}
	}
	return buildSegmentN(tfdtV, tfdtA, v0, v1)
}

// buildSegmentBare 构造视频样本为"无封装裸 NAL"（无 start code、无长度前缀）的分片。
func buildSegmentBare(tfdtV, tfdtA uint64) []byte {
	v0 := []byte{0x65, 0x11, 0x22, 0x33}       // IDR 切片 NAL（type 5）
	v1 := []byte{0x21, 0x44, 0x55, 0x66, 0x77} // 非 IDR 切片 NAL（type 1）
	return buildSegmentN(tfdtV, tfdtA, v0, v1)
}

func buildSegmentN(tfdtV, tfdtA uint64, v0, v1 []byte) []byte {
	return buildSegmentNA(tfdtV, tfdtA, v0, v1, []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0x11})
}

// buildSegmentNA 同 buildSegmentN，但音频样本可自定义。
func buildSegmentNA(tfdtV, tfdtA uint64, v0, v1, a0 []byte) []byte {
	mfhd := fullBox4("mfhd", 0, []byte{0, 0, 0, 1})
	tfhdV, tfhdA := tfhdBox(1), tfhdBox(2)
	tfdtVb, tfdtAb := tfdtBox(tfdtV), tfdtBox(tfdtA)
	trunV := trunBox([]uint32{uint32(len(v0)), uint32(len(v1))}, 0)
	trunA := trunBox([]uint32{uint32(len(a0))}, 0)

	trafV := box4("traf", append(append(append([]byte{}, tfhdV...), tfdtVb...), trunV...))
	trafA := box4("traf", append(append(append([]byte{}, tfhdA...), tfdtAb...), trunA...))
	moofB := box4("moof", append(append(append([]byte{}, mfhd...), trafV...), trafA...))

	mdatPayload := append(append(append([]byte{}, v0...), v1...), a0...)
	mdatB := box4("mdat", mdatPayload)

	// patch data_offset：视频轨指向 mdat payload 起点，音频轨紧随视频样本
	offV := int32(len(moofB) + 8)
	offA := offV + int32(len(v0)+len(v1))
	// moof 布局：moof头(8) + mfhd + trafV + trafA；traf 内：traF头(8) + tfhd + tfdt + trun
	trunVStart := 8 + len(mfhd) + 8 + len(tfhdV) + len(tfdtVb)
	binary.BigEndian.PutUint32(moofB[trunVStart+16:], uint32(offV)) // trun 内 data_offset 在 +16
	trunAStart := 8 + len(mfhd) + len(trafV) + 8 + len(tfhdA) + len(tfdtAb)
	binary.BigEndian.PutUint32(moofB[trunAStart+16:], uint32(offA))

	return append(moofB, mdatB...)
}

// hexDump 调试辅助：前 96 字节按 16 字节一行打印十六进制 + ASCII。
func hexDump(b []byte) string {
	const per = 16
	var sb bytes.Buffer
	n := len(b)
	if n > 96 {
		n = 96
	}
	for off := 0; off < n; off += per {
		end := off + per
		if end > n {
			end = n
		}
		fmt.Fprintf(&sb, "%04x  ", off)
		for i := off; i < end; i++ {
			fmt.Fprintf(&sb, "%02x ", b[i])
		}
		for i := end; i < off+per; i++ {
			sb.WriteString("   ")
		}
		sb.WriteString(" |")
		for i := off; i < end; i++ {
			c := b[i]
			if c < 0x20 || c > 0x7e {
				c = '.'
			}
			sb.WriteByte(c)
		}
		sb.WriteString("|\n")
	}
	if len(b) > n {
		fmt.Fprintf(&sb, "... 共 %d 字节", len(b))
	}
	return sb.String()
}

func collectMoofs(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var out [][]byte
	pos := 0
	for pos+8 <= len(data) {
		sz, _, typ, ok := boxHeader(data, pos)
		if !ok {
			break
		}
		if typ == "moof" {
			out = append(out, data[pos:pos+sz])
		}
		pos += sz
	}
	if len(out) == 0 {
		t.Fatal("未找到 moof")
	}
	return out
}

// TestNormalizeFMP4RawNAL 裸 NAL 分片：tfdt 归零 + NAL 转 AVCC + 样本表/尺寸重写。
func TestNormalizeFMP4RawNAL(t *testing.T) {
	seg := buildSegment(7792000, 7792128, true) // 直播已进行 7792 秒
	st := newNormState()
	out, err := normalizeFMP4Segment(seg, st)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	moofs := collectMoofs(t, out)
	moofBytes := moofs[0]
	moofAbs := 0 // 单分片内 moof 从 0 开始
	infos, perr := parseMoof(moofBytes, moofAbs)
	if perr != nil {
		t.Fatalf("parseMoof: %v", perr)
	}
	if len(infos) != 2 {
		t.Fatalf("traf=%d want 2", len(infos))
	}

	// 1. 两轨 tfdt 全部归零
	for i := range infos {
		if v := binary.BigEndian.Uint32(moofBytes[infos[i].tfdtOff:]); v != 0 {
			t.Fatalf("traf%d tfdt=%d want 0", i, v)
		}
	}

	// 2. 视频样本已转 AVCC（长度前缀），音频样本原样保留
	wantV0 := []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33, 0, 0, 0, 3, 0x67, 0xaa, 0xbb}
	wantV1 := []byte{0, 0, 0, 5, 0x65, 0x44, 0x55, 0x66, 0x77}
	audio := []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0x11}
	// mdat 位置：紧跟在 moof 后
	mdat := out[len(moofBytes):]
	if !bytes.Contains(mdat, wantV0) || !bytes.Contains(mdat, wantV1) || !bytes.Contains(mdat, audio) {
		t.Fatalf("mdat 内容不正确 len=%d:\n%s", len(mdat), hexDump(mdat))
	}
	if bytes.Contains(mdat, []byte{0, 0, 0, 1, 0x65}) || bytes.Contains(mdat, []byte{0, 0, 1, 0x65}) {
		t.Fatal("mdat 仍包含原始裸 NAL start code")
	}

	// 3. trun 样本大小已更新：视频 [15 9]，音频 [6]
	if len(infos[0].sizes) != 2 || infos[0].sizes[0] != 15 || infos[0].sizes[1] != 9 {
		t.Fatalf("视频轨样本大小=%v want [15 9]", infos[0].sizes)
	}
	if len(infos[1].sizes) != 1 || infos[1].sizes[0] != 6 {
		t.Fatalf("音频轨样本大小=%v want [6]", infos[1].sizes)
	}

	// 4. 音频轨 data_offset 已随视频样本增长同步（22 → 24）
	aStart := moofAbs + infos[1].dataStart
	if !bytes.Equal(out[aStart:aStart+6], audio) {
		t.Fatalf("音频轨 data_offset 未同步（位置 %d 处=% X）", aStart, out[aStart:aStart+6])
	}

	// 5. mdat 尺寸已重写：8 + 15 + 9 + 6 = 38
	if v := binary.BigEndian.Uint32(out[len(moofBytes):]); v != 38 {
		t.Fatalf("mdat size=%d want 38", v)
	}

	// 6. 基准已记录（视频 7792000 / 音频 7792128）
	snap := st.snapshot()
	if snap["1"] != 7792000 || snap["2"] != 7792128 {
		t.Fatalf("基准=%v", snap)
	}
}

// TestNormalizeFMP4BaselineAcrossSegments 第二分片沿用同一基准，
// 且基准可持久化/恢复（断点续传场景）。
func TestNormalizeFMP4BaselineAcrossSegments(t *testing.T) {
	st := newNormState()
	seg0 := buildSegment(7792000, 7792128, true)
	if _, err := normalizeFMP4Segment(seg0, st); err != nil {
		t.Fatalf("seg0: %v", err)
	}

	seg1 := buildSegment(7792000+96000, 7792128+96000, true)
	out1, err := normalizeFMP4Segment(seg1, st)
	if err != nil {
		t.Fatalf("seg1: %v", err)
	}
	moofs1 := collectMoofs(t, out1)
	infos1, _ := parseMoof(moofs1[0], 0)
	for i, want := range []uint32{96000, 96000} {
		if v := binary.BigEndian.Uint32(moofs1[0][infos1[i].tfdtOff:]); v != want {
			t.Fatalf("seg1 traf%d tfdt=%d want %d", i, v, want)
		}
	}

	// 模拟断点续传：持久化基准 → 新状态恢复 → 结果一致
	st2 := newNormState()
	st2.restore(st.snapshot())
	out2, err := normalizeFMP4Segment(seg1, st2)
	if err != nil {
		t.Fatalf("restore 后 normalize: %v", err)
	}
	moofs2 := collectMoofs(t, out2)
	infos2, _ := parseMoof(moofs2[0], 0)
	if v := binary.BigEndian.Uint32(moofs2[0][infos2[0].tfdtOff:]); v != 96000 {
		t.Fatalf("restore 后 tfdt=%d want 96000", v)
	}
}

// TestNormalizeFMP4AVCCPassthrough 已是 AVCC 的样本不被改动，仅时间戳归一化。
func TestNormalizeFMP4AVCCPassthrough(t *testing.T) {
	seg := buildSegment(2000, 2012, false)
	st := newNormState()
	out, err := normalizeFMP4Segment(seg, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(seg) {
		t.Fatalf("AVCC 输出长度 %d != 输入 %d", len(out), len(seg))
	}
	moofs := collectMoofs(t, out)
	infos, _ := parseMoof(moofs[0], 0)
	if infos[0].sizes[0] != 15 || infos[0].sizes[1] != 9 {
		t.Fatalf("AVCC 样本不应被改动: %v", infos[0].sizes)
	}
	if v := binary.BigEndian.Uint32(moofs[0][infos[0].tfdtOff:]); v != 0 {
		t.Fatalf("tfdt=%d want 0", v)
	}
}

// TestNormalizeFMP4BareNAL 无封装裸 NAL 样本（无 start code、无长度前缀）：
// 每个样本补 4 字节长度前缀转为 AVCC。
func TestNormalizeFMP4BareNAL(t *testing.T) {
	seg := buildSegmentBare(7792000, 7792128)
	st := newNormState()
	out, err := normalizeFMP4Segment(seg, st)
	if err != nil {
		t.Fatal(err)
	}

	moofs := collectMoofs(t, out)
	infos, perr := parseMoof(moofs[0], 0)
	if perr != nil {
		t.Fatal(perr)
	}
	mdat := out[len(moofs[0]):]

	// v0(4B)→8B，v1(5B)→9B；mdat = 8 + 8 + 9 + 6 = 31
	wantV0 := []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33}
	wantV1 := []byte{0, 0, 0, 5, 0x21, 0x44, 0x55, 0x66, 0x77}
	audio := []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0x11}
	if !bytes.Contains(mdat, wantV0) || !bytes.Contains(mdat, wantV1) || !bytes.Contains(mdat, audio) {
		t.Fatalf("mdat 内容不正确 len=%d:\n%s", len(mdat), hexDump(mdat))
	}
	if v := binary.BigEndian.Uint32(mdat); v != 31 {
		t.Fatalf("mdat size=%d want 31", v)
	}
	if len(infos[0].sizes) != 2 || infos[0].sizes[0] != 8 || infos[0].sizes[1] != 9 {
		t.Fatalf("视频轨样本大小=%v want [8 9]", infos[0].sizes)
	}
}

// TestNormalizeFMP4MixedNAL Annex-B 与裸 NAL 样本混合的轨道统一转换。
func TestNormalizeFMP4MixedNAL(t *testing.T) {
	// 手动拼接：v0 为 Annex-B（双 NAL），v1 为裸 NAL
	v0 := []byte{0, 0, 0, 1, 0x65, 0x11, 0x22, 0x33, 0, 0, 1, 0x67, 0xaa, 0xbb}
	v1 := []byte{0x21, 0x44, 0x55, 0x66, 0x77}
	seg := buildSegmentN(7792000, 7792128, v0, v1)
	st := newNormState()
	out, err := normalizeFMP4Segment(seg, st)
	if err != nil {
		t.Fatal(err)
	}
	moofs := collectMoofs(t, out)
	infos, _ := parseMoof(moofs[0], 0)
	if len(infos[0].sizes) != 2 || infos[0].sizes[0] != 15 || infos[0].sizes[1] != 9 {
		t.Fatalf("视频轨样本大小=%v want [15 9]", infos[0].sizes)
	}
	mdat := out[len(moofs[0]):]
	if !bytes.Contains(mdat, []byte{0, 0, 0, 5, 0x21, 0x44, 0x55, 0x66, 0x77}) {
		t.Fatalf("裸 NAL 样本未转换:\n%s", hexDump(mdat))
	}
}

// TestNormalizeFMP4WholeFile 整段拼接文件（init + 多分片）一次规范化。
func TestNormalizeFMP4WholeFile(t *testing.T) {
	init := append(box4("ftyp", []byte("isom")), box4("moov", []byte("dummy-moov"))...)
	seg0 := buildSegment(7792000, 7792128, true)
	seg1 := buildSegment(7792000+96000, 7792128+96000, true)
	full := append(append(append([]byte{}, init...), seg0...), seg1...)

	st := newNormState()
	out, err := normalizeFMP4Segment(full, st)
	if err != nil {
		t.Fatal(err)
	}
	moofs := collectMoofs(t, out)
	if len(moofs) != 2 {
		t.Fatalf("moof=%d want 2", len(moofs))
	}
	infos0, _ := parseMoof(moofs[0], 0)
	infos1, _ := parseMoof(moofs[1], 0)
	v0 := binary.BigEndian.Uint32(moofs[0][infos0[0].tfdtOff:])
	v1 := binary.BigEndian.Uint32(moofs[1][infos1[0].tfdtOff:])
	if v0 != 0 || v1 != 96000 {
		t.Fatalf("tfdt: seg0=%d seg1=%d want 0/96000", v0, v1)
	}
}

// TestNormalizeFMP4Styp moof 前有 styp 的分片正常处理。
func TestNormalizeFMP4Styp(t *testing.T) {
	styp := box4("styp", []byte("iso6"))
	seg := buildSegment(5000, 5012, true)
	full := append(append([]byte{}, styp...), seg...)

	st := newNormState()
	out, err := normalizeFMP4Segment(full, st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, styp) {
		t.Fatal("styp 被改动或丢失")
	}
	moofs := collectMoofs(t, out)
	infos, _ := parseMoof(moofs[0], 0)
	if v := binary.BigEndian.Uint32(moofs[0][infos[0].tfdtOff:]); v != 0 {
		t.Fatalf("tfdt=%d want 0", v)
	}
}

// TestNormalizeFMP4Passthrough 非 fMP4 / 结构异常数据原样放行、不报错。
func TestNormalizeFMP4Passthrough(t *testing.T) {
	st := newNormState()
	cases := [][]byte{
		nil,
		[]byte("GENERIC-BINARY-NOT-MP4"),
		[]byte{0x47, 0x11, 0x22, 0x33, 0x47, 0x00}, // TS 头
		{0, 0, 0, 12, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, // 仅 ftyp，无 moof
		{0, 0, 0, 8, 'm', 'o', 'o', 'f'},                       // 空 moof
	}
	for i, d := range cases {
		out, err := normalizeFMP4Segment(d, st)
		if err != nil {
			t.Fatalf("case %d 应优雅放行: %v", i, err)
		}
		if !bytes.Equal(out, d) {
			t.Fatalf("case %d 非 fMP4 数据被改动", i)
		}
	}
}

// TestNormalizeFMP4AudioUntouched 回归：AAC 音频轨（AOT=4 帧头 0x21 与 H.264
// NAL type 1 位冲突）在 init 段声明为 mp4a 后不得被误转为视频裸 NAL。
// 旧版 classifySample 特征猜测会把整条音频轨加上 4 字节长度前缀 → 声音断续。
func TestNormalizeFMP4AudioUntouched(t *testing.T) {
	init := buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000})
	st := newNormState()
	if _, err := normalizeFMP4Segment(init, st); err != nil {
		t.Fatal(err)
	}
	if !st.isVideoTrack(1) || st.isVideoTrack(2) {
		t.Fatalf("轨道类型解析错误: video(1)=%v audio(2)=%v", st.isVideoTrack(1), st.isVideoTrack(2))
	}

	// 视频裸 NAL 样本 + AAC-LTP 音频样本（首字节 0x21，会被旧逻辑判为裸 NAL）
	v0 := []byte{0x65, 0x11, 0x22, 0x33}
	v1 := []byte{0x21, 0x44, 0x55, 0x66, 0x77}
	a0 := []byte{0x21, 0x1C, 0x4D, 0xBD, 0xFF, 0xFF, 0xEF, 0xFF}
	seg := buildSegmentNA(7792000, 7792128, v0, v1, a0)
	out, err := normalizeFMP4Segment(seg, st)
	if err != nil {
		t.Fatal(err)
	}

	moofs := collectMoofs(t, out)
	infos, perr := parseMoof(moofs[0], 0)
	if perr != nil {
		t.Fatal(perr)
	}
	mdat := out[len(moofs[0]):]

	// 音频样本保持原样（无 4 字节前缀），尺寸不变
	if len(infos[1].sizes) != 1 || infos[1].sizes[0] != uint32(len(a0)) {
		t.Fatalf("音频样本大小=%v want [%d]（不得加前缀）", infos[1].sizes, len(a0))
	}
	if !bytes.Contains(mdat, a0) {
		t.Fatalf("音频样本被改动:\n%s", hexDump(mdat))
	}
	// 视频轨正常转换（裸 NAL → 4 字节前缀）
	wantV0 := []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33}
	if !bytes.Contains(mdat, wantV0) {
		t.Fatalf("视频样本未转换:\n%s", hexDump(mdat))
	}
}

// TestNormalizeRealFile 用真实录制的 fMP4 文件验证规范化。
// 手动运行：GOCATCHER_SAMPLE=路径 go test -run TestNormalizeRealFile ./internal/core
func TestNormalizeRealFile(t *testing.T) {	p := os.Getenv("GOCATCHER_SAMPLE")
	if p == "" {
		t.Skip("设置 GOCATCHER_SAMPLE 指向真实 fMP4 文件后手动运行")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	st := newNormState()
	out, err := normalizeFMP4Segment(data, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("基准: %v", st.snapshot())
	t.Logf("原始 %d 字节 → 规范化 %d 字节（+%d）", len(data), len(out), len(out)-len(data))
	moofs := collectMoofs(t, out)
	t.Logf("含 %d 个 moof", len(moofs))
	if err := os.WriteFile(p+".norm.mp4", out, 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("已写出 %s.norm.mp4", p)
}
