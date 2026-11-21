// 面板布局契约：**内容超高的面板不许吃掉操作按钮**（2026-09-30 学徒实测报出）。
// 用法（在 edge_extension 目录下）：node tests/panellayout.test.js
//
// 缺陷形态：学徒把浏览器窗口缩小后，「确认下载 / 取消」整行消失 —— 面板卡片封顶
// max-height:85vh + overflow:hidden，而内容直接当 flex 子项，超过 85vh 的部分被
// **静默裁掉**，当时也没有滚动容器，滚都滚不到。真机量到的阈值：面板内容 554px 时
// 窗口高 ≤652 就开始裁；800×520 下按钮 top/bottom = 398~442，而卡片底边在 352。
// 同一条外壳还管着 error 面板（兜底命令），它在 800×380 下也被裁（`.workbuddy/tmp/panel_fit.mjs`）。
//
// 本文件守的是**布局契约**，不是文案：
//   1. 卡片仍然封顶（不吃满整屏）—— 滚动区能救回按钮，前提是卡片自己不超过窗口；
//   2. 主体外面有**可收缩的滚动区**（`min-height:0` 不能省，缺了它 flex 子项不肯收缩，
//      等于没修 —— 真机上量过）；
//   3. 「必须够得着」的按钮（确认下载 / 取消 / 复制命令）落在滚动区**之外**且 id 唯一
//      —— 搬一半会变成两个同名 id，那比原来更糟。
//
// 为什么真跑而不是扫字符串：同一个 id 留在 body 里、或 footer 参数忘了插进模板，
// 纯字符串断言照样能全绿（只要白名单词还在文件里）。这里喂最小 DOM 桩，让
// renderConfirmPanel / showPanel 走一遍真实渲染，再对**产物 HTML** 做标签配对。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");
const src = fs.readFileSync(path.join(DIR, "content.js"), "utf8");

// ---------------- 最小 DOM 桩 ----------------
// 只要能承载 createPanel()/panel.innerHTML/panel.querySelector，不做真实渲染。
function makeNode() {
  const node = {
    id: "",
    style: {},
    dataset: {},
    children: [],
    classList: { add() {}, remove() {}, contains: () => false },
    _html: "",
    removed: false,
    appendChild(c) { node.children.push(c); return c; },
    removeChild() {},
    remove() { node.removed = true; },
    addEventListener() {},
    removeEventListener() {},
    setAttribute() {},
    getAttribute: () => null,
    querySelector: () => makeNode(),
    querySelectorAll: () => [],
    contains: () => false,
    getBoundingClientRect: () => ({ x: 0, y: 0, top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 }),
  };
  Object.defineProperty(node, "innerHTML", {
    get: () => node._html,
    set: (v) => { node._html = String(v); },
  });
  return node;
}

let lastPanel = null;
const listeners = [];
const window = { __m3u8_catcher_injected__: false, addEventListener: (t) => listeners.push(t) };
const document = {
  readyState: "loading", // 让 init() 不跑：本文件只测面板渲染
  addEventListener() {},
  createElement: () => makeNode(),
  querySelector: () => null,
  querySelectorAll: () => [],
  body: {
    appendChild(n) { lastPanel = n; return n; },
    removeChild() {},
    querySelector: () => null,
    querySelectorAll: () => [],
  },
  documentElement: { appendChild() {}, querySelector: () => null },
  head: { appendChild() {} },
  title: "",
};
const chrome = {
  storage: { local: { get: async () => ({}) } },
  runtime: { sendMessage: async () => { throw new Error("node 桩：无 background"); } },
};
const mod = { exports: {} };

