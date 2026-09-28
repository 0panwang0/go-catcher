// init 段里 mvhd / tkhd / mdhd 的字段边界必须按**该盒自己声明的长度**算（B-4）。
//
// 缺陷形态（与 2026-09-22 修过的 hdlrMediaType 完全同类）：解析器收的是
// `moov[payload:]` / `trak[payload:]` / `mdia[payload:]` —— 父盒的**剩余全部字节**，
// 而不是该盒声明的末端。于是一个"声明长度只到 version_flags"的畸形盒会被判成
// 长度足够（因为父盒后面还有大把字节），随后把**紧随其后的那个盒的字节**当成
// timescale / track_ID 读出来：
//   - mvhd 的 timescale 错 ⇒ mehd/mvhd 回填的总时长按错比例换算、静默写错；
//   - tkhd 的 track_ID 错 ⇒ trackTS 挂到不存在的轨道上 ⇒ 时长换算全错；
//   - mdhd 的 timescale 错 ⇒ 同上。
//
// 三个后果都是"产物能播但时长/时间轴是坏的"，而日志全绿 —— 本项目头号缺陷形态。
//
// 夹具把"会被泄进来的那 4 字节"钉成可辨认的常量 leak，因此：
//   - 修好后：被读的切片只剩 4 字节（连 20 字节都不到）⇒ 一律拒读 ⇒ 保守值；
//   - 改回父盒切片：正好读到 leak ⇒ 断言必红。
package fmp4

import (
	"encoding/binary"
	"testing"
)

// leakTimescale 夹具里故意放在"畸形读取会命中的位置"上的值。
// 挑成 10000 这种**看起来完全合理**的 timescale：这正是这类缺陷危险的地方 ——
// 读出来的垃圾不是哨兵值，没有第二处代码能发现它不对。
const leakTimescale uint32 = 10000

// shortBoxWithLeak 造一个**声明长度只到 version_flags** 的盒（8 头 + 4 字节 verflags），
// 其后紧跟一个 payload 前 4 字节为 leak 的盒。
//
// 关键位置演算（盒起点记为 X）：
//
//	短盒 占 X .. X+12；父盒切片按 payload=X+8 起算 ⇒ p[12:16] = 父盒[X+28 : X+32]
//	leak 盒 起于 X+12，头 X+12..X+20，payload 起于 X+20 ⇒ X+28 正是 leak
//
// 也就是说：**只有**把切片放到父盒末尾（而不是短盒末端）时，才会读到 leak。
func shortBoxWithLeak(typ string) []byte {
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], leakTimescale)
	return catB(fullBox4(typ, 0, nil), box4("skip", lp[:]))
}

// validTkhd 造一个合法的 v0 tkhd（trackID 落在 p[12:16]）。
func validTkhd(trackID uint32) []byte {
	p := make([]byte, 84)
	binary.BigEndian.PutUint32(p[8:], trackID)
	return fullBox4("tkhd", 0x7, p)
}

// validMvhd 造一个合法的 v0 mvhd（timescale 落在 p[12:16]）。
func validMvhd(timescale uint32) []byte {
	p := make([]byte, 100)
	binary.BigEndian.PutUint32(p[8:], timescale)
	return fullBox4("mvhd", 0, p)
}

// validMdhd 造一个合法的 v0 mdhd（timescale 落在 p[12:16]）。
func validMdhd(timescale uint32) []byte {
	p := make([]byte, 24)
	binary.BigEndian.PutUint32(p[8:], timescale)
	return fullBox4("mdhd", 0, p)
}

