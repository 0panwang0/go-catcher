// fMP4 分片规范化：时间戳归一化 + 裸 NAL→AVCC 封装转换。
//
// 直播流（如 B 站）的 fMP4 分片有两个会导致播放黑屏的已知问题：
//  1. tfdt 携带绝对时间（直播已进行时长，如 7792 秒），拼接后首帧 PTS 高达
//     数千秒，播放器会一直等待从 0 开始、实际不存在的帧 → 黑屏。
//  2. mdat 内视频样本是 Annex-B 裸 NAL（start code 分隔），不符合 MP4 的
//     AVCC 规范（4 字节长度前缀），Windows Media Player 等解析失败。
//
// normalizeFMP4Segment 在分片写入前统一修正：把每轨首个分片的 tfdt 记为基准
// （跨分片共享 normState，断点续传沿用同一基准），所有 tfdt 减去基准归零；
// 并把裸 NAL 样本改写为长度前缀形式，同时重写 trun 样本大小与 mdat 尺寸。
//
// 该函数对任何结构异常/非 fMP4 数据一律原样放行，保证下载管线不受影响。
package fmp4

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// normState fMP4 规范化的跨分片状态：每轨道的 tfdt 基准偏移与累计结束时间。
// 同一任务的所有分片（含断点续传后的新分片）必须共用同一基准；
// 每轨结束时间（归一化 tfdt + 样本总时长）用于任务完成时回填 mehd 总时长。
// trackVideo 记录各轨是否为视频轨（来自 init 段 stsd）：仅视频轨做
// 裸 NAL→AVCC 转换，音频轨永不转换（AAC 帧头与 H.264 NAL 头存在位冲突，
// 特征猜测会误伤音频样本导致声音断续）。
// 状态同时承担数据变换（normalize）：实现 NormState 接口，pipeline 只面对接口。
type normState struct {
	mu         sync.Mutex
	baseline   map[uint32]uint64 // trackID -> 首个分片中该轨的 tfdt
	end        map[uint32]uint64 // trackID -> 该轨累计最大相对结束时间（track timescale 单位）
	trackVideo map[uint32]bool   // trackID -> 是否视频轨（未知轨道默认按视频处理）
	init       *fmp4InitInfo     // init 段解析结果（mehd/mvhd 回填位置；finish 收尾用）
}

// NewState 创建 fMP4 规范化状态（core 容器注册表的状态工厂）。
// 返回具体类型：core 侧经 func() NormState 包装装配进注册表，
// 编译期即验证方法集满足 core.NormState 接口。
func NewState() *normState {
	return &normState{
		baseline:   make(map[uint32]uint64),
		end:        make(map[uint32]uint64),
		trackVideo: make(map[uint32]bool),
	}
}

// normPersist snapshot/restore 的持久化字节格式（JSON）：全部跨分片状态。
// Init 字段复用 fmp4InitInfo.persist() 的字节。
type normPersist struct {
	Baseline map[string]uint64 `json:"baseline,omitempty"` // trackID -> tfdt 基准
	End      map[string]uint64 `json:"end,omitempty"`      // trackID -> 累计结束时间
	Init     json.RawMessage   `json:"init,omitempty"`     // init 段解析结果（mehd/mvhd 回填位置）
}

// Normalize 实现 NormState：处理一段写入前的数据（内联 init 或 fMP4 分片）。
// 内部直接访问具体字段，无需经过接口中转。
func (n *normState) Normalize(data []byte) ([]byte, error) {
	return normalizeFMP4Segment(data, n)
}

// Snapshot 导出全部跨分片状态（tfdt 基准 + 每轨结束时间 + init 段解析结果）。
func (n *normState) Snapshot() []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	p := normPersist{Baseline: n.baselineStrings(), End: n.endStrings()}
	if n.init != nil {
		if b, err := json.Marshal(n.init.persist()); err == nil {
			p.Init = b
		}
	}
	out, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return out
}

// Restore 载入持久化的全部状态（断点续传）；未知/损坏字节静默忽略。
func (n *normState) Restore(b []byte) {
	if len(b) == 0 {
		return
	}
	var p normPersist
	if err := json.Unmarshal(b, &p); err != nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for ks, v := range p.Baseline {
		if k, err := strconv.ParseUint(ks, 10, 32); err == nil {
			n.baseline[uint32(k)] = v
		}
	}
	for ks, v := range p.End {
		if k, err := strconv.ParseUint(ks, 10, 32); err == nil {
			n.end[uint32(k)] = v
		}
	}
	if len(p.Init) > 0 {
		var pi persistFMP4InitInfo
		if err := json.Unmarshal(p.Init, &pi); err == nil {
			info := &fmp4InitInfo{trackTS: map[uint32]uint32{}}
			info.restore(&pi)
			n.init = info
			if len(info.trackVideo) > 0 {
				n.trackVideo = info.trackVideo
			}
		}
	}
}

