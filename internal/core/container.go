// 容器注册表：媒体容器格式的识别与拼接策略。
//
// 新增一种格式只需往 containerRegistry 追加一个 Container 条目
// （魔数探测 + 输出扩展名 + 初始化策略 + 拼接方式），下载管线会按探测结果
// 自动修正输出扩展名、写入 fMP4 init 段，无需改动其它代码。
package core

import (
	"errors"

	"github.com/0panwang0/go-catcher/internal/fmp4"
)

// InitPolicy 说明是否需要「初始化段」（fMP4 的 ftyp/moov 文件头）。
type InitPolicy int

const (
	InitNone    InitPolicy = iota // 无需 init 段（TS / MP3 / FLV …），分片裸拼即有效
	InitFromMap                   // init 段由 #EXT-X-MAP 单独提供（fMP4 直播/点播）
)

// ConcatKind 说明分片拼接方式。
type ConcatKind int

const (
	ConcatAppend     ConcatKind = iota // 分片直接 append（TS / MP3 …）
	ConcatInitAppend                   // 文件头先写 init 段，再 append 分片（fMP4）
)

// NormState 容器规范化的跨分片状态接口：有状态容器（当前 fMP4）实现；
// 无状态容器（generic 等）不实现，NewState 为 nil。
// 状态对象同时是「数据变换器」：Normalize 对一段写入前的数据（init 段或分片）
// 做处理，实现内部直接访问自己的具体字段——接口上不泄漏任何容器专属类型，
// 跨分片信息一律以不透明字节在 Snapshot/Restore 间传递。
// 方法导出：实现可位于独立工具包（internal/fmp4），靠 Go 接口的结构化
// 匹配满足本接口，无需反向 import core。
type NormState interface {
	// Snapshot 导出全部跨分片状态（tfdt 基准、结束时间、init 信息）为不透明字节
	Snapshot() []byte
	// Restore 载入持久化的状态（断点续传；格式自解释，未知/损坏字节静默忽略）
	Restore(b []byte)

	// Normalize 处理一段写入前的数据（fMP4：init 段补 mehd 占位 + 分片时间戳
	// 归一化 + 裸 NAL→AVCC）；非本容器数据原样返回（不报错），保证管线不受影响
	Normalize(data []byte) ([]byte, error)

	// Finish 任务完成/暂停收尾（fMP4：回填 mehd/mvhd 总时长；无状态容器为空实现）
	Finish(path string) error
}

// Container 一种媒体容器格式的完整描述（纯静态事实，不含算法）。
type Container struct {
	ID     string
	Ext    string // 输出文件扩展名（含点）；空 = 保持调用方给定的文件名
	Detect func([]byte) bool
	Init   InitPolicy
	Concat ConcatKind
	// NewState 创建该容器的跨分片规范化状态；nil = 无状态（generic）。
	// 状态自带数据变换（normalize）与收尾（finish），容器条目本身不携带算法。
	NewState func() NormState
}

// ---- 魔数探测 ----

// hasFtyp fMP4/MP4 文件头（ISO BMFF：4 字节 box size + "ftyp"）。
func hasFtyp(p []byte) bool {
	return len(p) >= 8 && string(p[4:8]) == "ftyp"
}

// hasMoof fMP4 媒体分片（"styp"/"moof" box 开头，无 init 头，需要 #EXT-X-MAP）。
func hasMoof(p []byte) bool {
	if len(p) >= 8 {
		if s := string(p[4:8]); s == "styp" || s == "moof" {
			return true
		}
	}
	return len(p) >= 4 && string(p[0:4]) == "moof"
}

// isTS MPEG-TS：0x47 同步字节（188 字节一包）。
// 必须至少两个同步字节（首包 + 第二包）才可信：单个 0x47 太常见
// （ASCII 'G'、任意二进制都可能以它开头），会误判普通文件为 TS。
func isTS(p []byte) bool {
	return len(p) >= 376 && p[0] == 0x47 && p[188] == 0x47
}

// isFLV FLV 容器：开头 "FLV"。
func isFLV(p []byte) bool {
	return len(p) >= 3 && string(p[0:3]) == "FLV"
}

