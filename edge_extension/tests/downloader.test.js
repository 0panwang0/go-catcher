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
const PAGE = "downloader.js";

const src = fs.readFileSync(path.join(DIR, PAGE), "utf8");
const importLines = src.split("\n").filter((l) => IMPORT_RE.test(l));
if (importLines.length !== 1 || !importLines[0].includes(PARSE_MODULE)) {
  console.error("downloader.js 必须且只能有一条单行 import，指向 " + PARSE_MODULE);
  console.error("实际匹配到:", importLines);
  process.exit(1);
}
const parseSrc = fs
  .readFileSync(path.join(DIR, PARSE_MODULE), "utf8")
  .replace(/^export /gm, "");

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
  parseSrc +
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
check("解析只有一份源码：import 共享模块",
  importLines[0].includes("parseSegments") && importLines[0].includes("parseVariants"));

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
check("非法字符逐个替换为下划线",
  api.makeFilename("https://x.com/a/b.m3u8", 'a/b:c*?"<>|d', "") === "a_b_c______d.ts",
  api.makeFilename("https://x.com/a/b.m3u8", 'a/b:c*?"<>|d', ""));
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

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
