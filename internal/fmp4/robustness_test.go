// fMP4 解析边界与容错路径测试：box 头变体、version 1 全盒、轨道类型兜底、
// tfhd/trun flag 组合、分片结构异常放行、扩展大小 mdat 重建、回填错误路径。
package fmp4

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// ---- 构造小工具 ----

// v1Payload 构造 version 1 全盒 payload（time 字段 64 位，目标字段在 +20）。
// total < 24 时生成截断形态（字段缺失），供解析拒绝路径测试。
func v1Payload(fieldAt20 uint32, total int) []byte {
	p := make([]byte, total)
	p[0] = 1
	if total >= 24 {
		binary.BigEndian.PutUint32(p[20:], fieldAt20)
	}
	return p
}

// hdlrPayload 构造 hdlr 盒 payload（handler_type 在 +8，共 24 字节）。
func hdlrPayload(handler string) []byte {
	p := make([]byte, 24)
	copy(p[8:], handler)
	return p
}

// mdhdV0 构造 version 0 的 mdhd 盒（timescale 位于 fullBox payload+8）。
func mdhdV0(timescale uint32) []byte {
	p := make([]byte, 24)
	binary.BigEndian.PutUint32(p[8:], timescale)
	return fullBox4("mdhd", 0, p)
}

// ---- boxHeader 边界 ----

func TestBoxHeaderBoundaries(t *testing.T) {
	if _, _, _, ok := boxHeader([]byte{0, 0, 0, 1, 'a'}, 0); ok {
		t.Fatal("不足 8 字节不应解析出 box")
	}
	// 扩展大小（size==1，largesize 64 位）
	ext := make([]byte, 24)
	binary.BigEndian.PutUint32(ext, 1)
	copy(ext[4:8], "mdat")
	binary.BigEndian.PutUint64(ext[8:], 24)
	sz, payload, typ, ok := boxHeader(ext, 0)
	if !ok || sz != 24 || payload != 16 || typ != "mdat" {
		t.Fatalf("扩展大小 box: sz=%d payload=%d typ=%q ok=%v", sz, payload, typ, ok)
	}
	if _, _, _, ok := boxHeader(ext[:12], 0); ok {
		t.Fatal("截断的扩展头不应通过")
	}
	// size==0：box 延伸到缓冲末尾
	till := append([]byte{0, 0, 0, 0, 'f', 'r', 'e', 'e'}, 1, 2, 3)
	sz, payload, typ, ok = boxHeader(till, 0)
	if !ok || sz != len(till) || payload != 8 || typ != "free" {
		t.Fatalf("size=0 box: sz=%d payload=%d typ=%q ok=%v", sz, payload, typ, ok)
	}
	// size 小于头长与越界 size
	tiny := make([]byte, 16)
	copy(tiny[4:8], "mdat")
	binary.BigEndian.PutUint32(tiny, 4)
	if _, _, _, ok := boxHeader(tiny, 0); ok {
		t.Fatal("size < 头长不应通过")
	}
	binary.BigEndian.PutUint32(tiny, 1000)
	if _, _, _, ok := boxHeader(tiny, 0); ok {
		t.Fatal("越界 size 不应通过")
	}
	// 非 0 起点
	buf := append(box4("skip", nil), till...)
	if _, _, _, ok := boxHeader(buf, 8); !ok {
		t.Fatal("非 0 起点应正常解析")
	}
	if bytesEqual([]byte{1, 2}, []byte{1, 3}) || bytesEqual([]byte{1}, []byte{1, 2}) {
		t.Fatal("bytesEqual 判定错误")
	}
}

// ---- version 1 全盒（mvhd/tkhd/mdhd 64 位时间戳形态）----