// isEBML WebM/MKV（EBML 魔数）。
func isEBML(p []byte) bool {
	return len(p) >= 4 && p[0] == 0x1a && p[1] == 0x45 && p[2] == 0xdf && p[3] == 0xa3
}

// isWebM EBML 且 DocType 含 webm。
func isWebM(p []byte) bool {
	return isEBML(p) && containsBytes(p, "webm")
}

// isMKV EBML 且 DocType 含 matroska（或无法判定时按 mkv 处理）。
func isMKV(p []byte) bool {
	return isEBML(p) && !containsBytes(p, "webm")
}

// isAAC ADTS AAC：0xFFF 同步且 layer 位为 00（ADTS 专有）。
func isAAC(p []byte) bool {
	return len(p) >= 2 && p[0] == 0xff && (p[1]&0xf6) == 0xf0
}

// isMP3 ID3v2 标签开头，或 MPEG 音频帧同步（layer 位非 00，区别于 ADTS）。
func isMP3(p []byte) bool {
	if len(p) >= 3 && string(p[0:3]) == "ID3" {
		return true
	}
	return len(p) >= 2 && p[0] == 0xff && (p[1]&0xe0) == 0xe0 && (p[1]&0x06) != 0
}

// isWAV RIFF/WAVE。
func isWAV(p []byte) bool {
	return len(p) >= 12 && string(p[0:4]) == "RIFF" && string(p[8:12]) == "WAVE"
}

// isOgg Ogg 容器。
func isOgg(p []byte) bool {
	return len(p) >= 4 && string(p[0:4]) == "OggS"
}

// isAVI RIFF/AVI。
func isAVI(p []byte) bool {
	return len(p) >= 12 && string(p[0:4]) == "RIFF" && string(p[8:12]) == "AVI "
}

func containsBytes(p []byte, s string) bool {
	return len(p) >= len(s) && indexBytes(p, []byte(s)) >= 0
}

