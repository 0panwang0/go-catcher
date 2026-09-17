// M3U8 Video Catcher - 下载器页面
//
// 职责边界：本页只做「列列表 + 选画质 + 调服务」。
// **实际下载一律委托给本地 Go 服务**（downloadViaServer 消息 → /download?mode=disk），
// 因为只有它能做这些事：AES-128 解密、#EXT-X-MAP init 段写入、fMP4 的 tfdt 归一化
// 与 mvhd 回填、落盘前的产物抽样校验、以及断点续传。
//
// 历史包袱：这里曾有一套自己的实现——分片 fetch 回来 Blob 原样拼接。它对加密流
// （没有解密能力）和 fMP4（init 段不在分片列表里）必然产出打不开的文件，而日志
// 照样打印"已保存"，是最隐蔽的一类损坏。那套实现连同它自带的 m3u8 解析已整体删除。
//
// 解析、文本净化、媒体 URL 判定、HTML 转义现在都只有一份源码：
// src/background/{m3u8-parse,cli-args,media-url,html-escape}.js
// （service worker 打包时用的也是它们；content.js 经 content-shared.js 取同一份）。
// 本文件是 ES 模块，直接 import——不再有"改一处忘另一处"。
// ⚠ 下面这几行 import 必须各自保持**单行**：tests/downloader.test.js 在 node 里靠
//   「去掉 import 行 + 逐个前置展开模块源码」来加载本文件（node 无法直接 import
//   扩展页脚本，也不值得为它改 package.json 的 type）。
import { fetchText, parseDuration, parseSegments, parseVariants, shortQuality, qualityFromURL, fmtDur } from "./src/background/m3u8-parse.js";
import { sanitizeFileName, assembleGoCommand } from "./src/background/cli-args.js";
import { isCandidateURL, isPlaylistURL } from "./src/background/media-url.js";
import { escapeHtml } from "./src/background/html-escape.js";

const ANALYZE_LIMIT = 12; // 最多分析的嗅探条数
const MAIN_VIDEO_MIN = 60; // 时长 >= 60s 判定为主视频
const POLL_INTERVAL_MS = 700; // 轮询服务端任务状态的间隔
const POLL_MAX_MISSES = 15; // 连续查不到任务多少次后放弃（≈10s，服务重启过）

const $ = (sel) => document.querySelector(sel);

// isCandidateURL / isPlaylistURL 由 media-url.js 提供（import 在文件头）：
// 这两条判定此前本文件与 background 侧各写一份、靠注释互相提醒"保持一致"，
// 而漂移的症状是「浮层能下的链接，扩展页下不了」（评审 P3-6）。

let running = false;
let lastCommand = ""; // 最近一次失败时生成的终端兜底命令

document.addEventListener("DOMContentLoaded", async () => {
  // 如果是从页面选择面板跳转过来的，自动填入并下载该视频。
  // 此时 URL 已经是用户选定的具体档位（不是 master），所以走 forcedPlaylistURL
  // 跳过选画质那一步；TS 与 MP4 都走同一条服务端路径。
  const params = new URLSearchParams(location.search);
  const autoUrl = params.get("url");
  const autoTitle = params.get("title") || "";
  const autoPage = params.get("pageUrl") || "";
  const autoQuality = params.get("quality") || "";
  if (autoUrl) {
    $("#manualUrl").value = autoUrl;
    $("#manualName").value = autoTitle;
    history.replaceState({}, "", location.pathname);
    startDownload(autoUrl, autoPage, autoTitle, autoUrl, autoQuality);
  }

  refreshList();

  // —— 设置卡片：exe 路径 + 服务端口（存 chrome.storage.local，background/content 共用）——
  const DEFAULT_SETTINGS = { exePath: "go-catcher.exe", port: 7891 };
  const saved = await chrome.storage.local.get(DEFAULT_SETTINGS);
  $("#setExePath").value = saved.exePath || DEFAULT_SETTINGS.exePath;
  $("#setPort").value = saved.port || DEFAULT_SETTINGS.port;
  $("#btnSaveSettings").addEventListener("click", async () => {
    const port = parseInt($("#setPort").value, 10);
    if (isNaN(port) || port < 1 || port > 65535) {
      $("#settingsFeedback").textContent = "端口需为 1 – 65535";
      $("#settingsFeedback").style.color = "#f56c6c";
      $("#settingsFeedback").style.display = "inline";
      return;
    }
    await chrome.storage.local.set({ exePath: $("#setExePath").value.trim() || DEFAULT_SETTINGS.exePath, port });
    $("#settingsFeedback").textContent = "✓ 已保存";
    $("#settingsFeedback").style.color = "#67c23a";
    $("#settingsFeedback").style.display = "inline";
    refreshSvcStatus();
  });
  refreshSvcStatus();

  $("#btnGo").addEventListener("click", () => {
    const url = $("#manualUrl").value.trim();
    const name = $("#manualName").value.trim();
    if (url) startDownload(url, "", name);
  });
  $("#btnClear").addEventListener("click", async () => {
    await chrome.runtime.sendMessage({ type: "clearList" });
    refreshList();
  });
  // 兜底命令复制（服务不可用时用）
  $("#btnCopyCmd").addEventListener("click", async () => {
    const fb = $("#copyFeedback");
    try {
      await navigator.clipboard.writeText(lastCommand);
      fb.textContent = "✓ 已复制";
      fb.style.color = "#67c23a";
    } catch {
      fb.textContent = "复制失败，请手动选中命令";
      fb.style.color = "#f56c6c";
    }
    fb.style.display = "inline";
    setTimeout(() => (fb.style.display = "none"), 2000);
  });
  // 嗅探列表变化时自动刷新（页面保持打开也能看到新视频）
  chrome.storage.onChanged.addListener((changes) => {
    if (changes.m3u8_list && !running) refreshList();
  });
});