func TestVersionOneBoxParsers(t *testing.T) {
	if ts, ok := mvhdTimescale(box4("mvhd", v1Payload(44100, 32))[8:]); !ok || ts != 44100 {
		t.Fatalf("mvhd v1: ts=%d ok=%v want 44100", ts, ok)
	}
	if _, ok := mvhdTimescale(box4("mvhd", make([]byte, 14))[8:]); ok {
		t.Fatal("过短 mvhd 不应解析")
	}
	if _, ok := mvhdTimescale(box4("mvhd", v1Payload(0, 20))[8:]); ok {
		t.Fatal("截断 v1 mvhd 不应解析")
	}
	if id := tkhdTrackID(box4("tkhd", v1Payload(7, 32))[8:]); id != 7 {
		t.Fatalf("tkhd v1: trackID=%d want 7", id)
	}
	if id := tkhdTrackID(box4("tkhd", make([]byte, 12))[8:]); id != 0 {
		t.Fatal("过短 tkhd 应返回 0")
	}
	if id := tkhdTrackID(box4("tkhd", v1Payload(0, 20))[8:]); id != 0 {
		t.Fatal("截断 v1 tkhd 应返回 0")
	}
	// mdhd v1：包在 mdia 内解析
	if ts, ok := mdhdTimescale(box4("mdia", box4("mdhd", v1Payload(48000, 32)))); !ok || ts != 48000 {
		t.Fatalf("mdhd v1: ts=%d ok=%v", ts, ok)
	}
	if _, ok := mdhdTimescale(box4("mdia", box4("mdhd", make([]byte, 10)))); ok {
		t.Fatal("过短 mdhd 不应解析")
	}
	if _, ok := mdhdTimescale(box4("mdia", box4("mdhd", v1Payload(0, 20)))); ok {
		t.Fatal("截断 v1 mdhd 不应解析")
	}
	// mdia 内先遇非 mdhd 盒：跳过后继续找；畸形首子盒：扫描中断
	mixed := box4("mdia", append(box4("hdlr", hdlrPayload("vide")), mdhdV0(3000)...))
	if ts, ok := mdhdTimescale(mixed); !ok || ts != 3000 {
		t.Fatalf("跳过 hdlr 后解析 mdhd: ts=%d ok=%v", ts, ok)
	}
	if _, ok := mdhdTimescale(box4("mdia", []byte{0, 0, 0, 4, 1, 2, 3, 4})); ok {
		t.Fatal("畸形 mdia 不应解析")
	}
}

// ---- 轨道类型判定兜底（hdlr/stsd/classify）----

