package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNativeHostExtensionIDMatchesManifest 防漂移：扩展清单里的 key 反算出的扩展 ID
// 必须与程序宣告的 allowed_origins 一致。
//
// 这两处一旦漂移，症状是"扩展能装上、其他功能都正常，但唤起永远失败且没有任何
// 报错"——浏览器只会静默拒绝来源不匹配的原生消息调用，排查起来极费时间。
func TestNativeHostExtensionIDMatchesManifest(t *testing.T) {
	p := filepath.Join("..", "..", "edge_extension", "manifest.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("扩展清单不可读，跳过: %v", err)
	}
	var m struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("解析扩展清单失败: %v", err)
	}
	if m.Key == "" {
		t.Fatal("扩展清单缺少 key 字段——没有它扩展 ID 会随机器变化，allowed_origins 就写不死")
	}
	der, err := base64.StdEncoding.DecodeString(m.Key)
	if err != nil {
		t.Fatalf("key 不是合法 base64: %v", err)
	}
	// 扩展 ID = SPKI 公钥 sha256 的前 16 字节，每个 nibble 映射到 a-p
	sum := sha256.Sum256(der)
	var id strings.Builder
	for i := 0; i < 16; i++ {
		id.WriteByte(byte('a' + (sum[i] >> 4)))
		id.WriteByte(byte('a' + (sum[i] & 0x0f)))
	}
	ids := nativeHostExtensionIDs()
	if len(ids) != 1 || ids[0] != id.String() {
		t.Fatalf("扩展 ID 漂移：清单算出 %q，程序宣告 %v", id.String(), ids)
	}
}

func TestHandleNativeHostRequestRejectsMalformed(t *testing.T) {
	resp := handleNativeHostRequest("go-catcher.exe", 7891, []byte("这不是 JSON"))
	if resp.OK {
		t.Fatal("非法 JSON 不应被判为成功")
	}
	if resp.Error == "" {
		t.Fatal("非法 JSON 应带错误说明")
	}
}

func TestHandleNativeHostRequestRejectsUnknownType(t *testing.T) {
	resp := handleNativeHostRequest("go-catcher.exe", 7891, []byte(`{"type":"launch-minesweeper"}`))
	if resp.OK {
		t.Fatal("未知消息类型不应被判为成功")
	}
	// 宿主只做"确保服务在跑"，不执行扩展传来的任何指令——这条断言钉住这个边界
	if !strings.Contains(resp.Error, "不支持") {
		t.Fatalf("错误信息应说明类型不受支持，实际: %q", resp.Error)
	}
}

// TestProbeLocalServiceDetectsAbsent 探一个必然没人监听的端口，应快速返回 false
// 而不是挂住或误报可用（误报会导致扩展以为服务在跑，接着在真实请求上失败）。
func TestProbeLocalServiceDetectsAbsent(t *testing.T) {
	if probeLocalService(1, 500*time.Millisecond) {
		t.Fatal("端口 1 上不可能有本服务，探测却报可用")
	}
}
