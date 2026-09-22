// hdlr 的字段边界必须按 hdlr 盒**自己声明的长度**判断（第七轮 P1-3）。
//
// 缺陷形态：hdlrMediaType 用 `len(mdia)-payload < 16` 判断"装得下 handler_type 吗" ——
// 判的是**父盒 mdia 的总长**，不是 hdlr 自己的 boxEnd。于是一个声明长度只到
// pre_defined 的畸形 hdlr，会被判成"长度足够"，接着从盒外（或盒内声明不到的
// 位置）把 4 个字节读成 handler_type。
//
// 危害不是崩溃而是**判错轨道类型**：hdlr 的优先级高于 stsd，读出的垃圾字节只要
// 恰好是 'vide'/'soun' 就会覆盖 stsd 的正确结论 —— 产物"能播但轨道标记错"，
// 日志全绿。这正是本项目头号缺陷形态。
//
// 同文件另四个盒遍历（stsdMediaType / mvhdTimescale / …）都按各自的 box 末端判断，
// 这条用例把 hdlr 也钉进同一判据。
package fmp4

import (
	"encoding/binary"
	"testing"
)

// buildInitWithShortHDL 造单轨 init 段：该轨 hdlr 声明的长度只到 pre_defined
// （8 头 + verflags 4 + pre_defined 4 + 4 字节 = 20 字节，payload 仅 12 字节），
// 那多出来的 4 字节写成 "vide"；同轨 stsd entry 是 mp4a（音频）。
//
// 两种实现因此给出相反结论：
//   - 按父盒 mdia 总长判断（旧）：mdia 后面还有很长 ⇒ 判"够长"，把那 4 字节
//     当 handler_type 读出来 = "vide" ⇒ 判成视频轨；
//   - 按 hdlr 自己的末端判断（新）：payload+16 > boxEnd ⇒ 拒读 ⇒ hdlr 无结论，
//     回退 stsd ⇒ 判成音频轨。
//
// 盒的物理长度与声明长度一致，所以 mdia 的盒遍历仍是通的（stsd 那条回退路径可达）。
func buildInitWithShortHDL() []byte {
	const trackID uint32 = 3

	tkhdPayload := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhdPayload[8:], trackID)
	tkhd := fullBox4("tkhd", 0x7, tkhdPayload)

	mdhdPayload := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhdPayload[8:], 90000)
	mdhd := fullBox4("mdhd", 0, mdhdPayload)

	// payload 12 字节：pre_defined(4) + 盒外那 4 字节 "vide"
	hdlr := fullBox4("hdlr", 0, []byte{0, 0, 0, 0, 'v', 'i', 'd', 'e'})

	stsd := fullBox4("stsd", 0, append([]byte{0, 0, 0, 1}, box4("mp4a", make([]byte, 8))...))
	minf := box4("minf", box4("stbl", stsd))
	mdia := box4("mdia", append(append(mdhd, hdlr...), minf...))
	trak := box4("trak", append(tkhd, mdia...))

	mvex := box4("mvex", fullBox4("trex", 0, make([]byte, 24)))
	mvhdPayload := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhdPayload[8:], 1000)
	mvhd := fullBox4("mvhd", 0, mvhdPayload)
	moov := box4("moov", append(append(mvhd, trak...), mvex...))
	return append(box4("ftyp", []byte("isom")), moov...)
}

// TestShortHDLDoesNotReadPastItsOwnBox hdlr 声明长度装不下 handler_type 时，
// 必须拒绝该字段并回退 stsd，而不是从盒外读出一个"看起来合法"的值。
func TestShortHDLDoesNotReadPastItsOwnBox(t *testing.T) {
	_, info := prepareInit(buildInitWithShortHDL(), true)

	isVideo, ok := info.trackVideo[3]
	if !ok {
		// 防退化：两条判定路径都没给出结论时，下面的断言会变成恒真
		// （没记录 = 读 map 得到零值 = false = "音频"）。这里必须先红。
		t.Fatal("轨道类型既没从 hdlr 也没从 stsd 得出 —— 用例失效（stsd 回退路径没跑到）")
	}
	if isVideo {
		t.Fatal(`hdlr 声明的长度装不下 handler_type，却把盒外的 4 字节读成了 "vide" 判成视频轨；` +
			`应拒绝该字段并回退 stsd（mp4a ⇒ 音频）`)
	}
}

// TestValidHDLStillWins 不误伤：合法 hdlr 仍然优先于 stsd。
// 收窄边界最怕的是"连正常盒也不读了"，这条用"hdlr=soun 而 stsd=hvc1"钉住优先级。
func TestValidHDLStillWins(t *testing.T) {
	init := buildInitWithHDL(1000, map[uint32]uint32{1: 90000},
		map[uint32]string{1: "soun"})
	_, info := prepareInit(init, true)

	isVideo, ok := info.trackVideo[1]
	if !ok {
		t.Fatal("合法 hdlr 未被读出，轨道类型缺失")
	}
	if isVideo {
		t.Fatal("hdlr=soun 应判音频（且必须压过 stsd 的 hvc1）")
	}
}
