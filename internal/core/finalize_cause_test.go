// 收尾起因（finalizeCause）把"起因"与"说明"绑成一个值，取代原先
// 「finalizeOutcome + 一参三义的 string reason」的写法。
//
// 被取代的形态为什么是缺陷：那句用户看到的话**含义完全由起因决定** —— 中断时它是
// 失败原因（进 errorMsg）、完成时它是直播的结束方式（进 stage 括号）、用户主动停止时
// 它根本没有位置。三义共用形参时，读一个调用点判断不出那串东西会落到哪；同一句话
// 还可能同时写在形参与收尾内部两处（`"用户停止录制"` 就是）。更早的版本里，同一个概念
// 在四处叫三个名字（alignResult.note / 局部 note / 局部 msg / 形参 reason），
// grep 任何一个名字都搜不全另外三个。
//
// 本文件守两件事：
//
//	① 文案映射（行为）—— liveEndKind / finalizeCause 各分支返回什么。三档判据强度
//	   不同、说法必须不同，改坏必红。
//	② 生成点唯一 + 接线不变（结构）—— 结束方式的字面量只能住在 liveEndKind.note()
//	   （download.go），调用点必须构造 finalizeCause 而不是摆一串并列实参。字面量抄回
//	   调用点的后果是"加第四种结束方式时漏改一处"，而漏改是静默的（退化成笼统的
//	   「已保存」，日志与状态一切正常）—— 正是本项目头号缺陷形态。
//
// ⚠️ 结构断言读的是**剥掉整行注释**后的源码：函数注释为说明历史会原样引用旧写法
// （本文件上头那段解释就写了那三档文案），不剥会假红。
// ⚠️ 它守的是**接线**（字面量住在哪个文件、签名长什么样），守不到"值被改写"
// （把「播放列表已结束」改成「列表结束了」这类字面量漂移）—— 值由 ① 的行为断言钉。
// 两者缺一不可。
package core

import (
	"os"
	"strings"
	"testing"
)

// nonCommentSource 读生产源码并丢掉**整行注释**，返回值用于结构断言。
//
// 只丢整行注释（TrimSpace 后以 // 开头）：行尾注释留着无妨 —— 负向断言要找的是
// 代码里真的出现了什么，而不是注释里提过什么。
func nonCommentSource(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", file, err)
	}
	var kept []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		kept = append(kept, l)
	}
	src := strings.Join(kept, "\n")
	if len(src) == 0 {
		t.Fatalf("%s 剥完注释是空的 —— 切分或读取失效，下面的断言会恒真", file)
	}
	return src
}

// ① 结束方式 → 文案。三档判据强度不同，说法必须不同：
// ENDLIST 是源站白纸黑字的确证；连续空轮询只是"列表不再增长"的推断，末尾可能被截断。
func TestLiveEndKindNoteMapping(t *testing.T) {
	cases := []struct {
		kind liveEndKind
		want string
		why  string
	}{
		{liveEndEndList, "播放列表已结束", "源站声明结束 —— 确证"},
		{liveEndInferred, "播放列表停止更新 · 末尾可能不全", "推断结束，末尾可能被截断"},
		{liveEndNone, "", "没走到正常出口，不该给出「录完」的说法"},
		{liveEndUserStop, "", "用户停止：说法由收尾内部的「已保存（用户停止录制）」给"},
	}
	for _, c := range cases {
		if got := c.kind.note(); got != c.want {
			t.Errorf("liveEndKind(%d).note() = %q，want %q（%s）", c.kind, got, c.want, c.why)
		}
	}
}

