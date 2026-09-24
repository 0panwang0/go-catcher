// 托盘菜单只留「显示主窗口」「退出客户端」两项（需求单 v0.6.0 特性 1）。
//
// 守的是"入口消失"这件事，不是"少了两行"：服务启停与"在浏览器打开监控"两个入口去掉后，
// 菜单里复活、或换个地方长出来，都要红。
//
// ⚠️ 扫源码前必须剥掉**整行注释**：注释里为说明历史会原样引用旧文案（app.go 与 tray.go
// 的注释里就写着"在浏览器打开监控"），不剥就会因为"文档写清楚了历史"而翻红。
// 只剥整行注释，保留字符串字面量与行尾注释 —— 字符串里出现这些词才是真问题。
package app

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// trayMenuItemCtor 匹配一处托盘菜单项构造，捕获其文案。
var trayMenuItemCtor = regexp.MustCompile(`systray\.AddMenuItem\(\s*"([^"]*)"`)

// bannedEntries 本次删掉的两个入口的文案，生产源码里不该再出现。
var bannedEntries = []string{"启动 / 停止服务", "在浏览器打开监控"}

// stripLineComments 去掉整行注释（仅空白 + `//` 开头的行）。
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// nonTestGoSources app 包内所有非测试 Go 源文件。
func nonTestGoSources(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列 app 包源码失败: %v", err)
	}
	var out []string
	for _, f := range all {
		if strings.HasSuffix(f, "_test.go") {
			continue // 测试文件里会引用被禁文案，不属于生产源码
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("app 包里一个非测试源文件都没找到 —— 扫描范围失效，本测试会恒绿")
	}
	return out
}

// TestTrayMenuHasExactlyTwoItems 托盘右键菜单恰为「显示主窗口」「退出客户端」。
func TestTrayMenuHasExactlyTwoItems(t *testing.T) {
	b, err := os.ReadFile("tray.go")
	if err != nil {
		t.Fatalf("读 tray.go 失败: %v", err)
	}
	matches := trayMenuItemCtor.FindAllStringSubmatch(stripLineComments(string(b)), -1)
	if len(matches) == 0 {
		t.Fatal("没扫到任何 systray.AddMenuItem —— 锚点失效，本测试什么都没验")
	}
	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, m[1])
	}
	want := []string{"❌ 退出客户端", "📺 显示主窗口"}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("托盘菜单项 %q want %q —— 增删菜单项要同步需求单（v0.6.0 特性 1）", got, want)
	}
}

// TestServiceToggleEntryAbolishedInAppPackage 服务启停与"在浏览器打开监控"的入口
// 不许在 app 包的生产源码里复活（菜单里或别处）。
func TestServiceToggleEntryAbolishedInAppPackage(t *testing.T) {
	for _, f := range nonTestGoSources(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		src := stripLineComments(string(b))
		for _, w := range bannedEntries {
			if strings.Contains(src, w) {
				t.Errorf("%s 里出现了已废弃的入口文案 %q —— 该入口已删除（v0.6.0 特性 1）", f, w)
			}
		}
	}
}
