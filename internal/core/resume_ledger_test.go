// 恢复账本（flushedMark）与重启后「继续」的验收测试。
//
// 对应 docs/review-2026-09-20.md 的 P0-1 / P0-4 / P0-5 与
// docs/plan-2026-09-20-resume-semantics.md 的验收判据 A1 / A2 / A3。
//
// 三条缺陷同根因 —— **下载/录制运行中不落盘** —— 后果分三支：
//   - P0-1：强杀后点「停止」按内存计数判"无内容" ⇒ 删掉用户录到的东西；
//   - P0-4：强杀后点「继续」按陈旧断点 append ⇒ 成品出现重复内容；
//   - P0-5：直播 from=0 绕过守卫 ⇒ 产成品是两段录制拼接。
//
// 为什么断言要钉住**字节级**结果：本项目头号缺陷形态是"产物坏了但日志正常"，
// 所以每个用例都落在最终文件的字节上，而不是"有没有报错"。
package core

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newVodServer 起一个点播源站：/vod.m3u8 声明 n 片并带 ENDLIST，/seg/i 回 "SEG-i"。
func newVodServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:6.0,\nseg/%d\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "SEG-%s", strings.TrimPrefix(r.URL.Path, "/seg/"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// expectedVodBody 返回 n 片点播流的期望成品内容（与 newVodServer 一一对应）。
func expectedVodBody(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "SEG-%d", i)
	}
	return b.String()
}

// runVodTask 在 testStd 里跑一条点播任务并等它收尾。
func runVodTask(t *testing.T, id string, srv *httptest.Server, dir, filename string, st taskState) *taskEntry {
	t.Helper()
	st.id = id
	st.m3u8URL = srv.URL + "/vod.m3u8"
	st.filename = filename
	st.saveDir = dir
	if st.finalPath == "" {
		st.finalPath = filepath.Join(dir, filename)
	}
	if st.started.IsZero() {
		st.started = time.Now()
	}
	st.queued = true
	te := &taskEntry{rt: testStd, st: st}
	testStd.tasks[id] = te
	go runDiskPipeline(te)
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "点播任务收尾")
	waitLimiterDrained(t)
	return te
}

