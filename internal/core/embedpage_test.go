// 内嵌页跳转的定点回归：监控页 / 设置页在 iframe 内部的跳转必须原样带上当前 query。
//
// 背景：/ 与 /settings 默认发 X-Frame-Options: DENY 防点击劫持；GUI 客户端里这两个
// 页面嵌在外壳 iframe 中，框架内跳转（点 ⚙ 去设置、点「返回监控」、保存后自动回跳）
// 若不带上内嵌豁免键，就会被浏览器拦成白屏。这类"新增一个跳转忘了带键"的回归，
// 编译期和其余测试都发现不了，所以用扫描把不变量钉住。
package core

import (
	"regexp"
	"strings"
	"testing"
)

// frameNavRe 匹配框架内导航的赋值语句，捕获整段右值表达式。
// 不捕获到 `;` 或换行 —— 键是以 `+ location.search` 拼接的，只取字符串字面量会漏判。
var frameNavRe = regexp.MustCompile(`location\.(?:href|replace|assign)\s*=\s*([^;\n]+)`)

func TestEmbeddedPagesPropagateEmbedKey(t *testing.T) {
	pages := map[string]string{
		"web/index.html":    homePageHTML,
		"web/settings.html": settingsPageHTML,
	}

	for name, page := range pages {
		hits := frameNavRe.FindAllStringSubmatch(page, -1)
		if len(hits) == 0 {
			// 一个跳转都没有说明页面结构变了，用例已失效——必须报出来而不是静默跳过
			t.Errorf("%s 未找到任何框架内跳转，本用例已失效（页面结构变了？）", name)
			continue
		}
		for _, h := range hits {
			expr := strings.TrimSpace(h[1])
			if !strings.Contains(expr, "location.search") {
				t.Errorf("%s 的框架内跳转 `location.href = %s` 没带 location.search："+
					"缺内嵌豁免键会被 X-Frame-Options 拦成白屏", name, expr)
			}
		}
	}
}
