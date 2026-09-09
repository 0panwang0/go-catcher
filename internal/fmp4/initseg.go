// fMP4 init 段处理：确保 moov 声明 fragment 总时长（mehd）。
//
// B 站等直播平台的 init 段（#EXT-X-MAP）通常只有 ftyp+moov，moov 内
// mvex 只带 trex 而没有 mehd（movie extends header）。播放器（如 Windows
// Media Player）打开文件时无法预知总时长，只能按「直播流」处理：
// 不显示结束时间、禁止拖动进度条。
//
// prepareInit 在 init 段落盘前给 mvex 插入一个占位 mehd（version=1，
// duration=0），录制/下载过程中 normState 跨分片累计每轨的最大结束时间，
// 任务完成时把总时长回填到 mehd 的 duration 字段（见 normState.Finish）。
// 插入发生在文件生成之初，后续分片追加在其后，无需移动任何已写数据。
//
// 对没有 moov/mvex 的 init 段（或普通 MP4）原样返回，不影响其它格式。
package fmp4

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strconv"
)

// fmp4InitInfo init 段解析结果：电影/轨道 timescale 与 mehd/mvhd 可写位置。
type fmp4InitInfo struct {
	movieTS    uint32            // mvhd timescale（mehd duration 的单位）
	trackTS    map[uint32]uint32 // trackID -> mdhd timescale
	trackVideo map[uint32]bool   // trackID -> 是否视频轨（来自 stsd entry 类型；nil=未知）
	mehdOff    int               // mehd box 在 init 段内的偏移（-1 = 无 mehd 可写）
	mehdWide   bool              // mehd version==1（8 字节 duration）
	mvhdOff    int               // mvhd box 起始偏移（-1 = 无；回填 mvhd.duration 用）
	mvhdWide   bool              // mvhd version==1（8 字节 duration）
}

