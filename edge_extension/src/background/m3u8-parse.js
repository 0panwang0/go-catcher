// m3u8 / 画质解析工具（downloader.js 侧保留一份等价实现：它是页面脚本，
// 与 SW 无法共享模块——改动两处时务必同步语义）。

export function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

export async function fetchText(url) {
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

export function parseDuration(text) {
  let total = 0;
  for (const m of text.matchAll(/#EXTINF:([\d.]+)/g)) {
    total += parseFloat(m[1]);
  }
  return total;
}

export function parseSegments(text, baseURL) {
  const list = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    list.push(new URL(line, baseURL).href);
  }
  return list;
}

export function parseVariants(text, baseURL) {
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

export function variantLabel(resolution, bandwidth, uri) {
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

export function shortQuality(variant) {
  return String(variant.label || "").split(" · ")[0] || "";
}

export function qualityFromURL(u) {
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
export function isMasterCandidateM3U8(url) {
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
export function masterFirst(a, b) {
  const am = isMasterCandidateM3U8(a.url) ? 0 : 1;
  const bm = isMasterCandidateM3U8(b.url) ? 0 : 1;
  if (am !== bm) return am - bm;
  return (b.time || 0) - (a.time || 0);
}
