// 「📂 打开文件夹」的位置判据（学徒 2026-09-21 三轮回合后定稿）：
//
//	**有「🗑 移除记录」的段落 ⇒ 📂 排在 🗑 前面（倒数第二）；没有 🗑 ⇒ 📂 自己占末位。**
//	⇒ 一句话：📂 一直贴着末尾走，只有 🗑 出现时它往左让一格。
//	（等价说法：📂 后面要么什么都没有，要么紧跟着 🗑，而 🗑 必须是最后一个。）
//
// 为什么单独再钉一条，而不是只靠 web_action_order_test.go 那 14 个 case：
// 那些 case 是**逐状态列举**的，将来新增一个状态而忘了补 case，约束就静默漏掉。
// 这一条按分支自动切分，新分支自动纳入检查；而且它对**每一处** `openFolder(` 都查
// （直播终态那种"一支两排法"的分支里有两处 📂，只查第一处会漏掉第二处）。
//
// ⚠️ 这条规则收敛了三轮，别按印象改回去（详见 web_action_order_test.go 头注释）：
// 旧判据是"📂 后紧邻必须是销毁类（✕ 或 🗑）"，那等于要求"销毁类压轴"，与定稿冲突 ——
// 定稿里没有 🗑 的状态是 `… ✕ 📂`，✕ 排在 📂 **之前**。
//
// 两条相邻约束不在本测试职责内：
//   - 已取消态**不许**有 📂（文件已删，没有文件夹可开）→ TestCanceledRowKeepsSingleButton；
//   - 主操作/取消/移除各自的配色 → TestControlButtonColorIsUnified。
package core

import (
	"regexp"
	"strings"
	"testing"
)

// branchSep 匹配 actions()/rowCard() 里相邻分支的分界：
// `} else if (...)`、`} else {`，以及每个函数开头那一个 `if (t.canceled) {`。
// 直播终态的 `if (t.done) { ... } else { ... }` 会被切成两段 —— 正合适：
// 两段各自只有一处 📂，"📂 占末位还是倒数第二"才判得准。
var branchSep = regexp.MustCompile(`(?m)^\s*\}?\s*else if \(|\} else \{|^\s*if \(t\.canceled\) \{`)

// buttonCtor 匹配一处按钮构造：详细模式 btn(...)、简略模式 ibtn(...)。
// 注意 `\b` 会让 `ibtn(` 只匹配一次（不会在 `b` 处再匹配成 `btn(`）。
var buttonCtor = regexp.MustCompile(`\b(?:btn|ibtn)\(`)

// folderCtor 匹配一处「打开文件夹」按钮构造。逐个查，不只看第一处。
var folderCtor = regexp.MustCompile(`openFolder\(`)

func TestFolderButtonIsLastOrBeforeRemove(t *testing.T) {
	page := homePageHTML

	iActs := strings.Index(page, "function actions(t){")
	// 右界必须收在 btn() 定义**之前**：`function btn(kind,label,onclick){` 自身含 "btn("，
	// 若落进 region 就会被算成一个按钮构造点，"📂 后面跟的是哪个按钮"就会算错。
	// （上一版就是这么写的，报了一条假红——扫描范围本身就是断言的一部分。）
	iBtn := strings.Index(page, "function btn(kind,label,onclick){")
	iRow := strings.Index(page, "function rowCard(t){")
	iTail := strings.Index(page, "// —— 名称滚动")
	if iActs < 0 || iBtn <= iActs || iRow <= iBtn || iTail <= iRow {
		t.Fatal("找不到 actions()/btn()/rowCard()/名称滚动 —— 页面结构变了，本测试锚点要跟着改")
	}

	for _, region := range []struct{ where, src string }{
		{"详细模式 actions()", page[iActs:iBtn]},
		{"简略模式 rowCard()", page[iRow:iTail]},
	} {
		spans := branchSep.FindAllStringIndex(region.src, -1)
		if len(spans) < 6 {
			// 切不出分支 ⇒ 下面一个分支都不会被检查。宁可失败，也不能退化成
			// "什么都没验、然后全绿"（本项目对无区分力断言的硬要求）。
			t.Fatalf("%s：只切出 %d 个分支（期望 ≥6）—— 切分规则失效，分支写法变了？",
				region.where, len(spans))
		}
		checked := 0
		for k, s := range spans {
			end := len(region.src)
			if k+1 < len(spans) {
				end = spans[k+1][0]
			}
			body := region.src[s[0]:end]
			ctors := buttonCtor.FindAllStringIndex(body, -1)
			if len(ctors) == 0 {
				continue
			}
			folders := folderCtor.FindAllStringIndex(body, -1)
			if len(folders) == 0 {
				continue // 本段没有 📂（已取消态就是这样，由另一条守卫管）
			}
			checked++

			// 逐处 📂 检查：它后面要么什么都没有（占末位），要么紧跟着「🗑 移除记录」
			// 且那个 🗑 必须是本段最后一个按钮。
			for _, f := range folders {
				next := -1
				for i, c := range ctors {
					if c[0] > f[0] {
						next = i
						break
					}
				}
				if next < 0 {
					// 📂 是最后一个按钮 —— 合法，但本段不能有「🗑 移除记录」
					// （有 🗑 时 📂 必须让到它前面，这是定稿的核心判据）。
					if strings.Contains(body, "removeTask(") {
						t.Errorf("%s：本段有「🗑 移除记录」却把「📂 打开文件夹」留在了末位 —— "+
							"约定是 📂 让到 🗑 前面（倒数第二）\n  分支正文：%s",
							region.where, oneline(body))
					}
					continue
				}
				segEnd := len(body)
				if next+1 < len(ctors) {
					segEnd = ctors[next+1][0]
				}
				seg := body[ctors[next][0]:segEnd]
				if !strings.Contains(seg, "removeTask(") {
					t.Errorf("%s：「📂 打开文件夹」后面的按钮不是「🗑 移除记录」—— 约定是"+
						"「… ✕ 取消 → 📂 打开文件夹」或「… 📂 打开文件夹 → 🗑 移除记录」\n"+
						"  该按钮：%s\n  分支正文：%s",
						region.where, oneline(seg), oneline(body))
					continue
				}
				if next+1 < len(ctors) {
					t.Errorf("%s：「🗑 移除记录」后面还有按钮 —— 它必须是最后一位\n  分支正文：%s",
						region.where, oneline(body))
				}
			}
		}
		// 门槛：两种模式各有 6 段真带 📂（直播已收尾 / 直播待收尾 / 已完成 / 已暂停 / 失败 / 运行中）。
		// 少于 5 说明有分支被漏检（例如锚点漂了），同样要显式失败。
		if checked < 5 {
			t.Fatalf("%s：只有 %d 个带 📂 的分支被检查（期望 ≥5）", region.where, checked)
		}
	}
}

// oneline 把分支正文压成一行，方便失败信息直接定位。
func oneline(s string) string { return strings.Join(strings.Fields(s), " ") }
