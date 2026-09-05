// M3U8 Video Catcher - IDM 式单视频下载按钮
// 鼠标悬停在某个 video 上时显示"下载该视频"按钮，点击后在页面内完成下载。
// 所有真实网络请求通过 MAIN world fetch 代理发出，Origin/Referer/Cookie 与页面一致，绕过 CDN 403。

(function () {
  if (window.__m3u8_catcher_injected__) return;
  window.__m3u8_catcher_injected__ = true;

  const MIN_W = 200;
  const MIN_H = 120;
  const SEGMENT_WINDOW_MS = 15000;
  const CONCURRENCY = 8;
  const MAX_RETRIES = 3;

  let currentBtn = null;
  let currentVideo = null;
  let panel = null;
  let activeTaskId = null; // 多任务并发：本次下载的 server 任务 id，轮询用
  let activePaused = false; // 当前任务是否处于暂停态（控制按钮切换用）
  let analyzing = false;
  let abortController = null;

  // ============================================================
  // MAIN world fetch 代理桥接
  // ============================================================
  let fetchId = 0;
  const fetchResolvers = new Map();

  function initBridge() {
    document.addEventListener("__m3u8_catcher_fetch_res__", (e) => {
      const { id, ok, data, error } = e.detail || {};
      const r = fetchResolvers.get(id);
      if (!r) return;
      fetchResolvers.delete(id);
      if (ok) r.resolve(data);
      else r.reject(new Error(error || "fetch failed"));
    });
  }

  function pageFetch(url, responseType = "text") {
    return new Promise((resolve, reject) => {
      const id = ++fetchId;
      fetchResolvers.set(id, { resolve, reject });
      document.dispatchEvent(
        new CustomEvent("__m3u8_catcher_fetch_req__", {
          detail: { id, url, responseType },
        })
      );
      setTimeout(() => {
        if (fetchResolvers.has(id)) {
          fetchResolvers.delete(id);
          reject(new Error("请求超时（可能被代理阻断或 CDN 无响应）"));
        }
      }, 60000);
    });
  }

  // ============================================================
  // 初始化 / 事件监听
  // ============================================================
  function init() {
    initBridge();
    document.addEventListener("mouseover", onHover, true);
    document.documentElement.addEventListener("mouseleave", hideButton);
    document.addEventListener("click", onDocumentClick, true);
  }

  function onHover(e) {
    if (currentBtn && (e.target === currentBtn || currentBtn.contains(e.target))) return;
    const video = findVideoAtPoint(e.clientX, e.clientY);
    if (video) showButton(video);
    else hideButton();
  }

  function findVideoAtPoint(x, y) {
    let stack = [];
    try {
      stack = document.elementsFromPoint(x, y) || [];
    } catch {
      return null;
    }
    for (const el of stack) {
      if (!(el instanceof HTMLVideoElement)) continue;
      const rect = el.getBoundingClientRect();
      if (rect.width < MIN_W || rect.height < MIN_H) continue;
      const style = getComputedStyle(el);
      if (style.display === "none" || style.visibility === "hidden") continue;
      return el;
    }
    return null;
  }

  function onDocumentClick(e) {
    if (panel && !panel.contains(e.target)) closePanel();
  }

  // ============================================================
  // 悬浮按钮
  // ============================================================
  function showButton(video) {
    if (currentVideo === video) return;
    hideButton();
    currentVideo = video;

    const btn = document.createElement("div");
    btn.className = "m3u8-catcher-btn";
    btn.innerHTML = `<span class="ic">⬇</span><span class="txt">下载该视频</span>`;
    Object.assign(btn.style, {
      position: "fixed",
      zIndex: "2147483647",
      display: "flex",
      alignItems: "center",
      gap: "4px",
      padding: "4px 10px",
      background: "linear-gradient(180deg, #4caf50, #2e9e44)",
      color: "#fff",
      fontSize: "12px",
      fontFamily: `"Segoe UI","Microsoft YaHei",sans-serif`,
      fontWeight: "600",
      borderRadius: "3px",
      boxShadow: "0 2px 6px rgba(0,0,0,0.3)",
      cursor: "pointer",
      userSelect: "none",
      lineHeight: "1.4",
      pointerEvents: "auto",
      whiteSpace: "nowrap",
    });

    btn.addEventListener("mouseenter", () => (btn.style.filter = "brightness(1.1)"));
    btn.addEventListener("mouseleave", () => (btn.style.filter = ""));
    btn.addEventListener("click", async (e) => {
      e.preventDefault();
      e.stopPropagation();
      await onDownloadClick(video);
    });

    document.body.appendChild(btn);
    currentBtn = btn;
    repositionButton();
  }

  function hideButton() {
    if (currentBtn) {
      currentBtn.remove();
      currentBtn = null;
    }
    currentVideo = null;
  }

  function repositionButton() {
    if (!currentBtn || !currentVideo) return;
    const rect = currentVideo.getBoundingClientRect();
    if (rect.width === 0 || rect.height === 0) {
      hideButton();
      return;
    }
    currentBtn.style.top = `${Math.max(rect.top, 0) + 8}px`;
    currentBtn.style.left = `${Math.max(rect.left, 0) + 8}px`;
  }

  window.addEventListener("scroll", repositionButton, true);
  window.addEventListener("resize", repositionButton);

  // ============================================================
  // 点击下载：只分析"这个视频"
  // ============================================================
  async function onDownloadClick(video) {
    if (analyzing) return;
    analyzing = true;
    showPanel("loading");

    let source = null; // hoisted: try/catch 是不同的块作用域，catch 里要读 source

    // Defensive: Extension context can be invalidated if user reloaded the
    // extension while this page is already open. Detect and bail with a
    // helpful message instead of just dumping a TypeError.
    if (!chrome?.runtime?.id) {
      showPanel("error", {
        message:
          "扩展上下文已失效，请刷新页面后重试（F5 或点地址栏回车）。" +
          "通常发生于重新加载扩展后、没刷新原网页。",
        m3u8Url: "",
        pageUrl: location.href,
        title: document.title || "",
      });
      analyzing = false;
      return;
    }

    try {
      const src = video.currentSrc || video.src || "";
      const pageUrl = location.href;
      const segmentDir = recentSegmentDir();

      const resp = await chrome.runtime.sendMessage({
        type: "getVideoSource",
        src,
        pageUrl,
        segmentDir,
        title: document.title || "",
      });

      if (!resp || !resp.ok || !resp.source) {
        showPanel("error", resp && resp.error ? resp.error : "未能识别该视频，请先播放一会儿再试。");
        return;
      }

      source = resp.source;

      if (source.variants && source.variants.length > 1) {
        renderVariantsPanel(source);
        return;
      }

      // 单画质：直接进确认面板
      renderConfirmPanel(source);
    } catch (e) {
      console.error("M3U8 Video Catcher analyze error:", e);
      showPanel("error", {
        message: String(e && e.message ? e.message : e),
        m3u8Url: source && source.url ? source.url : "",
        pageUrl: source && source.pageUrl ? source.pageUrl : location.href,
        title: (source && source.title) || document.title || "",
      });
    } finally {
      analyzing = false;
    }
  }

  async function startDownload(source) {
    if (abortController) abortController.abort();
    abortController = new AbortController();

    if (source.type === "mp4") {
      await downloadMP4(source.url, source.pageUrl, source.title, source.quality || "");
    } else {
      await downloadTS(source.url, source.pageUrl, source.title, source.quality || "");
    }
  }

  function recentSegmentDir() {
    try {
      const now = performance.now();
      const entries = performance.getEntriesByType("resource");
      const isPageAsset = /\.(m3u8|css|js|mjs|png|gif|webp|svg|ico|woff2?|ttf|json|html?|txt|xml)(\?|#|$)/i;
      let latest = null;
      for (const e of entries) {
        if (!/^https?:/i.test(e.name)) continue;
        if (isPageAsset.test(e.name)) continue;
        if (now - e.startTime > SEGMENT_WINDOW_MS) continue;
        if (!latest || e.startTime > latest.startTime) latest = e;
      }
      if (!latest) return "";
      const m = latest.name.match(/^(https?:\/\/[^?#]*\/)/);
      return m ? m[1] : "";
    } catch {
      return "";
    }
  }

  // ============================================================
  // 解析工具
  // ============================================================
  function resolveURL(base, rel) {
    try {
      return new URL(rel, base).href;
    } catch {
      return rel;
    }
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

  function parseDuration(text) {
    let total = 0;
    for (const m of text.matchAll(/#EXTINF:([\d.]+)/g)) {
      total += parseFloat(m[1]);
    }
    return total;
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
        label: variantLabel(next, res, bw),
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
      const map = { 2160: "4K", 1440: "2K", 1080: "1080P", 720: "720P", 480: "480P", 360: "360P", 240: "240P" };
      if (map[h]) return map[h];
    }
    if (bandwidth) return `${(bandwidth / 1e6).toFixed(1)} Mbps`;
    return "";
  }

  function variantLabel(uri, resolution, bandwidth) {
    const q = variantQuality(uri, resolution, bandwidth);
    const parts = [q || "未知画质"];
    if (resolution) parts.push(resolution);
    if (bandwidth) parts.push(`${(bandwidth / 1000).toFixed(0)} kbps`);
    return parts.join(" · ");
  }

  // ============================================================
  // TS 下载：m3u8 → 分片 → 合并
  // ============================================================
  async function downloadTS(m3u8URL, pageUrl, title, quality) {
    showPanel("progress", { title: title || "下载该视频", text: "正在获取播放列表…" });

    try {
      const playlistText = await pageFetch(m3u8URL, "text");
      // master playlist：弹出画质选择
      if (playlistText.includes("#EXT-X-STREAM-INF")) {
        const variants = parseVariants(playlistText, m3u8URL);
        if (variants.length > 1) {
          renderVariantsPanel({
            type: "ts",
            url: m3u8URL,
            title,
            pageUrl,
            variants,
          });
          return;
        }
        if (variants.length === 1) {
          return await downloadTSVariant(variants[0].url, pageUrl, title, variants[0].quality || quality);
        }
      }
      await downloadTSVariant(m3u8URL, pageUrl, title, quality);
    } catch (e) {
      // 浏览器侧拿不到 m3u8（典型 CORP/opaque 阻断）→ 直接出兜底面板，不再抛
      showPanel("error", {
        message: "获取播放列表失败: " + (e && e.message ? e.message : String(e)),
        m3u8Url: m3u8URL,
        pageUrl: pageUrl || location.href,
        title: title || "",
      });
    }
  }

  async function downloadTSVariant(playlistURL, pageUrl, title, quality) {
    showPanel("progress", { title: title || "下载该视频", text: "正在解析分片…" });
    const log = (msg) => updateProgressText(msg);

    let segments;
    try {
      const playlistText = await pageFetch(playlistURL, "text");
      segments = parseSegments(playlistText, playlistURL);
    } catch (e) {
      // 浏览器侧 fetch 失败 → 切到 Go 下载器方案
      showPanel("error", {
        message: "浏览器无法直接获取视频: " + e.message,
        m3u8Url: playlistURL,
        pageUrl: pageUrl || location.href,
        title,
      });
      return;
    }
    if (!segments.length) {
      showPanel("error", {
        message: "未解析到视频分片",
        m3u8Url: playlistURL,
        pageUrl,
        title,
      });
      return;
    }

    const duration = parseDuration(playlistText);
    log(`共 ${segments.length} 个分片，预计时长 ${fmtDur(duration)}，开始并发下载…`);

    const parts = await downloadSegments(segments, log, playlistURL, pageUrl, title);
    log("分片下载完成，正在合并…");
    const blob = new Blob(parts, { type: "video/mp2t" });
    const filename = makeFilename(playlistURL, title, quality, ".mp4");
    saveBlob(blob, filename);
    log(`已保存: ${filename}`);
    setTimeout(closePanel, 1200);
  }

  async function downloadSegments(urls, log, m3u8Url, pageUrl, title) {
    const parts = new Array(urls.length);
    let done = 0;
    let failed = 0;
    const queue = urls.map((url, idx) => ({ url, idx }));
    const workers = [];

    for (let w = 0; w < CONCURRENCY; w++) {
      workers.push(
        (async () => {
          while (true) {
            const job = queue.shift();
            if (!job) return;
            const { url, idx } = job;
            for (let attempt = 1; attempt <= MAX_RETRIES; attempt++) {
              try {
                if (abortController && abortController.signal.aborted) throw new Error("已取消");
                const buf = await pageFetch(url, "arraybuffer");
                parts[idx] = buf;
                done++;
                updateProgressBar(done / urls.length);
                if (done % 10 === 0 || done === urls.length) log(`下载进度 ${done}/${urls.length}`);
                break;
              } catch (e) {
                if (attempt === MAX_RETRIES) {
                  failed++;
                  throw new Error(`分片 ${idx + 1} 下载失败: ${e.message}`);
                }
                await sleep(500 * attempt);
              }
            }
          }
        })()
      );
    }

    try {
      await Promise.all(workers);
    } catch (e) {
      // 浏览器侧分片下载失败（CORS/CORP）→ 提示用户使用 Go 下载器
      showPanel("error", {
        message: `分片下载失败 (${e.message})，浏览器无法绕过 CDN 跨域保护。`,
        m3u8Url: m3u8Url || "",
        pageUrl: pageUrl || location.href,
        title: title || "",
      });
      throw e;
    }
    if (failed > 0) throw new Error(`${failed} 个分片下载失败`);
    return parts;
  }

  // ============================================================
  // MP4 直链下载
  // ============================================================
  async function downloadMP4(url, pageUrl, title, quality) {
    showPanel("progress", { title: title || "下载该视频", text: "正在下载 MP4…" });
    try {
      const buf = await pageFetch(url, "arraybuffer");
      const blob = new Blob([buf], { type: "video/mp4" });
      const filename = makeFilename(url, title, quality, ".mp4");
      saveBlob(blob, filename);
      updateProgressText(`已保存: ${filename}`);
      updateProgressBar(1);
      setTimeout(closePanel, 1200);
    } catch (e) {
      showPanel("error", {
        message: "MP4 下载失败: " + e.message,
        m3u8Url: url,
        pageUrl: pageUrl || location.href,
        title: title || "",
      });
    }
  }

  // ============================================================
  // 保存 / 文件名
  // ============================================================
  function saveBlob(blob, filename) {
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = filename;
    a.style.display = "none";
    document.body.appendChild(a);
    a.click();
    setTimeout(() => {
      URL.revokeObjectURL(a.href);
      a.remove();
    }, 1000);
  }

  function makeFilename(url, title, quality, ext) {
    const suffix = quality ? `_${quality}` : "";
    if (title) {
      const clean = String(title)
        .replace(/[\\/:*?"<>|]/g, "_")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 80);
      if (clean) return clean + suffix + ext;
    }
    try {
      const parts = new URL(url).pathname.split("/").filter((p) => p && !p.endsWith(".m3u8"));
      if (parts.length) return parts.join("_") + suffix + ext;
    } catch {}
    return `video_${Date.now()}${suffix}${ext}`;
  }

  function fmtDur(sec) {
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = Math.round(sec % 60);
    return h > 0
      ? `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`
      : `${m}:${String(s).padStart(2, "0")}`;
  }

  function sleep(ms) {
    return new Promise((r) => setTimeout(r, ms));
  }

  // ============================================================
  // 单视频选择面板
  // ============================================================
  function createPanel() {
    closePanel();
    panel = document.createElement("div");
    panel.id = "m3u8-catcher-panel";
    Object.assign(panel.style, {
      position: "fixed",
      top: "0",
      left: "0",
      right: "0",
      bottom: "0",
      zIndex: "2147483647",
      background: "rgba(0,0,0,0.35)",
      display: "flex",
      alignItems: "center",
      justifyContent: "center",
      fontFamily: `"Segoe UI","Microsoft YaHei",sans-serif`,
    });
    panel.addEventListener("click", (e) => {
      if (e.target === panel) closePanel();
    });
    document.body.appendChild(panel);
  }

  function panelShell(title, bodyHTML) {
    return `
      <div style="max-width:720px;width:90%;max-height:85vh;background:#fff;border-radius:8px;box-shadow:0 8px 32px rgba(0,0,0,0.35);display:flex;flex-direction:column;overflow:hidden;">
        <div style="display:flex;align-items:center;justify-content:space-between;padding:14px 18px;border-bottom:1px solid #e5e7eb;background:#f8f9fb;">
          <div style="font-size:16px;font-weight:700;color:#111;">${escapeHtml(title)}</div>
          <button id="m3u8-catcher-close" style="border:none;background:transparent;font-size:20px;color:#6b7280;cursor:pointer;line-height:1;">×</button>
        </div>
        ${bodyHTML}
      </div>
    `;
  }

  function showPanel(state, payload) {
    if (!panel) createPanel();
    if (state === "loading") {
      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:40px;text-align:center;color:#6b7280;">正在分析该视频的画质…</div>`
      );
    } else if (state === "error") {
      const errMsg = typeof payload === "string" ? payload : payload.message;
      const m3u8Url = payload.m3u8Url || "";
      const pageUrl = payload.pageUrl || location.href;
      const title = payload.title || document.title || "";
      const code = payload.code || "";

      // 用户取消 Save As：不算错误，直接关掉
      if (code === "USER_CANCEL") {
        closePanel();
        return;
      }

      const goCmd = buildGoCommand(m3u8Url, pageUrl, title);
      const outName = buildOutputName(m3u8Url, title);

      // 服务未启动：单独给出明确指引
      const serverDownBanner = code === "SERVER_DOWN"
        ? `<div style="background:#fef3c7;border-left:3px solid #f59e0b;padding:10px 14px;border-radius:4px;margin-bottom:14px;font-size:13px;color:#78350f;line-height:1.5;">
             <div style="font-weight:600;margin-bottom:4px;">⚠ 本地下载服务未启动</div>
             解决办法：打开 <code style="background:#fff;padding:1px 5px;border-radius:2px;font-family:Menlo,Consolas,monospace;">D:\\projects\\go_practice\\workspace\\go-catcher\\go-catcher.exe</code>（GoCatcher 客户端，打开即自动启动下载服务），之后视频下载就都是一键完成。
           </div>`
        : "";

      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:24px 28px 0;">
          ${serverDownBanner}
          <div style="color:#d32f2f;margin-bottom:12px;font-size:14px;line-height:1.5;">下载失败: ${escapeHtml(errMsg)}</div>
          <div style="color:#374151;font-size:13px;margin-bottom:8px;">也可以手动复制下面的命令到终端运行（不需要服务）：</div>
          ${outName ? `<div style="color:#374151;font-size:13px;margin-bottom:6px;">输出文件：<code style="background:#f3f4f6;padding:1px 5px;border-radius:3px;">${escapeHtml(outName)}</code></div>` : ""}
        </div>
         <div style="padding:0 28px 20px;">
           <div style="background:#1f2937;color:#e5e7eb;padding:12px;border-radius:6px;font-family:Menlo,Consolas,monospace;font-size:12px;line-height:1.6;word-break:break-all;white-space:pre-wrap;" id="go-cmd-text">${escapeHtml(goCmd)}</div>
           <div style="margin-top:10px;display:flex;gap:8px;align-items:center;">
             <button id="copy-go-cmd" style="padding:6px 14px;font-size:13px;border:none;background:#1652f0;color:#fff;border-radius:4px;cursor:pointer;font-weight:600;">复制命令</button>
             <span id="copy-feedback" style="color:#10b981;font-size:12px;display:none;">✓ 已复制到剪贴板</span>
           </div>
         </div>`
      );
      const btn = panel.querySelector("#copy-go-cmd");
      const fb = panel.querySelector("#copy-feedback");
      if (btn) {
        btn.addEventListener("click", async () => {
          try {
            await navigator.clipboard.writeText(goCmd);
            fb.style.display = "inline";
            setTimeout(() => (fb.style.display = "none"), 1800);
          } catch (e) {
            fb.textContent = "复制失败，请手动选择";
            fb.style.color = "#d32f2f";
            fb.style.display = "inline";
          }
        });
      }
    } else if (state === "progress") {
      const p = payload || {};
      panel.innerHTML = panelShell(
        p.title || "下载该视频",
        `<div style="padding:24px 28px;">
          <div id="m3u8-catcher-progress-text" style="margin-bottom:12px;color:#374151;font-size:13px;">${escapeHtml(p.text || "准备中…")}</div>
          <div style="width:100%;height:8px;background:#e5e7eb;border-radius:4px;overflow:hidden;">
            <div id="m3u8-catcher-progress-fill" style="width:0%;height:100%;background:#1652f0;transition:width .2s;"></div>
          </div>
          <div id="m3u8-catcher-progress-cancel" style="margin-top:14px;text-align:right;">
            <button style="padding:4px 12px;font-size:12px;border:1px solid #d1d5db;background:#fff;border-radius:4px;cursor:pointer;">取消</button>
          </div>
        </div>`
      );
      const cancel = panel.querySelector("#m3u8-catcher-progress-cancel button");
      if (cancel) {
        cancel.addEventListener("click", () => {
          if (abortController) abortController.abort();
          closePanel();
        });
      }
    } else if (state === "initiating") {
      // 正在请求 Go：弹文件夹框 + 启动 disk 下载（都是后台快速请求，理应很快返回）
      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:36px 28px;text-align:center;">
          <div style="font-size:14px;color:#374151;margin-bottom:14px;">正在准备…</div>
          <div style="display:inline-block;width:14px;height:14px;border:2px solid #1652f0;border-right-color:transparent;border-radius:50%;animation:m3u8spin 0.8s linear infinite;"></div>
          <div style="margin-top:16px;font-size:12px;color:#6b7280;">即将弹出文件夹选择框，请选择保存位置</div>
        </div>
        <style>@keyframes m3u8spin { to { transform: rotate(360deg); } }</style>`
      );
    } else if (state === "initiated") {
      // Go server 正在把视频直接落盘到所选目录
      const fn = (payload && payload.filename) || "video.mp4";
      const dir = (payload && payload.dir) || "";
      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:22px 28px;">
          <div style="color:#10b981;font-weight:600;font-size:14px;margin-bottom:10px;">✓ 已开始下载，由本地 Go 服务保存到所选文件夹</div>
          ${dir ? `<div style="font-size:12px;color:#6b7280;margin-bottom:12px;word-break:break-all;">保存位置：${escapeHtml(dir)}</div>` : ""}
          <div style="background:#f3f4f6;padding:8px 10px;border-radius:4px;font-size:12px;color:#374151;margin-bottom:14px;">
            文件：<code style="font-family:Menlo,Consolas,monospace;">${escapeHtml(fn)}</code>
          </div>
          <div id="initiated-progress-text" style="margin-bottom:10px;color:#374151;font-size:13px;">正在连接本地 Go 服务…</div>
          <div style="width:100%;height:8px;background:#e5e7eb;border-radius:4px;overflow:hidden;">
            <div id="initiated-progress-fill" style="width:0%;height:100%;background:#10b981;transition:width .2s;"></div>
          </div>
          <div id="initiated-controls" style="margin-top:14px;display:flex;gap:8px;align-items:center;justify-content:flex-end;"></div>
          <div style="margin-top:8px;font-size:11px;color:#9ca3af;">下载过程不经过浏览器下载栏，可在此暂停/继续或取消。</div>
        </div>`
      );
    } else if (state === "completed") {
      const fn = (payload && payload.filename) || "video.mp4";
      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:32px 28px;text-align:center;">
          <div style="font-size:32px;margin-bottom:10px;">🎉</div>
          <div style="color:#10b981;font-weight:600;font-size:16px;margin-bottom:8px;">下载完成</div>
          <div style="font-size:13px;color:#374151;margin-bottom:6px;">文件名：<code style="background:#f3f4f6;padding:2px 6px;border-radius:3px;font-family:Menlo,Consolas,monospace;">${escapeHtml(fn)}</code></div>
          ${payload && payload.finalPath ? `<div style="font-size:12px;color:#6b7280;margin-bottom:14px;word-break:break-all;">位置：${escapeHtml(payload.finalPath)}</div>` : ""}
          <button id="completed-close" style="margin-top:10px;padding:8px 20px;font-size:13px;border:none;background:#1652f0;color:#fff;border-radius:5px;cursor:pointer;">完成</button>
        </div>`
      );
      const doneBtn = panel.querySelector("#completed-close");
      if (doneBtn) doneBtn.addEventListener("click", closePanel);
    } else if (state === "cancelled") {
      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:32px 28px;text-align:center;">
          <div style="font-size:13px;color:#6b7280;margin-bottom:10px;">任务已取消，半成品文件已清理。</div>
          <button id="cancelled-close" style="padding:8px 20px;font-size:13px;border:none;background:#6b7280;color:#fff;border-radius:5px;cursor:pointer;">关闭</button>
        </div>`
      );
      const ccBtn = panel.querySelector("#cancelled-close");
      if (ccBtn) ccBtn.addEventListener("click", closePanel);
    }
    bindClose();
  }

  function updateProgressText(text, cls) {
    if (!panel) return;
    const el = panel.querySelector("#m3u8-catcher-progress-text");
    if (!el) return;
    el.textContent = text;
    if (cls === "error") el.style.color = "#d32f2f";
  }

  function updateProgressBar(ratio) {
    if (!panel) return;
    const el = panel.querySelector("#m3u8-catcher-progress-fill");
    if (!el) return;
    el.style.width = `${Math.max(0, Math.min(1, ratio)) * 100}%`;
  }

  function renderVariantsPanel(source) {
    if (!panel) createPanel();

    const rows = source.variants
      .map((v, idx) => {
        return `
        <div class="m3u8-catcher-row" data-idx="${idx}" style="display:flex;align-items:center;padding:10px 14px;border-bottom:1px solid #f3f4f6;cursor:pointer;">
          <div style="flex:1;min-width:0;">
            <div style="font-size:14px;color:#111;font-weight:600;">${escapeHtml(v.label)}</div>
            <div style="font-size:11px;color:#9ca3af;margin-top:2px;">${escapeHtml(v.codec)}</div>
          </div>
          <div style="margin-left:10px;padding:4px 10px;background:#1652f0;color:#fff;font-size:12px;border-radius:4px;font-weight:600;">下载</div>
        </div>
      `;
      })
      .join("");

    panel.innerHTML = panelShell(
      `下载该视频 - ${escapeHtml(source.title || "")}`,
      `<div style="overflow-y:auto;max-height:60vh;">${rows}</div>
       <div style="padding:12px 18px;border-top:1px solid #e5e7eb;background:#f8f9fb;color:#6b7280;font-size:12px;">点击对应画质即可开始下载。下载过程中可以选择保存位置。</div>`
    );

    bindClose();
    panel.querySelectorAll(".m3u8-catcher-row").forEach((row) => {
      row.addEventListener("mouseenter", () => (row.style.background = "#f0f7ff"));
      row.addEventListener("mouseleave", () => (row.style.background = "transparent"));
      row.addEventListener("click", () => {
        const idx = parseInt(row.dataset.idx, 10);
        const variant = source.variants[idx];
        if (variant) {
          // 选画质后进入确认面板，由用户再次确认 URL + 触发下载
          renderConfirmPanel({
            url: variant.url,
            type: source.type || "ts",
            quality: variant.quality || "",
            title: source.title,
            pageUrl: source.pageUrl,
          });
        }
      });
    });
  }

  // ============================================================
  // 确认面板：展示链接供核验，确认后让用户选位置，再交给本地 Go 服务下载
  // ============================================================
  function renderConfirmPanel(source) {
    if (!panel) createPanel();

    const url = source.url || "";
    const title = source.title || document.title || "";
    const quality = source.quality || "";
    const filename = buildOutputName(url, title) || "video.mp4";

    panel.innerHTML = panelShell(
      "下载该视频",
      `<div style="padding:20px 24px;">
        <div style="margin-bottom:14px;">
          <div style="font-size:12px;color:#6b7280;margin-bottom:3px;">视频名称</div>
          <div style="font-weight:600;color:#111;">${escapeHtml(title || "(无标题)")}</div>
        </div>

        ${quality ? `<div style="margin-bottom:14px;">
          <div style="font-size:12px;color:#6b7280;margin-bottom:3px;">画质</div>
          <div style="font-weight:600;color:#111;">${escapeHtml(quality)}</div>
        </div>` : ""}

        <div style="margin-bottom:14px;">
          <div style="font-size:12px;color:#6b7280;margin-bottom:3px;">资源链接</div>
          <div style="background:#f3f4f6;padding:10px;border-radius:4px;font-family:Menlo,Consolas,monospace;font-size:11px;line-height:1.5;word-break:break-all;max-height:90px;overflow-y:auto;color:#1f2937;">${escapeHtml(url)}</div>
          <button id="confirm-copy-url" style="margin-top:6px;padding:5px 12px;font-size:12px;border:1px solid #d1d5db;background:#fff;border-radius:4px;cursor:pointer;color:#374151;">📋 复制链接</button>
          <span id="confirm-copy-feedback" style="margin-left:8px;color:#10b981;font-size:12px;display:none;">✓ 已复制</span>
        </div>

        <div style="margin-bottom:18px;">
          <div style="font-size:12px;color:#6b7280;margin-bottom:3px;">保存文件名</div>
          <div style="background:#fef3c7;padding:8px 10px;border-radius:4px;font-family:Menlo,Consolas,monospace;font-size:12px;color:#78350f;">${escapeHtml(filename)}</div>
          <div style="margin-top:4px;font-size:11px;color:#9ca3af;">下载时可改名字 / 改位置</div>
        </div>

        <div style="display:flex;gap:10px;">
          <button id="confirm-cancel" style="flex:1;padding:11px;border:1px solid #d1d5db;background:#fff;border-radius:5px;cursor:pointer;font-size:14px;color:#374151;">取消</button>
          <button id="confirm-go" style="flex:2;padding:11px;border:none;background:#10b981;color:#fff;border-radius:5px;cursor:pointer;font-size:14px;font-weight:600;">⬇ 确认下载</button>
        </div>

        <div style="margin-top:14px;padding:10px 12px;background:#f0f9ff;border-radius:5px;font-size:12px;color:#0c4a6e;line-height:1.5;">
          💡 点击"确认下载"后会弹出 Windows"选择文件夹"框，选好后由本地 Go 程序直接下载并保存到该文件夹。<br>
          <span style="color:#0369a1;">首次使用需先打开一次 <code style="background:#fff;padding:1px 4px;border-radius:2px;">go-catcher.exe</code>（GoCatcher 客户端，打开即自动启动下载服务）。</span>
        </div>
      </div>`
    );

    bindClose();

    const cancelBtn = panel.querySelector("#confirm-cancel");
    const goBtn = panel.querySelector("#confirm-go");
    const copyBtn = panel.querySelector("#confirm-copy-url");
    const copyFb = panel.querySelector("#confirm-copy-feedback");

    if (cancelBtn) cancelBtn.addEventListener("click", closePanel);
    if (copyBtn) {
      copyBtn.addEventListener("click", async () => {
        try {
          await navigator.clipboard.writeText(url);
          copyFb.style.display = "inline";
          setTimeout(() => (copyFb.style.display = "none"), 1800);
        } catch (e) {
          copyFb.textContent = "复制失败";
          copyFb.style.color = "#d32f2f";
          copyFb.style.display = "inline";
        }
      });
    }
    if (goBtn) {
      goBtn.addEventListener("click", () => triggerDownload(source));
    }
  }

  // 点击"确认下载" → 后台请求 Go：先弹文件夹框选目录，再让 Go 直接落盘下载
  async function triggerDownload(source) {
    showPanel("initiating", source);

    try {
      const resp = await chrome.runtime.sendMessage({
        type: "downloadViaServer",
        m3u8Url: source.url,
        referer: source.pageUrl || location.href,
        title: source.title || document.title || "",
        quality: source.quality || "",
        filename: buildOutputName(source.url, source.title || document.title || ""),
      });

      if (!resp) throw new Error("扩展后台未响应");
      if (!resp.ok) {
        showPanel("error", {
          message: resp.error || "下载启动失败",
          code: resp.code || "",
          m3u8Url: source.url,
          pageUrl: source.pageUrl || location.href,
          title: source.title || document.title || "",
        });
        return;
      }

      // 目录已选好，Go 已开始落盘 → 进入进度面板
      // 多任务并发：记录本次任务 id，供后续轮询精确定位（多个标签页各下各的互不串扰）
      activeTaskId = resp.taskId || null;
      activePaused = false;
      showPanel("initiated", {
        ...source,
        filename: resp.filename || buildOutputName(source.url, source.title || document.title || ""),
        dir: resp.dir || "",
      });

      // 轮询 server /status 显示真实进度
      trackDownload(source);
    } catch (e) {
      console.error("download trigger error:", e);
      showPanel("error", {
        message: String(e && e.message ? e.message : e),
        m3u8Url: source.url,
        pageUrl: source.pageUrl || location.href,
        title: source.title || document.title || "",
      });
    }
  }

  // 追踪 Go server 下载进度（轮询 /status），把阶段/百分比回写到面板，并按暂停态切换控制按钮
  function trackDownload(source) {
    const interval = setInterval(async () => {
      try {
        const resp = await chrome.runtime.sendMessage({ type: "queryDownload", taskId: activeTaskId });
        if (!resp || !resp.exists) return; // server 还没就绪或短暂不可达，继续轮询
        const state = resp.state;
        if (state === "inProgress" || state === "paused") {
          const stage = resp.stage || "下载中";
          const pct = typeof resp.pct === "number" ? resp.pct / 100 : 0;
          if (activePaused !== !!resp.paused) {
            activePaused = !!resp.paused;
            renderInitiatedControls();
          }
          updateInitiatedProgress(pct, stage, resp);
        } else if (state === "complete") {
          clearInterval(interval);
          showPanel("completed", {
            ...source,
            finalPath: resp.finalPath,
            filename: resp.filename,
          });
        } else if (state === "error" || state === "interrupted" || state === "canceled") {
          clearInterval(interval);
          if (state === "canceled") {
            showPanel("cancelled");
          } else {
            showPanel("error", {
              message: `下载失败：${resp.error || ""}`,
              m3u8Url: source.url,
              pageUrl: source.pageUrl || location.href,
              title: source.title || document.title || "",
            });
          }
        }
      } catch (e) {
        // 通讯失败，忽略继续轮询
      }
    }, 700);

    const stopTracking = () => clearInterval(interval);
    if (panel) {
      const closeBtn = panel.querySelector("#m3u8-catcher-close");
      if (closeBtn) closeBtn.addEventListener("click", stopTracking, { once: true });
    }
    renderInitiatedControls();
  }

  // 渲染进度面板底部的 暂停/继续 + 取消 控制按钮
  function renderInitiatedControls() {
    const wrap = panel && panel.querySelector("#initiated-controls");
    if (!wrap) return;
    if (activePaused) {
      wrap.innerHTML =
        '<button data-ctl="resume" style="padding:6px 16px;font-size:13px;border:none;background:#10b981;color:#fff;border-radius:5px;cursor:pointer;font-weight:600;">▶ 继续下载</button>' +
        '<button data-ctl="cancel" style="padding:6px 16px;font-size:13px;border:1px solid #e5e7eb;background:#fff;color:#374151;border-radius:5px;cursor:pointer;">✕ 取消</button>';
    } else {
      wrap.innerHTML =
        '<button data-ctl="pause" style="padding:6px 16px;font-size:13px;border:1px solid #e5e7eb;background:#fff;color:#374151;border-radius:5px;cursor:pointer;">⏸ 暂停</button>' +
        '<button data-ctl="cancel" style="padding:6px 16px;font-size:13px;border:none;background:#ef4444;color:#fff;border-radius:5px;cursor:pointer;font-weight:600;">✕ 取消</button>';
    }
    wrap.querySelectorAll("button[data-ctl]").forEach((btn) => {
      btn.addEventListener("click", () => {
        sendControl(btn.dataset.ctl);
      });
    });
  }

  // 向 Go server 发送 暂停/继续/取消（经 background 代理）；立即切换本地按钮态，等 poll 确认
  async function sendControl(action) {
    const btn = panel && panel.querySelector(`#initiated-controls button[data-ctl="${action}"]`);
    if (btn) { btn.disabled = true; btn.style.opacity = ".5"; }
    try {
      const resp = await chrome.runtime.sendMessage({ type: "controlTask", taskId: activeTaskId, action });
      if (!resp || !resp.ok) {
        // 失败：提示并恢复按钮
        const errMsg = (resp && resp.error) || "操作失败";
        updateInitiatedProgress(0, `操作失败：${errMsg}`, {});
        renderInitiatedControls();
        return;
      }
      // pause → 立即进暂停态（省一次 poll）；resume → 立即回到下载态
      if (action === "pause") { activePaused = true; renderInitiatedControls(); updateInitiatedProgress(0, "已暂停，点击继续可恢复", {}); }
      else if (action === "resume") { activePaused = false; renderInitiatedControls(); }
      else if (action === "cancel") { activePaused = false; renderInitiatedControls(); }
    } catch (e) {
      renderInitiatedControls();
    }
  }

  // ratio∈[0,1]；stage 为 Go server 侧阶段文案（解析/下载分片/合并…）
  function updateInitiatedProgress(ratio, stage, extra) {
    const fill = panel && panel.querySelector("#initiated-progress-fill");
    const text = panel && panel.querySelector("#initiated-progress-text");
    if (fill) fill.style.width = `${Math.max(0, Math.min(1, ratio)) * 100}%`;
    if (text) {
      let t = stage || "下载中";
      const segTot = extra && extra.segTot;
      if (segTot > 0) {
        const segDone = (extra && extra.segDone) || 0;
        t += `（${segDone}/${segTot} 分片）`;
      }
      if (ratio > 0 && ratio < 1 && segTot > 0) {
        t += ` ${Math.round(ratio * 100)}%`;
      }
      text.textContent = t;
    }
  }

  function fmtBytes(n) {
    if (!n) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
  }

  function bindClose() {
    const close = panel.querySelector("#m3u8-catcher-close");
    if (close) close.addEventListener("click", closePanel);
  }

  function closePanel() {
    if (panel) {
      panel.remove();
      panel = null;
    }
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"}[c]));
  }

  // 生成跨 PowerShell / Git Bash / CMD 可直接粘贴运行的下载命令
  // 用 ; 而非 && 分隔：Windows PowerShell 5.1 不支持 && 作语句分隔符
  // 路径加双引号：bash 下不加引号会把位置参数按空白切片
  function buildGoCommand(m3u8Url, pageUrl, title) {
    // 直接调用单文件 exe（三种模式之一：--url= 直下，无需先起服务）
    const goArgs = ['"D:\\projects\\go_practice\\workspace\\go-catcher\\go-catcher.exe"'];
    if (m3u8Url) goArgs.push(`--url="${m3u8Url}"`);
    if (pageUrl) goArgs.push(`--referer="${pageUrl}"`);

    // -o 输出文件名：视频标题 + 画质，跟浏览器端 makeFilename 保持一致
    const outName = buildOutputName(m3u8Url, title);
    if (outName) goArgs.push(`-o "${outName}"`);

    return [
      "chcp 65001",
      goArgs.join(" "),
    ].join(" ; ");
  }

  // 拼输出文件名：<标题>_<画质>.mp4
  // 画质从 m3u8 URL 里推断（.../1080p/video.m3u8 或 xxx_720p.m3u8）
  function buildOutputName(m3u8Url, title) {
    let name = String(title || "").trim();

    // 标题拿不到就退回用 URL 里的视频 ID 段
    // 注意跳过画质段（/1080p/），否则会生成 "1080p_1080P.mp4" 这种重复且无意义的名字
    if (!name && m3u8Url) {
      try {
        const segments = new URL(m3u8Url).pathname
          .split("/")
          .map((p) => p.replace(/\.m3u8$/i, "")) // 剥掉 .m3u8 后缀（保留下划线开头的片段名）
          .filter(
            (p) =>
              p &&
              !/^(2160p|1440p|1080p|720p|480p|360p|240p)$/i.test(p)
          );
        // 倒着找第一个还有内容的段（兼容域名前缀空段 + 单段 URL）
        for (let i = segments.length - 1; i >= 0; i--) {
          if (segments[i] && segments[i].trim()) {
            name = segments[i];
            break;
          }
        }
        if (!name && segments[0]) name = segments[0];
      } catch {}
    }
    if (!name) return "";

    // 清掉 Windows 文件名非法字符，并把替换留下的碎屑归并干净
    // （例："Bad/Name: with* x" → "Bad_Name_with_x"，而不是 "Bad_Name_ with_ x"）
    name = name
      .replace(/[\\/:*?"<>|]/g, "_")
      .replace(/_{2,}/g, "_")
      .replace(/\s*_\s*/g, "_")
      .replace(/\s+/g, " ")
      .replace(/^[_\s]+|[_\s]+$/g, "")
      .trim()
      .slice(0, 80);
    if (!name) return "";

    // 画质后缀（输出 mp4：Go 侧检测到 ffmpeg 会做无损重封装，没装也能播）
    const q = qualityFromUrl(m3u8Url);
    return name + (q ? `_${q}` : "") + ".mp4";
  }

  // 从 URL 里抠画质档位：优先路径里的 /1080p/，其次 xxx_720p.m3u8
  function qualityFromUrl(u) {
    if (!u) return "";
    try {
      const path = decodeURIComponent(new URL(u).pathname);
      const m =
        path.match(/\/(2160p|1440p|1080p|720p|480p|360p|240p)\//i) ||
        path.match(/[_-](2160p|1440p|1080p|720p|480p|360p|240p)\.m3u8/i);
      if (m) return m[1].toUpperCase();
    } catch {}
    return "";
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
