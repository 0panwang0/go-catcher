// 页面归属判定与候选筛选：从嗅探列表里挑出「属于当前页面 / 这个视频」的记录。
import { isMasterCandidateM3U8, masterFirst } from "./m3u8-parse.js";

// hostOf 提取 hostname（非法 URL 返回空串；旧记录无该字段时安全）。
export function hostOf(u) {
  try {
    return new URL(u).hostname;
  } catch {
    return "";
  }
}

// sameSite 判定嗅探记录是否属于查询页面：顶层页 pageUrl 或发起请求的 frame
// 文档 frameUrl 任一命中即算。跨域 iframe 播放器（video 所在 frame 的
// location.href 与顶层页不同域）必须靠 frameUrl 才能对上；frameUrl 侧仅比
// hostname（解析页路径对同一站点所有视频都相同，比路径只会全部漏掉）。
export function sameSite(it, pageUrl) {
  if (!it.pageUrl || !pageUrl) return false;
  const h = hostOf(pageUrl);
  if (!h) return false;
  return hostOf(it.pageUrl) === h || hostOf(it.frameUrl) === h;
}

// isCandidateURL 嗅探记录读出自愈：过滤历史上被误录的解析页 URL（路径无媒体
// 扩展名、靠 ?url= 跳转参数尾部伪装 .m3u8）。路径带扩展名、或不含 ?url=
// 参数的记录才作为候选（?url= 是苹果CMS 系解析页的通用签名）。
export function isCandidateURL(u) {
  try {
    const p = new URL(u);
    return /\.(m3u8|mp4)$/i.test(p.pathname) || !p.searchParams.has("url");
  } catch {
    return false;
  }
}

// hostCandidates 取与 pageUrl 同站的全部嗅探记录（顶层页或发起 frame 任一
// 命中），master 候选排前。
export async function hostCandidates(pageUrl) {
  const { m3u8_list = [], mp4_list = [] } = await chrome.storage.local.get([
    "m3u8_list",
    "mp4_list",
  ]);
  const hostM3U8 = m3u8_list.filter((it) => sameSite(it, pageUrl) && isCandidateURL(it.url));
  const hostMP4 = mp4_list.filter((it) => sameSite(it, pageUrl) && isCandidateURL(it.url));
  // master candidate 永远在前：避免播放器先请求 240p 变体导致我们误中预览片
  hostM3U8.sort(masterFirst);
  hostMP4.sort(masterFirst);
  return { hostM3U8, hostMP4 };
}

// pageCandidates 在同站基础上进一步限定「同一视频页」，逐级收窄、宁多勿漏：
//   1. frameUrl 完全相等——iframe 播放器场景最准（解析页 URL 的 ?url= 参数
//      编码了目标 m3u8，同站不同视频的 frameUrl 各不相同，可精确隔离）
//   2. 顶层页 hostname+pathname 相同——常规站内页（B 站房间号在路径里可隔离）
//   3. 都为空（SPA 路由变化等）回退同站全集
export async function pageCandidates(pageUrl) {
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

// findSniffedByURL 解析页 iframe 的 ?url= 目标地址 → 嗅探记录。
// 匹配两级：1) origin+pathname 完全相同；2) 同一目录（同一视频的各档 playlist
// 与分片同目录）。忽略 query，避免签名/时间戳参数造成假不匹配。
export async function findSniffedByURL(target) {
  if (!target) return null;
  const { m3u8_list = [], mp4_list = [] } = await chrome.storage.local.get([
    "m3u8_list",
    "mp4_list",
  ]);
  const all = [...m3u8_list, ...mp4_list].filter((it) => isCandidateURL(it.url));
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
