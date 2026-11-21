// 悬停按钮的保命契约：**指针已经在按钮上、或正朝按钮去时，站点把小窗收走，不许把按钮
// 一起收走**（2026-09-30 学徒实测：「这种小窗视频，下载按钮点不到，移过去就会消失」）。
// 用法（在 edge_extension 目录下）：node tests/buttonpin.test.js
//
// 真机量到的现场（`.workbuddy/tmp/mini_btn.mjs`，真 Chrome + 真扩展 + 假页面）：
//   按钮 (20,16)~(125,48) —— 放在视频**外部上方**；视频矩形 (20,56)~(380,256)。
//   中间那条 8px 的缝就是 BTN_GAP。指针从视频中心往上走，到 y=52（缝里）时站点把小窗
//   收掉（真站点两种做法都试了：销毁 video / 置 display:none），实时矩形塌成 0×0 ⇒
//   旧代码里 inButtonZone 退化成"原点上的一个点" ⇒ 下一次 mouseover 走 findMediaTargetAtPoint
//   找不到目标 ⇒ hideButton。修复前那台真机上 7 项里 4 项 FAIL（两种形态各 2 项）。
//
// 本文件守两件事：
//   ① 「按钮区」这个契约本身（pinHold 真值表：在按钮上 / 在缝里 / 在目标里 / 远处）；
//   ② 接线（真跑一遍 showButtonIfSniffed → 站点收窗 → repositionButton）：
//      指针在按钮区里 ⇒ **按钮还在且没挪窝**；指针在远处 ⇒ **必须收起**。
//      后一条是成对的对照腿：没有它，"永远不收起"这种改法也能全绿。
//
// 为什么真跑而不是扫字符串：把 `pointerInButtonZone()` 那个守卫删掉、只留一句注释或
// 一个同名但恒真的函数，纯文本断言照样能绿（白名单词还在文件里）。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");
const src = fs.readFileSync(path.join(DIR, "content.js"), "utf8");

// ---------------- 最小 DOM 桩 ----------------
// 只要够撑住 createElement/appendChild/getBoundingClientRect/remove 这条链。
function makeNode() {
  const node = {
    className: "",
    style: {},
    offsetHeight: 32, // 真机上按钮高 32px（按钮区条带的高度就靠它）
    removed: false,
    _html: "",
    appendChild(c) { return c; },
    remove() { node.removed = true; },
    addEventListener() {},
    removeEventListener() {},
    setAttribute() {},
    contains: () => false,
    // 桩里没有真解析器：本文件不碰按钮上的 ✕，返回 null 即可
    // （showButton 对它是 `if (closeEl)` 的用法，null 走得通）。
    querySelector: () => null,
    getBoundingClientRect: () => ({ left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }),
  };
  Object.defineProperty(node, "innerHTML", {
    get: () => node._html,
    set: (v) => { node._html = String(v); },
  });
  return node;
}

let lastButton = null;
const window = { __m3u8_catcher_injected__: false, addEventListener() {} };
const document = {
  readyState: "loading", // 让 init() 不跑：本文件只测按钮定位与隐藏
  addEventListener() {},
  createElement: () => makeNode(),
  querySelector: () => null,
  querySelectorAll: () => [],
  body: {
    appendChild(n) { lastButton = n; return n; },
    removeChild() {},
    querySelector: () => null,
    querySelectorAll: () => [],
  },
  documentElement: { appendChild() {}, querySelector: () => null },
  head: { appendChild() {} },
  title: "测试页",
};
const chrome = {
  storage: { local: { get: async () => ({}) } },
  runtime: {
    id: "test-extension-id",
    // 门控查询：恒"已嗅探到"，好让按钮真的建出来
    sendMessage: async (msg) => (msg && msg.type === "getVideoSource" ? { ok: true, source: { url: "https://cdn.example.com/a/index.m3u8" } } : { ok: true }),
  },
};
const mod = { exports: {} };

globalThis.__m3u8Shared = {
  POLL_MAX_MISSES: 15,
  sanitizeFileName: (s) => String(s || ""),
  qualityFromURL: () => "",
  escapeHtml: (s) => String(s == null ? "" : s),
  assembleGoCommand: () => "",
};

new Function(
  "window", "document", "chrome", "location", "history", "navigator", "module", src
)(window, document, chrome, { href: "https://example.com/watch/1" }, {}, { userAgent: "" }, mod);

let pass = 0;
let fail = 0;
function check(name, cond, extra) {
  if (cond) { pass++; console.log("  ok  -", name); }
  else { fail++; console.log("  FAIL-", name, extra === undefined ? "" : JSON.stringify(extra)); }
}

const ui = mod.exports.__ui;
if (!ui || typeof ui.pinHold !== "function" || typeof ui.repositionButton !== "function"
  || typeof ui.showButtonIfSniffed !== "function" || typeof ui.setPointer !== "function"
  || typeof ui.targetRect !== "function") {
  console.error("content.js 未导出按钮保命逻辑的测试钩子（被删了？）");
  process.exit(1);
}

