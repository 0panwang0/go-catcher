// 两个「输出文件名构造器」的对照锁（评审 P3-6）。
// 用法（在 edge_extension 目录下）：node tests/filename-consistency.test.js
//
// 为什么值得单独一个文件：浮层的 buildOutputName（content.js）与扩展页的
// makeFilename（downloader.js）**有意各自保留一份**（输入不同：浮层只能从 URL
// 猜画质，扩展页的画质来自用户手选的档位）。但"有意不同"不能靠嘴说——
// 各自的测试只看自己那一半（contentparse.test.js 看浮层、downloader.test.js 看
// 扩展页），**没有任何一处同时跑两个函数**，而"同一个视频从两个入口下载得到两个
// 文件名"恰好落在这一半与那一半之间：各测各的时候两边都是绿的（评审 P2-7 的
// 原始症状就是这么漏过去的）。
//
// 本文件把两个函数放进同一进程真跑，钉两层：
//   1. 有标题（正常路径）：必须**逐字节相同**——共享的 sanitizeFileName 与
//      qualityFromURL 稍有漂移，这里立刻红。
//   2. 无标题（兜底路径）：差异必须**恰好**是对照表里写明的那两条
//      （下载器页面把路径段连起来、浮层取倒数第一个非画质段），多一条少一条
//      都算漂移。
//
// 判据是"真跑函数"而不是"读源码文本"：两个文件都能在 node 里加载（各自的测试
// 已经解决过加载方式，本文件沿用那两套），所以不需要猜行为。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");

// ------------------------------------------------------------
// 加载 A：content.js —— 经典 content script，靠 globalThis.__m3u8Shared 取共享实现
// ------------------------------------------------------------
require(path.join(DIR, "content-shared.js"));
const shared = globalThis.__m3u8Shared;
if (!shared || typeof shared.qualityFromURL !== "function") {
  console.error("content-shared.js 没挂出 __m3u8Shared（先跑 node build.mjs）");
  process.exit(1);
}
const contentSrc = fs.readFileSync(path.join(DIR, "content.js"), "utf8");
const contentMod = { exports: {} };
new Function("window", "document", "chrome", "location", "history", "navigator", "module", contentSrc)(
  { __m3u8_catcher_injected__: false, addEventListener() {} },
  {
    readyState: "loading", // 让 init() 不跑：本文件只测纯函数
    addEventListener() {},
    querySelector: () => null,
    querySelectorAll: () => [],
    createElement: () => ({ style: {}, appendChild() {}, addEventListener() {}, setAttribute() {} }),
    body: { appendChild() {}, removeChild() {} },
    documentElement: { appendChild() {} },
  },
  {},
  { href: "https://example.com/" },
  {},
  { userAgent: "" },
  contentMod
);
const overlay = contentMod.exports.__ui;
if (!overlay || typeof overlay.buildOutputName !== "function") {
  console.error("content.js 没导出 __ui.buildOutputName");
  process.exit(1);
}

// ------------------------------------------------------------
// 加载 B：downloader.js —— ES 模块，去掉单行 import 并把被 import 的模块前置展开
//（与 tests/downloader.test.js 同一套；那边有更详细的注释）
// ------------------------------------------------------------
const IMPORT_RE = /^import \{[^}]*\} from "\.\.?\/[^"]+";$/;
const dlSrc = fs.readFileSync(path.join(DIR, "downloader.js"), "utf8");
const imported = dlSrc
  .split("\n")
  .filter((l) => IMPORT_RE.test(l))
  .map((l) => l.match(/from "([^"]+)";$/)[1]);
const sharedSrc = imported
  .map((m) => fs.readFileSync(path.join(DIR, m), "utf8").replace(/^export /gm, ""))
  .join("\n");
