// 按钮上的 ✕：点掉之后**本页不再自动弹出**（2026-09-30 学徒：「下载按钮应该可以关掉，
// 关掉后需重新刷新页面后才会显示」）。
// 用法（在 edge_extension 目录下）：node tests/buttondismiss.test.js
//
// 契约：
//   ① 按钮里有 ✕，且与文字之间有一条竖分隔线（形态由学徒在三个方案里拍板为 A）；
//   ② 点 ✕ ⇒ 按钮撤掉，且**之后**悬停任何视频都不再弹（不是"只删掉当前这个元素"）；
//   ③ **刷新页面恢复**：所以关闭状态只能是内存里的，一旦落盘就跨会话记住了 ——
//      这条在真机上由 `.workbuddy/tmp/btn_close_verify.mjs` 用"刷新后按钮回来"证明，
//      这里守它的前提：整个过程不写 storage；
//   ④ 在途的链接检查回来时不许把按钮画回去（用户点 ✕ 的那一刻，别的目标的检查
//      可能正挂在 await 上）。
//
// 为什么真跑而不是扫字符串：把 showButtonIfSniffed 里的抑制删掉、只把 `btnDismissed`
// 这个名字留在注释里，纯文本断言照样能绿。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");
const src = fs.readFileSync(path.join(DIR, "content.js"), "utf8");

// ---------------- 最小 DOM 桩 ----------------
// 比 buttonpin.test.js 多一样：innerHTML 里出现过的 class 会被记下来，
// querySelector(".cls") 据此返回一个**能挂监听器的假子节点** —— ✕ 的点击处理
// 就是挂在那上面的，不模拟这一步就没法真跑"点关闭"。
function makeNode(tag) {
  const node = {
    tagName: (tag || "div").toUpperCase(),
    className: "",
    style: {},
    offsetHeight: 32,
    removed: false,
    _html: "",
    _classes: [],
    _listeners: {},
    _children: {},
    appendChild(c) { return c; },
    remove() { node.removed = true; },
    addEventListener(type, fn) { (node._listeners[type] = node._listeners[type] || []).push(fn); },
    removeEventListener() {},
    setAttribute() {},
    contains: () => false,
    getBoundingClientRect: () => ({ left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }),
  };
  Object.defineProperty(node, "innerHTML", {
    get: () => node._html,
    set: (v) => {
      node._html = String(v);
      node._classes = [...String(v).matchAll(/class="([^"]+)"/g)]
        .flatMap((m) => m[1].split(/\s+/))
        .filter(Boolean);
    },
  });
  node.querySelector = (sel) => {
    const want = String(sel).replace(/^\./, "");
    if (!node._classes.includes(want)) return null;
    node._children[want] = node._children[want] || makeNode("span");
    return node._children[want];
  };
  return node;
}

let lastButton = null;
let storageWrites = 0;
let sendMessageCalls = 0;
// 门控查询：默认恒"已嗅探到"，好让按钮真的建出来；在途场景里换成挂起的 promise
let sendMessageImpl = async () =>
  ({ ok: true, source: { url: "https://cdn.example.com/a/index.m3u8" } });

