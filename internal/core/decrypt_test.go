// HLS AES-128 解密回归：#EXT-X-KEY 解析、注册表查找、解密器单元行为
// 与加密流 E2E（显式 IV / 序号派生 IV 两条路径）。
package core

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 单元：#EXT-X-KEY 行解析 ----

func TestParseKeyLine(t *testing.T) {
	cases := []struct {
		name          string
		line          string
		base          string
		wantNil       bool
		wantMalformed bool
		method        string
		uri           string
		wantIV        []byte
		wantNoIV      bool
	}{
		{
			name:   "显式IV大写hex",
			line:   `#EXT-X-KEY:METHOD=AES-128,URI="enc.key",IV=0x00000000000000000000000000000000`,
			base:   "https://vv.example.com/play/x/index.m3u8",
			method: "AES-128", uri: "https://vv.example.com/play/x/enc.key",
			wantIV: make([]byte, 16),
		},
		{
			name:   "无IV属性",
			line:   `#EXT-X-KEY:METHOD=AES-128,URI="https://cdn.example.com/key"`,
			base:   "https://vv.example.com/play/x/index.m3u8",
			method: "AES-128", uri: "https://cdn.example.com/key",
			wantNoIV: true,
		},
		{
			name:   "方法名归一为大写",
			line:   `#EXT-X-KEY:method=aes-128,URI="k.bin",IV=0xDEADBEEF0123456789ABCDEF00000010`,
			base:   "https://s.example.com/a/b.m3u8",
			method: "AES-128", uri: "https://s.example.com/a/k.bin",
			wantIV: mustHex(t, "DEADBEEF0123456789ABCDEF00000010"),
		},
		{
			name:    "METHOD=NONE视为明文",
			line:    `#EXT-X-KEY:METHOD=NONE,URI="enc.key"`,
			base:    "https://s.example.com/a.m3u8",
			wantNil: true,
		},
		{
			name:          "缺URI",
			line:          `#EXT-X-KEY:METHOD=AES-128`,
			base:          "https://s.example.com/a.m3u8",
			wantNil:       true,
			wantMalformed: true,
		},
		{
			name:    "缺METHOD",
			line:    `#EXT-X-KEY:URI="enc.key"`,
			base:    "https://s.example.com/a.m3u8",
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, malformed := parseKeyLine(tc.line, tc.base)
			if malformed != tc.wantMalformed {
				t.Fatalf("malformed=%v want %v（声明了加密但 URI 解析不出必须报畸形，不能静默降级为明文）",
					malformed, tc.wantMalformed)
			}
			if tc.wantNil {
				if k != nil {
					t.Fatalf("want nil, got %+v", k)
				}
				return
			}
			if k == nil {
				t.Fatal("want non-nil key")
			}
			if k.Method != tc.method || k.URI != tc.uri {
				t.Fatalf("method/uri = %q/%q, want %q/%q", k.Method, k.URI, tc.method, tc.uri)
			}
			if tc.wantNoIV && k.IV != nil {
				t.Fatalf("want nil IV, got %x", k.IV)
			}
			if tc.wantIV != nil && !bytes.Equal(k.IV, tc.wantIV) {
				t.Fatalf("IV = %x, want %x", k.IV, tc.wantIV)
			}
		})
	}
}

func TestParsePlaylistKeyAndMediaSeq(t *testing.T) {
	content := "#EXTM3U\n" +
		"#EXT-X-MEDIA-SEQUENCE:5\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="enc.key",IV=0x00000000000000000000000000000000` + "\n" +
		"#EXTINF:6.0,\nseg0.ts\n#EXTINF:6.0,\nseg1.ts\n#EXT-X-ENDLIST\n"
	pl := parsePlaylist(content, "https://cdn.example.com/live/index.m3u8")
	if pl.mediaSeq != 5 {
		t.Fatalf("mediaSeq = %d, want 5", pl.mediaSeq)
	}
	if pl.key == nil {
		t.Fatal("未解析出 #EXT-X-KEY")
	}
	if pl.key.URI != "https://cdn.example.com/live/enc.key" {
		t.Fatalf("key URI = %q（相对路径应按 base 解析）", pl.key.URI)
	}
	// METHOD=NONE 覆盖先前 key：明文流 key 必须为 nil
	pl = parsePlaylist("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:1.0,\ns.ts\n", "https://x.com/a.m3u8")
	if pl.key != nil {
		t.Fatalf("METHOD=NONE 后 key 应为 nil, got %+v", pl.key)
	}
	// 坏的 MEDIA-SEQUENCE 行不应报错，保持默认 0
	pl = parsePlaylist("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:abc\n#EXTINF:1.0,\ns.ts\n", "https://x.com/a.m3u8")
	if pl.mediaSeq != 0 {
		t.Fatalf("坏序号行 mediaSeq = %d, want 0", pl.mediaSeq)
	}
}

