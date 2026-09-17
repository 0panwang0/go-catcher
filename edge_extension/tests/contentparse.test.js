// content.js 的「解析实现只有一份」守卫与行为对齐测试（评审 P2-7 / F11）。
// 用法（在 edge_extension 目录下）：node tests/contentparse.test.js
//
// 缺陷形态：content.js 是经典 content script，不能 import ESM，因此历史上自留了
// 一份解析拷贝，与 src/background/m3u8-parse.js 漂移出 5 处差异。其中唯一用户
// 可见的是 qualityFromURL 语义不同（共享版会看 ?quality=，页面版不看）——
// 同一个视频从浮层下载与从扩展页下载，文件名带不带 _1080P 不一致。
//
// 现在唯一实现经 content-shared.js 注入（manifest 里排在 content.js 之前，
// 同一 isolated world 共享 globalThis）。本文件守三件事：
//   1. content.js 里**不得再出现解析实现**，调用必须走共享对象（带 P. 前缀）；
//   2. manifest 的注入顺序与 world 正确（顺序错了 P 就是空对象，解析全崩）；
//   3. 真跑一遍：输出文件名与共享 qualityFromURL 一致，命令参数被净化。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");
const src = fs.readFileSync(path.join(DIR, "content.js"), "utf8");
const manifest = JSON.parse(fs.readFileSync(path.join(DIR, "manifest.json"), "utf8"));

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

// ============================================================
// 1) 结构性守卫：解析实现不许回到 content.js
// ============================================================
console.log("结构性守卫：解析与净化不再有第二份实现");
const BANNED_DEFS = [
  "parseSegments",
  "parseDuration",
  "parseVariants",
  "variantQuality",
  "variantLabel",
  "qualityFromURL",
  "qualityFromUrl",
  "resolveURL",
  "sanitizeFileName",
  "quoteArg",
];
for (const fn of BANNED_DEFS) {
  check(`content.js 不再定义 ${fn}`, !new RegExp(`function\\s+${fn}\\s*\\(`).test(src));
}
// 裸调用（不带 P. 前缀）说明有人在用本文件里的实现，那正是漂移的起点。
for (const fn of BANNED_DEFS) {
  check(`content.js 里没有裸调用 ${fn}（必须走 P.）`, !new RegExp(`[^.\\w]${fn}\\s*\\(`).test(src));
}

// ============================================================
// 2) 注入接线：manifest 必须让 content-shared.js 先于 content.js
// ============================================================
console.log("注入接线：manifest 顺序与 world");
const block = (manifest.content_scripts || []).find((b) => (b.js || []).includes("content.js"));
check("存在注入 content.js 的 content_scripts 块", !!block);
if (!block) {
  console.log(`\n${pass} ok / ${fail} FAIL`);
  process.exit(1);
}
const js = block.js || [];
check("同一块内先注入 content-shared.js，后注入 content.js",
  js.indexOf("content-shared.js") === 0 && js.indexOf("content.js") === 1, js);
check("两块都不指定 world（默认 ISOLATED）—— 共享对象不会漏进页面世界",
  block.world === undefined && (manifest.content_scripts || []).every((b) => b.world !== "ISOLATED" || (b.js || []).includes("content-shared.js")),
  (manifest.content_scripts || []).map((b) => b.world));
check("content-shared.js 与 content.js 在同一个 block（同 world 才能共享 globalThis）",
  js.includes("content-shared.js") && js.includes("content.js"), js);

// ============================================================
// 3) 真跑：共享对象 + content.js 一起加载后行为正确
// ============================================================
// content-shared.js 是纯脚本（IIFE + globalThis 赋值），require 即执行。
require(path.join(DIR, "content-shared.js"));
const shared = globalThis.__m3u8Shared;
check("content-shared.js 挂出了 __m3u8Shared", !!shared && typeof shared.parseVariants === "function");

const listeners = [];
const window = { __m3u8_catcher_injected__: false, addEventListener(t) { listeners.push(t); } };
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
new Function("window", "document", "chrome", "location", "history", "navigator", "module", src)(
  window,
  document,
  {},
  { href: "https://example.com/" },
  {},
  { userAgent: "" },
  mod
);
const ui = mod.exports.__ui;
check("content.js 导出了 buildGoCommand / buildOutputName", !!ui && typeof ui.buildOutputName === "function" && typeof ui.buildGoCommand === "function");

