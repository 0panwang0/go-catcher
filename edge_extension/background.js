var __m3u8catcher = (() => {
  var __defProp = Object.defineProperty;
  var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
  var __getOwnPropNames = Object.getOwnPropertyNames;
  var __hasOwnProp = Object.prototype.hasOwnProperty;
  var __export = (target, all) => {
    for (var name in all)
      __defProp(target, name, { get: all[name], enumerable: true });
  };
  var __copyProps = (to, from, except, desc) => {
    if (from && typeof from === "object" || typeof from === "function") {
      for (let key of __getOwnPropNames(from))
        if (!__hasOwnProp.call(to, key) && key !== except)
          __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
    }
    return to;
  };
  var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);

  // src/background/main.js
  var main_exports = {};
  __export(main_exports, {
    __test__: () => __test__
  });

  // src/background/constants.js
  var MAX_SNIFFED = 30;
  var MAX_SNIFF_AGE_MS = 7 * 24 * 60 * 60 * 1e3;

  // src/background/list-store.js
  var storageWriteChain = Promise.resolve();
  function withListLock(fn) {
    const run = storageWriteChain.then(fn, fn);
    storageWriteChain = run.catch(() => {
    });
    return run;
  }
  function pickEvictionIndex(list, currentTabId) {
    let pick = -1;
    let pickRank = -1;
    let pickTime = Infinity;
    for (let i = 0; i < list.length; i++) {
      const it = list[i];
      const rank = (it.analyzed ? 2 : 0) + (currentTabId != null && it.tabId !== currentTabId ? 1 : 0);
      const t = it.time || 0;
      if (rank > pickRank || rank === pickRank && t <= pickTime) {
        pick = i;
        pickRank = rank;
        pickTime = t;
      }
    }
    return pick;
  }
  function pruneExpired(list) {
    const now = Date.now();
    return list.filter((it) => !it.time || now - it.time < MAX_SNIFF_AGE_MS);
  }
  function recordMedia(url, pageUrl, frameUrl, title, type, size = 0, tabId = null) {
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
          tabId
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
  function updateMediaItem(key, url, patch) {
    return withListLock(async () => {
      const { [key]: list = [] } = await chrome.storage.local.get(key);
      const cur = list.find((it) => it.url === url);
      if (!cur) return { ok: false, error: "\u8BB0\u5F55\u5DF2\u4E0D\u5B58\u5728" };
      Object.assign(cur, patch || {});
      await chrome.storage.local.set({ [key]: list });
      return { ok: true };
    });
  }
  function clearLists() {
    return withListLock(() => chrome.storage.local.set({ m3u8_list: [], mp4_list: [] }));
  }

  // src/background/sniff-probes.js
  var SNIFF_PROBES = [
    {
      name: "webRequest-m3u8",
      install() {
        chrome.webRequest.onCompleted.addListener(
          (details) => {
            if (details.tabId < 0 || !shouldSniff(details, ".m3u8")) return;
            withTab(details, (tab) => {
              recordMedia(details.url, tab.url || "", details.documentUrl || "", tab.title || "", "m3u8", 0, details.tabId);
            });
          },
          { urls: ["*://*/*.m3u8", "*://*/*.m3u8?*"] }
        );
      }
    },
    {
      name: "webRequest-mp4",
      // MP4 直链（IDM 也常提供 MP4 格式选项）
      install() {
        chrome.webRequest.onCompleted.addListener(
          (details) => {
            if (details.tabId < 0 || !shouldSniff(details, ".mp4")) return;
            withTab(details, (tab) => {
              recordMedia(details.url, tab.url || "", details.documentUrl || "", tab.title || "", "mp4", contentLengthOf(details), details.tabId);
            });
          },
          { urls: ["*://*/*.mp4", "*://*/*.mp4?*"] },
          ["responseHeaders"]
        );
      }
    },
    {
      name: "webRequest-contenttype",
      // 无扩展名流的兜底通道（见文件头注释）
      install() {
        chrome.webRequest.onCompleted.addListener(
          (details) => {
            if (details.tabId < 0) return;
            if (details.type === "main_frame" || details.type === "sub_frame") return;
            const type = contentTypeMediaType(details);
            if (!type) return;
            if (type === "mp4" && !isLikelyFullFile(details)) return;
            withTab(details, (tab) => {
              recordMedia(details.url, tab.url || "", details.documentUrl || "", tab.title || "", type, contentLengthOf(details), details.tabId);
            });
          },
          { urls: ["*://*/*"] },
          // 只要响应头。请求头在 onCompleted 拿不到（见文件头 ⚠ 说明），
          // Range 改由 statusCode === 206 / Content-Range 识别。
          ["responseHeaders"]
        );
      }
    }
  ];
  function withTab(details, fn) {
    chrome.tabs.get(details.tabId, (tab) => {
      if (chrome.runtime.lastError) return;
      fn(tab);
    });
  }
  function installSniffProbes() {
    for (const p of SNIFF_PROBES) {
      try {
        p.install();
      } catch (e) {
        console.error("\u55C5\u63A2\u63A2\u6D4B\u5668\u5B89\u88C5\u5931\u8D25:", p.name, e);
      }
    }
  }
  function shouldSniff(details, ext) {
    if (details.type === "main_frame" || details.type === "sub_frame") return false;
    try {
      return new URL(details.url).pathname.toLowerCase().endsWith(ext);
    } catch {
      return false;
    }
  }
  function headerValueOf(details, name) {
    if (!details.responseHeaders) return "";
    const h = details.responseHeaders.find((x) => x.name.toLowerCase() === name.toLowerCase());
    return h ? h.value || "" : "";
  }
  function contentLengthOf(details) {
    const v = parseInt(headerValueOf(details, "content-length"), 10);
    return Number.isFinite(v) && v > 0 ? v : 0;
  }
  var playlistCTRe = /^(application\/(vnd\.apple\.mpegurl|x-mpegurl|mpegurl)|audio\/mpegurl)\b/i;
  function contentTypeMediaType(details) {
    const ct = headerValueOf(details, "content-type").split(";")[0].trim();
    if (!ct) return null;
    if (playlistCTRe.test(ct)) return "m3u8";
    if (/^video\/mp4\b/i.test(ct)) return "mp4";
    return null;
  }
  function isLikelyFullFile(details) {
    if (details.statusCode === 206) return false;
    if (headerValueOf(details, "content-range")) return false;
    return contentLengthOf(details) > 1024 * 1024;
  }

  // src/background/m3u8-parse.js
  function sleep(ms) {
    return new Promise((r) => setTimeout(r, ms));
  }
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
        label: variantLabel(res, bw, next)
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
      q = {
        2160: "4K",
        1440: "2K",
        1080: "1080P",
        720: "720P",
        480: "480P",
        360: "360P",
        240: "240P"
      }[h] || resolution;
    } else if (bandwidth) {
      q = (bandwidth / 1e6).toFixed(1) + " Mbps";
    }
    const parts = [q || "\u672A\u77E5\u753B\u8D28"];
    if (resolution) parts.push(resolution);
    if (bandwidth) parts.push(`${(bandwidth / 1e3).toFixed(0)} kbps`);
    return parts.join(" \xB7 ");
  }
  function shortQuality(variant) {
    return String(variant.label || "").split(" \xB7 ")[0] || "";
  }
  function qualityFromURL(u) {
    try {
      const m = decodeURIComponent(new URL(u).pathname).match(
        /(2160p|1440p|1080p|720p|480p|360p|240p)/i
      );
      if (m) return m[1].toUpperCase();
      const q = new URL(u).searchParams.get("quality");
      if (q) return q;
    } catch {
    }
    return "";
  }
  function isMasterCandidateM3U8(url) {
    try {
      const path = new URL(url).pathname;
      if (!/\.m3u8$/i.test(path)) return false;
      if (/_\d{3,4}p\.(m3u8|m4s)(\?|$)/i.test(path)) return false;
      if (/_(sd|hd|uhd|low|mid|high|max|min)\.(m3u8|m4s)(\?|$)/i.test(path)) return false;
      if (/[/_]master\.(m3u8|m4s)(\?|$)/i.test(path)) return true;
      const base = path.split("/").pop() || "";
      if (/^\d+\.m3u8$/i.test(base)) return true;
      if (!/_/.test(base)) return true;
      if (/^[a-z]+\d*\.m3u8$/i.test(base)) return true;
      return false;
    } catch {
      return false;
    }
  }
  function masterFirst(a, b) {
    const am = isMasterCandidateM3U8(a.url) ? 0 : 1;
    const bm = isMasterCandidateM3U8(b.url) ? 0 : 1;
    if (am !== bm) return am - bm;
    return (b.time || 0) - (a.time || 0);
  }

  // src/background/page-match.js
  function hostOf(u) {
    try {
      return new URL(u).hostname;
    } catch {
      return "";
    }
  }
  function sameSite2(it, pageUrl) {
    if (!it.pageUrl || !pageUrl) return false;
    const h = hostOf(pageUrl);
    if (!h) return false;
    return hostOf(it.pageUrl) === h || hostOf(it.frameUrl) === h;
  }
  function isCandidateURL2(u) {
    try {
      const p = new URL(u);
      return /\.(m3u8|mp4)$/i.test(p.pathname) || !p.searchParams.has("url");
    } catch {
      return false;
    }
  }
  async function hostCandidates(pageUrl) {
    const { m3u8_list = [], mp4_list = [] } = await chrome.storage.local.get([
      "m3u8_list",
      "mp4_list"
    ]);
    const hostM3U8 = m3u8_list.filter((it) => sameSite2(it, pageUrl) && isCandidateURL2(it.url));
    const hostMP4 = mp4_list.filter((it) => sameSite2(it, pageUrl) && isCandidateURL2(it.url));
    hostM3U8.sort(masterFirst);
    hostMP4.sort(masterFirst);
    return { hostM3U8, hostMP4 };
  }
  async function pageCandidates(pageUrl) {
    const { hostM3U8, hostMP4 } = await hostCandidates(pageUrl);
    const byFrame = (it) => !!it.frameUrl && it.frameUrl === pageUrl;
    const frameM3U8 = hostM3U8.filter(byFrame);
    const frameMP4 = hostMP4.filter(byFrame);
    if (frameM3U8.length || frameMP4.length) {
      return { hostM3U8: frameM3U8, hostMP4: frameMP4 };
    }
    const samePath = (it) => {
      if (!it.pageUrl || !pageUrl) return false;
      try {
        return new URL(it.pageUrl).pathname === new URL(pageUrl).pathname;
      } catch {
        return false;
      }
    };
    const pageM3U8 = hostM3U8.filter(samePath);
    const pageMP4 = hostMP4.filter(samePath);
    if (pageM3U8.length || pageMP4.length) {
      return { hostM3U8: pageM3U8, hostMP4: pageMP4 };
    }
    return { hostM3U8, hostMP4 };
  }
  async function findSniffedByURL(target) {
    if (!target) return null;
    const { m3u8_list = [], mp4_list = [] } = await chrome.storage.local.get([
      "m3u8_list",
      "mp4_list"
    ]);
    const all = [...m3u8_list, ...mp4_list].filter((it) => isCandidateURL2(it.url));
    const norm = (u) => {
      try {
        const p = new URL(u);
        return p.origin + p.pathname;
      } catch {
        return u;
      }
    };
    const key = norm(target);
    const exact = all.find((it) => norm(it.url) === key);
    if (exact) return exact;
    const dir = key.replace(/[^/]*$/, "");
    if (dir.length > 24) {
      const sameDir = all.find((it) => norm(it.url).startsWith(dir));
      if (sameDir) return sameDir;
    }
    return null;
  }

  // src/background/video-source.js
  async function getVideoOptions(tab) {
    if (!tab) throw new Error("\u65E0\u6CD5\u83B7\u53D6\u5F53\u524D\u6807\u7B7E\u9875");
    const pageUrl = tab.url || "";
    const title = tab.title || "";
    const { m3u8_list = [], mp4_list = [] } = await chrome.storage.local.get([
      "m3u8_list",
      "mp4_list"
    ]);
    const m3u8Items = m3u8_list.filter((it) => sameSite(it, pageUrl) && isCandidateURL(it.url));
    const mp4Items = mp4_list.filter((it) => sameSite(it, pageUrl) && isCandidateURL(it.url));
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
              label: v.label
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
            label: "\u672A\u77E5\u753B\u8D28"
          });
        }
      } catch (e) {
        console.error("\u89E3\u6790 m3u8 \u5931\u8D25:", it.url, e);
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
        label: it.size ? `${(it.size / 1024 / 1024).toFixed(1)} MB` : ""
      });
    }
    const MAIN_VIDEO_MIN = 60;
    const filtered = options.filter(
      (o) => o.type === "mp4" || o.duration >= MAIN_VIDEO_MIN
    );
    const qualityRank = (q) => ({
      "4K": 8,
      "2K": 7,
      "1080P": 6,
      "720P": 5,
      "480P": 4,
      "360P": 3,
      "240P": 2,
      "\u672A\u77E5\u753B\u8D28": 1,
      "": 0
    })[String(q).toUpperCase()] || 0;
    filtered.sort((a, b) => {
      if (a.type !== b.type) return a.type === "ts" ? -1 : 1;
      const rankDiff = qualityRank(a.quality) - qualityRank(b.quality);
      if (rankDiff !== 0) return -rankDiff;
      return b.bandwidth - a.bandwidth;
    });
    filtered.forEach((o, i) => o.id = i + 1);
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
      url: chrome.runtime.getURL("downloader.html?" + params.toString())
    });
  }
  async function getVideoSources({ src = "", pageUrl = "", embedUrl = "" }) {
    const { hostM3U8, hostMP4 } = await pageCandidates(pageUrl);
    const sources = [];
    if (/^https?:/i.test(src)) {
      sources.push({ type: "mp4", url: src, title: "", pageUrl, fromSrc: true });
    }
    if (embedUrl) {
      const hit = await findSniffedByURL(embedUrl);
      if (hit) {
        sources.push({
          type: hit.type === "mp4" ? "mp4" : "ts",
          url: hit.url,
          title: hit.title || "",
          pageUrl: hit.pageUrl || pageUrl,
          size: hit.size || 0
        });
      }
    }
    for (const it of hostM3U8) {
      sources.push({
        type: "ts",
        url: it.url,
        title: it.title || "",
        pageUrl: it.pageUrl || pageUrl
      });
    }
    for (const it of hostMP4) {
      sources.push({
        type: "mp4",
        url: it.url,
        title: it.title || "",
        pageUrl: it.pageUrl || pageUrl,
        size: it.size || 0
      });
    }
    return sources.filter((s, i, arr) => arr.findIndex((o) => o.url === s.url) === i);
  }
  async function getVideoSource({ src = "", pageUrl = "", embedUrl = "", segmentDir = "", title = "" }) {
    if (embedUrl) {
      const hit = await findSniffedByURL(embedUrl);
      if (hit) return makeSourceFor(hit, title, pageUrl);
    }
    const { hostM3U8, hostMP4 } = await pageCandidates(pageUrl);
    if (/^https?:/i.test(src)) {
      const matched = hostMP4.find((it) => it.url === src);
      if (matched) {
        return makeMP4Source(matched, title, pageUrl);
      }
      return {
        type: "mp4",
        url: src,
        title,
        pageUrl,
        quality: qualityFromURL(src)
      };
    }
    if (segmentDir && hostM3U8.length) {
      const byDir = pickBestByDir(hostM3U8, segmentDir);
      if (byDir) return makeM3U8Source(byDir, title, pageUrl);
    }
    if (hostM3U8.length) {
      return makeM3U8Source(hostM3U8[0], title, pageUrl);
    }
    const realMP4 = hostMP4.filter((it) => (it.size || 0) > 1024 * 1024).sort((a, b) => (b.size || 0) - (a.size || 0));
    if (realMP4.length) {
      return makeMP4Source(realMP4[0], title, pageUrl);
    }
    throw new Error("\u6682\u672A\u55C5\u63A2\u5230\u8BE5\u89C6\u9891\uFF0C\u8BF7\u5148\u64AD\u653E\u51E0\u79D2\u518D\u8BD5\u3002");
  }
  function pickBestByDir(list, segmentDir) {
    const inDir = list.filter((it) => it.url.startsWith(segmentDir));
    if (!inDir.length) return null;
    const master = inDir.find((it) => isMasterCandidateM3U8(it.url));
    return master || inDir[0];
  }
  function makeM3U8Source(item, title, pageUrl) {
    return {
      type: "ts",
      url: item.url,
      title: item.title || title,
      pageUrl: item.pageUrl || pageUrl,
      quality: qualityFromURL(item.url)
    };
  }
  function makeMP4Source(item, title, pageUrl) {
    return {
      type: "mp4",
      url: item.url,
      title: item.title || title,
      pageUrl: item.pageUrl || pageUrl,
      quality: qualityFromURL(item.url),
      size: item.size || 0
    };
  }
  function makeSourceFor(item, title, pageUrl) {
    return item && item.type === "mp4" ? makeMP4Source(item, title, pageUrl) : makeM3U8Source(item, title, pageUrl);
  }

  // src/background/server-api.js
  var settingsCache = null;
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
  var apiTokenCache = null;
  async function getApiToken(force = false) {
    if (!force) {
      if (apiTokenCache) return apiTokenCache;
      const s = await chrome.storage.local.get({ apiToken: "" });
      if (s.apiToken) {
        apiTokenCache = s.apiToken;
        return apiTokenCache;
      }
    }
    try {
      const resp = await fetch(`${await serverBase()}/svc/info`, { cache: "no-store" });
      if (resp.ok) {
        const info = await resp.json().catch(() => null);
        if (info && info.token) {
          apiTokenCache = info.token;
          await chrome.storage.local.set({ apiToken: info.token });
          if (info.exe) await chrome.storage.local.set({ detectedExePath: info.exe });
          return apiTokenCache;
        }
      }
    } catch {
    }
    return apiTokenCache || "";
  }
  async function apiFetch(pathname, opts) {
    const call = async (tok2) => {
      const sep = pathname.includes("?") ? "&" : "?";
      const url = tok2 ? `${await serverBase()}${pathname}${sep}t=${encodeURIComponent(tok2)}` : `${await serverBase()}${pathname}`;
      return fetch(url, opts);
    };
    const tok = await getApiToken();
    let resp = await call(tok);
    if (resp.status === 401) {
      const fresh = await getApiToken(true);
      if (fresh && fresh !== tok) resp = await call(fresh);
    }
    return resp;
  }
  async function pingServer(timeoutMs = 2e3) {
    try {
      const ctrl = new AbortController();
      const timer = setTimeout(() => ctrl.abort(), timeoutMs);
      const resp = await fetch(`${await serverBase()}/health`, { signal: ctrl.signal });
      clearTimeout(timer);
      if (!resp.ok) return false;
      const text = (await resp.text()).trim();
      if (text === "ok") {
        detectExePath();
        return true;
      }
      return false;
    } catch {
      return false;
    }
  }
  async function detectExePath() {
    await getApiToken(true);
  }
  async function downloadViaServer({ m3u8Url, referer, title, filename } = {}) {
    if (!m3u8Url) return { ok: false, error: "\u7F3A\u5C11 m3u8Url" };
    const healthy = await pingServer();
    if (!healthy) {
      return {
        ok: false,
        error: "\u672C\u5730\u4E0B\u8F7D\u670D\u52A1\u672A\u542F\u52A8\u3002\u8BF7\u6253\u5F00 go-catcher.exe\uFF08GoCatcher \u5BA2\u6237\u7AEF\uFF0C\u6253\u5F00\u5373\u81EA\u52A8\u542F\u52A8\u4E0B\u8F7D\u670D\u52A1\uFF09\uFF0C\u7136\u540E\u91CD\u8BD5\u3002",
        code: "SERVER_DOWN"
      };
    }
    try {
      const pkRes = await apiFetch("/pickdir", {
        method: "GET",
        cache: "no-store"
      });
      if (!pkRes.ok) {
        return { ok: false, error: `\u76EE\u5F55\u9009\u62E9\u5931\u8D25 (HTTP ${pkRes.status})` };
      }
      const pk = await pkRes.json().catch(() => null);
      if (!pk || pk.cancelled) {
        return { ok: false, error: "\u7528\u6237\u53D6\u6D88\u4E86\u6587\u4EF6\u5939\u9009\u62E9", code: "USER_CANCEL" };
      }
      const dir = pk.dir;
      if (!dir) {
        return { ok: false, error: "\u672A\u83B7\u53D6\u5230\u6709\u6548\u76EE\u5F55\u8DEF\u5F84" };
      }
      const params = new URLSearchParams({ m3u8: m3u8Url, mode: "disk", dir });
      if (referer) params.set("referer", referer);
      const safeFn = filename || `${title || "video"}.ts`;
      params.set("filename", safeFn);
      const dlRes = await apiFetch(`/download?${params.toString()}`, {
        method: "GET",
        cache: "no-store"
      });
      if (!dlRes.ok) {
        const txt = await dlRes.text().catch(() => "");
        return { ok: false, error: `\u4E0B\u8F7D\u542F\u52A8\u5931\u8D25 (HTTP ${dlRes.status})\uFF1A${txt}` };
      }
      const dl = await dlRes.json().catch(() => ({}));
      return { ok: true, dir, filename: safeFn, started: !!(dl && dl.started), taskId: dl && dl.id };
    } catch (e) {
      if (/canceled|cancelled|user.*cancel/i.test(String(e && e.message))) {
        return { ok: false, error: "\u7528\u6237\u53D6\u6D88\u4E86\u6587\u4EF6\u5939\u9009\u62E9", code: "USER_CANCEL" };
      }
      return { ok: false, error: String(e && e.message ? e.message : e) };
    }
  }
  async function queryDownload(msg) {
    try {
      let t = null;
      const taskId = msg && msg.taskId;
      if (taskId) {
        const resp = await apiFetch(`/status?id=${encodeURIComponent(taskId)}`, { cache: "no-store" });
        if (!resp.ok) return { exists: false, notFound: resp.status === 404, error: `status HTTP ${resp.status}` };
        t = await resp.json().catch(() => null);
      } else {
        const resp = await apiFetch("/status", { cache: "no-store" });
        if (!resp.ok) return { exists: false, error: `status HTTP ${resp.status}` };
        const d = await resp.json().catch(() => null);
        if (d && Array.isArray(d.tasks) && d.tasks.length > 0) {
          const active = d.tasks.find((x) => x.queued || x.running) || d.tasks[d.tasks.length - 1];
          t = active;
        }
      }
      if (!t) return { exists: false };
      if (t.done) {
        if (t.finalPath) {
          return {
            exists: true,
            id: t.id,
            state: "complete",
            finalPath: t.finalPath,
            filename: t.finalPath ? t.finalPath.split(/[\\/]/).pop() : "",
            pct: 100,
            stage: t.stage || "\u5DF2\u4FDD\u5B58"
          };
        }
        if (t.canceled) {
          return {
            exists: true,
            id: t.id,
            state: "canceled",
            stage: t.stage || "\u5DF2\u53D6\u6D88"
          };
        }
        return {
          exists: true,
          id: t.id,
          state: "error",
          error: t.error || "\u4E0B\u8F7D\u5931\u8D25",
          stage: t.stage || "\u5931\u8D25"
        };
      }
      return {
        exists: true,
        id: t.id,
        state: t.paused ? "paused" : "inProgress",
        stage: t.queued ? "\u6392\u961F\u4E2D" : t.paused ? "\u5DF2\u6682\u505C" : t.stage || "\u4E0B\u8F7D\u4E2D",
        pct: t.pct || 0,
        segDone: t.segDone || 0,
        segTot: t.segTot || 0,
        queued: !!t.queued,
        running: !!t.running,
        paused: !!t.paused,
        canceled: !!t.canceled
      };
    } catch (e) {
      return { exists: false, error: String(e) };
    }
  }
  async function controlTask({ taskId, action } = {}) {
    if (!taskId) return { ok: false, error: "\u7F3A\u5C11 taskId" };
    const actionMap = { pause: "pause", resume: "resume", cancel: "cancel" };
    const path = actionMap[action];
    if (!path) return { ok: false, error: `\u672A\u77E5\u52A8\u4F5C: ${action}` };
    const healthy = await pingServer();
    if (!healthy) {
      return { ok: false, error: "\u672C\u5730 Go \u4E0B\u8F7D\u670D\u52A1\u672A\u542F\u52A8", code: "SERVER_DOWN" };
    }
    try {
      const resp = await apiFetch(
        `/${path}?id=${encodeURIComponent(taskId)}`,
        { cache: "no-store" }
      );
      const body = await resp.json().catch(() => ({}));
      if (!resp.ok) {
        return { ok: false, error: body && body.error ? body.error : `HTTP ${resp.status}`, notFound: resp.status === 404 };
      }
      return { ok: true, id: taskId, action, ...body || {} };
    } catch (e) {
      return { ok: false, error: String(e) };
    }
  }

  // src/background/referer-rules.js
  async function setRefererRules(tabId, pairs) {
    const existing = await chrome.declarativeNetRequest.getDynamicRules();
    const ids = existing.filter((r) => r.id >= 1e3 && r.id < 1e4).map((r) => r.id);
    if (ids.length) {
      await chrome.declarativeNetRequest.updateDynamicRules({
        removeRuleIds: ids
      });
    }
    const seen = /* @__PURE__ */ new Set();
    const rules = [];
    let id = 1e3;
    for (const { host, referer } of pairs) {
      if (!host || !referer) continue;
      const key = `${host}|${referer}`;
      if (seen.has(key)) continue;
      seen.add(key);
      let origin = "";
      try {
        origin = new URL(referer).origin;
      } catch {
      }
      if (!origin) continue;
      rules.push({
        id: id++,
        priority: 1,
        action: {
          type: "modifyHeaders",
          requestHeaders: [
            { header: "Referer", operation: "set", value: referer },
            { header: "Origin", operation: "set", value: origin }
          ]
        },
        condition: {
          urlFilter: `||${host}`,
          resourceTypes: ["xmlhttprequest"],
          tabIds: [tabId]
        }
      });
    }
    if (rules.length) {
      await chrome.declarativeNetRequest.updateDynamicRules({
        addRules: rules
      });
    }
  }

  // src/background/main.js
  chrome.action.onClicked.addListener(() => {
    chrome.tabs.create({ url: chrome.runtime.getURL("downloader.html") });
  });
  chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
    if (msg.type === "setReferers") {
      setRefererRules(msg.tabId, msg.pairs).then(() => sendResponse({ ok: true })).catch((e) => sendResponse({ ok: false, error: String(e) }));
      return true;
    }
    if (msg.type === "getVideoOptions") {
      getVideoOptions(sender.tab).then((res) => sendResponse({ ok: true, options: res })).catch((e) => sendResponse({ ok: false, error: String(e) }));
      return true;
    }
    if (msg.type === "getVideoSource") {
      getVideoSource(msg).then((res) => sendResponse({ ok: true, source: res })).catch((e) => sendResponse({ ok: false, error: String(e && e.message ? e.message : e) }));
      return true;
    }
    if (msg.type === "getVideoSources") {
      getVideoSources(msg).then((res) => sendResponse({ ok: true, sources: res })).catch((e) => sendResponse({ ok: false, error: String(e && e.message ? e.message : e) }));
      return true;
    }
    if (msg.type === "openDownloader") {
      openDownloader(msg.item);
      sendResponse({ ok: true });
      return true;
    }
    if (msg.type === "clearList") {
      clearLists().then(() => {
        chrome.action.setBadgeText({ text: "" });
        sendResponse({ ok: true });
      }).catch((e) => sendResponse({ ok: false, error: String(e) }));
      return true;
    }
    if (msg.type === "updateMediaItem") {
      updateMediaItem(msg.key, msg.url, msg.patch).then((res) => sendResponse(res)).catch((e) => sendResponse({ ok: false, error: String(e) }));
      return true;
    }
    if (msg.type === "downloadViaServer") {
      downloadViaServer(msg).then((res) => sendResponse(res)).catch((e) => sendResponse({ ok: false, error: String(e) }));
      return true;
    }
    if (msg.type === "queryDownload") {
      queryDownload(msg).then((res) => sendResponse(res)).catch((e) => sendResponse({ exists: false, error: String(e) }));
      return true;
    }
    if (msg.type === "controlTask") {
      controlTask(msg).then((res) => sendResponse(res)).catch((e) => sendResponse({ ok: false, error: String(e) }));
      return true;
    }
    if (msg.type === "getApiToken") {
      getApiToken().then((token) => sendResponse({ ok: !!token, token })).catch(() => sendResponse({ ok: false, token: "" }));
      return true;
    }
    if (msg.type === "getExePath") {
      detectExePath().then(async () => sendResponse({ ok: true, exePath: (await getSettings()).detectedExePath || "go-catcher.exe" })).catch(() => sendResponse({ ok: false, exePath: "go-catcher.exe" }));
      return true;
    }
  });
  installSniffProbes();
  detectExePath();
  var __test__ = {
    recordMedia,
    updateMediaItem,
    pickEvictionIndex,
    pruneExpired,
    clearLists,
    findSniffedByURL,
    getVideoSource,
    getVideoSources,
    isCandidateURL: isCandidateURL2,
    sameSite: sameSite2,
    // 探测器判据（纯函数，供 tests/probes.test.js 直接断言）
    contentTypeMediaType,
    isLikelyFullFile
  };
  return __toCommonJS(main_exports);
})();
