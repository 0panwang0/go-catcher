// 「不支持特性的显式拒绝」判据的接线守卫。
//
// 为什么需要它：这类判据的全部价值在「接线」上 —— 函数写得再对，只要没人调用，
// 缺陷照样静默通过（本项目头号形态）。行为测试证明不了这一点：测试自己会直接调用
// 这些函数（如 byterange 的用例就单测 ensureNoByteRange），于是「生产代码忘了接线」
// 时它们照样全绿。所以这里**只扫非测试源码** —— 正是这条限定让它有区分力。
//
// 两个入口的分工（原因见各自注释）：
//   - validatePlaylist：能从媒体播放列表本身看出来的；
//   - fetchPlaylist / pickHighestBitrateM3U8：只在 master 层看得见的
//     （master 的内容在取出子播放列表那一刻就被替换掉了）。
package core

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestUnsupportedFeatureChecksAreAllWired(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录失败: %v", err)
	}
	defRe := regexp.MustCompile(`(?m)^func (ensure[A-Za-z0-9_]+)\(`)
	var names []string
	var prod strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		// 必须先剥注释：这些函数的注释里为说明历史会原样引用旧写法，
		// 不剥就会凭空多出"调用点"，守卫退化成恒绿。
		src := stripLineComments(string(b))
		prod.WriteString(src)
		for _, m := range defRe.FindAllStringSubmatch(src, -1) {
			names = append(names, m[1])
		}
	}
	if len(names) < 6 {
		t.Fatalf("只找到 %d 个 ensure 判据（%v）—— 切分失效，这条守卫等于没跑", len(names), names)
	}
	body := prod.String()
	for _, n := range names {
		// 定义处自己占一次，所以「≥2」= 至少有一个调用点。
		if strings.Count(body, n+"(") < 2 {
			t.Errorf("%s 只有定义、没有任何生产调用点 —— 判据没接线就等于不存在", n)
		}
	}
}
