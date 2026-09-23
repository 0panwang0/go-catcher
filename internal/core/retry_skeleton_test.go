// 播放列表读路径的重试骨架（net.go 的 retryGet）及其接线守卫。
//
// 背景：httpGetWithRetry 与 httpGetPlaylist 原先各写一份逐行相同的重试循环
// （attempt 迭代 + sleepCtx 退避 + lastErr 记账），第七轮给两侧各加体积上限后，
// 重复从"骨架"升级成"骨架 + 上限判定"，同一个判据要在两处保持一致 ——
// "改一处漏一处"的风险翻倍。骨架收成一份后，本文件守两件事：
//   - retryGet 自身的行为（走向分类 / 退避可中断 / 零次尝试必须报错）；
//   - 两条读路径**确实**还在用这份骨架（结构守卫，防止循环被抄回第二份）。
//
// ⚠️ 最要紧的一条是 TestRetryGetZeroFateStaysConservative：getFate 的零值必须
// 落在保守侧。本轮重构的第一版把 getDone 放在零值位，于是所有漏写 fate 的失败
// 分支都返回"成功 + HTTP 200" —— 一个字段的默认值把 500 变成 200，恰是本项目
// 头号缺陷形态（产物坏了而日志正常）。当时是两条既有测试（TestHTTPGetWithRetryStatusFailure /
// TestRetryLimitZeroFailsInsteadOfReportingSuccess）先翻红才发现的。
package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRetryGetZeroFateStaysConservative 单次尝试**漏写 fate** 时，必须读作
// "可重试失败"，绝不能读作"成功"。
//
// 用 noBackoff 触发重试，免得为这条断言白等一轮退避。
func TestRetryGetZeroFateStaysConservative(t *testing.T) {
	var attempts int
	_, err := retryGet(context.Background(), 2, func(int) ([]byte, getOutcome) {
		attempts++
		// 刻意不写 fate：模拟单次尝试里某一支失败分支漏填
		return nil, getOutcome{noBackoff: true, err: fmt.Errorf("boom")}
	})
	if err == nil {
		t.Fatal("零值 fate 被当成了成功并返回：getFate 的零值必须落在保守侧（getRetry），" +
			"否则单次尝试里漏写的一支会把失败静默成成功")
	}
	if attempts != 2 {
		t.Fatalf("尝试次数=%d want 2：零值 fate 应读作「可重试」", attempts)
	}
}

// TestRetryGetRetriesUntilSuccess 失败之后必须真的再试，且把成功那次的产物交出来。
// 挡的是"循环写成只跑一轮"这类改坏 —— 那时 attempts 会是 1、结果为空。
func TestRetryGetRetriesUntilSuccess(t *testing.T) {
	var attempts int
	got, err := retryGet(context.Background(), 3, func(n int) ([]byte, getOutcome) {
		attempts++
		if n == 1 {
			return nil, getOutcome{noBackoff: true, err: fmt.Errorf("第一次失败")}
		}
		return []byte("ok"), getOutcome{fate: getDone}
	})
	if err != nil {
		t.Fatalf("第二次应成功，得到错误: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("产物=%q want \"ok\"", got)
	}
	if attempts != 2 {
		t.Fatalf("尝试次数=%d want 2", attempts)
	}
}

// TestRetryGetFatalStopsImmediately getFatal 必须立刻收手。
//
// 挡的是反向改坏：把致命失败也拿去退避重试。超限 / 内容不是播放列表这两类
// 失败重试只会再拿回同一份东西，空耗 maxRetries 轮退避后依然失败。
func TestRetryGetFatalStopsImmediately(t *testing.T) {
	var attempts int
	_, err := retryGet(context.Background(), 5, func(int) ([]byte, getOutcome) {
		attempts++
		return nil, getOutcome{fate: getFatal, err: fmt.Errorf("致命")}
	})
	if err == nil {
		t.Fatal("getFatal 应把错误原样返回")
	}
	if attempts != 1 {
		t.Fatalf("尝试次数=%d want 1：致命失败不该再试（重试只会拿回同一份内容）", attempts)
	}
}

// TestRetryGetStopsOnCancelDuringBackoff 退避途中被取消必须**立刻**返回。
//
// 挡的是把 sleepCtx 换回裸 time.Sleep：那会让暂停/取消最多等一整轮退避
// （第一轮就是 2 秒）才生效。断言耗时而不是只看返回值 —— 裸 Sleep 也会返回
// ctx 错误，只有时间能区分。
func TestRetryGetStopsOnCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	_, err := retryGet(ctx, 3, func(n int) ([]byte, getOutcome) {
		if n == 1 {
			// 第一次失败后骨架开始退避，此时取消
			go func() {
				time.Sleep(30 * time.Millisecond)
				cancel()
			}()
			return nil, getOutcome{err: fmt.Errorf("失败")}
		}
		return []byte("不该走到这里"), getOutcome{fate: getDone}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("退避途中取消应立刻返回，实际等了 %s（第一轮退避是 2 秒，说明用了不可中断的等待）", elapsed)
	}
}

