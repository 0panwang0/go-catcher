// #EXTINF 时长解析的静默错值回归（2026-09-18 m3u8 fuzz 发现）。
//
// strconv.ParseFloat 对 "NaN" / "+Inf" / "-Inf" 会成功解析（err==nil）但结果是
// 非有限值。旧实现 `d, _ := strconv.ParseFloat(rest, 64)` 让这些值径直溜进
// totalDur 与 durs：下游净时长比较、时间轴推算全部被污染成 NaN，日志却全绿 ——
// 典型的"静默产出损坏"形态。此类输入从种子语料便触发，但只在发散式 fuzz
// （-fuzztime）下才被撞到，质量门只跑种子不探测断言，故这里用确定性用例锚定。
package core

import (
	"math"
	"testing"
)

func TestParseEXTINFDurationRejectsNonFinite(t *testing.T) {
	for name, line := range map[string]string{
		"NaN":     "#EXTINF:NaN,",
		"正无穷":     "#EXTINF:+Inf,",
		"负无穷":     "#EXTINF:-Inf,",
		"NaN 带尾注": "#EXTINF:NaN,something-else",
		"空值":      "#EXTINF:,",
		"纯冒号":     "#EXTINF:",
		"非数字":     "#EXTINF:abc,",
	} {
		t.Run(name, func(t *testing.T) {
			if d := parseEXTINFDuration(line); d != 0 {
				t.Fatalf("非有限/非法时长应归 0，得到 %v", d)
			}
		})
	}
}

func TestParseEXTINFDurationAcceptsFinite(t *testing.T) {
	for name, want := range map[string]float64{
		"整数秒": 10,
		"小数秒": 6.5,
		"带尾注": 8.25,
	} {
		line := map[string]string{
			"整数秒": "#EXTINF:10,",
			"小数秒": "#EXTINF:6.5,",
			"带尾注": "#EXTINF:8.25,title",
		}[name]
		if d := parseEXTINFDuration(line); d != want {
			t.Fatalf("合法时长应解析为 %v，得到 %v", want, d)
		}
	}
}

// TestExtinfAbsurdValueIsRejected（A-6）：**有限但荒谬**的时长同样要归 0。
//
// 缺陷形态：旧检查只挡 NaN/Inf，而 `1e300` 是有限值 ⇒ 一路累加进 job.gapMillis，
// `int64(sec*1000)` 溢出 int64、回绕成负数 ⇒ gapSecondsNow() 恒为负 ⇒ 缺口提示对
// 该任务**永久关闭**。产物中间有洞，界面一声不响 —— 头号形态在"数值累加"上的版本。
func TestExtinfAbsurdValueIsRejected(t *testing.T) {
	rejected := []struct{ name, line string }{
		{"1e300（有限但不可能是真的）", "#EXTINF:1e300,"},
		{"1e308", "#EXTINF:1e308,"},
		{"超过一小时上限", "#EXTINF:3601,"},
	}
	for _, c := range rejected {
		if d := parseEXTINFDuration(c.line); d != 0 {
			t.Errorf("%s 应归 0，得到 %v", c.name, d)
		}
	}
	// 反向：边界内必须照常接受（不能为了防溢出把正常时长的解析也关掉）
	accepted := map[string]float64{
		"#EXTINF:3600,": 3600, // 恰好在上限上
		"#EXTINF:59.9,": 59.9,
	}
	for line, want := range accepted {
		if d := parseEXTINFDuration(line); d != want {
			t.Errorf("%q 应解析为 %v，得到 %v", line, want, d)
		}
	}
}

