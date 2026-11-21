// B-1 / B-2 / B-3：moof 内 data_offset 的**基准语义**、trun 形态与样本数上限。
//
// 三条都属头号形态（产物坏、日志全绿），触发源都是不可信的远端字节。
package fmp4

import (
	"encoding/binary"
	"testing"
)

func u32b(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func catB(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// trunBoxRaw 构造 flags 可自定义的 trun（用于造"不带 data_offset"等形态）。
func trunBoxRaw(flags uint32, nc uint32, dataOff int32, sizes []uint32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p, nc)
	if flags&0x1 != 0 {
		binary.BigEndian.PutUint32(p[4:], uint32(dataOff))
	}
	for _, s := range sizes {
		p = append(p, u32b(s)...)
	}
	return fullBox4("trun", flags, p)
}

// readTrunDataOff 读第 idx 个 traf 的 trun data_offset（按 infos 记录的偏移）。
func readTrunDataOff(t *testing.T, infos []*trafInfo, out []byte, idx int) int32 {
	t.Helper()
	if idx >= len(infos) || infos[idx].dataOffOff < 0 {
		t.Fatalf("第 %d 个 traf 没有 data_offset（infos=%d）", idx, len(infos))
	}
	return int32(binary.BigEndian.Uint32(out[infos[idx].dataOffOff:]))
}

// TestBuildMoofShiftsOnlyMoofBasedOffsets（B-1）：
// 先行轨变大后，**其后**只有"基准 = moof 起点"的轨才该平移 data_offset。
//
// 缺陷形态：buildMoof 原先对**所有**带 data_offset 的轨一律平移，而 parseTraf 的
// base 有两种语义（moof 起点 / 上一轨数据末尾）。对后者，基准本身会随前面轨的长度
// 一起移动，data_offset 是相对值、本该不动 —— 多平移一次 ⇒ 样本起点算错、音画错位，
// 而日志全绿（因为每一处"看起来"都成功了）。
//
// 夹具（三轨）：
//
//	轨1 首轨、被转换且样本变大 +200  → 自己的 offset 不动（自己的增量不移自己）
//	轨2 带 default-base-is-moof     → 前面变大 ⇒ 必须 +200
//	轨3 无标记且非首轨（基准=轨2末尾）→ 相对值不动 ⇒ 必须保持原值
func TestBuildMoofShiftsOnlyMoofBasedOffsets(t *testing.T) {
	mfhd := fullBox4("mfhd", 0, []byte{0, 0, 0, 1})

	// 轨 1：首轨（无标记也按 moof 起点），将被转换、样本变大。
	traf1 := box4("traf", catB(
		fullBox4("tfhd", 0, u32b(1)),
		tfdtBox(0),
		trunBoxRaw(0x201, 2, 100, []uint32{14, 8}),
	))
	// 轨 2：带 default-base-is-moof ⇒ base = moof 起点。
	traf2 := box4("traf", catB(
		fullBox4("tfhd", 0x020000, u32b(2)),
		tfdtBox(0),
		trunBoxRaw(0x201, 1, 1000, []uint32{6}),
	))
	// 轨 3：**不带**该标记、且不是首轨 ⇒ base = 上一轨数据末尾。
	traf3 := box4("traf", catB(
		fullBox4("tfhd", 0, u32b(3)),
		tfdtBox(0),
		trunBoxRaw(0x201, 1, 90, []uint32{6}),
	))
	moof := box4("moof", catB(mfhd, traf1, traf2, traf3))

	infos, err := parseMoof(moof, 0)
	if err != nil {
		t.Fatalf("parseMoof: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("traf 数=%d want 3", len(infos))
	}
	if !infos[0].baseIsMoof {
		t.Error("轨 1 是首轨，baseIsMoof 应为 true")
	}
	if !infos[1].baseIsMoof {
		t.Error("轨 2 有 default-base-is-moof，baseIsMoof 应为 true")
	}
	if infos[2].baseIsMoof {
		t.Error("轨 3 无该标记且非首轨，baseIsMoof 应为 false（否则会多平移一次）")
	}

	// 模拟轨 1 的样本在 Annex-B→AVCC 转换后变大：{14,8}(=22) → {114,108}(=222)，growth=+200
	out := buildMoof(&pendingMoof{orig: moof, infos: infos}, map[int][]uint32{0: {114, 108}})

	if got := readTrunDataOff(t, infos, out, 0); got != 100 {
		t.Errorf("轨 1 自己的增量不该平移自己的 data_offset，应保持 100，got %d", got)
	}
	if got := readTrunDataOff(t, infos, out, 1); got != 1200 {
		t.Errorf("轨 2（moof 基准）的 data_offset 应 +200 ⇒ 1200，got %d", got)
	}
	if got := readTrunDataOff(t, infos, out, 2); got != 90 {
		t.Errorf("轨 3（上一轨末尾基准）的 data_offset **不该**平移，应保持 90，got %d", got)
	}

	// 反向：growth 为 0 时三轨都不许动（别把"没变化"也平移了）
	out0 := buildMoof(&pendingMoof{orig: moof, infos: infos}, map[int][]uint32{0: {14, 8}})
	for i, want := range []int32{100, 1000, 90} {
		if got := readTrunDataOff(t, infos, out0, i); got != want {
			t.Errorf("样本大小没变时轨 %d 不该平移，应保持 %d，got %d", i, want, got)
		}
	}
}

// TestParseTrafRejectsSecondTrunWithDataOffset（B-2）：
// 同 traf 多个 trun 各自带 data_offset 时，后一个会覆盖前一个 ⇒ dataStart 指向错的
// 样本起点 ⇒ 产物错位。当前版本不支持这种形态，必须**显式拒绝**（宁缺勿假），
// 而不是猜一个。
func TestParseTrafRejectsSecondTrunWithDataOffset(t *testing.T) {
	tfhd := fullBox4("tfhd", 0x020000, u32b(1))

	// 两个 trun 都带 data_offset ⇒ 拒绝
	both := box4("moof", catB(
		fullBox4("mfhd", 0, []byte{0, 0, 0, 1}),
		box4("traf", catB(tfhd, tfdtBox(0),
			trunBoxRaw(0x201, 1, 100, []uint32{10}),
			trunBoxRaw(0x201, 1, 200, []uint32{20}),
		)),
	))
	if _, err := parseMoof(both, 0); err == nil {
		t.Fatal("同 traf 两个 trun 各带 data_offset 应显式拒绝（B-2）")
	}

	// 反向：第二个 trun **不带** data_offset 是合法形态，必须放行
	// （否则会把正常的"首 trun 给起点、后续 trun 紧随其后"整类流挡掉）
	ok := box4("moof", catB(
		fullBox4("mfhd", 0, []byte{0, 0, 0, 1}),
		box4("traf", catB(tfhd, tfdtBox(0),
			trunBoxRaw(0x201, 1, 100, []uint32{10}),
			trunBoxRaw(0x200, 1, 0, []uint32{20}),
		)),
	))
	if _, err := parseMoof(ok, 0); err != nil {
		t.Fatalf("只有一个 trun 带 data_offset 是合法形态，不该拒绝: %v", err)
	}
}

// TestParseTrafCapsSamplesAcrossTruns（B-3）：
// maxTrafSamples 必须按**整个 traf**卡。只卡单个 trun 时，"多个各声明 100 万样本的
// trun"能把逐样本 slice 一路撑到内存耗尽 —— 而 trun 不带逐样本字段是合法形态，
// 所以字节数卡不住它（这正是当初引入 maxTrafSamples 的原因）。
func TestParseTrafCapsSamplesAcrossTruns(t *testing.T) {
	// tfhd 不带 default-sample-size ⇒ 不分配 sizes，只累计 sampleCount（省内存）
	tfhd := fullBox4("tfhd", 0x020000, u32b(1))
	half := uint32(maxTrafSamples)

	one := box4("moof", catB(
		fullBox4("mfhd", 0, []byte{0, 0, 0, 1}),
		box4("traf", catB(tfhd, tfdtBox(0), trunBoxRaw(0, half, 0, nil))),
	))
	if _, err := parseMoof(one, 0); err != nil {
		t.Fatalf("单个 trun 恰好在上限上，不该拒绝: %v", err)
	}

	// 两个 trun 各 half ⇒ 累计 2×上限 ⇒ 必须拒绝
	two := box4("moof", catB(
		fullBox4("mfhd", 0, []byte{0, 0, 0, 1}),
		box4("traf", catB(tfhd, tfdtBox(0),
			trunBoxRaw(0, half, 0, nil),
			trunBoxRaw(0, half, 0, nil),
		)),
	))
	if _, err := parseMoof(two, 0); err == nil {
		t.Fatal("跨 trun 累计超过 maxTrafSamples 应拒绝（B-3）")
	}
}