// ---- 单元：注册表 ----

func TestFindKeyMethod(t *testing.T) {
	for _, name := range []string{"AES-128", "aes-128", "Aes-128"} {
		if findKeyMethod(name) == nil {
			t.Fatalf("findKeyMethod(%q) 应命中 AES-128 条目", name)
		}
	}
	for _, name := range []string{"SAMPLE-AES", "AES-256", "", "unknown"} {
		if findKeyMethod(name) != nil {
			t.Fatalf("findKeyMethod(%q) 不应命中", name)
		}
	}
}

// ---- 单元：AES-128 解密器 ----

// aesCBCEncrypt 测试辅助：以 key/iv 做 AES-128-CBC 加密（构造密文分片用）。
func aesCBCEncrypt(t *testing.T, key, iv, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, plain)
	return ct
}

// fakeTSPlain 构造 4×188 字节（块对齐的 752 字节）TS 形态明文，每 188 字节 0x47 同步。
func fakeTSPlain(seed byte) []byte {
	plain := make([]byte, 4*188)
	for pkt := 0; pkt < 4; pkt++ {
		off := pkt * 188
		plain[off] = 0x47
		for i := 1; i < 188; i++ {
			plain[off+i] = byte(pkt) + seed + byte(i)
		}
	}
	return plain
}

func TestAES128DecryptorRoundtrip(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := mustHex(t, "00000000000000000000000000000000")

	t.Run("显式IV", func(t *testing.T) {
		plain := fakeTSPlain(0)
		d, err := newAES128Decryptor(KeySpec{Key: key, IV: iv})
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.DecryptSegment(123, aesCBCEncrypt(t, key, iv, plain)) // seq 被忽略
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatal("解密结果与明文不一致")
		}
	})

	t.Run("序号派生IV", func(t *testing.T) {
		plain := fakeTSPlain(7)
		d, err := newAES128Decryptor(KeySpec{Key: key}) // 无 IV → 按 seq 派生
		if err != nil {
			t.Fatal(err)
		}
		ct := aesCBCEncrypt(t, key, mediaSeqIV(7), plain)
		got, err := d.DecryptSegment(7, ct)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatal("序号派生 IV 解密不一致")
		}
		// 派生 IV 对 seq 敏感：错序号解出的首块必错
		wrong, err := d.DecryptSegment(8, ct)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(wrong[:16], plain[:16]) {
			t.Fatal("错误 seq 不应解出正确首块")
		}
	})
}