// ============================================================
// 嗅探列表 + 主视频识别
// ============================================================
// fetchList 取嗅探列表。必须走后台消息而不是直读 storage：list-store 持内存
// 权威副本，合并落盘窗口内 storage 里可能还是旧值（评审 P2-6 / F10）。
// 后台不可达时（SW 崩溃 / 正在重启）退回直读 storage —— 那种情况下内存副本
// 也不存在，两边一致；宁可显示可能晚一拍的列表，也好过整页空白。
async function fetchList() {
  try {
    const r = await chrome.runtime.sendMessage({ type: "getList", key: "m3u8_list" });
    if (r && r.ok) return r.list || [];
  } catch {}
  const { m3u8_list = [] } = await chrome.storage.local.get("m3u8_list");
  return m3u8_list;
}

async function refreshList() {
  // 自愈过滤历史误录的解析页假链接
  const list = (await fetchList()).filter(isCandidateURL);
  renderList(list);
  if (list.some((it) => !it.analyzed)) analyzeList(list);
}

async function analyzeList(list) {
  // 注入 Referer + Origin 规则（每个 host+来源页一条）。必须在本页发请求——
  // 规则是按 tabId 生效的，service worker 的请求拿不到，会被 CDN 403。
  try {
    const tab = await chrome.tabs.getCurrent();
    if (!tab) return;
    const pairs = list.map((it) => {
      let host = "";
      try { host = new URL(it.url).hostname; } catch {}
      return { host, referer: it.pageUrl || "" };
    });
    await chrome.runtime.sendMessage({
      type: "setReferers",
      tabId: tab.id,
      pairs,
    });
  } catch {}

  // 分析未处理的条目：获取时长/分片数/画质，识别主视频
  const todo = list.filter((it) => !it.analyzed).slice(0, ANALYZE_LIMIT);
  for (const item of todo) {
    await analyzeItem(item);
    // 分析完一条就保存并重绘，主视频尽早浮上来。
    // 写事务委托给 service worker（与嗅探写入共用同一把 storage 写锁）：页面与
    // SW 是两个独立 JS 上下文，各自直接写 storage 时无法互相串行化，会互相覆盖。
    try {
      await chrome.runtime.sendMessage({
        type: "updateMediaItem",
        key: "m3u8_list",
        url: item.url,
        patch: item,
      });
    } catch {}
    if (!running) {
      renderList(await fetchList());
    }
  }
}

async function analyzeItem(item) {
  try {
    let text = await fetchText(item.url);
    let quality = qualityFromURL(item.url);
    let duration = parseDuration(text);
    let segments = parseSegments(text, item.url).length;

    // master playlist：取最高码率子列表分析画质与时长
    if (text.includes("#EXT-X-STREAM-INF")) {
      const variants = parseVariants(text, item.url);
      const best = variants[0];
      quality = shortQuality(best);
      const sub = await fetchText(best.url);
      duration = parseDuration(sub);
      segments = parseSegments(sub, best.url).length;
    }
    item.duration = duration;
    item.segments = segments;
    item.quality = quality;
  } catch {
    item.duration = -1; // 分析失败（如未连 VPN / CDN 拒绝）
  }
  item.analyzed = true;
}

