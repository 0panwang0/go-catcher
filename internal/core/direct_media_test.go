// R8：直链整体下载（MP4 等）产物的拒绝式校验。
//
// 缺陷形态：HLS 有三层 validateOutput，直链（CLI 与 GUI 两条路径）一层都没有
// ⇒ CDN 把 403/502 的 HTML 错误页当视频发回来时，会被当作成品保存、界面显示成功。
// 修法是"拒绝式"判据：只封堵**明显不是媒体**的开头特征，不用白名单（否则误杀合法直链）。
package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHead 把 head 原样写进临时文件并返回路径。
func writeHead(t *testing.T, head []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "media.bin")
	if err := os.WriteFile(p, head, 0o644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	return p
}

// TestValidateDirectMediaRejectsErrorPages：该拒的必须拒 —— 明显的错误页/空产物。
// 删掉任何一条判据（'<' / '{' '[' / 空 / 全空白）这里都会红。
func TestValidateDirectMediaRejectsErrorPages(t *testing.T) {
	cases := []struct {
		name string
		head []byte
	}{
		{"空文件", nil},
		{"HTML 错误页", []byte("<html><body>403 Forbidden</body></html>")},
		{"DOCTYPE 开头", []byte("<!DOCTYPE html>\n<html>...")},
		{"XML 错误体", []byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`)},
		{"带 BOM 的 HTML", append([]byte{0xEF, 0xBB, 0xBF}, []byte("<html>404</html>")...)},
		{"前导换行后的 HTML", []byte("\r\n  <html>502</html>")},
		{"JSON 错误体", []byte(`{"error":"not found"}`)},
		{"JSON 数组", []byte(`[{"code":403}]`)},
		{"纯空白", []byte("   \r\n\t  ")},
	}
	for _, c := range cases {
		if err := validateDirectMedia(writeHead(t, c.head)); err == nil {
			t.Errorf("%s：应拒绝，却放行了", c.name)
		}
	}
}

// TestValidateDirectMediaAcceptsRealMedia：成对用例 —— 真媒体必须放行。
// 缺这条则"任何输入都拒"也能让上面那条全绿（断言无区分力）。
//
// ⚠️ 特意放了 MP4（首字节就是 0x00）：判据若顺手把"首字节是 NUL"当异常，
// 会误杀所有 MP4 —— 正是拒绝式判据最容易踩的坑。
func TestValidateDirectMediaAcceptsRealMedia(t *testing.T) {
	mp4 := append([]byte{0x00, 0x00, 0x00, 0x20}, []byte("ftypisom")...)
	cases := []struct {
		name string
		head []byte
	}{
		{"MP4（首字节就是 0x00）", append(append([]byte{}, mp4...), make([]byte, 64)...)},
		{"TS 同步字节 0x47", append([]byte{0x47, 0x40, 0x11, 0x10}, make([]byte, 188)...)},
		{"FLV", append([]byte("FLV\x01"), make([]byte, 64)...)},
		{"WebM/EBML", append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 64)...)},
		{"无魔数的 octet-stream", []byte{0x9C, 0x02, 0xFF, 0x00, 0x7A, 0x13}},
	}
	for _, c := range cases {
		if err := validateDirectMedia(writeHead(t, c.head)); err != nil {
			t.Errorf("%s：不该拒绝，却拒了（误杀合法直链）: %v", c.name, err)
		}
	}
}

// TestDirectDownloadPathsCallValidateDirectMedia：接线断言。
// 只测纯函数的话，两条直链路径里那行调用被删掉照样全绿（R8 的原始形态就是"没有校验"）。
func TestDirectDownloadPathsCallValidateDirectMedia(t *testing.T) {
	for _, c := range []struct{ file, sig string }{
		{"pipeline.go", "func runDirectDownload(ctx context.Context, te *taskEntry, job *dlJob, st *taskState)"},
		{"cli.go", "func RunCLI(o CLIOptions) int"},
	} {
		body := strings.Join(sourceFuncBody(t, c.file, c.sig), "\n")
		if !strings.Contains(body, "validateDirectMedia") {
			t.Errorf("%s 的 %s 没有调用 validateDirectMedia —— 直链错误页会被当成媒体保存（R8）", c.file, c.sig)
		}
	}
}
