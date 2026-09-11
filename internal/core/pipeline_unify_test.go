// 批次2 回归：CLI 与 GUI 必须走同一套容器装配。
//
// 此前 RunCLI 从不给 job.container / job.norm 赋值：同一份 fMP4 源，
// 命令行下载既不做 init 段的 mehd 占位（Normalize），收尾也不回填总时长，
// 产物与界面下载的不是同一个东西。本测试对同一份源跑两条路径并逐字节比对。
package core

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- 最小 fMP4 构造（仅测试用，结构与 internal/fmp4 的测试数据一致）----

func fmBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func fmFullBox(typ string, verflags uint32, payload []byte) []byte {
	b := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(b[0:], verflags)
	copy(b[4:], payload)
	return fmBox(typ, b)
}

// mkFMP4Init 构造最小 fMP4 init 段：ftyp + moov(mvhd + trak(tkhd+mdia(mdhd+minf/stbl/stsd)) + mvex(trex))。
// 关键是 mvex 里**没有** mehd —— 这正是 Normalize 要插入占位的地方，
// 于是"有没有走规范化"在产物字节上直接可见。
func mkFMP4Init(movieTS uint32, trackTS map[uint32]uint32) []byte {
	mvhdPayload := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhdPayload[8:], movieTS)
	mvhd := fmFullBox("mvhd", 0, mvhdPayload)

	var traks []byte
	for id, ts := range trackTS {
		tkhdPayload := make([]byte, 84)
		binary.BigEndian.PutUint32(tkhdPayload[8:], id)
		tkhd := fmFullBox("tkhd", 0x7, tkhdPayload)
		mdhdPayload := make([]byte, 24)
		binary.BigEndian.PutUint32(mdhdPayload[8:], ts)
		mdhd := fmFullBox("mdhd", 0, mdhdPayload)
		stsd := fmFullBox("stsd", 0, append([]byte{0, 0, 0, 1}, fmBox("hvc1", make([]byte, 8))...))
		mdia := fmBox("mdia", append(mdhd, fmBox("minf", fmBox("stbl", stsd))...))
		traks = append(traks, fmBox("trak", append(tkhd, mdia...))...)
	}
	trex := fmFullBox("trex", 0, make([]byte, 24))
	moov := fmBox("moov", append(append(mvhd, traks...), fmBox("mvex", trex)...))
	return append(fmBox("ftyp", []byte("isom")), moov...)
}

// TestCLIAndGUISharePipeline 同一份 fMP4 源，命令行与界面产物必须逐字节一致，
// 且都带 Normalize 插入的 mehd 占位。
func TestCLIAndGUISharePipeline(t *testing.T) {
	saveRestoreState(t)
	oldLimiter := testStd.limiter
	testStd.limiter = newResizableSem(1)
	t.Cleanup(func() { testStd.limiter = oldLimiter })

	initSeg := mkFMP4Init(1000, map[uint32]uint32{1: 90000})
	mediaSeg := append([]byte{0, 0, 0, 0x18}, []byte("moof")...)

	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n"+
			"#EXTINF:6.0,\nseg0.m4s\n#EXTINF:6.0,\nseg1.m4s\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) { w.Write(initSeg) })
	mux.HandleFunc("/seg0.m4s", func(w http.ResponseWriter, r *http.Request) { w.Write(mediaSeg) })
	mux.HandleFunc("/seg1.m4s", func(w http.ResponseWriter, r *http.Request) { w.Write(mediaSeg) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()

	// ---- GUI 路径（任务管线）----
	te := &taskEntry{rt: testStd, st: taskState{
		id: "tunify", queued: true, stage: "排队中", started: time.Now(),
		m3u8URL: srv.URL + "/vod.m3u8", filename: "gui.ts", saveDir: dir,
		finalPath: filepath.Join(dir, "gui.ts"),
	}}
	testStd.tasks[te.st.id] = te
	go runDiskPipeline(te)
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "GUI 下载完成")
	waitLimiterDrained(t)
	te.mu.Lock()
	guiSt := te.st
	te.mu.Unlock()
	if guiSt.errorMsg != "" {
		t.Fatalf("GUI 下载失败: %s", guiSt.errorMsg)
	}
	if ext := filepath.Ext(guiSt.finalPath); ext != ".mp4" {
		t.Fatalf("GUI 扩展名=%q want .mp4（应按容器修正）", ext)
	}

	// ---- CLI 路径（RunCLI）----
	cliOut := filepath.Join(dir, "cli.ts")
	if code := RunCLI(CLIOptions{
		URL: srv.URL + "/vod.m3u8", Proxy: "direct", Output: cliOut, Concurrency: 2,
	}); code != 0 {
		t.Fatalf("RunCLI 退出码=%d want 0", code)
	}
	cliFinal := filepath.Join(dir, "cli.mp4")

	guiData, err := os.ReadFile(guiSt.finalPath)
	if err != nil {
		t.Fatalf("读 GUI 产物: %v", err)
	}
	cliData, err := os.ReadFile(cliFinal)
	if err != nil {
		t.Fatalf("读 CLI 产物 %s: %v", cliFinal, err)
	}

	if !bytes.Contains(guiData, []byte("mehd")) || !bytes.Contains(cliData, []byte("mehd")) {
		t.Fatal("产物缺少 mehd 占位：说明 init 段没经过 Normalize（两条路径都应经过）")
	}
	if !bytes.Equal(guiData, cliData) {
		t.Fatalf("CLI 与 GUI 产物不一致：gui=%d 字节, cli=%d 字节", len(guiData), len(cliData))
	}
}