// TestShortMvhdDoesNotLeakTimescale mvhd 声明长度装不下 timescale 时，
// 必须判"没有可回填位置"，而不是从下一个盒读出 leak 当 timescale。
func TestShortMvhdDoesNotLeakTimescale(t *testing.T) {
	moov := box4("moov", catB(shortBoxWithLeak("mvhd"), validTkhd(1)))
	init := catB(box4("ftyp", []byte("isom")), moov)

	_, info := prepareInit(init, false)
	if info.movieTS != 0 {
		t.Fatalf("畸形 mvhd 的 timescale 被读成了 %d（= 下一个盒的字节），应为 0（拒读）", info.movieTS)
	}
	if info.mvhdOff != -1 {
		t.Fatalf("mvhd 声明长度装不下 timescale，却记下了回填位置 %d；"+
			"按此回填会把总时长写进盒外，破坏后续 box", info.mvhdOff)
	}
}

// TestShortTkhdDoesNotLeakTrackID tkhd 声明长度装不下 track_ID 时，
// 必须判"该轨没有 ID"，而不是把下一个盒的字节当成 ID 挂到不存在轨道上。
func TestShortTkhdDoesNotLeakTrackID(t *testing.T) {
	trak := box4("trak", catB(shortBoxWithLeak("tkhd"), box4("mdia", validMdhd(90000))))
	moov := box4("moov", catB(validMvhd(1000), trak))
	init := catB(box4("ftyp", []byte("isom")), moov)

	_, info := prepareInit(init, false)
	// 防退化：必须确认合法 mvhd 那条路是通的，否则"什么都没读到"也可能因为别的原因
	if info.movieTS != 1000 {
		t.Fatalf("合法 mvhd 的 timescale 没读出（=%d），用例失效", info.movieTS)
	}
	if _, ok := info.trackTS[leakTimescale]; ok {
		t.Fatalf("畸形 tkhd 把下一个盒的字节读成了 track_ID=%d 并据此记账", leakTimescale)
	}
	if len(info.trackTS) != 0 {
		t.Fatalf("畸形 tkhd 不该产生任何 trackTS 条目，实得 %v", info.trackTS)
	}
}

// TestShortMdhdDoesNotLeakTimescale mdhd 声明长度装不下 timescale 时，
// 必须判"该轨 timescale 未知"，而不是读到下一个盒的字节。
func TestShortMdhdDoesNotLeakTimescale(t *testing.T) {
	trak := box4("trak", catB(validTkhd(1), box4("mdia", shortBoxWithLeak("mdhd"))))
	moov := box4("moov", catB(validMvhd(1000), trak))
	init := catB(box4("ftyp", []byte("isom")), moov)

	_, info := prepareInit(init, false)
	// 防退化：合法 tkhd 必须被读出，否则 trackTS 空是因为 ID 没读到、而不是 mdhd 被判短
	if _, ok := info.trackTS[1]; ok {
		t.Fatalf("畸形 mdhd 却记下了 timescale=%d（应为漏读的 %d）", info.trackTS[1], leakTimescale)
	}
	if len(info.trackTS) != 0 {
		t.Fatalf("畸形 mdhd 不该产生任何 trackTS 条目，实得 %v", info.trackTS)
	}
}

// TestValidTimescaleBoxesStillRecorded 反向：收窄边界不许把正常盒一起挡掉。
// 同时钉住 mvhdOff 记的是**绝对**偏移（moov 前面有 ftyp 时必须算进去，
// 否则回填会把时长写进 moov 的 size 字段）。
func TestValidTimescaleBoxesStillRecorded(t *testing.T) {
	trak := box4("trak", catB(validTkhd(7), box4("mdia", validMdhd(90000))))
	moov := box4("moov", catB(validMvhd(1000), trak))
	ftyp := box4("ftyp", []byte("isom"))
	init := catB(ftyp, moov)

	_, info := prepareInit(init, false)
	if info.movieTS != 1000 {
		t.Fatalf("合法 mvhd timescale=%d want 1000", info.movieTS)
	}
	if want := len(ftyp) + 8; info.mvhdOff != want {
		t.Fatalf("mvhdOff=%d want %d（绝对偏移，含 moov 之前的 ftyp）", info.mvhdOff, want)
	}
	if got := info.trackTS[7]; got != 90000 {
		t.Fatalf("合法 mdhd track 7 timescale=%d want 90000", got)
	}
}
