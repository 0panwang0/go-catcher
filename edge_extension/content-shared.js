// 自动生成，请勿手改。
// 源：src/background/ 下的共享模块（清单见 src/content-shared-entry.js 的 import）
// 重新生成：node build.mjs —— scripts/check.mjs 会校验本文件与源码是否同步
(() => {
  var __defProp = Object.defineProperty;
  var __export = (target, all) => {
    for (var name in all)
      __defProp(target, name, { get: all[name], enumerable: true });
  };

  // src/background/m3u8-parse.js
  var m3u8_parse_exports = {};
  __export(m3u8_parse_exports, {
    fetchText: () => fetchText,
    fmtDur: () => fmtDur,
    isMasterCandidateM3U8: () => isMasterCandidateM3U8,
    masterFirst: () => masterFirst,
    parseDuration: () => parseDuration,
    parseSegments: () => parseSegments,
    parseVariants: () => parseVariants,
    qualityFromURL: () => qualityFromURL,
    resolveURL: () => resolveURL,
    sanitizeQuality: () => sanitizeQuality,
    shortQuality: () => shortQuality,
    sleep: () => sleep,
    variantLabel: () => variantLabel,
    variantQuality: () => variantQuality
  });
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
  function resolveURL(base, rel) {
    try {
      return new URL(rel, base).href;
    } catch {
      return rel;
    }
  }
  function parseDuration(text) {
    let total = 0;
    for (const m of text.matchAll(/#EXTINF:([\d.]+)/g)) {
      total += parseFloat(m[1]);
    }
    return total;
  }
  function fmtDur(sec) {
    const h = Math.floor(sec / 3600);
    const m = Math.floor(sec % 3600 / 60);
    const s = Math.round(sec % 60);
    return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}` : `${m}:${String(s).padStart(2, "0")}`;
  }
  function parseSegments(text, baseURL) {
    const list = [];
    for (const raw of text.split("\n")) {
      const line = raw.trim();
      if (!line || line.startsWith("#")) continue;
      list.push(resolveURL(baseURL, line));
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
      const codec = (info.match(/CODECS="([^"]+)"/) || [])[1] || "";
      list.push({
        url: resolveURL(baseURL, next),
        bandwidth: bw,
        resolution: res,
        codec,
        quality: variantQuality(next, res, bw),
        label: variantLabel(next, res, bw)
      });
    }
    list.sort((a, b) => b.bandwidth - a.bandwidth);
    return list;
  }
  function variantQuality(uri, resolution, bandwidth) {
    const m = String(uri).match(/(2160p|1440p|1080p|720p|480p|360p|240p)/i);
    if (m) return m[1].toUpperCase();
    if (resolution) {
      const h = parseInt(resolution.split("x")[1], 10);
      const byHeight = {
        2160: "4K",
        1440: "2K",
        1080: "1080P",
        720: "720P",
        480: "480P",
        360: "360P",
        240: "240P"
      };
      if (byHeight[h]) return byHeight[h];
    }
    if (bandwidth) return `${(bandwidth / 1e6).toFixed(1)} Mbps`;
    return "";
  }
  function variantLabel(uri, resolution, bandwidth) {
    const q = variantQuality(uri, resolution, bandwidth);
    const parts = [q || "\u672A\u77E5\u753B\u8D28"];
    if (resolution) parts.push(resolution);
    if (bandwidth) parts.push(`${(bandwidth / 1e3).toFixed(0)} kbps`);
    return parts.join(" \xB7 ");
  }
  function shortQuality(variant) {
    return String(variant.label || "").split(" \xB7 ")[0] || "";
  }
  function sanitizeQuality(v) {
    const s = String(v ?? "");
    return /^[A-Za-z0-9._+\- ]{1,24}$/.test(s) ? s : "";
  }
  function qualityFromURL(u) {
    if (!u) return "";
    try {
      const p = new URL(u);
      const m = decodeURIComponent(p.pathname).match(
        /(2160p|1440p|1080p|720p|480p|360p|240p)/i
      );
      if (m) return m[1].toUpperCase();
      const q = sanitizeQuality(p.searchParams.get("quality"));
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

  // src/background/cli-args.js
  var cli_args_exports = {};
  __export(cli_args_exports, {
    assembleGoCommand: () => assembleGoCommand,
    quoteArg: () => quoteArg,
    sanitizeFileName: () => sanitizeFileName
  });
  var CONTROL_CHARS = /[\u0000-\u001f\u007f-\u009f]/g;
  function sanitizeFileName(name) {
    return String(name == null ? "" : name).replace(CONTROL_CHARS, "").replace(/[\\/:*?"<>|]/g, "_").replace(/_{2,}/g, "_").replace(/\s*_\s*/g, "_").replace(/\s+/g, " ").replace(/^[_\s]+|[_\s]+$/g, "").trim();
  }
  function quoteArg(v) {
    return String(v == null ? "" : v).replace(CONTROL_CHARS, "").replace(/"/g, "").trim();
  }
  function assembleGoCommand(m3u8URL, pageURL, outName, exePath) {
    const exe = quoteArg(exePath) || "go-catcher.exe";
    const args = [`"${exe}"`];
    if (m3u8URL) args.push(`--url="${quoteArg(m3u8URL)}"`);
    if (pageURL) args.push(`--referer="${quoteArg(pageURL)}"`);
    if (outName) args.push(`-o "${outName}"`);
    return ["chcp 65001", args.join(" ")].join(" ; ");
  }

  // src/background/media-url.js
  var media_url_exports = {};
  __export(media_url_exports, {
    isCandidateURL: () => isCandidateURL,
    isPlaylistURL: () => isPlaylistURL
  });
  function isCandidateURL(u) {
    try {
      const p = new URL(u);
      return /\.(m3u8|mp4)$/i.test(p.pathname) || !p.searchParams.has("url");
    } catch {
      return false;
    }
  }
  function isPlaylistURL(u) {
    try {
      return /\.m3u8$/i.test(new URL(u).pathname);
    } catch {
      return false;
    }
  }

  // src/background/html-escape.js
  var html_escape_exports = {};
  __export(html_escape_exports, {
    escapeHtml: () => escapeHtml
  });
  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({
      "&": "&amp;",
      "<": "&lt;",
      ">": "&gt;",
      '"': "&quot;",
      "'": "&#39;"
    })[c]);
  }

  // src/background/constants.js
  var constants_exports = {};
  __export(constants_exports, {
    MAX_SNIFFED: () => MAX_SNIFFED,
    MAX_SNIFF_AGE_MS: () => MAX_SNIFF_AGE_MS,
    POLL_MAX_MISSES: () => POLL_MAX_MISSES
  });
  var MAX_SNIFFED = 30;
  var MAX_SNIFF_AGE_MS = 7 * 24 * 60 * 60 * 1e3;
  var POLL_MAX_MISSES = 15;

  // src/content-shared-entry.js
  globalThis.__m3u8Shared = { ...m3u8_parse_exports, ...cli_args_exports, ...media_url_exports, ...html_escape_exports, ...constants_exports };
})();
