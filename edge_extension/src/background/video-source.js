// 「这个视频」的候选与选项解析：
//   getVideoSources —— 全部候选链接（不解析，供 content.js 做 IDM 式列表）
//   getVideoSource  —— 悬停门控的单选命中（按钮显隐用）
import { pageCandidates, findSniffedByURL } from "./page-match.js";
import { qualityFromURL, isMasterCandidateM3U8 } from "./m3u8-parse.js";

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
