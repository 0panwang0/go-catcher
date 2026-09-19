// downloader.js（工具栏下载器页面）的回归测试。
// 用法（在 edge_extension 目录下）：node tests/downloader.test.js
//
// 这个页面曾经有**第二套下载实现**：分片 fetch 回来 Blob 原样拼接，不经过 Go 服务。
// 它对两类流必然产出打不开的文件（加密流没有解密能力、fMP4 缺少 #EXT-X-MAP 声明的
// init 段），而日志会打印"已保存"——最隐蔽的一类损坏。现在下载全部委托给本地服务，
// 本文件主要守两件事：
//
//   1. **那套实现不能再回来**（结构性断言：不允许出现 fetchSegment / Blob 拼接 /
//      自带解析函数）；解析只有一份源码，本页必须 import 共享模块。
//   2. 本页剩下的纯函数（文件名、候选过滤、兜底命令）行为正确。
//
// 加载方式：downloader.js 现在是 ES 模块（要 import m3u8-parse.js）。node 里既不
// 方便为一个页面脚本改 package.json 的 type，也无法复现扩展侧的模块解析，所以做一次
// 极小的展开——去掉单行 import，把 m3u8-parse.js 去掉 export 后前置，再交给
// new Function。顶层只有 DOMContentLoaded 注册（桩里空实现），加载过程无副作用。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");
const IMPORT_RE = /^import \{[^}]*\} from "\.\.?\/[^"]+";$/;
const PARSE_MODULE = "./src/background/m3u8-parse.js";
const CLI_MODULE = "./src/background/cli-args.js";
const MEDIA_URL_MODULE = "./src/background/media-url.js";
const HTML_ESCAPE_MODULE = "./src/background/html-escape.js";
const PAGE = "downloader.js";

const src = fs.readFileSync(path.join(DIR, PAGE), "utf8");
const importLines = src.split("\n").filter((l) => IMPORT_RE.test(l));
const imported = importLines.map((l) => l.match(/from "([^"]+)";$/)[1]);
if (!imported.includes(PARSE_MODULE)) {
  console.error("downloader.js 必须 import 共享解析模块 " + PARSE_MODULE);
  console.error("实际匹配到:", importLines);
  process.exit(1);
}
for (const m of imported) {
  if (!m.startsWith("./src/background/")) {
    console.error("downloader.js 只允许 import src/background/ 下的共享模块，实际：" + m);
    process.exit(1);
  }
}
// 把被 import 的共享模块逐个前置展开（去掉 export 前缀——new Function 里
// 不需要模块语法）。展开顺序与 import 行一致，模块之间无同名符号。
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
const document = {
  addEventListener() {},
  querySelector: () => fakeEl(),
  createElement: () => fakeEl(),
  querySelectorAll: () => [],
  body: { appendChild() {}, removeChild() {} },
};
const chrome = {
  storage: {
    local: {
      async get(defaults) { return defaults || {}; },
      async set() {},
    },
    onChanged: { addListener() {} },
  },
  runtime: { sendMessage: async () => ({}) },
  tabs: { getCurrent: async () => null },
};

const api = new Function(
  "chrome", "document", "location", "history",
  sharedSrc +
    "\n" +
    src.replace(new RegExp(IMPORT_RE.source, "gm"), "").replace(/^export /gm, "") +
    "\nreturn __test__;"
)(chrome, document, { search: "", pathname: "/downloader.html" }, { replaceState() {} });

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

