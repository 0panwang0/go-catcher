// m3u8 解析：URL 拼接与 dlJob 分片解析。
package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func urlBase(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	idx := strings.LastIndex(u.Path, "/")
	if idx >= 0 {
		u.Path = u.Path[:idx+1] // 保留尾部 /，让 ResolveReference 正确拼接
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// looksBinary 判断内容是否含不可打印控制字符（m3u8 是纯文本，出现即异常）。
// 腾讯等站点在检测到非浏览器请求时会返回加密/混淆的二进制响应，
// 直接拿去切行解析会把二进制残片当成分片 URL，最终 url.Parse 报
// "invalid control character in URL"，错误完全不可读。
func looksBinary(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// isDirectMediaFile 判断响应开头是否是"直链媒体文件"（非 m3u8 播放列表）。
// 播放列表优先：内容里出现 #EXTM3U / #EXT-X- 就按播放列表处理，避免误判
// （腾讯等站点的真实播放列表里可能夹杂二进制残片行）。
func isDirectMediaFile(peek []byte, contentType string) bool {
	s := string(peek)
	if strings.Contains(s, "#EXTM3U") || strings.Contains(s, "#EXT-X-") {
		return false
	}
	if !looksBinary(s) {
		return false
	}
	// 常见媒体文件头：
	//   MP4/fMP4: 4-8 字节 "ftyp"（ISO BMFF 容器）
	//   FLV: 开头 "FLV"
	//   WebM/MKV: EBML 魔数
	//   MPEG-TS: 0x47 同步字节
	if len(s) >= 8 && s[4:8] == "ftyp" {
		return true
	}
	if strings.HasPrefix(s, "FLV") || strings.HasPrefix(s, "\x1a\x45\xdf\xa3") || (len(s) > 0 && s[0] == 0x47) {
		return true
	}
	// Content-Type 兜底：服务端声明了媒体类型但开头魔数不明确（如 TS 起始被截断）
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "video/") || strings.Contains(ct, "audio/") || strings.Contains(ct, "octet-stream")
}

// directExtFromURL 从 URL 路径提取媒体扩展名（直链文件用它决定输出扩展名）。
func directExtFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch ext := strings.ToLower(filepath.Ext(u.Path)); ext {
	case ".mp4", ".ts", ".flv", ".mkv", ".webm", ".mov", ".m4v", ".m4s", ".m4a", ".aac", ".mp3", ".avi", ".wmv":
		return ext
	}
	return ""
}

// playlistInfo 一次媒体播放列表解析结果（含分段、init 段与直播/点播标记）。
type playlistInfo struct {
	segments   []string  // 分片绝对 URL（播放列表内顺序）
	durs       []float64 // 各分片对应的 #EXTINF 时长（与 segments 一一对应，缺失为 0）
	hasMap     bool      // 存在 #EXT-X-MAP（fMP4 init 段声明）
	mapURI     string    // init 段绝对 URL（hasMap 时有效）
	hasEndList bool      // 存在 #EXT-X-ENDLIST（点播；缺失 = 直播/事件流）
	totalDur   float64   // EXTINF 时长累加（点播总时长 / 直播已见时长）
	mediaSeq   uint64    // #EXT-X-MEDIA-SEQUENCE（缺省 0；密钥无显式 IV 时派生 IV 用）
	key        *KeyInfo  // #EXT-X-KEY（nil = 明文流；METHOD=NONE 同样为 nil）
	// keyMalformed 播放列表声明了加密（METHOD 不是 NONE），但 URI 属性缺失/为空。
	// 这种行绝不能让 pl.key 停在 nil —— nil 与「明文流」在后续每条判断里完全
	// 不可区分：validatePlaylist 放行、ensureDecryptor(nil) 清空解密器、落盘校验的
	// ProbeInfo.Encrypted=false 让 generic 守卫也不设防。结果是密文被当明文拼进
	// 成品、全链路日志正常 —— 本项目反复出现的头号缺陷形态。用这个标记让
	// validatePlaylist 显式失败，而不是静默降级到明文管线。
	keyMalformed bool
	// multiKey 播放列表在"已经下过分片之后"换了另一组 KEY（key rotation）。
	// 当前管线只保留最后一条 key，前面的分片会被解成随机字节且毫无提示 ——
	// 用这个标记让调用方显式失败，而不是静默产出损坏文件。
	multiKey bool
	// segSeenForKey 当前 key 生效期间已见分片数：只有"用过之后才换 key"才算轮换，
	// 播放列表里每个分片前重复同一条 key 是合法且常见的写法。
	segSeenForKey int
	// hasByteRange 播放列表用了 #EXT-X-BYTERANGE（同一个文件按字节区间切成多段，
	// 单文件 HLS 的常见做法）。当前管线的语义是"一行分片 = 一个完整 URL"，
	// N 行会解析出同一个 URL 并各下一遍再顺序 append —— 体积放大 N 倍、时间轴
	// 完全错位，而且不会有任何报错。用这个标记让调用方显式失败。
	hasByteRange bool
	// mapByteRange #EXT-X-MAP 行带了 BYTERANGE 属性：init 段是某个文件的**字节片段**。
	// 当前实现只按 URI 把整个文件拉回来当 init 段 —— 偏移非 0 时那根本不是 init 段，
	// fMP4 的容器识别与轨道信息都跟着错，产物"能播但时间轴/轨道错"而日志全绿。
	// 与 hasByteRange 同源（同一个特性，落在 MAP 行上），并进同一条校验。
	// 只在 MAP 行同时给出 URI 时置位：没有 URI 就没有要取的 init 段，
	// 不该因为一行畸形声明拒掉一个本来正常的流。
	mapByteRange bool
	// hasDiscontinuity 播放列表用了 #EXT-X-DISCONTINUITY（或声明断点总数的
	// #EXT-X-DISCONTINUITY-SEQUENCE）：声明「接下来的分片与前面的不在同一条
	// 时间轴上」。编码参数变化、广告插入、直播中编码器重启都会产生它。当前管线
	// 把分片一律当一条连续时间轴顺序拼接，既不重置时间基准也不重编号 —— 断点
	// 之后的分片落在错误的时间位置，产物"能播但时间轴错"，且不会有任何报错。
	// 用这个标记让 validatePlaylist 显式失败，而不是静默拼出一条时间轴错位的文件。
	hasDiscontinuity bool
}

// sameKey 判定两条 #EXT-X-KEY 是否等价（METHOD/URI/IV/KEYFORMAT 全同）。
func sameKey(a, b *KeyInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Method == b.Method && a.URI == b.URI && bytes.Equal(a.IV, b.IV) &&
		normKeyFormat(a.KeyFormat) == normKeyFormat(b.KeyFormat)
}

// validatePlaylist 播放列表语义校验的唯一入口。
//
// 这里收的都是「当前实现不支持、但按错误语义硬跑会静默产出损坏文件」的 HLS 特性。
// 之所以合成一个入口而不是让调用方逐个调：管线与 CLI 两处调用点必须保持同步，
// 分成多个函数时新增一条校验就多一次「漏加一个调用点」的机会，而漏掉的后果
// 恰恰是这类静默损坏。新增校验请加在这个函数里，别加在调用点。
func validatePlaylist(pl playlistInfo) error {
	if err := ensureKeyDeclared(pl); err != nil {
		return err
	}
	if err := ensureSingleKey(pl); err != nil {
		return err
	}
	if err := ensureNoByteRange(pl); err != nil {
		return err
	}
	if err := ensureNoDiscontinuity(pl); err != nil {
		return err
	}
	return ensureIdentityKeyFormat(pl)
}

// ensureKeyDeclared 校验「声明了加密就必须给出可解析的 URI」。
//
// parseKeyLine 对「METHOD 非 NONE 但 URI 缺失/为空」的行返回 nil，若直接拿这个
// nil 当 pl.key，畸形声明与明文流在后续每条判断里都不可区分（validatePlaylist
// 放行、ensureDecryptor(nil) 清空解密器、ProbeInfo.Encrypted=false 让 generic
// 守卫也不设防），密文会被当明文拼进成品且全程无报错。因此畸形声明必须显式失败。
func ensureKeyDeclared(pl playlistInfo) error {
	if pl.keyMalformed {
		return fmt.Errorf("播放列表声明了加密（#EXT-X-KEY 的 METHOD 不是 NONE）但没有给出可用的 URI 属性：" +
			"无法获取密钥，拒绝下载（若该流实为明文，请让源站修正这条声明）")
	}
	return nil
}

// ensureIdentityKeyFormat 校验密钥格式是 identity（或缺省）。
//
// 非 identity 的 KEYFORMAT 表示 URI 指向的是密钥系统而不是裸密钥：商业 DRM 的
// 许可端点都属这种形态。此时 METHOD 往往仍写着
// AES-128，所以单看 METHOD 会一路放行——拉回来的东西被当作 16 字节 key 用，
// 产物是"能播但花屏/无声"的损坏文件，而日志一切正常。
func ensureIdentityKeyFormat(pl playlistInfo) error {
	if pl.key == nil || isIdentityKeyFormat(pl.key.KeyFormat) {
		return nil
	}
	return fmt.Errorf("播放列表声明了非 identity 的密钥格式（KEYFORMAT=%q）："+
		"该 URI 指向的是密钥系统而不是裸密钥，本工具无法解密，拒绝下载", pl.key.KeyFormat)
}

// ensureSingleKey 校验播放列表没有中途换 key。key rotation 需要按分片选择
// 解密器，当前实现不支持；显式报错远好过产出一个"前几段是噪声"的文件。
func ensureSingleKey(pl playlistInfo) error {
	if pl.multiKey {
		return fmt.Errorf("播放列表在中途更换了加密密钥（出现多组不同的 METHOD/URI/IV），当前版本只支持全程同一把密钥，无法安全解密")
	}
	return nil
}

// ensureNoByteRange 校验播放列表没有使用字节范围取流。
//
// #EXT-X-BYTERANGE 让多个分片行指向同一个 URL 的不同字节区间。当前管线把每行
// 当成独立文件整份下载，结果是同一个文件被下 N 遍再顺序拼接：体积放大 N 倍、
// 时间轴错位，且 validateOutput 的同步字节判据发现不了（每一份都是合法 TS）。
// 完整支持需要给分片附上 Range 语义，属于较大的改动；定版前先显式拒绝。
//
// 同一个特性落在 #EXT-X-MAP 行上（init 段是字节片段）危害更大：管线会把整个
// 文件当成 init 段，容器识别与轨道信息全错，而"产物能播"会让人以为没问题。
// 所以这**不是**只查分片行——两条来源并进同一个判据，新增来源时也往这里加。
func ensureNoByteRange(pl playlistInfo) error {
	if pl.hasByteRange || pl.mapByteRange {
		return fmt.Errorf("播放列表使用了 #EXT-X-BYTERANGE（单文件按字节区间切片），当前版本不支持字节范围分片，无法正确下载该流")
	}
	return nil
}

// ensureNoDiscontinuity 校验播放列表没有时间轴断点。
//
// #EXT-X-DISCONTINUITY 声明「接下来的分片与前面的不在同一条时间轴上」：编码
// 参数切换、广告插入、直播中编码器重启都会产生它。当前管线把分片一律当一条
// 连续时间轴顺序拼接，既不重置时间基准也不重编号 —— 断点之后的分片会落在
// 错误的时间位置，产物"能播但时间轴错"，而且不会有任何报错。完整支持需要处理
// 时间基准重置（并可能涉及编码参数切换后的重新封装），属于较大的改动；
// 与 #EXT-X-BYTERANGE 同类，定版前先显式拒绝。
func ensureNoDiscontinuity(pl playlistInfo) error {
	if pl.hasDiscontinuity {
		return fmt.Errorf("播放列表包含 #EXT-X-DISCONTINUITY（时间轴断点），当前版本不支持在断点处重置时间轴，无法保证产物的时间轴正确")
	}
	return nil
}

// ensureNotNestedMaster 校验取出的一跳子播放列表不是又一个 master。
//
// master 里 #EXT-X-STREAM-INF 之后的行是下一级播放列表的地址。当前实现只做
// 一跳（master → 媒体播放列表）：第二跳拿到的东西只验了「首行是 #EXTM3U」，
// 若它又是 master，就意味着里面那一串地址是**下一级播放列表**，而 parsePlaylist
// 会跳过所有以 # 开头的行、把这些地址当成分片逐个下载 —— 拼出来的成品是若干份
// m3u8 文本，日志却一切正常（本项目头号缺陷形态）。
//
// 归在「不支持特性显式拒绝」这一族，但没并进 validatePlaylist：master 的内容在
// 取出子播放列表那一刻就被替换掉了，出了 fetchPlaylist 再没有地方能看到它。
func ensureNotNestedMaster(subBody []byte, subURL string) error {
	if hasPlaylistTag(subBody, "#EXT-X-STREAM-INF") {
		return fmt.Errorf("master playlist 指向的子播放列表又是一个 master（多级嵌套），"+
			"当前版本只支持一跳，无法正确下载该流: %s", sanitizeURLForError(subURL))
	}
	return nil
}

// ensureNoSeparateAudio 校验选中的变体没有把音频放在独立的 #EXT-X-MEDIA 轨道上。
//
// master 可以用 #EXT-X-MEDIA:TYPE=AUDIO 把音频单独切成一条播放列表，再让变体通过
// AUDIO="<组名>" 引用它（demuxed 音频）。当前实现只下载单一播放列表、完全不认识
// #EXT-X-MEDIA，于是下回来的成品**没有声音**，而日志一切正常 —— 与 validatePlaylist
// 里那几条同源，只是这条的线索只在 master 里（同样出不了 fetchPlaylist）。
func ensureNoSeparateAudio(content, variantLine string) error {
	if masterHasSeparateAudioTrack(content, variantLine) {
		return fmt.Errorf("master playlist 把音频放在独立的 #EXT-X-MEDIA 轨道上" +
			"（选中的变体用 AUDIO 属性引用了带 URI 的音频组），当前版本只下载单一播放列表、" +
			"无法把独立音轨合并进来，会产出没有声音的成品")
	}
	return nil
}

// masterHasSeparateAudioTrack 判断选中的变体是否真的依赖一条独立音频轨道。
//
// 判据刻意收窄，否则会误伤一大片本来正常的流：
//   - 变体没写 AUDIO 属性 ⇒ 音频与视频在同一条播放列表里（muxed），正常，不拒；
//   - #EXT-X-MEDIA 的 URI 缺省 ⇒ 规范规定这就是「音频与该组视频同容器」的写法
//     （URI 是可选属性），正常，不拒；
//   - TYPE=SUBTITLES / CLOSED-CAPTIONS ⇒ 只是字幕轨，缺了不影响正片可播，不拒。
//
// 三者叠加（被引用 + 是音频 + 带 URI）才说明「关键内容确实在别的播放列表里」。
func masterHasSeparateAudioTrack(content, variantLine string) bool {
	group := firstGroup(streamAudioRe, variantLine)
	if group == "" {
		return false
	}
	for _, line := range strings.Split(strings.TrimPrefix(content, "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		// 必须带冒号：#EXT-X-MEDIA-SEQUENCE 是另一个标签，前缀相同但语义无关。
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		if !strings.EqualFold(firstGroup(mediaTypeRe, line), "AUDIO") {
			continue
		}
		if firstGroup(mediaGroupRe, line) != group {
			continue
		}
		if firstGroup(mediaURIRe, line) != "" {
			return true
		}
	}
	return false
}

var (
	// EXT-X-MEDIA 行的属性：TYPE / GROUP-ID / URI。
	mediaTypeRe  = regexp.MustCompile(`(?i)\bTYPE\s*=\s*"?([^",]+)"?`)
	mediaGroupRe = regexp.MustCompile(`(?i)\bGROUP-ID\s*=\s*"?([^",]+)"?`)
	mediaURIRe   = regexp.MustCompile(`(?i)\bURI\s*=\s*"?([^",]+)"?`)
	// EXT-X-STREAM-INF 行的 AUDIO 属性：引用音频组的组名。
	streamAudioRe = regexp.MustCompile(`(?i)\bAUDIO\s*=\s*"?([^",]+)"?`)
)

// hasPlaylistTag 按行判断播放列表正文里是否出现某个标签（忽略行首尾空白与 BOM）。
// 用整行前缀而不是 strings.Contains：标签名是别的词的前缀时（如 #EXT-X-MEDIA
// 与 #EXT-X-MEDIA-SEQUENCE）才不会被误判。
func hasPlaylistTag(body []byte, tag string) bool {
	s := strings.TrimPrefix(string(body), "\ufeff")
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), tag) {
			return true
		}
	}
	return false
}