// prepareInit 解析 init 段并（可选）给 mvex 补 mehd。
// insert=true 且 mvex 无 mehd 时插入占位（version 1, duration=0），返回改写后的数据；
// 已有 mehd 或 insert=false 时不改数据，仅记录位置与 timescale。
func prepareInit(data []byte, insert bool) ([]byte, *fmp4InitInfo) {
	info := &fmp4InitInfo{mehdOff: -1, mvhdOff: -1, trackTS: map[uint32]uint32{}}
	pos := 0
	for pos+8 <= len(data) {
		sz, _, typ, ok := boxHeader(data, pos)
		if !ok {
			break
		}
		if typ == "moov" {
			moov := data[pos : pos+sz]
			newMoov, mehdInMoov, wide := patchMoov(moov, insert)
			if mehdInMoov >= 0 {
				info.mehdOff = pos + mehdInMoov
				info.mehdWide = wide
			}
			if !bytesEqual(newMoov, moov) {
				// moov 重建（可能已插入 mehd）：拼回 init 段，moov 之后的 box 原样后移
				rebuilt := make([]byte, 0, pos+len(newMoov)+len(data)-(pos+sz))
				rebuilt = append(rebuilt, data[:pos]...)
				rebuilt = append(rebuilt, newMoov...)
				rebuilt = append(rebuilt, data[pos+sz:]...)
				data = rebuilt
			}
			parseTimescales(data, info)
			break
		}
		pos += sz
	}
	return data, info
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// patchMoov 检查 moov 内 mvex 是否含 mehd；无则插入占位 mehd。
// 返回重建的 moov、mehd 在 moov 内的偏移（-1=无）以及 mehd 是否 64 位。
func patchMoov(moov []byte, insert bool) ([]byte, int, bool) {
	// 先定位 mvex（含其子 box 布局）
	mvexPos, mvexSz := -1, 0
	pos := 8
	for pos+8 <= len(moov) {
		sz, _, typ, ok := boxHeader(moov, pos)
		if !ok {
			break
		}
		if typ == "mvex" {
			mvexPos, mvexSz = pos, sz
			break
		}
		pos += sz
	}
	if mvexPos < 0 {
		return moov, -1, false // 无 mvex：非 fMP4 init，不动
	}

	// 找 mvex 内是否已有 mehd
	mehdOff := -1
	q := mvexPos + 8
	for q+8 <= mvexPos+mvexSz {
		sz, _, typ, ok := boxHeader(moov, q)
		if !ok {
			break
		}
		if typ == "mehd" {
			mehdOff = q
			break
		}
		q += sz
	}
	if mehdOff >= 0 {
		wide := moov[mehdOff+8] == 1
		return moov, mehdOff, wide
	}
	if !insert {
		return moov, -1, false
	}

	// 构造 mehd v1（size+type+verflags+duration64）
	var mehd [20]byte
	binary.BigEndian.PutUint32(mehd[0:], 20)
	copy(mehd[4:], "mehd")
	mehd[8] = 1 // version 1
	// duration（偏移 12..19）保持 0 占位

	// 重建 moov：mvex 增大 20 字节（mehd 插在 mvex 子 box 序列最前）
	newMoov := make([]byte, 0, len(moov)+20)
	newMoov = append(newMoov, moov[:mvexPos+8]...)
	newMoov = append(newMoov, mehd[:]...)
	newMoov = append(newMoov, moov[mvexPos+8:]...)
	binary.BigEndian.PutUint32(newMoov[0:], uint32(len(newMoov)))                    // moov size
	binary.BigEndian.PutUint32(newMoov[mvexPos:], uint32(mvexSz+20))                 // mvex size
	return newMoov, mvexPos + 8, true // mehd 位于 mvex 子 box 序列起点
}

// parseTimescales 解析 mvhd（电影 timescale）与各 trak 的 tkhd/mdhd。
func parseTimescales(init []byte, info *fmp4InitInfo) {
	pos := 0
	for pos+8 <= len(init) {
		sz, _, typ, ok := boxHeader(init, pos)
		if !ok {
			break
		}
		if typ == "moov" {
			parseMoovTimescales(init[pos:pos+sz], pos, info)
			return
		}
		pos += sz
	}
}

// parseMoovTimescales 解析 moov 内 mvhd 的 timescale 与绝对偏移。
// moovAbs 是 moov 在 init 段中的偏移：mvhdOff 必须记绝对偏移，
// 否则 moov 前面有 ftyp 等 box 时，回填会把时长写进 moov 的 size 字段。
func parseMoovTimescales(moov []byte, moovAbs int, info *fmp4InitInfo) {
	pos := 8
	for pos+8 <= len(moov) {
		sz, payload, typ, ok := boxHeader(moov, pos)
		if !ok {
			break
		}
		switch typ {
		case "mvhd":
			if ts, ok := mvhdTimescale(moov[payload:]); ok {
				info.movieTS = ts
				info.mvhdOff = moovAbs + pos
				info.mvhdWide = moov[payload] == 1
			}
		case "trak":
			parseTrakTimescales(moov[pos:pos+sz], info)
		}
		pos += sz
	}
}

// parseTrakTimescales 从 trak 里读 tkhd.trackID、mdia/mdhd.timescale 与媒体种类。
// 轨道类型判定：hdlr.handler_type（ISO 标准权威字段）优先，stsd entry 列表兜底。
func parseTrakTimescales(trak []byte, info *fmp4InitInfo) {
	var trackID uint32
	var trackTS uint32
	var mediaType string
	pos := 8
	for pos+8 <= len(trak) {
		sz, payload, typ, ok := boxHeader(trak, pos)
		if !ok {
			break
		}
		switch typ {
		case "tkhd":
			trackID = tkhdTrackID(trak[payload:])
		case "mdia":
			if ts, ok := mdhdTimescale(trak[pos:pos+sz]); ok {
				trackTS = ts
			}
			if m := hdlrMediaType(trak[pos : pos+sz]); m != "" {
				mediaType = m
			} else if m := stsdMediaType(trak[pos : pos+sz]); m != "" {
				mediaType = m
			}
		}
		pos += sz
	}
	if trackID != 0 {
		if trackTS != 0 {
			info.trackTS[trackID] = trackTS
		}
		if mediaType != "" {
			if info.trackVideo == nil {
				info.trackVideo = map[uint32]bool{}
			}
			info.trackVideo[trackID] = mediaType == "video"
		}
	}
}

// hdlrMediaType 在 mdia 内解析 hdlr.handler_type（'vide'/'soun'）。
// ISO 14496-12 以 handler_type 权威定义轨道用途，任何编解码器都适用；
// 未知/缺失 handler 返回空串，由调用方回退 stsd entry 判定。
func hdlrMediaType(mdia []byte) string {
	pos := 8
	for pos+8 <= len(mdia) {
		sz, payload, typ, ok := boxHeader(mdia, pos)
		if !ok {
			break
		}
		if typ == "hdlr" {
			// hdlr: verflags(4) + pre_defined(4) + handler_type(4) + reserved(12) + name*
			if len(mdia)-payload < 16 {
				return ""
			}
			switch string(mdia[payload+8 : payload+12]) {
			case "vide":
				return "video"
			case "soun":
				return "audio"
			}
			return ""
		}
		pos += sz
	}
	return ""
}

// stsdMediaType 在 mdia 内解析 stsd 首个 entry 类型，映射为 "video"/"audio"。
// 仅当 hdlr 缺失或 handler 未知时作为兜底；新增编解码器需在此追加条目。
// 未知类型返回空串（不写入 trackVideo，避免错误标记）。
func stsdMediaType(mdia []byte) string {
	pos := 8
	for pos+8 <= len(mdia) {
		sz, _, typ, ok := boxHeader(mdia, pos)
		if !ok {
			break
		}
		if typ == "minf" {
			q := pos + 8
			for q+8 <= pos+sz {
				msz, _, mtyp, mok := boxHeader(mdia, q)
				if !mok {
					break
				}
				if mtyp == "stbl" {
					r := q + 8
					for r+8 <= q+msz {
						ssz, _, styp, sok := boxHeader(mdia, r)
						if !sok {
							break
						}
						if styp == "stsd" {
							e := r + 8 + 8 // version_flags(4) + entry_count(4)
							if e+8 <= r+ssz {
								if _, _, etyp, eok := boxHeader(mdia, e); eok {
									return classifyMediaType(etyp)
								}
							}
						}
						r += ssz
					}
				}
				q += msz
			}
		}
		pos += sz
	}
	return ""
}

// classifyMediaType 根据 stsd entry 类型判断轨种类：视频编解码条目 → video，
// 音频编解码条目 → audio，其它（未知/数据轨）→ 空串。
func classifyMediaType(entry string) string {
	switch entry {
	case "hvc1", "hev1", "avc1", "avc3", "av01", "vp08", "vp09", "mp4v", "s263", "dvh1", "dvhe":
		return "video"
	case "mp4a", "enca", "ac-3", "ec-3", "Opus", "fLaC", "alac", "samr", "sawb":
		return "audio"
	}
	return ""
}

// mvhdTimescale 解析 mvhd 的 timescale（payload 从 version_flags 起）。
func mvhdTimescale(p []byte) (uint32, bool) {
	if len(p) < 20 {
		return 0, false
	}
	if p[0] == 1 {
		if len(p) < 28 {
			return 0, false
		}
		return binary.BigEndian.Uint32(p[20:]), true // 16(creation/mod)+4(timescale)
	}
	return binary.BigEndian.Uint32(p[12:]), true // 8(creation/mod)+4(timescale)
}

// tkhdTrackID 解析 tkhd 的 track_ID。
func tkhdTrackID(p []byte) uint32 {
	if len(p) < 16 {
		return 0
	}
	if p[0] == 1 {
		if len(p) < 24 {
			return 0
		}
		return binary.BigEndian.Uint32(p[20:]) // 16(creation/mod)+4(track_ID)
	}
	return binary.BigEndian.Uint32(p[12:]) // 8(creation/mod)+4(track_ID)
}

// mdhdTimescale 在 mdia 内解析 mdhd 的 timescale。
func mdhdTimescale(mdia []byte) (uint32, bool) {
	pos := 8
	for pos+8 <= len(mdia) {
		sz, payload, typ, ok := boxHeader(mdia, pos)
		if !ok {
			break
		}
		if typ == "mdhd" {
			p := mdia[payload:]
			if len(p) < 16 {
				return 0, false
			}
			if p[0] == 1 {
				if len(p) < 24 {
					return 0, false
				}
				return binary.BigEndian.Uint32(p[20:]), true
			}
			return binary.BigEndian.Uint32(p[12:]), true
		}
		pos += sz
	}
	return 0, false
}

// mehdDuration 把每轨累计结束时间换算为 movie timescale 单位，取全局最大。
func mehdDuration(n *normState, info *fmp4InitInfo) uint64 {
	if info == nil || info.movieTS == 0 {
		return 0
	}
	var max uint64
	for ks, end := range n.endSnapshot() {
		trackID, err := strconv.ParseUint(ks, 10, 32)
		if err != nil {
			continue
		}
		ts, ok := info.trackTS[uint32(trackID)]
		if !ok || ts == 0 {
			continue
		}
		v := end*uint64(info.movieTS) / uint64(ts)
		if v > max {
			max = v
		}
	}
	return max
}

// persistFMP4InitInfo fmp4InitInfo 的持久化形态（JSON 的 map 键必须是字符串）。
type persistFMP4InitInfo struct {
	MovieTS    uint32            `json:"movieTS"`
	TrackTS    map[string]uint32 `json:"trackTS,omitempty"`
	TrackVideo map[string]bool   `json:"trackVideo,omitempty"`
	MehdOff    int               `json:"mehdOff"`
	MehdWide   bool              `json:"mehdWide"`
	MvhdOff    int               `json:"mvhdOff"`
	MvhdWide   bool              `json:"mvhdWide"`
}

func (i *fmp4InitInfo) persist() *persistFMP4InitInfo {
	if i == nil {
		return nil
	}
	p := &persistFMP4InitInfo{
		MovieTS: i.movieTS, MehdOff: i.mehdOff, MehdWide: i.mehdWide,
		MvhdOff: i.mvhdOff, MvhdWide: i.mvhdWide,
	}
	if len(i.trackTS) > 0 {
		p.TrackTS = make(map[string]uint32, len(i.trackTS))
		for k, v := range i.trackTS {
			p.TrackTS[strconv.FormatUint(uint64(k), 10)] = v
		}
	}
	if len(i.trackVideo) > 0 {
		p.TrackVideo = make(map[string]bool, len(i.trackVideo))
		for k, v := range i.trackVideo {
			p.TrackVideo[strconv.FormatUint(uint64(k), 10)] = v
		}
	}
	return p
}

func (i *fmp4InitInfo) restore(p *persistFMP4InitInfo) {
	i.movieTS = p.MovieTS
	i.mehdOff = p.MehdOff
	i.mehdWide = p.MehdWide
	i.mvhdOff = p.MvhdOff
	i.mvhdWide = p.MvhdWide
	if i.trackTS == nil {
		i.trackTS = map[uint32]uint32{}
	}
	for ks, v := range p.TrackTS {
		if k, err := strconv.ParseUint(ks, 10, 32); err == nil {
			i.trackTS[uint32(k)] = v
		}
	}
	if len(p.TrackVideo) > 0 {
		i.trackVideo = map[uint32]bool{}
		for ks, v := range p.TrackVideo {
			if k, err := strconv.ParseUint(ks, 10, 32); err == nil {
				i.trackVideo[uint32(k)] = v
			}
		}
	}
}

// ---- 状态方法（init 段消费与收尾；normalize/finish 内部调用）----

// hasInit 是否已持有 init 段解析结果（consumeInit 幂等判断用）。
func (n *normState) hasInit() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.init != nil
}