func TestMediaTypeFallback(t *testing.T) {
	for handler, want := range map[string]string{"vide": "video", "soun": "audio", "meta": "", "text": ""} {
		if got := hdlrMediaType(box4("mdia", box4("hdlr", hdlrPayload(handler)))); got != want {
			t.Fatalf("hdlr(%s)=%q want %q", handler, got, want)
		}
	}
	// hdlr 过短（payload 不足 16 字节）与畸形 mdia 容器
	if got := hdlrMediaType(box4("mdia", box4("hdlr", make([]byte, 8)))); got != "" {
		t.Fatalf("过短 hdlr=%q want 空", got)
	}
	if got := hdlrMediaType(box4("mdia", []byte{0, 0, 0, 4, 1, 2, 3, 4})); got != "" {
		t.Fatalf("畸形 mdia=%q want 空", got)
	}
	// stsdMediaType：无 minf / 空 stsd / 未知 entry / 各层尾随垃圾
	if got := stsdMediaType(box4("mdia", box4("hdlr", hdlrPayload("vide")))); got != "" {
		t.Fatalf("无 minf: %q want 空", got)
	}
	stsdBare := fullBox4("stsd", 0, nil) // 无 entry_count 与 entry
	if got := stsdMediaType(box4("mdia", []byte{0, 0, 0, 4, 1, 2, 3, 4})); got != "" {
		t.Fatalf("畸形 mdia: %q want 空", got)
	}
	if got := stsdMediaType(box4("mdia", box4("minf", box4("stbl", stsdBare)))); got != "" {
		t.Fatalf("空 stsd: %q want 空", got)
	}
	stsdText := fullBox4("stsd", 0, append([]byte{0, 0, 0, 1}, box4("text", make([]byte, 8))...))
	if got := stsdMediaType(box4("mdia", box4("minf", box4("stbl", stsdText)))); got != "" {
		t.Fatalf("未知 entry: %q want 空", got)
	}
	junk := []byte{0, 0, 0, 4, 1, 2, 3, 4} // 声明 size=4 的畸形盒
	if got := stsdMediaType(box4("mdia", box4("minf", append(box4("stbl", stsdBare), junk...)))); got != "" {
		t.Fatalf("minf 尾随垃圾: %q want 空", got)
	}
	if got := stsdMediaType(box4("mdia", box4("minf", append(box4("stbl", stsdBare), junk...)))); got != "" {
		t.Fatalf("stbl 尾随垃圾: %q want 空", got)
	}
	if got := stsdMediaType(box4("mdia", append(box4("minf", box4("stbl", stsdBare)), junk...))); got != "" {
		t.Fatalf("mdia 尾随垃圾: %q want 空", got)
	}
	// classifyMediaType：编解码条目枚举（video / audio / 未知）
	for entry, want := range map[string]string{"av01": "video", "vp09": "video", "dvh1": "video",
		"enca": "audio", "fLaC": "audio", "Opus": "audio", "text": "", "url ": ""} {
		if got := classifyMediaType(entry); got != want {
			t.Fatalf("classify(%s)=%q want %q", entry, got, want)
		}
	}
	// 解析容错：顶层/轨道内畸形盒中断扫描
	info := &fmp4InitInfo{}
	parseTimescales([]byte{0, 0, 0, 4, 1, 2, 3, 4}, info)
	if info.movieTS != 0 {
		t.Fatalf("畸形顶层盒后 movieTS=%d want 0", info.movieTS)
	}
	tkhdPayload := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhdPayload[8:], 9)
	trakInfo := &fmp4InitInfo{trackTS: map[uint32]uint32{}}
	trak := box4("trak", append(append(append([]byte{},
		fullBox4("tkhd", 0, tkhdPayload)...), junk...), box4("mdia", mdhdV0(48000))...))
	parseTrakTimescales(trak, trakInfo)
	if _, ok := trakInfo.trackTS[9]; ok {
		t.Fatal("畸形子盒后不应继续解析 mdia")
	}
}

// ---- mehdDuration 边界 ----

func TestMehdDurationEdge(t *testing.T) {
	st := NewState()
	if d := mehdDuration(st, nil); d != 0 {
		t.Fatalf("nil info: %d want 0", d)
	}
	if d := mehdDuration(st, &fmp4InitInfo{}); d != 0 {
		t.Fatalf("movieTS=0: %d want 0", d)
	}
	st.end[7] = 500   // 无 timescale 的轨道：跳过
	st.end[1] = 90000 // 90000*1000/90000 = 1000
	st.end[2] = 96000 // 96000*1000/48000 = 2000：多轨取最大换算时长
	info := &fmp4InitInfo{movieTS: 1000, trackTS: map[uint32]uint32{1: 90000, 2: 48000}}
	if d := mehdDuration(st, info); d != 2000 {
		t.Fatalf("mehdDuration=%d want 2000", d)
	}
}

// ---- persist / backfill / writeDuration 错误路径 ----

func TestPersistNilInfo(t *testing.T) {
	var nilInfo *fmp4InitInfo
	if nilInfo.persist() != nil {
		t.Fatal("nil 接收者应返回 nil")
	}
}

