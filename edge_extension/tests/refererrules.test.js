// 回归测试：DNR Referer 规则的定向清理（2026-09-12 评审 P1-1）。
// 用法（在 edge_extension 目录下）：
//   node tests/refererrules.test.js
//
// 旧实现每次全量清空 1000-9999 号段再重建，有两个用户可见后果：
//   - 两个下载器页签先后设置时，后写者把先写者的规则一并删掉
//     （last-writer-wins）→ 先打开的页面分析请求被 CDN 403
//   - 页签关闭后规则永久残留，长期占用动态规则配额（Chrome 上限 5000 条）
// 本测试用数组模拟 DNR 动态规则存储，断言新实现的语义：
//   - 清理范围与规则作用域同维度（只动本 tab 的规则）
//   - 同 tab 重设为替换语义，remove/add 合并为一次 updateDynamicRules
//   - clearRefererRules / sweepStaleRules 各自只清目标规则
const fs = require("fs");
const path = require("path");

// ============================================================
// chrome 桩：数组当规则存储（结构化克隆语义），aliveTabs 模拟存活页签
// ============================================================
let store = [];
let aliveTabs = [];
const updates = [];
const clone = (v) => JSON.parse(JSON.stringify(v));
const noop = { addListener() {} };

const chrome = {
  action: { onClicked: noop, setBadgeText() {}, setBadgeBackgroundColor() {} },
  webRequest: { onCompleted: noop },
  runtime: { onMessage: noop, getURL: (p) => p, lastError: undefined },
  tabs: {
    get() {},
    create() {},
    onRemoved: noop,
    query: async () => aliveTabs.map((id) => ({ id })),
  },
  declarativeNetRequest: {
    getDynamicRules: async () => clone(store),
    updateDynamicRules: async (opts) => {
      updates.push(clone(opts));
      const rm = new Set(opts.removeRuleIds || []);
      store = store.filter((r) => !rm.has(r.id));
      store.push(...clone(opts.addRules || []));
    },
  },
  storage: {
    onChanged: noop,
    local: {
      get: async () => ({}),
      set: async () => {},
    },
  },
};

const src = fs.readFileSync(path.join(__dirname, "..", "background.js"), "utf8");
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

function tabRules(tabId) {
  return store.filter((r) => (r.condition?.tabIds || []).includes(tabId));
}

const PAIRS_A = [{ host: "a.com", referer: "https://a.com/watch/1" }];
const PAIRS_B = [{ host: "b.com", referer: "https://b.com/watch/2" }];

(async () => {
  // 场景 1：两个页签先后设置，互不删除（旧实现在这里丢 tab10 的规则）
  await api.setRefererRules(10, PAIRS_A);
  await api.setRefererRules(20, PAIRS_B);
  check(
    "tab10 与 tab20 的规则都存活",
    store.length === 2 && tabRules(10).length === 1 && tabRules(20).length === 1,
    store.map((r) => r.condition.tabIds),
  );

  // 场景 2：同 tab 重设 = 替换语义，且 remove/add 单次调用完成
  updates.length = 0;
  await api.setRefererRules(10, [
    { host: "a.com", referer: "https://a.com/watch/1" },
    { host: "c.com", referer: "https://c.com/watch/3" },
  ]);
  check("同 tab 重设后规则数为 2（替换而非追加）", tabRules(10).length === 2, store.length);
  check(
    "重设在单次 updateDynamicRules 内完成（remove 旧 1 条 + add 新 2 条）",
    updates.length === 1 &&
      (updates[0].removeRuleIds || []).length === 1 &&
      (updates[0].addRules || []).length === 2,
    updates,
  );

  // 场景 3：空 pairs = 清空本 tab，不影响其他 tab
  await api.setRefererRules(20, []);
  check(
    "空 pairs 清空 tab20，tab10 不受影响",
    tabRules(20).length === 0 && tabRules(10).length === 2,
    store.map((r) => r.condition.tabIds),
  );

  // 场景 4：clearRefererRules 只删指定 tab
  await api.clearRefererRules(10);
  check("clearRefererRules(10) 后无任何残留", store.length === 0, store.length);

  // 场景 5：sweep 只清指向已关闭 tab 的规则
  await api.setRefererRules(30, PAIRS_A);
  await api.setRefererRules(40, PAIRS_B);
  aliveTabs = [40];
  await api.sweepStaleRules();
  check(
    "sweep 清掉死 tab(30) 的规则、保留存活 tab(40) 的",
    tabRules(30).length === 0 && tabRules(40).length === 1,
    store.map((r) => r.condition.tabIds),
  );

  // 场景 6：规则内容——按 tab 限定，注入 Referer + Origin
  const r = tabRules(40)[0];
  check(
    "规则按 tab 限定且注入 Referer+Origin",
    r.condition.tabIds.includes(40) &&
      r.condition.urlFilter === "||b.com" &&
      r.condition.resourceTypes.includes("xmlhttprequest") &&
      r.action.requestHeaders.some(
        (h) => h.header === "Referer" && h.value === "https://b.com/watch/2",
      ) &&
      r.action.requestHeaders.some(
        (h) => h.header === "Origin" && h.value === "https://b.com",
      ),
    r,
  );

  // 场景 7：无效条目跳过（缺 host / 缺 referer / referer 非 URL）
  await api.setRefererRules(50, [
    { host: "", referer: "https://x.com/1" },
    { host: "x.com", referer: "::::not-a-url" },
    { host: "x.com" },
  ]);
  check("无效 pairs 不产生规则", tabRules(50).length === 0, store.length);

  // 场景 8（P2-3）：号段内 id 全被占用时，setRefererRules 必须显式失败 ——
  // 旧实现会生成 id: undefined 的规则，updateDynamicRules 随后抛错，报错
  // 完全看不出根因。号段上界也必须落在 Chrome 的单扩展配额（5000）之内。
  store = [];
  for (let id = 1000; id < 5000; id++) {
    store.push({ id, priority: 1, action: {}, condition: { tabIds: [999] } });
  }
  check("号段取满时 allocRuleIds 返回不足长度", api.allocRuleIds(store, 1).length === 0);
  let quotaErr = "";
  try {
    await api.setRefererRules(50, [{ host: "x.com", referer: "https://x.com/1" }]);
  } catch (e) {
    quotaErr = String(e);
  }
  check("配额耗尽时 setRefererRules 显式报错", /配额不足/.test(quotaErr), quotaErr);
  check(
    "配额耗尽时不产生 id:undefined 的规则",
    store.every((r) => r.id !== undefined),
    store.length,
  );
  store = [];
  check(
    "号段起点仍是 1000、且分配结果落在 Chrome 上限之内",
    api.allocRuleIds([], 1)[0] === 1000,
    api.allocRuleIds([], 1),
  );

  console.log(`\n${fail ? "FAIL" : "PASS"}: ${pass} ok, ${fail} failed`);
  process.exitCode = fail ? 1 : 0;
})();