func TestAES128DecryptorErrors(t *testing.T) {
	if _, err := newAES128Decryptor(KeySpec{Key: []byte("short-key")}); err == nil {
		t.Fatal("非 16 字节 key 应报错")
	}
	d, err := newAES128Decryptor(KeySpec{Key: []byte("0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.DecryptSegment(0, []byte("not-16-aligned")); err == nil {
		t.Fatal("非块对齐密文应报错")
	}
	if got, err := d.DecryptSegment(0, nil); err != nil || got != nil {
		t.Fatalf("空分片应原样返回: got=%v err=%v", got, err)
	}
}

func TestMediaSeqIV(t *testing.T) {
	if got := mediaSeqIV(0); !bytes.Equal(got, make([]byte, 16)) {
		t.Fatalf("seq 0 IV = %x, want 全零", got)
	}
	want := mustHex(t, "0000000000000000000000000000000A")
	if got := mediaSeqIV(10); !bytes.Equal(got, want) {
		t.Fatalf("seq 10 IV = %x, want %x", got, want)
	}
}

func TestKeyFingerprint(t *testing.T) {
	a := &KeyInfo{Method: "AES-128", URI: "https://x/k", IV: []byte{1, 2}}
	b := &KeyInfo{Method: "AES-128", URI: "https://x/k", IV: []byte{1, 2}}
	c := &KeyInfo{Method: "AES-128", URI: "https://x/k2", IV: []byte{1, 2}}
	if a.fingerprint() != b.fingerprint() {
		t.Fatal("相同 key 指纹应相等")
	}
	if a.fingerprint() == c.fingerprint() {
		t.Fatal("不同 URI 指纹应不同")
	}
}

// ---- 单元：ensureDecryptor ----

func TestEnsureDecryptorUnsupportedMethod(t *testing.T) {
	j := &dlJob{rt: testStd}
	err := j.ensureDecryptor(context.Background(), &KeyInfo{Method: "SAMPLE-AES", URI: "https://x/k"})
	if err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("未注册方法应报不支持错误, got %v", err)
	}
	if j.decryptor != nil {
		t.Fatal("失败时不应装配解密器")
	}
}

// TestEnsureDecryptorIdempotent 幂等契约：key 指纹不变时重复调用不重拉 key
// （直播每轮轮询都会调用；key 轮换后按新指纹重建）。
func TestEnsureDecryptorIdempotent(t *testing.T) {
	var keyHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/k1":
			keyHits++
			w.Write([]byte("first-key-16byte"))
		case "/k2":
			keyHits++
			w.Write([]byte("second-key-16byt"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	j := &dlJob{rt: testStd}
	k1 := &KeyInfo{Method: "AES-128", URI: srv.URL + "/k1", IV: []byte{1}}
	if err := j.ensureDecryptor(context.Background(), k1); err != nil {
		t.Fatalf("首次装配: %v", err)
	}
	if err := j.ensureDecryptor(context.Background(), k1); err != nil {
		t.Fatalf("重复装配: %v", err)
	}
	if keyHits != 1 {
		t.Fatalf("key 拉取 %d 次，指纹不变应恰好 1 次", keyHits)
	}

	// key 轮换（URI 变化）：按新指纹重建并重新拉取
	k2 := &KeyInfo{Method: "AES-128", URI: srv.URL + "/k2", IV: []byte{2}}
	if err := j.ensureDecryptor(context.Background(), k2); err != nil {
		t.Fatalf("轮换装配: %v", err)
	}
	if keyHits != 2 {
		t.Fatalf("key 轮换后应重拉, hits=%d", keyHits)
	}

	// 明文播放列表：清空解密器
	if err := j.ensureDecryptor(context.Background(), nil); err != nil {
		t.Fatalf("明文清空: %v", err)
	}
	if j.decryptor != nil || j.decKeyID != "" {
		t.Fatalf("明文流应清空解密器, got %+v", j.decryptor)
	}
}

// ---- E2E：加密流下载（管线级）----

// encStreamServer 起一个加密 HLS 测试服务：
//   - /enc.key 返回 key；/index.m3u8 返回 key 行 + n 个分片 + ENDLIST
//   - 分片是 AES-128-CBC 加密的 TS 形态明文（每片 4×188 字节，0x47 同步）
//
// ivMode "explicit" 用全零 IV 属性；"seq" 不带 IV（按 mediaSeq+序号派生），
// 并带 #EXT-X-MEDIA-SEQUENCE 起始值验证序号基准换算。
type encStreamServer struct {
	srv      *httptest.Server
	key      []byte
	mediaSeq uint64
	plain    [][]byte // 各分片明文（校验落盘内容用）
	mu       sync.Mutex
	hits     map[string]int
}

func newEncStreamServer(t *testing.T, n int, ivMode string) *encStreamServer {
	t.Helper()
	e := &encStreamServer{key: []byte("e2e-test-key-16!"), mediaSeq: 5, hits: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/enc.key", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hits["key"]++
		e.mu.Unlock()
		w.Write(e.key)
	})
	var lines []string
	lines = append(lines, "#EXTM3U", "#EXT-X-VERSION:3")
	if ivMode == "seq" {
		lines = append(lines, fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d", e.mediaSeq))
	}
	for i := 0; i < n; i++ {
		plain := fakeTSPlain(byte(i * 3))
		e.plain = append(e.plain, plain)
		iv := make([]byte, 16)
		if ivMode == "explicit" {
			lines = append(lines, `#EXT-X-KEY:METHOD=AES-128,URI="enc.key",IV=0x00000000000000000000000000000000`)
		} else {
			iv = mediaSeqIV(e.mediaSeq + uint64(i))
			if i == 0 {
				lines = append(lines, `#EXT-X-KEY:METHOD=AES-128,URI="enc.key"`)
			}
		}
		ct := aesCBCEncrypt(t, e.key, iv, plain)
		path := fmt.Sprintf("/seg%d.ts", i)
		ctCopy := ct
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			e.mu.Lock()
			e.hits[path]++
			e.mu.Unlock()
			w.Write(ctCopy)
		})
		lines = append(lines, fmt.Sprintf("#EXTINF:6.0,\nseg%d.ts", i))
	}
	lines = append(lines, "#EXT-X-ENDLIST")
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, strings.Join(lines, "\n")+"\n")
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// runEncPipeline 用 runDiskPipeline 跑一次完整加密流任务，返回任务条目与落盘内容。
func runEncPipeline(t *testing.T, m3u8URL, saveDir string) (*taskEntry, []byte) {
	t.Helper()
	te := &taskEntry{rt: testStd, intent: intentNone}
	te.st = taskState{
		id: "tenc", queued: true, started: time.Now(),
		m3u8URL: m3u8URL, filename: "enc.ts", saveDir: saveDir,
		finalPath: filepath.Join(saveDir, "enc.ts"),
	}
	testStd.tasksMu.Lock()
	testStd.tasks[te.st.id] = te
	testStd.tasksMu.Unlock()
	t.Cleanup(func() {
		testStd.tasksMu.Lock()
		delete(testStd.tasks, te.st.id)
		testStd.tasksMu.Unlock()
	})

	go runDiskPipeline(te)
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "完成/失败")
	if te.st.errorMsg != "" {
		t.Fatalf("任务失败: %s", te.st.errorMsg)
	}
	data, err := os.ReadFile(te.st.finalPath)
	if err != nil {
		t.Fatalf("读取成品: %v", err)
	}
	return te, data
}