// baselineStrings 导出基准（持久化，续传恢复用）。
func (n *normState) baselineStrings() map[string]uint64 {
	out := make(map[string]uint64, len(n.baseline))
	for k, v := range n.baseline {
		out[strconv.FormatUint(uint64(k), 10)] = v
	}
	return out
}

// endStrings 导出各轨结束时间（持久化，续传后继续累计）。
func (n *normState) endStrings() map[string]uint64 {
	out := make(map[string]uint64, len(n.end))
	for k, v := range n.end {
		out[strconv.FormatUint(uint64(k), 10)] = v
	}
	return out
}

// endSnapshot 导出各轨结束时间（私有：mehd 回填换算与测试用）。
func (n *normState) endSnapshot() map[string]uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.endStrings()
}

// restoreEnd 载入持久化的各轨结束时间（私有：测试与内部恢复用）。
func (n *normState) restoreEnd(m map[string]uint64) {
	if len(m) == 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for ks, v := range m {
		if k, err := strconv.ParseUint(ks, 10, 32); err == nil {
			n.end[uint32(k)] = v
		}
	}
}

// isVideoTrack 判断轨道是否应做视频样本转换：
// 已知类型按类型；未知轨道（无 init 或非 fMP4）默认 true，保持旧行为。
func (n *normState) isVideoTrack(trackID uint32) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.trackVideo[trackID]
	if !ok {
		return true
	}
	return v
}

// normTfdt 按跨分片基准归一化 tfdt（首个分片记基准）并累计该轨结束时间，
// 返回归一化值。基准/结束时间跨分片共享（断点续传沿用同一基准），原子更新。
func (n *normState) normTfdt(id uint32, raw, dur uint64) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	var nv uint64
	if base, ok := n.baseline[id]; ok {
		if raw > base {
			nv = raw - base
		}
	} else {
		n.baseline[id] = raw
	}
	if dur > 0 {
		if end := nv + dur; end > n.end[id] {
			n.end[id] = end
		}
	}
	return nv
}

// trafInfo 一个 traf（单轨分片）的解析结果。
type trafInfo struct {
	trackID    uint32
	tfdtOff    int    // tfdt 值字段在 moof 内的偏移（-1 = 无 tfdt）
	tfdtVal    uint64 // 原始 baseMediaDecodeTime
	tfdtWide   bool   // tfdt version==1（64 位值）
	durTotal   uint64 // 本 traf 全部样本总时长（track timescale 单位；未知为 0）
	durSet     bool   // 是否从 trun/tfhd 读到了时长信息
	sampleCount int   // 本 traf 样本总数（跨多个 trun 累计）
	dataStart  int    // 本轨首个样本的绝对文件偏移
	dataOffOff int    // trun 内 data_offset 字段在 moof 内的偏移（-1 = 无该字段）
	sizes      []uint32
	sizeOffs   []int // trun 内逐样本 size 字段在 moof 内的偏移（空 = trun 无逐样本 size）
}

// boxHeader 解析 b[pos:] 处的一个 box 头，返回总大小、payload 偏移、类型。
func boxHeader(b []byte, pos int) (size, payload int, typ string, ok bool) {
	if pos+8 > len(b) {
		return 0, 0, "", false
	}
	sz := int(binary.BigEndian.Uint32(b[pos:]))
	typ = string(b[pos+4 : pos+8])
	hdr := 8
	switch sz {
	case 1:
		if pos+16 > len(b) {
			return 0, 0, "", false
		}
		sz = int(binary.BigEndian.Uint64(b[pos+8:]))
		hdr = 16
	case 0:
		sz = len(b) - pos
	}
	if sz < hdr || pos+sz > len(b) {
		return 0, 0, "", false
	}
	return sz, pos + hdr, typ, true
}

