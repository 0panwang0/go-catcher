// 回归测试：嗅探记录的并发写入与淘汰策略（R5）。
// 用法（在 edge_extension 目录下）：
//   node tests/storage.test.js
//
// 关键：chrome.storage 的桩必须做「结构化克隆」语义（get 返回快照、set 存快照），
// 否则读写共享同一数组引用会掩盖竞态。get 延迟到宏任务返回，保证 N 个并发调用
// 的读阶段全部先于写阶段完成 —— 这正是真实场景（webRequest 每请求一个回调）。
const fs = require("fs");
const path = require("path");

let store = {};
const deepClone = (v) => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));
const noop = { addListener() {} };

const chrome = {
  action: { onClicked: noop, setBadgeText() {}, setBadgeBackgroundColor() {} },
  webRequest: { onCompleted: noop },
  runtime: { onMessage: noop },
  tabs: { get() {}, create() {} },
  storage: {
    onChanged: noop,
    local: {
      get(keys) {
        return new Promise((resolve) => {
          setTimeout(() => {
            const out = {};
            for (const k of [].concat(keys)) out[k] = deepClone(store[k]);
            resolve(out);
          }, 0);
        });
      },
      set(o) {
        return new Promise((resolve) => {
          setTimeout(() => {
            for (const [k, v] of Object.entries(o)) store[k] = deepClone(v);
            resolve();
          }, 0);
        });
      },
    },
  },
};

const src = fs.readFileSync(path.join(__dirname, "..", "background.js"), "utf8");
// background.js 是 esbuild 打包产物（IIFE + 全局名 __m3u8catcher），
// 测试面（recordMedia 等）经 main.js 的 __test__ 导出。
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

const TOP = "https://www.xmfyy.com/index.php/vod/play/id/290898/sid/1/nid/1.html";
// i 越大越新；基准取「刚刚」，避免被 TTL 当成过期记录清掉。
const NOW = Date.now();
const rec = (i, extra = {}) => ({
  url: `https://vv.jisuzyv.com/play/e0RLJ1Vb/v${i}/index.m3u8`,
  pageUrl: TOP,
  frameUrl: "",
  title: "异种污染",
  type: "m3u8",
  time: NOW - (30 - i) * 1000,
  tabId: 1,
  ...extra,
});

(async () => {
  console.log("并发写入不丢记录（旧实现会丢 N-1 条）：");
  store = { m3u8_list: [], mp4_list: [] };
  const N = 20;
  await Promise.all(
    Array.from({ length: N }, (_, i) =>
      api.recordMedia(rec(i).url, TOP, "", "异种污染", "m3u8", 0, 1)
    )
  );
  check(`20 个并发 recordMedia 全部落库`, store.m3u8_list.length === N, {
    got: store.m3u8_list.length,
  });
  check(
    "无重复 URL",
    new Set(store.m3u8_list.map((it) => it.url)).size === N
  );

  console.log("并发写入 + 同 URL 更新不产生重复条目：");
  store = { m3u8_list: [], mp4_list: [] };
  await Promise.all([
    api.recordMedia(rec(0).url, TOP, "", "标题A", "m3u8", 0, 1),
    api.recordMedia(rec(0).url, TOP, "https://frame.example/x/", "标题B", "m3u8", 0, 1),
    api.recordMedia(rec(0).url, TOP, "", "标题C", "m3u8", 0, 1),
  ]);
  check("同 URL 只保留一条", store.m3u8_list.length === 1, store.m3u8_list.length);
  check(
    "frameUrl 被合并保留",
    store.m3u8_list[0].frameUrl === "https://frame.example/x/",
    store.m3u8_list[0]
  );

  console.log("淘汰策略：优先淘汰已分析记录：");
  store = { m3u8_list: [], mp4_list: [] };
  for (let i = 0; i < 30; i++) store.m3u8_list.push(rec(i, { analyzed: i === 5 }));
  await api.recordMedia(rec(99).url, TOP, "", "新片", "m3u8", 0, 1);
  check("长度维持上限 30", store.m3u8_list.length === 30, store.m3u8_list.length);
  check(
    "analyzed 记录被淘汰",
    !store.m3u8_list.some((it) => it.url === rec(5).url)
  );
  check(
    "新记录已入列",
    store.m3u8_list.some((it) => it.url === rec(99).url)
  );

  console.log("淘汰策略：优先淘汰非当前标签页记录（即使它最新）：");
  store = { m3u8_list: [], mp4_list: [] };
  store.m3u8_list.push(rec(0, { tabId: 9, time: NOW + 60_000 }));
  for (let i = 1; i < 30; i++) store.m3u8_list.push(rec(i));
  await api.recordMedia(rec(99).url, TOP, "", "新片", "m3u8", 0, 1);
  check(
    "非当前 tab 的最新记录被淘汰",
    !store.m3u8_list.some((it) => it.url === rec(0).url)
  );
  check("当前 tab 的旧记录保留", store.m3u8_list.some((it) => it.url === rec(1).url));
  check("长度维持上限 30", store.m3u8_list.length === 30, store.m3u8_list.length);

  console.log("TTL：过期记录在下次写入时清理：");
  store = { m3u8_list: [], mp4_list: [] };
  store.m3u8_list.push(rec(0, { time: Date.now() - 8 * 24 * 3600 * 1000 }));
  await api.recordMedia(rec(1).url, TOP, "", "新片", "m3u8", 0, 1);
  check("8 天前的记录被清理", !store.m3u8_list.some((it) => it.url === rec(0).url));
  check("当天记录保留", store.m3u8_list.some((it) => it.url === rec(1).url));

  console.log("updateMediaItem：页面侧回写走同一把锁：");
  store = { m3u8_list: [rec(0)], mp4_list: [] };
  await api.updateMediaItem("m3u8_list", rec(0).url, { analyzed: true, quality: "1080p" });
  check(
    "补丁已合并",
    store.m3u8_list[0].analyzed === true && store.m3u8_list[0].quality === "1080p",
    store.m3u8_list[0]
  );
  const missRes = await api.updateMediaItem("m3u8_list", "https://nope/x.m3u8", { a: 1 });
  check("不存在的 URL 返回 ok:false", missRes && missRes.ok === false, missRes);

  console.log("updateMediaItem 与 recordMedia 并发不互相覆盖：");
  store = { m3u8_list: [rec(0)], mp4_list: [] };
  await Promise.all([
    api.updateMediaItem("m3u8_list", rec(0).url, { analyzed: true }),
    api.recordMedia(rec(7).url, TOP, "", "另一片", "m3u8", 0, 1),
  ]);
  check("两条写入都生效", store.m3u8_list.length === 2, store.m3u8_list.length);
  check(
    "analyzed 标记未丢失",
    store.m3u8_list.some((it) => it.url === rec(0).url && it.analyzed === true)
  );

  console.log(`\n${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
})();