// TestStreamWriterFlushBytesMatchFileSize 账本字节数必须**等于** flush 那一刻文件里
// 真实存在的字节数。
//
// 为什么要求"精确相等"而不是"不超过"：恢复时按这个值把 .part 截断对齐 —— 报小了
// 会把已经落盘的内容重下一遍（重复），报大了会把用户已下的内容截掉。两个方向都坏
// 产物，所以它必须是精确值，不能是保守估计。
func TestStreamWriterFlushBytesMatchFileSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.ts")
	const segCount, segSize = 8, 300 << 10 // 8 × 300KB，跨过 1MB 刷新阈值

	var mu sync.Mutex
	var lastSegs, lastBytes int64
	flushes := 0
	sw, err := newStreamWriter(path, 0, 0, func(int) {}, func(segs int, b int64) {
		mu.Lock()
		lastSegs, lastBytes = int64(segs), b
		flushes++
		mu.Unlock()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < segCount; i++ {
		if err := sw.submit(i, bytes.Repeat([]byte{byte(i + 1)}, segSize)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		segs, b := lastSegs, lastBytes
		mu.Unlock()
		if b != fi.Size() {
			t.Fatalf("第 %d 片后：账本报 %d 字节、文件实际 %d 字节（不一致 ⇒ 恢复会截错位置）",
				i, b, fi.Size())
		}
		if want := segs * segSize; b != want {
			t.Fatalf("第 %d 片后：账本 (segs=%d, bytes=%d) 不同源（应满足 bytes==segs*%d）",
				i, segs, b, segSize)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(segCount * segSize); fi.Size() != want {
		t.Fatalf("最终文件 %d 字节 want %d", fi.Size(), want)
	}
	if got := sw.FlushedBytes(); got != fi.Size() {
		t.Fatalf("Close 后账本 %d ≠ 文件大小 %d", got, fi.Size())
	}
	mu.Lock()
	n := flushes
	mu.Unlock()
	if n == 0 {
		t.Fatal("整个写入过程一次都没 flush：本用例没覆盖到账本结算路径（断言无区分力）")
	}
}

// TestFlushedMarkIsAtomicPair 账本必须"成对"发布：并发读永远不能看到
// 「新分片序号 + 旧字节数」这种错配组合 —— 那会让恢复按错误位置截断 .part：
// 截少了是重复写入（P0-4），截多了是删掉用户已经下到的东西。
//
// 这条断言针对的是"新增字节字段但拆成两个独立原子值"这种改法（看起来能用、
// 并发下会错）。旧实现只有序号，没有这个问题；有了字节数才产生这个约束。
func TestFlushedMarkIsAtomicPair(t *testing.T) {
	j := &dlJob{rt: testStd}
	const perSeg = 4096

	var bad, reads int64
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(0); i < 50000; i++ {
			j.setFlushed(i, i*perSeg)
		}
		close(done)
	}()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				m := j.flushedMarkNow()
				atomic.AddInt64(&reads, 1)
				if m.bytes != m.segs*perSeg {
					atomic.AddInt64(&bad, 1)
				}
			}
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt64(&bad); n != 0 {
		t.Fatalf("读到 %d 次错配组合（分片序号与字节数不同源）：恢复会按错误位置截断 .part", n)
	}
	if atomic.LoadInt64(&reads) == 0 {
		t.Fatal("读者一次都没读到账本：本用例没有区分力")
	}
}

// TestResumeTruncatesPartToLedger 崩溃落在两次 flush 之间（.part 比账本长）时，
// 恢复必须先把 .part **截断到账本位置**，再从断点续写。
//
// 不截断就是 P0-4：O_APPEND 从文件末尾接着写，账本之后那几片被再写一遍 ——
// 探针实测产物形如 SEG-0SEG-1SEG-2SEG-1SEG-2（内容重复、时长变长、零报错）。
func TestResumeTruncatesPartToLedger(t *testing.T) {
	saveRestoreState(t)
	oldLimiter := testStd.limiter
	testStd.limiter = newResizableSem(1)
	t.Cleanup(func() { testStd.limiter = oldLimiter })

	const segs = 3
	srv := newVodServer(t, segs)
	dir := t.TempDir()
	final := filepath.Join(dir, "crash.ts")
	// 文件里已有 3 片，但账本只推进到第 1 片。
	if err := os.WriteFile(final+".part", []byte(expectedVodBody(segs)), 0644); err != nil {
		t.Fatal(err)
	}

	te := runVodTask(t, "crash", srv, dir, "crash.ts", taskState{
		stage: "已暂停", segDone: 1, flushedBytes: int64(len("SEG-0")),
	})

	te.mu.Lock()
	errMsg := te.st.errorMsg
	te.mu.Unlock()
	if errMsg != "" {
		t.Fatalf("续传不该失败: %s", errMsg)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if want := expectedVodBody(segs); string(data) != want {
		t.Fatalf("成品=%q want %q（未按账本截断 ⇒ 断点之后的内容被重复追加）", data, want)
	}
}

// TestLegacyStateWithoutLedgerRestartsFromZero 旧状态文件（v0.5.0 没有 flushedBytes
// 字段）的点播任务：字节账无法重建，必须**自动从 0 重下**并留下可见提示 ——
// 而不是按陈旧断点 append（产出重复内容），也不是报错停下（那是死路，违反验收 A1）。
func TestLegacyStateWithoutLedgerRestartsFromZero(t *testing.T) {
	saveRestoreState(t)
	oldLimiter := testStd.limiter
	testStd.limiter = newResizableSem(1)
	t.Cleanup(func() { testStd.limiter = oldLimiter })

	const segs = 3
	srv := newVodServer(t, segs)
	dir := t.TempDir()
	final := filepath.Join(dir, "legacy.ts")
	// 旧任务留下的半成品：内容确实是前两片，但账本里只有分片序号、没有字节数
	// （旧版本从没记过每片多少字节）。
	if err := os.WriteFile(final+".part", []byte("SEG-0SEG-1"), 0644); err != nil {
		t.Fatal(err)
	}

	te := runVodTask(t, "legacy", srv, dir, "legacy.ts", taskState{
		stage: "已暂停", segDone: 2, flushedBytes: 0, // ← 旧状态文件形态
	})

	te.mu.Lock()
	errMsg, note := te.st.errorMsg, te.st.restartNote
	te.mu.Unlock()
	if errMsg != "" {
		t.Fatalf("旧任务必须能自动走到终态（验收 A1：不留死路），实际失败: %s", errMsg)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if want := expectedVodBody(segs); string(data) != want {
		// 按陈旧断点 append 会得到 SEG-0SEG-1SEG-2SEG-2（或类似重复）
		t.Fatalf("成品=%q want %q（必须清空重下，而不是接着旧断点追加）", data, want)
	}
	if note == "" {
		t.Fatal("清空重下必须留下可见提示：悄悄重来会让用户以为程序在浪费带宽")
	}
}

// TestResumeResultMatchesFullDownload 验收判据 A2 的直接形式：同一源站，
// 「跑完整趟」与「崩在两次 flush 之间再恢复」两份成品必须**逐字节相同**。
func TestResumeResultMatchesFullDownload(t *testing.T) {
	saveRestoreState(t)
	oldLimiter := testStd.limiter
	testStd.limiter = newResizableSem(1)
	t.Cleanup(func() { testStd.limiter = oldLimiter })

	const segs = 4
	srv := newVodServer(t, segs)

	// 参照组：全新下载，没有 .part。
	dirA := t.TempDir()
	runVodTask(t, "full", srv, dirA, "full.ts", taskState{stage: "排队中"})
	full, err := os.ReadFile(filepath.Join(dirA, "full.ts"))
	if err != nil {
		t.Fatalf("参照组成品不存在: %v", err)
	}

	// 实验组：同一份内容，模拟"下到第 2 片时被强杀"，账本落后于文件。
	dirB := t.TempDir()
	finalB := filepath.Join(dirB, "resumed.ts")
	if err := os.WriteFile(finalB+".part", []byte(expectedVodBody(segs)), 0644); err != nil {
		t.Fatal(err)
	}
	runVodTask(t, "resumed", srv, dirB, "resumed.ts", taskState{
		stage: "已暂停", segDone: 2, flushedBytes: int64(len("SEG-0SEG-1")),
	})
	resumed, err := os.ReadFile(finalB)
	if err != nil {
		t.Fatalf("恢复组成品不存在: %v", err)
	}

	if !bytes.Equal(full, resumed) {
		t.Fatalf("恢复后的成品与完整下载不一致（验收 A2 不满足）:\n  完整=%q (%d 字节)\n  恢复=%q (%d 字节)",
			full, len(full), resumed, len(resumed))
	}
}

// TestFinishStopKeepsPartWhenLedgerIsStale P0-1 的核心断言：强杀后重启、用户点
// 「停止」，**绝不能**按内存计数把用户录到的东西删掉。
//
// 形态：任务从状态文件恢复出来、没有 job（pipeline 早已不在），状态里的断点是
// 下载开始前的值（运行中不落盘时的必然结果）。旧实现判 `segFlushedNow()==0`
// ⇒ 走"无内容"分支 ⇒ os.Remove(.part)。
func TestFinishStopKeepsPartWhenLedgerIsStale(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "killed.ts")
	const recorded = "SEG-RECORDED-1SEG-RECORDED-2"
	if err := os.WriteFile(final+".part", []byte(recorded), 0644); err != nil {
		t.Fatal(err)
	}
	te := &taskEntry{rt: testStd, st: taskState{
		id: "killed", live: true, paused: true, stage: "录制中断（程序异常退出）",
		filename: "killed.ts", saveDir: dir, finalPath: final,
		segDone: 0, flushedBytes: 0, // 陈旧/空账本
	}}
	testStd.tasks[te.st.id] = te

	finishStoppedTask(te)

	te.mu.Lock()
	st := te.st
	te.mu.Unlock()
	if !st.done {
		t.Fatalf("停止收尾没有把任务带到终态: %+v", st)
	}
	if st.errorMsg == "" {
		t.Fatal("中断原因必须保留：把它改写成'已完成'等于抹掉故障记录")
	}
	// 这条路的兜底原因同样是"上次进程被强杀"（状态文件里没有 errorMsg 可沿用）。
	// 学徒 2026-09-21 定：非用户主动退出统一写「程序异常退出导致中断」。
	if st.errorMsg != "程序异常退出导致中断" {
		t.Fatalf("兜底原因=%q want 「程序异常退出导致中断」", st.errorMsg)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("用户录到的内容被删掉了（P0-1 复现）: %v", err)
	}
	if string(data) != recorded {
		t.Fatalf("成品=%q want %q", data, recorded)
	}
	if _, err := os.Stat(final + ".part"); !os.IsNotExist(err) {
		t.Fatal("收尾后残留 .part")
	}
}

// TestChunkedPartModeWithLostBitmapRestarts 直链的分片模式用 WriteAt 稀疏写，
// 文件大小 ≠ 有效字节数。位图丢失后若按文件大小续传，那些洞就永远留在成品里
// （能播，但其中几段是坏数据，日志一切正常）。
//
// 判据必须取"上次记录的模式"：拿"现在能不能读到位图"当判据等于没有守卫 ——
// 位图丢失本身就是要防的形态。
//
// ⚠️ 2026-09-23：本用例的夹具原先用 body[:size/3]（有效前缀），而「按大小 append
// 一个有效前缀」与「清空重下」产出的字节完全相同 ⇒ 那条字节断言恒真、不产生信息。
// 已改为真·带洞夹具，并把「续传起点是否为 0」变成独立断言（rr.first()）。
// 空 partMode 那一态由 TestUnknownPartModeWithLostBitmapRestarts 覆盖 ——
// 两者合起来才是完整的守卫面（已知分片 / 模式未知）。
func TestChunkedPartModeWithLostBitmapRestarts(t *testing.T) {
	saveRestoreState(t)
	const size = 3 << 20
	body := directMP4Stub(size)
	rr := &rangeRecorder{}
	srv := newDirectRangeServer(rr, body)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	part := filepath.Join(dir, "movie.mp4.part")
	// ⚠️ 夹具必须是真机可达的形态：WriteAt 稀疏写留下的**带洞**文件。
	// 原先这里用 body[:size/3]（有效前缀），而"按大小 append 一个有效前缀"与
	// "清空重下"产出的字节**完全相同** ⇒ 那条字节相等断言恒真、不产生信息，
	// 整条用例的区分力全靠尾部 takeRestartNote() 撑着（2026-09-23 查实并改正）。
	holeyPart(t, part, body, size)

	job := &dlJob{rt: testStd, m3u8URL: srv.URL + "/movie.mp4"}
	job.setPartMode(partModeChunked)
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	assertSameAsSource(t, part, body)
	// 续传起点必须回到 0。这条比字节相等更强：它把"到底有没有清空"变成可观测事实 ——
	// 守卫失效时第一个请求会带 bytes=<残留长度>-，而两种写法的**最终字节可以相同**。
	if got := rr.first(); got != "" {
		t.Fatalf("分片模式位图丢失必须清空重下（首个请求不该带 Range），实际 Range=%q", got)
	}
	if note := job.takeRestartNote(); note == "" {
		t.Fatal("清空重下必须留下可见提示（悄悄重来会让人以为程序在浪费带宽）")
	}
}

// TestUnknownPartModeWithLostBitmapRestarts 空 partMode + 带洞 .part 必须清空重下。
//
// 这是 2026-09-23 修掉的真实缺陷：原判据是 `partModeNow() == partModeChunked`，
// 只认「已知是分片」。而 v0.5.0 已经有 WriteAt 分片下载、却没有 partMode 字段 ⇒
// 升级上来的任务读到空串 ⇒ 守卫整个不生效 ⇒ 带洞的 .part 被按文件大小续传，
// 那几段洞永久留在成品里（能播、日志一切正常）。
//
// 形态归因：**「对象不存在」与「对象为空」必须同等对待** —— 判据只写了非空的那
// 一半，等于给「不知道」发了张通行证。
func TestUnknownPartModeWithLostBitmapRestarts(t *testing.T) {
	saveRestoreState(t)
	const size = 3 << 20
	body := directMP4Stub(size)
	rr := &rangeRecorder{}
	srv := newDirectRangeServer(rr, body)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	part := filepath.Join(dir, "movie.mp4.part")
	holeyPart(t, part, body, size)

	job := &dlJob{rt: testStd, m3u8URL: srv.URL + "/movie.mp4"}
	// 关键：**不**调 setPartMode —— 模拟 v0.5.0 的状态文件里没有 partMode 字段。
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	assertSameAsSource(t, part, body)
	if got := rr.first(); got != "" {
		t.Fatalf("模式未知时不许按文件大小续传（首个请求 Range=%q）："+
			"空值的含义是「不知道」，不是「安全」", got)
	}
	if note := job.takeRestartNote(); note == "" {
		t.Fatal("清空重下必须留下可见提示")
	}
}

// TestStreamPartModeResumesBySize 与上面两条成对（"该放的放了"）：确知上次是
// 单连接连续追加时，文件大小**就是**有效字节数，续传是对的 —— 不许误伤。
//
// 这条挡的是"把守卫改成一律清空"的过度修正：那样成品仍然正确、上两条也照绿，
// 但每次重启都把已下好的部分白扔。判据取源站收到的 Range：续传必须从残留长度处
// 接着要，而不是重新要全量。
func TestStreamPartModeResumesBySize(t *testing.T) {
	saveRestoreState(t)
	const size = 3 << 20
	body := directMP4Stub(size)
	rr := &rangeRecorder{}
	srv := newDirectRangeServer(rr, body)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	part := filepath.Join(dir, "movie.mp4.part")
	const prefix = size / 2
	// stream 模式的残留是**有效前缀**（连续追加写的），所以这里用前缀是对的 ——
	// 与上面两条的带洞夹具形成对照，说明本组用例真的在区分两种残留形态。
	if err := os.WriteFile(part, body[:prefix], 0644); err != nil {
		t.Fatal(err)
	}

	job := &dlJob{rt: testStd, m3u8URL: srv.URL + "/movie.mp4"}
	job.setPartMode(partModeStream)
	if err := job.downloadDirect(context.Background(), part); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	assertSameAsSource(t, part, body)
	want := fmt.Sprintf("bytes=%d-", prefix)
	if got := rr.first(); got != want {
		t.Fatalf("确知 stream 模式必须从残留长度续传，Range 应为 %q，实际 %q", want, got)
	}
	if note := job.takeRestartNote(); note != "" {
		t.Fatalf("stream 模式的合法续传不该报「从头下载」：%q", note)
	}
}

// TestDirectResumeGuardIsFailSafe 结构守卫：直链"按文件大小续传"的前置判据必须是
// **fail-safe 方向**的 —— 只有确知上次是 stream 才放行。
//
// 为什么行为用例之外还要这一条：上面三条只证明"当前这三种取值的后果"，若有人把判据
// 换成另一种"看起来更严谨"的写法（如白名单式 `mode == partModeChunked || mode ==
// partModeStream` 再补分支），行为可能仍然全绿而语义已经反过来 —— 那正是本次缺陷的
// 成因。这里直接钉住判据本体与两个方向性文案。
func TestDirectResumeGuardIsFailSafe(t *testing.T) {
	src, err := os.ReadFile("download.go")
	if err != nil {
		t.Fatalf("读 download.go 失败: %v", err)
	}
	// 只看**代码**：本次修正的注释里为了说明来龙去脉，原样引用了旧判据
	// `partModeNow() == partModeChunked`。若把注释也算进去，这条守卫会因为
	// "文档写清楚了历史"而翻红 —— 断言的是接线，不是散文。
	// （与 limits_test.go 里"跳过 _test.go"同源：都要把非代码噪声排除掉。）
	var code strings.Builder
	for _, ln := range strings.Split(string(src), "\n") {
		if t := strings.TrimSpace(ln); strings.HasPrefix(t, "//") {
			continue
		}
		code.WriteString(ln)
		code.WriteByte('\n')
	}
	s := code.String()
	if strings.TrimSpace(s) == "" {
		t.Fatal("剥掉注释后源码为空 —— 读取或切分失效，下面的断言会恒真")
	}

	if !strings.Contains(s, "offset > 0 && mode != partModeStream") {
		t.Error("downloadDirect 的模式守卫不再是 fail-safe 写法" +
			"（应为 `offset > 0 && mode != partModeStream`）：只有确知上次是单连接连续追加，" +
			"文件大小才等于有效字节数")
	}
	// 反向：旧的"只认已知分片"写法不许复活（它漏掉空串 = 升级任务）。
	if strings.Contains(s, "partModeNow() == partModeChunked") {
		t.Error("守卫又回到了只认「已知是分片」的旧写法：partMode 为空（v0.5.0 升级任务）时整个失效")
	}
	// 两种情形各要有自己的可见文案：合并成一句含混的话会让用户分不清
	// "位图被删了"和"这是升级上来的旧任务"。
	for _, want := range []string{"续传位图丢失，已从头下载", "续传信息不完整，已从头下载"} {
		if !strings.Contains(s, want) {
			t.Errorf("download.go 缺少恢复提示文案 %q（且必须写在代码里，不是注释里）", want)
		}
	}
}

// rangeRecorder 记录直链源站收到的 Range 头（按请求顺序），用于断言"这次到底是
// 续传还是从头下"。光看最终字节分不出这两者，见各用例注释。
type rangeRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (rr *rangeRecorder) add(r *http.Request) {
	rr.mu.Lock()
	rr.seen = append(rr.seen, r.Header.Get("Range"))
	rr.mu.Unlock()
}

// first 返回首个请求的 Range 头；"<无请求>" 与 "" 可区分（后者 = 请求了但没带 Range）。
func (rr *rangeRecorder) first() string {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.seen) == 0 {
		return "<无请求>"
	}
	return rr.seen[0]
}

// newDirectRangeServer 起一个直链源站：记录 Range 并交给 serveDirectRange 应答。
func newDirectRangeServer(rr *rangeRecorder, body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rr.add(r)
		w.Header().Set("Content-Type", "video/mp4")
		serveDirectRange(w, r, body)
	}))
}

