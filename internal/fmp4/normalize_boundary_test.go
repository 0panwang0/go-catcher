// fMP4 解析边界回归测试（P1-2）：tfhd/tfdt/trun 的字段是按 flag 位"声明存在"的
// 可选字段流，box 声明长度与实际字段布局不符时，裸读有两种后果——
// 越出缓冲直接 panic（带走整个进程），或读到紧邻 box 的字节当字段值（静默错值）。
//
// 夹具直接复原第六轮评审探针的 48 字节畸形 moof，并补一个"不 panic 但静默读错值"
// 的变体：后者不会崩，反而更危险，必须有测试钉住。
package fmp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// malformedTrunMoof 48 字节畸形 moof（第六轮评审 P1-2 探针夹具）：
// trun 的 box 头声明 flags=0x201（data_offset + 逐样本 size）、sample_count=3，
// 但 box 只有 8 字节 payload —— 字段区根本不存在。
// 布局：moof 头(8) + traf 头(8) + tfhd(16) + trun(16) = 48。
func malformedTrunMoof() []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, 3) // sample_count=3：声明要读 3 个样本的字段
	trun := fullBox4("trun", 0x201, p)
	traf := box4("traf", append(append([]byte{}, tfhdBox(1)...), trun...))
	return box4("moof", traf)
}

// shortTfhdMoof 另一种畸形：tfhd 声明 default_sample_duration 位（flag 0x8），
// 但 box 只有 verflags + track_ID 共 8 字节 payload。
// box 之后还紧跟着一个 trun，所以裸读**不会 panic** —— 它会读到 trun 的 box 长度
// 当"默认样本时长"，是本项目最忌讳的那种"产物坏了但日志正常"。
func shortTfhdMoof() []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, 1)
	tfhd := fullBox4("tfhd", 0x020008, p) // 声明 default_sample_duration 存在
	traf := box4("traf", append(append([]byte{}, tfhd...), trunBox([]uint32{8}, 0)...))
	return box4("moof", traf)
}

// TestParseTrafTruncatedFieldsNoPanic 字段区截断：必须返回 error，不得 panic。
func TestParseTrafTruncatedFieldsNoPanic(t *testing.T) {
	moof := malformedTrunMoof()
	if len(moof) != 48 {
		t.Fatalf("夹具长度=%d want 48（与评审探针一致）", len(moof))
	}
	if _, err := parseMoof(moof, 0); err == nil {
		t.Fatal("trun 声明 3 个样本却无字段区：应返回 error，不能按 0 个样本蒙混过去")
	}
}

// TestParseTrafShortBoxNoSilentRead 声明了字段、box 里却没有：必须报错，
// 不能静默读到下一个 box 的字节。
func TestParseTrafShortBoxNoSilentRead(t *testing.T) {
	if _, err := parseMoof(shortTfhdMoof(), 0); err == nil {
		t.Fatal("tfhd 声明 default_sample_duration 却无该字段：应返回 error，不能读下一个 box 的字节")
	}
}

// TestNormalizeMalformedInputPassesThrough 兑现 normalize.go 顶部契约：
// 结构异常的数据原样返回（不报错、不 panic）——畸形输入绝不能让整条下载管线崩掉。
func TestNormalizeMalformedInputPassesThrough(t *testing.T) {
	for name, in := range map[string][]byte{
		"trun 字段区截断":   malformedTrunMoof(),
		"tfhd 声明与实际不符": shortTfhdMoof(),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := NewState().Normalize(in)
			if err != nil {
				t.Fatalf("不应上抛错误: %v", err)
			}
			if !bytes.Equal(out, in) {
				t.Fatalf("畸形输入应原样放行：%d 字节 → %d 字节", len(in), len(out))
			}
		})
	}
}