// consumeInit 消费一段可能的 init 段字节：补 mehd 占位、解析轨道类型与回填位置。
// 已持有 init / 数据不含 moov 时原样返回（幂等：重放同一 init 不会重复插入）。
func (n *normState) consumeInit(data []byte) []byte {
	if n.hasInit() {
		return data
	}
	nd, info := prepareInit(data, true)
	if len(info.trackVideo) > 0 {
		n.mu.Lock()
		n.trackVideo = info.trackVideo
		n.mu.Unlock()
	}
	if info.mehdOff < 0 {
		return data // 无 mvex/mehd 可回填（非 fMP4 init），不持有解析结果
	}
	n.mu.Lock()
	n.init = info
	n.mu.Unlock()
	return nd
}

// Finish 任务完成/暂停收尾：把各轨累计结束时间回填为 mehd/mvhd 总时长
// （无 init 解析结果时静默跳过，非 fMP4 容器不受影响）。
// 实现 core.NormState 接口（方法名导出，供注册表跨包装配）。
func (n *normState) Finish(path string) error {
	n.mu.Lock()
	info := n.init
	n.mu.Unlock()
	if info == nil {
		return nil
	}
	return backfillDurations(path, info, n)
}

// backfillDurations 任务完成后把各轨累计结束时间换算为总时长，写回文件：
// mehd.fragment_duration（fMP4 标准总时长声明，播放器据此识别结束时间/支持拖动）
// + mvhd.duration（兼容只读 mvhd 的播放器）。
// init 段解析结果由调用方传入（来自 normState.finish）；无 mehd/mvhd 可写位置
// 或时长不可用时静默跳过。
func backfillDurations(path string, info *fmp4InitInfo, n *normState) error {
	if info == nil {
		return nil
	}
	dur := mehdDuration(n, info)
	if dur == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if info.mehdOff >= 0 {
		if !boxTypeAt(f, info.mehdOff, "mehd") {
			fmt.Printf("[disk] WARN: mehd 回填位置校验失败（偏移 %d），跳过\n", info.mehdOff)
		} else if err := writeDuration(f, info.mehdOff+12, dur, info.mehdWide); err != nil {
			return err
		}
	}
	if info.mvhdOff >= 0 {
		// mvhd 布局：box 头(8)+version_flags(4)+[creation+modification]+timescale+duration
		off := info.mvhdOff + 8
		if info.mvhdWide {
			off += 24 // v1：verflags(4)+creation(8)+modification(8)+timescale(4)
		} else {
			off += 16 // v0：verflags(4)+creation(4)+modification(4)+timescale(4)
		}
		if !boxTypeAt(f, info.mvhdOff, "mvhd") {
			fmt.Printf("[disk] WARN: mvhd 回填位置校验失败（偏移 %d），跳过\n", info.mvhdOff)
		} else if err := writeDuration(f, off, dur, info.mvhdWide); err != nil {
			return err
		}
	}
	return nil
}

// boxTypeAt 校验文件 off 处确实是指定类型的 box（读 size+type 头）。
// 防止偏移出错（如旧版本持久化的相对偏移）时把时长写进错误位置破坏文件。
func boxTypeAt(f *os.File, off int, want string) bool {
	var b [8]byte
	n, err := f.ReadAt(b[:], int64(off))
	if err != nil || n < 8 {
		return false
	}
	return string(b[4:8]) == want
}

// writeDuration 在文件 off 处写入 duration（wide=8 字节 v1，否则 4 字节 v0 并截断超范围值）。
func writeDuration(f *os.File, off int, dur uint64, wide bool) error {
	var buf [8]byte
	if wide {
		binary.BigEndian.PutUint64(buf[:], dur)
		_, err := f.WriteAt(buf[:], int64(off))
		return err
	}
	if dur > math.MaxUint32 {
		dur = math.MaxUint32
	}
	binary.BigEndian.PutUint32(buf[:4], uint32(dur))
	_, err := f.WriteAt(buf[:4], int64(off))
	return err
}
