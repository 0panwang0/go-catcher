// m3u8 解析：URL 拼接与 dlJob 分片解析。
package core

import (
	"bytes"
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
	// multiKey 播放列表在"已经下过分片之后"换了另一组 KEY（key rotation）。
	// 当前管线只保留最后一条 key，前面的分片会被解成随机字节且毫无提示 ——
	// 用这个标记让调用方显式失败，而不是静默产出损坏文件。
	multiKey bool
	// segSeenForKey 当前 key 生效期间已见分片数：只有"用过之后才换 key"才算轮换，
	// 播放列表里每个分片前重复同一条 key 是合法且常见的写法。
	segSeenForKey int
}

// sameKey 判定两条 #EXT-X-KEY 是否等价（METHOD/URI/IV 全同）。
func sameKey(a, b *KeyInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Method == b.Method && a.URI == b.URI && bytes.Equal(a.IV, b.IV)
}

// ensureSingleKey 校验播放列表没有中途换 key。key rotation 需要按分片选择
// 解密器，当前实现不支持；显式报错远好过产出一个"前几段是噪声"的文件。
func ensureSingleKey(pl playlistInfo) error {
	if pl.multiKey {
		return fmt.Errorf("播放列表在中途更换了加密密钥（出现多组不同的 METHOD/URI/IV），当前版本只支持全程同一把密钥，无法安全解密")
	}
	return nil
}

// KeyInfo 一条 #EXT-X-KEY 声明（URI 已按播放列表 base 解析为绝对地址）。
type KeyInfo struct {
	Method string // AES-128 / SAMPLE-AES …（大写）
	URI    string // 密钥绝对 URL
	IV     []byte // 显式 IV（16 字节）；nil = 按 media sequence 派生
}

// fetchPlaylist 获取并返回媒体播放列表内容与其基准 URL。
// 返回的 base 是真正承载分片的那份播放列表的 URL：
// master playlist 会先选最高码率子流，base 即子流 URL（相对分片按其解析）。
func (j *dlJob) fetchPlaylist() (content, base string, isDirect bool, err error) {
	body, isDirect, status, err := j.rt.httpGetPlaylist(j.m3u8URL, j.referer)
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
		subBody, _, err := j.rt.httpGetWithRetry(subURL, j.referer)
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
			nk := parseKeyLine(line, base)
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
	keyURIRe    = regexp.MustCompile(`(?i)URI="([^"]*)"`)
	keyIVRe     = regexp.MustCompile(`(?i)IV=0[xX]([0-9A-Fa-f]{32})`)
	// mapURIRe #EXT-X-MAP 的 URI 属性（模块级编译，别在逐行循环里反复编译）
	mapURIRe = regexp.MustCompile(`URI="([^"]*)"`)
)

// parseKeyLine 解析 #EXT-X-KEY 行：METHOD、URI（相对路径按 base 解析）与
// 十六进制 IV。METHOD=NONE（明文）或缺 URI 返回 nil。
func parseKeyLine(line, base string) *KeyInfo {
	m := keyMethodRe.FindStringSubmatch(line)
	if len(m) != 2 {
		return nil
	}
	method := strings.ToUpper(m[1])
	if method == "NONE" {
		return nil
	}
	um := keyURIRe.FindStringSubmatch(line)
	if len(um) != 2 || um[1] == "" {
		return nil
	}
	k := &KeyInfo{Method: method, URI: resolveURL(base, um[1])}
	if iv := keyIVRe.FindStringSubmatch(line); len(iv) == 2 {
		if b, err := hex.DecodeString(iv[1]); err == nil {
			k.IV = b
		}
	}
	return k
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
