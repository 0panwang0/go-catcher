// 临时验证脚本：在 Node 里用 chrome 桩加载 background.js，校验 ?url= 锚点匹配。
// 用法（在 edge_extension 目录下）：
//   node tests/embedmatch.test.js
//
// 注意：文件名不能以 "_" 开头（Chrome 保留），且不能放在会被 manifest 引用的位置。
const fs = require("fs");
const path = require("path");

let store = {};
const noop = { addListener() {} };
const chrome = {
  action: { onClicked: noop, setBadgeText() {}, setBadgeBackgroundColor() {} },
  webRequest: { onCompleted: noop },
  runtime: { onMessage: noop },
  tabs: { get() {}, create() {} },
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
  const M3U8 = "https://vv.jisuzyv.com/play/e0RLJ1Vb/index.m3u8";
  const PARSE_IFRAME = "https://jisuzyjiexi.com/play/?url=" + M3U8;
  const TOP = "https://www.xmfyy.com/index.php/vod/play/id/290898/sid/1/nid/1.html";

  store = {
    m3u8_list: [
      // 真实场景：解析页跳到别的域播放，frameUrl 与 iframe.src 不同站
      { url: M3U8, pageUrl: TOP, frameUrl: "https://player.other-host.com/x/", title: "异种污染", type: "m3u8", time: 1 },
    ],
    mp4_list: [],
  };

  console.log("findSniffedByURL：");
  const hit = await api.findSniffedByURL(M3U8);
  check("完全相同 → 命中", hit && hit.url === M3U8, hit);
  const hitQ = await api.findSniffedByURL(M3U8 + "?sign=abc&t=1");
  check("带签名参数 → 忽略 query 命中", hitQ && hitQ.url === M3U8, hitQ);
  const miss = await api.findSniffedByURL("https://vv.jisuzyv.com/play/OTHER/index.m3u8");
  check("别的视频 → 不命中", miss === null, miss);

  store.m3u8_list = [
    { url: "https://vv.jisuzyv.com/play/e0RLJ1Vb/1080p/index.m3u8", pageUrl: TOP, frameUrl: "https://player.other-host.com/x/", type: "m3u8" },
  ];
  const dirHit = await api.findSniffedByURL(M3U8);
  check("同目录变体 → 命中", dirHit && dirHit.url.includes("1080p"), dirHit);

  console.log("getVideoSource（embedUrl 锚点，同站匹配必然落空的场景）：");
  store.m3u8_list = [
    { url: M3U8, pageUrl: TOP, frameUrl: "https://player.other-host.com/x/", title: "异种污染", type: "m3u8" },
  ];
  const s = await api.getVideoSource({ src: "", pageUrl: PARSE_IFRAME, embedUrl: M3U8, title: "异种污染" });
  check("embedUrl 命中并返回 ts 源", s && s.type === "ts" && s.url === M3U8, s);
  const sNoAnchor = await api
    .getVideoSource({ src: "", pageUrl: PARSE_IFRAME, title: "异种污染" })
    .then((r) => r, (e) => null);
  check("不带 embedUrl（旧逻辑）→ 无候选（复现按钮消失根因）", !sNoAnchor, sNoAnchor);

  console.log("getVideoSources（列表面板）：");
  const list = await api.getVideoSources({ src: "", pageUrl: PARSE_IFRAME, embedUrl: M3U8 });
  check("列表含锚点源", Array.isArray(list) && list.some((x) => x.url === M3U8), list);

  console.log("isCandidateURL：");
  check("解析页假链接被过滤", api.isCandidateURL(PARSE_IFRAME) === false);
  check("真 m3u8 保留", api.isCandidateURL(M3U8) === true);

  console.log(`\n${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
})();
