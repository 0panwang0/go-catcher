// /probe：服务端代拉 m3u8 文本（带 Referer 的完整浏览器头 + uTLS 指纹），
// 供浏览器扩展预检候选链接的画质/时长。
// 浏览器侧 fetch 对无 CORS 头的 CDN 只能拿到 opaque 空响应（"未预检"的根源），
// 预检必须与下载路径同能力，故由本地服务代理拉取。
package core

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const probeMaxBytes = 4 << 20 // playlist/master 体积很小，4MB 上限防御异常响应

const probeTimeout = 12 * time.Second // 预检是交互路径，快速失败优于长重试

func handleProbe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	referer := strings.TrimSpace(r.URL.Query().Get("referer"))
	if !validProbeTarget(target) {
		http.Error(w, "invalid url param", http.StatusBadRequest)
		return
	}

	req, err := newRequest(target, referer)
	if err != nil {
		http.Error(w, cleanURLParseErr(err, target).Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	resp, err := getClient().Do(req.WithContext(ctx))
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream status "+resp.Status, http.StatusBadGateway)
		return
	}
	body, err := readProbeBody(resp)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(body)
}

// validProbeTarget 仅接受完整的 http/https URL。
func validProbeTarget(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// readProbeBody 读取响应体并按 Content-Encoding 兜底解压 gzip
//（自定义 Transport + uTLS 握手下个别 CDN 会把压缩流原样返回，同 httpGetPlaylist）。
func readProbeBody(resp *http.Response) ([]byte, error) {
	rd := io.Reader(io.LimitReader(resp.Body, probeMaxBytes))
	if enc := resp.Header.Get("Content-Encoding"); strings.Contains(enc, "gzip") {
		if gz, err := gzip.NewReader(resp.Body); err == nil {
			defer gz.Close()
			rd = io.LimitReader(gz, probeMaxBytes)
		}
	}
	return io.ReadAll(rd)
}
