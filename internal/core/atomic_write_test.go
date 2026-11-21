package core

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// P1-2 的可见性质：位图落盘必须是原子的（临时文件 + Sync + rename）。
//
// 缺陷形态：saveChunkMeta 原先用 os.WriteFile 就地 O_TRUNC —— 写盘瞬间被强杀/
// 断电会留下**截断的 JSON**，loadChunkMeta 判定失败 ⇒ 位图不可用；而此刻 .part
// 是分片模式 WriteAt 稀疏写的产物（文件大小 ≠ 有效字节），续传只剩"从文件末尾
// 往后追加"，中间的空洞被永久留在成品里：能播、其中几段是坏数据、日志正常。
//
// 本文件分三层验：
//   1. 行为：成功写完整内容、覆盖旧内容、失败不留垃圾；
//   2. 性质：并发读**永远看不到半份内容**（这条直接抓"就地 O_TRUNC 写"）；
//   3. 收口：removeChunkMeta 连临时文件一起清，只有 .meta.tmp 时不当位图用。

func TestWriteFileAtomicWritesWholeContent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.meta")
	want := []byte(`{"total":20971520,"size":8388608,"done":[true,false,true]}`)
	if err := writeFileAtomic(p, want, 0644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("内容不符：%q", got)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("成功后不得残留 .tmp（rename 应已把它挪走）")
	}
}

// 覆盖已存在的目标：Windows 上 rename 覆盖是独立能力（MoveFileEx 的
// REPLACE_EXISTING），不测就会在"第二次落盘"时才暴露。
func TestWriteFileAtomicOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.meta")
	if err := os.WriteFile(p, []byte("旧内容"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("新内容"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "新内容" {
		t.Fatalf("应被覆盖为「新内容」，实际 %q", got)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("覆盖成功后不得残留 .tmp")
	}
}

// rename 失败（目标是个目录）时必须清掉临时文件：留着它既没用，又会在收尾时
// 被当成垃圾（而且下次 O_TRUNC 覆盖它时谁都看不见）。
func TestWriteFileAtomicCleansTmpWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("data"), 0644); err == nil {
		t.Fatal("目标是目录时 rename 应当失败")
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("rename 失败后必须清理 .tmp")
	}
}

// 写入期间必须能观察到 .tmp —— 这是"先写临时文件、再 rename"这条路径的直接证据，
// 而且是**确定性**的：就地 O_TRUNC 写永远不会有这个文件。
//
// 为什么不用"并发读内容"来验（第一版就是那么写的，实测抓不到就地写）：
// os.WriteFile 的「截断 → 写完」窗口在页缓存下只有微秒级，读者撞不进去 ⇒ 断言
// 恒绿却不产生信息。时序类断言在这里不可靠（反向验证 6 条里唯独它没红），
// 于是换成可观测的实现痕迹。
func TestWriteFileAtomicUsesTempFileWhileWriting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.meta")
	// 8MB：只为拉长写入窗口让观察循环稳定命中，不影响断言的性质
	body := []byte(strings.Repeat("A", 8<<20))
	if err := writeFileAtomic(p, body, 0644); err != nil {
		t.Fatal(err)
	}

	var seen atomic.Bool
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(p + ".tmp"); err == nil {
				seen.Store(true)
				return
			}
		}
	}()
	for i := 0; i < 3; i++ {
		if err := writeFileAtomic(p, body, 0644); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("第 %d 次原子写失败: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if !seen.Load() {
		t.Fatal("写入期间从未出现 .tmp —— 没走「临时文件 + rename」路径")
	}
}