// firstGroup 取正则第一个捕获组（无匹配返回空串）。
func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	return ""
}

// KeyInfo 一条 #EXT-X-KEY 声明（URI 已按播放列表 base 解析为绝对地址）。
type KeyInfo struct {
	Method string // AES-128 / SAMPLE-AES …（大写）
	URI    string // 密钥绝对 URL
	IV     []byte // 显式 IV（16 字节）；nil = 按 media sequence 派生
	// KeyFormat #EXT-X-KEY 的 KEYFORMAT 属性（缺省 = "identity"）。
	// 非 identity 表示 URI 指向的是「密钥系统」（商业 DRM 的许可服务），而不是
	// 16 字节裸密钥 —— 拿它当 AES-128 的 key 用，运气好是长度不合法报错，
	// 运气不好是静默解出随机字节。
	KeyFormat string
}

// fetchPlaylist 获取并返回媒体播放列表内容与其基准 URL。
// 返回的 base 是真正承载分片的那份播放列表的 URL：
// master playlist 会先选最高码率子流，base 即子流 URL（相对分片按其解析）。
//
// ctx 一路传到两次网络请求（含重试退避）：暂停/取消时不必等一轮重试跑完。
func (j *dlJob) fetchPlaylist(ctx context.Context) (content, base string, isDirect bool, err error) {
	// playlist 与分片一样经 segmentReferer 按 host 选用 Referer：有的站把 m3u8 也
	// 放在白名单型 CDN 上，一律用页面 Referer 会被 403（见 dlJob.segmentReferer）。
	body, isDirect, status, err := j.rt.httpGetPlaylist(ctx, j.m3u8URL, j.segmentReferer(j.m3u8URL))
	if err != nil {
		if status != 0 {
			return "", "", false, fmt.Errorf("HTTP %d: %w", status, err)
		}
		return "", "", false, err
	}
	if isDirect {
		return "", "", true, nil
	}
	content = string(body)
	base = j.m3u8URL

	// 检查是否是 master playlist
	if strings.Contains(content, "EXT-X-STREAM-INF") {
		subURL, err := j.pickHighestBitrateM3U8(content)
		if err != nil {
			return "", "", false, err
		}
		fmt.Printf("发现 master playlist，选择最高码率: %s\n", subURL)
		// 第二跳（master → 子播放列表）同样按 host 选用：子流可能落在与 m3u8
		// 不同的 CDN 上，用页面 Referer 同样可能被白名单型防盗链 403。
		subBody, _, err := j.rt.httpGetWithRetry(ctx, subURL, j.segmentReferer(subURL))
		if err != nil {
			return "", "", false, err
		}
		if !isM3U8Playlist(subBody) {
			return "", "", false, fmt.Errorf(
				"子播放列表响应不是 m3u8 内容（可能是网页/解析页）: %s", sanitizeURLForError(subURL))
		}
		if err := ensureNotNestedMaster(subBody, subURL); err != nil {
			return "", "", false, err
		}
		return string(subBody), subURL, false, nil
	}
	return content, base, false, nil
}

