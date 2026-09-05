// m3u8 解析：URL 拼接与 dlJob 分片解析。
package core

import (
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
	segments  []string // 分片绝对 URL（播放列表内顺序）
	hasMap    bool     // 存在 #EXT-X-MAP（fMP4 init 段声明）
	mapURI    string   // init 段绝对 URL（hasMap 时有效）
	hasEndList bool    // 存在 #EXT-X-ENDLIST（点播；缺失 = 直播/事件流）
	totalDur  float64  // EXTINF 时长累加（点播总时长 / 直播已见时长）
}

// fetchPlaylist 获取并返回媒体播放列表内容与其基准 URL。
// 返回的 base 是真正承载分片的那份播放列表的 URL：
// master playlist 会先选最高码率子流，base 即子流 URL（相对分片按其解析）。
func (j *dlJob) fetchPlaylist() (content, base string, isDirect bool, err error) {
	body, isDirect, status, err := httpGetPlaylist(j.m3u8URL, j.referer)
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
		subBody, _, err := httpGetWithRetry(subURL, j.referer)
		if err != nil {
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
	lines := strings.Split(m3u8Text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			pl.hasEndList = true
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			if m := regexp.MustCompile(`URI="([^"]*)"`).FindStringSubmatch(line); len(m) == 2 && m[1] != "" {
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

func extractBitrate(line string) int64 {
	re := regexp.MustCompile(`BANDWIDTH=(\d+)`)
	m := re.FindStringSubmatch(line)
	if len(m) == 2 {
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
