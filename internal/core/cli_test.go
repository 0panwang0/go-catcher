// CLI 参数解析测试。
package core

import "testing"

// TestParseCLI 全参数解析与默认值回退。
func TestParseCLI(t *testing.T) {
	oldProxy, oldOutput, oldConc := getProxyAddr(), outputFile, concurrency
	t.Cleanup(func() { setProxyAddr(oldProxy); outputFile, concurrency = oldOutput, oldConc })
	setProxyAddr("http://127.0.0.1:7890")
	outputFile = "output.ts"
	concurrency = 10

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

	// 未指定参数回退全局默认
	o2 := ParseCLI([]string{"--url=https://y/b.m3u8"})
	if o2.Proxy != "http://127.0.0.1:7890" || o2.Output != "output.ts" || o2.Concurrency != 10 {
		t.Fatalf("默认值错误: %+v", o2)
	}
	if o2.ServerMode || o2.Limit != 0 || o2.Port != 0 {
		t.Fatalf("默认零值错误: %+v", o2)
	}
}