func TestBackfillErrorPaths(t *testing.T) {
	st := NewState()
	st.end[1] = 90000
	if err := backfillDurations("x", nil, st); err != nil {
		t.Fatalf("nil info: %v", err)
	}
	if err := backfillDurations("x", &fmp4InitInfo{movieTS: 1000}, NewState()); err != nil {
		t.Fatalf("无累计结束时间应跳过: %v", err)
	}
	info := &fmp4InitInfo{
		movieTS: 1000, trackTS: map[uint32]uint32{1: 90000},
		mehdOff: 8, mvhdOff: 8,
	}
	if err := backfillDurations(filepath.Join(t.TempDir(), "nonexist.mp4"), info, st); err == nil {
		t.Fatal("不存在的文件应返回打开错误")
	}
	// 偏移处不是 mehd/mvhd：校验失败跳过（不报错、不破坏文件）
	path := filepath.Join(t.TempDir(), "zeros.bin")
	if err := os.WriteFile(path, make([]byte, 64), 0644); err != nil {
		t.Fatal(err)
	}
	if err := backfillDurations(path, info, st); err != nil {
		t.Fatalf("位置校验失败应跳过而非报错: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if boxTypeAt(f, 1000, "mehd") || boxTypeAt(f, 0, "mehd") {
		t.Fatal("boxTypeAt 越界/类型不符应返回 false")
	}
}

func TestBackfillWideMvhd(t *testing.T) {
	// v1 mvhd（64 位时间戳形态）：回填偏移按 wide 布局（盒头 8 + verflags 4 + 双时间戳 16 + timescale 4）
	p := make([]byte, 108)
	binary.BigEndian.PutUint32(p[16:], 1000)
	mvhd1 := fullBox4("mvhd", 1<<24, p)
	path := filepath.Join(t.TempDir(), "wide.bin")
	if err := os.WriteFile(path, mvhd1, 0644); err != nil {
		t.Fatal(err)
	}
	st := NewState()
	st.end[1] = 90000
	info := &fmp4InitInfo{
		movieTS: 1000, trackTS: map[uint32]uint32{1: 90000},
		mvhdOff: 0, mvhdWide: true,
	}
	if err := backfillDurations(path, info, st); err != nil {
		t.Fatalf("wide mvhd 回填: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b8 [8]byte
	if _, err := f.ReadAt(b8[:], 32); err != nil {
		t.Fatal(err)
	}
	if v := binary.BigEndian.Uint64(b8[:]); v != 1000 {
		t.Fatalf("wide 回填=%d want 1000", v)
	}
}

func TestWriteDurationTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dur.bin")
	if err := os.WriteFile(path, make([]byte, 32), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// 非 wide 且超 32 位：截断为 MaxUint32
	if err := writeDuration(f, 0, 1<<40, false); err != nil {
		t.Fatal(err)
	}
	var b4 [4]byte
	if _, err := f.ReadAt(b4[:], 0); err != nil {
		t.Fatal(err)
	}
	if v := binary.BigEndian.Uint32(b4[:]); v != math.MaxUint32 {
		t.Fatalf("截断写入=%d want %d", v, uint32(math.MaxUint32))
	}
	// wide：完整 64 位
	if err := writeDuration(f, 8, 1<<40, true); err != nil {
		t.Fatal(err)
	}
	var b8 [8]byte
	if _, err := f.ReadAt(b8[:], 8); err != nil {
		t.Fatal(err)
	}
	if v := binary.BigEndian.Uint64(b8[:]); v != 1<<40 {
		t.Fatalf("wide 写入=%d want %d", v, uint64(1<<40))
	}
}

// ---- Restore 有效 init 恢复 ----

func TestRestoreValidInit(t *testing.T) {
	st := NewState()
	st.consumeInit(buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000}))
	st2 := NewState()
	st2.Restore(st.Snapshot())
	if st2.init == nil {
		t.Fatal("Restore 应恢复 init 解析结果")
	}
	if st2.init.movieTS != 1000 || st2.init.mehdOff < 0 || st2.init.trackTS[1] != 90000 {
		t.Fatalf("init 恢复不完整: %+v", st2.init)
	}
	// trackVideo 一并恢复：track1=hvc1（视频）、track2=mp4a（音频）
	if !st2.isVideoTrack(1) || st2.isVideoTrack(2) {
		t.Fatal("trackVideo 恢复错误")
	}
	st2.restoreEnd(nil) // 空数据：无操作
}

// ---- tfhd / trun flag 组合 ----

// tfhdFullBox 全 flag 形态 tfhd（base_data_offset/sample_desc/默认时长/大小/标志）。
func tfhdFullBox(trackID uint32) []byte {
	p := make([]byte, 28)
	binary.BigEndian.PutUint32(p[0:], trackID)
	binary.BigEndian.PutUint64(p[4:], 12345) // base_data_offset
	binary.BigEndian.PutUint32(p[12:], 2)   // sample_description_index
	binary.BigEndian.PutUint32(p[16:], 3000) // default_sample_duration
	binary.BigEndian.PutUint32(p[20:], 100)  // default_sample_size
	binary.BigEndian.PutUint32(p[24:], 0)    // default_sample_flags
	return fullBox4("tfhd", 0x02003b, p)
}

// trunFullBox 全 flag 形态 trun（first_sample_flags + 逐样本时长/大小/标志/cto）。
func trunFullBox(n int, dataOff int32) []byte {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p[0:], uint32(n))
	binary.BigEndian.PutUint32(p[4:], uint32(dataOff))
	binary.BigEndian.PutUint32(p[8:], 0x01010000)
	for i := 0; i < n; i++ {
		p = append(p, 0, 0, 0, 10) // duration
		p = append(p, 0, 0, 0, 8)  // size
		p = append(p, 0, 0, 0, 2)  // flags
		p = append(p, 0, 0, 0, 0)  // cto
	}
	return fullBox4("trun", 0xf05, p)
}

