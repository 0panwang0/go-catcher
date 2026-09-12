// Referer/Origin 注入规则（declarativeNetRequest 动态规则）。
// 下载器页面（chrome-extension://...）请求媒体时 CDN 常校验 Referer，
// 这里按 host 批量注入页面来源。
//
// 作用域语义（P1-1）：规则的生效范围与清理范围必须同维度——规则按
// condition.tabIds 限定在单个下载器页，清理也只清该 tab 的规则。
// 旧实现每次全量清空号段再重建，两个页签先后设置时后写者会把先写者
// 的规则一并删掉；页签关闭后规则永久残留、占用动态规则配额。

// 本扩展占用的动态规则 id 号段；清理与 id 分配都只认这个号段。
const RULE_ID_MIN = 1000;
const RULE_ID_MAX = 10000;

// DNR 读改写串行化：并发的 set/clear/sweep 若交错执行「读现有规则 →
// 算新 id → 写回」，会分配出重复 id 互相覆盖。SW 单实例，模块级
// promise 链即可当互斥锁用。
let dnrChain = Promise.resolve();

// enqueueDnr 把一次 DNR 读改写排进串行队列；单次失败不断链、也不产生
// unhandled rejection（失败已上报给调用方，队列必须继续服务后续请求）。
function enqueueDnr(job) {
  const run = dnrChain.then(job);
  dnrChain = run.catch(() => {});
  return run;
}

function oursInSegment(rules) {
  return rules.filter((r) => r.id >= RULE_ID_MIN && r.id < RULE_ID_MAX);
}

// ruleIdsForTab 号段内、作用于指定 tab 的规则 id。getDynamicRules 返回的
// 规则自带 tabIds——它本身就是持久化的 tab→规则映射，无需另建映射表。
function ruleIdsForTab(rules, tabId) {
  return oursInSegment(rules)
    .filter((r) => (r.condition?.tabIds || []).includes(tabId))
    .map((r) => r.id);
}

// allocRuleIds 在号段内分配 count 个未占用的 id。占用集合取自当前快照，
// 含本批即将被 remove 的旧规则——同批 remove/add 不复用 id。
function allocRuleIds(rules, count) {
  const used = new Set(oursInSegment(rules).map((r) => r.id));
  const ids = [];
  for (let id = RULE_ID_MIN; ids.length < count && id < RULE_ID_MAX; id++) {
    if (!used.has(id)) ids.push(id);
  }
  return ids;
}

export function setRefererRules(tabId, pairs) {
  return enqueueDnr(() => applyRefererRules(tabId, pairs));
}

export function clearRefererRules(tabId) {
  return enqueueDnr(() => removeRules((rules) => ruleIdsForTab(rules, tabId)));
}

// sweepStaleRules 清掉指向已关闭 tab 的规则：SW 休眠会错过
// tabs.onRemoved 事件，每次唤醒时按存活 tab 集合扫一遍补漏。
export function sweepStaleRules() {
  return enqueueDnr(async () => {
    const tabs = await chrome.tabs.query({});
    const alive = new Set(tabs.map((t) => t.id));
    await removeRules((rules) =>
      oursInSegment(rules)
        .filter((r) => {
          const ids = r.condition?.tabIds || [];
          return ids.length > 0 && ids.every((id) => !alive.has(id));
        })
        .map((r) => r.id),
    );
  });
}

async function removeRules(pickIds) {
  const rules = await chrome.declarativeNetRequest.getDynamicRules();
  const removeRuleIds = pickIds(rules);
  if (removeRuleIds.length) {
    await chrome.declarativeNetRequest.updateDynamicRules({ removeRuleIds });
  }
}

// applyRefererRules 用 pairs 重建该 tab 的规则：只删本 tab 的旧规则，
// remove/add 合并为一次 updateDynamicRules（缩短读改写窗口）。
async function applyRefererRules(tabId, pairs) {
  const rules = await chrome.declarativeNetRequest.getDynamicRules();
  const removeRuleIds = ruleIdsForTab(rules, tabId);

  const seen = new Set();
  const specs = [];
  for (const { host, referer } of pairs || []) {
    if (!host || !referer) continue;
    const key = `${host}|${referer}`;
    if (seen.has(key)) continue;
    seen.add(key);
    let origin = "";
    try {
      origin = new URL(referer).origin;
    } catch {}
    if (!origin) continue;
    specs.push({ host, referer, origin });
  }

  const ids = allocRuleIds(rules, specs.length);
  const addRules = specs.map((s, i) => ({
    id: ids[i],
    priority: 1,
    action: {
      type: "modifyHeaders",
      requestHeaders: [
        { header: "Referer", operation: "set", value: s.referer },
        { header: "Origin", operation: "set", value: s.origin },
      ],
    },
    condition: {
      urlFilter: `||${s.host}`,
      resourceTypes: ["xmlhttprequest"],
      tabIds: [tabId],
    },
  }));

  if (!removeRuleIds.length && !addRules.length) return;
  await chrome.declarativeNetRequest.updateDynamicRules({ removeRuleIds, addRules });
}
