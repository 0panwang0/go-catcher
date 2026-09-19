package core

import (
	"strings"
	"testing"
)

// TestDirectProgressTextIsNotPlaceholder 直链下载中要按字节显示真实百分比
// （学徒 2026-09-19 定），而不是一律退回"下载中…"。
//
// 这条规则有两份实现：详细模式的 card() 与简略模式的 rowCard()，两处必须同步。
// 判据刻意要求 **`pct>0` 这个条件存在** —— 只断言"出现了 pct.toFixed"不够，
// 一个"无条件显示 pct"的实现（分母未知时显示 0.0%）同样能让那种断言过。
func TestDirectProgressTextIsNotPlaceholder(t *testing.T) {
	page := homePageHTML

	const anchor = "'下载直链文件中'"
	// 锚点后面紧跟的判据必须含 pct>0。两种写法的括号位置不同
	// （card 是 `'…' ? (pct>0`，rowCard 是 `'…') ? (pct>0`），
	// 所以按"锚点之后的窗口"检查，而不是钉某一种字符串形态。
	const window = 120

	n := 0
	for idx := 0; ; {
		i := strings.Index(page[idx:], anchor)
		if i < 0 {
			break
		}
		at := idx + i
		idx = at + len(anchor)
		n++
		end := at + window
		if end > len(page) {
			end = len(page)
		}
		seg := page[at:end]
		if !strings.Contains(seg, "pct>0") {
			runes := []rune(seg)
			if len(runes) > 40 {
				runes = runes[:40]
			}
			t.Errorf("第 %d 处「下载直链文件中」分支没有 pct>0 判据（分母未知时该退回占位符，已知时该给真数），"+
				"附近文本：%s", n, string(runes))
		}
	}
	if n != 2 {
		t.Errorf("「下载直链文件中」条件分支应有 2 处（card / rowCard 各一），找到 %d 处 —— "+
			"两处是同一条规则的两份实现，改一处必须同步改另一处", n)
	}
	for _, placeholder := range []string{
		"'下载直链文件中' ? '下载中…'",
		"'下载直链文件中') ? '…'",
	} {
		if strings.Contains(page, placeholder) {
			t.Errorf("直链下载中仍恒显示 %s —— 学徒要的是按字节的真实百分比", placeholder)
		}
	}
}