// parseMoof 解析 moof 内所有 traf（tfhd/tfdt/trun），计算每轨样本数据偏移。
// moofAbs 是 moof box 的绝对文件偏移（data_offset 以它为基准）。
func parseMoof(moof []byte, moofAbs int) ([]*trafInfo, error) {
	var infos []*trafInfo
	prevDataEnd := 0 // 前一轨数据末尾（绝对偏移），无 default-base-is-moof 时作 base
	pos := 8
	for pos+8 <= len(moof) {
		sz, _, typ, ok := boxHeader(moof, pos)
		if !ok {
			break
		}
		if typ == "traf" {
			ti, perr := parseTraf(moof, pos, sz, moofAbs, prevDataEnd, len(infos) == 0)
			if perr != nil {
				return nil, perr
			}
			infos = append(infos, ti)
			prevDataEnd = ti.dataStart + int(sum(ti.sizes))
		}
		pos += sz
	}
	return infos, nil
}

// parseTraf 解析一个 traf：tfhd（trackID/默认样本大小/时长/base 标记）、
// tfdt（baseMediaDecodeTime）、trun（样本表），并推算本轨首个样本的绝对偏移。
func parseTraf(moof []byte, pos, size int, moofAbs int, prevDataEnd int, firstTraf bool) (*trafInfo, error) {
	ti := &trafInfo{tfdtOff: -1, dataOffOff: -1}
	var defaultSampleSize uint32
	hasDefaultSize := false
	var defaultDur uint32
	hasDefaultDur := false
	var dataOff int32
	hasDataOff := false
	defaultBaseIsMoof := false
	hasTfhd := false

	end := pos + size
	q := pos + 8
	for q+8 <= end {
		sz, payload, typ, ok := boxHeader(moof, q)
		if !ok || q+sz > end {
			break
		}
		switch typ {
		case "tfhd":
			hasTfhd = true
			flags := binary.BigEndian.Uint32(moof[payload:]) & 0xFFFFFF
			defaultBaseIsMoof = flags&0x020000 != 0
			ti.trackID = binary.BigEndian.Uint32(moof[payload+4:])
			r := payload + 8
			if flags&0x1 != 0 {
				r += 8 // base_data_offset
			}
			if flags&0x2 != 0 {
				r += 4 // sample_description_index
			}
			if flags&0x8 != 0 { // default_sample_duration
				defaultDur = binary.BigEndian.Uint32(moof[r:])
				hasDefaultDur = true
				r += 4
			}
			if flags&0x10 != 0 { // default_sample_size
				defaultSampleSize = binary.BigEndian.Uint32(moof[r:])
				hasDefaultSize = true
				r += 4
			}
			if flags&0x20 != 0 {
				r += 4 // default_sample_flags
			}
		case "tfdt":
			ti.tfdtWide = moof[payload] == 1
			ti.tfdtOff = payload + 4
			if ti.tfdtWide {
				ti.tfdtVal = binary.BigEndian.Uint64(moof[payload+4:])
			} else {
				ti.tfdtVal = uint64(binary.BigEndian.Uint32(moof[payload+4:]))
			}
		case "trun":
			flags := binary.BigEndian.Uint32(moof[payload:]) & 0xFFFFFF
			nc := int(binary.BigEndian.Uint32(moof[payload+4:]))
			ti.sampleCount += nc
			r := payload + 8
			if flags&0x1 != 0 {
				dataOff = int32(binary.BigEndian.Uint32(moof[r:]))
				ti.dataOffOff = r
				hasDataOff = true
				r += 4
			}
			if flags&0x4 != 0 {
				r += 4 // first_sample_flags
			}
			// 逐样本字段按固定顺序交错出现（只含置位了的 flag）：
			// duration(0x100) → size(0x200) → flags(0x400) → cto(0x800)
			var dsum uint64
			for i := 0; i < nc; i++ {
				if flags&0x100 != 0 {
					dsum += uint64(binary.BigEndian.Uint32(moof[r:]))
					r += 4
				}
				if flags&0x200 != 0 {
					ti.sizes = append(ti.sizes, binary.BigEndian.Uint32(moof[r:]))
					ti.sizeOffs = append(ti.sizeOffs, r)
					r += 4
				}
				if flags&0x400 != 0 {
					r += 4 // sample_flags
				}
				if flags&0x800 != 0 {
					r += 4 // composition_time_offset
				}
			}
			if flags&0x100 != 0 {
				ti.durTotal += dsum
				ti.durSet = true
			}
			// trun 无逐样本 size 时退回 tfhd 的默认样本大小
			if flags&0x200 == 0 && hasDefaultSize {
				for i := 0; i < nc; i++ {
					ti.sizes = append(ti.sizes, defaultSampleSize)
				}
			}
		}
		q += sz
	}
	if !hasTfhd {
		return nil, errors.New("traf 缺少 tfhd")
	}
	// trun 无逐样本时长时退回 tfhd 的默认样本时长（样本数 × 默认时长）
	if !ti.durSet && hasDefaultDur && ti.sampleCount > 0 {
		ti.durTotal = uint64(ti.sampleCount) * uint64(defaultDur)
		ti.durSet = true
	}

	// base：default-base-is-moof 或第一轨 → moof 起点；否则上一轨数据末尾
	base := prevDataEnd
	if defaultBaseIsMoof || firstTraf {
		base = moofAbs
	}
	ti.dataStart = base
	if hasDataOff {
		ti.dataStart += int(dataOff)
	}
	return ti, nil
}

