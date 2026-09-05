// M3U8 Video Catcher - background service worker

const MAX_SNIFFED = 30;

// 点击工具栏图标：打开下载器页面
chrome.action.onClicked.addListener(() => {
  chrome.tabs.create({ url: chrome.runtime.getURL("downloader.html") });
});

// 嗅探所有 .m3u8 请求
chrome.webRequest.onCompleted.addListener(
  (details) => {
    if (details.tabId < 0) return;
    chrome.tabs.get(details.tabId, (tab) => {
      if (chrome.runtime.lastError) return;
      recordMedia(details.url, tab.url || "", tab.title || "", "m3u8");
    });
  },
  { urls: ["*://*/*.m3u8", "*://*/*.m3u8?*"] }
);

// 嗅探 MP4 直链（IDM 也常提供 MP4 格式选项）
chrome.webRequest.onCompleted.addListener(
  (details) => {
    if (details.tabId < 0) return;
    chrome.tabs.get(details.tabId, (tab) => {
      if (chrome.runtime.lastError) return;
      let size = 0;
      if (details.responseHeaders) {
        const len = details.responseHeaders.find(
          (h) => h.name.toLowerCase() === "content-length"
        );
        if (len && len.value) size = parseInt(len.value, 10) || 0;
      }
      recordMedia(details.url, tab.url || "", tab.title || "", "mp4", size);
    });
  },
  { urls: ["*://*/*.mp4", "*://*/*.mp4?*"] },
  ["responseHeaders"]
);

async function recordMedia(url, pageUrl, title, type, size = 0) {
  const key = type === "mp4" ? "mp4_list" : "m3u8_list";
  const { [key]: list = [] } = await chrome.storage.local.get(key);
  const existing = list.find((it) => it.url === url);
  if (existing) {
    existing.pageUrl = pageUrl || existing.pageUrl;
    existing.title = title || existing.title;
    existing.time = Date.now();
    if (size) existing.size = size;
  } else {
    list.unshift({
      url,
      pageUrl,
      title,
      time: Date.now(),
      type,
      size,
      analyzed: false,
    });
    while (list.length > MAX_SNIFFED) list.pop();
  }
  await chrome.storage.local.set({ [key]: list });
  if (type === "m3u8") {
    chrome.action.setBadgeText({ text: String(list.length) });
    chrome.action.setBadgeBackgroundColor({ color: "#e74c3c" });
  }
}