func indexBytes(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if string(hay[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

// ---- 注册表 ----

// fmp4State 把 fmp4.NewState 包装为 NormState 接口工厂（注册表条目共用）。
// 此处即编译期验证：*fmp4.normState 的方法集满足 NormState 接口。
func fmp4State() NormState { return fmp4.NewState() }

// containerRegistry 按序探测（先命中的格式优先）。fmp4 有两个条目：
//   - 首片自带 ftyp（完整 init 内联）→ 无需 #EXT-X-MAP，裸拼即可
//   - 首片是 styp/moof（纯媒体分片）→ 需要 #EXT-X-MAP 提供的 init 段
//
// generic 恒真 Detect 兜底，必须保持在最后。
var containerRegistry = []Container{
	{ID: "ts", Ext: ".ts", Detect: isTS, Init: InitNone, Concat: ConcatAppend},
	{ID: "fmp4", Ext: ".mp4", Detect: hasFtyp, Init: InitNone, Concat: ConcatAppend, NewState: fmp4State},
	{ID: "fmp4", Ext: ".mp4", Detect: hasMoof, Init: InitFromMap, Concat: ConcatInitAppend, NewState: fmp4State},
	{ID: "flv", Ext: ".flv", Detect: isFLV, Init: InitNone, Concat: ConcatAppend},
	{ID: "webm", Ext: ".webm", Detect: isWebM, Init: InitNone, Concat: ConcatAppend},
	{ID: "mkv", Ext: ".mkv", Detect: isMKV, Init: InitNone, Concat: ConcatAppend},
	{ID: "aac", Ext: ".aac", Detect: isAAC, Init: InitNone, Concat: ConcatAppend},
	{ID: "mp3", Ext: ".mp3", Detect: isMP3, Init: InitNone, Concat: ConcatAppend},
	{ID: "wav", Ext: ".wav", Detect: isWAV, Init: InitNone, Concat: ConcatAppend},
	{ID: "ogg", Ext: ".ogg", Detect: isOgg, Init: InitNone, Concat: ConcatAppend},
	{ID: "avi", Ext: ".avi", Detect: isAVI, Init: InitNone, Concat: ConcatAppend},
	// 未知二进制（普通文件等）：保持原扩展名、裸拼
	{ID: "generic", Ext: "", Detect: func([]byte) bool { return true }, Init: InitNone, Concat: ConcatAppend},
}

// findContainerByID 按 ID 找回容器（断点续传时恢复规范化等格式相关行为）。
// fmp4 在注册表有两个条目（内联 init / #EXT-X-MAP）：续传时文件已有 init 段，
// 状态行为（NewState）两条目相同，统一返回 #EXT-X-MAP 形态的条目。
func findContainerByID(id string) *Container {
	var fallback *Container
	for i := range containerRegistry {
		if containerRegistry[i].ID != id {
			continue
		}
		if containerRegistry[i].Init == InitFromMap {
			return &containerRegistry[i]
		}
		if fallback == nil {
			fallback = &containerRegistry[i]
		}
	}
	return fallback
}

// looksLikeMedia 宽松判断数据是否具有可识别的媒体特征。
// 它比 detectContainer 的判定更宽容（只求"看起来像媒体"），
// 用于加密流场景下的误判守卫：探测结果是 generic 时，若数据连媒体
// 特征都没有，几乎可断定仍是密文/解密失败，而不是某个罕见但合法的容器。
func looksLikeMedia(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	// 高熵随机的 AES 密文偶尔也会撞上单个 0x47 / 字节碰巧命中某魔数前缀，
	// 故逐项收紧：TS 需连续两包同步、ISO BMFF 需完整 ftyp/moof/styp 判定，
	// 音频/容器魔数用其最小可区分长度。
	switch {
	case isTS(p):
		return true
	case hasFtyp(p), hasMoof(p):
		return true
	case isFLV(p), isEBML(p), isAAC(p), isMP3(p), isWAV(p), isOgg(p), isAVI(p):
		return true
	}
	return false
}

// guardGenericMedia 加密流误判守卫。
// 管线按首个分片探测到 generic 容器（"识别不出已知媒体格式"）时调用：
//
//   - key == nil（明文流/直链）：generic 是合法结果（可能是任意文件），放行。
//   - key != nil（播放列表声明了 #EXT-X-KEY，应解密成媒体）：
//     此时首个分片若在解密后仍无任何媒体特征，几乎必然意味着解密未生效
//     或源被加扰，当前正把密文/垃圾当成品保存——这是比"下载失败"更隐蔽的
//     数据损坏。返回描述性错误让任务判失败，而不是静默产出不可播文件。
//
// 已解密出明确媒体格式的分片不会走到这里（detectContainer 会命中 ts/fmp4/...），
// 因此本守卫只对"真出问题"的加密流生效，不误伤正常下载。
func guardGenericMedia(container *Container, key *KeyInfo, pre []byte) error {
	if container == nil || container.ID != "generic" {
		return nil
	}
	if key == nil {
		return nil
	}
	if looksLikeMedia(pre) {
		// 数据具备媒体特征但未被严格识别（罕见容器变体）：放行，避免误伤。
		return nil
	}
	return errDecryptProbeFailed
}

// errDecryptProbeFailed generic + 声明加密 + 解密后首片无媒体特征 → 判定解密失败。
var errDecryptProbeFailed = errors.New(
	"探测到视频已声明 AES 加密(#EXT-X-KEY)，但解密后的首个分片不包含任何可识别的媒体数据，" +
		"疑似解密失败或视频源被加扰，已中止下载以避免保存损坏文件")

// detectContainer 根据首个分片的开头字节 + 播放列表元信息识别容器。
//
// 规则：
//   - 播放列表有 #EXT-X-MAP 且首片不含 ftyp（即首片不是完整 init）→ fMP4，需取 init 段
//   - 否则按注册表魔数逐个探测
//   - 有 map 但魔数不明确 → 仍按 fMP4 处理
//   - 全部不匹配 → generic（普通文件，保持调用方给的扩展名）
func detectContainer(peek []byte, hasMap bool) *Container {
	if hasMap && !hasFtyp(peek) {
		return findContainerByID("fmp4")
	}
	for i := range containerRegistry {
		if containerRegistry[i].Detect(peek) {
			return &containerRegistry[i]
		}
	}
	if hasMap {
		return findContainerByID("fmp4")
	}
	return findContainerByID("generic")
}
