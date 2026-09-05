// m3u8 解析：URL 拼接与 dlJob 分片解析。
package core

import (
	"fmt"
	"net/url"
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

func (j *dlJob) fetchM3U8() (string, error) {
	body, status, err := httpGetWithRetry(j.m3u8URL, j.referer)
	if err != nil {
		return "", fmt.Errorf("HTTP %d: %w", status, err)
	}
	content := string(body)

	// 检查是否是 master playlist
	if strings.Contains(content, "EXT-X-STREAM-INF") {
		subURL, err := j.pickHighestBitrateM3U8(content)
		if err != nil {
			return "", err
		}
		fmt.Printf("发现 master playlist，选择最高码率: %s\n", subURL)
		subBody, _, err := httpGetWithRetry(subURL, j.referer)
		if err != nil {
			return "", err
		}
		return string(subBody), nil
	}
	return content, nil
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

func (j *dlJob) parseTSSegments(m3u8Text string) ([]string, error) {
	lines := strings.Split(m3u8Text, "\n")
	base := urlBase(j.m3u8URL)
	var list []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// 跳过空行和注释行（#开头）
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 所有非注释行都是分片 URL（可能是 .ts / .m4s / .jpeg 等任意格式）
		list = append(list, resolveURL(base, line))
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("m3u8 中未找到任何媒体分片")
	}
	return list, nil
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
