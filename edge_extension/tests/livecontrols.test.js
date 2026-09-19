// content.js 进度面板「控制按钮分流」的回归测试。
// 用法（在 edge_extension 目录下）：node tests/livecontrols.test.js
//
// 缺陷形态（学徒实测报出）：直播任务的面板里显示「⏸ 暂停」，而服务端 /pause
// 对直播直接 400 —— "直播录制请使用停止：直播流不支持暂停后续录（暂停期间的分片
// 已从列表滚走）"。用户点下去只会换来一条报错提示，按钮本身不动。
//
// 直播只有「停止」「取消」两态：暂停期间的流已从滑动窗口滚走，续录只会在产物
// 中间留一个时间轴空洞。这不是能力缺失，是产品语义 —— 主界面 index.html 早已
// 按这个语义走（'⏹ 停止（保存已录）'），扩展面板当时漏了。
//
// 守两件事：
//   1. **直播不得出现暂停/继续**（controlButtons 真跑，不是扫字符串）；
//   2. **点播的暂停/继续不能被改坏**（同一条分流逻辑的另一半）。
//
// 加载方式与 tests/downloader.test.js 相同：content.js 是经典脚本 IIFE，
// 不做模块化改造，用 new Function 喂进最小 DOM/chrome 桩。readyState 置
// "loading"，init() 不执行；顶层只有两个 window.addEventListener 注册。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");
const src = fs.readFileSync(path.join(DIR, "content.js"), "utf8");

const listeners = [];
const window = {
  __m3u8_catcher_injected__: false,
  addEventListener(type) {
    listeners.push(type);
  },
};
const document = {
  readyState: "loading", // 让 init() 不跑：本文件只测纯函数
  addEventListener() {},
  querySelector: () => null,
  querySelectorAll: () => [],
  createElement: () => ({ style: {}, appendChild() {}, addEventListener() {}, setAttribute() {} }),
  body: { appendChild() {}, removeChild() {} },
  documentElement: { appendChild() {} },
};
const mod = { exports: {} };