// trunBareBox 最小 trun（仅 data_offset）：时长/大小均退回 tfhd 默认值。
func trunBareBox(n int, dataOff int32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[0:], uint32(n))
	binary.BigEndian.PutUint32(p[4:], uint32(dataOff))
	return fullBox4("trun", 0x1, p)
}

func TestParseTrafFlagVariants(t *testing.T) {
	mfhd := fullBox4("mfhd", 0, []byte{0, 0, 0, 1})
	tfdt := tfdtBox(500)
	traf1 := box4("traf", append(append(append([]byte{}, tfhdFullBox(1)...), tfdt...), trunFullBox(2, 400)...))
	traf2 := box4("traf", append(append([]byte{}, tfhdFullBox(2)...), trunBareBox(1, 500)...))
	moof := box4("moof", append(append(append([]byte{}, mfhd...), traf1...), traf2...))

	infos, err := parseMoof(moof, 0)
	if err != nil {
		t.Fatalf("parseMoof: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("traf=%d want 2", len(infos))
	}
	v, a := infos[0], infos[1]
	if v.trackID != 1 || v.tfdtVal != 500 || v.tfdtWide {
		t.Fatalf("视频轨解析: %+v", v)
	}
	if len(v.sizes) != 2 || v.sizes[0] != 8 || v.sizes[1] != 8 {
		t.Fatalf("全 flag trun sizes=%v want [8 8]", v.sizes)
	}
	if !v.durSet || v.durTotal != 20 {
		t.Fatalf("全 flag trun durTotal=%d want 20", v.durTotal)
	}
	if len(a.sizes) != 1 || a.sizes[0] != 100 {
		t.Fatalf("bare trun sizes=%v want [100]（tfhd 默认大小）", a.sizes)
	}
	if !a.durSet || a.durTotal != 3000 {
		t.Fatalf("bare trun durTotal=%d want 3000（tfhd 默认时长）", a.durTotal)
	}
}

func TestParseTrafErrors(t *testing.T) {
	// traf 无 tfhd：解析错误
	noTfhd := box4("traf", trunFullBox(1, 0))
	if _, err := parseMoof(box4("moof", noTfhd), 0); err == nil {
		t.Fatal("无 tfhd 的 traf 应报错")
	}
	// 子盒声明大小越过 traf 边界：扫描中断（等价缺 tfhd）
	bigChild := make([]byte, 12)
	binary.BigEndian.PutUint32(bigChild, 60)
	copy(bigChild[4:8], "tfhd")
	pad := box4("free", make([]byte, 80))
	if _, err := parseMoof(box4("moof", append(append([]byte{}, box4("traf", bigChild)...), pad...)), 0); err == nil {
		t.Fatal("子盒跨 traf 边界应报错")
	}
	// moof 内畸形子盒：静默中断返回空列表
	infos, err := parseMoof(box4("moof", []byte{0, 0, 0, 4, 1, 2, 3, 4}), 0)
	if err != nil || len(infos) != 0 {
		t.Fatalf("畸形 moof: infos=%d err=%v", len(infos), err)
	}
	// patchMoov：insert=false 且 mvex 无 mehd 时不改数据
	init := buildInit(1000, map[uint32]uint32{1: 90000})
	moov := init[len(box4("ftyp", []byte("isom"))):]
	if out, off, wide := patchMoov(moov, false); off != -1 || wide || !bytes.Equal(out, moov) {
		t.Fatalf("insert=false: off=%d wide=%v", off, wide)
	}
	// patchMoov：mvex 内畸形子盒中断扫描后仍可插入
	trex := fullBox4("trex", 0, make([]byte, 24))
	mvexBad := box4("mvex", append(append([]byte{}, trex...), 0, 0, 0, 4, 1, 2, 3, 4))
	moovBad := box4("moov", append(box4("mvhd", make([]byte, 100)), mvexBad...))
	out, off, wide := patchMoov(moovBad, true)
	if off < 0 || !wide || string(out[off+4:off+8]) != "mehd" {
		t.Fatalf("畸形 mvex 内插入: off=%d wide=%v", off, wide)
	}
}

// ---- normalizeTraf 直连 ----

func TestNormalizeTrafVariants(t *testing.T) {
	st := NewState()
	// v1 tfdt：64 位归一化写回
	buf := make([]byte, 16)
	ti := &trafInfo{trackID: 1, tfdtOff: 8, tfdtWide: true, tfdtVal: 1000, durTotal: 500}
	normalizeTraf(buf, ti, st)
	if v := binary.BigEndian.Uint64(buf[8:]); v != 0 {
		t.Fatalf("v1 tfdt 归一化=%d want 0", v)
	}
	// 无 tfdt 的 traf：直接返回不动字节
	normalizeTraf(buf, &trafInfo{tfdtOff: -1}, st)
	if v := binary.BigEndian.Uint64(buf[8:]); v != 0 {
		t.Fatalf("无 tfdt 不应改写字节: %d", v)
	}
}

// ---- 样本封装判定边界 ----

func TestSampleClassifyEdge(t *testing.T) {
	if got := classifySample(nil); got != modeUnknown {
		t.Fatalf("空样本=%v want Unknown", got)
	}
	if startsWithStartCode([]byte{0, 0}) {
		t.Fatal("过短样本不应判定为 start code 开头")
	}
	// Unknown 样本原样返回
	raw := []byte{0xff, 0xf1}
	if out, err := sampleToAVCC(raw, modeUnknown); err != nil || !bytes.Equal(out, raw) {
		t.Fatalf("Unknown 样本应原样返回: %x %v", out, err)
	}
	// Annex-B 内 00 00 后跟非 0 非 1 字节：不构成分界，NAL 保持完整
	nals := splitAnnexB([]byte{0, 0, 1, 0x65, 0, 0, 5, 0x65})
	if len(nals) != 1 || !bytes.Equal(nals[0], []byte{0x65, 0, 0, 5, 0x65}) {
		t.Fatalf("splitAnnexB=%v", nals)
	}
	// 只含 start code 无 NAL：报错
	if _, err := annexBToAVCC([]byte{0, 0, 1}); err == nil {
		t.Fatal("无 NAL 的 Annex-B 应报错")
	}
}

// ---- 分片结构变体与异常放行 ----

func TestNormalizeSegmentStructureVariants(t *testing.T) {
	seg := buildSegment(1000, 1012, false) // AVCC（无样本转换干扰）
	moof := firstMoof(seg)
	mdat := seg[len(moof):]
	free := box4("free", nil)

	// (a) 连续两个 moof：第二个触发第一个 flush；第二个的 data_offset 恰好指向 mdat
	st := NewState()
	out, err := st.Normalize(append(append(append([]byte{}, moof...), moof...), mdat...))
	if err != nil {
		t.Fatalf("连续 moof: %v", err)
	}
	if n := len(collectMoofs(t, out)); n != 2 {
		t.Fatalf("连续 moof 输出 moof 数=%d want 2", n)
	}

	// (b) moof 与 mdat 之间夹 free：free 触发 flush，mdat 无 pending 原样写
	st2 := NewState()
	in := append(append(append([]byte{}, moof...), free...), mdat...)
	out, err = st2.Normalize(in)
	if err != nil {
		t.Fatalf("moof+free+mdat: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("moof+free+mdat 长度=%d want %d", len(out), len(in))
	}

	// (c) 尾部只有 moof（无 mdat）：最终 flush
	st3 := NewState()
	out, err = st3.Normalize(append([]byte{}, moof...))
	if err != nil {
		t.Fatalf("尾部 moof: %v", err)
	}
	if len(out) != len(moof) {
		t.Fatalf("尾部 moof 长度=%d want %d", len(out), len(moof))
	}

	// (d) moof 解析失败：原样放行不报错
	st4 := NewState()
	bad := box4("moof", box4("traf", trunFullBox(1, 0))) // 无 tfhd
	out, err = st4.Normalize(bad)
	if err != nil {
		t.Fatalf("解析失败应原样放行: %v", err)
	}
	if !bytes.Equal(out, bad) {
		t.Fatal("解析失败的分片应原样输出")
	}
}

func TestNormalizeSegmentConversionFailure(t *testing.T) {
	// Annex-B 样本只含 start code 无 NAL：转换失败 → 保留归一化 moof + 原 mdat
	seg := buildSegmentNA(1000, 1012, []byte{0, 0, 1}, []byte{0, 0, 1}, []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0x11})
	st := NewState()
	out, err := st.Normalize(seg)
	if err != nil {
		t.Fatalf("转换失败不应上抛错误: %v", err)
	}
	if len(out) != len(seg) {
		t.Fatalf("转换失败应保持长度: %d want %d", len(out), len(seg))
	}
}

// buildTwoTrackSeg 构造双轨分片；vSize/aSize 伪造样本大小、aDataOff 伪造音频偏移，
// 用于触发 rebuildMdat 的越界/重叠检查。返回分片与视频样本偏移 offV。
func buildTwoTrackSeg(vSize, aSize uint32, aDataOff int32) ([]byte, int32) {
	mfhd := fullBox4("mfhd", 0, []byte{0, 0, 0, 1})
	tfhdV, tfhdA := tfhdBox(1), tfhdBox(2)
	tfdtV, tfdtA := tfdtBox(1000), tfdtBox(1012)
	v0 := []byte{0, 0, 0, 4, 0x65, 1, 2, 3}
	a0 := []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0x11}
	trafV := box4("traf", append(append(append([]byte{}, tfhdV...), tfdtV...), trunBox([]uint32{vSize}, 0)...))
	trafA := box4("traf", append(append(append([]byte{}, tfhdA...), tfdtA...), trunBox([]uint32{aSize}, 0)...))
	moofB := box4("moof", append(append(append([]byte{}, mfhd...), trafV...), trafA...))
	mdatB := box4("mdat", append(append([]byte{}, v0...), a0...))
	offV := int32(len(moofB) + 8)
	trunVStart := 8 + len(mfhd) + 8 + len(tfhdV) + len(tfdtV)
	binary.BigEndian.PutUint32(moofB[trunVStart+16:], uint32(offV))
	trunAStart := 8 + len(mfhd) + len(trafV) + 8 + len(tfhdA) + len(tfdtA)
	binary.BigEndian.PutUint32(moofB[trunAStart+16:], uint32(aDataOff))
	return append(moofB, mdatB...), offV
}

