// m3u8 解析：URL 拼接与 dlJob 分片解析。
package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
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
	segments   []string // 分片绝对 URL（播放列表内顺序）
	hasMap     bool     // 存在 #EXT-X-MAP（fMP4 init 段声明）
	mapURI     string   // init 段绝对 URL（hasMap 时有效）
	hasEndList bool     // 存在 #EXT-X-ENDLIST（点播；缺失 = 直播/事件流）
	totalDur   float64  // EXTINF 时长累加（点播总时长 / 直播已见时长）
	mediaSeq   uint64   // #EXT-X-MEDIA-SEQUENCE（缺省 0；密钥无显式 IV 时派生 IV 用）
	key        *KeyInfo // #EXT-X-KEY（nil = 明文流；METHOD=NONE 同样为 nil）
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
// 非 identity 的 KEYFORMAT 表示 URI 指向的是密钥系统而不是裸密钥：FairPlay 的
// skd:// URI、Widevine 的 license 端点等都是这种形态。此时 METHOD 往往仍写着
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

// ensureNoByteRange 校验播放列表没有使用字节范围分片。
//
// #EXT-X-BYTERANGE 让多个分片行指向同一个 URL 的不同字节区间。当前管线把每行
// 当成独立文件整份下载，结果是同一个文件被下 N 遍再顺序拼接：体积放大 N 倍、
// 时间轴错位，且 validateOutput 的同步字节判据发现不了（每一份都是合法 TS）。
// 完整支持需要给分片附上 Range 语义，属于较大的改动；定版前先显式拒绝。
func ensureNoByteRange(pl playlistInfo) error {
	if pl.hasByteRange {
		return fmt.Errorf("播放列表使用了 #EXT-X-BYTERANGE（单文件按字节区间切片），当前版本不支持字节范围分片，无法正确下载该流")
	}
	return nil
}

// KeyInfo 一条 #EXT-X-KEY 声明（URI 已按播放列表 base 解析为绝对地址）。
type KeyInfo struct {
	Method string // AES-128 / SAMPLE-AES …（大写）
	URI    string // 密钥绝对 URL
	IV     []byte // 显式 IV（16 字节）；nil = 按 media sequence 派生
	// KeyFormat #EXT-X-KEY 的 KEYFORMAT 属性（缺省 = "identity"）。
	// 非 identity 表示 URI 指向的是「密钥系统」（如 skd:// 的 FairPlay、
	// Widevine 的 license 服务），而不是 16 字节裸密钥 —— 拿它当 AES-128 的
	// key 用，运气好是长度不合法报错，运气不好是静默解出随机字节。
	KeyFormat string
}

// fetchPlaylist 获取并返回媒体播放列表内容与其基准 URL。
// 返回的 base 是真正承载分片的那份播放列表的 URL：
// master playlist 会先选最高码率子流，base 即子流 URL（相对分片按其解析）。
//
// ctx 一路传到两次网络请求（含重试退避）：暂停/取消时不必等一轮重试跑完。
func (j *dlJob) fetchPlaylist(ctx context.Context) (content, base string, isDirect bool, err error) {
	body, isDirect, status, err := j.rt.httpGetPlaylist(ctx, j.m3u8URL, j.referer)
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
		subBody, _, err := j.rt.httpGetWithRetry(ctx, subURL, j.referer)
		if err != nil {
			return "", "", false, err
		}
		if !isM3U8Playlist(subBody) {
			return "", "", false, fmt.Errorf(
				"子播放列表响应不是 m3u8 内容（可能是网页/解析页）: %s", sanitizeURLForError(subURL))
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
			if m := mapURIRe.FindStringSubmatch(line); len(m) == 2 && m[1] != "" {
				pl.hasMap = true
				pl.mapURI = resolveURL(base, m[1])
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			pl.totalDur += parseEXTINFDuration(line)
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			// 不解析区间，只记录"见过"——由 ensureNoByteRange 显式失败（见该函数注释）
			pl.hasByteRange = true
		case strings.HasPrefix(line, "#"):
			// 其它标签（EXT-X-TARGETDURATION 等）无需处理
		default:
			// 防御：含控制字符的行不是合法 URL（二进制响应切碎后的残片），跳过
			if !looksBinary(line) {
				pl.segments = append(pl.segments, resolveURL(base, line))
				pl.segSeenForKey++
			}
		}
	}
	return pl
}

// parseEXTINFDuration 解析 "#EXTINF:10.0," 中的秒数；解析失败返回 0。
func parseEXTINFDuration(line string) float64 {
	rest := strings.TrimPrefix(line, "#EXTINF:")
	rest = strings.TrimSpace(rest)
	if i := strings.IndexAny(rest, ",\t "); i >= 0 {
		rest = rest[:i]
	}
	d, _ := strconv.ParseFloat(rest, 64)
	return d
}

// #EXT-X-KEY 行属性的正则（模块级编译，parseKeyLine 每行解析复用；
// 属性名按大小写不敏感匹配，容忍非规范播放列表）。
var (
	keyMethodRe = regexp.MustCompile(`(?i)METHOD=([A-Za-z0-9-]+)`)
	// keyURIRe URI 属性：规范要求 quoted-string，但非规范播放列表会写成裸值
	// （URI=k.ts,IV=…），两种都认——与下面 keyFormatRe 的宽容度对齐。旧实现只认
	// 带引号形态，裸值时 URI 取不到、整条声明被当成明文流（见 ensureKeyDeclared）。
	// 第 1 组是带引号形态，第 2 组是裸值（止于逗号/空白）。
	keyURIRe = regexp.MustCompile(`(?i)URI=(?:"([^"]*)"|([^",\s]*))`)
	keyIVRe  = regexp.MustCompile(`(?i)IV=0[xX]([0-9A-Fa-f]{32})`)
	// keyFormatRe KEYFORMAT 属性：规范要求 quoted-string，但非规范播放列表会写成
	// 裸值（KEYFORMAT=identity,），两种都认。第 1 组是带引号形态，第 2 组是裸值。
	keyFormatRe = regexp.MustCompile(`(?i)KEYFORMAT=(?:"([^"]*)"|([^",]*))`)
	// mapURIRe #EXT-X-MAP 的 URI 属性（模块级编译，别在逐行循环里反复编译）
	mapURIRe = regexp.MustCompile(`URI="([^"]*)"`)
)

// parseKeyLine 解析 #EXT-X-KEY 行：METHOD、URI（相对路径按 base 解析）、
// 十六进制 IV 与 KEYFORMAT。
//
// 返回值刻意做成三态，调用方必须区分（把后两者不加区分地当成 nil = 明文，
// 正是「加密声明解析失败后静默按明文跑」的成因）：
//   - (nil, false)：行里没有 METHOD 属性（不是有效 KEY 声明），或 METHOD=NONE
//     （显式明文）——两者都该按明文处理；
//   - (key, false)：解析成功；
//   - (nil, true)：声明了加密 METHOD，但 URI 缺失/为空 —— 畸形，必须显式失败。
func parseKeyLine(line, base string) (*KeyInfo, bool) {
	m := keyMethodRe.FindStringSubmatch(line)
	if len(m) != 2 {
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
					}
				}
			}
		}
	}
	if bestURL == "" {
		return "", fmt.Errorf("master playlist 中未找到子 m3u8")
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
