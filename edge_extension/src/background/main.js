// M3U8 Video Catcher - background service worker（入口）
// ------------------------------------------------------------
// 这里只做装配：安装嗅探探测器、挂消息路由、挂工具栏点击。
// 具体职责拆分见同目录各模块：
//   list-store    嗅探列表存储（写锁 / 淘汰 / TTL）
//   sniff-probes  嗅探探测器注册表（webRequest 三通道）
//   page-match    页面归属判定与候选筛选
//   m3u8-parse    m3u8/画质解析
//   video-source  「这个视频」的候选与选项
//   server-api    本地 Go 服务访问（令牌握手 / 下载 / 轮询 / 控制）
//   referer-rules DNR Referer 注入
import {
  recordMedia,
  updateMediaItem,
  pickEvictionIndex,
  pruneExpired,
  clearLists,
} from "./list-store.js";
import { installSniffProbes, contentTypeMediaType, isLikelyFullFile } from "./sniff-probes.js";
import { sameSite, isCandidateURL, findSniffedByURL } from "./page-match.js";
import { getVideoSources, getVideoSource, openDownloader } from "./video-source.js";
import {
  getSettings,
  getApiToken,
  pingServer,
  detectExePath,
  downloadViaServer,
  queryDownload,
  controlTask,
} from "./server-api.js";
import { setRefererRules, clearRefererRules, sweepStaleRules, allocRuleIds } from "./referer-rules.js";

// 点击工具栏图标：打开下载器页面
chrome.action.onClicked.addListener(() => {
  chrome.tabs.create({ url: chrome.runtime.getURL("downloader.html") });
});

// ============================================================
// 消息路由
// ============================================================
chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg.type === "setReferers") {
    setRefererRules(msg.tabId, msg.pairs)
      .then(() => sendResponse({ ok: true }))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  if (msg.type === "getVideoSource") {
    getVideoSource(msg)
      .then((res) => sendResponse({ ok: true, source: res }))
      .catch((e) => sendResponse({ ok: false, error: String(e && e.message ? e.message : e) }));
    return true;
  }
  if (msg.type === "getVideoSources") {
    getVideoSources(msg)
      .then((res) => sendResponse({ ok: true, sources: res }))
      .catch((e) => sendResponse({ ok: false, error: String(e && e.message ? e.message : e) }));
    return true;
  }
  if (msg.type === "openDownloader") {
    openDownloader(msg.item);
    sendResponse({ ok: true });
    return true;
  }
  if (msg.type === "clearList") {
    clearLists()
      .then(() => {
        chrome.action.setBadgeText({ text: "" });
        sendResponse({ ok: true });
      })
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  if (msg.type === "updateMediaItem") {
    // 扩展页面回写单条嗅探记录（分析结果）——走同一把写锁，避免与 recordMedia 撞车
    updateMediaItem(msg.key, msg.url, msg.patch)
      .then((res) => sendResponse(res))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
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
  if (msg.type === "getApiToken") {
    // content.js 跑在页面上下文里，调 /probe 前需要访问令牌。
    // 令牌只经扩展内部通道传递，不落到页面世界（页面脚本拿不到）。
    getApiToken()
      .then((token) => sendResponse({ ok: !!token, token }))
      .catch(() => sendResponse({ ok: false, token: "" }));
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

installSniffProbes();

// 下载器页签关闭时清掉它的 Referer 规则：规则按 tabId 生效，tab 没了规则
// 就永远匹配不到请求，只会占用动态规则配额（Chrome 上限 5000 条）。
// 监听器必须在顶层同步注册（MV3 事件唤醒机制的要求）。
chrome.tabs.onRemoved.addListener((tabId) => {
  clearRefererRules(tabId).catch(() => {});
});

// SW 每次唤醒（页面导航/webRequest/消息都会唤醒）都探测一次 exe 路径。
// 兜底命令可能在从未发起过下载时出现（如"视频未嗅探到"错误面板），
// 不能只依赖下载流程里 pingServer 的顺带探测。
detectExePath();

// SW 休眠期间错过的 tabs.onRemoved 无法追补，每次唤醒扫一遍死 tab 的规则。
sweepStaleRules().catch(() => {});

// ============================================================
// 测试导出面（node tests 通过 new Function + chrome 桩加载本 bundle 后取用）
// 真实浏览器里这份导出只是挂在全局上，无副作用。
// ============================================================
export const __test__ = {
  recordMedia,
  updateMediaItem,
  pickEvictionIndex,
  pruneExpired,
  clearLists,
  findSniffedByURL,
  getVideoSource,
  getVideoSources,
  isCandidateURL,
  sameSite,
  // 探测器判据（纯函数，供 tests/probes.test.js 直接断言）
  contentTypeMediaType,
  isLikelyFullFile,
  // DNR Referer 规则管理（tests/refererrules.test.js）
  setRefererRules,
  clearRefererRules,
  sweepStaleRules,
  allocRuleIds,
};