// normalizeTraf 用跨分片基准把 tfdt 归零并原位写回 moof 字节，
// 同时把该轨相对结束时间（归一化 tfdt + 样本总时长）累计进状态。
func normalizeTraf(moof []byte, ti *trafInfo, n *normState) {
	if ti.tfdtOff < 0 {
		return
	}
	nv := n.normTfdt(ti.trackID, ti.tfdtVal, ti.durTotal)
	if ti.tfdtWide {
		binary.BigEndian.PutUint64(moof[ti.tfdtOff:], nv)
	} else {
		binary.BigEndian.PutUint32(moof[ti.tfdtOff:], uint32(nv))
	}
}

// pendingMoof 等待与后续 mdat 配对处理的 moof（moof 必须先于 mdat 输出，
// 但 trun 样本大小要等 mdat 转换结果才能定，故延迟到 mdat 处理时一并写出）。
type pendingMoof struct {
	orig  []byte
	infos []*trafInfo
}

// buildMoof 重建 moof：克隆原字节（tfdt 已归一化），若提供 newSizes 则重写
// 对应轨的 trun 样本大小，并把其后各轨的 data_offset 同步平移
// （default-base-is-moof 下 data_offset 相对 moof 起点，前面样本变大必须跟着加）。
func buildMoof(p *pendingMoof, newSizes map[int][]uint32) []byte {
	out := make([]byte, len(p.orig))
	copy(out, p.orig)
	growth := 0 // 前面已转换轨道的大小增量（原始 → 新）
	for idx, ti := range p.infos {
		if growth != 0 && ti.dataOffOff >= 0 {
			cur := int32(binary.BigEndian.Uint32(out[ti.dataOffOff:]))
			binary.BigEndian.PutUint32(out[ti.dataOffOff:], uint32(cur+int32(growth)))
		}
		ns, ok := newSizes[idx]
		if !ok {
			continue
		}
		for i, off := range ti.sizeOffs {
			binary.BigEndian.PutUint32(out[off:], ns[i])
		}
		growth += int(sum(ns)) - int(sum(ti.sizes))
	}
	return out
}

// startsWithStartCode 判断样本是否为 Annex-B 裸流（以 start code 开头）。
func startsWithStartCode(s []byte) bool {
	if len(s) < 3 {
		return false
	}
	if s[0] == 0 && s[1] == 0 && s[2] == 1 {
		return true
	}
	return len(s) >= 4 && s[0] == 0 && s[1] == 0 && s[2] == 0 && s[3] == 1
}

// sampleMode 单个样本的封装模式。
type sampleMode int

const (
	modeUnknown sampleMode = iota
	modeAnnexB             // start code 分隔（00 00 01 / 00 00 00 01）
	modeAVCC               // 4 字节长度前缀
	modeRawNAL             // 无封装裸 NAL（首字节就是 NAL 头）
)

// isVideoNalType H.264 常见 NAL 单元类型（含非 VCL 的 AUD/SEI/SPS/PPS）。
func isVideoNalType(t byte) bool {
	switch t {
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 12, 19, 20, 21:
		return true
	}
	return false
}

// classifySample 判断单个样本的封装模式：
// start code 开头 → Annex-B；前 4 字节是合理长度且 NAL 头类型合法 → AVCC；
// 首字节就是合法 NAL 头 → 裸 NAL；否则 Unknown（如 AAC 音频样本 0xFF 开头）。
func classifySample(s []byte) sampleMode {
	if len(s) == 0 {
		return modeUnknown
	}
	if startsWithStartCode(s) {
		return modeAnnexB
	}
	if len(s) >= 5 {
		l := int(binary.BigEndian.Uint32(s))
		if l > 0 && l <= len(s)-4 && isVideoNalType(s[4]&0x1F) {
			return modeAVCC
		}
	}
	if isVideoNalType(s[0] & 0x1F) {
		return modeRawNAL
	}
	return modeUnknown
}