func TestRebuildMdatBoundsErrors(t *testing.T) {
	// 正常基线：双轨样本各归其位，不报错
	// 音频 data_offset 需指向视频样本之后：先取 offV 再重建一次正确值
	_, offV := buildTwoTrackSeg(8, 6, 0)
	segOK, _ := buildTwoTrackSeg(8, 6, offV+8)
	if out, err := NewState().Normalize(segOK); err != nil || len(out) == 0 {
		t.Fatalf("正常双轨分片: %v", err)
	}

	// 音频样本区间与视频重叠：rebuildMdat 报错（原样放行，不返回错误）
	segOverlap, _ := buildTwoTrackSeg(8, 6, offV)
	if out, err := NewState().Normalize(segOverlap); err != nil || len(out) != len(segOverlap) {
		t.Fatalf("重叠分片应原样放行: %v", err)
	}
	// 音频 data_offset 越过 payload 末尾
	segFar, _ := buildTwoTrackSeg(8, 6, offV+1000)
	if out, err := NewState().Normalize(segFar); err != nil || len(out) != len(segFar) {
		t.Fatalf("越界分片应原样放行: %v", err)
	}
	// 音频声明大小超过 payload
	segAudioOOB, _ := buildTwoTrackSeg(8, 100, offV+8)
	if out, err := NewState().Normalize(segAudioOOB); err != nil || len(out) != len(segAudioOOB) {
		t.Fatalf("音频越界分片应原样放行: %v", err)
	}
	// 视频声明大小超过 payload
	segVideoOOB, _ := buildTwoTrackSeg(100, 6, offV+8)
	if out, err := NewState().Normalize(segVideoOOB); err != nil || len(out) != len(segVideoOOB) {
		t.Fatalf("视频越界分片应原样放行: %v", err)
	}
}

