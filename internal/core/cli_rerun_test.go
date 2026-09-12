// CLI 重跑语义回归（2026-09-11 评审 P1-2）。
//
// 旧行为：streamWriter 以 O_APPEND 打开 `<输出>.part` 且 startIdx 恒为 0，
// 上一次失败留下的几百 MB 既不续传也不截断，新一轮从文件末尾再写一遍 ——
// 产物是一个"视频播两遍"的合法 TS，体积翻倍且校验发现不了。
//
// 现在明确为"重跑 = 重下"：残留 .part 在开工前被丢弃。
package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// mkTSSegment 造一个能通过 TS 校验的假分片（每 188 字节一个同步字节）。
func mkTSSegment(packets int) []byte {
	b := make([]byte, 188*packets)
	for i := 0; i < len(b); i += 188 {
		b[i] = 0x47
	}
	return b
}

func TestCLIRerunDiscardsLeftoverPart(t *testing.T) {
	seg0 := mkTSSegment(8)
	seg1 := mkTSSegment(9)
	wantSize := len(seg0) + len(seg1)

	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg0.ts\n#EXTINF:6.0,\nseg1.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(seg0) })
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(seg1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "v.ts")
	opts := CLIOptions{URL: srv.URL + "/vod.m3u8", Proxy: "direct", Output: out, Concurrency: 2}

	checkSize := func(step string) {
		t.Helper()
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("%s: 读产物失败: %v", step, err)
		}
		if len(got) != wantSize {
			t.Fatalf("%s: 产物 %d 字节，want %d（多出来的是被重复追加的旧内容）", step, len(got), wantSize)
		}
	}

	// 连跑三次：没修好时第 2 次就会翻倍
	for i := 1; i <= 3; i++ {
		if code := RunCLI(opts); code != 0 {
			t.Fatalf("第 %d 次 RunCLI 退出码=%d，want 0", i, code)
		}
		checkSize(fmt.Sprintf("第 %d 次", i))
	}

	// 模拟"上次跑到一半失败"：手工留下一个半成品 .part，再跑一次。
	//
	// 残留内容必须是**合法 TS**（同步字节齐全）而不是一堆 0：只有合法残留才会
	// 走到"校验通过 + 内容翻倍"的静默损坏路径；全 0 的残留会被 validateOutput
	// 当损坏拦下，那是运气，不是修复。
	part := out + ".part"
	if err := os.WriteFile(part, mkTSSegment(12), 0644); err != nil {
		t.Fatal(err)
	}
	if code := RunCLI(opts); code != 0 {
		t.Fatalf("存在残留 .part 时 RunCLI 退出码=%d，want 0", code)
	}
	checkSize("残留 .part 后重跑")
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("残留 .part 应被丢弃（成功收尾会 rename 成成品），实际仍存在: %v", err)
	}
}

// TestResetPartForRerunKeepsNothingOnFailure 删除失败必须中止，
// 否则会退化成"旧内容 + 新内容"那个 bug（而不是静默继续写）。
func TestResetPartForRerunLeavesNoStaleFile(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "v.ts.part")
	if err := os.WriteFile(part, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	// 位图一并清掉，避免被直链分片续传逻辑误用
	if err := os.WriteFile(chunkMetaPath(part), []byte("bm"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := resetPartForRerun(part); err != nil {
		t.Fatalf("正常清理不应报错: %v", err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Error(".part 应被删除")
	}
	if _, err := os.Stat(chunkMetaPath(part)); !os.IsNotExist(err) {
		t.Error(".part.meta 应被删除")
	}
	// 文件本就不存在时应是无操作（CLI 正常首跑路径）
	if err := resetPartForRerun(part); err != nil {
		t.Errorf("文件不存在时应无操作，得到 %v", err)
	}
}