// ============================================================
// 消息处理
// ============================================================
chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg.type === "setReferers") {
    setRefererRules(msg.tabId, msg.pairs)
      .then(() => sendResponse({ ok: true }))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  if (msg.type === "getVideoOptions") {
    getVideoOptions(sender.tab)
      .then((res) => sendResponse({ ok: true, options: res }))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  if (msg.type === "getVideoSource") {
    getVideoSource(msg)
      .then((res) => sendResponse({ ok: true, source: res }))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  if (msg.type === "openDownloader") {
    openDownloader(msg.item);
    sendResponse({ ok: true });
    return true;
  }
  if (msg.type === "clearList") {
    chrome.storage.local.set({ m3u8_list: [], mp4_list: [] }, () => {
      chrome.action.setBadgeText({ text: "" });
      sendResponse({ ok: true });
    });
    return true;
  }
  if (msg.type === "downloadViaServer") {
    downloadViaServer(msg)
      .then((res) => sendResponse(res))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  if (msg.type === "queryDownload") {
    queryDownload(msg)
      .then((res) => sendResponse(res))
      .catch((e) => sendResponse({ exists: false, error: String(e) }));
    return true;
  }
  if (msg.type === "controlTask") {
    // 暂停 / 恢复 / 取消 Go server 上某个任务（disk 落盘模式）
    controlTask(msg)
      .then((res) => sendResponse(res))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  if (msg.type === "getExePath") {
    // content.js 生成兜底命令前询问（错误面板可能在从未联系过服务的情况下出现，
    // 那时 detectExePath 还没跑过；这里实时探测一次再返回）
    detectExePath()
      .then(async () => sendResponse({ ok: true, exePath: (await getSettings()).detectedExePath || "go-catcher.exe" }))
      .catch(() => sendResponse({ ok: false, exePath: "go-catcher.exe" }));
    return true;
  }
});

async function setRefererRules(tabId, pairs) {
  // 给下载器页面（chrome-extension://...）的请求注入 Referer + Origin
  // declarativeNetRequest 只能基于目标 URL 匹配，这里按 host 批量添加
  // 规则 ID 范围：1000-9999，按 host 映射
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

// ============================================================
// 解析当前页面所有可下载项（TS 各档 + MP4 直链）
// ============================================================
async function getVideoOptions(tab) {
  if (!tab) throw new Error("无法获取当前标签页");
  const pageUrl = tab.url || "";
  const title = tab.title || "";

  const { m3u8_list = [], mp4_list = [] } = await chrome.storage.local.get([
    "m3u8_list",
    "mp4_list",
  ]);

  // 过滤属于当前页的媒体（同域名或同页面）
  const samePage = (it) => {
    if (!it.pageUrl) return false;
    try {
      return new URL(it.pageUrl).hostname === new URL(pageUrl).hostname;
    } catch {
      return false;
    }
  };

  const m3u8Items = m3u8_list.filter(samePage);
  const mp4Items = mp4_list.filter(samePage);

  const options = [];
  let seq = 1;

  for (const it of m3u8Items) {
    try {
      const text = await fetchText(it.url);
      if (text.includes("#EXT-X-STREAM-INF")) {
        const variants = parseVariants(text, it.url);
        for (const v of variants) {
          const subText = await fetchText(v.url);
          const duration = parseDuration(subText);
          const segments = parseSegments(subText, v.url).length;
          options.push({
            id: seq++,
            type: "ts",
            url: v.url,
            pageUrl: it.pageUrl,
            title: it.title || title,
            quality: shortQuality(v),
            resolution: v.resolution,
            bandwidth: v.bandwidth,
            duration,
            segments,
            label: v.label,
          });
        }
      } else {
        const duration = parseDuration(text);
        const segments = parseSegments(text, it.url).length;
        options.push({
          id: seq++,
          type: "ts",
          url: it.url,
          pageUrl: it.pageUrl,
          title: it.title || title,
          quality: qualityFromURL(it.url),
          resolution: "",
          bandwidth: 0,
          duration,
          segments,
          label: "未知画质",
        });
      }
    } catch (e) {
      console.error("解析 m3u8 失败:", it.url, e);
    }
  }

  for (const it of mp4Items) {
    options.push({
      id: seq++,
      type: "mp4",
      url: it.url,
      pageUrl: it.pageUrl,
      title: it.title || title,
      quality: qualityFromURL(it.url),
      resolution: "",
      bandwidth: 0,
      duration: 0,
      segments: 0,
      size: it.size || 0,
      label: it.size ? `${(it.size / 1024 / 1024).toFixed(1)} MB` : "",
    });
  }

  // 过滤掉预览片（时长 < 60s 的 TS），只保留主视频档位 + MP4 直链
  const MAIN_VIDEO_MIN = 60;
  const filtered = options.filter(
    (o) => o.type === "mp4" || o.duration >= MAIN_VIDEO_MIN
  );

  // 排序：TS 优先，同画质按码率从高到低
  const qualityRank = (q) =>
  ({
    "4K": 8,
    "2K": 7,
    "1080P": 6,
    "720P": 5,
    "480P": 4,
    "360P": 3,
    "240P": 2,
    "未知画质": 1,
    "": 0,
  }[String(q).toUpperCase()] || 0);

  filtered.sort((a, b) => {
    if (a.type !== b.type) return a.type === "ts" ? -1 : 1;
    const rankDiff = qualityRank(a.quality) - qualityRank(b.quality);
    if (rankDiff !== 0) return -rankDiff;
    return b.bandwidth - a.bandwidth;
  });

  // 重新编号
  filtered.forEach((o, i) => (o.id = i + 1));
  return filtered;
}

function openDownloader(item) {
  const params = new URLSearchParams();
  params.set("url", item.url);
  params.set("title", item.title || "");
  params.set("pageUrl", item.pageUrl || "");
  params.set("quality", item.quality || "");
  params.set("format", item.type || "ts");
  chrome.tabs.create({
    url: chrome.runtime.getURL("downloader.html?" + params.toString()),
  });
}

// ============================================================
// 只分析"这个视频"的源
// ============================================================
async function getVideoSource({ src = "", pageUrl = "", segmentDir = "", title = "" }) {
  const { m3u8_list = [], mp4_list = [] } = await chrome.storage.local.get([
    "m3u8_list",
    "mp4_list",
  ]);

  const sameHost = (it) => {
    if (!it.pageUrl || !pageUrl) return false;
    try {
      return new URL(it.pageUrl).hostname === new URL(pageUrl).hostname;
    } catch {
      return false;
    }
  };

  let hostM3U8 = m3u8_list.filter(sameHost);
  let hostMP4 = mp4_list.filter(sameHost);

  // master candidate 永远在前：避免播放器先请求 240p 变体导致我们误中预览片
  hostM3U8.sort(masterFirst);
  hostMP4.sort(masterFirst);

  // 1. 如果 video.src 本身就是 http 直链，优先用它
  if (/^https?:/i.test(src)) {
    const matched = hostMP4.find((it) => it.url === src);
    if (matched) {
      return makeMP4Source(matched, title, pageUrl);
    }
    // 虽然不是我们嗅探到的，但用户页面直接给了一个视频地址
    return {
      type: "mp4",
      url: src,
      title,
      pageUrl,
      quality: qualityFromURL(src),
    };
  }

  // 2. 用最近分片目录匹配 m3u8（视频正在播放时最准）
  if (segmentDir && hostM3U8.length) {
    const byDir = pickBestByDir(hostM3U8, segmentDir);
    if (byDir) return makeM3U8Source(byDir, title, pageUrl);
  }

  // 3. 取当前页最新的一条 master candidate m3u8（通常就是主视频）
  if (hostM3U8.length) {
    return makeM3U8Source(hostM3U8[0], title, pageUrl);
  }

  // 4.  fallback：当前页最大的 MP4 直链（过滤掉广告小片段）
  const realMP4 = hostMP4
    .filter((it) => (it.size || 0) > 1024 * 1024) // > 1MB
    .sort((a, b) => (b.size || 0) - (a.size || 0));

  if (realMP4.length) {
    return makeMP4Source(realMP4[0], title, pageUrl);
  }

  throw new Error("暂未嗅探到该视频，请先播放几秒再试。");
}

// 在 startsWith(segmentDir) 的候选集合里再次过滤：master 候选优先
function pickBestByDir(list, segmentDir) {
  const inDir = list.filter((it) => it.url.startsWith(segmentDir));
  if (!inDir.length) return null;
  const master = inDir.find((it) => isMasterCandidateM3U8(it.url));
  return master || inDir[0];
}

function makeM3U8Source(item, title, pageUrl) {
  // 不再在 background 里 fetch m3u8：扩展 Origin 会导致 CDN 403。
  // 把 URL 返回给 content.js，由页面主世界 fetch 代理解析（请求头与页面一致）。
  return {
    type: "ts",
    url: item.url,
    title: item.title || title,
    pageUrl: item.pageUrl || pageUrl,
    quality: qualityFromURL(item.url),
  };
}

function makeMP4Source(item, title, pageUrl) {
  return {
    type: "mp4",
    url: item.url,
    title: item.title || title,
    pageUrl: item.pageUrl || pageUrl,
    quality: qualityFromURL(item.url),
    size: item.size || 0,
  };
}

// ============================================================
// m3u8 / 画质解析工具（与 downloader.js 保持一致）
// ============================================================
async function fetchText(url) {
  for (let a = 1; a <= 3; a++) {
    try {
      const resp = await fetch(url, { credentials: "omit" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      return await resp.text();
    } catch (e) {
      if (a === 3) throw e;
      await sleep(500 * a);
    }
  }
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

function parseDuration(text) {
  let total = 0;
  for (const m of text.matchAll(/#EXTINF:([\d.]+)/g)) {
    total += parseFloat(m[1]);
  }
  return total;
}

function parseSegments(text, baseURL) {
  const list = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    list.push(new URL(line, baseURL).href);
  }
  return list;
}

function parseVariants(text, baseURL) {
  const lines = text.split("\n");
  const list = [];
  for (let i = 0; i < lines.length; i++) {
    if (!lines[i].startsWith("#EXT-X-STREAM-INF")) continue;
    const info = lines[i];
    const next = (lines[i + 1] || "").trim();
    if (!next || next.startsWith("#")) continue;
    const bw = parseInt((info.match(/BANDWIDTH=(\d+)/) || [])[1] || "0", 10);
    const res = (info.match(/RESOLUTION=(\d+x\d+)/) || [])[1] || "";
    list.push({
      url: new URL(next, baseURL).href,
      bandwidth: bw,
      resolution: res,
      label: variantLabel(res, bw, next),
    });
  }
  list.sort((a, b) => b.bandwidth - a.bandwidth);
  return list;
}

function variantLabel(resolution, bandwidth, uri) {
  let q = "";
  const m = String(uri).match(/(2160p|1440p|1080p|720p|480p|360p|240p)/i);
  if (m) {
    q = m[1].toUpperCase();
  } else if (resolution) {
    const h = parseInt(resolution.split("x")[1], 10);
    q =
      {
        2160: "4K",
        1440: "2K",
        1080: "1080P",
        720: "720P",
        480: "480P",
        360: "360P",
        240: "240P",
      }[h] || resolution;
  } else if (bandwidth) {
    q = (bandwidth / 1e6).toFixed(1) + " Mbps";
  }
  const parts = [q || "未知画质"];
  if (resolution) parts.push(resolution);
  if (bandwidth) parts.push(`${(bandwidth / 1000).toFixed(0)} kbps`);
  return parts.join(" · ");
}

function shortQuality(variant) {
  return String(variant.label || "").split(" · ")[0] || "";
}

function qualityFromURL(u) {
  try {
    const m = decodeURIComponent(new URL(u).pathname).match(
      /(2160p|1440p|1080p|720p|480p|360p|240p)/i
    );
    if (m) return m[1].toUpperCase();
    const q = new URL(u).searchParams.get("quality");
    if (q) return q;
  } catch { }
  return "";
}

// ============================================================
// Master / Variant 判定
// ============================================================
// 嗅探列表里 master 和各变体（或 ad 预热片）混在一起。
// 按 URL 形态判断"是不是 master 候选"——变体通常带 _240p/_720p 等后缀，
// master 则一般是 /ID.m3u8。这种启发式足够处理常见 CDN（growcdn/alibaba 等）。
function isMasterCandidateM3U8(url) {
  try {
    const path = new URL(url).pathname;
    if (!/\.m3u8$/i.test(path)) return false;
    // 变体后缀：_240p/_360p/_480p/_720p/_1080p/_1440p/_2160p + 可选 .m4s/.m3u8
    if (/_\d{3,4}p\.(m3u8|m4s)(\?|$)/i.test(path)) return false;
    // 常见质量短语
    if (/_(sd|hd|uhd|low|mid|high|max|min)\.(m3u8|m4s)(\?|$)/i.test(path)) return false;
    // 带 master 字样的明确是 master
    if (/[/_]master\.(m3u8|m4s)(\?|$)/i.test(path)) return true;
    // 文件名纯以 ID 命名（典型）/.../12345.m3u8
    const base = path.split("/").pop() || "";
    if (/^\d+\.m3u8$/i.test(base)) return true;
    // 文件名没有下划线（去掉 _xxxp 之类的尾巴后），就视为候选
    if (!/_/.test(base)) return true;
    // 有下划线但不含分辨率后缀，谨慎返回 true（如 index.m3u8）
    if (/^[a-z]+\d*\.m3u8$/i.test(base)) return true;
    return false;
  } catch {
    return false;
  }
}

// 排序时 master candidate 排前面，时序近的排前面
function masterFirst(a, b) {
  const am = isMasterCandidateM3U8(a.url) ? 0 : 1;
  const bm = isMasterCandidateM3U8(b.url) ? 0 : 1;
  if (am !== bm) return am - bm;
  return (b.time || 0) - (a.time || 0);
}

// ============================================================
// 通过本地 Go HTTP 服务启动下载
// 流程：ping /health → 调 /pickdir 弹 Windows 原生文件夹框（返回绝对路径）
// → 调 /download?mode=disk&dir=<路径> 让 Go 异步把视频直接落盘到该目录
// → 扩展轮询 /status 拿真实进度（阶段 / 分片数 / 完成路径）
// ============================================================

// 扩展设置（downloader.html 设置卡片里可改，存 chrome.storage.local）：
//   exePath —— go-catcher.exe 本机路径，兜底"复制命令到终端"用；默认裸文件名（要求在 PATH）
//   port    —— 本地服务端口，与 GoCatcher 客户端「设置」里的端口保持一致
// 缓存 + onChanged 失效：service worker 随时可能被回收，重启后首次读取重建缓存
let settingsCache = null;
async function getSettings() {
  if (!settingsCache) {
    settingsCache = await chrome.storage.local.get({ exePath: "go-catcher.exe", port: 7891, detectedExePath: "" });
  }
  return settingsCache;
}
chrome.storage.onChanged.addListener((changes, area) => {
  if (area === "local") settingsCache = null;
});
async function serverBase() {
  const s = await getSettings();
  return `http://127.0.0.1:${s.port}`;
}

async function pingServer(timeoutMs = 2000) {
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeoutMs);
    const resp = await fetch(`${await serverBase()}/health`, { signal: ctrl.signal });
    clearTimeout(timer);
    if (!resp.ok) return false;
    const text = (await resp.text()).trim();
    if (text === "ok") {
      detectExePath(); // fire-and-forget：服务活着就顺手记下本机 exe 路径
      return true;
    }
    return false;
  } catch {
    return false;
  }
}

// 从 /svc/info 记录服务自报的 exe 绝对路径。
// 兜底"复制命令到终端"需要完整路径，但每台机器路径不同不能写死；
// 服务跑过一次这里就缓存下来，content.js 生成兜底命令时直接用（用户零配置）。
async function detectExePath() {
  try {
    const resp = await fetch(`${await serverBase()}/svc/info`, { cache: "no-store" });
    if (!resp.ok) return;
    const info = await resp.json().catch(() => null);
    if (info && info.exe) await chrome.storage.local.set({ detectedExePath: info.exe });
  } catch {}
}

async function downloadViaServer({ m3u8Url, referer, title, filename } = {}) {
  if (!m3u8Url) return { ok: false, error: "缺少 m3u8Url" };

  const healthy = await pingServer();
  if (!healthy) {
    return {
      ok: false,
      error: "本地下载服务未启动。请打开 go-catcher.exe（GoCatcher 客户端，打开即自动启动下载服务），然后重试。",
      code: "SERVER_DOWN",
    };
  }

  try {
    // 1) 弹 Windows 原生"选择文件夹"框（由本地 Go 服务弹出，返回绝对路径）
    const pkRes = await fetch(`${await serverBase()}/pickdir`, {
      method: "GET",
      cache: "no-store",
    });
    if (!pkRes.ok) {
      return { ok: false, error: `目录选择失败 (HTTP ${pkRes.status})` };
    }
    const pk = await pkRes.json().catch(() => null);
    if (!pk || pk.cancelled) {
      return { ok: false, error: "用户取消了文件夹选择", code: "USER_CANCEL" };
    }
    const dir = pk.dir;
    if (!dir) {
      return { ok: false, error: "未获取到有效目录路径" };
    }

    // 2) 请求 Go 直接落盘到所选目录（异步：立即返回，扩展轮询 /status）
    const params = new URLSearchParams({ m3u8: m3u8Url, mode: "disk", dir });
    if (referer) params.set("referer", referer);
    const safeFn = filename || `${title || "video"}.ts`;
    params.set("filename", safeFn);

    const dlRes = await fetch(`${await serverBase()}/download?${params.toString()}`, {
      method: "GET",
      cache: "no-store",
    });
    if (!dlRes.ok) {
      const txt = await dlRes.text().catch(() => "");
      return { ok: false, error: `下载启动失败 (HTTP ${dlRes.status})：${txt}` };
    }
    const dl = await dlRes.json().catch(() => ({}));

    // 多任务并发：server 返回本次任务的 id，扩展用它轮询该任务进度
    return { ok: true, dir, filename: safeFn, started: !!(dl && dl.started), taskId: dl && dl.id };
  } catch (e) {
    if (/canceled|cancelled|user.*cancel/i.test(String(e && e.message))) {
      return { ok: false, error: "用户取消了文件夹选择", code: "USER_CANCEL" };
    }
    return { ok: false, error: String(e && e.message ? e.message : e) };
  }
}

async function queryDownload(msg) {
  // disk 模式：查 Go server 的任务状态。带 taskId 查指定任务；无 taskId 则取最新一个。
  try {
    let t = null;
    const taskId = msg && msg.taskId;
    if (taskId) {
      // 多任务：按任务 id 精确查
      const resp = await fetch(`${await serverBase()}/status?id=${encodeURIComponent(taskId)}`, { cache: "no-store" });
      if (!resp.ok) return { exists: false, notFound: resp.status === 404, error: `status HTTP ${resp.status}` };
      t = await resp.json().catch(() => null);
    } else {
      // 兜底：无 id（旧调用方）→ 取列表里最新一个（优先运行中/排队，否则最近完成）
      const resp = await fetch(`${await serverBase()}/status`, { cache: "no-store" });
      if (!resp.ok) return { exists: false, error: `status HTTP ${resp.status}` };
      const d = await resp.json().catch(() => null);
      if (d && Array.isArray(d.tasks) && d.tasks.length > 0) {
        const active = d.tasks.find((x) => x.queued || x.running) || d.tasks[d.tasks.length - 1];
        t = active;
      }
    }
    if (!t) return { exists: false };
    // 归一化字段，供 content.js 统一处理
    if (t.done) {
      if (t.finalPath) {
        return {
          exists: true,
          id: t.id,
          state: "complete",
          finalPath: t.finalPath,
          filename: t.finalPath ? t.finalPath.split(/[\\/]/).pop() : "",
          pct: 100,
          stage: t.stage || "已保存",
        };
      }
      if (t.canceled) {
        return {
          exists: true,
          id: t.id,
          state: "canceled",
          stage: t.stage || "已取消",
        };
      }
      return {
        exists: true,
        id: t.id,
        state: "error",
        error: t.error || "下载失败",
        stage: t.stage || "失败",
      };
    }
    return {
      exists: true,
      id: t.id,
      state: t.paused ? "paused" : "inProgress",
      stage: t.queued ? "排队中" : (t.paused ? "已暂停" : (t.stage || "下载中")),
      pct: t.pct || 0,
      segDone: t.segDone || 0,
      segTot: t.segTot || 0,
      queued: !!t.queued,
      running: !!t.running,
      paused: !!t.paused,
      canceled: !!t.canceled,
    };
  } catch (e) {
    return { exists: false, error: String(e) };
  }
}

// 对 Go server 上某任务执行控制动作：pause / resume / cancel
// action 直接作为 URL 路径段，id 作为查询参数（与 server 路由一致）
async function controlTask({ taskId, action } = {}) {
  if (!taskId) return { ok: false, error: "缺少 taskId" };
  const actionMap = { pause: "pause", resume: "resume", cancel: "cancel" };
  const path = actionMap[action];
  if (!path) return { ok: false, error: `未知动作: ${action}` };

  const healthy = await pingServer();
  if (!healthy) {
    return { ok: false, error: "本地 Go 下载服务未启动", code: "SERVER_DOWN" };
  }
  try {
    const resp = await fetch(
      `${await serverBase()}/${path}?id=${encodeURIComponent(taskId)}`,
      { cache: "no-store" }
    );
    const body = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      return { ok: false, error: body && body.error ? body.error : `HTTP ${resp.status}`, notFound: resp.status === 404 };
    }
    // 成功：返回 server 回传的状态字段（含新 stage/paused/canceled/done）
    return { ok: true, id: taskId, action, ...(body || {}) };
  } catch (e) {
    return { ok: false, error: String(e) };
  }
}

// SW 每次唤醒（页面导航/webRequest/消息都会唤醒）都探测一次 exe 路径。
// 兜底命令可能在从未发起过下载时出现（如"视频未嗅探到"错误面板），
// 不能只依赖下载流程里 pingServer 的顺带探测。
detectExePath();
