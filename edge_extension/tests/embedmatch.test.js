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

  // ------------------------------------------------------------
  // 同页多候选（2026-09-18 修）：首页/栏目页同一文档拉多条整文件 mp4
  //
  // 首页横幅（品牌/广告循环）与卡片预览都是同站、整文件、体积过 MB 的 mp4，
  // 且 frameUrl 都等于顶层文档 URL（它们由首页文档自己发起，不是 iframe）。
  // 悬停门控在「无活跃信号」时若仍按体积取最大，会稳定命中体积最大的横幅广告
  // —— 按钮亮起、点下去却下载到装饰资源。候选多条时必须有活跃信号（segmentDir）
  // 才敢挑；否则判失败，不出按钮（列表不受影响，仍可在扩展页自取）。
  // ------------------------------------------------------------
  console.log("同页多候选（首页横幅 + 卡片预览，frameUrl 都等于顶层文档）：");
  const HOME_DOC = "https://www.bilibili.example.com/";
  const BANNER_MP4 = "https://i0.hdslb.example.com/bfs/vc/banner-loop.mp4";
  const CARD_PREVIEW = "https://upos.example.com/preview/card1.mp4";
  store = {
    m3u8_list: [],
    mp4_list: [
      { url: BANNER_MP4, pageUrl: HOME_DOC, frameUrl: HOME_DOC, title: "横幅", type: "mp4", size: 5234500, time: 1 },
      { url: CARD_PREVIEW, pageUrl: HOME_DOC, frameUrl: HOME_DOC, title: "卡片", type: "mp4", size: 2100000, time: 2 },
    ],
  };
  api.resetListCache();
  const multiNoSignal = await api
    .getVideoSource({ src: "blob:https://www.bilibili.example.com/x1", pageUrl: HOME_DOC, segmentDir: "", title: "首页" })
    .then((r) => r, () => null);
  check("多条候选 + 无活跃信号 → 不出按钮（不再按体积猜最大）", multiNoSignal === null, multiNoSignal);

  const multiWithDir = await api
    .getVideoSource({ src: "blob:https://www.bilibili.example.com/x1", pageUrl: HOME_DOC, segmentDir: "https://upos.example.com/preview/", title: "首页" })
    .then((r) => r, () => null);
  check("多条候选 + 活跃目录命中卡片 → 返回卡片预览", multiWithDir && multiWithDir.url === CARD_PREVIEW, multiWithDir);

  const multiDirBanner = await api
    .getVideoSource({ src: "blob:https://www.bilibili.example.com/x1", pageUrl: HOME_DOC, segmentDir: "https://i0.hdslb.example.com/bfs/vc/", title: "首页" })
    .then((r) => r, () => null);
  check("多条候选 + 活跃目录命中横幅 → 返回横幅", multiDirBanner && multiDirBanner.url === BANNER_MP4, multiDirBanner);

  const multiDirMiss = await api
    .getVideoSource({ src: "blob:https://www.bilibili.example.com/x1", pageUrl: HOME_DOC, segmentDir: "https://cdn.example.com/other/", title: "首页" })
    .then((r) => r, () => null);
  check("多条候选 + 活跃目录不命中任何候选 → 判失败", multiDirMiss === null, multiDirMiss);

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

  // ------------------------------------------------------------
  // 权威 tab 级唯一候选回退（2026-09-18 方案1）：
  //   同源 iframe + MSE(blob) + 加密源（同源播放器脚本页 /static/player/...?url=<hex>）。
  //   sniff 的 frameUrl 常为空（details.documentUrl 拿不到），content.js 悬停
  //   在 iframe 里上报的 pageUrl 是 iframe 的 location.href（加密参数），与
  //   记录锚定的顶层页 URL（/video/38492-1-2.html）对不上，前面几档全落空。
  //   改用浏览器权威顶层页 URL（sender.tab.url）+ 唯一性闸门：只有整页恰好一条
  //   候选才敢给按钮；多条则继续判失败。回退只许顶层 frame（frameId === 0）消费，
  //   否则 all_frames 注入下顶层与 iframe 各一按钮、同屏两个。
  // ------------------------------------------------------------
  console.log("权威 tab URL 回退 + 唯一性闸门（同源 iframe + 加密源）：");
  const VIDEO_PAGE = "https://media.example.com/video/38492-1-2.html";
  const PLAYER_IFRAME = "https://media.example.com/static/player/player/?url=1a2b3c4d5e";
  const SECURE_M3U8 = "https://media.example.com/video_m3u8/secure.php?skey=abc123";
  const TOP_SENDER = { tab: { url: VIDEO_PAGE }, frameId: 0 };

  store = {
    m3u8_list: [
      { url: SECURE_M3U8, pageUrl: VIDEO_PAGE, frameUrl: "", title: "加密源视频", type: "m3u8", time: 1 },
    ],
    mp4_list: [],
  };
  api.resetListCache();
  const securedHit = await api
    .getVideoSource({ src: "", pageUrl: PLAYER_IFRAME, title: "加密源视频" }, TOP_SENDER)
    .then((r) => r, () => null);
  check("顶层 frame + 加密 iframe → 权威 tab URL 唯一候选命中", securedHit && securedHit.url === SECURE_M3U8, securedHit);

  const securedList = await api.getVideoSources({ src: "", pageUrl: PLAYER_IFRAME }, TOP_SENDER);
  check("点击下载列表同步含该候选（不再假按钮）", securedList.some((x) => x.url === SECURE_M3U8), securedList);

  // 子 frame（iframe 里的第二份 content.js）不应消费页级回退 → 不出按钮
  const securedChild = await api
    .getVideoSource({ src: "", pageUrl: PLAYER_IFRAME, title: "加密源视频" }, { tab: { url: VIDEO_PAGE }, frameId: 1 })
    .then((r) => r, () => null);
  check("子 frame 不消费页级回退 → 不出按钮（避免双按钮）", securedChild === null, securedChild);

  // 顶层页同时挂多条媒体（主视频 + 装饰 mp4）→ 唯一性闸门拒绝，不出按钮
  store = {
    m3u8_list: [
      { url: SECURE_M3U8, pageUrl: VIDEO_PAGE, frameUrl: "", title: "加密源视频", type: "m3u8", time: 1 },
    ],
    mp4_list: [
      { url: "https://media.example.com/static/banner.mp4", pageUrl: VIDEO_PAGE, frameUrl: "", title: "横幅", type: "mp4", size: 5300000, time: 1 },
    ],
  };
  api.resetListCache();
  const secMulti = await api
    .getVideoSource({ src: "", pageUrl: PLAYER_IFRAME, title: "加密源视频" }, TOP_SENDER)
    .then((r) => r, () => null);
  check("同一顶层页多条候选 → 唯一性闸门拒绝（不出按钮）", secMulti === null, secMulti);

  console.log("isCandidateURL：");
  check("解析页假链接被过滤", api.isCandidateURL(PARSE_IFRAME) === false);
  check("真 m3u8 保留", api.isCandidateURL(M3U8) === true);

  console.log(`\n${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
})();