// TestRetryGetRejectsZeroAttempts 零次尝试必须显式报错。
//
// 一次都不试就返回 (零值, nil) 是"成功 + 空内容"：调用方会拿着空体继续往下走，
// 产物坏了而日志全绿 —— 正是 P0-3 那条缺陷的形态。下限由 maxRetriesNow() 在
// 读取入口夹取，这里是"万一漏了"的第二道闸，所以必须真的报错而不是静默返回。
func TestRetryGetRejectsZeroAttempts(t *testing.T) {
	called := false
	for _, n := range []int{0, -1} {
		_, err := retryGet(context.Background(), n, func(int) ([]byte, getOutcome) {
			called = true
			return nil, getOutcome{fate: getFatal}
		})
		if err == nil {
			t.Errorf("maxAttempts=%d 时返回了成功：0 次尝试是「成功 + 空内容」，必须显式失败", n)
		}
	}
	if called {
		t.Error("尝试次数 < 1 时不该真的发起请求")
	}
}

// TestPlaylistReadPathsShareOneRetrySkeleton 结构守卫：两条播放列表读路径必须
// 共用同一份重试骨架，不许把手写循环抄回第二份。
//
// 行为用例挡不住"抄回第二份"—— 两份实现都能让上面所有断言全绿，而重复本身
// 正是下一次"改一处漏一处"的温床。所以直接钉住 net.go 的形态：
//   - 取了重试次数的地方，就必须把次数交给 retryGet（不许自己写 for 循环）；
//   - 旧骨架的标志写法 `for attempt := 1; attempt <=` 不许复活。
//
// 读源码前**剥掉注释**：注释里为说明来龙去脉会引用旧写法，不剥会因为
// "文档写清楚了历史"而假红（与 limits_test.go 跳过 _test.go 同源）。
func TestPlaylistReadPathsShareOneRetrySkeleton(t *testing.T) {
	src, err := os.ReadFile("net.go")
	if err != nil {
		t.Fatalf("读 net.go 失败: %v", err)
	}
	code := stripLineComments(string(src))

	if !strings.Contains(code, "func retryGet[") {
		t.Fatal("net.go 里找不到共用的重试骨架 retryGet（下面的判据会全部退化成恒真）")
	}
	if strings.Contains(code, "for attempt := 1; attempt <=") {
		t.Error("net.go 里又出现了手写的重试循环（旧骨架的写法）：" +
			"两条读路径必须共用 retryGet，抄回第二份就是下一次「改一处漏一处」")
	}
	uses := strings.Count(code, "r.maxRetriesNow()")
	viaSkeleton := strings.Count(code, "retryGet(ctx, r.maxRetriesNow()")
	if uses < 2 {
		t.Fatalf("net.go 里只找到 %d 处重试次数读取，期望两条读路径各一处（枚举失效 ⇒ 断言恒真）", uses)
	}
	if viaSkeleton != uses {
		t.Errorf("net.go 里 %d 处取了重试次数，只有 %d 处交给了 retryGet："+
			"剩下的是手写循环，重试语义从此有两份（退避/记账/中断各改一遍）", uses, viaSkeleton)
	}
}

// stripLineComments 逐行去掉 `//` 之后的注释正文（不处理块注释：net.go 没有）。
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if j := strings.Index(l, "//"); j >= 0 {
			lines[i] = l[:j]
		}
	}
	return strings.Join(lines, "\n")
}

// TestPlaylistReadRetriesAfterServerError 端到端：httpGetPlaylist 遇到 500 必须
// 重试，第二次拿到合法播放列表就成功返回 —— 证明 retryGet 与这条路径真的接上了
// （骨架自身的用例全在闭包里，接线断了它们照样全绿）。
func TestPlaylistReadRetriesAfterServerError(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte("#EXTM3U\n#EXTINF:1,\nseg0.ts\n"))
	}))
	defer srv.Close()
	saveRestoreHTTP(t, srv.Client(), 3)

	body, isDirect, status, err := testStd.httpGetPlaylist(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("第二次应成功，得到错误: %v", err)
	}
	if isDirect || status != http.StatusOK {
		t.Fatalf("isDirect=%v status=%d want false/200", isDirect, status)
	}
	if !strings.Contains(string(body), "#EXTM3U") {
		t.Fatalf("body=%q want 完整播放列表", body)
	}
	if hits != 2 {
		t.Fatalf("请求次数=%d want 2：第一次 500 必须触发重试", hits)
	}
}
