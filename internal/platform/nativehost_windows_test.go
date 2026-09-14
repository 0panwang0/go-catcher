//go:build windows

package platform

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

// TestNativeMessageFrameLayout 钉住帧格式：4 字节小端长度前缀 + UTF-8 消息体。
// 格式写错的症状是"扩展连上了但永远收不到响应"，浏览器侧不会给出任何提示。
func TestNativeMessageFrameLayout(t *testing.T) {
	var buf bytes.Buffer
	body := []byte(`{"a":1}`)
	if err := WriteNativeMessage(&buf, body); err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	got := buf.Bytes()
	if len(got) != 4+len(body) {
		t.Fatalf("帧长度 = %d，期望 %d", len(got), 4+len(body))
	}
	if n := binary.LittleEndian.Uint32(got[:4]); n != uint32(len(body)) {
		t.Fatalf("长度前缀 = %d，期望 %d（必须是小端）", n, len(body))
	}
	if !bytes.Equal(got[4:], body) {
		t.Fatalf("消息体 = %q，期望 %q", got[4:], body)
	}
}

func TestNativeMessageRoundTrip(t *testing.T) {
	body := []byte(`{"type":"ensure-server"}`)
	var buf bytes.Buffer
	if err := WriteNativeMessage(&buf, body); err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	got, err := ReadNativeMessage(&buf)
	if err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("往返后消息体 = %q，期望 %q", got, body)
	}
}

func TestNativeMessageEmptyBody(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteNativeMessage(&buf, nil); err != nil {
		t.Fatalf("写空消息失败: %v", err)
	}
	got, err := ReadNativeMessage(&buf)
	if err != nil {
		t.Fatalf("读空消息失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空消息读出 %d 字节", len(got))
	}
}

// TestReadNativeMessageRejectsOversized 长度超限必须报错而不是照着分配：
// 否则一个损坏或伪造的长度前缀就能让我们申请几 GB 内存。
func TestReadNativeMessageRejectsOversized(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], nativeMessageMaxBytes+1)
	buf.Write(hdr[:])
	if _, err := ReadNativeMessage(&buf); err == nil {
		t.Fatal("超长帧应当报错")
	}
}

func TestReadNativeMessageTruncated(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 10)
	buf.Write(hdr[:])
	buf.WriteString("abc") // 声称 10 字节，只给 3 字节
	if _, err := ReadNativeMessage(&buf); err == nil {
		t.Fatal("截断的消息应当报错")
	}
}

// TestBuildNativeHostManifest 清单内容必须是浏览器认得的形状：
// type=stdio、path 是可执行文件绝对路径、allowed_origins 精确到扩展 ID。
func TestBuildNativeHostManifest(t *testing.T) {
	const exe = `C:\app\go-catcher.exe`
	m := buildNativeHostManifest(exe, []string{"abc", "  ", "", "def"})

	if m.Name != NativeHostName {
		t.Fatalf("name = %q，期望 %q（必须与注册表子项名一致）", m.Name, NativeHostName)
	}
	if m.Type != "stdio" {
		t.Fatalf("type = %q，期望 stdio", m.Type)
	}
	if m.Path != exe {
		t.Fatalf("path = %q，期望 %q", m.Path, exe)
	}
	want := []string{"chrome-extension://abc/", "chrome-extension://def/"}
	if !reflect.DeepEqual(m.AllowedOrigins, want) {
		t.Fatalf("allowed_origins = %v，期望 %v（空白项应被跳过）", m.AllowedOrigins, want)
	}
}

// TestNativeHostRegistryRootsCoverChromium 两个浏览器位置都要写：
// 扩展是标准 MV3，Chrome 与 Edge 都能装；只写一个会让另一个浏览器唤起不了。
func TestNativeHostRegistryRootsCoverChromium(t *testing.T) {
	if len(nativeHostRegistryRoots) < 2 {
		t.Fatalf("注册表位置只有 %d 个，Chromium 系浏览器应各写一份", len(nativeHostRegistryRoots))
	}
}
