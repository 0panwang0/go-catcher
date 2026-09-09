// M3U8 Video Catcher - 下载器页面
// 所有下载在扩展页面内完成（浏览器真实 TLS 指纹 + 系统代理），无需 uTLS

const CONCURRENCY = 8;
const MAX_RETRIES = 3;
const ANALYZE_LIMIT = 12;    // 最多分析的嗅探条数
const MAIN_VIDEO_MIN = 60;   // 时长 >= 60s 判定为主视频

const $ = (sel) => document.querySelector(sel);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// isCandidateURL 与 background.js 同名实现保持一致（service worker 上下文
// 隔离无法共享）：过滤路径无媒体扩展名、靠 ?url= 参数尾部伪装 .m3u8 的
// 解析页假链接，点它下载拉回 HTML 网页任务必失败。
function isCandidateURL(u) {
  try {
    const p = new URL(u);
    return /\.(m3u8|mp4)$/i.test(p.pathname) || !p.searchParams.has("url");
  } catch {
    return false;
  }
}

let running = false;

document.addEventListener("DOMContentLoaded", async () => {
  // 如果是从页面选择面板跳转过来的，自动填入并下载该视频
  const params = new URLSearchParams(location.search);
  const autoUrl = params.get("url");
  const autoTitle = params.get("title") || "";
  const autoPage = params.get("pageUrl") || "";
  const autoQuality = params.get("quality") || "";
  const autoFormat = params.get("format") || "ts";
  if (autoUrl) {
    $("#manualUrl").value = autoUrl;
    $("#manualName").value = autoTitle;
    history.replaceState({}, "", location.pathname);
    if (autoFormat === "mp4") {
      downloadMP4(autoUrl, autoPage, autoTitle, autoQuality);
    } else {
      startDownload(autoUrl, autoPage, autoTitle, autoUrl, autoQuality);
    }
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
  // 嗅探列表变化时自动刷新（页面保持打开也能看到新视频）
  chrome.storage.onChanged.addListener((changes) => {
    if (changes.m3u8_list && !running) refreshList();
  });
});

// ============================================================
// 嗅探列表 + 主视频识别
// ============================================================
async function refreshList() {
  const { m3u8_list = [] } = await chrome.storage.local.get("m3u8_list");
  // 自愈过滤历史误录的解析页假链接（路径无 .m3u8 扩展名、靠 ?url= 参数尾部
  // 伪装）：点它下载拉回 HTML 网页，任务必失败
  const list = m3u8_list.filter(isCandidateURL);
  renderList(list);
  if (list.some((it) => !it.analyzed)) analyzeList(list);
}

async function analyzeList(list) {
  // 注入 Referer + Origin 规则（每个 host+来源页一条）
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
    // 分析完一条就保存并重绘，主视频尽早浮上来
    const { m3u8_list = [] } = await chrome.storage.local.get("m3u8_list");
    const cur = m3u8_list.find((it) => it.url === item.url);
    if (cur) Object.assign(cur, item);
    await chrome.storage.local.set({ m3u8_list });
    if (!running) renderList(m3u8_list);
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

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

// m3u8 总时长（秒）：累加 #EXTINF
function parseDuration(text) {
  let total = 0;
  for (const m of text.matchAll(/#EXTINF:([\d.]+)/g)) {
    total += parseFloat(m[1]);
  }
  return total;
}

function fmtDur(sec) {
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = Math.round(sec % 60);
  return h > 0
    ? `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`
    : `${m}:${String(s).padStart(2, "0")}`;
}

// ============================================================
// 下载主流程
// ============================================================
async function startDownload(m3u8URL, pageURL, videoName, forcedPlaylistURL = "", forcedQuality = "") {
  if (running) return;
  running = true;
  const btns = document.querySelectorAll("button");
  btns.forEach((b) => (b.disabled = true));

  const logBox = $("#log");
  logBox.style.display = "block";
  logBox.textContent = "";
  const log = (msg, cls) => {
    const line = document.createElement("div");
    if (cls) line.className = cls;
    line.textContent = `[${new Date().toLocaleTimeString()}] ${msg}`;
    logBox.appendChild(line);
    logBox.scrollTop = logBox.scrollHeight;
  };

  try {
    // 1. 注入 Referer + Origin 规则（扩展页 fetch 的 Origin 头会导致 CDN 403）
    const tab = await chrome.tabs.getCurrent();
    let host = "";
    try { host = new URL(m3u8URL).hostname; } catch {}
    if (tab && host) {
      await chrome.runtime.sendMessage({
        type: "setReferers",
        tabId: tab.id,
        pairs: [{ host, referer: pageURL || "" }],
      });
    }
    log(`已伪装 Referer/Origin: ${pageURL || "（无来源页，未伪装）"}`);

    // 2. 获取 m3u8；若是 master playlist 且未指定子播放列表，则解析画质让用户选
    let playlistURL = forcedPlaylistURL || m3u8URL;
    let qualityShort = forcedQuality || qualityFromURL(m3u8URL);
    let text = await fetchText(playlistURL);
    if (!forcedPlaylistURL && text.includes("#EXT-X-STREAM-INF")) {
      const variants = parseVariants(text, playlistURL);
      log(`Master playlist，检测到 ${variants.length} 个画质档位`);
      const choice = await pickQuality(variants);
      if (!choice) {
        log("已取消");
        return;
      }
      playlistURL = choice.url;
      qualityShort = shortQuality(choice);
      log(`已选择画质: ${choice.label}`);
      text = await fetchText(playlistURL);
    }

    // 3. 解析分片（接受任意扩展名，兼容 .jpeg 伪装分片）
    const segments = parseSegments(text, playlistURL);
    if (!segments.length) throw new Error("未解析到任何分片");
    log(`解析到 ${segments.length} 个分片`);

    // 4. 并发下载
    const parts = new Array(segments.length);
    let done = 0;
    let bytes = 0;
    const t0 = performance.now();
    showProgress(0, segments.length, 0, 0);

    let aborted = false;
    let firstErr = null;
    let cursor = 0;

    const worker = async () => {
      while (!aborted && cursor < segments.length) {
        const i = cursor++;
        try {
          const buf = await fetchSegment(segments[i]);
          parts[i] = buf;
          done++;
          bytes += buf.byteLength;
        } catch (e) {
          if (!firstErr) firstErr = `[${i}] ${e.message}`;
          aborted = true;
          return;
        }
        const secs = (performance.now() - t0) / 1000;
        showProgress(done, segments.length, bytes, secs);
      }
    };
    await Promise.all(Array.from({ length: CONCURRENCY }, worker));
    if (firstErr) throw new Error(`分片下载失败: ${firstErr}`);

    // 5. 合并保存
    log("下载完成，正在合并…");
    const blob = new Blob(parts, { type: "video/mp2t" });
    const filename = makeFilename(playlistURL, videoName, qualityShort, "ts");
    saveBlob(blob, filename);
    const secs = (performance.now() - t0) / 1000;
    log(`已保存 ${filename}（${(bytes / 1024 / 1024).toFixed(1)} MB，耗时 ${secs.toFixed(0)}s）`, "ok");
    log("提示：如果浏览器没有弹出保存框，请检查本页面右上角是否被拦截了多个文件下载", "ok");
  } catch (e) {
    log(`失败: ${e.message}`, "err");
    log("若持续 403/失败，请确认：1) Clash 已开启系统代理（Edge 跟随系统代理）；2) 下载的是主视频而非预览片", "err");
    console.error(e);
  } finally {
    running = false;
    btns.forEach((b) => (b.disabled = false));
    refreshList();
  }
}

// ============================================================
// m3u8 解析
// ============================================================
async function fetchText(url) {
  let lastErr;
  for (let a = 1; a <= MAX_RETRIES; a++) {
    try {
      const resp = await fetch(url, { credentials: "omit" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      return await resp.text();
    } catch (e) {
      lastErr = e;
      await sleep(500 * a);
    }
  }
  throw new Error(`获取 m3u8 失败: ${lastErr.message}`);
}

// master playlist：解析全部画质档位（对应 IDM 的画质识别原理）
// 每行 #EXT-X-STREAM-INF:BANDWIDTH=...,RESOLUTION=... 后跟子播放列表 URL
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
      label: variantLabel(res, bw, next),
    });
  }
  // 按码率从高到低排序
  list.sort((a, b) => b.bandwidth - a.bandwidth);
  if (!list.length) throw new Error("master playlist 中未找到子播放列表");
  return list;
}

// 生成画质标签，如 "1080P · 1920x1080 · 4.5 Mbps"
function variantLabel(resolution, bandwidth, uri) {
  let q = "";
  const m = String(uri).match(/(2160p|1440p|1080p|720p|480p|360p|240p)/i);
  if (m) {
    q = m[1].toUpperCase();
  } else if (resolution) {
    const h = parseInt(resolution.split("x")[1], 10);
    q = { 2160: "4K", 1440: "2K", 1080: "1080P", 720: "720P", 480: "480P", 360: "360P", 240: "240P" }[h] || resolution;
  } else if (bandwidth) {
    q = (bandwidth / 1e6).toFixed(1) + " Mbps";
  }
  const parts = [q || "未知画质"];
  if (resolution) parts.push(resolution);
  if (bandwidth) parts.push((bandwidth / 1e6).toFixed(1) + " Mbps");
  return parts.join(" · ");
}

function shortQuality(variant) {
  return String(variant.label || "").split(" · ")[0] || "";
}

// 从 URL 中提取画质标识（用于嗅探列表角标，如 /1080p/video.m3u8 → 1080P）
function qualityFromURL(u) {
  try {
    const m = decodeURIComponent(new URL(u).pathname).match(/(2160p|1440p|1080p|720p|480p|360p|240p)/i);
    if (m) return m[1].toUpperCase();
    const q = new URL(u).searchParams.get("quality");
    if (q) return q;
  } catch {}
  return "";
}

// 媒体播放列表：所有非注释行都是分片（兼容 .ts / .jpeg / .m4s 伪装）
function parseSegments(text, baseURL) {
  const list = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    list.push(new URL(line, baseURL).href);
  }
  return list;
}

