// 暂停文案必须按"这个任务在数什么"分流（A-3）。
//
// 缺陷形态：任务有两个互斥的进度分母 —— 分片任务数分片（segDone/segTot），
// 直链任务的计数器是字节（bytesDone/bytesTot），直链的 segTot 恒为 0。
// stageText 的暂停分支照"分片"写，于是直链任务显示
// 「已暂停 · 已下载 0 个分片，可从断点继续」——**同一张卡片左侧的百分比是真的**，
// 两行自相矛盾。用户会以为什么都没下到，去点「取消」，把已下好的 .part 删掉。
//
// 这是头号缺陷形态在**展示层**的又一例：任务状态本身是对的，界面用错了字段，
// 把"已经下了几个 GB"说成"0 个分片"。
//
// 判据取"segDone 的插值必须落在 segTot>0 这一支里"，而不是"文件里出现过 segTot>0"
// —— 后者恒真（card 的指标行本来就写了 segTot>0，本缺陷恰好在同文件另一处）。
package core

import (
	"strings"
	"testing"
)

func TestPausedWordingMatchesCounter(t *testing.T) {
	page := homePageHTML

	// ⚠ 锚点必须限定在 stageText 内：`if(t.paused) return t.live` 在 badge() 里
	// 还有一处（那里只决定徽章文字、不碰片数），用全局 Index 会取到错的那一处。
	iStage := strings.Index(page, "function stageText(t){")
	if iStage < 0 {
		t.Fatal("找不到 stageText —— 页面结构变了，本测试的锚点要跟着改")
	}
	sub := page[iStage:]

	start := strings.Index(sub, "if(t.paused) return t.live")
	if start < 0 {
		t.Fatal("找不到 stageText 的暂停分支锚点 —— 页面结构变了，本测试的锚点要跟着改")
	}
	end := strings.Index(sub[start:], "if(t.queued)")
	if end < 0 {
		t.Fatal("找不到暂停分支的结束锚点（if(t.queued)）—— 锚点要跟着页面结构改")
	}
	seg := sub[start : start+end]

	// 必须存在分流判据
	if !strings.Contains(seg, "t.segTot > 0") {
		t.Error("暂停文案没有按 segTot 分流 —— 直链任务会显示「已下载 0 个分片」")
	}

	// 核心判据：segDone 只能出现在 segTot>0 的成立支里
	iTot := strings.Index(seg, "t.segTot > 0")
	iDone := strings.Index(seg, "t.segDone")
	if iDone >= 0 && iDone < iTot {
		t.Error("segDone 出现在 segTot 判定之前 —— 直链任务（segTot==0）会显示「0 个分片」")
	}

	// 反向：分片任务的文案不能丢（改成"一律不报片数"会让分片任务少信息）
	if !strings.Contains(seg, "t.segDone") {
		t.Error("分片任务的暂停文案丢掉了已下载片数（过度修复）")
	}

	// 反向：直链那一支必须有独立文案，不能与分片支共用同一句
	if !strings.Contains(seg, "可从断点继续") {
		t.Error("暂停文案缺少「可从断点继续」的兜底分支")
	}
}
