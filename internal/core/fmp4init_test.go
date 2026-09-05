// fMP4 init 段与总时长回填测试：mehd 占位插入、每轨结束时间累计、mehd/mvhd 回填。
package core

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildInit 构造最小 fMP4 init 段：ftyp + moov(mvhd + 每轨 trak(tkhd+mdia(mdhd)) + mvex(trex))。
// 注意：fullBox 的 payload 从 version_flags 之后开始，各字段偏移相应减 4。
func buildInit(movieTS uint32, trackTS map[uint32]uint32) []byte {
	// mvhd v0：verflags(4)+creation(4)+modification(4)+timescale(4)+duration(4)+rate(4)+
	// volume(2)+reserved(2)+reserved(8)+matrix(36)+predefined(24)+nextTrackID(4)=100
	mvhdPayload := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhdPayload[8:], movieTS)
	mvhd := fullBox4("mvhd", 0, mvhdPayload)

	var traks []byte
	for id, ts := range trackTS {
		// tkhd v0：verflags(4)+creation(4)+modification(4)+trackID(4)+...=84
		tkhdPayload := make([]byte, 84)
		binary.BigEndian.PutUint32(tkhdPayload[8:], id)
		tkhd := fullBox4("tkhd", 0x7, tkhdPayload)
		// mdhd v0：verflags(4)+creation(4)+modification(4)+timescale(4)+duration(4)+lang(2)+pre(2)=24
		mdhdPayload := make([]byte, 24)
		binary.BigEndian.PutUint32(mdhdPayload[8:], ts)
		mdhd := fullBox4("mdhd", 0, mdhdPayload)
		// stsd：video 轨用 hvc1、其它轨用 mp4a（供轨道类型识别）
		entryType := "mp4a"
		if id == 1 {
			entryType = "hvc1"
		}
		stsd := fullBox4("stsd", 0, append([]byte{0, 0, 0, 1}, box4(entryType, make([]byte, 8))...))
		stbl := box4("stbl", stsd)
		minf := box4("minf", stbl)
		mdia := box4("mdia", append(mdhd, minf...))
		trak := box4("trak", append(tkhd, mdia...))
		traks = append(traks, trak...)
	}

	trex := fullBox4("trex", 0, make([]byte, 24)) // verflags(4)+trackID(4)+...=24
	mvex := box4("mvex", trex)
	moov := box4("moov", append(append(mvhd, traks...), mvex...))
	return append(box4("ftyp", []byte("isom")), moov...)
}

// trunBoxDur 构造 trun flags=0x301：data-offset + 逐样本 duration + 逐样本 size。
func trunBoxDur(sizes, durs []uint32, dataOff int32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p, uint32(len(sizes)))
	binary.BigEndian.PutUint32(p[4:], uint32(dataOff))
	for i := range sizes {
		var db, sb [4]byte
		binary.BigEndian.PutUint32(db[:], durs[i])
		p = append(p, db[:]...)
		binary.BigEndian.PutUint32(sb[:], sizes[i])
		p = append(p, sb[:]...)
	}
	return fullBox4("trun", 0x301, p)
}

// buildSegmentDur 构造带逐样本 duration 的合成分片（视频轨 trackID=1 + 音频轨 trackID=2）。
func buildSegmentDur(tfdtV, tfdtA uint64, v0, v1 []byte, durV0, durV1, durA uint32) []byte {
	a0 := []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0x11}
	mfhd := fullBox4("mfhd", 0, []byte{0, 0, 0, 1})
	tfhdV, tfhdA := tfhdBox(1), tfhdBox(2)
	tfdtVb, tfdtAb := tfdtBox(tfdtV), tfdtBox(tfdtA)
	trunV := trunBoxDur([]uint32{uint32(len(v0)), uint32(len(v1))}, []uint32{durV0, durV1}, 0)
	trunA := trunBoxDur([]uint32{uint32(len(a0))}, []uint32{durA}, 0)

	trafV := box4("traf", append(append(append([]byte{}, tfhdV...), tfdtVb...), trunV...))
	trafA := box4("traf", append(append(append([]byte{}, tfhdA...), tfdtAb...), trunA...))
	moofB := box4("moof", append(append(append([]byte{}, mfhd...), trafV...), trafA...))

	mdatPayload := append(append(append([]byte{}, v0...), v1...), a0...)
	mdatB := box4("mdat", mdatPayload)

	offV := int32(len(moofB) + 8)
	offA := offV + int32(len(v0)+len(v1))
	trunVStart := 8 + len(mfhd) + 8 + len(tfhdV) + len(tfdtVb)
	binary.BigEndian.PutUint32(moofB[trunVStart+16:], uint32(offV))
	trunAStart := 8 + len(mfhd) + len(trafV) + 8 + len(tfhdA) + len(tfdtAb)
	binary.BigEndian.PutUint32(moofB[trunAStart+16:], uint32(offA))

	return append(moofB, mdatB...)
}