// parsePlaylist 解析媒体播放列表：分片 URL（相对路径按 base 解析）、
// #EXT-X-MAP init 段、#EXT-X-ENDLIST（有无决定直播/点播）、EXTINF 时长。
// 不返回错误：未知标签与非法行一律跳过，由调用方检查 segments 是否为空。
func parsePlaylist(m3u8Text, base string) playlistInfo {
	var pl playlistInfo
	// pendingDur 暂存最近一条 #EXTINF 的时长，等它对应的分片 URL 行出现时一并记入 durs
	pendingDur := 0.0
	// 剥 BOM：带 BOM 的播放列表首行 "\ufeff#EXTM3U" 不以 # 开头，
	// 会被当成分片 URL 产出一条垃圾条目
	m3u8Text = strings.TrimPrefix(m3u8Text, "\ufeff")
	lines := strings.Split(m3u8Text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			pl.hasEndList = true
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY"):
			// 两种标签都计入：#EXT-X-DISCONTINUITY（断点本身）与
			// #EXT-X-DISCONTINUITY-SEQUENCE（断点序列号，出现即说明有断点）。
			pl.hasDiscontinuity = true
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64); err == nil {
				pl.mediaSeq = v
			}
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			// 后一条 key 行覆盖前一条（含 METHOD=NONE 显式转为明文）。
			// 但"已经按旧 key 下过分片之后"再换 key 就是 key rotation：管线只
			// 保留最后一条，前面的分片会被解错。这里记录，交给 ensureSingleKey 报错。
			nk, malformed := parseKeyLine(line, base)
			if malformed {
				// 记录畸形：即便后面还有正常的 key 行，这份播放列表已经声明过
				// 一条无法解析的加密，整条流都不能按明文处理（见 keyMalformed 注释）。
				pl.keyMalformed = true
			}
			if !sameKey(pl.key, nk) {
				if pl.segSeenForKey > 0 {
					pl.multiKey = true
				}
				pl.segSeenForKey = 0
			}
			pl.key = nk
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			if u := mapURIAttr(line); u != "" {
				pl.hasMap = true
				pl.mapURI = resolveURL(base, u)
				// init 段的 BYTERANGE 与分片行的 BYTERANGE 是同一特性，
				// 但管线对 init 段只做"按 URI 整份下载"，同样按错误语义硬跑。
				pl.mapByteRange = pl.mapByteRange || mapHasByteRangeAttr(line)
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			// EXTINF 行在它对应的分片 URL 行之前出现，先记下等 URL 行配对
			pendingDur = parseEXTINFDuration(line)
			pl.totalDur += pendingDur
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			// 不解析区间，只记录"见过"——由 ensureNoByteRange 显式失败（见该函数注释）
			pl.hasByteRange = true
		case strings.HasPrefix(line, "#"):
			// 其它标签（EXT-X-TARGETDURATION 等）无需处理
		default:
			// 防御：含控制字符的行不是合法 URL（二进制响应切碎后的残片），跳过
			if !looksBinary(line) {
				pl.segments = append(pl.segments, resolveURL(base, line))
				pl.durs = append(pl.durs, pendingDur)
				pendingDur = 0
				pl.segSeenForKey++
			}
		}
	}
	return pl
}

