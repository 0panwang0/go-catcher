// 回归测试：嗅探记录的并发写入、合并落盘与淘汰策略（R5 + P2-6/F10）。
// 用法（在 edge_extension 目录下）：
//   node tests/storage.test.js
//
// 关键：chrome.storage 的桩必须做「结构化克隆」语义（get 返回快照、set 存快照），
// 否则读写共享同一数组引用会掩盖竞态。get 延迟到宏任务返回，保证 N 个并发调用
// 的读阶段全部先于写阶段完成 —— 这正是真实场景（webRequest 每请求一个回调）。
//
// 另一条关键：list-store 持**内存权威副本**，所以每个用例在重置 storage 桩之后
// 必须调 api.resetListCache()，否则会读到上一个用例的副本。
const fs = require("fs");
const path = require("path");

let store = {};
let setCalls = 0;
let badgeCalls = 0;
const deepClone = (v) => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));
const noop = { addListener() {} };

const chrome = {
  action: {
    onClicked: noop,
    setBadgeText() {
      badgeCalls++;
    },
    setBadgeBackgroundColor() {},
  },
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
        setCalls++;
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

// fresh 重置 storage 桩与内存副本，进入一个干净的用例场景。
function fresh(seed = {}) {
  store = { m3u8_list: [], mp4_list: [], ...seed };
  setCalls = 0;
  badgeCalls = 0;
  api.resetListCache();
}

const TOP = "https://www.xmfyy.com/index.php/vod/play/id/290898/sid/1/nid/1.html";
// i 越大越新；基准取「刚刚」，避免被 TTL 当成过期记录清掉。
const NOW = Date.now();
const rec = (i, extra = {}) => ({
  url: `https://cdn.example.com/play/e0RLJ1Vb/v${i}/index.m3u8`,
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
  fresh();
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
  fresh();
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
  fresh();
  for (let i = 0; i < 30; i++) store.m3u8_list.push(rec(i, { analyzed: i === 5 }));
  api.resetListCache(); // 直接改了桩，重新载入副本
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
  fresh();
  store.m3u8_list.push(rec(0, { tabId: 9, time: NOW + 60_000 }));
  for (let i = 1; i < 30; i++) store.m3u8_list.push(rec(i));
  api.resetListCache();
  await api.recordMedia(rec(99).url, TOP, "", "新片", "m3u8", 0, 1);
  check(
    "非当前 tab 的最新记录被淘汰",
    !store.m3u8_list.some((it) => it.url === rec(0).url)
  );
  check("当前 tab 的旧记录保留", store.m3u8_list.some((it) => it.url === rec(1).url));
  check("长度维持上限 30", store.m3u8_list.length === 30, store.m3u8_list.length);

  console.log("TTL：过期记录在下次写入时清理：");
  fresh();
  store.m3u8_list.push(rec(0, { time: Date.now() - 8 * 24 * 3600 * 1000 }));
  api.resetListCache();
  await api.recordMedia(rec(1).url, TOP, "", "新片", "m3u8", 0, 1);
  check("8 天前的记录被清理", !store.m3u8_list.some((it) => it.url === rec(0).url));
  check("当天记录保留", store.m3u8_list.some((it) => it.url === rec(1).url));

  console.log("updateMediaItem：页面侧回写走同一把锁：");
  fresh({ m3u8_list: [rec(0)] });
  await api.updateMediaItem("m3u8_list", rec(0).url, { analyzed: true, quality: "1080p" });
  check(
    "补丁已合并",
    store.m3u8_list[0].analyzed === true && store.m3u8_list[0].quality === "1080p",
    store.m3u8_list[0]
  );
  const missRes = await api.updateMediaItem("m3u8_list", "https://nope/x.m3u8", { a: 1 });
  check("不存在的 URL 返回 ok:false", missRes && missRes.ok === false, missRes);
  const badKey = await api.updateMediaItem("oops_list", rec(0).url, { a: 1 });
  check("未知列表名显式报错（否则会静默写进一个没人读的地方）",
    badKey && badKey.ok === false, badKey);

  console.log("updateMediaItem 与 recordMedia 并发不互相覆盖：");
  fresh({ m3u8_list: [rec(0)] });
  await Promise.all([
    api.updateMediaItem("m3u8_list", rec(0).url, { analyzed: true }),
    api.recordMedia(rec(7).url, TOP, "", "另一片", "m3u8", 0, 1),
  ]);
  check("两条写入都生效", store.m3u8_list.length === 2, store.m3u8_list.length);
  check(
    "analyzed 标记未丢失",
    store.m3u8_list.some((it) => it.url === rec(0).url && it.analyzed === true)
  );

  // ------------------------------------------------------------
  // P2-6 / F10：合并落盘
  // ------------------------------------------------------------
  console.log("合并落盘：突发写入合并成一次 set（旧实现每次命中一次全表写）：");
  fresh();
  const M = 50;
  await Promise.all(
    Array.from({ length: M }, (_, i) =>
      api.recordMedia(rec(i).url, TOP, "", "异种污染", "m3u8", 0, 1)
    )
  );
  check(`${M} 次并发 recordMedia 的 storage.set 次数 < 5（旧实现 = ${M}）`,
    setCalls < 5, { setCalls });
  // 50 条 > 上限 30：这里要的是「合并落盘后淘汰策略照常生效、最新那条在列」
  check("落库条数 = 上限 30", store.m3u8_list.length === 30, store.m3u8_list.length);
  check("最新一条在列（合并落盘没把最后几次写吞掉）",
    store.m3u8_list.some((it) => it.url === rec(M - 1).url));
  check("徽标只在落盘后更新一次（旧实现每次命中都设）", badgeCalls === 1, { badgeCalls });

  console.log("空闲时零延迟：写入之间没有事务排队就立刻落盘（不攒固定窗口）：");
  fresh();
  await api.recordMedia(rec(1).url, TOP, "", "甲", "m3u8", 0, 1);
  check("单次写入后立刻落盘", setCalls === 1 && store.m3u8_list.length === 1, {
    setCalls,
    n: store.m3u8_list.length,
  });
  await api.recordMedia(rec(2).url, TOP, "", "乙", "m3u8", 0, 1);
  check("两次独立写入 = 两次 set（第一次没被窗口拖住）", setCalls === 2, { setCalls });

  console.log("读取走内存权威副本：");
  fresh();
  await api.recordMedia(rec(1).url, TOP, "", "甲", "m3u8", 0, 1);
  const readBack = await api.readLists();
  check("readLists 读得到刚写入的记录", readBack.m3u8_list.length === 1, readBack.m3u8_list.length);
  check("两个列表都在返回值里", Array.isArray(readBack.mp4_list), Object.keys(readBack));

  console.log("落盘失败：保留脏标记、不假装存好、下次事务一并重试：");
  fresh();
  const realSet = chrome.storage.local.set;
  const realError = console.error;
  const errors = [];
  console.error = (...a) => errors.push(a[0]);
  let failNext = true;
  chrome.storage.local.set = (o) => {
    if (failNext) {
      failNext = false;
      return Promise.reject(new Error("QUOTA_BYTES exceeded"));
    }
    return realSet(o);
  };
  await api.recordMedia(rec(1).url, TOP, "", "甲", "m3u8", 0, 1);
  check("storage 里没有记录（没把失败当成功）", store.m3u8_list.length === 0, store.m3u8_list.length);
  check("内存副本仍有该记录（脏标记保留）", (await api.readLists()).m3u8_list.length === 1);
  check("失败被记进控制台（不静默）", errors.length === 1, errors);
  await api.recordMedia(rec(2).url, TOP, "", "乙", "m3u8", 0, 1);
  check("下次事务把上一次的脏数据一并写下去（重试生效）",
    store.m3u8_list.length === 2, store.m3u8_list.length);
  chrome.storage.local.set = realSet;
  console.error = realError;

  console.log("clearLists：清空两个列表并归零徽标：");
  fresh({ m3u8_list: [rec(0)], mp4_list: [{ url: "https://x/a.mp4" }] });
  await api.clearLists();
  check("两个列表都空了", store.m3u8_list.length === 0 && store.mp4_list.length === 0, store);
  check("徽标被清空（不是显示 0）", badgeCalls === 1, { badgeCalls });

  console.log(`\n${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
})();