function renderList(list) {
  const box = $("#list");
  box.innerHTML = "";
  if (!list.length) {
    box.innerHTML =
      '<div class="empty">暂未嗅探到 m3u8 —— 先在浏览器里打开视频页面并播放，然后回到这里（列表会自动更新）</div>';
    return;
  }

  // 主视频排最前：按时长降序；未分析/失败的排后面
  const sorted = [...list].sort((a, b) => {
    const da = a.duration || -1;
    const db = b.duration || -1;
    return db - da;
  });

  for (const item of sorted) {
    const div = document.createElement("div");
    div.className = "item";

    let host = "", time = "";
    try {
      host = new URL(item.url).hostname;
      time = new Date(item.time).toLocaleTimeString();
    } catch {}

    // 判定主视频 / 预览
    let kind = "", kindClass = "";
    if (item.duration > MAIN_VIDEO_MIN) {
      kind = "主视频"; kindClass = "main";
    } else if (item.duration > 0) {
      kind = "预览"; kindClass = "preview";
    } else if (item.duration === -1) {
      kind = "分析失败"; kindClass = "fail";
    }

    const durText = item.duration > 0 ? fmtDur(item.duration) : "";
    const segText = item.segments ? `${item.segments} 分片` : "";
    const q = item.quality || "";
    const info = [durText, segText, q].filter(Boolean).join(" · ");

    const srcLink = item.pageUrl
      ? `<a class="src" href="${escapeHtml(item.pageUrl)}" target="_blank" rel="noreferrer">来源页 ↗</a>`
      : "";

    const body = document.createElement("div");
    body.className = "body";
    body.innerHTML = `
      <div class="title">${escapeHtml(item.title || host)}
        ${kind ? `<span class="badge ${kindClass}">${kind}</span>` : ""}
        ${!item.analyzed ? '<span class="badge">分析中…</span>' : ""}
      </div>
      <div class="meta">
        <span class="host">${escapeHtml(host)}</span>
        ${info ? `<span class="info">${info}</span>` : ""}
        <span class="time">${time}</span>
        ${srcLink}
      </div>
      <div class="url" title="${escapeHtml(item.url)}">${escapeHtml(item.url)}</div>`;

    const btn = document.createElement("button");
    btn.textContent = "下载";
    btn.onclick = () => startDownload(item.url, item.pageUrl, item.title || "");
    div.appendChild(body);
    div.appendChild(btn);
    box.appendChild(div);
  }
}

// escapeHtml / fmtDur 也搬到了共享模块（html-escape.js / m3u8-parse.js）：
// 这两份此前与 content.js 侧的拷贝**逐字相同**，即"两边都能跑，改一处只对一边生效"
// —— escapeHtml 漏一处的后果是把用户可控文本原样塞进 innerHTML（评审 P3-6）。