// TestPrepareInitInsertsMehd init 段无 mehd 时插入占位，并解析 timescale/位置。
func TestPrepareInitInsertsMehd(t *testing.T) {
	init := buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000})
	before := len(init)
	out, info := prepareInit(init, true)
	if len(out) != before+20 {
		t.Fatalf("插入 mehd 后长度 %d != %d+20", len(out), before)
	}
	if info.mehdOff < 0 {
		t.Fatal("未找到 mehd 位置")
	}
	if !info.mehdWide {
		t.Fatal("占位 mehd 应为 version 1（8 字节 duration）")
	}
	if info.movieTS != 1000 {
		t.Fatalf("movieTS=%d want 1000", info.movieTS)
	}
	if info.trackTS[1] != 90000 || info.trackTS[2] != 48000 {
		t.Fatalf("trackTS=%v", info.trackTS)
	}
	if info.mvhdOff < 0 || string(out[info.mvhdOff+4:info.mvhdOff+8]) != "mvhd" {
		t.Fatalf("mvhd 位置/版本未解析: off=%d", info.mvhdOff)
	}
	if !info.mvhdWide == false {
		t.Fatalf("mvhd wide=%v want false", info.mvhdWide)
	}
	// mehd 位于 mvex 子 box 序列起点，version=1，占位 duration=0
	if string(out[info.mehdOff+4:info.mehdOff+8]) != "mehd" {
		t.Fatal("mehd 位置 type 不正确")
	}
	if out[info.mehdOff+8] != 1 {
		t.Fatal("mehd version 应为 1")
	}
	if v := binary.BigEndian.Uint64(out[info.mehdOff+12:]); v != 0 {
		t.Fatalf("占位 duration=%d want 0", v)
	}
	// 再次调用：已存在 mehd，不重复插入，位置不变
	out2, info2 := prepareInit(out, true)
	if len(out2) != len(out) {
		t.Fatal("重复 prepareInit 不应再插入")
	}
	if info2.mehdOff != info.mehdOff {
		t.Fatalf("二次解析 mehdOff=%d want %d", info2.mehdOff, info.mehdOff)
	}
	// 非 fMP4（无 moov/mvex）原样返回
	passthrough := box4("moov", []byte("no-mvex-here"))
	raw, info3 := prepareInit(passthrough, true)
	if !bytes.Equal(raw, passthrough) {
		t.Fatal("无 mvex 的 moov 不应被改动")
	}
	if info3.mehdOff != -1 {
		t.Fatal("无 mvex 时不应记录 mehd 位置")
	}
}

// TestInitInfoPersistRoundTrip initInfo 持久化/恢复往返一致。
func TestInitInfoPersistRoundTrip(t *testing.T) {
	init := buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000})
	_, info := prepareInit(init, true)
	var restored fmp4InitInfo
	restored.restore(info.persist())
	if restored.movieTS != info.movieTS || restored.mehdOff != info.mehdOff ||
		restored.mehdWide != info.mehdWide || restored.mvhdOff != info.mvhdOff ||
		restored.mvhdWide != info.mvhdWide {
		t.Fatalf("restore 不一致: %+v vs %+v", restored, info)
	}
	if restored.trackTS[1] != 90000 || restored.trackTS[2] != 48000 {
		t.Fatalf("restore trackTS=%v", restored.trackTS)
	}
}

// TestNormStateEndAccumulation 跨分片累计每轨相对结束时间，且可持久化/恢复。
// trun 同时带 duration 与 size 字段（0x301），同时验证交错字段读取正确性。
func TestNormStateEndAccumulation(t *testing.T) {
	v0 := []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33}       // 已封装 AVCC，8B
	v1 := []byte{0, 0, 0, 5, 0x65, 0x44, 0x55, 0x66, 0x77} // 9B
	st := newNormState()

	seg0 := buildSegmentDur(9000000, 9000000, v0, v1, 3000, 3000, 2048)
	out0, err := normalizeFMP4Segment(seg0, st)
	if err != nil {
		t.Fatal(err)
	}
	// 交错字段读取验证：duration 与 size 同帧出现时 size 不能被 duration 污染
	moofs0 := collectMoofs(t, out0)
	infos0, _ := parseMoof(moofs0[0], 0)
	if len(infos0[0].sizes) != 2 || infos0[0].sizes[0] != 8 || infos0[0].sizes[1] != 9 {
		t.Fatalf("交错字段下视频轨样本大小=%v want [8 9]", infos0[0].sizes)
	}
	if infos0[0].durTotal != 6000 || infos0[1].durTotal != 2048 {
		t.Fatalf("durTotal=%v want [6000 2048]", []uint64{infos0[0].durTotal, infos0[1].durTotal})
	}
	end := st.endSnapshot()
	if end["1"] != 6000 || end["2"] != 2048 {
		t.Fatalf("end=%v want {1:6000 2:2048}", end)
	}

	seg1 := buildSegmentDur(9006000, 9002048, v0, v1, 3000, 3000, 2048)
	if _, err := normalizeFMP4Segment(seg1, st); err != nil {
		t.Fatal(err)
	}
	end = st.endSnapshot()
	if end["1"] != 12000 || end["2"] != 4096 {
		t.Fatalf("end=%v want {1:12000 2:4096}", end)
	}

	// 断点续传：持久化 → 新状态恢复 → 继续累计
	st2 := newNormState()
	st2.restore(st.snapshot())
	st2.restoreEnd(st.endSnapshot())
	seg2 := buildSegmentDur(9012000, 9004096, v0, v1, 3000, 3000, 2048)
	if _, err := normalizeFMP4Segment(seg2, st2); err != nil {
		t.Fatal(err)
	}
	if e := st2.endSnapshot(); e["1"] != 18000 || e["2"] != 6144 {
		t.Fatalf("restore 后 end=%v want {1:18000 2:6144}", e)
	}
}

