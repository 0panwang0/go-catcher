// 恢复提示（restartNote）必须在界面上真的渲染出来。
//
// 缺陷形态（2026-09-20 R2/R3 落地时抓到的**方案外缺口**）：后端在
// "账本不可用 / 位图丢失 ⇒ 自动从 0 重下"时会写 restartNote
// （pipeline.go 的 alignPartToLedger，以及 download.go 的 downloadDirect），
// 方案 §7 要求这条路径"必须可见"；但 web/index.html 当时 **0 处渲染它** ——
// 用户只会看到进度条从零重新爬，以为程序在重下已经下过的东西。
// 而 GUI 是 windowsgui 子系统、没有控制台，写日志等于没写。
//
// 这是本项目反复出现的头号缺陷形态的又一例：**行为是对的，界面把事实藏起来了**。
//
// 双向守卫（都有区分力，扰动验证做过）：
//   - 删掉渲染函数或那一行插桩 → 红；
//   - 服务端字段名改了而前端没跟（token 漂移）→ 红；
//   - 只留空壳元素、不渲染文字 → 红。
package core

import (
	"strings"
	"testing"
)

func TestRestartNoteIsRendered(t *testing.T) {
	page := homePageHTML

	iCard := strings.Index(page, "function card(t){")
	iRow := strings.Index(page, "function rowCard(t){")
	if iCard < 0 || iRow < 0 || iRow <= iCard {
		t.Fatal("找不到 card()/rowCard() —— 页面结构变了，本测试的锚点要跟着改")
	}

	mustHave := []struct{ frag, why string }{
		// 服务端字段名（task.go 的 DTO 里是 RestartNote / json:"restartNote"）。
		// 命名漂移要先在这里红，而不是等到"用户看不见提示"。
		{"t.restartNote", "前端没有读服务端的 restartNote 字段（命名漂移？）"},
		{"function noteRow(t)", "缺少恢复提示的渲染函数"},
		// 详细模式：插进 card() 主体、与 stageRow 同层（顺序：状态行 → 恢复提示 → 进度行）
		{"+ stageRow(t)\n    + noteRow(t)", "card() 没有把恢复提示插进卡片（写了函数却没调用）"},
		// 简略模式：34px 高放不下句子，用 ⚠ + title（与详细模式同源同文案）
		{"rowwarn", "简略模式缺少恢复提示的标记样式/元素"},
	}
	for _, c := range mustHave {
		if !strings.Contains(page, c.frag) {
			t.Errorf("web/index.html 缺少 %q —— %s", c.frag, c.why)
		}
	}

	// 反向：提示必须带可读文字，不能只剩一个空壳元素（空壳 = 还是没提示）
	if !strings.Contains(page, `'<div class="rnote">⚠ '+esc(t.restartNote)+'</div>'`) {
		t.Error("noteRow 没有把提示文字渲染出来（空壳元素等于没提示）")
	}

	// 反向：简略模式的 ⚠ 必须把全文放进 title，否则那一个标记点不开也读不到。
	//
	// ⚠️ 2026-09-20 起这里挂的不再只是 t.restartNote，而是 rowNote(t) —— 方案 A 让
	// 中断/失败原因也走同一个 ⚠（简略模式此前只有徽章，原因一个字都没有）。所以本行改钉
	// `esc(note)`；"⚠ 不许退回只认 restartNote 的旧写法"由
	// web_row_cause_test.go 的 TestRowNoteCoversCauseNotJustRestartNote 负责。
	if !strings.Contains(page, `'<span class="rowwarn" title="'+esc(note)+'">⚠</span>'`) {
		t.Error("简略模式的恢复提示没有把全文放进 title —— 只剩一个读不出内容的标记")
	}
}