// ---- 扩展大小（64 位）mdat 的解析与重建 ----

func TestNormalizeExtendedMdat(t *testing.T) {
	sample := []byte{0, 0, 1, 0x65, 0x11, 0x22, 0x33} // Annex-B（触发转换）
	mfhd := fullBox4("mfhd", 0, []byte{0, 0, 0, 1})
	tfhd := tfhdBox(1)
	tfdt := tfdtBox(1000)
	traf := box4("traf", append(append(append([]byte{}, tfhd...), tfdt...), trunBox([]uint32{uint32(len(sample))}, 0)...))
	moofB := box4("moof", append(append([]byte{}, mfhd...), traf...))

	// 64 位扩展大小 mdat（16 字节头）
	extMdat := make([]byte, 16+len(sample))
	binary.BigEndian.PutUint32(extMdat, 1)
	copy(extMdat[4:8], "mdat")
	binary.BigEndian.PutUint64(extMdat[8:], uint64(16+len(sample)))
	copy(extMdat[16:], sample)

	// data_offset 指向扩展头之后的 payload
	offV := int32(len(moofB) + 16)
	trunStart := 8 + len(mfhd) + 8 + len(tfhd) + len(tfdt)
	binary.BigEndian.PutUint32(moofB[trunStart+16:], uint32(offV))

	out, err := NewState().Normalize(append(moofB, extMdat...))
	if err != nil {
		t.Fatalf("扩展大小 mdat: %v", err)
	}
	// 输出 mdat 仍为扩展头形态，payload 为转换后的 AVCC 样本（4+4=8 字节）
	sz, payload, typ, ok := boxHeader(out, len(moofB))
	if !ok || typ != "mdat" || sz != 16+8 || payload != len(moofB)+16 {
		t.Fatalf("重建后 mdat: sz=%d payload=%d typ=%q", sz, payload, typ)
	}
	want := []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33}
	if !bytes.Equal(out[payload:payload+8], want) {
		t.Fatalf("重建后样本=%x want %x", out[payload:payload+8], want)
	}
}