console.log("结构性守卫：不存在第二套下载实现");
check(
  "无分片下载函数（fetchSegment / saveBlob / downloadMP4）",
  !/\bfetchSegment\b|\bsaveBlob\b|\bdownloadMP4\b/.test(src)
);
check("无 Blob 拼接（new Blob / createObjectURL）", !/new Blob\(|createObjectURL/.test(src));
check("无自带 parseSegments/parseDuration 定义", !/^function parseSegments|^function parseDuration/m.test(src));
check("无自带的容器识别/加密守卫（已由服务端承担）",
  !/detectSegmentContainer|browserDownloadRejectReason|playlistKeyMethod/.test(src));
check("走服务端：下载入口是 downloadViaServer 消息", /type: "downloadViaServer"/.test(src));
check("解析只有一份源码：import 共享解析模块",
  importLines.some((l) => l.includes(PARSE_MODULE) && l.includes("parseSegments") && l.includes("parseVariants")));
check("文本净化 / 命令拼装只有一份源码：import 共享 cli-args 模块",
  importLines.some((l) => l.includes(CLI_MODULE) && l.includes("sanitizeFileName") && l.includes("assembleGoCommand")));

// ------------------------------------------------------------
// P3-6：四份"两边都有、靠注释保持一致"的拷贝已收进共享模块。
// 症状分别是：候选判定漂移（浮层能下的链接扩展页下不了）、转义漂移（用户可控
// 文本原样进 innerHTML）、时长显示漂移。判据是"本文件不许再定义它们"——
// 定义一回来，就又是一份会各自漂移的拷贝。
// ------------------------------------------------------------
console.log("结构性守卫：共享实现不再各留一份拷贝（P3-6）");
for (const fn of ["isCandidateURL", "isPlaylistURL", "escapeHtml", "fmtDur"]) {
  check(`downloader.js 不再定义 ${fn}（改用共享模块）`,
    !new RegExp(`function\\s+${fn}\\s*\\(`).test(src));
}
check("import 共享 media-url 模块（候选判定与播放列表判定）",
  importLines.some((l) => l.includes(MEDIA_URL_MODULE) && l.includes("isCandidateURL") && l.includes("isPlaylistURL")));
check("import 共享 html-escape 模块",
  importLines.some((l) => l.includes(HTML_ESCAPE_MODULE) && l.includes("escapeHtml")));

// buildGoCommand 收缩成"薄适配层"：只决定输出文件名怎么算，拼装交给共享实现。
const bgcBody = (src.match(/function buildGoCommand[\s\S]*?\n\}/) || [""])[0];
check("buildGoCommand 委托给共享 assembleGoCommand", /assembleGoCommand\(/.test(bgcBody), bgcBody);
check("buildGoCommand 里不再自己拼 chcp / 自己净化引号",
  !/chcp/.test(bgcBody) && !/quoteArg/.test(bgcBody), bgcBody);

console.log("import 接线（共享模块的函数确实可用）");
const PLAIN = "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg0.ts\n#EXTINF:6.0,\nseg1.ts\n#EXT-X-ENDLIST\n";
check("parseDuration 累加 EXTINF", api.parseDuration(PLAIN) === 12, api.parseDuration(PLAIN));
const FMP4 = "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\nseg0.m4s\n#EXT-X-ENDLIST\n";
const segs = api.parseSegments(FMP4, "https://cdn.example.com/v/");
check("parseSegments 按 base 解析且跳过 #EXT-X-MAP 行",
  segs.length === 1 && segs[0] === "https://cdn.example.com/v/seg0.m4s", segs);
const MASTER = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720\n720p/v.m3u8\n" +
  "#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1920x1080\n1080p/v.m3u8\n";
const variants = api.parseVariants(MASTER, "https://cdn.example.com/a/index.m3u8");
check("parseVariants 按码率降序", variants.length === 2 && variants[0].bandwidth === 3000000, variants);
check("shortQuality 取标签首段", api.shortQuality(variants[0]) === "1080P", api.shortQuality(variants[0]));
check("qualityFromURL 从路径取画质", api.qualityFromURL("https://x.com/a/1080p/v.m3u8") === "1080P");
check("fmtDur 秒 → 时长（转出的是共享实现，不是本文件的拷贝）",
  api.fmtDur(187) === "3:07" && api.fmtDur(3723) === "1:02:03", [api.fmtDur(187), api.fmtDur(3723)]);
check("escapeHtml 五个有语义的字符都转（含单引号：属性值可能用单引号包裹）",
  api.escapeHtml("&<>\"'") === "&amp;&lt;&gt;&quot;&#39;", api.escapeHtml("&<>\"'"));

console.log("isPlaylistURL：区分播放列表与 MP4 直链");
check(".m3u8 → true", api.isPlaylistURL("https://x.com/a/b.m3u8") === true);
check("带查询串的 .m3u8 → true", api.isPlaylistURL("https://x.com/a/b.m3u8?t=1") === true);
check(".mp4 → false（不去 fetch 整个视频）", api.isPlaylistURL("https://x.com/a/b.mp4") === false);
check("非法 URL → false（不抛）", api.isPlaylistURL("not a url") === false);

console.log("isCandidateURL：过滤解析页假链接");
check("正常媒体直链 → true", api.isCandidateURL("https://x.com/a/b.m3u8") === true);
check("?url= 尾部分片 → false", api.isCandidateURL("https://x.com/play/?url=https://y.com/a.m3u8") === false);
check("无扩展名但无 ?url= → true（不误杀）", api.isCandidateURL("https://x.com/live/stream") === true);
check("非法 URL → false（不抛）", api.isCandidateURL("not a url") === false);

console.log("makeFilename：输出名");
check("标题 + 画质", api.makeFilename("https://x.com/a/b.m3u8", "标题", "1080P") === "标题_1080P.ts");
check("非法字符替换为下划线并归并连续下划线",
  api.makeFilename("https://x.com/a/b.m3u8", 'a/b:c*?"<>|d', "") === "a_b_c_d.ts",
  api.makeFilename("https://x.com/a/b.m3u8", 'a/b:c*?"<>|d', ""));
check("控制字符不进文件名（\\x07 \\x1b 不在 \\s 覆盖范围内）",
  api.makeFilename("https://x.com/a/b.m3u8", "标\x07题\x1b", "") === "标题.ts",
  JSON.stringify(api.makeFilename("https://x.com/a/b.m3u8", "标\x07题\x1b", "")));
check("无标题 → 退回 URL 路径段（丢掉 .m3u8 与画质段）",
  api.makeFilename("https://x.com/v/abc-123/1080p/v.m3u8", "", "") === "v_abc-123.ts",
  api.makeFilename("https://x.com/v/abc-123/1080p/v.m3u8", "", ""));
check("URL 也不可用 → video_<ts> 兜底",
  /^video_\d+\.ts$/.test(api.makeFilename("", "", "")), api.makeFilename("", "", ""));

console.log("buildGoCommand：终端兜底命令");
const cmd = api.buildGoCommand("https://x.com/a/b.m3u8", "https://x.com/watch/1", "标题", "C:\\Tools\\go-catcher.exe");
check("含 chcp 65001（UTF-8 输出）", cmd.startsWith("chcp 65001 ;"));
check("含 --url / --referer / -o 且路径加引号",
  cmd.includes('"C:\\Tools\\go-catcher.exe"') &&
  cmd.includes('--url="https://x.com/a/b.m3u8"') &&
  cmd.includes('--referer="https://x.com/watch/1"') &&
  cmd.includes('-o "标题.ts"'), cmd);
check("用 ; 分隔（PowerShell 5.1 不认 &&）", !cmd.includes("&&"));
const noRef = api.buildGoCommand("https://x.com/a/b.m3u8", "", "标题", "");
check("默认 exe 名 + 无来源页时不带 --referer",
  noRef.includes('"go-catcher.exe"') && !noRef.includes("--referer"), noRef);

// P1-2：用户从 master 手选了档位后，兜底命令必须下那一档。画质由调用方传入 ——
// master 的 URL 里没有档位标记，qualityFromURL 推不出来；旧实现丢了这个入参，
// 于是用户以为在下 720p、实际拿到的是最高码率、文件名也没有 _720P 后缀。
const picked = api.buildGoCommand("https://x.com/a/master.m3u8", "", "标题", "", "720P");
check("用户手选的画质进输出名（master URL 推不出画质也要带）",
  picked.includes('-o "标题_720P.ts"'), picked);
// 有区分力：同一个 master URL 不传画质就只有无后缀的名字 —— 上一条真的靠传入值
check("（该断言有区分力：同一 URL 不传画质则无后缀）",
  api.buildGoCommand("https://x.com/a/master.m3u8", "", "标题", "").includes('-o "标题.ts"'));
check("--url 指向被选中的档位而不是原始 master",
  api.buildGoCommand("https://x.com/a/720p/index.m3u8", "", "标题", "", "720P")
    .includes('--url="https://x.com/a/720p/index.m3u8"'));

// P1-2 的调用点：光有"buildGoCommand 支持 quality"不够 —— 缺陷发生在 startDownload
// 的 catch 里用了原始 m3u8URL。这条钉住"选中后的档位真的被传下去"。
const catchBlock = (src.match(/} catch \(e\) \{[\s\S]*?\n {2}\}/) || [""])[0];
check("兜底调用点在 catch 块里（提取得到，否则下面两条形同虚设）",
  catchBlock.includes("offerFallback"), catchBlock);