// parseEXTINFDuration 解析 "#EXTINF:10.0," 中的秒数；解析失败或产生非有限值
// （如 "#EXTINF:NaN," / "#EXTINF:+Inf,"：strconv.ParseFloat 会成功返回但结果是
// NaN/+Inf，污染 totalDur 与 durs 令下游净时长比较失效）返回 0。
func parseEXTINFDuration(line string) float64 {
	rest := strings.TrimPrefix(line, "#EXTINF:")
	rest = strings.TrimSpace(rest)
	if i := strings.IndexAny(rest, ",\t "); i >= 0 {
		rest = rest[:i]
	}
	d, err := strconv.ParseFloat(rest, 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) {
		return 0
	}
	return d
}

// #EXT-X-KEY 行属性的正则（模块级编译，parseKeyLine 每行解析复用；
// 属性名按大小写不敏感匹配，容忍非规范播放列表）。
var (
	// keyMethodRe METHOD 属性值。规范要求裸枚举值，但非规范播放列表会写成
	// METHOD="AES-128"（带引号）或 METHOD = AES-128（等号两侧有空白）。
	//
	// 旧实现只认裸值，且**把"正则匹配不上"直接当成"没有 METHOD 属性"** ⇒ 带引号
	// 的 METHOD 会让整条加密声明蒸发：key=nil、keyMalformed 不置位、validatePlaylist
	// 放行 ⇒ 密文被当明文拼进成品、日志全绿（P1-7）。宽容度现与 keyURIRe /
	// keyFormatRe / mapURIRe 对齐 —— 那三个一直容忍带引号与裸值两种写法。
	keyMethodRe = regexp.MustCompile(`(?i)METHOD\s*=\s*"?([A-Za-z0-9-]+)"?`)
	// keyMethodAttrRe 宽松探测「这一行到底有没有声明 METHOD 属性」：只看属性名，
	// 不看值。**畸形判据必须建立在比解析器更宽的判据上** —— 拿同一个窄正则既当
	// 解析器又当判据，等于让"解析失败"与"根本没有这个属性"永远不可区分，而这两者
	// 的处置完全相反（前者必须显式失败，后者才是明文）。见 parseKeyLine。
	keyMethodAttrRe = regexp.MustCompile(`(?i)[:,\s]METHOD\s*=`)
	// keyURIRe URI 属性：规范要求 quoted-string，但非规范播放列表会写成裸值
	// （URI=k.ts,IV=…），两种都认——与下面 keyFormatRe 的宽容度对齐。旧实现只认
	// 带引号形态，裸值时 URI 取不到、整条声明被当成明文流（见 ensureKeyDeclared）。
	// 第 1 组是带引号形态，第 2 组是裸值（止于逗号/空白）。
	keyURIRe = regexp.MustCompile(`(?i)URI=(?:"([^"]*)"|([^",\s]*))`)
	keyIVRe  = regexp.MustCompile(`(?i)IV=0[xX]([0-9A-Fa-f]{32})`)
	// keyFormatRe KEYFORMAT 属性：规范要求 quoted-string，但非规范播放列表会写成
	// 裸值（KEYFORMAT=identity,），两种都认。第 1 组是带引号形态，第 2 组是裸值。
	keyFormatRe = regexp.MustCompile(`(?i)KEYFORMAT=(?:"([^"]*)"|([^",]*))`)
	// mapURIRe #EXT-X-MAP 的 URI 属性：与 keyURIRe 逐字对齐 —— 规范要求
	// quoted-string，但非规范播放列表会写成裸值（URI=init.mp4,BYTERANGE=…）。
	// 旧实现只认带引号形态，裸值时 hasMap/mapURI 取不到 ⇒ 容器判不出 fMP4，
	// 任务以「识别为 fMP4 分片流，但播放列表没有 #EXT-X-MAP 初始化段」失败：
	// 明明有 init 段却报"没有"（见 m3u8_mapuri_test.go）。
	// 第 1 组是带引号形态，第 2 组是裸值（止于逗号/空白）。
	mapURIRe = regexp.MustCompile(`(?i)URI=(?:"([^"]*)"|([^",\s]*))`)
)

