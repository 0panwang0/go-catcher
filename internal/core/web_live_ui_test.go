// 直播 UI 的定点回归（B0-2）。
//
// 语义（学徒 2026-09-15 定）：直播任务在界面上只给「停止」与「取消」，
// 不给暂停/继续/重试；中断收尾的任务显示成「已中断」而不是「失败」
// （文件其实已经保存了，显示成失败会让用户以为白录了）。
//
// 这类回归编译期发现不了（页面是 HTML 字符串），前端又没有运行时测试，
// 所以用扫描把不变量钉住——参照 embedpage_test.go 的做法。
// 断言只取不含缩进的短片段：这里钉的是"有没有这个行为"，不是排版。
package core

import (
	"strings"
	"testing"
)

func TestLiveUIOffersStopNotResume(t *testing.T) {
	page := homePageHTML

	mustHave := []struct{ frag, why string }{
		{"function stopTask(id)", "缺少直播停止入口：服务端已提供 /stop 但页面调不到"},
		{"/stop?id=", "stopTask 没有打到 /stop 端点"},
		// 运行中：直播给「停止」，点播给「暂停」（卡片式与紧凑式各一处）
		{"'⏹ 停止','stopTask('+id+')'", "卡片式运行态没把直播的「暂停」换成「停止」"},
		{"'⏹','停止','stopTask('+id+')'", "紧凑式运行态没把直播的「暂停」换成「停止」"},
		// 终态：中断/失败/旧暂停态的直播只给「停止（保存已录）」+「取消」
		{"t.live && (t.done || t.paused || t.error)", "缺少直播终态分支：这些任务会掉进点播按钮，出现「重试」"},
		{"停止（保存已录）", "卡片式终态没有「保存已录」的停止入口"},
		{"停止（保存已录部分）", "紧凑式终态没有「保存已录」的停止入口"},
		{"已中断", "中断收尾的任务没有独立徽章，会被显示成「失败」"},
		{"录制中断", "中断态文案缺失：产物不完整必须如实说出来"},
	}
	for _, c := range mustHave {
		if !strings.Contains(page, c.frag) {
			t.Errorf("web/index.html 缺少 %q —— %s", c.frag, c.why)
		}
	}

	// 反向断言：这句承诺与实现矛盾（暂停期间的流已从列表滚走），必须彻底删掉
	if strings.Contains(page, "可继续录制") {
		t.Error("web/index.html 仍含「可继续录制」：直播暂停后无法续录，这句承诺是错的")
	}
}
