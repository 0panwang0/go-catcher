// 临时验证脚本：在 Node 里用 chrome 桩加载 background.js，校验 ?url= 锚点匹配。
// 用法（在 edge_extension 目录下）：
//   node tests/embedmatch.test.js
//
// 注意：文件名不能以 "_" 开头（浏览器保留），且不能放在会被 manifest 引用的位置。
const fs = require("fs");
const path = require("path");

let store = {};
const noop = { addListener() {} };
const chrome = {
  action: { onClicked: noop, setBadgeText() {}, setBadgeBackgroundColor() {} },
  webRequest: { onCompleted: noop },
  runtime: { onMessage: noop },
  tabs: { get() {}, create() {}, onRemoved: noop, query: async () => [] },
  declarativeNetRequest: {
    getDynamicRules: async () => [],
    updateDynamicRules: async () => {},
  },
  storage: {
    onChanged: noop,
    local: {
      async get(keys) {
        const out = {};
        for (const k of [].concat(keys)) out[k] = store[k];
        return out;
      },
      async set(o) {
        Object.assign(store, o);
      },
    },
  },
};

const src = fs.readFileSync(path.join(__dirname, "..", "background.js"), "utf8");
// background.js 是 esbuild 打包产物（IIFE + 全局名 __m3u8catcher），
// 测试面经 main.js 的 __test__ 导出。
const api = new Function("chrome", src + "\nreturn __m3u8catcher.__test__;")(chrome);

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

