// 按钮/图标顺序的统一约定（学徒 2026-09-16 定）。
//
// 语义：
//   - 每个任务的操作按钮，**第一个位置固定是「主操作」**（运行中 ⏸ / 直播运行中 ⏹；
//     已暂停 ▶继续；失败 ▶重试；已完成 ▶打开文件），
//   - **第二个位置固定是「📂 打开文件夹」**，
//   - 之后才是「✕ 取消」「🗑 移除记录」。
//   - 详细模式（actions()）与简略模式（rowCard()）**必须给出同一套顺序**。
//
// **失败态例外**（学徒 2026-09-19 定）：次序是 ↻重试 → ✕取消 → 📂打开文件夹，
// 且**不提供「🗑 移除记录」** —— 失败是该被修的对象，不是该被清掉的垃圾。
// 两处实现同样必须一致，由 TestFailedRowHasNoRemoveAction 钉住。
//
// 为什么要有这条回归：两处原来是各自逐状态手拼的，于是 📂 在「运行中/已暂停/失败」
// 排第 1 位、在「已完成」排第 2 位 —— 图标列宽度不一，📂 横向漂移，右对齐也救不回来。
// 这是"两处实现同一条规则"的典型漏法：只改一处，漂移会换个地方出现。
//
// 页面是 HTML 字符串，前端没有运行时测试，所以用扫描把顺序钉住
// （参照 web_live_ui_test.go）。断言只看相对次序，不管缩进与排版。
package core

import (
	"strings"
	"testing"
)