// ============================================================
// 下载主流程（本页不下载，只调服务 + 报进度）
// ============================================================
async function startDownload(m3u8URL, pageURL, videoName, forcedPlaylistURL = "", forcedQuality = "") {
  if (running) return;
  running = true;
  const btns = document.querySelectorAll("button");
  btns.forEach((b) => (b.disabled = true));
  hideFallback();
  const log = openLog();

  try {
    // 1. 定下要下载的播放列表。
    //    forcedPlaylistURL 有值 = 调用方已指定具体档位，直接用；否则若还是 master
    //    playlist，先让用户选一档。这一步是**可选增强**，CDN 拒绝/网络不通都只降级
    //    为"不选档位"，照样把原 URL 交给服务端（它会自己取最高码率）。
    let playlistURL = forcedPlaylistURL || m3u8URL;
    let quality = forcedQuality || qualityFromURL(m3u8URL);
    if (!forcedPlaylistURL && isPlaylistURL(m3u8URL)) {
      await injectReferer(m3u8URL, pageURL);
      const variants = await fetchMasterVariants(m3u8URL);
      if (variants && variants.length) {
        log(`Master playlist，检测到 ${variants.length} 个画质档位`);
        const choice = await pickQuality(variants);
        if (!choice) {
          log("已取消");
          return;
        }
        playlistURL = choice.url;
        quality = shortQuality(choice);
        log(`已选择画质: ${choice.label}`);
      }
    }

    // 2. 交给本地服务下载：解密 / init 段 / 产物校验都在那边做。
    //    扩展名给 .ts 即可——服务端会按实际探测到的容器改名（TS 保持 .ts，fMP4 改 .mp4）。
    const filename = makeFilename(playlistURL, videoName, quality);
    log(`交给本地服务下载：${filename}`);
    const resp = await chrome.runtime.sendMessage({
      type: "downloadViaServer",
      m3u8Url: playlistURL,
      referer: pageURL || "",
      title: videoName || "",
      filename,
    });
    if (!resp) throw new Error("扩展后台未响应");
    if (!resp.ok) {
      if (resp.code === "USER_CANCEL") {
        log("已取消文件夹选择");
        return;
      }
      throw new Error(resp.error || "下载启动失败");
    }
    log(`已开始落盘（目录 ${resp.dir || "?"}）`);
    await pollTask(resp.taskId, log);
  } catch (e) {
    log(`失败: ${e.message}`, "err");
    log("若本地服务未启动，请先打开 go-catcher.exe；也可用下方「复制命令」在终端直接下载。", "err");
    await offerFallback(m3u8URL, pageURL, videoName);
    console.error(e);
  } finally {
    running = false;
    btns.forEach((b) => (b.disabled = false));
    refreshList();
  }
}

// injectReferer 按 host 注入来源页的 Referer/Origin。
// 扩展页直接 fetch 会带 chrome-extension:// 的 Origin，CDN 视为非页面请求而 403。
async function injectReferer(url, referer) {
  if (!referer) return;
  let host = "";
  try { host = new URL(url).hostname; } catch { return; }
  const tab = await chrome.tabs.getCurrent();
  if (!tab) return;
  await chrome.runtime.sendMessage({ type: "setReferers", tabId: tab.id, pairs: [{ host, referer }] });
}

// fetchMasterVariants 拉一份 master playlist 并解析出全部档位。
// 失败一律返回 null 而不抛：拿不到档位不该让整个下载失败——服务端自己会选最高码率。
async function fetchMasterVariants(m3u8URL) {
  try {
    const text = await fetchText(m3u8URL);
    if (!text.includes("#EXT-X-STREAM-INF")) return null;
    return parseVariants(text, m3u8URL);
  } catch (e) {
    console.warn("解析 master playlist 失败，交给服务端自选码率:", e);
    return null;
  }
}

// pollTask 轮询服务端任务状态直到收尾。
// 连续查不到任务（服务重启过 / 任务被清理）会放弃，而不是无限轮询。
async function pollTask(taskId, log) {
  let lastStage = "";
  let misses = 0;
  for (;;) {
    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
    let st = null;
    try {
      st = await chrome.runtime.sendMessage({ type: "queryDownload", taskId });
    } catch { /* SW 正在唤醒或短暂不可达，下一轮再看 */ }
    if (!st || !st.exists) {
      if (++misses > POLL_MAX_MISSES) {
        throw new Error("任务状态丢失（本地服务可能已重启）");
      }
      continue;
    }
    misses = 0;
    const stage = st.stage || "下载中";
    if (stage !== lastStage) {
      lastStage = stage;
      log(stage);
    }
    if (st.state === "inProgress" || st.state === "paused") {
      // pct 是 0-100 的整数；total 为 0（还没解析出分片数）时按 0% 显示
      const pct = typeof st.pct === "number" ? st.pct : 0;
      showProgress(st.state === "paused" ? "" : pct, stage);
      continue;
    }
    if (st.state === "complete") {
      showProgress(100, "已完成");
      log(`已保存 ${st.finalPath || st.filename || ""}`, "ok");
      return;
    }
    if (st.state === "canceled") {
      log("已取消", "err");
      return;
    }
    throw new Error(st.error || "下载失败");
  }
}

