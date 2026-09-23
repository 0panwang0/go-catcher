// content.js 两条「晚到的异步结果」缺陷的回归测试（第七轮 D3）。
// 用法（在 edge_extension 目录下）：node tests/polllimit.test.js
//
// 【P2-2 轮询无失败上限】trackDownload 每 700ms 查一次任务状态。原实现是
//   `if (!resp || !resp.exists) return;` —— 没有失败上限 ⇒ 本地服务重启 / 任务
//   被清理后，面板**永远**停在「正在连接…」，用户看不出已经失败（本项目头号
//   缺陷形态：静默失败）。downloader.js 的 pollTask 早有 POLL_MAX_MISSES = 15，
//   浮层侧漏了 ⇒ 修法是把已有那套搬过去，不是另造一套。
//   ⚠ 两处是同一套语义 ⇒ 阈值只能有一个来源。D3 当时的写法是两处各写一份 15、
//   由本文件跨文件对值防漂移；**质量审查（2026-09-23）改成单一来源**：常量落在
//   src/background/constants.js，content.js 经 content-shared.js 取、
//   downloader.js 直接 import。现在本文件守的是"没有第二份字面量复活 + 两条
//   取用路径都真的接上了"。
//
// 【P3-3 在途检查覆盖「隐藏」】hideButton 原实现不碰 gateToken ⇒ 悬停期间发起的
//   getVideoSource 检查晚一步返回时 token 仍然匹配，会把**已经隐藏**的按钮重新
//   建出来（旧状态覆盖新状态）。修法是 hideButton 自增 gateToken，使在途结果作废。
//   本文件**真跑** hideButton 断言 token 前移，而不是扫字符串。
//
// 加载方式与 tests/livecontrols.test.js 相同：content.js 是经典脚本 IIFE，
// 用 new Function 喂进最小 DOM/chrome 桩；readyState 置 "loading"，init() 不执行。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");
const src = fs.readFileSync(path.join(DIR, "content.js"), "utf8");
const downloaderSrc = fs.readFileSync(path.join(DIR, "downloader.js"), "utf8");
const constSrc = fs.readFileSync(path.join(DIR, "src", "background", "constants.js"), "utf8");
const sharedSrc = fs.readFileSync(path.join(DIR, "content-shared.js"), "utf8");

// 阈值的唯一来源。**从源码解析真值**再喂进下面的桩，而不是写死 15：
// 写死的话"content.js 有没有真的从共享对象取值"就测不出来了（夹具恒真）。
const constMatch = constSrc.match(/export const POLL_MAX_MISSES\s*=\s*(\d+)/);

const window = {
  __m3u8_catcher_injected__: false,
  addEventListener() {},
};
const document = {
  readyState: "loading", // 让 init() 不跑：本文件只测纯函数与导出钩子
  addEventListener() {},
  querySelector: () => null,
  querySelectorAll: () => [],
  createElement: () => ({ style: {}, appendChild() {}, addEventListener() {}, setAttribute() {} }),
  body: { appendChild() {}, removeChild() {} },
  documentElement: { appendChild() {} },
};
const mod = { exports: {} };

// 桩模拟真机装配：content-shared.js 在 content.js 之前注入，把 constants.js 的
// 导出挂到 globalThis。POLL_MAX_MISSES 喂**从源码解析来的真值**（见上）——
// 桩里写死一个数字，就等于替生产代码把答案填好了。
globalThis.__m3u8Shared = {
  sanitizeFileName: (s) => s,
  qualityFromURL: () => "",
  POLL_MAX_MISSES: constMatch ? Number(constMatch[1]) : NaN,
};

new Function(
  "window",
  "document",
  "chrome",
  "location",
  "history",
  "navigator",
  "module",
  src
)(window, document, {}, { href: "https://example.com/" }, {}, { userAgent: "" }, mod);

const ui = mod.exports.__ui;
if (!ui || typeof ui.pollGiveUp !== "function") {
  console.error("content.js 未导出 pollGiveUp（测试钩子被删了？）");
  process.exit(1);
}
if (typeof ui.hideButton !== "function" || typeof ui.gateTokenNow !== "function") {
  console.error("content.js 未导出 hideButton / gateTokenNow（测试钩子被删了？）");
  process.exit(1);
}

let pass = 0;
let fail = 0;
function check(name, cond, extra) {
  if (cond) {
    pass++;
    console.log("  ok  -", name);
  } else {
    fail++;
    console.log("  FAIL-", name, extra === undefined ? "" : JSON.stringify(extra));
  }
}

// ---------- A. 单一来源：阈值只许有一份定义 ----------
console.log("A. 阈值只有一份来源（constants.js），两处消费方都不许自留字面量");
const constMax = constMatch ? Number(constMatch[1]) : NaN;
check("constants.js 里能取到 POLL_MAX_MISSES（夹具自检：取不到就是解析失效）",
  Number.isFinite(constMax), constMax);