// spanBetween 取 [from, to) 之间的文本。任一锚点缺失时 ok=false。
func spanBetween(s, from, to string) (string, bool) {
	i := strings.Index(s, from)
	if i < 0 {
		return "", false
	}
	rest := s[i:]
	j := strings.Index(rest, to)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// assertMarkerOrder 断言 markers 在 reg 中**按给定顺序**出现（允许中间夹别的东西）。
func assertMarkerOrder(t *testing.T, where, reg string, markers []string) {
	t.Helper()
	prev, prevName := -1, ""
	for _, m := range markers {
		i := strings.Index(reg, m)
		if i < 0 {
			t.Errorf("%s：找不到 %q —— 分支被改写，或按钮没按约定拼进去", where, m)
			return
		}
		if i < prev {
			t.Errorf("%s：%q 排在了 %q 之前 —— 约定是「① 主操作 ② 📂 打开文件夹 ③ ✕ 取消 / 🗑 移除记录」",
				where, m, prevName)
			return
		}
		prev, prevName = i, m
	}
}

// TestActionButtonOrderIsUnified 两种模式、各个状态的操作顺序都必须服从同一套约定。
func TestActionButtonOrderIsUnified(t *testing.T) {
	page := homePageHTML

	iActs := strings.Index(page, "function actions(t){")
	iRow := strings.Index(page, "function rowCard(t){")
	if iActs < 0 || iRow < 0 || iRow <= iActs {
		t.Fatal("找不到 actions()/rowCard() —— 页面结构变了，本测试的锚点要跟着改")
	}
	actionsRegion := page[iActs:iRow]
	rowRegion := page[iRow:]

	const (
		// 详细模式（按钮带文字）
		aMainRun    = "'⏸ 暂停','pauseTask("
		aMainResume = "'⏯ 继续下载','resumeTask("
		aMainRetry  = "'↻ 重试','resumeTask("
		aMainOpen   = "'▶ 打开文件','openFile("
		aMainStop   = "'⏹ 停止（保存已录）'"
		aFolder     = "'📂 打开文件夹','openFolder("
		aCancel     = "'✕ 取消','cancelTask("
		aRemove     = "'🗑 移除记录','removeTask("
		// 简略模式（图标按钮）
		rMainRun    = "'⏸','暂停','pauseTask("
		rMainResume = "'⏯','继续下载','resumeTask("
		rMainRetry  = "'↻','重试','resumeTask("
		rMainOpen   = "'▶','打开文件','openFile("
		rMainStop   = "'⏹','停止（保存已录部分）'"
		rFolder     = "'📂','打开文件夹','openFolder("
		rCancel     = "'✕','取消（删除文件）','cancelTask("
		rRemove     = "'🗑','移除记录','removeTask("
	)

	cases := []struct {
		where    string
		src      string
		from, to string
		markers  []string
	}{
		// —— 详细模式 ——
		{"详细模式·直播终态", actionsRegion,
			"} else if (t.live && (t.done || t.paused || t.error)) {", "} else if (t.done && !t.error) {",
			[]string{aMainOpen, aFolder, aMainStop}},
		{"详细模式·已完成", actionsRegion,
			"} else if (t.done && !t.error) {", "} else if (t.paused) {",
			[]string{aMainOpen, aFolder, aRemove}},
		{"详细模式·已暂停", actionsRegion,
			"} else if (t.paused) {", "} else if (t.error) {",
			[]string{aMainResume, aFolder, aCancel}},
		// 失败态是通用约定的例外：打开文件夹挪到末位、且不给移除记录
		{"详细模式·失败", actionsRegion,
			"} else if (t.error) {", "// 下载中 / 排队中",
			[]string{aMainRetry, aCancel, aFolder}},
		{"详细模式·运行/排队中", actionsRegion,
			"// 下载中 / 排队中", "return b.length",
			[]string{aMainRun, aFolder, aCancel}},

		// —— 简略模式（同状态必须与上面同序）——
		{"简略模式·直播终态", rowRegion,
			"} else if (t.live && (t.done || t.paused || t.error)) {", "} else if (t.done && !t.error) {",
			[]string{rMainOpen, rFolder, rMainStop}},
		{"简略模式·已完成", rowRegion,
			"} else if (t.done && !t.error) {", "} else if (t.paused) {",
			[]string{rMainOpen, rFolder, rRemove}},
		{"简略模式·已暂停", rowRegion,
			"} else if (t.paused) {", "} else if (t.error) {",
			[]string{rMainResume, rFolder, rCancel}},
		// 简略模式失败态同样服从那条例外
		{"简略模式·失败", rowRegion,
			"} else if (t.error) {", "acts = (t.live",
			[]string{rMainRetry, rCancel, rFolder}},
		{"简略模式·运行中", rowRegion,
			"acts = (t.live", "const name = esc(fnameOf(t));",
			[]string{rMainRun, rFolder, rCancel}},
	}

	for _, c := range cases {
		reg, ok := spanBetween(c.src, c.from, c.to)
		if !ok {
			t.Errorf("%s：区间锚点 %q … %q 取不到 —— 分支结构变了，测试锚点要跟着改", c.where, c.from, c.to)
			continue
		}
		assertMarkerOrder(t, c.where, reg, c.markers)
	}
}

// TestCanceledRowKeepsSingleButton 已取消行只留「移除记录」，不补空位。
//
// 学徒 2026-09-16 选的方案 A：文件已删、也没有文件夹可开，单独靠右反而干净。
// 一旦有人给它补占位（空 ibtn / visibility:hidden）来"对齐"，这里会红——
// 那时要确认是**有意改方案**，而不是为了让某条断言过而顺手加的空洞。
func TestCanceledRowKeepsSingleButton(t *testing.T) {
	page := homePageHTML

	iActs := strings.Index(page, "function actions(t){")
	iRow := strings.Index(page, "function rowCard(t){")
	if iActs < 0 || iRow < 0 || iRow <= iActs {
		t.Fatal("找不到 actions()/rowCard() —— 页面结构变了，本测试的锚点要跟着改")
	}

	for _, c := range []struct{ where, src string }{
		{"详细模式·已取消", page[iActs:iRow]},
		{"简略模式·已取消", page[iRow:]},
	} {
		reg, ok := spanBetween(c.src, "if (t.canceled) {", "} else if (t.live &&")
		if !ok {
			t.Errorf("%s：取不到已取消分支", c.where)
			continue
		}
		if !strings.Contains(reg, "removeTask(") {
			t.Errorf("%s：缺少「移除记录」入口", c.where)
		}
		if strings.Contains(reg, "打开文件夹") {
			t.Errorf("%s：已取消行出现了「打开文件夹」—— 文件已删除，这里不该有它", c.where)
		}
	}
}

// TestFailedRowHasNoRemoveAction 失败态不给「移除记录」（学徒 2026-09-19 定）。
//
// 这不是审美问题：失败任务是**要被修的对象**。给了移除入口，一键就把「重试所需的
// 上下文（URL / Referer / 断点）」连同记录一起清掉，而用户当时想点的往往只是「取消」。
// 所以失败态只留 ↻重试 / ✕取消 / 📂打开文件夹。
//
// 双向守卫：塞回 removeTask 要红（清太多），缺了三个按钮之一也要红（删过头）。
func TestFailedRowHasNoRemoveAction(t *testing.T) {
	page := homePageHTML

	iActs := strings.Index(page, "function actions(t){")
	iRow := strings.Index(page, "function rowCard(t){")
	if iActs < 0 || iRow < 0 || iRow <= iActs {
		t.Fatal("找不到 actions()/rowCard() —— 页面结构变了，本测试的锚点要跟着改")
	}

	for _, c := range []struct{ where, src, to string }{
		{"详细模式·失败", page[iActs:iRow], "// 下载中 / 排队中"},
		{"简略模式·失败", page[iRow:], "acts = (t.live"},
	} {
		reg, ok := spanBetween(c.src, "} else if (t.error) {", c.to)
		if !ok {
			t.Errorf("%s：取不到失败态分支 —— 分支结构变了，本测试的锚点要跟着改", c.where)
			continue
		}
		if strings.Contains(reg, "removeTask(") {
			t.Errorf("%s：失败态出现了「移除记录」—— 学徒定的是失败不许移除记录（该修，不该清）", c.where)
		}
		for _, frag := range []string{"重试", "取消", "打开文件夹"} {
			if !strings.Contains(reg, frag) {
				t.Errorf("%s：失败态缺少 %q —— 失败态是 重试 / 取消 / 打开文件夹 三个", c.where, frag)
			}
		}
	}
}

// TestActionOrderConventionIsDocumented 约定本身要写在代码里。
//
// 这条规矩靠"两处同步"维持，最容易的失效方式就是后来的人不知道有这条规矩。
func TestActionOrderConventionIsDocumented(t *testing.T) {
	page := homePageHTML
	for _, frag := range []string{
		"① 主操作",     // 约定原文
		"② 📂 打开文件夹", // 固定位次
		"rowCard()", // 明确点出两处同源
	} {
		if !strings.Contains(page, frag) {
			t.Errorf("web/index.html 缺少顺序约定的说明片段 %q —— 后来的人会继续逐状态自由发挥", frag)
		}
	}
}

// TestControlButtonColorIsUnified 按钮配色也是统一约定（学徒 2026-09-16 定）。
//
//	媒体控制键（▶ 播放 / ⏸ 暂停 / ⏹ 停止 / ⏯ 继续 / ↻ 重试）一律 `primary`（蓝底）——
//	  它们都是"让任务动起来"，同色才成一组；
//	`📂 打开文件夹`、`🗑 移除记录` 保持中性描边 —— 它们不改任务状态，只是看一眼磁盘 / 清列表；
//	`✕ 取消` 保持 `danger` 红。
//
// 为什么要有这条回归：同一位置原先各拼各的配色 —— 运行中的 ⏸/⏹ 是 warn（黄底）、
// 而 ⏯继续 / ↻重试 是 primary（蓝底），于是"运行中"在点播与直播下长得不一样，
// 已暂停行与已完成行也不是同一族。**只改一处就会把不统一挪个地方出现。**
func TestControlButtonColorIsUnified(t *testing.T) {
	page := homePageHTML

	iActs := strings.Index(page, "function actions(t){")
	iRow := strings.Index(page, "function rowCard(t){")
	if iActs < 0 || iRow < 0 || iRow <= iActs {
		t.Fatal("找不到 actions()/rowCard() —— 页面结构变了，本测试的锚点要跟着改")
	}
	actionsRegion := page[iActs:iRow]
	rowRegion := page[iRow:]

	// —— 主操作必须是蓝底 ——
	cases := []struct {
		where    string
		src      string
		from, to string
		frags    []string
	}{
		{"详细模式·运行中", actionsRegion, "// 下载中 / 排队中", "return b.length",
			[]string{"btn('primary','⏸ 暂停'", "btn('primary','⏹ 停止'"}},
		{"详细模式·已暂停", actionsRegion, "} else if (t.paused) {", "} else if (t.error) {",
			[]string{"btn('primary','⏯ 继续下载'"}},
		{"详细模式·失败", actionsRegion, "} else if (t.error) {", "// 下载中 / 排队中",
			[]string{"btn('primary','↻ 重试'"}},
		{"详细模式·已完成", actionsRegion, "} else if (t.done && !t.error) {", "} else if (t.paused) {",
			[]string{"btn('primary','▶ 打开文件'"}},
		{"详细模式·直播终态", actionsRegion, "} else if (t.live &&", "} else if (t.done && !t.error) {",
			[]string{"btn('primary','▶ 打开文件'", "btn('primary','⏹ 停止（保存已录）'"}},

		{"简略模式·运行中", rowRegion, "acts = (t.live", "const name = esc(fnameOf(t));",
			[]string{"ibtn('primary','⏸','暂停'", "ibtn('primary','⏹','停止'"}},
		{"简略模式·已暂停", rowRegion, "} else if (t.paused) {", "} else if (t.error) {",
			[]string{"ibtn('primary','⏯','继续下载'"}},
		{"简略模式·失败", rowRegion, "} else if (t.error) {", "acts = (t.live",
			[]string{"ibtn('primary','↻','重试'"}},
		{"简略模式·已完成", rowRegion, "} else if (t.done && !t.error) {", "} else if (t.paused) {",
			[]string{"ibtn('primary','▶','打开文件'"}},
		{"简略模式·直播终态", rowRegion, "} else if (t.live &&", "} else if (t.done && !t.error) {",
			[]string{"ibtn('primary','▶','打开文件'", "ibtn('primary','⏹','停止（保存已录部分）'"}},
	}
	for _, c := range cases {
		reg, ok := spanBetween(c.src, c.from, c.to)
		if !ok {
			t.Errorf("%s：区间锚点取不到 —— 分支结构变了，测试锚点要跟着改", c.where)
			continue
		}
		for _, frag := range c.frags {
			if !strings.Contains(reg, frag) {
				t.Errorf("%s：主操作不是蓝底的 %q —— 媒体控制键必须统一 `primary`", c.where, frag)
			}
		}
	}

	// —— 非主操作：中性 / 红 ——
	for _, c := range []struct{ where, src, frag string }{
		{"详细模式·打开文件夹保持中性", actionsRegion, "btn('ghost','📂 打开文件夹'"},
		{"详细模式·移除记录保持中性", actionsRegion, "btn('ghost','🗑 移除记录'"},
		{"详细模式·取消保持红", actionsRegion, "btn('danger','✕ 取消'"},
		{"简略模式·打开文件夹保持中性", rowRegion, "ibtn('','📂','打开文件夹'"},
		{"简略模式·移除记录保持中性", rowRegion, "ibtn('','🗑','移除记录'"},
		{"简略模式·取消保持红", rowRegion, "ibtn('danger','✕','取消（删除文件）'"},
	} {
		if !strings.Contains(c.src, c.frag) {
			t.Errorf("%s：找不到 %q —— 配色约定被改动了（学徒定：这两类不动）", c.where, c.frag)
		}
	}

	// —— 反向：这几样不该再回来 ——
	for _, c := range []struct{ why, src, frag string }{
		{"黄底按钮已废弃（主操作改蓝底）", actionsRegion, "btn('warn'"},
		{"`button.warn` 样式已删，定义不得复活", page, "button.warn{"},
		{"「继续」不得再用 ▶ —— 会和「打开文件」同形", rowRegion, "ibtn('primary','▶','继续下载'"},
		{"详细模式「继续」同样不得用 ▶", actionsRegion, "'▶ 继续下载'"},
		{"详细模式「重试」不得用 ▶ —— 与「打开文件」同形，简略模式用的是 ↻", actionsRegion, "'▶ 重试'"},
		{"简略模式「打开文件」不得退回中性（学徒定：播放要蓝底）", rowRegion, "ibtn('','▶','打开文件'"},
	} {
		if strings.Contains(c.src, c.frag) {
			t.Errorf("发现 %q —— %s", c.frag, c.why)
		}
	}
}