func TestEncryptedPlaylistE2E(t *testing.T) {
	t.Run("显式IV", func(t *testing.T) {
		saveRestoreState(t)
		oldLimiter, oldConc, oldRetries := testStd.limiter, testStd.concurrencyNow(), testStd.maxRetriesNow()
		testStd.limiter = newResizableSem(2)
		testStd.setDownloadTuning(4, 1)
		t.Cleanup(func() { testStd.limiter = oldLimiter; testStd.setDownloadTuning(oldConc, oldRetries) })

		e := newEncStreamServer(t, 3, "explicit")
		_, data := runEncPipeline(t, e.srv.URL+"/index.m3u8", t.TempDir())

		var want []byte
		for _, p := range e.plain {
			want = append(want, p...)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("落盘长度 %d, want %d（解密后明文应逐字节一致）", len(data), len(want))
		}
		waitLimiterDrained(t)
	})

	t.Run("序号派生IV带MEDIA-SEQUENCE偏移", func(t *testing.T) {
		saveRestoreState(t)
		oldLimiter, oldConc, oldRetries := testStd.limiter, testStd.concurrencyNow(), testStd.maxRetriesNow()
		testStd.limiter = newResizableSem(2)
		testStd.setDownloadTuning(4, 1)
		t.Cleanup(func() { testStd.limiter = oldLimiter; testStd.setDownloadTuning(oldConc, oldRetries) })

		// mediaSeq=5：分片 i 的 IV = mediaSeqIV(5+i)，验证管线把
		// MEDIA-SEQUENCE 偏移正确换算进每个分片的派生 IV
		e := newEncStreamServer(t, 4, "seq")
		_, data := runEncPipeline(t, e.srv.URL+"/index.m3u8", t.TempDir())

		var want []byte
		for _, p := range e.plain {
			want = append(want, p...)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("落盘长度 %d, want %d", len(data), len(want))
		}
		waitLimiterDrained(t)
	})
}

