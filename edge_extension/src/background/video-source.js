// 「这个视频」的候选与选项解析：
//   getVideoSources —— 全部候选链接（不解析，供 content.js 做链接列表）
//   getVideoSource  —— 悬停门控的单选命中（按钮显隐用）
import { pageCandidates, listCandidates, findSniffedByURL, uniqueTabCandidate } from "./page-match.js";
import { qualityFromURL, isMasterCandidateM3U8 } from "./m3u8-parse.js";

export function openDownloader(item) {
  const params = new URLSearchParams();
  params.set("url", item.url);
  params.set("title", item.title || "");
  params.set("pageUrl", item.pageUrl || "");
  params.set("quality", item.quality || "");
  // 不带 format：downloader 页从不读它（全扩展无 get("format") 消费者，
  // 是历史残留的死参数，评审自审 #15）。
  chrome.tabs.create({
    url: chrome.runtime.getURL("downloader.html?" + params.toString()),
  });
}

// getVideoSources 返回"该视频"的全部候选链接（不挑选、不解析），
// 供 content.js 做链接列表（解析放页面主世界，避免扩展 Origin 403）。
//
// 用 listCandidates（宽）而不是 getVideoSource 用的 pageCandidates（严）：
// 列表是"给用户挑"的，宁多勿漏；按钮是"替用户承诺"的，宁缺勿假。两套语义见
// page-match.js 里两个函数的注释。
export async function getVideoSources({ src = "", pageUrl = "", embedUrl = "" }, sender = {}) {
  const { hostM3U8, hostMP4 } = await listCandidates(pageUrl);
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
  // 权威 tab 级唯一候选（方案1，与 getVideoSource 同步）：点击下载走的是列表
  // 数据源，若只补按钮那侧、列表不补，就会出现「按钮亮着、点下去却报无法识别」
  // 的假按钮。这里把同一个唯一候选补进列表，保证按钮承诺与点击动作一致。
  const tabAuth = tabAuthority(sender);
  if (tabAuth.isTop && tabAuth.tabUrl && tabAuth.tabUrl !== pageUrl) {
    const only = await uniqueTabCandidate(tabAuth.tabUrl);
    if (only) {
      sources.push({
        type: only.type === "mp4" ? "mp4" : "ts",
        url: only.url,
        title: only.title || "",
        pageUrl: only.pageUrl || pageUrl,
        size: only.size || 0,
      });
    }
  }
  // src 直链可能与嗅探到的 mp4 重复
  return sources.filter((s, i, arr) => arr.findIndex((o) => o.url === s.url) === i);
}

export async function getVideoSource({ src = "", pageUrl = "", embedUrl = "", segmentDir = "", title = "" }, sender = {}) {
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

  // 4.  fallback：当前页最大的 MP4 直链（过滤掉广告小片段）。
  //    候选只是「同页记录」，同一页仍可能同时挂着别的整文件 mp4（卡片预览、
  //    页面背景循环动画），它们与主视频在请求形态上完全一样（同站、整文件、
  //    体积过 MB），只按体积挑最大的会把装饰资源当成视频。
  //    因此这一步有两种正面信号才敢挑：
  //      a) 活跃信号（segmentDir）——页面最近一条媒体请求的目录，命中候选
  //         的目录才算「这个视频正在拉」；
  //      b) 候选唯一——同页只有这一条 mp4，按体积回退没有歧义。
  //    segmentDir 命中多条（同一目录下多个整文件）仍按体积取最大；命中为空
  //    但候选多条（首页横幅 + 卡片预览并存）则直接判失败 —— 宁缺勿假，不给
  //    一个看起来正常、实际是装饰资源的错误链接（用户可在扩展页嗅探列表自取）。
  const realMP4 = hostMP4
    .filter((it) => (it.size || 0) > 1024 * 1024) // > 1MB
    .sort((a, b) => (b.size || 0) - (a.size || 0));
  if (segmentDir) {
    const playingMP4 = realMP4.filter((it) => it.url.startsWith(segmentDir));
    if (playingMP4.length) return makeMP4Source(playingMP4[0], title, pageUrl);
  } else if (realMP4.length === 1) {
    return makeMP4Source(realMP4[0], title, pageUrl);
  }

  // 5. 权威 tab 级唯一候选回退（方案1，2026-09-18）。
  //    「同源 iframe + MSE(blob) + 加密源」站点（同源播放器脚本页 /static/player/...?url=<hex>）：
  //    sniff 的 frameUrl 常为空，content.js 悬停时上报的 pageUrl 是 iframe 的
  //    location.href（含加密参数），与记录锚定的顶层页 URL 对不上，前面几档全落空。
  //    改拿浏览器权威的顶层页 URL（sender.tab.url）重做同页判定，唯一性闸门兜底。
  //    ⚠ 只允许顶层 frame 消费这个回退：all_frames 注入下顶层与 iframe 各有一份
  //    content.js 都会判定，「整个 tab 唯一视频」是页级信号，若两 frame 都吃它
  //    就会同屏冒出两个悬停按钮。
  //    残余风险（已知、可接受）：顶层页恰好只有一条可嗅探媒体、而它其实是装饰
  //    资源（真实视频走 WebRTC/DRM 等不可嗅探通道）时仍会误中——几率极低，且
  //    比「按体积猜最大」稳妥得多。
  const tabAuth = tabAuthority(sender);
  if (tabAuth.isTop && tabAuth.tabUrl && tabAuth.tabUrl !== pageUrl) {
    const only = await uniqueTabCandidate(tabAuth.tabUrl);
    if (only) return makeSourceFor(only, title, tabAuth.tabUrl);
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

// tabAuthority 从 sender 提取「权威 tab 归属」信息（方案1）：
//   tabUrl —— 浏览器给的顶层标签页 URL（sender.tab.url），不可被页面伪造（相比
//     content.js 上报的 location.href 可靠）。
//   isTop  —— 发消息的 frame 是否为顶层（sender.frameId === 0）。all_frames 注入
//     下顶层与 iframe 各有一份 content.js；「整个 tab 唯一视频」是页级信号，只该
//     由顶层 frame 消费，否则同屏会冒出两个悬停按钮。
function tabAuthority(sender) {
  const tabUrl = (sender && sender.tab && sender.tab.url) || "";
  const isTop = !!(sender && sender.frameId === 0);
  return { tabUrl, isTop };
}
