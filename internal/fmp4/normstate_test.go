// NormState 接口行为测试：状态工厂与接口方法（Normalize/Snapshot/Restore/Finish）、
// Restore 容错与并发安全。
// 全程只通过导出方法集操作（即 core.NormState 接口的方法集），
// 验证「状态即变换器」的架构。
package fmp4

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// firstMoof 提取数据中第一个 moof box（nil = 未找到）。
func firstMoof(data []byte) []byte {
	pos := 0
	for pos+8 <= len(data) {
		sz, _, typ, ok := boxHeader(data, pos)
		if !ok {
			break
		}
		if typ == "moof" {
			return data[pos : pos+sz]
		}
		pos += sz
	}
	return nil
}

// TestNormStateInterfaceRoundTrip 只通过接口驱动：工厂创建状态 → Normalize 处理分片
// → Snapshot 导出 → Restore 恢复 → 继续 Normalize，基准跨「断点续传」保持一致。
func TestNormStateInterfaceRoundTrip(t *testing.T) {
	st := NewState()
	if st == nil {
		t.Fatal("NewState 返回 nil")
	}
	seg0 := buildSegment(7792000, 7792128, true)
	out0, err := st.Normalize(seg0)
	if err != nil {
		t.Fatalf("接口 Normalize: %v", err)
	}
	moofs0 := collectMoofs(t, out0)
	infos0, _ := parseMoof(moofs0[0], 0)
	for i := range infos0 {
		if v := binary.BigEndian.Uint32(moofs0[0][infos0[i].tfdtOff:]); v != 0 {
			t.Fatalf("首分片 traf%d tfdt=%d want 0", i, v)
		}
	}

	// 断点续传：新状态仅凭 Snapshot 字节恢复
	st2 := NewState()
	st2.Restore(st.Snapshot())
	seg1 := buildSegment(7792000+96000, 7792128+96000, true)
	out1, err := st2.Normalize(seg1)
	if err != nil {
		t.Fatalf("Restore 后 Normalize: %v", err)
	}
	moofs1 := collectMoofs(t, out1)
	infos1, _ := parseMoof(moofs1[0], 0)
	if v := binary.BigEndian.Uint32(moofs1[0][infos1[0].tfdtOff:]); v != 96000 {
		t.Fatalf("续传后 tfdt=%d want 96000", v)
	}
	// 无 init 解析结果时 Finish 静默跳过（不触碰文件）
	if err := st2.Finish(filepath.Join(t.TempDir(), "nonexist.mp4")); err != nil {
		t.Fatalf("无 init 的 Finish 应返回 nil: %v", err)
	}
}

// TestNormStateRestoreTolerant Restore 对空/损坏/非法键/坏 init 一律静默忽略。
func TestNormStateRestoreTolerant(t *testing.T) {
	st := NewState()
	st.Restore(nil)                                     // 空字节
	st.Restore([]byte("{broken"))                       // 非 JSON
	st.Restore([]byte(`{"baseline":{"abc":1,"1":50}}`)) // 非法键忽略、合法键生效
	if st.baselineStrings()["1"] != 50 {
		t.Fatalf("非法键应忽略、合法键生效: %v", st.baselineStrings())
	}
	st.Restore([]byte(`{"baseline":{"1":60},"init":"not-an-object"}`)) // 坏 init 忽略
	if st.init != nil {
		t.Fatal("坏 init 字节不应被恢复")
	}
	// 容错后状态仍可正常使用：基线 60 生效，tfdt 1000 → 940
	seg := buildSegment(1000, 1012, true)
	out, err := st.Normalize(seg)
	if err != nil {
		t.Fatalf("容错后 Normalize: %v", err)
	}
	moofs := collectMoofs(t, out)
	infos, _ := parseMoof(moofs[0], 0)
	if v := binary.BigEndian.Uint32(moofs[0][infos[0].tfdtOff:]); v != 940 {
		t.Fatalf("容错后 tfdt=%d want 940", v)
	}
}

