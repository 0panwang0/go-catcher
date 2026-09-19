// 分片 Referer 捕获：按「分片主机」记录浏览器实际发出的 Referer。
//
// 背景：下载时不能一律用页面 Referer。分片请求「带不带 Referer、带哪个」只有
// 浏览器知道，且站点间差异很大：
//   * 常见防盗链认播放页/解析站域名（Referer=来源页）；
//   * 有的站声明 no-referrer，浏览器对分片根本不带 Referer，而它的 CDN 是**白名单**
//     语义：空 Referer 与白名单域名放行，来自页面域名的 Referer 反而 403。
// 这里经 onSendHeaders 为每个 media/xhr 请求记录 host→Referer，其中 Referer 为
// "" 表示「浏览器没带」——这个「不带」同样是必须如实传递的信号：丢了它，下载层
// 会回退页面 Referer，白名单型 CDN 必 403。下载时序列化成 segrefs 传给 Go，
// 由 Go 抓分片时按 host 选用。
//
// 持久化用 storage.session：SW 随时可能被回收，纯内存映射会在「嗅探完 → 用户
// 点下载」之间被清空；storage.session 跨 SW 重启存活、随浏览器会话结束而清空，
// 隐私上比 storage.local 更克制（不落盘）。

const KEY = "segRefByHost";
const MAX_ENTRIES = 256;

// cache = Map<host, referer>；null 表示尚未从 storage.session 加载。只在
// storage.session 不存在（极老浏览器 / 测试桩）时退化为纯内存映射。
let cache = null;

function sessionArea() {
  return chrome && chrome.storage && chrome.storage.session ? chrome.storage.session : null;
}

async function loadCache() {
  if (cache) return cache;
  cache = new Map();
  const area = sessionArea();
  if (!area) return cache;
  try {
    const got = await area.get(KEY);
    const obj = got && got[KEY];
    if (obj && typeof obj === "object") {
      // 空串是有效值（= 浏览器对该 host 没带 Referer），只排除非字符串。
      for (const [h, r] of Object.entries(obj)) {
        if (typeof r === "string" && h) cache.set(h, r);
      }
    }
  } catch {}
  return cache;
}

async function flush() {
  const area = sessionArea();
  if (!area || !cache) return;
  try {
    const obj = {};
    for (const [h, r] of cache) obj[h] = r;
    await area.set({ [KEY]: obj });
  } catch {}
}

// refererFromDetails 从 webRequest.onSendHeaders 的 details 里抽出分片主机与
// 浏览器实际使用的 Referer；referer 可为 ""（页面 no-referrer 时浏览器根本不带
// Referer，这个「不带」也必须如实传下去，否则 Go 兜底塞页面 Referer 会被 CDN 判
// 403）。只有拿不到 host、或非 media/xhr 请求、或无关联页签时才返回 null。
export function refererFromDetails(details) {
  if (!details || details.tabId < 0) return null;
  const type = details.type;
  if (type !== "xmlhttprequest" && type !== "media") return null;
  let referer = "";
  for (const h of details.requestHeaders || []) {
    if (h && h.name && h.name.toLowerCase() === "referer") {
      referer = h.value || "";
      break;
    }
  }
  let host = "";
  try {
    host = new URL(details.url).host;
  } catch {
    return null;
  }
  if (!host) return null;
  return { host, referer };
}

// captureSegmentReferer 记录 host→Referer。referer 允许为空串：它表示「浏览器对
// 该 host 没带 Referer」，是必须留档的有效信号（Go 侧据此不再回退页面 Referer）；
// 只有 host 拿不到时才跳过。
export async function captureSegmentReferer(host, referer) {
  if (!host) return;
  const m = await loadCache();
  if (m.get(host) === referer) return;
  // 简单 FIFO 淘汰：超出上限时先逐出最早写入的条目（插入顺序即写入顺序）。
  while (m.size >= MAX_ENTRIES && !m.has(host)) {
    const oldest = m.keys().next().value;
    m.delete(oldest);
  }
  m.delete(host);
  m.set(host, referer);
  await flush();
}

// segmentReferers 返回当前捕获到的 host→Referer 映射（普通对象，供序列化）。
export async function segmentReferers() {
  const m = await loadCache();
  const out = {};
  for (const [h, r] of m) out[h] = r;
  return out;
}

// installRefererCapture 注册 onSendHeaders 侦听。Referer 属于 Chromium 的
// 「需 extraHeaders 才可见」请求头集合，extraInfoSpec 必须同时带 requestHeaders
// 与 extraHeaders，否则读不到 Referer、捕获到的映射为空。
export function installRefererCapture() {
  const wreq = chrome && chrome.webRequest;
  if (!wreq || !wreq.onSendHeaders) return;
  wreq.onSendHeaders.addListener(
    (details) => {
      const hit = refererFromDetails(details);
      if (hit) captureSegmentReferer(hit.host, hit.referer).catch(() => {});
    },
    { urls: ["http://*/*", "https://*/*"] },
    ["requestHeaders", "extraHeaders"],
  );
}