// 画质选择器：等用户点选后 resolve 选中的档位（取消则 resolve null）
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
// 分片下载（带重试）
// ============================================================
async function fetchSegment(url) {
  let lastErr;
  for (let a = 1; a <= MAX_RETRIES; a++) {
    try {
      const resp = await fetch(url, { credentials: "omit" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      return await resp.arrayBuffer();
    } catch (e) {
      lastErr = e;
      await sleep(500 * a);
    }
  }
  throw new Error(`${url.split("/").pop()} ${lastErr.message}`);
}

// ============================================================
// 工具
// ============================================================
function showProgress(done, total, bytes, secs) {
  $("#progressBox").style.display = "block";
  const pct = total ? Math.round((done / total) * 100) : 0;
  $("#progressFill").style.width = pct + "%";
  const mb = (bytes / 1024 / 1024).toFixed(1);
  const speed = secs > 0.5 ? (bytes / 1024 / 1024 / secs).toFixed(1) : "--";
  $("#progressText").textContent =
    `下载进度: ${done} / ${total}（${pct}%）· ${mb} MB · ${speed} MB/s`;
}

// 文件名：优先 "视频名称_画质.扩展名"
// 默认 ts：HLS 原始流直接落盘（Go 侧不做封装），PotPlayer/VLC 可正常播放
function makeFilename(m3u8URL, videoName, quality, ext = "ts") {
  const suffix = quality ? `_${quality}` : "";
  if (videoName) {
    let clean = videoName
      .replace(/[\\/:*?"<>|]/g, "_")
      .replace(/\s+/g, " ")
      .trim()
      .slice(0, 80);
    if (clean) return clean + suffix + `.${ext}`;
  }
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
    if (parts.length) return parts.join("_") + suffix + `.${ext}`;
  } catch {}
  return `video_${Date.now()}${suffix}.${ext}`;
}

// MP4 直链下载
async function downloadMP4(url, pageURL, videoName, quality) {
  if (running) return;
  running = true;
  const btns = document.querySelectorAll("button");
  btns.forEach((b) => (b.disabled = true));

  const logBox = $("#log");
  logBox.style.display = "block";
  logBox.textContent = "";
  const log = (msg, cls) => {
    const line = document.createElement("div");
    if (cls) line.className = cls;
    line.textContent = `[${new Date().toLocaleTimeString()}] ${msg}`;
    logBox.appendChild(line);
    logBox.scrollTop = logBox.scrollHeight;
  };

  try {
    const tab = await chrome.tabs.getCurrent();
    let host = "";
    try { host = new URL(url).hostname; } catch {}
    if (tab && host) {
      await chrome.runtime.sendMessage({
        type: "setReferers",
        tabId: tab.id,
        pairs: [{ host, referer: pageURL || "" }],
      });
    }
    log(`已伪装 Referer/Origin: ${pageURL || "（无来源页，未伪装）"}`);
    log(`开始下载 MP4: ${url}`);

    const resp = await fetch(url, { credentials: "omit" });
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);

    const len = resp.headers.get("content-length");
    const totalMB = len ? (parseInt(len, 10) / 1024 / 1024).toFixed(1) : "?";
    log(`MP4 大小约 ${totalMB} MB`);

    const blob = await resp.blob();
    const filename = makeFilename(url, videoName, quality, "mp4");
    saveBlob(blob, filename);
    log(`已保存 ${filename}（${(blob.size / 1024 / 1024).toFixed(1)} MB）`, "ok");
  } catch (e) {
    log(`失败: ${e.message}`, "err");
  } finally {
    running = false;
    btns.forEach((b) => (b.disabled = false));
  }
}

function saveBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // 释放 blob URL（留足保存时间）
  setTimeout(() => URL.revokeObjectURL(url), 10 * 60 * 1000);
}

// 设置卡片顶部的服务状态指示：按当前配置端口 ping /health
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
async function detectExePath(port) {
  try {
    const resp = await fetch(`http://127.0.0.1:${port}/svc/info`, { cache: "no-store" });
    if (!resp.ok) return;
    const info = await resp.json().catch(() => null);
    if (info && info.exe) await chrome.storage.local.set({ detectedExePath: info.exe });
  } catch {}
}