// 收口：删位图必须连原子写的临时文件一起删，否则任务结束后它就是孤儿。
func TestRemoveChunkMetaClearsBitmapAndTmp(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "movie.mp4.part")
	if err := os.WriteFile(chunkMetaPath(part), []byte(`{"total":1,"size":1,"done":[true]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chunkMetaTmpPath(part), []byte("{半份"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := removeChunkMeta(part); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{chunkMetaPath(part), chunkMetaTmpPath(part)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 应被删除", filepath.Base(p))
		}
	}
	// 收尾路径在没有位图时也会调一次：不存在不算错
	if err := removeChunkMeta(part); err != nil {
		t.Fatalf("对不存在的位图应当无错：%v", err)
	}
}

// 只有 .meta.tmp（"在 Sync 与 Rename 之间被杀"的残留）时，绝不能把它当位图 ——
// 那是半份状态，按它续传等于按错误的断点拼产物。
func TestLoadChunkMetaIgnoresOnlyTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "movie.mp4.part")
	body := `{"total":20971520,"size":8388608,"done":[true,false,true]}`
	if err := os.WriteFile(chunkMetaTmpPath(part), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadChunkMeta(part); ok {
		t.Fatal("只有 .meta.tmp 时不得当作位图")
	}
}

// saveChunkMeta 的往返 + 不留临时文件（真正落盘的那条路径）。
func TestSaveChunkMetaRoundTripLeavesNoTmp(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "movie.mp4.part")
	m := &chunkMeta{Total: 20 << 20, Size: chunkSizeFixed, Done: []bool{true, false, true}}
	if err := saveChunkMeta(part, m); err != nil {
		t.Fatal(err)
	}
	got, ok := loadChunkMeta(part)
	if !ok {
		t.Fatal("刚写下的位图应当可读回")
	}
	if got.Total != m.Total || got.Size != m.Size || len(got.Done) != 3 ||
		!got.Done[0] || got.Done[1] || !got.Done[2] {
		t.Fatalf("往返内容不符：%+v", got)
	}
	if _, err := os.Stat(chunkMetaTmpPath(part)); !os.IsNotExist(err) {
		t.Fatal("saveChunkMeta 成功后不得残留 .meta.tmp")
	}
}

// 结构级防回退：位图不许再退回"就地 O_TRUNC 写"。这条是承诺型断言的替代品 ——
// 上面的性质断言已经能抓到就地写，这里额外钉住实现方式，避免有人绕开 writeFileAtomic
// 另写一条等价路径（那就又回到"没有机制在保证"的状态了）。
func TestChunkMetaGoesThroughAtomicWriter(t *testing.T) {
	raw, err := os.ReadFile("download.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if strings.Contains(src, "os.WriteFile(chunkMetaPath(") {
		t.Fatal("saveChunkMeta 不得用 os.WriteFile 就地写（P1-2，必须走 writeFileAtomic）")
	}
	if !strings.Contains(src, "writeFileAtomic(chunkMetaPath(partPath)") {
		t.Fatal("saveChunkMeta 应经 writeFileAtomic 落盘")
	}
}

// Sync 必须在 Rename 之前。这一步**不可行为测试**（测试里没法真的断电），
// 所以只能钉住实现顺序：rename 只让"目录项切换"不可分割，数据可能还在页缓存里，
// 少了 Sync 就会出现"文件名已是新的、内容却是空的"——比半份 JSON 更难发现。
func TestAtomicWriterSyncsBeforeRename(t *testing.T) {
	raw, err := os.ReadFile("output.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "func writeFileAtomic(")
	if start < 0 {
		t.Fatal("output.go 里没找到 writeFileAtomic")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("writeFileAtomic 函数体解析失败")
	}
	body := src[start : start+end]
	syncAt := strings.Index(body, "f.Sync()")
	renameAt := strings.Index(body, "os.Rename(")
	if syncAt < 0 {
		t.Fatal("writeFileAtomic 必须先 Sync 再 rename（否则断电会留下「文件名新、内容空」）")
	}
	if renameAt < 0 || syncAt > renameAt {
		t.Fatal("Sync 必须出现在 Rename 之前")
	}
}

// 删除点收口：位图只能经 removeChunkMeta 删。别处裸删 .meta 会漏掉 .meta.tmp，
// 任务结束后就留下一个再也无人覆盖的孤儿。
func TestNoStrayChunkMetaRemoval(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "download.go" {
			continue // download.go 是 removeChunkMeta 自己的家
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "os.Remove(") {
				continue
			}
			if strings.Contains(line, ".meta") || strings.Contains(line, "chunkMetaPath") {
				t.Fatalf("%s:%d 裸删位图，改用 removeChunkMeta（否则漏掉 .meta.tmp）: %s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("一个文件都没扫到：glob 或过滤条件写错了，这条守卫等于没跑")
	}
}

// TestPartRemovalAlwaysDropsBitmap 删 `.part` 的地方必须同时删位图。
//
// 为什么单列一条（P0-5）：上面那条守卫的判据是「行内含 `.meta` 字样」，而
// handlers.go 的失败转取消分支写的是 `os.Remove(part)` —— **它看不见**。于是留下
// 「.part 没了、位图还在」的组合：用户重新下载同一个视频时会认领同一个文件名
// （uniquePath 只看 .part 在不在），新建的 0 字节 .part 配上残留位图，被标记
// 「已完成」的片就永远不下载 ⇒ 成品带洞却报成功（downloadDirect 返回 nil）。
//
// 判据取「删的是 .part ⇒ 8 行内必须出现 removeChunkMeta」，而不是「两边数量相等」：
// 数量相等只能说明总数对不上，定位不到是哪一处。
//
// 边界（这条守卫抓不到的）：
//   - moveFile 跨卷回退里的 os.Remove(src)：那时位图早已删掉（分片模式成功收尾）
//     或本来就没有（单连接模式），故未纳入判据；
//   - 「删了 .part、位图隔了 8 行以上才删」的写法 —— 窗口是有意留的余量。
func TestPartRemovalAlwaysDropsBitmap(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// 只认「删 .part 路径」的行：变量名走本仓库既有命名（part / partPath / outPath），
	// 或行内直接拼了 ".part" 字面量。
	partRemoval := regexp.MustCompile(`os\.Remove\((part|partPath|outPath)\b|os\.Remove\([^)]*\.part`)
	const window = 8
	scanned, hits := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if !partRemoval.MatchString(line) {
				continue
			}
			if strings.Contains(line, "chunkMetaPath") || strings.Contains(line, "chunkMetaTmpPath") {
				continue // 这两行就是位图本体的删除
			}
			hits++
			end := i + window
			if end > len(lines) {
				end = len(lines)
			}
			paired := false
			for _, l := range lines[i:end] {
				if strings.Contains(l, "removeChunkMeta(") {
					paired = true
					break
				}
			}
			if !paired {
				t.Errorf("%s:%d 删了 .part 却没删位图（%d 行内不见 removeChunkMeta）："+
					"残留位图会让「重新下载同一个视频」悄悄跳片、成品带洞却报成功: %s",
					f, i+1, window, strings.TrimSpace(line))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("一个文件都没扫到：glob 或过滤条件写错了，这条守卫等于没跑")
	}
	if hits < 5 {
		t.Fatalf("只匹配到 %d 处删 .part —— 判据或命名约定已经漂移，守卫正在静默失效", hits)
	}
}