const window = { __m3u8_catcher_injected__: false, addEventListener() {} };
const document = {
  readyState: "loading", // 让 init() 不跑：本文件只测按钮本体，不碰悬停链
  addEventListener() {},
  createElement: (t) => makeNode(t),
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
  storage: { local: { get: async (d) => d, set: async () => { storageWrites++; } } },
  runtime: {
    id: "test-extension-id",
    sendMessage: (msg) => { sendMessageCalls++; return sendMessageImpl(msg); },
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
if (!ui || typeof ui.showButtonIfSniffed !== "function" || typeof ui.hideButton !== "function"
  || typeof ui.dismissButton !== "function" || typeof ui.btnDismissedNow !== "function") {
  console.error("content.js 未导出关闭按钮所需的测试钩子（被删了？）");
  process.exit(1);
}

const VID = { left: 20, right: 380, top: 56, bottom: 256, width: 360, height: 200 };
const mkTarget = (tag) => ({
  el: { isConnected: true, tag, getBoundingClientRect: () => VID },
  src: `https://cdn.example.com/${tag}/index.m3u8`,
  pageUrl: `https://example.com/watch/${tag}`,
});

(async () => {
  // ---------------- ① 结构：✕ 与分隔线 ----------------
  console.log("① 按钮结构（学徒拍板的 A 形态：✕ 常驻内嵌 + 竖分隔线）");
  ui.hideButton();
  lastButton = null;
  await ui.showButtonIfSniffed(mkTarget("A"));
  check("未关闭时，命中链接 ⇒ 按钮建得出来（对照腿：否则「永远不显示」也能全绿）",
    !!lastButton && lastButton.removed === false);
  if (!lastButton) { console.log(`\n按钮都没建出来，后续免谈：共 1 项，FAIL 1`); process.exit(1); }
  check("按钮里有 ✕", lastButton.innerHTML.includes("✕"), lastButton.innerHTML);
  check("✕ 与文字之间有竖分隔线（.sep）", lastButton.innerHTML.includes('class="sep"'));
  const closeEl = lastButton.querySelector(".cls");
  const closeHandler = closeEl && closeEl._listeners.click && closeEl._listeners.click[0];
  check("✕ 上挂了点击处理（没有它就是个装饰）", typeof closeHandler === "function");

  // ---------------- ② 在途场景：点 ✕ 的同一刻，别的目标的检查还挂在 await 上 ----------------
  // 真实时序：按钮（属于目标 A）还显示着 → 指针移到目标 B → B 的链接检查发出去（未回）
  // → 用户点了 ✕ → B 的检查才回来。它**不许**把按钮画回来。
  console.log("② 在途的链接检查回来时不许把按钮画回去");
  let release;
  sendMessageImpl = () => new Promise((r) => { release = r; });
  const pending = ui.showButtonIfSniffed(mkTarget("B"));
  await Promise.resolve(); // 推进到 await sendMessage（此刻 B 的监听在途）

  const ev = {
    preventDefault() { ev.preventDefaultCalled = true; },
    stopPropagation() { ev.stopPropagationCalled = true; },
  };
  closeHandler(ev); // ← 真调一次 ✕ 的处理函数（等价于用户点下去）

  check("点 ✕ 后按钮被撤掉", lastButton.removed === true);
  check("点 ✕ 调了 stopPropagation（不拦下来会冒泡到按钮自己的 click，变成「点关闭反而开始下载」）",
    ev.stopPropagationCalled === true);
  check("点 ✕ 调了 preventDefault", ev.preventDefaultCalled === true);
  check("关闭标志已置位", ui.btnDismissedNow() === true);

  release({ ok: true, source: { url: "https://cdn.example.com/b/index.m3u8" } });
  await pending;
  check("在途的 B 检查回来 ⇒ 没有新建按钮", !lastButton || lastButton.removed === true,
    lastButton && lastButton.removed);

  // ---------------- ③ 关闭后：本页不再自动弹（同一个 / 换一个目标都不行） ----------------
  console.log("③ 关闭后本页不再自动弹按钮（全局，不是只记住关掉的那一个）");
  sendMessageImpl = async () => ({ ok: true, source: { url: "https://cdn.example.com/a/index.m3u8" } });
  const callsBefore = sendMessageCalls;
  lastButton = null;
  await ui.showButtonIfSniffed(mkTarget("A"));
  check("再悬停**同一个**视频 ⇒ 不弹", lastButton === null, lastButton && "居然建了按钮");
  lastButton = null;
  await ui.showButtonIfSniffed(mkTarget("C"));
  check("悬停**另一个**视频 ⇒ 也不弹（关闭是本页级的）", lastButton === null, lastButton && "居然建了按钮");
  // 抑制挡在函数最前面 ⇒ 连"这次悬停有没有链接"的消息都不该发出去。
  // 这条才是入口那道检查的区分力所在：只把抑制挪到 sendMessage 之后，按钮一样不弹、
  // 上面两条照样绿，但消息已经白白发了两趟（页面上每次悬停都发）。
  check("已关闭 ⇒ 一次 getVideoSource 都不发（抑制挡在发消息之前）",
    sendMessageCalls === callsBefore, { before: callsBefore, after: sendMessageCalls });

  // ---------------- ④ 不落盘（"刷新才恢复"的前提） ----------------
  check("整个过程没写 storage（写进去就跨会话记住了，刷新也不会恢复）",
    storageWrites === 0, { storageWrites });
  check("关闭状态只在内存里：没有可读回的持久化字段",
    !/btnDismissed\s*[^\n]*chrome\.storage/.test(src) &&
    !src.includes("storage.local.set({ btnDismissed"));

  console.log(`\n按钮关闭契约：共 ${pass + fail} 项，PASS ${pass}，FAIL ${fail}`);
  process.exit(fail ? 1 : 0);
})();