check("兜底命令用选中后的 playlistURL（不是原始 m3u8URL）",
  /offerFallback\(\s*playlistURL\s*,/.test(catchBlock) && !/offerFallback\(\s*m3u8URL\s*,/.test(catchBlock),
  catchBlock);
check("兜底命令带上用户选的 quality（否则档位在后缀上又丢一次）",
  /offerFallback\(\s*playlistURL\s*,\s*pageURL\s*,\s*videoName\s*,\s*quality\s*\)/.test(catchBlock),
  catchBlock);

// ------------------------------------------------------------
// F9 / P2-8：参数值净化
// ------------------------------------------------------------
// 判据不用「逐字比对命令」（重构一下就全红），用「分词后的结构不变」：
// 真实的坏形态是值里的引号把参数切碎、或控制字符把一条命令截成两条。
// tokenize 粗略还原 shell 分词：连续空白分隔，双引号内不切。
function tokenize(cmd) {
  const out = [];
  let cur = "";
  let inQuote = false;
  for (const ch of cmd) {
    if (ch === '"') {
      inQuote = !inQuote;
      continue;
    }
    if (!inQuote && /\s/.test(ch)) {
      if (cur) out.push(cur);
      cur = "";
      continue;
    }
    cur += ch;
  }
  if (cur) out.push(cur);
  return out;
}

console.log("buildGoCommand：参数值净化（F9 / P2-8）");
// 用户从资源管理器「复制文件地址」拿到的就是带双引号的路径。
// 旧实现直接插值 → ""C:\Program Files\go-catcher.exe""，空引号对被解析成空串，
// 真路径裸奔后被空白切成「C:\Program」「Files\go-catcher.exe」两个参数。
const quotedExe = api.buildGoCommand(
  "https://x.com/a/b.m3u8",
  "https://x.com/watch/1",
  "标题",
  '"C:\\Program Files\\go-catcher.exe"'
);
check(
  "带引号的 exe 路径被净化：命令结构仍是 chcp + exe + 3 个参数",
  tokenize(quotedExe).join("|") ===
    [
      "chcp",
      "65001",
      ";",
      "C:\\Program Files\\go-catcher.exe",
      "--url=https://x.com/a/b.m3u8",
      "--referer=https://x.com/watch/1",
      "-o",
      "标题.ts",
    ].join("|"),
  tokenize(quotedExe)
);
check("引号总数为偶数（配对不错位）", (quotedExe.match(/"/g) || []).length % 2 === 0, quotedExe);
check("exe 路径里的换行被剔除（否则一条命令被截成两条）",
  !/[\r\n]/.test(api.buildGoCommand("https://x.com/a/b.m3u8", "", "标题", "C:\\a\nb.exe")));
check("URL 里的引号被剔除（浏览器真实形态是 %22，这里是纵深防御）",
  api.buildGoCommand('https://x.com/a/"b".m3u8', "", "标题", "").includes(
    '--url="https://x.com/a/b.m3u8"'
  ));
check("标题里的换行 / 控制字符不进命令",
  (() => {
    const c = api.buildGoCommand("https://x.com/a/b.m3u8", "", "标\n题\x07", "");
    return !/[\n\x07]/.test(c) && c.includes('-o "标题.ts"');
  })());

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