// content.js 顶层要求共享对象已注入（缺失立即抛——评审自审 #11 的显式失败口径，
// 不再降级成空对象）。本文件只测纯函数，给个最小桩让顶层通过即可。
globalThis.__m3u8Shared = { sanitizeFileName: (s) => s, qualityFromURL: () => "" };

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
if (!ui || typeof ui.controlButtons !== "function") {
  console.error("content.js 未导出 controlButtons（测试钩子被删了？）");
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
const ctls = (live, paused) => ui.controlButtons(live, paused).map((b) => b.ctl);
const hasText = (live, paused, needle) =>
  ui.controlButtons(live, paused).some((b) => b.label.includes(needle));

console.log("直播：只有「停止」「取消」");
check("运行中 → stop + cancel，没有 pause", JSON.stringify(ctls(true, false)) === '["stop","cancel"]', ctls(true, false));
check("中断态（paused=true）仍是 stop + cancel —— 直播没有\"继续\"", JSON.stringify(ctls(true, true)) === '["stop","cancel"]', ctls(true, true));
check("停止按钮文案说\"停止\"不说\"暂停\"", hasText(true, false, "停止") && !hasText(true, false, "暂停"));
check("stop 是主操作（primary），cancel 是危险操作（danger）",
  ui.controlButtons(true, false)[0].kind === "primary" && ui.controlButtons(true, false)[1].kind === "danger");
check("每个按钮都带可执行的 ctl 值（renderInitiatedControls 直接取用）",
  ui.controlButtons(true, false).every((b) => typeof b.ctl === "string" && b.ctl.length > 0));

console.log("点播：暂停 / 继续 保持不变");
check("运行中 → pause + cancel", JSON.stringify(ctls(false, false)) === '["pause","cancel"]', ctls(false, false));
check("暂停中 → resume + cancel", JSON.stringify(ctls(false, true)) === '["resume","cancel"]', ctls(false, true));
check("暂停态文案仍是\"继续下载\"", hasText(false, true, "继续"));
check("点播运行中的 pause 按钮不带\"停止\"字样", !hasText(false, false, "停止"));

console.log("配色与图标：与内置前端同一套");
const CASES = [
  [true, false, "直播运行中"],
  [false, false, "点播运行中"],
  [false, true, "点播已暂停"],
];
check(
  "三个态的主操作都是 primary（蓝底）——暂停原先漏成 plain",
  CASES.every(([live, paused]) => ui.controlButtons(live, paused)[0].kind === "primary"),
  CASES.map(([l, p]) => ui.controlButtons(l, p)[0].kind)
);
check(
  "三个态的 cancel 都是 danger（红）——暂停态的取消原先漏成 plain",
  CASES.every(([live, paused]) => {
    const c = ui.controlButtons(live, paused).find((b) => b.ctl === "cancel");
    return c && c.kind === "danger";
  }),
  CASES.map(([l, p]) => (ui.controlButtons(l, p).find((b) => b.ctl === "cancel") || {}).kind)
);
check("「继续」用 ⏯ 不用 ▶ —— ▶ 已被「打开文件 / 播放」占用，同形不同义",
  hasText(false, true, "⏯") && !hasText(false, true, "▶"));
check("控制按钮的 kind 只剩 primary/danger 两种（plain 退为未知 kind 的兜底）",
  !/ctl:\s*"[a-z]+",\s*label:\s*"[^"]*",\s*kind:\s*"plain"/.test(src));

console.log("接线：面板与 background 真的懂 stop");
check("renderInitiatedControls 消费 controlButtons", /renderInitiatedControls[\s\S]{0,700}?controlButtons\(/.test(src));
check("trackDownload 按服务端 live 字段更新本地态",
  /const live = !!resp\.live/.test(src) && /activeLive = live\b/.test(src));
check("activeLive 有初值（首帧不至于先画成点播）", /let activeLive\s*=/.test(src));
check("sendControl 未把 stop 排除在外", /"stop"|'stop'/.test(src));
const apiSrc = fs.readFileSync(path.join(DIR, "src", "background", "server-api.js"), "utf8");
check("server-api actionMap 含 stop → /stop 且走 POST",
  /stop:\s*\{\s*path:\s*"stop",\s*method:\s*"POST"\s*\}/.test(apiSrc));
check("server-api actionMap resume 走 POST（与 Go 侧 routeDefs 同步）",
  /resume:\s*\{\s*path:\s*"resume",\s*method:\s*"POST"\s*\}/.test(apiSrc));
check("server-api actionMap pause/cancel 维持历史 GET 契约",
  /pause:\s*\{\s*path:\s*"pause",\s*method:\s*"GET"\s*\}/.test(apiSrc) &&
  /cancel:\s*\{\s*path:\s*"cancel",\s*method:\s*"GET"\s*\}/.test(apiSrc));
check("controlTask 把动作的方法带给 apiFetch", /method:\s*act\.method/.test(apiSrc));
check("triggerDownload 按本任务重算 activeLive（清上一任务的直播残留）",
  /activeLive = !!\(source && source\.live\)/.test(src));
check("queryDownload 把 live 透出给 content.js", /live:\s*!!t\.live/.test(apiSrc));

console.log("反向断言：直播不再被承诺\"可暂停/继续\"");
check("面板底部的固定文案已按 live 分流（不再是无条件那句话）",
  !/>下载过程不经过浏览器下载栏，可在此暂停\/继续或取消。</.test(src));
check("content.js 里没有把 pause 直接发给直播的兜底路径",
  !/live[\s\S]{0,80}sendControl\(["']pause/.test(src));

console.log(`\n${fail === 0 ? "全部通过" : "有失败"}：${pass} ok / ${fail} FAIL`);
process.exit(fail === 0 ? 0 : 1);
