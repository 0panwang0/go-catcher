// 全局常量：嗅探列表容量、记录保留时长、轮询失败上限（background 与两边
// content script 共用；跨侧消费必须经 content-shared.js，见该文件头的说明）。
export const MAX_SNIFFED = 30;

// 嗅探记录最长保留时长：超过即视为过期，下次写入时顺手清掉（storage.local 里的
// 页面 URL / 标题 / Referer 属于隐私数据，不该无限期留存）。
export const MAX_SNIFF_AGE_MS = 7 * 24 * 60 * 60 * 1000;

// POLL_MAX_MISSES 轮询查询任务状态的失败上限（浮层 trackDownload 与下载器页
// pollTask 共用）：连续这么多次查不到任务（本地服务重启过、任务被清理）就落到
// 失败态，而不是每 700ms 永久轮询 —— 否则面板会永停在「正在连接…」，用户看不出
// 已经失败。取值由轮询周期决定：15 × 700ms ≈ 10.5s，足够跨过"服务刚起、任务还
// 没登记"的正常窗口，又不用用户干等。
//
// ⚠ 这里的"单一来源"是 P2-2 的修订结果：两处原先各写一份 15，靠测试跨文件对值
// 防漂移（那种写法下，改一处忘另一处要等测试才发现）。现在 content.js 经
// content-shared.js 取、downloader.js 直接 import，漂移在结构上不可能发生。
export const POLL_MAX_MISSES = 15;
