// CLI 参数解析测试。
package core

import "testing"

// TestParseCLI 全参数解析与默认值回退。
// G1 后 ParseCLI 的默认值是内置字面量（不再读任何运行时状态）；
// RunCLI 会创建一次性 Runtime 并应用解析结果。
func TestParseCLI(t *testing.T) {
	o := ParseCLI([]string{
		"--url=https://x/a.m3u8", "--referer=https://ref.example",
		"-c", "5", "-o", "out.ts", "--limit", "3", "--server", "--port", "9999",
	})
	if o.URL != "https://x/a.m3u8" {
		t.Fatalf("URL=%q", o.URL)
	}
	if o.Referer != "https://ref.example" {
		t.Fatalf("Referer=%q", o.Referer)
	}
	if o.Concurrency != 5 || o.Output != "out.ts" || o.Limit != 3 {
		t.Fatalf("o=%+v", o)
	}
	if !o.ServerMode || o.Port != 9999 {
		t.Fatalf("server/port 解析错误: %+v", o)
	}

	// 未指定参数回退内置默认
	o2 := ParseCLI([]string{"--url=https://y/b.m3u8"})
	if o2.Proxy != "system" || o2.Output != "output.ts" || o2.Concurrency != defaultConfig().SegConcurrency {
		t.Fatalf("默认值错误: %+v", o2)
	}
	if o2.ServerMode || o2.Limit != 0 || o2.Port != 0 {
		t.Fatalf("默认零值错误: %+v", o2)
	}
}
