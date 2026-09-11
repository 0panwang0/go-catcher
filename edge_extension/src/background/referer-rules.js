// Referer/Origin 注入规则（declarativeNetRequest 动态规则）。
// 下载器页面（chrome-extension://...）请求媒体时 CDN 常校验 Referer，
// 这里按 host 批量注入页面来源。

export async function setRefererRules(tabId, pairs) {
  const existing = await chrome.declarativeNetRequest.getDynamicRules();
  const ids = existing
    .filter((r) => r.id >= 1000 && r.id < 10000)
    .map((r) => r.id);
  if (ids.length) {
    await chrome.declarativeNetRequest.updateDynamicRules({
      removeRuleIds: ids,
    });
  }

  const seen = new Set();
  const rules = [];
  let id = 1000;
  for (const { host, referer } of pairs) {
    if (!host || !referer) continue;
    const key = `${host}|${referer}`;
    if (seen.has(key)) continue;
    seen.add(key);
    let origin = "";
    try {
      origin = new URL(referer).origin;
    } catch { }
    if (!origin) continue;
    rules.push({
      id: id++,
      priority: 1,
      action: {
        type: "modifyHeaders",
        requestHeaders: [
          { header: "Referer", operation: "set", value: referer },
          { header: "Origin", operation: "set", value: origin },
        ],
      },
      condition: {
        urlFilter: `||${host}`,
        resourceTypes: ["xmlhttprequest"],
        tabIds: [tabId],
      },
    });
  }
  if (rules.length) {
    await chrome.declarativeNetRequest.updateDynamicRules({
      addRules: rules,
    });
  }
}