// 下面两条是"有没有退回到两处各写一份"的机器守卫。D3 当时的写法靠"两处恰好还
// 相等"防漂移 —— 那种判据在有人只改一处、且恰好没触发对值断言时是漏的。
check("content.js 不再本地定义 POLL_MAX_MISSES 字面量",
  !/POLL_MAX_MISSES\s*=\s*\d/.test(src));
check("downloader.js 不再本地定义 POLL_MAX_MISSES 字面量",
  !/POLL_MAX_MISSES\s*=\s*\d/.test(downloaderSrc));
check("downloader.js 从 constants.js import 该常量（ESM 侧的取用路径）",
  /import\s*\{[^}]*\bPOLL_MAX_MISSES\b[^}]*\}\s*from\s*["'][^"']*constants\.js["']/.test(downloaderSrc));
// 内容脚本侧的取用路径：常量必须真挂在 content-shared.js 的共享对象上。
// 漏挂时 content.js 的 P.POLL_MAX_MISSES 是 undefined（面板永停「正在连接…」），
// 只有产物这一层的痕迹能提前发现。
check("content-shared.js 产物挂着 POLL_MAX_MISSES（漏挂则浮层取到 undefined）",
  /POLL_MAX_MISSES/.test(sharedSrc));
check("content.js 取到的阈值等于唯一来源的值（改了一处忘另一处必红）",
  ui.POLL_MAX_MISSES === constMax, { content: ui.POLL_MAX_MISSES, constants: constMax });
// 轮询周期是 700ms：阈值太小会让"服务刚起、任务还没登记"的正常窗口被判成丢失。
check("阈值对应至少 5 秒的等待窗口（15 × 700ms ≈ 10.5s）",
  ui.POLL_MAX_MISSES >= 5, ui.POLL_MAX_MISSES);

// ---------- B. 判据真值表（边界） ----------
console.log("B. pollGiveUp 的边界");
check("第 1 次查不到（misses=1）不放弃", ui.pollGiveUp(1) === false);
check("刚到上限（misses=15）仍不放弃 —— 上限说的是「超过」", ui.pollGiveUp(ui.POLL_MAX_MISSES) === false,
  ui.pollGiveUp(ui.POLL_MAX_MISSES));
check("超过上限（misses=16）放弃", ui.pollGiveUp(ui.POLL_MAX_MISSES + 1) === true);
check("返回值是布尔而不是计数（调用方直接当条件用）",
  ui.pollGiveUp(0) === false && ui.pollGiveUp(999999) === true);

// ---------- C. 接线：放弃时必须落到可区分终态，不许静默停 ----------
console.log("C. 接线：停止轮询 + 落到失败面板");
check("misses 计数器声明存在", /\blet misses = 0;/.test(src));
// ⚠ 这条必须钉住"重置的位置"，不能只测 `misses = 0;` 出现过 —— `let misses = 0;`
//   自身就含这个子串，那样的断言恒真（反向验证 P5 抓到过：删掉重置它照样绿）。
check("查到任务后重置计数（否则一次网络抖动会一路累计到放弃）",
  /misses = 0;\s*\n\s*const state = resp\.state;/.test(src));
check("miss 分支不再是裸 return（回退到 P2-2 的老写法会红）",
  /if \(!resp \|\| !resp\.exists\) \{/.test(src) &&
  !/if \(!resp \|\| !resp\.exists\) return;/.test(src));
check("放弃时清掉 interval（真停轮询）",
  /pollGiveUp\([\s\S]{0,400}?clearInterval\(interval\)/.test(src));
check("放弃时落到 error 面板（不是静默停 —— 用户要能看出失败了）",
  /pollGiveUp\([\s\S]{0,700}?showPanel\("error"/.test(src));
check("失败文案点明原因（服务重启 / 任务被清理）",
  /任务状态丢失（本地服务可能已重启）/.test(src));

// ---------- D. P3-3 真跑：hideButton 必须让在途检查作废 ----------
console.log("D. hideButton 作废在途检查（真跑，不是扫字符串）");
const t0 = ui.gateTokenNow();
ui.hideButton();
check("hideButton 之后 token 前移 1（在途检查的 token 不再匹配 ⇒ 不会画回按钮）",
  ui.gateTokenNow() === t0 + 1, { before: t0, after: ui.gateTokenNow() });
ui.hideButton();
check("连续隐藏继续前移（幂等且不回退）", ui.gateTokenNow() === t0 + 2, ui.gateTokenNow());
// 结构性兜底：token 的自增点必须有两处（发起检查 + 作废在途结果）。
// 注意两种写法都要算：发起检查处是前置自增 `++gateToken`，hideButton 处是后置。
const bumps = (src.match(/\+\+gateToken|gateToken\+\+/g) || []).length;
check("gateToken 至少两处自增（发起检查 / hideButton 作废）", bumps >= 2, bumps);

console.log(`\n${fail === 0 ? "全部通过" : "有失败"}：${pass} ok / ${fail} FAIL`);
process.exit(fail === 0 ? 0 : 1);