// mapURIAttr 提取 #EXT-X-MAP 行的 URI 属性值，带引号与裸值两种形态都认。
// 属性缺失或值为空返回 ""（调用方据此判"没有 init 段"）。
func mapURIAttr(line string) string {
	m := mapURIRe.FindStringSubmatch(line)
	if len(m) != 3 {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return strings.TrimSpace(m[2])
}

// mapHasByteRangeAttr 判定 #EXT-X-MAP 行是否带 BYTERANGE 属性。
//
// 不在整行上正则匹配 `BYTERANGE=`：URI 属性是 quoted-string，查询串里完全可能
// 出现这个词（`URI="https://x/i.mp4?BYTERANGE=0"`），整行匹配会把合法流误判成
// 不支持。规范里属性用逗号分隔、quoted-string 内部的逗号不算分隔符，所以按引号
// 状态切段、再逐段比对**属性名**，就只认真正的属性位置。
func mapHasByteRangeAttr(line string) bool {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return false
	}
	attrs := line[i+1:]
	inQuote := false
	start := 0
	for j := 0; j < len(attrs); j++ {
		c := attrs[j]
		if c == '"' {
			inQuote = !inQuote
			continue
		}
		if c != ',' || inQuote {
			continue
		}
		if isByteRangeToken(attrs[start:j]) {
			return true
		}
		start = j + 1
	}
	return isByteRangeToken(attrs[start:])
}