// ============================================================
// 画质选择器（唯一需要解析播放列表的场景）
// ============================================================
function pickQuality(variants) {
  return new Promise((resolve) => {
    const box = $("#qualityPicker");
    box.innerHTML = "";
    box.style.display = "flex";
    $("#progressBox").style.display = "block";
    $("#progressText").textContent = `检测到 ${variants.length} 个画质，请选择要下载的档位：`;
    $("#progressFill").style.width = "0%";
    const cleanup = () => {
      box.style.display = "none";
      box.innerHTML = "";
    };
    for (const v of variants) {
      const btn = document.createElement("button");
      btn.textContent = v.label;
      btn.onclick = () => { cleanup(); resolve(v); };
      box.appendChild(btn);
    }
    const cancel = document.createElement("button");
    cancel.textContent = "取消";
    cancel.className = "secondary";
    cancel.onclick = () => { cleanup(); resolve(null); };
    box.appendChild(cancel);
  });
}

// ============================================================
// 进度与日志
// ============================================================
function openLog() {
  const box = $("#log");
  box.style.display = "block";
  box.textContent = "";
  return (msg, cls) => {
    const line = document.createElement("div");
    if (cls) line.className = cls;
    line.textContent = `[${new Date().toLocaleTimeString()}] ${msg}`;
    box.appendChild(line);
    box.scrollTop = box.scrollHeight;
  };
}

// showProgress 画进度条。pct 传空串表示"进度未知"（如暂停），只更新阶段文字。
function showProgress(pct, stage) {
  $("#progressBox").style.display = "block";
  if (pct !== "" && pct != null) {
    $("#progressFill").style.width = Math.max(0, Math.min(100, pct)) + "%";
  }
  $("#progressText").textContent = stage || "";
}

// ============================================================
// 服务不可用时的兜底：把 CLI 直下命令交给用户复制
// ============================================================
// 生成跨 PowerShell / Git Bash / cmd 都能直接粘贴运行的命令。
// 拼装本身（chcp 前缀 / 引号 / ; 分隔 / 逐值净化）走共享的 assembleGoCommand——唯一实现；
// 本函数只剩一件事：把「输出文件名怎么算」翻译成它的入参，见下面 makeFilename 对照表。
// 这里与 content.js 的 buildGoCommand **有意**不同：本页的档位是用户从 master 里手选的，
// 画质直接来自被选中的变体；浮层没有选择面，只能从 URL 里猜（qualityFromURL）。
function buildGoCommand(m3u8URL, pageURL, videoName, exePath) {
  const outName = makeFilename(m3u8URL, videoName, qualityFromURL(m3u8URL));
  return assembleGoCommand(m3u8URL, pageURL, outName, exePath);
}

// exe 路径优先级：设置里显式填的（等于默认值 go-catcher.exe 视为未填）→
// 服务自报的路径（refreshSvcStatus 顺带缓存）→ 裸文件名（依赖 PATH）。
// ⚠ 与 content.js 的 getExePath 是同一套优先级，措辞也刻意保持相同；那边多一跳
//   （缓存为空时让 background 代问 /svc/info——页面世界直发本机请求会被 CORS 挡）。
//   两份有意保留（评审 P3-6 留档）：改这条优先级时两处必须一起改。
async function getExePath() {
  const s = await chrome.storage.local.get({ exePath: "", detectedExePath: "" });
  const custom = String(s.exePath || "").trim();
  if (custom && custom !== "go-catcher.exe") return custom;
  return s.detectedExePath || "go-catcher.exe";
}

async function offerFallback(m3u8URL, pageURL, videoName) {
  lastCommand = buildGoCommand(m3u8URL, pageURL, videoName, await getExePath());
  const box = $("#fallback");
  if (!box) return;
  $("#fallbackCmd").textContent = lastCommand;
  box.style.display = "flex";
}

function hideFallback() {
  const box = $("#fallback");
  if (box) box.style.display = "none";
}

