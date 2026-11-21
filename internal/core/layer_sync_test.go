// taskState → persistedTask → taskStateDTO 三层结构的接线守卫（2026-09-23 质量审查）。
//
// 为什么需要：这三层是**手工同步**的 —— 加一个字段要改两个 struct 加两个转换
// 函数（再加前端契约），而 Go 对"具名字面量漏写字段"是**静默的**（补零值，
// 编译期不报错）。于是三类缺陷全都不可见：
//   - persistedTask 加了字段、collectPersisted 没赋值 ⇒ 永远落盘零值 ⇒ 重启后静默丢数据；
//   - persistedTask 加了字段、loadState 没读           ⇒ 存了没人用（死字段）；
//   - taskStateDTO 加了字段、toTaskStateDTO 没填       ⇒ 前端永远收到零值。
//
// 本项目的头号缺陷形态就是"产物坏了/数据丢了，而日志一切正常"，这三条都是它。
//
// ⚠ 判据不能是"三层字段集相等"：它们本来就**不该**相等（restartNote 有意只在
// DTO、queued/running/openPath 等有意不落盘、Pct/FileMissing 是派生字段）
// —— 强行相等会逼出一个假的不变量，然后为了维持它去改生产代码。
// 该守的是"每个字段都被**显式**处理过"：要么接线，要么登记为有意省略。
//
// 实现用**反射取字段名**（保证"加了字段就会被扫到"）+ 在生产函数体（**剥注释**）
// 里按**词边界**找标识符（`Done` 不会被 `SegDone` 命中）。
package core

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// sourceFuncBody 读生产源码，取出某个函数的正文并**剥掉注释**。
//
// 定位方式是"签名行 → 下一个顶格 }"：这三个函数的右花括号都顶格，体内没有嵌套
// 函数。找不到就 Fatal —— 切分失效时下面的断言会退化成恒真（本项目最忌讳的
// "绿着却不产生信息"）。
//
// 剥注释是必须的（2026-09-23 C1.6 的教训）：注释里为了说清来龙去脉常**原样引用**
// 字段名或旧写法，不剥会让断言因为"文档写清楚了"而假红/假绿。
func sourceFuncBody(t *testing.T, file, signature string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", file, err)
	}
	lines := strings.Split(string(data), "\n")

	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), signature) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s 里找不到函数 %q（切分失效 ⇒ 断言会恒真）", file, signature)
	}
	end := -1
	for i := start; i < len(lines); i++ {
		if lines[i] == "}" {
			end = i
			break
		}
	}
	if end < 0 {
		t.Fatalf("%s 里函数 %q 找不到顶格的结束花括号", file, signature)
	}

	body := make([]string, 0, end-start+1)
	for _, l := range lines[start : end+1] {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i] // 行尾注释也一并剥掉
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		body = append(body, l)
	}
	if len(body) < 5 {
		t.Fatalf("%s 的函数 %q 剥注释后只剩 %d 行（切分失效）", file, signature, len(body))
	}
	return body
}

// identsIn 报告 fields 里哪些名字在正文中以**独立标识符**出现过。
// 按词边界匹配：`Done` 不会被 `SegDone` 命中，`partMode` 不会被 `partModeNow` 命中。
func identsIn(body []string, fields []string) map[string]bool {
	alts := make([]string, 0, len(fields))
	for _, f := range fields {
		alts = append(alts, regexp.QuoteMeta(f))
	}
	re := regexp.MustCompile(`\b(?:` + strings.Join(alts, "|") + `)\b`)
	found := make(map[string]bool)
	for _, m := range re.FindAllString(strings.Join(body, "\n"), -1) {
		found[m] = true
	}
	return found
}

// structFields 用反射取结构体字段名 —— 编译期的真实字段，不是文本解析，
// 所以"加了字段却忘了处理"必然被扫到。
func structFields(v any) []string {
	rt := reflect.TypeOf(v)
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		out = append(out, rt.Field(i).Name)
	}
	return out
}