// sampleToAVCC 把样本转为 AVCC 长度前缀形式：
// Annex-B 按 start code 拆成多个 NAL；裸 NAL 整体视为单个 NAL。
func sampleToAVCC(s []byte, mode sampleMode) ([]byte, error) {
	switch mode {
	case modeAnnexB:
		return annexBToAVCC(s)
	case modeRawNAL:
		out := make([]byte, 4+len(s))
		binary.BigEndian.PutUint32(out, uint32(len(s)))
		copy(out[4:], s)
		return out, nil
	}
	return s, nil
}

// annexBToAVCC 把 Annex-B 裸流样本（start code 分隔）转为 AVCC（4 字节长度前缀）。
func annexBToAVCC(s []byte) ([]byte, error) {
	nals := splitAnnexB(s)
	if len(nals) == 0 {
		return nil, errors.New("样本内没有 NAL")
	}
	out := make([]byte, 0, len(s)+4*len(nals))
	var hdr [4]byte
	for _, n := range nals {
		binary.BigEndian.PutUint32(hdr[:], uint32(len(n)))
		out = append(out, hdr[:]...)
		out = append(out, n...)
	}
	return out, nil
}

// splitAnnexB 按 start code（00 00 01，或更长 0 前缀 + 01）拆分 NAL 单元。
// 合法流中 NAL 内部不会出现 00 00 00（有防竞争字节），故 00 00 00 01 只能是分界。
func splitAnnexB(s []byte) [][]byte {
	var out [][]byte
	start := -1
	i := 0
	for i+3 <= len(s) {
		if s[i] == 0 && s[i+1] == 0 {
			j := i + 2
			for j < len(s) && s[j] == 0 {
				j++
			}
			if j < len(s) && s[j] == 1 {
				if start >= 0 {
					out = append(out, s[start:i])
				}
				start = j + 1
				i = start
				continue
			}
			i = j
			continue
		}
		i++
	}
	if start >= 0 && start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func sum(u []uint32) uint32 {
	var t uint32
	for _, v := range u {
		t += v
	}
	return t
}

// rebuildMdat 重写一个 mdat：把裸 NAL 视频样本转成 AVCC 长度前缀。
// 仅视频轨（init 段 stsd 判定）参与转换；音频轨原样复制，防止
// AAC 帧头被误判为视频 NAL 而损坏。
// 返回新的 mdat box 与每轨新样本大小（nil 表示该轨未转换）；无转换时原样返回。
func rebuildMdat(mdat []byte, mdatAbs int, infos []*trafInfo, n *normState) ([]byte, map[int][]uint32, error) {
	hdr := 8
	if len(mdat) >= 8 && binary.BigEndian.Uint32(mdat) == 1 {
		hdr = 16
	}
	payload := mdat[hdr:]

	var out bytes.Buffer
	cursor := 0 // 已复制的原始 payload 字节数
	newSizes := map[int][]uint32{}
	for idx, ti := range infos {
		if len(ti.sizes) == 0 {
			continue
		}
		rel := ti.dataStart - mdatAbs - hdr // 本轨首个样本相对 payload 的偏移
		if rel < cursor {
			return nil, nil, fmt.Errorf("track=%d 样本区间重叠", ti.trackID)
		}
		if rel > len(payload) {
			return nil, nil, fmt.Errorf("track=%d 样本越界", ti.trackID)
		}
		out.Write(payload[cursor:rel])
		cursor = rel

		if !n.isVideoTrack(ti.trackID) {
			// 音频/其它轨：样本原样复制，不做封装猜测与转换
			for _, sz := range ti.sizes {
				if cursor+int(sz) > len(payload) {
					return nil, nil, fmt.Errorf("track=%d 样本越界", ti.trackID)
				}
				out.Write(payload[cursor : cursor+int(sz)])
				cursor += int(sz)
			}
			continue
		}

		// 视频轨：第一遍检测本轨所有样本是否都是视频类封装（Annex-B 或裸 NAL），
		// 全部是才整轨转换（AVCC 样本保持原样，避免破坏标准封装流）。
		convert := len(ti.sizes) > 0
		q := cursor
		for _, sz := range ti.sizes {
			if q+int(sz) > len(payload) {
				return nil, nil, fmt.Errorf("track=%d 样本越界", ti.trackID)
			}
			switch classifySample(payload[q : q+int(sz)]) {
			case modeAnnexB, modeRawNAL:
			default:
				convert = false
			}
			q += int(sz)
		}

		if convert && ti.sizeOffs != nil {
			ns := make([]uint32, len(ti.sizes))
			for i, sz := range ti.sizes {
				s := payload[cursor : cursor+int(sz)]
				conv, err := sampleToAVCC(s, classifySample(s))
				if err != nil {
					return nil, nil, fmt.Errorf("track=%d: %v", ti.trackID, err)
				}
				out.Write(conv)
				ns[i] = uint32(len(conv))
				cursor += int(sz)
			}
			newSizes[idx] = ns
		} else {
			for _, sz := range ti.sizes {
				out.Write(payload[cursor : cursor+int(sz)])
				cursor += int(sz)
			}
		}
	}
	out.Write(payload[cursor:])

	if len(newSizes) == 0 {
		return mdat, nil, nil // 没有需要转换的轨道，原样返回
	}
	newPayload := out.Bytes()
	rebuilt := make([]byte, 0, hdr+len(newPayload))
	if hdr == 16 {
		rebuilt = append(rebuilt, 0, 0, 0, 1, 'm', 'd', 'a', 't')
		var szb [8]byte
		binary.BigEndian.PutUint64(szb[:], uint64(16+len(newPayload)))
		rebuilt = append(rebuilt, szb[:]...)
	} else {
		var szb [4]byte
		binary.BigEndian.PutUint32(szb[:], uint32(8+len(newPayload)))
		rebuilt = append(rebuilt, szb[:]...)
		rebuilt = append(rebuilt, 'm', 'd', 'a', 't')
	}
	rebuilt = append(rebuilt, newPayload...)
	return rebuilt, newSizes, nil
}

// normalizeFMP4Segment 规范化一个 fMP4 分片（或整段拼接文件）：
// 时间戳归零（跨分片基准）+ 裸 NAL→AVCC。
// 分片开头若内联 init（ftyp/moov）则先经 consumeInit 补 mehd 占位、解析轨道类型
// 与回填位置；非 fMP4 / 结构异常的数据原样返回（不报错），保证下载管线不受影响。
// 由 normState.Normalize 调用（接口入口），内部直接访问具体字段。
func normalizeFMP4Segment(data []byte, n *normState) ([]byte, error) {
	if len(data) < 16 || n == nil {
		return data, nil
	}
	// 内联 init 在分片首部：补 mehd 占位、解析轨道类型与回填位置
	// （consumeInit 内部完成；已持有 init 或数据无 moov 时原样返回）
	data = n.consumeInit(data)
	if len(data) < 16 {
		return data, nil
	}
	out := make([]byte, 0, len(data)+len(data)/16)
	var pending *pendingMoof
	pos := 0
	for pos+8 <= len(data) {
		sz, _, typ, ok := boxHeader(data, pos)
		if !ok {
			break
		}
		switch typ {
		case "moof":
			if pending != nil {
				out = append(out, buildMoof(pending, nil)...)
				pending = nil
			}
			infos, perr := parseMoof(data[pos:pos+sz], pos)
			if perr != nil {
				out = append(out, data[pos:]...)
				return out, nil
			}
			// 复制后再归一化 tfdt，绝不修改调用方的输入缓冲
			moofCopy := make([]byte, sz)
			copy(moofCopy, data[pos:pos+sz])
			for _, ti := range infos {
				normalizeTraf(moofCopy, ti, n)
			}
			pending = &pendingMoof{orig: moofCopy, infos: infos}
			pos += sz
		case "mdat":
				if pending != nil {
					newMdat, newSizes, rerr := rebuildMdat(data[pos:pos+sz], pos, pending.infos, n)
				if rerr != nil {
					// 转换失败：保留 tfdt 归一化，mdat 原样放行
					out = append(out, buildMoof(pending, nil)...)
					out = append(out, data[pos:pos+sz]...)
				} else {
					out = append(out, buildMoof(pending, newSizes)...)
					out = append(out, newMdat...)
				}
				pending = nil
			} else {
				out = append(out, data[pos:pos+sz]...)
			}
			pos += sz
		default:
			if pending != nil {
				out = append(out, buildMoof(pending, nil)...)
				pending = nil
			}
			out = append(out, data[pos:pos+sz]...)
			pos += sz
		}
	}
	if pending != nil {
		out = append(out, buildMoof(pending, nil)...)
	}
	if pos < len(data) {
		out = append(out, data[pos:]...)
	}
	return out, nil
}