const fakeEl = () => ({
  style: {},
  className: "",
  innerHTML: "",
  textContent: "",
  value: "",
  appendChild() {},
  addEventListener() {},
  remove() {},
  click() {},
  scrollTop: 0,
  scrollHeight: 0,
});
const downloader = new Function(
  "chrome", "document", "location", "history",
  sharedSrc +
    "\n" +
    dlSrc.replace(new RegExp(IMPORT_RE.source, "gm"), "").replace(/^export /gm, "") +
    "\nreturn __test__;"
)(
  {
    storage: {
      local: { async get(d) { return d || {}; }, async set() {} },
      onChanged: { addListener() {} },
    },
    runtime: { sendMessage: async () => ({}) },
    tabs: { getCurrent: async () => null },
  },
  {
    addEventListener() {},
    querySelector: () => fakeEl(),
    createElement: () => fakeEl(),
    querySelectorAll: () => [],
    body: { appendChild() {}, removeChild() {} },
  },
  { search: "", pathname: "/downloader.html" },
  { replaceState() {} }
);
if (!downloader || typeof downloader.makeFilename !== "function") {
  console.error("downloader.js 没导出 __test__.makeFilename");
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

// 两个函数在"画质同源"时的等价调用：扩展页按 URL 推断画质（这也是它自己的
// buildGoCommand 走的路径），浮层只能这么推。
const dl = (url, title) => downloader.makeFilename(url, title, downloader.qualityFromURL(url));
const ov = (url, title) => overlay.buildOutputName(url, title);

// ------------------------------------------------------------
// 1) 有标题：逐字节相同（共享部分必须真的共享）
// ------------------------------------------------------------
console.log("有标题（正常路径）：两个构造器必须逐字节相同");
const SAME = [
  ["https://cdn.x/v/abc-123/1080p/v.m3u8", "标题", "标题_1080P.ts"],
  ["https://cdn.x/v/abc-123/v_720p.m3u8", "标题", "标题_720P.ts"],
  ["https://cdn.x/a/index.m3u8?quality=1440", "标题", "标题_1440.ts"],
  ["https://cdn.x/a/index.m3u8", "标题", "标题.ts"],
  ["https://cdn.x/a/index.m3u8", 'a/b:c*?"<>|d', "a_b_c_d.ts"],
  ["https://cdn.x/a/index.m3u8", "标\x07题\x1b", "标题.ts"],
  ["https://cdn.x/a/index.m3u8", "  a  b  ", "a b.ts"],
  ["https://cdn.x/a/index.m3u8", "x".repeat(200), "x".repeat(80) + ".ts"],
];
for (const [url, title, want] of SAME) {
  const a = ov(url, title);
  const b = dl(url, title);
  check(
    `一致：${JSON.stringify(title).slice(0, 24)} @ ${url} → ${want.length > 24 ? want.slice(0, 24) + "…" : want}`,
    a === want && b === want,
    { overlay: a, downloader: b, want }
  );
}

// ------------------------------------------------------------
// 2) 无标题：差异恰好是写明的那些（漂移就会被看见）
// ------------------------------------------------------------
// 下载器页面把路径段用 _ 连起来（"v_abc-123"），浮层取倒数第一个非画质段（"v"）——
// 这是两份实现唯一真正不同的地方，写在这里是**故意**的：谁想改其中一边，这里先红。
console.log("无标题（兜底路径）：差异必须恰好是写明的两条");
{
  const url = "https://x.com/v/abc-123/1080p/v.m3u8";
  check(
    "扩展页：路径段连起来（v_abc-123）",
    dl(url, "") === "v_abc-123_1080P.ts",
    dl(url, "")
  );
  check(
    "浮层：取倒数第一个非画质段（v）",
    ov(url, "") === "v_1080P.ts",
    ov(url, "")
  );
  // 两边都剔除了画质段：文件名里不许出现 "1080p_1080P" 这种自我重复
  check(
    "两侧都不把画质段当名字（无 abc-123_1080p_1080P 这类重复）",
    !/_1080p_/i.test(dl(url, "")) && !/_1080p_/i.test(ov(url, "")),
    { downloader: dl(url, ""), overlay: ov(url, "") }
  );
}
{
  const url = "https://x.com/a/b.m3u8"; // 目录 + 播放列表文件名
  check("扩展页：丢掉播放列表文件名那段（a）", dl(url, "") === "a.ts", dl(url, ""));
  check("浮层：丢 .m3u8 后缀后取最后那段（b）", ov(url, "") === "b.ts", ov(url, ""));
}
{
  const url = ""; // 标题、URL 都没有
  check("扩展页：兜底 video_<时间戳>.ts（服务端要求非空文件名）",
    /^video_\d+\.ts$/.test(dl(url, "")), dl(url, ""));
  check("浮层：返回空串（调用方据此整条 -o 都不传）", ov(url, "") === "", ov(url, ""));
}

// ------------------------------------------------------------
// 3) 扩展名：只有扩展页可传参（浮层固定 .ts）
// ------------------------------------------------------------
console.log("扩展名：扩展页可传参，浮层固定 .ts");
check("扩展页 ext=mp4 只换后缀",
  downloader.makeFilename("https://x.com/a/b.m3u8", "标题", "1080P", "mp4") === "标题_1080P.mp4",
  downloader.makeFilename("https://x.com/a/b.m3u8", "标题", "1080P", "mp4"));
check("浮层没有 ext 参数，恒为 .ts",
  ov("https://x.com/a/1080p/b.m3u8", "标题").endsWith(".ts"),
  ov("https://x.com/a/1080p/b.m3u8", "标题"));

console.log(`\n${fail === 0 ? "全部通过" : "有失败"}：${pass} ok / ${fail} FAIL`);
process.exit(fail === 0 ? 0 : 1);