// ============================================================
// 文件名：makeFilename —— 与 content.js 的 buildOutputName 的对照
// ------------------------------------------------------------
// 优先 "视频名称_画质.扩展名"，拿不到名称就退回 URL 路径段。扩展名只是个初值——
// 服务端会按实际探测到的容器改名（fMP4 → .mp4），所以这里不必也不能猜容器。
//
// 这一对**不合并**（评审 P3-6 明说了"受 content script 限制的先留档"）：
// 两者的输入不同，强行统一会让一侧失真。但凡是两侧应当一致的部分，都由共享
// 函数决定，并由 tests/filename-consistency.test.js 逐条钉住（有标题时必须
// 逐字节相同）。对照表如下：
//
//   维度           本页 makeFilename            浮层 buildOutputName
//   -------------  ---------------------------  ---------------------------
//   画质来源       调用方传入（用户手选的档位）  从 URL 推断 qualityFromURL
//   无标题兜底     路径段用 _ 连起来            取倒数第一个非画质段
//   名字仍为空     video_<毫秒时间戳>          返回空串（不传 -o）
//   扩展名         可传参（默认 .ts）           固定 .ts
//   -------------  ---------------------------  ---------------------------
//   名称清洗       sanitizeFileName             sanitizeFileName
//   画质段剔除     跳过 2160p…240p              跳过 2160p…240p
//   长度上限       80 字符                     80 字符
//
// 「画质来源」这一行的差异是有意的：本页能弹档位选择框，学到的画质是 CDN 自报的，
// 比从 URL 猜准；浮层没有选择面。但两侧**默认路径**下画质都出自共享的
// qualityFromURL，所以同一个视频从哪进去下载，文件名后缀都一样（评审 P2-7）。
// ============================================================
function makeFilename(m3u8URL, videoName, quality, ext = "ts") {
  const suffix = sanitizeFileName(quality || "");
  let base = String(videoName || "").trim();
  if (!base) {
    try {
      const parts = new URL(m3u8URL).pathname
        .split("/")
        .filter(
          (p) =>
            p &&
            !p.endsWith(".m3u8") &&
            // 跳过画质段，否则会拼出 "abc-123_1080p_1080P" 这种重复后缀
            !/^(2160p|1440p|1080p|720p|480p|360p|240p)$/i.test(p)
        );
      if (parts.length) base = parts.join("_");
    } catch {}
  }
  base = sanitizeFileName(base).slice(0, 80);
  if (!base) base = `video_${Date.now()}`;
  return base + (suffix ? `_${suffix}` : "") + `.${ext}`;
}

// ============================================================
// 设置卡片顶部的服务状态指示
// ============================================================
async function refreshSvcStatus() {
  const el = $("#svcStatus");
  if (!el) return;
  const port = (await chrome.storage.local.get({ port: 7891 })).port || 7891;
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 2000);
    const resp = await fetch(`http://127.0.0.1:${port}/health`, { signal: ctrl.signal });
    clearTimeout(timer);
    const healthy = resp.ok && (await resp.text()).trim() === "ok";
    el.textContent = healthy
      ? `✓ 服务运行中（127.0.0.1:${port}）`
      : `服务异常（127.0.0.1:${port}，HTTP ${resp.status}）`;
    el.style.color = healthy ? "#67c23a" : "#f56c6c";
    if (healthy) detectExePath(port); // 顺带缓存 exe 路径，供兜底命令用
  } catch {
    el.textContent = `✗ 服务未启动（127.0.0.1:${port}）——打开 go-catcher.exe 即自动启动`;
    el.style.color = "#f56c6c";
  }
}

// 扩展页面直连 /svc/info 缓存服务自报的 exe 路径（本页面有 host 权限，fetch 不受限）
// ⚠ 与 src/background/server-api.js 的 detectExePath 是**同一个目的、两条通道**，
//   有意不合并（评审 P3-6）：那边跑在 service worker 里、借 getApiToken 的握手
//   顺带写令牌，本页不需要令牌（下载一律经消息交给 service worker），直连更短。
//   两处写的是同一个 storage 键 detectedExePath，值同源（/svc/info 的 exe 字段），
//   谁先跑谁生效——所以这里改字段名时必须连那边一起改。
async function detectExePath(port) {
  try {
    const resp = await fetch(`http://127.0.0.1:${port}/svc/info`, { cache: "no-store" });
    if (!resp.ok) return;
    const info = await resp.json().catch(() => null);
    if (info && info.exe) await chrome.storage.local.set({ detectedExePath: info.exe });
  } catch {}
}

// ============================================================
// 测试导出面
// node tests 通过「去掉 import 行 + 前置展开 m3u8-parse.js + new Function」加载
// 本文件后取这里的 __test__。真实浏览器里模块作用域不外泄，无副作用。
// ============================================================
export const __test__ = {
  isCandidateURL,
  isPlaylistURL,
  makeFilename,
  buildGoCommand,
  // 转出共享模块的函数，让测试能断言 import 真的接上了（不是各留一份拷贝）
  parseDuration,
  parseSegments,
  parseVariants,
  shortQuality,
  qualityFromURL,
  sanitizeFileName,
  assembleGoCommand,
  fmtDur,
  escapeHtml,
};
