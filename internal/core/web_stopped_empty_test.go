// R3：用户主动停止且没录到内容（done + 无 finalPath + 非取消/中断/失败）
// 必须有独立徽章，不能显示成「已完成」—— 与状态行的「已停止（无内容）」自相矛盾。
//
// 页面是 HTML 字符串，编译期发现不了，用扫描钉不变量（参照 web_live_ui_test.go）。
package core

import (
	"strings"
	"testing"
)

func TestBadgeStoppedEmptyIsNotDone(t *testing.T) {
	page := homePageHTML

	i := strings.Index(page, "if(t.done) return t.fileMissing")
	if i < 0 {
		t.Fatal("找不到 badge 的 done 分支 —— 页面结构变了，本测试锚点要跟着改")
	}
	seg := page[i:]
	if j := strings.Index(seg, ";"); j > 0 {
		seg = seg[:j]
	}

	if !strings.Contains(seg, "t.finalPath ?") {
		t.Error("badge 的 done 分支没有按 finalPath 分流：停止且无内容会被显示成「已完成」（R3）")
	}
	if !strings.Contains(seg, "已停止") {
		t.Error("badge 缺少「已停止」徽章：停止且无内容会被显示成「已完成」（R3）")
	}
}