// TestGapDurationCannotBePoisoned（A-6）：累加器本身也要有防线。
// 解析层挡住 `1e300` 只是第一道；addGapSeconds 是"往 int64 里加"的地方，
// 它的入参将来可能来自别处（估算函数、新特性），不能假设上游都干净。
func TestGapDurationCannotBePoisoned(t *testing.T) {
	job := &dlJob{}

	// 脏输入（溢出/非有限/负数）一律不许让累加值掉到 0 以下
	job.addGapSeconds(1e300)
	job.addGapSeconds(math.Inf(1))
	job.addGapSeconds(math.Inf(-1))
	job.addGapSeconds(math.NaN())
	job.addGapSeconds(-5)
	if got := job.gapSecondsNow(); got != 0 {
		t.Fatalf("脏输入不该计入缺口，got %v", got)
	}

	// 正常值必须真的计入（防"为了安全把功能关掉"）
	job.addGapSeconds(12)
	if got := job.gapSecondsNow(); got != 12 {
		t.Fatalf("gapSecondsNow=%.1f want 12", got)
	}

	// 单次封顶：一天。10 亿秒是荒谬值，不是"要报的缺口"，但也不能为负。
	big := &dlJob{}
	big.addGapSeconds(1e9)
	if got := big.gapSecondsNow(); got != 24*3600 {
		t.Fatalf("单次应封顶到一天（86400s），got %v", got)
	}

	// 累计封顶：七天。加一百次"一天"不该溢出，也不该无限增长。
	many := &dlJob{}
	for i := 0; i < 100; i++ {
		many.addGapSeconds(24 * 3600)
	}
	if got := many.gapSecondsNow(); got != float64(7*24*3600) {
		t.Fatalf("累计应封顶到七天（604800s），got %v", got)
	}
}

// TestParsePlaylistDurationInvariants 播种 fuzz（FuzzParseM3U8Playlist）里的关键
// 语义不变量：segments/durs 等长、totalDur 与每条 durs 有限 —— 让质量门也能断言，
// 而非只靠发散式 fuzz。构造覆盖各边界的输入直接跑 parsePlaylist。
func TestParsePlaylistDurationInvariants(t *testing.T) {
	bad := "#EXTM3U\n#EXTINF:NaN,\nseg0.ts\n#EXTINF:+Inf,\nseg1.ts\n#EXTINF:6.0,\nseg2.ts\n"
	pl := parsePlaylist(bad, fuzzBase)

	if len(pl.segments) != len(pl.durs) {
		t.Fatalf("segments/durs 长度不一致: %d vs %d", len(pl.segments), len(pl.durs))
	}
	if math.IsNaN(pl.totalDur) || math.IsInf(pl.totalDur, 0) {
		t.Fatalf("totalDur 非有限值: %v", pl.totalDur)
	}
	for i := range pl.durs {
		if math.IsNaN(pl.durs[i]) || math.IsInf(pl.durs[i], 0) {
			t.Fatalf("durs[%d] 非有限值: %v", i, pl.durs[i])
		}
	}
	if got := pl.durs[0]; got != 0 {
		t.Fatalf("NaN 时长分片应归 0，得到 %v", got)
	}
	if got := pl.totalDur; got != 6.0 {
		t.Fatalf("totalDur 应为有效分片时长累加 6.0，得到 %v", got)
	}
}

func TestParseKeyLineTriStateContract(t *testing.T) {
	// 畸形（malformed=true）但返回了非 nil key = 契约破坏（现实现不会，fuzz 守未来回归）。
	// 这里的确定性用例直接钉住"畸形声明必须显式失败，不得当明文/错钥静默放行"。
	lines := []string{
		"#EXT-X-KEY:METHOD=AES-128",          // 加密但无 URI → 畸形
		"#EXT-X-KEY:METHOD=SAMPLE-AES,URI=",  // URI 空 → 畸形
		"#EXT-X-KEY:METHOD=AES-128,URI=\"\"", // URI 空引号 → 畸形
	}
	for _, line := range lines {
		k, malformed := parseKeyLine(line, fuzzBase)
		if !malformed {
			t.Fatalf("行应判畸形: %q", line)
		}
		if k != nil {
			t.Fatalf("畸形行不得返回 key: %q key=%+v", line, k)
		}
	}
	// 合法加密 / 显式明文 不得误判畸形
	valid := map[string]*KeyInfo{
		"#EXT-X-KEY:METHOD=NONE":                  nil,
		"#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\"": {Method: "AES-128", URI: "https://cdn.example.test/v/k.bin"},
		"#EXT-X-KEY:METHOD=AES-128,URI=k.bin":     {Method: "AES-128", URI: "https://cdn.example.test/v/k.bin"},
		"#EXT-X-KEY:URI=k.bin":                    nil,
	}
	for line, want := range valid {
		k, malformed := parseKeyLine(line, fuzzBase)
		if malformed {
			t.Fatalf("合法行不得判畸形: %q", line)
		}
		if (want == nil) != (k == nil) {
			t.Fatalf("key 解析结果与预期不符: line=%q k=%+v", line, k)
		}
		if k != nil && (k.Method != want.Method || k.URI != want.URI) {
			t.Fatalf("key 值不符: line=%q got=%+v want=%+v", line, k, want)
		}
	}
}