// holeyPart 造一个"WriteAt 稀疏写留下的带洞文件"：写 [0,size/6) 与 [size/3,size/2)，
// 中间 [size/6,size/3) 是洞。文件大小 = size/2，但有效字节数只有 size/3。
//
// 为什么必须是真·带洞而不是"有效前缀"：前缀文件按大小续传**也能得到正确结果**，
// 只有带洞文件才能把守卫的缺失暴露出来（这正是原用例夹具的盲区）。
func holeyPart(t *testing.T, path string, body []byte, size int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(body[:size/6], 0); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteAt(body[size/3:size/2], int64(size/3)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// assertSameAsSource 断言产物与源站字节逐字节相同。守卫缺失时中间那段洞会留下，
// 这条就会红 —— "产物坏了"的可观测事实，而不是"有没有报错"。
func assertSameAsSource(t *testing.T, part string, body []byte) {
	t.Helper()
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("成品 %d 字节 want %d：按文件大小续传会把空洞留在产物里（应清空重下）",
			len(got), len(body))
	}
}

// TestLiveResumeViaHandlerSavesRecordedPart 覆盖"用户真的点了「继续」"那一路：
// handleResume 对直播任务必须在后端分流到收尾（保存已录内容），
// 既不能返回错误（那是死路），也不能去续录（那会产出两段拼接的产物）。
func TestLiveResumeViaHandlerSavesRecordedPart(t *testing.T) {
	saveRestoreState(t)
	dir := t.TempDir()
	final := filepath.Join(dir, "live.ts")
	const recorded = "SEG-LIVE-1SEG-LIVE-2"
	if err := os.WriteFile(final+".part", []byte(recorded), 0644); err != nil {
		t.Fatal(err)
	}
	te := &taskEntry{rt: testStd, st: taskState{
		id: "liveresume", live: true, paused: true, stage: "录制中断（程序异常退出）",
		filename: "live.ts", saveDir: dir, finalPath: final,
		segDone: 0, flushedBytes: 0,
	}}
	testStd.tasks[te.st.id] = te

	rec := httptest.NewRecorder()
	testEngine().handleResume(rec, httptest.NewRequest(http.MethodGet, "/resume?id=liveresume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/resume 对直播任务应 200（分流到收尾），实际 %d: %s", rec.Code, rec.Body.String())
	}
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "直播 resume 收尾")

	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("已录的直播内容被丢弃: %v", err)
	}
	if string(data) != recorded {
		t.Fatalf("成品=%q want %q", data, recorded)
	}
}