// TestPersistedTaskRoundTripIsComplete：persistedTask 的每个字段都必须既被写
// （collectPersisted）又被读（loadState）。只写不读 = 死字段；只读不写 = 永远
// 落盘零值 —— 重启后数据无声消失，且没有任何症状。
func TestPersistedTaskRoundTripIsComplete(t *testing.T) {
	fields := structFields(persistedTask{})
	if len(fields) < 20 {
		t.Fatalf("只反射到 %d 个 persistedTask 字段（少于 20）：断言会退化成恒真", len(fields))
	}
	written := identsIn(sourceFuncBody(t, "persist.go", "func (r *Runtime) collectPersisted()"), fields)
	read := identsIn(sourceFuncBody(t, "persist.go", "func (r *Runtime) loadState()"), fields)

	for _, f := range fields {
		if !written[f] {
			t.Errorf("persistedTask.%s 没被 collectPersisted 写入：落盘的永远是零值，重启后静默丢数据", f)
		}
		if !read[f] {
			t.Errorf("persistedTask.%s 没在 loadState 里读取：存了没人用（死字段）", f)
		}
	}
}

// TestTaskStatePersistenceIsExplicit：taskState 的每个字段要么落盘（出现在
// collectPersisted），要么在 persistedOmissions 里写明理由 —— "忘了持久化"与
// "想过、决定不持久化"必须在代码里长得不一样，否则后者会掩护前者。
func TestTaskStatePersistenceIsExplicit(t *testing.T) {
	fields := structFields(taskState{})
	if len(fields) < 25 {
		t.Fatalf("只反射到 %d 个 taskState 字段（少于 25）：断言会退化成恒真", len(fields))
	}
	written := identsIn(sourceFuncBody(t, "persist.go", "func (r *Runtime) collectPersisted()"), fields)
	inState := make(map[string]bool, len(fields))
	for _, f := range fields {
		inState[f] = true
	}

	for _, f := range fields {
		if written[f] {
			continue
		}
		if _, ok := persistedOmissions[f]; !ok {
			t.Errorf("taskState.%s 既没落盘、也没登记在 persistedOmissions：加字段时必须显式二选一"+
				"（忘了持久化会让它在重启后无声消失）", f)
		}
	}
	// 反向两条：登记表是一份会撒谎的文档 —— 它必须只包含真实存在的字段，
	// 且不许声称某个"其实已经落盘"的字段不落盘。
	for name, reason := range persistedOmissions {
		if !inState[name] {
			t.Errorf("persistedOmissions 里的 %q 不是 taskState 的字段（改名/删字段后忘了同步登记表）", name)
		}
		if written[name] {
			t.Errorf("persistedOmissions 里的 %q 声称不落盘，但它出现在 collectPersisted 里（登记表与实现不符）", name)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("persistedOmissions[%q] 没写理由：登记表的价值就在理由上", name)
		}
	}
}

// TestTaskStateDTOIsFullyPopulated：taskStateDTO 的每个字段（含派生字段
// Pct / FileMissing）都必须出现在 toTaskStateDTO 的返回字面量里。漏填的字段是
// 静默零值 —— 前端会把它当成"确实是 false / 0 / 空串"渲染。
func TestTaskStateDTOIsFullyPopulated(t *testing.T) {
	fields := structFields(taskStateDTO{})
	if len(fields) < 20 {
		t.Fatalf("只反射到 %d 个 taskStateDTO 字段（少于 20）：断言会退化成恒真", len(fields))
	}
	joined := strings.Join(sourceFuncBody(t, "task.go", "func toTaskStateDTO(t taskState)"), "\n")
	for _, f := range fields {
		// 具名字面量赋值形如 `Pct: pct,` / `Started:  t.started.Format(...)`
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(f) + `\s*:`).MatchString(joined) {
			t.Errorf("taskStateDTO.%s 没在 toTaskStateDTO 里赋值：前端会收到静默零值", f)
		}
	}
}