// 真机现场的两个矩形
const BTN = { left: 20, right: 125, top: 16, bottom: 48 };
const VID = { left: 20, right: 380, top: 56, bottom: 256, width: 360, height: 200 };

console.log("① pinHold 真值表（按钮区契约）");
check("在按钮上 ⇒ 算", ui.pinHold(72, 32, BTN, VID, 32) === true);
check("在缝里（y=52，视频顶 56 / 按钮底 48）⇒ 算 —— 这就是真机上按钮死掉的那一步",
  ui.pinHold(80, 52, BTN, VID, 32) === true);
check("在视频里 ⇒ 算", ui.pinHold(80, 156, BTN, VID, 32) === true);
check("目标矩形底下（y=300）⇒ 不算", ui.pinHold(80, 300, BTN, VID, 32) === false);
check("横向跑出去（x=400）⇒ 不算", ui.pinHold(400, 156, BTN, VID, 32) === false);
check("按钮上的点不依赖目标矩形（目标矩形缺了也得算在按钮上）",
  ui.pinHold(72, 32, BTN, null, 32) === true);
check("没有按钮矩形 ⇒ 不算（别对空对象放行）", ui.pinHold(72, 32, null, VID, 32) === false);

// ---------------- 真跑：建按钮 ----------------
async function mountButton(rect) {
  ui.hideButton();
  lastButton = null;
  const video = {
    isConnected: true,
    rect,
    getBoundingClientRect() { return this.rect; },
  };
  await ui.showButtonIfSniffed({ el: video, src: "https://cdn.example.com/a/index.m3u8", pageUrl: "https://example.com/watch/1" });
  return video;
}

(async () => {
  console.log("② 真跑：按钮出现在视频外部正上方");
  const v1 = await mountButton(VID);
  check("按钮已建出来", !!lastButton && lastButton.removed === false);
  check("落在视频上方 8px 处（top=56-32-8=16 / left=20）",
    lastButton.style.top === "16px" && lastButton.style.left === "20px",
    [lastButton.style.top, lastButton.style.left]);

  console.log("③ 视频贴近视口顶端 ⇒ 回退到视频内部左上角（老行为，别改坏）");
  const v2 = await mountButton({ left: 20, top: 10, right: 380, bottom: 210, width: 360, height: 200 });
  check("上方塞不下时放进视频内部（top=10+8=18 / left=20+8=28）",
    lastButton.style.top === "18px" && lastButton.style.left === "28px",
    [lastButton.style.top, lastButton.style.left]);
  v2.isConnected = false;

  // ---- ④ 核心：站点把小窗收走 ----
  // 形态 A：video 被销毁（真站点常见）；形态 B：只藏起来，矩形塌成 0×0。
  for (const scene of [
    { name: "销毁小窗（元素断连）", kill: (v) => { v.isConnected = false; } },
    { name: "藏起小窗（矩形塌成 0×0）", kill: (v) => { v.rect = { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; } },
  ]) {
    const v = await mountButton(VID);
    const before = [lastButton.style.top, lastButton.style.left];
    ui.setPointer(80, 52); // 指针走到缝里 —— 真机上按钮就是死在这一步
    scene.kill(v);
    ui.repositionButton();
    check(`[${scene.name}] 指针在缝里 ⇒ 按钮留着`, lastButton && lastButton.removed === false);
    check(`[${scene.name}] 且没挪窝（还是 ${before.join("/")}）`,
      lastButton.style.top === before[0] && lastButton.style.left === before[1],
      [lastButton.style.top, lastButton.style.left]);
  }

  // ---- ⑤ 对照腿：指针在按钮上（伸手的最后一步） ----
  {
    const v = await mountButton(VID);
    ui.setPointer(72, 32);
    v.isConnected = false;
    ui.repositionButton();
    check("指针正压在按钮上 ⇒ 按钮留着", lastButton.removed === false);
  }

  // ---- ⑥ 对照腿：同样收窗，但指针在远处 ⇒ 必须收起（防"永远不收起"） ----
  {
    const v = await mountButton(VID);
    ui.setPointer(400, 400);
    v.isConnected = false;
    ui.repositionButton();
    check("指针在远处 ⇒ 按钮必须收起（成对：否则钉住就成了永不消失）",
      !lastButton || lastButton.removed === true);
  }

  // ---- ⑦ 收起后不留残影：targetRect 归零（下一个目标不受上一个视频的矩形影响） ----
  ui.hideButton();
  check("hideButton 之后 targetRect() 归 null", ui.targetRect() === null, ui.targetRect());

  console.log(`\n按钮保命契约：共 ${pass + fail} 项，PASS ${pass}，FAIL ${fail}`);
  process.exit(fail ? 1 : 0);
})();