// TestNormalizeInitTruncatedNoPanic init 段路径（pipeline 里 writeInitSegmentFor 走的就是它）
// 同样不得 panic：这一段以前没有任何 recover，一段畸形 init 段就是进程崩溃。
func TestNormalizeInitTruncatedNoPanic(t *testing.T) {
	init := buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000})
	// 逐字节截断 + 逐段砍尾：任何前缀都不许 panic，且要么原样、要么是合法重建。
	for n := 0; n <= len(init); n++ {
		st := NewState()
		if _, err := st.Normalize(append([]byte{}, init[:n]...)); err != nil {
			t.Fatalf("前 %d 字节的 init 段不应上抛错误: %v", n, err)
		}
	}
	// 尾部截断的 init（moov 声明长度大于实际数据）
	trunc := append([]byte{}, init...)
	tail := len(trunc) - 24
	binary.BigEndian.PutUint32(trunc[0:], uint32(len(trunc)+40)) // moov 声明比实际长
	st := NewState()
	if _, err := st.Normalize(append(trunc[:tail], trunc[tail:]...)); err != nil {
		t.Fatalf("声明长度越界的 init 段不应上抛错误: %v", err)
	}
}

// TestParseTrafAbsurdSampleCount fuzz 30 秒实测撞出来的形态：trun 不带逐样本
// 字段（flags=0，合法形态）却声明 sample_count=2,751,463,424 —— 逐样本循环会跑
// 27 亿次把进程拖死（fuzz worker 报 "hung or terminated unexpectedly"）。
// 这种"声明值荒谬"的输入必须被上限挡下。
func TestParseTrafAbsurdSampleCount(t *testing.T) {
	mfhd := fullBox4("mfhd", 0, []byte{0, 0, 0, 1})
	tfhd := tfhdBox(1)
	tfdt := tfdtBox(0x3e8)
	p := make([]byte, 12) // verflags + sample_count + 4 字节残留（box 声明 28 字节）
	binary.BigEndian.PutUint32(p, 2751463424)
	trun := fullBox4("trun", 0, p)
	traf := box4("traf", append(append(append([]byte{}, tfhd...), tfdt...), trun...))
	moof := box4("moof", append(mfhd, traf...))

	if _, err := parseMoof(moof, 0); err == nil {
		t.Fatal("sample_count=2.7e9 应被上限拒绝，不能真去跑循环")
	}
	// 同一输入走公开入口：原样放行、不报错、不卡死
	out, err := NewState().Normalize(moof)
	if err != nil {
		t.Fatalf("Normalize 不应上抛错误: %v", err)
	}
	if !bytes.Equal(out, moof) {
		t.Fatal("结构异常的分片应原样放行")
	}
}

// TestParseTrafFieldsDoNotFit 逐样本字段总数装不下时立即报错
// （而不是逐项读到越界才失败，也不许把"装不下"当"刚好读完"）。
func TestParseTrafFieldsDoNotFit(t *testing.T) {
	tfhd := tfhdBox(1)
	p := make([]byte, 12) // verflags + sample_count + data_offset + 仅 1 个 size 字段
	binary.BigEndian.PutUint32(p, 2)
	trun := fullBox4("trun", 0x201, p) // 声明 2 个样本各带 size，字段区只够 1 个
	traf := box4("traf", append(append([]byte{}, tfhd...), trun...))
	if _, err := parseMoof(box4("moof", traf), 0); err == nil {
		t.Fatal("样本字段区不足应报错")
	}
}