// isByteRangeToken 判定一个属性片段是否为 BYTERANGE=…（属性名大小写不敏感）。
func isByteRangeToken(seg string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(seg)), "BYTERANGE=")
}

// parseKeyLine 解析 #EXT-X-KEY 行：METHOD、URI（相对路径按 base 解析）、
// 十六进制 IV 与 KEYFORMAT。
//
// 返回值刻意做成三态，调用方必须区分（把后两者不加区分地当成 nil = 明文，
// 正是「加密声明解析失败后静默按明文跑」的成因）：
//   - (nil, false)：行里没有 METHOD 属性（不是有效 KEY 声明），或 METHOD=NONE
//     （显式明文）——两者都该按明文处理；
//   - (key, false)：解析成功；
//   - (nil, true)：声明了加密但读不懂 —— METHOD 值缺失/为空、或 URI 缺失/为空。
//
// ⚠️ 第三态的判据取 **keyMethodAttrRe（宽）** 而不是 keyMethodRe（窄）：只要行里
// 出现了 METHOD 属性名，而我们没能解析出一个可用的方法值，就必须报畸形。
// 反过来用窄正则当判据时，"METHOD 的写法没被认出来"会退化成第一态（明文），
// 与它本该走的第三态（显式失败）正好相反 —— 这个漏洞 2026-09-24 用探针复现过
// （METHOD="AES-128" 与 METHOD = AES-128 两种写法 ⇒ key=nil + 校验放行）。
func parseKeyLine(line, base string) (*KeyInfo, bool) {
	m := keyMethodRe.FindStringSubmatch(line)
	if len(m) != 2 {
		if keyMethodAttrRe.MatchString(line) {
			return nil, true
		}
		return nil, false
	}
	method := strings.ToUpper(m[1])
	if method == "NONE" {
		return nil, false
	}
	uri := keyURIAttr(line)
	if uri == "" {
		return nil, true
	}
	k := &KeyInfo{Method: method, URI: resolveURL(base, uri)}
	if iv := keyIVRe.FindStringSubmatch(line); len(iv) == 2 {
		if b, err := hex.DecodeString(iv[1]); err == nil {
			k.IV = b
		}
	}
	if fm := keyFormatRe.FindStringSubmatch(line); len(fm) == 3 {
		if fm[1] != "" {
			k.KeyFormat = fm[1]
		} else {
			k.KeyFormat = strings.TrimSpace(fm[2])
		}
	}
	return k, false
}