// buildInitWithHDL 同 buildInit，但每个 trak 的 mdia 额外带 hdlr（handler_type 按 handler 参数）。
// hdlr 完整 payload：verflags(4)+pre_defined(4)+handler_type(4)+reserved(12)；
// fullBox4 已含 verflags，故 hdlrPayload = pre_defined(4)+handler_type(4)+reserved(12) = 20 字节。
func buildInitWithHDL(movieTS uint32, trackTS map[uint32]uint32, handler map[uint32]string) []byte {
	var traks []byte
	for id, ts := range trackTS {
		tkhdPayload := make([]byte, 84)
		binary.BigEndian.PutUint32(tkhdPayload[8:], id)
		tkhd := fullBox4("tkhd", 0x7, tkhdPayload)
		mdhdPayload := make([]byte, 24)
		binary.BigEndian.PutUint32(mdhdPayload[8:], ts)
		mdhd := fullBox4("mdhd", 0, mdhdPayload)
		hdlrPayload := make([]byte, 20)
		copy(hdlrPayload[4:], []byte(handler[id])) // handler_type 位于 pre_defined 之后
		hdlr := fullBox4("hdlr", 0, hdlrPayload)
		entryType := "mp4a"
		if id == 1 {
			entryType = "hvc1"
		}
		stsd := fullBox4("stsd", 0, append([]byte{0, 0, 0, 1}, box4(entryType, make([]byte, 8))...))
		stbl := box4("stbl", stsd)
		minf := box4("minf", stbl)
		mdia := box4("mdia", append(append(mdhd, hdlr...), minf...))
		trak := box4("trak", append(tkhd, mdia...))
		traks = append(traks, trak...)
	}
	trex := fullBox4("trex", 0, make([]byte, 24))
	mvex := box4("mvex", trex)
	mvhdPayload := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhdPayload[8:], movieTS)
	mvhd := fullBox4("mvhd", 0, mvhdPayload)
	moov := box4("moov", append(append(mvhd, traks...), mvex...))
	return append(box4("ftyp", []byte("isom")), moov...)
}

// TestHdlrMediaTypePriority hdlr.handler_type 是轨道类型权威来源，且优先于 stsd entry 列表。
// 用例：hdlr 声明 track1=soun / track2=vide，与 stsd 的 hvc1/mp4a 完全相反，
// 结果必须按 hdlr 判定（track1 音频、track2 视频）。
func TestHdlrMediaTypePriority(t *testing.T) {
	init := buildInitWithHDL(1000, map[uint32]uint32{1: 90000, 2: 48000},
		map[uint32]string{1: "soun", 2: "vide"})
	_, info := prepareInit(init, true)
	if len(info.trackVideo) != 2 {
		t.Fatalf("trackVideo=%v want 2 个轨道", info.trackVideo)
	}
	if info.trackVideo[1] {
		t.Fatalf("track1: hdlr=soun 应判音频，got %v", info.trackVideo[1])
	}
	if !info.trackVideo[2] {
		t.Fatalf("track2: hdlr=vide 应判视频，got %v", info.trackVideo[2])
	}
}

// TestHdlrFallbackToStsd 无 hdlr 时回退 stsd entry 判定（hvc1=视频 / mp4a=音频）。
func TestHdlrFallbackToStsd(t *testing.T) {
	init := buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000})
	_, info := prepareInit(init, true)
	if !info.trackVideo[1] || info.trackVideo[2] {
		t.Fatalf("stsd fallback 判定错误: %v", info.trackVideo)
	}
}

