// 嗅探探测器注册表
// ------------------------------------------------------------
// 每个探测器只负责「发现一个候选媒体 URL 并交给 recordMedia」，与记录 / 淘汰 /
// 持久化完全解耦。新增探测通道（DOM <video> 探测、HLS 实例 hook 等）只需往
// 这里追加一条，不必动记录逻辑。
//
// frameUrl 记录发起请求的 frame 文档 URL（details.documentUrl）：跨域 iframe
// 播放器（苹果CMS 第三方解析页等）里 video 所在 frame 的 location.href 与顶层
// 页 pageUrl 不同域，没有它匹配时候选会全部落空（悬停按钮永不出现）。
//
// 探测通道全景（G3）：
//   - webRequest-m3u8 / webRequest-mp4：按 URL 路径扩展名匹配，覆盖绝大多数站点。
//   - webRequest-contenttype：按响应 Content-Type 匹配，覆盖「URL 不带扩展名」
//     的流（/videoplayback?… 之类的直链、fetch() 拉的无后缀 playlist）。
//     为避免把海量分片灌进列表，只收两种信号：
//       * playlist 类 Content-Type（m3u8）——每页就几条，且 recordMedia 按 URL 去重；
//       * video/mp4 且「非分段响应（非 206 / 无 Content-Range）+ Content-Length > 1MB」
//         ——DASH/分段播放器的分片几乎都是 206 分段响应，直链整文件才返回 200。
//     ⚠ onCompleted 的 extraInfoSpec 只认 "extraHeaders" / "responseHeaders" 两个值，
//       传 "requestHeaders"（那是 onBeforeSendHeaders 才认的）会让**整个探测器静默
//       安装失败**——不报错到调用点之外，只是功能没了。所以判据一律取响应侧，
//       不要在这里读 details.requestHeaders。tests/probes.test.js 守着这条。
//   - 仍属盲区：blob:/MSE 播放器（分片经 SourceBuffer 注入、请求 URL 与媒体
//     无关）——那类站点需要页面内 hook，代价是扩大 MAIN world 面（与 S3 相反
//     方向），暂不做。
import { recordMedia } from "./list-store.js";

export const SNIFF_PROBES = [
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
    },
  },
  {
    name: "webRequest-mp4", // MP4 直链（IDM 也常提供 MP4 格式选项）
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
    },
  },
  {
    name: "webRequest-contenttype", // 无扩展名流的兜底通道（见文件头注释）
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
    },
  },
];

// withTab 取请求所属标签页信息后回调；单个探测器安装失败不影响其它探测器。
export function withTab(details, fn) {
  chrome.tabs.get(details.tabId, (tab) => {
    if (chrome.runtime.lastError) return;
    fn(tab);
  });
}

export function installSniffProbes() {
  for (const p of SNIFF_PROBES) {
    try {
      p.install();
    } catch (e) {
      console.error("嗅探探测器安装失败:", p.name, e);
    }
  }
}

// shouldSniff 嗅探入录前置过滤：
//   1. main_frame/sub_frame 是文档导航（网页/iframe 页面本身），永远不是媒体
//   2. URL 的路径部分必须以对应扩展名结尾——解析页 URL 形如
//      https://parser.com/play/?url=…index.m3u8，查询参数尾部恰好是 .m3u8，
//      恰好命中 webRequest 匹配模式（模式按 path+query 整体匹配），会被误录
//      成 m3u8；点它下载拉回的是 HTML 网页，任务必失败
export function shouldSniff(details, ext) {
  if (details.type === "main_frame" || details.type === "sub_frame") return false;
  try {
    return new URL(details.url).pathname.toLowerCase().endsWith(ext);
  } catch {
    return false;
  }
}

// headerValueOf 大小写无关地取响应头。
function headerValueOf(details, name) {
  if (!details.responseHeaders) return "";
  const h = details.responseHeaders.find((x) => x.name.toLowerCase() === name.toLowerCase());
  return h ? h.value || "" : "";
}

// contentLengthOf 响应 Content-Length（解析失败返回 0）。
function contentLengthOf(details) {
  const v = parseInt(headerValueOf(details, "content-length"), 10);
  return Number.isFinite(v) && v > 0 ? v : 0;
}

// playlistCTRe 命中即视为 m3u8 播放列表的 Content-Type。
const playlistCTRe = /^(application\/(vnd\.apple\.mpegurl|x-mpegurl|mpegurl)|audio\/mpegurl)\b/i;

// contentTypeMediaType 从响应 Content-Type 判定媒体类型：
//   返回 "m3u8" / "mp4"，都不是则返回 null。分片类型（video/mp2t 等）故意
//   不收——分片 URL 会以每片一条的频率灌进列表，把主视频挤出 30 条配额。
export function contentTypeMediaType(details) {
  const ct = headerValueOf(details, "content-type").split(";")[0].trim();
  if (!ct) return null;
  if (playlistCTRe.test(ct)) return "m3u8";
  if (/^video\/mp4\b/i.test(ct)) return "mp4";
  return null;
}

// isLikelyFullFile 对 video/mp4 命中做二次确认，挡掉 DASH/分段播放器的分片。
// 判据全部取响应侧：onCompleted 的 extraInfoSpec 不接受 requestHeaders，
// 读 details.requestHeaders 永远是 undefined（硬写进 extraInfoSpec 还会让探测器装不上）。
// 与「请求带 Range」等价的响应侧信号：
//   - statusCode === 206 或带 Content-Range → 分段响应，跳过
//     （整文件直链首次下载返回 200 且无 Content-Range）
//   - Content-Length ≤ 1MB → 小分片 / 预览片，跳过
export function isLikelyFullFile(details) {
  if (details.statusCode === 206) return false;
  if (headerValueOf(details, "content-range")) return false;
  return contentLengthOf(details) > 1024 * 1024;
}