func TestEncryptedPlaylistKeyFetchFail(t *testing.T) {
	saveRestoreState(t)
	oldLimiter, oldConc, oldRetries := testStd.limiter, testStd.concurrencyNow(), testStd.maxRetriesNow()
	testStd.limiter = newResizableSem(2)
	testStd.setDownloadTuning(4, 1)
	t.Cleanup(func() { testStd.limiter = oldLimiter; testStd.setDownloadTuning(oldConc, oldRetries) })

	// key 永久 404：任务应失败且错误信息明确指向密钥
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"missing.key\"\n#EXTINF:6.0,\ns.ts\n#EXT-X-ENDLIST\n")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	te := &taskEntry{rt: testStd, intent: intentNone}
	te.st = taskState{
		id: "tkey404", queued: true, started: time.Now(),
		m3u8URL: srv.URL + "/i.m3u8", filename: "x.ts", saveDir: t.TempDir(),
		finalPath: filepath.Join(t.TempDir(), "x.ts"),
	}
	testStd.tasksMu.Lock()
	testStd.tasks[te.st.id] = te
	testStd.tasksMu.Unlock()
	t.Cleanup(func() {
		testStd.tasksMu.Lock()
		delete(testStd.tasks, te.st.id)
		testStd.tasksMu.Unlock()
	})

	go runDiskPipeline(te)
	waitTaskState(t, te, func(s taskState) bool { return s.done }, "失败终态")
	if te.st.errorMsg == "" || !strings.Contains(te.st.errorMsg, "密钥") {
		t.Fatalf("错误信息应指向密钥获取失败, got %q", te.st.errorMsg)
	}
	waitLimiterDrained(t)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestEnsureSingleKey 中途换 key（key rotation）必须显式失败，而不是静默解错
// 前面的分片；重复同一条 key（逐分片重复声明，合法且常见）不得误报。
func TestEnsureSingleKey(t *testing.T) {
	base := "https://x.com/a.m3u8"
	rotated := "#EXTM3U\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="k1"` + "\n#EXTINF:1.0,\ns1.ts\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="k2"` + "\n#EXTINF:1.0,\ns2.ts\n#EXT-X-ENDLIST\n"
	if err := ensureSingleKey(parsePlaylist(rotated, base)); err == nil {
		t.Fatal("中途换 key 应报错")
	}

	repeated := "#EXTM3U\n"
	for i := 0; i < 3; i++ {
		repeated += `#EXT-X-KEY:METHOD=AES-128,URI="k1"` + "\n#EXTINF:1.0,\ns.ts\n"
	}
	repeated += "#EXT-X-ENDLIST\n"
	if err := ensureSingleKey(parsePlaylist(repeated, base)); err != nil {
		t.Fatalf("重复同一条 key 不应报错: %v", err)
	}

	// IV 变化同样算轮换
	ivChanged := "#EXTM3U\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="k1",IV=0x00000000000000000000000000000000` + "\n#EXTINF:1.0,\ns1.ts\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="k1",IV=0x11111111111111111111111111111111` + "\n#EXTINF:1.0,\ns2.ts\n#EXT-X-ENDLIST\n"
	if err := ensureSingleKey(parsePlaylist(ivChanged, base)); err == nil {
		t.Fatal("换 IV 应报错")
	}

	// key 声明在首个分片之前切换（没有分片用过旧 key）不算轮换
	earlyChange := "#EXTM3U\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="k1"` + "\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="k2"` + "\n#EXTINF:1.0,\ns1.ts\n#EXT-X-ENDLIST\n"
	if err := ensureSingleKey(parsePlaylist(earlyChange, base)); err != nil {
		t.Fatalf("未有分片使用旧 key 时的切换不应报错: %v", err)
	}
}