// TestBackfillDurationsWrites 任务完成后把总时长回填 mehd 与 mvhd。
func TestBackfillDurationsWrites(t *testing.T) {
	init := buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000})
	init, info := prepareInit(init, true)
	if info.mehdOff < 0 {
		t.Fatal("init 无 mehd")
	}

	// 模拟真实管线：init 落盘 → 分片经 normFn（累计到同一 norm）落盘
	st := newNormState()
	v0 := []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33}
	v1 := []byte{0, 0, 0, 5, 0x65, 0x44, 0x55, 0x66, 0x77}
	seg := buildSegmentDur(900000, 480000, v0, v1, 450000, 450000, 240000) // 视频 10s / 音频 5s
	seg2 := buildSegmentDur(1800000, 960000, v0, v1, 450000, 450000, 240000)

	data := append([]byte{}, init...)
	for _, s := range [][]byte{seg, seg2} {
		out, err := normalizeFMP4Segment(s, st)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, out...)
	}
	// 视频 end=1800000(20s)，音频 end=720000(15s) → movie 单位 max=20000
	if d := mehdDuration(st, info); d != 20000 {
		t.Fatalf("mehdDuration=%d want 20000", d)
	}

	path := filepath.Join(t.TempDir(), "rec.mp4")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	// 真实管线中 init 信息由规范化状态持有（setInit），Backfill 只收状态
	st.setInit(info)
	if err := backfillDurations(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 偏移必须指向真实 box（防止相对偏移把时长写进 moov size 字段）
	if string(got[info.mehdOff+4:info.mehdOff+8]) != "mehd" {
		t.Fatalf("mehdOff=%d 处不是 mehd box", info.mehdOff)
	}
	if string(got[info.mvhdOff+4:info.mvhdOff+8]) != "mvhd" {
		t.Fatalf("mvhdOff=%d 处不是 mvhd box", info.mvhdOff)
	}
	// mehd v1：duration 8 字节，位于 mehd+12
	if v := binary.BigEndian.Uint64(got[info.mehdOff+12:]); v != 20000 {
		t.Fatalf("mehd duration=%d want 20000", v)
	}
	// mvhd v0：duration 4 字节，位于 mvhd+8+16
	if v := binary.BigEndian.Uint32(got[info.mvhdOff+24:]); v != 20000 {
		t.Fatalf("mvhd duration=%d want 20000", v)
	}
}

// TestBackfillDurationsFtypLayout 回归测试：moov 前面有 ftyp（如 B 站真实 init），
// mvhd 偏移必须记绝对位置，否则回填会把时长写进 moov 的 size 字段破坏文件。
func TestBackfillDurationsFtypLayout(t *testing.T) {
	init := buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000})
	// 把 ftyp 加长到 32 字节（同真实 init），moov 不再从偏移 0 开始
	// buildInit 的 ftyp 是 12 字节（box4("ftyp","isom")），其后的 moov 从 12 开始
	init = append(box4("ftyp", make([]byte, 24)), init[12:]...)
	init, info := prepareInit(init, true)
	if info.mvhdOff <= 16 {
		t.Fatalf("mvhdOff=%d 应为绝对偏移（moov 前的 ftyp 之后）", info.mvhdOff)
	}
	if string(init[info.mvhdOff+4:info.mvhdOff+8]) != "mvhd" {
		t.Fatalf("mvhdOff=%d 处不是 mvhd box", info.mvhdOff)
	}

	st := newNormState()
	v0 := []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33}
	v1 := []byte{0, 0, 0, 5, 0x65, 0x44, 0x55, 0x66, 0x77}
	seg := buildSegmentDur(900000, 480000, v0, v1, 450000, 450000, 240000)
	data := append([]byte{}, init...)
	out, err := normalizeFMP4Segment(seg, st)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, out...)

	path := filepath.Join(t.TempDir(), "rec.mp4")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	st.setInit(info)
	if err := backfillDurations(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// moov size 字段（moov box 起点前 4 字节）不得被覆盖：仍等于 moov 真实大小（~500）
	moovAbs := info.mvhdOff - 8 // mvhd 是 moov 的第一个子 box
	moovSize := int(binary.BigEndian.Uint32(got[moovAbs:]))
	if moovSize < 100 || moovSize >= 20000 {
		t.Fatalf("moov size=%d 疑似被时长覆盖（应为 ~500）", moovSize)
	}
	// mvhd.duration 在 mvhd+24（v0），mehd 在 mehd+12（单分片：视频 10s → movie 单位 10000）
	if v := binary.BigEndian.Uint32(got[info.mvhdOff+24:]); v != 10000 {
		t.Fatalf("mvhd duration=%d want 10000", v)
	}
	if v := binary.BigEndian.Uint64(got[info.mehdOff+12:]); v != 10000 {
		t.Fatalf("mehd duration=%d want 10000", v)
	}
}
