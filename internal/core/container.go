// 容器注册表：媒体容器格式的识别与拼接策略。
//
// 新增一种格式只需往 containerRegistry 追加一个 Container 条目
// （魔数探测 + 输出扩展名 + 初始化策略 + 拼接方式），下载管线会按探测结果
// 自动修正输出扩展名、写入 fMP4 init 段，无需改动其它代码。
package core

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

// NormState 容器规范化的跨分片状态接口：有状态容器（当前 fMP4）实现
// 快照/恢复/结束时间累计；无状态容器（generic 等）不实现，NewState 为 nil。
// 方法未导出：状态实现目前集中在 core 包内，跨包格式可后续导出发布。
type NormState interface {
	snapshot() map[string]uint64
	restore(map[string]uint64)
	endSnapshot() map[string]uint64
	restoreEnd(map[string]uint64)
}

// Container 一种媒体容器格式的完整描述。
type Container struct {
	ID     string
	Ext    string // 输出文件扩展名（含点）；空 = 保持调用方给定的文件名
	Detect func([]byte) bool
	Init   InitPolicy
	Concat ConcatKind
	// NewState 创建该容器的跨分片规范化状态；nil = 无状态（generic）。
	NewState func() NormState
	// Normalize 分片写入前的规范化处理（当前仅 fMP4：时间戳归一化 + 裸 NAL→AVCC）。
	// st 由 NewState 创建并跨分片复用（tfdt 基准、每轨结束时间、轨道类型）。
	Normalize func(data []byte, st NormState) ([]byte, error)
	// Backfill 任务完成/暂停收尾时的容器特定处理（当前仅 fMP4：回填 mehd/mvhd 总时长）。
	// 容器自有的 init 信息由规范化状态持有（NewState 创建的 st），实现内自行获取；
	// nil = 无需收尾处理（generic 等）。有 Backfill 的容器必然需要 init 信息（见 pipeline 续传分支）。
	Backfill func(path string, st NormState) error
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

// containerRegistry 按序探测（先命中的格式优先）。fmp4 有两个条目：
//   - 首片自带 ftyp（完整 init 内联）→ 无需 #EXT-X-MAP，裸拼即可
//   - 首片是 styp/moof（纯媒体分片）→ 需要 #EXT-X-MAP 提供的 init 段
//
// generic 恒真 Detect 兜底，必须保持在最后。
var containerRegistry = []Container{
	{ID: "ts", Ext: ".ts", Detect: isTS, Init: InitNone, Concat: ConcatAppend},
	{ID: "fmp4", Ext: ".mp4", Detect: hasFtyp, Init: InitNone, Concat: ConcatAppend, NewState: newFMP4State, Normalize: normalizeFMP4Segment, Backfill: backfillDurations},
	{ID: "fmp4", Ext: ".mp4", Detect: hasMoof, Init: InitFromMap, Concat: ConcatInitAppend, NewState: newFMP4State, Normalize: normalizeFMP4Segment, Backfill: backfillDurations},
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
// 只依赖 Normalize 行为（两条目相同），统一返回 #EXT-X-MAP 形态的条目。
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