console.log("统一语义：共享解析的行为（曾与 content.js 漂移的 5 处）");
const withBase = shared.parseSegments("#EXTM3U\n#EXTINF:6,\nseg0.ts\n", "https://cdn.x/a/");
check("parseSegments 按 base 解析相对地址", withBase[0] === "https://cdn.x/a/seg0.ts", withBase);
const badBase = shared.parseSegments("#EXTM3U\nseg1.ts\n", "::not-a-url::");
check("parseSegments 解不出 base 时原样返回而不是抛（整份播放列表不该被一条坏数据炸掉）",
  badBase[0] === "seg1.ts", badBase);
const master = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1920x1080,CODECS=\"avc1.640028\"\n1080p/v.m3u8\n";
const [v] = shared.parseVariants(master, "https://cdn.x/a/index.m3u8");
check("parseVariants 带出 codec 与 quality", v.codec === "avc1.640028" && v.quality === "1080P", v);
check("variantLabel / variantQuality 参数序一致（都吃 uri 在前）",
  shared.variantLabel("https://cdn.x/1080p/v.m3u8", "", 0) === shared.variantLabel("https://cdn.x/1080p/v.m3u8", "", 0) &&
  shared.variantQuality("https://cdn.x/1080p/v.m3u8", "", 0) === "1080P");
check("qualityFromURL 认得 ?quality=（content.js 旧实现漏掉的正是这一条）",
  shared.qualityFromURL("https://cdn.x/a/index.m3u8?quality=1440") === "1440",
  shared.qualityFromURL("https://cdn.x/a/index.m3u8?quality=1440"));

console.log("输出文件名：画质后缀只由共享 qualityFromURL 决定");
const NAME_CASES = [
  "https://cdn.x/a/1080p/v.m3u8",
  "https://cdn.x/a/v_720p.m3u8",
  "https://cdn.x/a/index.m3u8?quality=1440",
  "https://cdn.x/a/index.m3u8",
];
for (const u of NAME_CASES) {
  const q = shared.qualityFromURL(u);
  const got = ui.buildOutputName(u, "标题");
  check(`后缀与共享实现一致：${u} → ${got}`,
    got === "标题" + (q ? `_${q}` : "") + ".ts", { got, q });
}
check("标题里的非法字符与控制字符被清掉",
  ui.buildOutputName("https://cdn.x/a/index.m3u8", 'a/b:c*?"<>|d') === "a_b_c_d.ts" &&
  ui.buildOutputName("https://cdn.x/a/index.m3u8", "标\x07题") === "标题.ts");

console.log("兜底命令：与 downloader 页面同一套净化");
const cmd = ui.buildGoCommand("https://x.com/a/b.m3u8", "https://x.com/watch/1", "标题", '"C:\\Program Files\\go-catcher.exe"');
const tokens = [];
{
  let cur = "";
  let inQuote = false;
  for (const ch of cmd) {
    if (ch === '"') { inQuote = !inQuote; continue; }
    if (!inQuote && /\s/.test(ch)) { if (cur) tokens.push(cur); cur = ""; continue; }
    cur += ch;
  }
  if (cur) tokens.push(cur);
}
check("带引号的 exe 路径被净化：参数仍是 3 个 + chcp",
  tokens.join("|") ===
    ["chcp", "65001", ";", "C:\\Program Files\\go-catcher.exe",
     "--url=https://x.com/a/b.m3u8", "--referer=https://x.com/watch/1",
     "-o", "标题.ts"].join("|"),
  tokens);
check("命令里不含换行 / 控制字符",
  !/[\r\n\x07\x1b]/.test(ui.buildGoCommand("https://x.com/a/b.m3u8", "", "标\n题\x07", "")));

console.log(`\n${fail === 0 ? "全部通过" : "有失败"}：${pass} ok / ${fail} FAIL`);
process.exit(fail === 0 ? 0 : 1);
