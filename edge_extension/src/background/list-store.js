// 嗅探列表存储层：写串行化 + 内存权威副本 + 合并落盘 + 容量淘汰 + TTL 清理。
// 上层（嗅探探测器 / 消息路由 / 下载器页面）只跟 recordMedia / updateMediaItem /
// readLists / clearLists 打交道，不直接碰 storage —— 绕过缓存直接改 storage
// 会让内存副本与磁盘分叉（详见下面「内存权威副本」一节的不变量）。
import { MAX_SNIFFED, MAX_SNIFF_AGE_MS } from "./constants.js";

const LIST_KEYS = ["m3u8_list", "mp4_list"];

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

// ============================================================
// 内存权威副本 + 合并落盘（评审 P2-6 / F10）
// ------------------------------------------------------------
// 旧实现在**每个**写事务里做「读整表 → 改 → 写整表」：一次页面命中的每个 URL
// 都是一次全表 get + 全表 set 的 IPC，并触发一次 storage.onChanged 广播
// （开着的下载器页面每次都要重渲染）。同一批嗅探事件彼此无关、又高度密集，
// 合并落盘才是它们的正确写模型。
//
// 现在：内存里持一份权威副本，写事务只改副本并标脏；**由写链上最后一个事务
// 负责落盘**。刻意不设「N 毫秒后写」的定时器 —— 队列一空立刻写：空闲时零延迟，
// 突发时自动合并成一次。而 MV3 的 service worker 随时可能被回收，攒一个固定
// 窗口、窗口没到就被回收，脏数据就真丢了。
//
// 不变量（改动本文件时务必守住）：
//   1. list 的读走 readLists、写走 withListLock —— 不许再有第二条写路径。
//   2. 落盘失败保留脏标记，由下次事务或下次 flush 重试；不吞掉失败当成功。
//   3. 记账必须晚于落盘：副本先改、storage 后写，中间任何一步失败都不能
//      让调用方以为「已经存好了」（落盘失败会往调用方抛，见 withListLock）。
// ============================================================
let listCache = null;
const dirtyKeys = new Set();
let flushing = false;
let queued = 0; // 已排入写链、尚未 settle 的事务数

async function loadLists() {
  if (!listCache) {
    const got = await chrome.storage.local.get(LIST_KEYS);
    listCache = {};
    for (const k of LIST_KEYS) {
      listCache[k] = Array.isArray(got[k]) ? got[k] : [];
    }
  }
  return listCache;
}

// readLists 取两个列表（数组层浅拷贝，调用方不会 splice 到权威副本）。
// 元素本身不深拷贝：service worker 内没有任何一处会就地改记录，
// 唯一的改入口是 updateMediaItem（走写锁），跨进程的那几处经消息传递自动克隆。
export async function readLists() {
  const c = await loadLists();
  return { m3u8_list: c.m3u8_list.slice(), mp4_list: c.mp4_list.slice() };
}

// resetListCache 丢弃内存副本，下次读取重新从 storage 拉。
// 只有测试用它（每个用例直接重置 storage 桩来造场景），线上无调用点。
export function resetListCache() {
  listCache = null;
  dirtyKeys.clear();
}

function updateBadge() {
  if (!listCache) return;
  const n = listCache.m3u8_list.length;
  chrome.action.setBadgeText({ text: n ? String(n) : "" });
  chrome.action.setBadgeBackgroundColor({ color: "#e74c3c" });
}

// flushDirty 把标脏的列表整体写回 storage。落盘期间又变脏就再写一次。
// 失败时保留脏标记并抛出 —— 调用方（withListLock）负责让上层知道这次没存成。
function flushDirty() {
  if (flushing || !dirtyKeys.size) return Promise.resolve();
  flushing = true;
  const keys = [...dirtyKeys];
  dirtyKeys.clear();
  const patch = {};
  for (const k of keys) patch[k] = listCache[k];
  return chrome.storage.local.set(patch).then(
    () => {
      flushing = false;
      if (keys.includes("m3u8_list")) updateBadge();
      if (dirtyKeys.size) return flushDirty(); // 落盘期间又有事务标脏
    },
    (e) => {
      flushing = false;
      for (const k of keys) dirtyKeys.add(k); // 保留脏标记，下次重试
      throw e;
    }
  );
}

export function withListLock(fn) {
  queued++;
  const run = storageWriteChain.then(fn, fn);
  // 吞掉链上的错误，避免一次失败让后续事务全部短路（fn 内部自行兜错）
  storageWriteChain = run.catch(() => {});
  // 事务结束时若写链已空，就地把脏数据落盘。失败路径同样要走一遍：
  // fn 抛错不该把「已经改好的副本」永远留在内存里不落盘。
  const settle = async (v) => {
    queued--;
    if (queued === 0) {
      try {
        await flushDirty();
      } catch (e) {
        // 脏标记已保留，下次事务或下次唤醒会重试。这里必须让调用方看得见：
        // 列表没落盘，页面读到的会是旧值。
        console.error("[M3U8 Video Catcher] 嗅探列表落盘失败（已保留待重试）:", e);
      }
    }
    return v;
  };
  return run.then(settle, (e) => settle(undefined).then(() => { throw e; }));
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
    const cache = await loadLists();
    const list = pruneExpired(cache[key]);
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
    cache[key] = list;
    dirtyKeys.add(key);
  });
}

// updateMediaItem 供扩展页面（downloader.js 的分析流程）回写单条记录用。
// 页面与 service worker 是各自独立的 JS 上下文，页面无法参与本模块的写串行化，
// 所以它不直接写 storage，而是把这次写事务发消息委托给这里，与 recordMedia
// 共用同一条写锁。
export function updateMediaItem(key, url, patch) {
  if (!LIST_KEYS.includes(key)) {
    return Promise.resolve({ ok: false, error: `未知列表 ${key}` });
  }
  return withListLock(async () => {
    const cache = await loadLists();
    const list = cache[key];
    const cur = list.find((it) => it.url === url);
    if (!cur) return { ok: false, error: "记录已不存在" };
    Object.assign(cur, patch || {});
    dirtyKeys.add(key);
    return { ok: true };
  });
}

// clearLists 清空两个嗅探列表（工具栏 / 下载器页面的「清空列表」按钮）。
// 徽标由落盘后的 updateBadge 统一归零，调用方不必再自己清。
export function clearLists() {
  return withListLock(async () => {
    const cache = await loadLists();
    cache.m3u8_list = [];
    cache.mp4_list = [];
    dirtyKeys.add("m3u8_list");
    dirtyKeys.add("mp4_list");
  });
}