// keyURIAttr 提取 #EXT-X-KEY 行的 URI 属性值：带引号（规范）与裸值（非规范）
// 两种形态都认。属性缺失或值为空返回 ""。
func keyURIAttr(line string) string {
	m := keyURIRe.FindStringSubmatch(line)
	if len(m) != 3 {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return strings.TrimSpace(m[2])
}

func (j *dlJob) pickHighestBitrateM3U8(content string) (string, error) {
	lines := strings.Split(content, "\n")
	var bestURL string
	// bestLine 记住选中变体的 #EXT-X-STREAM-INF 行：它的属性（如 AUDIO）决定
	// 这条流的关键内容是否被拆到了别的播放列表里。
	var bestLine string
	var bestBw int64 = -1
	base := urlBase(j.m3u8URL)
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			bw := extractBitrate(line)
			if i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if !strings.HasPrefix(next, "#") && next != "" {
					if bw > bestBw {
						bestBw = bw
						bestURL = resolveURL(base, next)
						bestLine = line
					}
				}
			}
		}
	}
	if bestURL == "" {
		return "", fmt.Errorf("master playlist 中未找到子 m3u8")
	}
	if err := ensureNoSeparateAudio(content, bestLine); err != nil {
		return "", err
	}
	return bestURL, nil
}

// bandwidthRe #EXT-X-STREAM-INF 的 BANDWIDTH 属性（模块级编译：
// 原先每次调用都 MustCompile 一遍，逐行解析 master playlist 时白烧 CPU）。
var bandwidthRe = regexp.MustCompile(`BANDWIDTH=(\d+)`)

func extractBitrate(line string) int64 {
	if m := bandwidthRe.FindStringSubmatch(line); len(m) == 2 {
		bw, _ := strconv.ParseInt(m[1], 10, 64)
		return bw
	}
	return 0
}

func resolveURL(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return baseURL.ResolveReference(refURL).String()
}

// ============================================================
// 下载任务上下文 dlJob
// 每个下载任务持有一份 dlJob，替代原先的共享全局变量(tempDir/计数/referer/url/limit)，
// 从而支持多个下载任务并行互不干扰。sharedClient 为只读复用的 http.Client 单例(并发安全)。
// ============================================================
