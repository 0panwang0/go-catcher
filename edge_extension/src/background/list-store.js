// 嗅探列表存储层：写串行化 + 容量淘汰 + TTL 清理 + 记录增改。
// 上层（嗅探探测器 / 消息路由 / 下载器页面）只跟 recordMedia / updateMediaItem /
// clearLists 打交道，不直接碰 storage。
import { MAX_SNIFFED, MAX_SNIFF_AGE_MS } from "./constants.js";

// ============================================================
// storage 写串行化
// ------------------------------------------------------------
// m3u8_list / mp4_list 的每次写入都是「读出整份快照 → 改数组 → 整体写回」。
// 而 webRequest.onCompleted 是每请求一个回调、并发触发：打开一个 HLS 播放页，
// 几百毫秒内会命中 master + 各档 variant + 各档 media playlist + 广告/预览，
// 每个命中各走一遍 recordMedia。await 会让出事件循环，两次调用的 get/set 一旦
// 交错，后写的那次就会用「改之前」的旧快照覆盖前一次的结果 —— 静默丢掉一条
// 记录（而且丢的常常是后到的主视频，因为广告/预览先到）。
//
// 这里用一条进程内 promise 链把「读-改-写」整体串行化：每个写事务排队执行，
// 前一个 settle 后下一个才开始，杜绝丢失更新。所有 list 的写入口都必须经过
// withListLock（recordMedia / clearLists / updateMediaItem）。
let storageWriteChain = Promise.resolve();

export function withListLock(fn) {
  const run = storageWriteChain.then(fn, fn);
  // 吞掉链上的错误，避免一次失败让后续事务全部短路（fn 内部自行兜错）
  storageWriteChain = run.catch(() => {});
  return run;
}

// pickEvictionIndex 选出一条最该被淘汰的记录下标。优先级从高到低：
//   1. 已分析过的（analyzed === true）—— 用户已经看过/用过，价值最低
//   2. 不属于当前标签页的 —— 用户已经切走的页面
//   3. 同优先级里最老的（time 最小；列表是 unshift 新记录在队首，故下标越大越老）
// 旧的 list.pop() 直接弹队尾：unshift 插队首 + pop 弹队尾，看似「淘汰最老」，
// 但广告/预览请求会把 30 条配额瞬间填满，把主视频一路挤到队尾弹掉。
export function pickEvictionIndex(list, currentTabId) {
  let pick = -1;
  let pickRank = -1;
  let pickTime = Infinity;
  for (let i = 0; i < list.length; i++) {
    const it = list[i];
    const rank =
      (it.analyzed ? 2 : 0) +
      (currentTabId != null && it.tabId !== currentTabId ? 1 : 0);
    const t = it.time || 0;
    if (rank > pickRank || (rank === pickRank && t <= pickTime)) {
      pick = i;
      pickRank = rank;
      pickTime = t;
    }
  }
  return pick;
}

// pruneExpired 就地剔除过期记录（无 time 字段的旧记录保留，交给体积上限处理）。
export function pruneExpired(list) {
  const now = Date.now();
  return list.filter((it) => !it.time || now - it.time < MAX_SNIFF_AGE_MS);
}

// recordMedia 把一条嗅探记录并入对应列表。整个「读-改-写」都在 storage 写锁里
// 完成，避免并发调用互相覆盖（详见 withListLock 注释）。tabId 仅用于淘汰排序：
// 优先淘汰非当前标签页的记录。
export function recordMedia(url, pageUrl, frameUrl, title, type, size = 0, tabId = null) {
  const key = type === "mp4" ? "mp4_list" : "m3u8_list";
  return withListLock(async () => {
    const { [key]: stored = [] } = await chrome.storage.local.get(key);
    const list = pruneExpired(stored);
    const existing = list.find((it) => it.url === url);
    if (existing) {
      existing.pageUrl = pageUrl || existing.pageUrl;
      existing.frameUrl = frameUrl || existing.frameUrl || "";
      existing.title = title || existing.title;
      existing.time = Date.now();
      if (tabId != null) existing.tabId = tabId;
      if (size) existing.size = size;
    } else {
      list.unshift({
        url,
        pageUrl,
        frameUrl: frameUrl || "",
        title,
        time: Date.now(),
        type,
        size,
        analyzed: false,
        tabId,
      });
      while (list.length > MAX_SNIFFED) {
        const idx = pickEvictionIndex(list, tabId);
        if (idx < 0) break;
        list.splice(idx, 1);
      }
    }
    await chrome.storage.local.set({ [key]: list });
    if (type === "m3u8") {
      chrome.action.setBadgeText({ text: String(list.length) });
      chrome.action.setBadgeBackgroundColor({ color: "#e74c3c" });
    }
  });
}

// updateMediaItem 供扩展页面（downloader.js 的分析流程）回写单条记录用。
// 页面与 service worker 是各自独立的 JS 上下文，各自直接读写 storage 时无法
// 互相串行化 —— 所以页面不直接写 storage，而是把这次写事务委托给这里，
// 与 recordMedia 共用同一条写锁。
export function updateMediaItem(key, url, patch) {
  return withListLock(async () => {
    const { [key]: list = [] } = await chrome.storage.local.get(key);
    const cur = list.find((it) => it.url === url);
    if (!cur) return { ok: false, error: "记录已不存在" };
    Object.assign(cur, patch || {});
    await chrome.storage.local.set({ [key]: list });
    return { ok: true };
  });
}

// clearLists 清空两个嗅探列表（工具栏 / 下载器页面的「清空列表」按钮）。
export function clearLists() {
  return withListLock(() => chrome.storage.local.set({ m3u8_list: [], mp4_list: [] }));
}
