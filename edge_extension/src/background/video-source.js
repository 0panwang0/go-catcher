// 「这个视频」的候选与选项解析：
//   getVideoSources —— 全部候选链接（不解析，供 content.js 做 IDM 式列表）
//   getVideoSource  —— 悬停门控的单选命中（按钮显隐用）
//   getVideoOptions —— 下载器页面：当前页全部可下载项（各档画质展开）
import { pageCandidates, findSniffedByURL } from "./page-match.js";
import {
  fetchText,
  parseDuration,
  parseSegments,
  parseVariants,
  shortQuality,
  qualityFromURL,
  isMasterCandidateM3U8,
} from "./m3u8-parse.js";

// 解析当前页面所有可下载项（TS 各档 + MP4 直链）
export async function getVideoOptions(tab) {
  if (!tab) throw new Error("无法获取当前标签页");
  const pageUrl = tab.url || "";
  const title = tab.title || "";

  const { m3u8_list = [], mp4_list = [] } = await chrome.storage.local.get([
    "m3u8_list",
    "mp4_list",
  ]);

  // 过滤属于当前页的媒体（顶层页或发起 frame 任一同域；并自愈过滤历史误录
  // 的解析页假链接）
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

export function openDownloader(item) {
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

// getVideoSources 返回"该视频"的全部候选链接（不挑选、不解析），
// 供 content.js 做 IDM 式链接列表（解析放页面主世界，避免扩展 Origin 403）。
export async function getVideoSources({ src = "", pageUrl = "", embedUrl = "" }) {
  const { hostM3U8, hostMP4 } = await pageCandidates(pageUrl);
  const sources = [];
  if (/^https?:/i.test(src)) {
    sources.push({ type: "mp4", url: src, title: "", pageUrl, fromSrc: true });
  }
  // 解析页 iframe 的 ?url= 目标优先（同 getVideoSource 第 0 步的理由）
  if (embedUrl) {
    const hit = await findSniffedByURL(embedUrl);
    if (hit) {
      sources.push({
        type: hit.type === "mp4" ? "mp4" : "ts",
        url: hit.url,
        title: hit.title || "",
        pageUrl: hit.pageUrl || pageUrl,
        size: hit.size || 0,
      });
    }
  }
  for (const it of hostM3U8) {
    sources.push({
      type: "ts",
      url: it.url,
      title: it.title || "",
      pageUrl: it.pageUrl || pageUrl,
    });
  }
  for (const it of hostMP4) {
    sources.push({
      type: "mp4",
      url: it.url,
      title: it.title || "",
      pageUrl: it.pageUrl || pageUrl,
      size: it.size || 0,
    });
  }
  // src 直链可能与嗅探到的 mp4 重复
  return sources.filter((s, i, arr) => arr.findIndex((o) => o.url === s.url) === i);
}

export async function getVideoSource({ src = "", pageUrl = "", embedUrl = "", segmentDir = "", title = "" }) {
  // 0. 解析页 iframe：src 形如 https://parser/play/?url=<目标地址>，把 ?url= 里
  //    的目标地址直接拿去命中嗅探记录。解析页常在跳转到别的主机后才真正拉流，
  //    此时 frameUrl 同站匹配会全部落空（按钮永远不出现），只有这个是可靠的锚点。
  if (embedUrl) {
    const hit = await findSniffedByURL(embedUrl);
    if (hit) return makeSourceFor(hit, title, pageUrl);
  }

  const { hostM3U8, hostMP4 } = await pageCandidates(pageUrl);

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

// makeSourceFor 按嗅探记录自身的类型（m3u8 / mp4）产出统一的 source 结构
function makeSourceFor(item, title, pageUrl) {
  return item && item.type === "mp4"
    ? makeMP4Source(item, title, pageUrl)
    : makeM3U8Source(item, title, pageUrl);
}
