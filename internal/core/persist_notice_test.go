// S1：留档类通知必须活过后续的成功落盘。
//
// 缺陷形态：persistHealth 原先只有 lastErr 一个槽位，saveState 每次成功都把它清空
// （recordPersistResult(nil)），而"配置 / 状态文件损坏已留档"这类通知也写进同一个
// 槽位 ⇒ 留档之后第一次成功落盘就把它抹掉，用户一个字都看不到；GUI 有任务时
// markDirty 是唯一落盘入口、高频触发，窗口只有几百毫秒。
//
// 修法：另开 notice 槽位 —— 成功落盘只清 lastErr（表达"最近一次落盘健不健康"），
// 不碰 notice（表达"发生过一件用户该知道的事"）。
//
// 判据方向：把 recordPersistNotice 改回写 lastErr（旧写法）⇒ 本文件第一条必红。
package core

import (
	"errors"
	"strings"
	"testing"
)

// resetPersistHealth 清空共享 Runtime 上的持久化健康槽位。
//
// **前置也清、后置也清**：只做后置清理的话，本用例断言 Notice=="" 时会被
// 前面用例留下的通知污染成假红/假绿。清不掉 notice 的写法（只调
// recordPersistResult(nil)）本身就是 S1 的成因，所以这里直接摸槽位。
func resetPersistHealth(t *testing.T) {
	t.Helper()
	clear := func() {
		testStd.persistHealth.mu.Lock()
		testStd.persistHealth.lastErr = ""
		testStd.persistHealth.notice = ""
		testStd.persistHealth.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// TestPersistNoticeSurvivesSuccessfulSave 是本轮 S1 修复的判别断言：
// 记一条通知 → 跑一次**真正成功**的 saveState → 通知必须还在。
func TestPersistNoticeSurvivesSuccessfulSave(t *testing.T) {
	saveRestoreState(t)
	resetPersistHealth(t)

	testStd.recordPersistNotice("配置文件解析失败，已留档为 x.bak（本次用默认配置）")
	if d := testStd.persistDTOOf(); d.Notice == "" {
		t.Fatalf("通知记下后应当立刻可见: %+v", d)
	}

	testStd.saveState() // 同步写盘，成功末尾会走 recordPersistResult(nil)

	d := testStd.persistDTOOf()
	if d.Notice == "" {
		t.Fatalf("通知被一次成功的落盘清掉了（S1 未修）: %+v", d)
	}
	if !strings.Contains(d.Notice, "已留档") {
		t.Fatalf("通知内容应原样保留: %+v", d)
	}
	if !d.OK {
		t.Fatalf("成功落盘后 ok 应为 true（notice 与落盘健康度互不干扰）: %+v", d)
	}
}

// 成对的另一半：lastErr 的既有语义不能被改坏 —— 成功落盘**应当**清掉落盘错误，
// 否则上面那条可以靠"干脆什么都不清"骗过。
func TestPersistErrorIsClearedBySuccessfulSave(t *testing.T) {
	saveRestoreState(t)
	resetPersistHealth(t)

	testStd.recordPersistResult(errors.New("写入失败: 磁盘已满"))
	if d := testStd.persistDTOOf(); d.OK || d.Error == "" {
		t.Fatalf("落盘错误应当可见: %+v", d)
	}

	testStd.saveState() // 成功落盘

	if d := testStd.persistDTOOf(); !d.OK || d.Error != "" {
		t.Fatalf("成功落盘应当清掉落盘错误（lastErr 的既有语义）: %+v", d)
	}
}

// TestPersistNoticeAccumulatesDistinctAndDeduplicates：一次启动里"配置损坏"（loadConfig
// 期）与"状态文件损坏"（loadState 期，紧随其后）可以同时发生 —— 单槽位覆盖会让后写的
// 把先写的挤掉，而被挤掉的可能是后果更重的一条。故不同内容累加、相同内容不重复。
func TestPersistNoticeAccumulatesDistinctAndDeduplicates(t *testing.T) {
	resetPersistHealth(t)

	testStd.recordPersistNotice("通知甲")
	testStd.recordPersistNotice("通知乙")
	got := testStd.persistDTOOf().Notice
	if !strings.Contains(got, "通知甲") || !strings.Contains(got, "通知乙") {
		t.Fatalf("两条不同通知都应保留: %q", got)
	}

	testStd.recordPersistNotice("通知甲") // 同一进程内重复
	got = testStd.persistDTOOf().Notice
	if strings.Count(got, "通知甲") != 1 {
		t.Fatalf("重复通知不应重复追加: %q", got)
	}
}

// TestPersistNoticeEmptyTextIsIgnored：空串不入槽（否则会在前端渲染出一条空横幅）。
func TestPersistNoticeEmptyTextIsIgnored(t *testing.T) {
	resetPersistHealth(t)

	testStd.recordPersistNotice("")
	if got := testStd.persistDTOOf().Notice; got != "" {
		t.Fatalf("空通知不该入槽: %q", got)
	}
}