// ① 起因 → 说明。三个分支的"说明"归属互不重叠，且互不串味。
func TestFinalizeCauseNoteMapping(t *testing.T) {
	// 中断：说明 = 失败原因（写进 errorMsg，必须保留，否则就是"产物不完整而记录正常"）
	if got := (finalizeCause{outcome: finalizeInterrupted, failReason: "拉流失败"}).note(); got != "拉流失败" {
		t.Errorf("中断收尾的说明应为失败原因，得到 %q", got)
	}
	// 完成：说明 = 结束方式
	if got := (finalizeCause{outcome: finalizeComplete, endKind: liveEndInferred}).note(); got != "播放列表停止更新 · 末尾可能不全" {
		t.Errorf("完成收尾的说明应为结束方式，得到 %q", got)
	}
	// 点播完成：endKind 是零值 liveEndNone ⇒ 无说明，stage 保持干脆的「已保存」
	if got := (finalizeCause{outcome: finalizeComplete}).note(); got != "" {
		t.Errorf("点播完成不该有结束说明，得到 %q", got)
	}
	// 用户停止：**即使字段被填上也不产出说明** —— 这条是"三义不串味"的判据：
	// 若 note() 退化成"只要哪个字段非空就返回它"，前两种起因的语义就重新混在一起了。
	if got := (finalizeCause{outcome: finalizeStopped, failReason: "不该被用", endKind: liveEndEndList}).note(); got != "" {
		t.Errorf("用户停止不该有外部说明，得到 %q（起因与说明的绑定松了）", got)
	}
}

// ② 结束方式的字面量只能住在 liveEndKind.note() 里。
func TestEndKindLiteralLivesOnlyInItsEnum(t *testing.T) {
	pipelineSrc := nonCommentSource(t, "pipeline.go")
	downloadSrc := nonCommentSource(t, "download.go")

	// 防退化：源文件被换成别的东西时，下面两条会一起"通过"
	if !strings.Contains(pipelineSrc, "func runDiskPipeline") {
		t.Fatal("pipeline.go 里找不到 runDiskPipeline —— 读到的不是预期文件，断言会恒真")
	}
	if !strings.Contains(downloadSrc, "func (k liveEndKind) note()") {
		t.Fatal("download.go 里找不到 liveEndKind.note —— 文案的唯一定义处没了")
	}

	for _, lit := range []string{"播放列表已结束", "播放列表停止更新"} {
		if strings.Contains(pipelineSrc, lit) {
			t.Errorf("pipeline.go 的代码里出现了结束方式字面量 %q —— 文案只能住在 "+
				"liveEndKind.note()（download.go）；抄回调用点的后果是加第四种结束方式时"+
				"漏改一处，而漏改是静默的", lit)
		}
		if !strings.Contains(downloadSrc, lit) {
			t.Errorf("download.go 的 liveEndKind.note() 里找不到 %q —— 文案映射被改坏或搬走了", lit)
		}
	}
}

// ② 接线：收尾函数收的是"起因"这一个值，不许退回"枚举 + 独立字符串参数"。
func TestFinalizeTakesCauseNotBareStrings(t *testing.T) {
	sigs := []struct{ file, sig string }{
		{"pipeline.go", "func finalizeRecording(te *taskEntry, job *dlJob, partPath string, cause finalizeCause) error"},
		{"pipeline.go", "func finishStop(te *taskEntry, id, part string, segDone int64, cause finalizeCause)"},
	}
	for _, s := range sigs {
		if !strings.Contains(nonCommentSource(t, s.file), s.sig) {
			t.Errorf("%s 的收尾函数签名不是预期的形状：\n  想要 %s\n"+
				"签名里再出现一个独立的 string 参数，就是「同一个 string 一参三义」的复活点",
				s.file, s.sig)
		}
	}

	// 旧写法的指纹：枚举被**当独立实参**传（前有逗号、后紧跟逗号）。
	// 与结构体字面量里的 `outcome: finalizeInterrupted,`（前有冒号）区分得开。
	for _, file := range []string{"pipeline.go", "engine.go"} {
		src := nonCommentSource(t, file)
		for _, pat := range []string{", finalizeInterrupted,", ", finalizeStopped,", ", finalizeComplete,"} {
			if strings.Contains(src, pat) {
				t.Errorf("%s 里出现 %q —— 又把收尾起因当独立参数传了（应构造 finalizeCause{...}）", file, pat)
			}
		}
	}
}