// TestNormStateSnapshotIncludesInit 持有 init 解析结果后，Snapshot 字节必须包含
// init 信息（persist 往返一致），确保续传后 Finish 仍能回填。
func TestNormStateSnapshotIncludesInit(t *testing.T) {
	raw := buildInit(1000, map[uint32]uint32{1: 90000}) // 原始 init：mvex 无 mehd
	st := NewState()
	nd := st.consumeInit(raw)
	if len(nd) != len(raw)+20 {
		t.Fatalf("consumeInit 应插入 mehd 占位: %d → %d", len(raw), len(nd))
	}
	// 插入位置校验：重解析（不插入）应找到 mehd
	_, info2 := prepareInit(nd, false)
	if info2.mehdOff < 0 {
		t.Fatal("consumeInit 未插入 mehd")
	}
	snap := st.Snapshot()
	var p normPersist
	if err := json.Unmarshal(snap, &p); err != nil {
		t.Fatalf("snapshot 非 JSON: %v", err)
	}
	if len(p.Init) == 0 {
		t.Fatal("snapshot 应包含 init 解析结果")
	}
	var pi persistFMP4InitInfo
	if err := json.Unmarshal(p.Init, &pi); err != nil || pi.MehdOff != info2.mehdOff {
		t.Fatalf("snapshot 内 init 不一致: err=%v mehdOff=%d want %d", err, pi.MehdOff, info2.mehdOff)
	}
}

// TestNormStateConcurrentNormalize 多分片并发 Normalize：同一状态共享基准，
// 各分片 tfdt 归一化互不干扰，结束时间并发累计一致（配合 -race 验证无数据竞争）。
func TestNormStateConcurrentNormalize(t *testing.T) {
	st := NewState()
	v0 := []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33}
	v1 := []byte{0, 0, 0, 5, 0x65, 0x44, 0x55, 0x66, 0x77}

	const workers = 8
	const segs = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < segs; k++ {
				tfdtV := uint64(7792000 + k*96000)
				tfdtA := uint64(7792128 + k*96000)
				seg := buildSegmentDur(tfdtV, tfdtA, v0, v1, 3000, 3000, 2048)
				out, err := st.Normalize(seg)
				if err != nil {
					errs <- err
					return
				}
				moof := firstMoof(out)
				if moof == nil {
					errs <- errors.New("输出中未找到 moof")
					return
				}
				infos, perr := parseMoof(moof, 0)
				if perr != nil {
					errs <- perr
					return
				}
				want := uint32(k * 96000)
				if v := binary.BigEndian.Uint32(moof[infos[0].tfdtOff:]); v != want {
					errs <- &segCheckError{track: 0, got: v, want: want}
					return
				}
				if v := binary.BigEndian.Uint32(moof[infos[1].tfdtOff:]); v != want {
					errs <- &segCheckError{track: 1, got: v, want: want}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// 结束时间取所有分片的最大相对结束时间：末段 tfdt=(segs-1)*96000 + 段内样本时长
	maxV := uint64(segs-1)*96000 + 6000
	maxA := uint64(segs-1)*96000 + 2048
	if e := st.endSnapshot(); e["1"] != maxV || e["2"] != maxA {
		t.Fatalf("并发累计 end=%v want {1:%d 2:%d}", e, maxV, maxA)
	}
}

// TestNormStatePassthrough 非 fMP4 / 结构异常数据经接口原样放行。
func TestNormStatePassthrough(t *testing.T) {
	st := NewState()
	for _, in := range [][]byte{
		nil,
		[]byte("ab"),                       // 过短
		[]byte("not a box at all........"), // 无合法 box 头
	} {
		out, err := st.Normalize(in)
		if err != nil {
			t.Fatalf("normalize(%q): %v", in, err)
		}
		if !bytes.Equal(out, in) {
			t.Fatalf("非 fMP4 数据被改写: %q → %q", in, out)
		}
	}
}

// segCheckError 并发测试中的 tfdt 校验错误。
type segCheckError struct {
	track int
	got   uint32
	want  uint32
}

func (e *segCheckError) Error() string {
	return "tfdt 校验失败"
}