// TestPatchMoovTruncatedMehd init 段里的畸形 mehd（声明长度装不下 version/duration
// 字段）必须被当成"没有可回填位置"：读越界会 panic，猜偏移会把时长写进后面的 box。
// 合法的 v0 mehd 是 16 字节（头 8 + verflags 4 + duration 4），必须照常接受。
func TestPatchMoovTruncatedMehdNoPanic(t *testing.T) {
	build := func(mehdSize uint32) []byte {
		bad := make([]byte, 8)
		binary.BigEndian.PutUint32(bad, mehdSize)
		copy(bad[4:], "mehd")
		// 后面紧跟 trex：修复前 moov[mehdOff+8] 会读到 trex 的长度字节当 version
		mvex := box4("mvex", append(append([]byte{}, bad...), fullBox4("trex", 0, make([]byte, 24))...))
		return box4("moov", append(box4("mvhd", make([]byte, 100)), mvex...))
	}

	for name, mehdSize := range map[string]uint32{"长度 8（无字段）": 8, "长度 12（缺 duration）": 12} {
		t.Run(name, func(t *testing.T) {
			moov := build(mehdSize)
			out, off, wide := patchMoov(moov, true)
			if off != -1 || wide {
				t.Fatalf("畸形 mehd(size=%d) 应视为不可回填: off=%d wide=%v", mehdSize, off, wide)
			}
			if !bytes.Equal(out, moov) {
				t.Fatal("不可回填时不应改动 moov 字节")
			}
		})
	}

	t.Run("合法 v0 mehd 仍可回填", func(t *testing.T) {
		moov := build(16)
		_, off, wide := patchMoov(moov, true)
		if off < 0 || wide {
			t.Fatalf("合法 mehd 应可回填: off=%d wide=%v", off, wide)
		}
	})
}

// TestBackfillSkipsShortBox 回填前必须确认盒声明长度装得下 duration 字段，
// 否则会写到 box 外面，破坏后续字节。这里用一个"声明长度只有 12 字节的 mehd"探它。
func TestBackfillSkipsShortBox(t *testing.T) {
	st := NewState()
	st.end[1] = 90000
	info := &fmp4InitInfo{movieTS: 1000, trackTS: map[uint32]uint32{1: 90000}, mehdOff: 0}

	bad := make([]byte, 12) // 声明 12 字节：装得下 verflags，装不下 duration
	binary.BigEndian.PutUint32(bad, 12)
	copy(bad[4:], "mehd")
	path := filepath.Join(t.TempDir(), "short-mehd.bin")
	if err := os.WriteFile(path, append(bad, 0xAA, 0xBB, 0xCC, 0xDD), 0644); err != nil {
		t.Fatal(err)
	}
	if err := backfillDurations(path, info, st); err != nil {
		t.Fatalf("短盒应跳过而非报错: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(bad, 0xAA, 0xBB, 0xCC, 0xDD)) {
		t.Fatalf("盒外字节被改写: %x", got)
	}
}

// FuzzNormalize 模糊测试：任何字节序列都不得让解析层 panic。
//
// 直接打 parseMoof / prepareInit（**不带** Normalize 入口的 recover），
// 这样边界助手本身的漏洞才暴露得出来；若只 fuzz Normalize，入口兜底会把
// 恐慌吞掉，fuzz 就永远发现不了回归。
//
// 不进质量门：`go test` 只跑下面的种子语料，需要时手跑
// `go test ./internal/fmp4/ -run=^$ -fuzz=FuzzNormalize -fuzztime=30s`。
func FuzzNormalize(f *testing.F) {
	f.Add(malformedTrunMoof())
	f.Add(shortTfhdMoof())
	f.Add(buildSegment(7792000, 7792128, true))
	f.Add(buildSegment(1000, 1012, false))
	f.Add(buildInit(1000, map[uint32]uint32{1: 90000, 2: 48000}))
	f.Add([]byte{0, 0, 0, 1, 'm', 'o', 'o', 'f'})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		pos := 0
		for pos+8 <= len(data) {
			sz, _, typ, ok := boxHeader(data, pos)
			if !ok {
				break
			}
			switch typ {
			case "moof":
				parseMoof(data[pos:pos+sz], pos) //nolint:errcheck // 错误是允许的结果，panic 才是缺陷
			case "moov":
				prepareInit(data[pos:pos+sz], true)
			}
			pos += sz
		}
		st := NewState()
		if _, err := st.Normalize(data); err != nil {
			t.Fatalf("Normalize 不应上抛错误（异常数据原样放行）: %v", err)
		}
	})
}