(async () => {
  const M3U8 = "https://cdn.example.com/play/e0RLJ1Vb/index.m3u8";
  // 第三方解析页 iframe：src 的 ?url= 参数编码了目标地址（域名一律用中性示例）
  const PARSE_IFRAME = "https://parser.example.com/play/?url=" + M3U8;
  const TOP = "https://site.example.com/play/id/290898";

  store = {
    m3u8_list: [
      // 真实场景：解析页跳到别的域播放，frameUrl 与 iframe.src 不同站
      { url: M3U8, pageUrl: TOP, frameUrl: "https://player.other-host.com/x/", title: "示例片", type: "m3u8", time: 1 },
    ],
    mp4_list: [],
  };
  // list-store 持内存权威副本：直接改了桩就必须丢弃副本，否则读到上一个场景
  api.resetListCache();

  console.log("findSniffedByURL：");
  const hit = await api.findSniffedByURL(M3U8);
  check("完全相同 → 命中", hit && hit.url === M3U8, hit);
  const hitQ = await api.findSniffedByURL(M3U8 + "?sign=abc&t=1");
  check("带签名参数 → 忽略 query 命中", hitQ && hitQ.url === M3U8, hitQ);
  const miss = await api.findSniffedByURL("https://cdn.example.com/play/OTHER/index.m3u8");
  check("别的视频 → 不命中", miss === null, miss);

  store.m3u8_list = [
    { url: "https://cdn.example.com/play/e0RLJ1Vb/1080p/index.m3u8", pageUrl: TOP, frameUrl: "https://player.other-host.com/x/", type: "m3u8" },
  ];
  api.resetListCache();
  const dirHit = await api.findSniffedByURL(M3U8);
  check("同目录变体 → 命中", dirHit && dirHit.url.includes("1080p"), dirHit);

  console.log("getVideoSource（embedUrl 锚点，同站匹配必然落空的场景）：");
  store.m3u8_list = [
    { url: M3U8, pageUrl: TOP, frameUrl: "https://player.other-host.com/x/", title: "示例片", type: "m3u8" },
  ];
  api.resetListCache();
  const s = await api.getVideoSource({ src: "", pageUrl: PARSE_IFRAME, embedUrl: M3U8, title: "示例片" });
  check("embedUrl 命中并返回 ts 源", s && s.type === "ts" && s.url === M3U8, s);
  const sNoAnchor = await api
    .getVideoSource({ src: "", pageUrl: PARSE_IFRAME, title: "示例片" })
    .then((r) => r, (e) => null);
  check("不带 embedUrl（旧逻辑）→ 无候选（别页记录不再放宽）", !sNoAnchor, sNoAnchor);

  console.log("getVideoSources（列表面板）：");
  const list = await api.getVideoSources({ src: "", pageUrl: PARSE_IFRAME, embedUrl: M3U8 });
  check("列表含锚点源", Array.isArray(list) && list.some((x) => x.url === M3U8), list);

  // ------------------------------------------------------------
  // 跨页面污染（2026-09-17 修）：同站 ≠ 同视频
  //
  // 站点首页/栏目页会加载整文件 mp4（品牌动画、卡片预览之类），它们在嗅探侧
  // 与主视频无法区分（同站、video/mp4、非 206 分段、体积过 MB），只有「归属哪个
  // 页面」能区分。候选必须来自同一个 frame 文档或同 host+path 的页面。
  // ------------------------------------------------------------
  console.log("跨页面污染（同站其它页面的记录）：");
  const HOME = "https://www.video.example.com/";
  const WATCH = "https://www.video.example.com/watch/12345";
  const CARD_MP4 = "https://static.example.com/media/brand-loop.mp4";
  const blobSrc = "blob:https://www.video.example.com/0f1e2d3c";

  store = {
    m3u8_list: [],
    mp4_list: [
      // 首页加载的整文件 mp4：会被嗅探录进列表，但它属于首页，不是这个视频页的
      { url: CARD_MP4, pageUrl: HOME, frameUrl: "", title: "首页", type: "mp4", size: 1678189, time: 1 },
    ],
  };
  api.resetListCache();
  const polluted = await api
    .getVideoSource({ src: blobSrc, pageUrl: WATCH, segmentDir: "", title: "某视频" })
    .then((r) => r, () => null);
  check("别页记录（blob 主视频）→ 按钮判定失败，不误报", polluted === null, polluted);
  const pollutedList = await api.getVideoSources({ src: blobSrc, pageUrl: WATCH });
  check("别页记录 → 候选列表不含它", !pollutedList.some((x) => x.url === CARD_MP4), pollutedList);

  console.log("同页记录仍然认（别把按钮一关了之）：");
  store.mp4_list = [
    { url: CARD_MP4, pageUrl: WATCH, frameUrl: "", title: "某视频", type: "mp4", size: 1678189, time: 1 },
  ];
  api.resetListCache();
  const samePage = await api
    .getVideoSource({ src: blobSrc, pageUrl: WATCH, segmentDir: "", title: "某视频" })
    .then((r) => r, () => null);
  check("同页记录 + 无活跃信号 → 认（回退体积启发式）", samePage && samePage.url === CARD_MP4, samePage);

  const sameDir = await api
    .getVideoSource({ src: blobSrc, pageUrl: WATCH, segmentDir: "https://static.example.com/media/", title: "某视频" })
    .then((r) => r, () => null);
  check("同页记录 + 活跃目录命中 → 认", sameDir && sameDir.url === CARD_MP4, sameDir);

  const otherStream = await api
    .getVideoSource({ src: blobSrc, pageUrl: WATCH, segmentDir: "https://cdn.example.com/other/", title: "某视频" })
    .then((r) => r, () => null);
  check("同页记录 + 页面正在拉别的流 → 判失败", otherStream === null, otherStream);

  store.mp4_list = [
    { url: CARD_MP4, pageUrl: WATCH + "/", frameUrl: "", title: "某视频", type: "mp4", size: 1678189, time: 1 },
  ];
  api.resetListCache();
  const slashHit = await api
    .getVideoSource({ src: blobSrc, pageUrl: WATCH, segmentDir: "", title: "某视频" })
    .then((r) => r, () => null);
  check("同页（路径末尾斜杠差异）→ 认", slashHit && slashHit.url === CARD_MP4, slashHit);

  console.log("frame 文档完全相等仍然认（iframe 播放器）：");
  const FRAME_URL = "https://player.example.com/embed/1";
  store = {
    m3u8_list: [
      { url: M3U8, pageUrl: "https://site.example.com/watch/999", frameUrl: FRAME_URL, title: "示例片", type: "m3u8", time: 1 },
    ],
    mp4_list: [],
  };
  api.resetListCache();
  const frameHit = await api
    .getVideoSource({ src: "", pageUrl: FRAME_URL, title: "示例片" })
    .then((r) => r, () => null);
  check("frameUrl 相等 → 认", frameHit && frameHit.url === M3U8, frameHit);

  // ------------------------------------------------------------
  // iframe URL 带一次性查询参数（时间戳 / 播放令牌）：列表宽、按钮严（P1-6）
  //
  // 页面世界拿到的 iframe URL 常带一次性参数，与嗅探记录里存的 frameUrl 不会
  // 逐字符相等，但 hostname 稳定。两套语义必须分开：按钮宁缺勿假（证据不足就
  // 不出按钮），列表宁多勿漏（用户在列表里看得到标题与体积，自己判断）。
  // 收窄 button 口径时把列表一起收窄，症状就是"明明嗅到了却列不出来"。
  // ------------------------------------------------------------
  console.log("iframe URL 带一次性查询参数（列表宽 / 按钮严）：");
  const FRAME_BASE = "https://player.example.com/embed/1";
  store = {
    m3u8_list: [
      { url: M3U8, pageUrl: "https://site.example.com/watch/999", frameUrl: FRAME_BASE, title: "示例片", type: "m3u8", time: 1 },
    ],
    mp4_list: [],
  };
  api.resetListCache();
  const withToken = FRAME_BASE + "?t=1712345678";
  const listWithToken = await api.getVideoSources({ src: "", pageUrl: withToken });
  check("列表：frame 同 host（查询参数不同）仍然列出", listWithToken.some((x) => x.url === M3U8), listWithToken);
  const btnWithToken = await api
    .getVideoSource({ src: "", pageUrl: withToken, title: "示例片" })
    .then((r) => r, () => null);
  check("按钮：同一场景仍然不出现（证据不足不给承诺）", btnWithToken === null, btnWithToken);

  console.log("isCandidateURL：");
  check("解析页假链接被过滤", api.isCandidateURL(PARSE_IFRAME) === false);
  check("真 m3u8 保留", api.isCandidateURL(M3U8) === true);

  console.log(`\n${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
})();