// 顶层只校验 P 与 P.POLL_MAX_MISSES（缺了立刻抛）。其余字段按本文件用到的给。
globalThis.__m3u8Shared = {
  POLL_MAX_MISSES: 15,
  sanitizeFileName: (s) => String(s || ""),
  qualityFromURL: () => "",
  escapeHtml: (s) => String(s == null ? "" : s),
  assembleGoCommand: (u, ref, out) => `chcp 65001 ; go-catcher.exe --url="${u}" -o "${out}"`,
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
if (!ui || typeof ui.renderConfirmPanel !== "function" || typeof ui.showPanel !== "function") {
  console.error("content.js 未导出 renderConfirmPanel / showPanel（测试钩子被删了？）");
  process.exit(1);
}

// ---------------- 产物 HTML 的结构读取 ----------------
// 找出面板主体的滚动容器 [start, end)：定位靠 `overflow-y:auto`（外壳里第一个出现的
// 就是主体容器，它排在所有内容之前），再回溯到它自己的 <div>，按 <div>/</div> 配对
// 找闭合位置。模板里没有自闭合 div、没有注释。
// ⚠️ 定位串**只认 overflow-y:auto**：把 `min-height:0` 也写进定位串，会让"少写这条"
// 的失败信息退化成"找不到滚动区"，指向不准 —— 那一条单独断言（见下面的 openTag）。
function scrollerRange(html) {
  const at = html.indexOf("overflow-y:auto");
  if (at < 0) return null;
  const start = html.lastIndexOf("<div", at);
  if (start < 0) return null;
  const openTag = html.slice(start, html.indexOf(">", start) + 1);
  const re = /<div\b|<\/div>/g;
  re.lastIndex = start;
  let depth = 0;
  let m;
  while ((m = re.exec(html)) !== null) {
    depth += m[0] === "</div>" ? -1 : 1;
    if (depth === 0) return { start, end: m.index + "</div>".length, openTag, html };
  }
  return null;
}
const countId = (html, id) => (html.match(new RegExp(`id="${id}"`, "g")) || []).length;
const inside = (r, needle) => r && (() => { const i = r.html.indexOf(needle); return i > r.start && i < r.end; })();

// ---------------- ① 外壳本身 ----------------
console.log("外壳：卡片封顶 + 主体可滚 + footer 槽");
const shellNoFooter = ui.panelShell("标题", "<div>正文</div>");
check("卡片仍封顶 max-height:85vh（滚动区能救回按钮的前提）", /max-height:85vh/.test(shellNoFooter));
check("卡片仍 overflow:hidden（内容不许顶出卡片）", /overflow:hidden/.test(shellNoFooter));
check("主体包在可滚动容器里", scrollerRange(shellNoFooter) !== null);
check("滚动容器能收缩（flex:1 1 auto + min-height:0）—— 缺 min-height:0 则 flex 子项默认 min-height:auto，不肯收缩＝没修",
  /flex:1 1 auto/.test(scrollerRange(shellNoFooter).openTag) && /min-height:0/.test(scrollerRange(shellNoFooter).openTag),
  scrollerRange(shellNoFooter).openTag);
check("不传 footer 时不留一行 undefined 文本", !/>\s*undefined\s*</.test(shellNoFooter), shellNoFooter.slice(-120));

const shellWithFooter = ui.panelShell("标题", "<div>正文</div>", `<div id="ft">底部</div>`);
const rShell = scrollerRange(shellWithFooter);
check("传了 footer 就渲染出来", shellWithFooter.includes('id="ft"'));
check("footer 排在滚动区之外（之后），不跟内容一起滚", rShell && shellWithFooter.indexOf('id="ft"') > rShell.end);

// ---------------- ② 确认面板 ----------------
console.log("确认面板：按钮钉底");
ui.renderConfirmPanel({ url: "https://example.com/media/1080p/index.m3u8?sig=abc", title: "第一集", quality: "1080P" });
const confirmHtml = lastPanel && lastPanel.innerHTML;
check("确认面板渲染出 HTML", typeof confirmHtml === "string" && confirmHtml.length > 200);
const rConfirm = scrollerRange(confirmHtml || "");
check("有滚动区", rConfirm !== null);
check("内容（视频名称/资源链接/保存文件名）在滚动区里",
  inside(rConfirm, "视频名称") && inside(rConfirm, "资源链接") && inside(rConfirm, "保存文件名"));
check("「⬇ 确认下载」在滚动区之外 —— 窗口变小也够得着",
  !!rConfirm && (confirmHtml.indexOf('id="confirm-go"') > rConfirm.end));
check("「取消」同样在滚动区之外", !!rConfirm && (confirmHtml.indexOf('id="confirm-cancel"') > rConfirm.end));
check("两个按钮都只出现一次（搬一半会变成同名 id 两个）",
  countId(confirmHtml, "confirm-go") === 1 && countId(confirmHtml, "confirm-cancel") === 1,
  [countId(confirmHtml, "confirm-go"), countId(confirmHtml, "confirm-cancel")]);

(async () => {
  // ---------------- ③ error 面板（兜底命令） ----------------
  console.log("error 面板：复制命令钉底");
  await ui.showPanel("error", {
    message: "目录选择失败 (HTTP 500)",
    m3u8Url: "https://example.com/media/1080p/index.m3u8?sig=" + "x".repeat(400),
    pageUrl: "https://example.com/watch/1",
    title: "第一集",
    code: "SERVER_DOWN",
  });
  const errHtml = lastPanel && lastPanel.innerHTML;
  check("error 面板渲染出 HTML", typeof errHtml === "string" && errHtml.length > 200);
  const rErr = scrollerRange(errHtml || "");
  check("有滚动区", rErr !== null);
  check("命令块在滚动区里（命令越长面板越高，由它自己消化）", inside(rErr, 'id="go-cmd-text"'));
  check("「复制命令」在滚动区之外 —— 超长 URL 也够得着",
    !!rErr && (errHtml.indexOf('id="copy-go-cmd"') > rErr.end));
  check("「复制命令」只出现一次", countId(errHtml, "copy-go-cmd") === 1, countId(errHtml, "copy-go-cmd"));
  check("复制后的反馈行跟着按钮一起钉底（不能留在滚动区里）",
    !!rErr && (errHtml.indexOf('id="copy-feedback"') > rErr.end));

  // ---------------- ④ 短面板不受影响 ----------------
  console.log("短面板（loading）：不传 footer 也正常");
  ui.showPanel("loading");
  const loadHtml = lastPanel && lastPanel.innerHTML;
  check("loading 面板正常渲染且无 undefined 残字",
    typeof loadHtml === "string" && loadHtml.includes("正在分析该视频的画质") && !/>\s*undefined\s*</.test(loadHtml));

  console.log(`\n${fail === 0 ? "全部通过" : "有失败项"}：pass=${pass} fail=${fail}`);
  process.exit(fail === 0 ? 0 : 1);
})();
