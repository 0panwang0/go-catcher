// M3U8 Video Catcher - IDM 式单视频下载按钮
// 鼠标悬停在某个 video 上时显示"下载该视频"按钮，点击后在页面内完成下载。
// 所有真实网络请求通过 MAIN world fetch 代理发出，Origin/Referer/Cookie 与页面一致，绕过 CDN 403。

(function () {
  if (window.__m3u8_catcher_injected__) return;
  window.__m3u8_catcher_injected__ = true;

  const MIN_W = 200;
  const MIN_H = 120;
  const BTN_GAP = 8; // 按钮与视频边缘的间距
  const SRC_RECHECK_MS = 900; // "暂无链接"视频的重复检查间隔（播放后嗅探需要时间）
  const MEDIA_RECHECK_MS = 700; // 指针停在媒体上时的复查周期（应对 DOM 重建/链接晚到）
  const SEGMENT_WINDOW_MS = 15000;
  const CONCURRENCY = 8;
  const MAX_RETRIES = 3;
  const DEBUG = false; // true 时在页面控制台输出悬停判定日志（排查按钮不出现用）

  function dbg(...args) {
    if (DEBUG) console.log("[M3U8 Catcher]", ...args);
  }

  let currentBtn = null;
  // currentTarget 悬停目标：{el, src, pageUrl}。
  //   video 场景：本 frame 内的 video 元素，pageUrl = location.href
  //   iframe 场景：跨域播放器 iframe（video 在 frame 内，本 frame 拿不到），
  //     pageUrl = iframe.src —— 恰好等于后台嗅探记录的 frameUrl（发起请求的
  //     frame 文档 URL），可直接按 frameUrl 匹配候选
  let currentTarget = null;
  let deniedEl = null; // 最近一次判定"暂无链接"的目标元素（节流重查用）
  let deniedAt = 0;
  let gateToken = 0; // 悬停检查令牌：仅最新一次悬停的结果生效
  let gatePending = null; // 正在检查链接的目标元素
  let lastPointer = { x: -1, y: -1 }; // 最近一次指针位置（复查用）
  let recheckTimer = null;
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
    // 记录指针位置：mediaRecheck 定时器据此在「指针没动但页面变了」时补显示按钮
    document.addEventListener(
      "mousemove",
      (e) => {
        lastPointer.x = e.clientX;
        lastPointer.y = e.clientY;
      },
      true
    );
    document.documentElement.addEventListener("mouseleave", hideButton);
    document.addEventListener("click", onDocumentClick, true);
    startMediaRecheck();
  }

  // startMediaRecheck 周期性复查「指针当前压着的那块媒体」并保证按钮状态正确。
  // 只靠 mouseover 一次性判定有两个硬伤：
  //   1) 页面在指针静止时重建了 player（iframe 被替换 / 站点提示层重排），
  //      旧的 currentTarget 变成游离节点，按钮跟着消失且不再回来；
  //   2) 首帧嗅探还没落库（分片/playlist 请求比 hover 晚），一次性判定失败后
  //      就再没有第二次机会（除非常用户再次移动鼠标触发 mouseover）。
  // 定时复查用「坐标」而不是「元素引用」重新定位目标，天然免疫元素重建。
  function startMediaRecheck() {
    if (recheckTimer) return;
    recheckTimer = setInterval(() => {
      const { x, y } = lastPointer;
      if (x < 0 || y < 0) return;
      let target = null;
      try {
        target = findMediaTargetAtPoint(x, y, true);
      } catch {
        return;
      }
      if (!target) return; // 指针不在媒体上：交给 mouseover/hideButton 处理
      if (currentTarget && currentTarget.el === target.el && target.el.isConnected) return;
      dbg("recheck 命中媒体，重新判定按钮", target.el.tagName, target.pageUrl);
      showButtonIfSniffed(target);
    }, MEDIA_RECHECK_MS);
  }

  function onHover(e) {
    if (currentBtn && (e.target === currentBtn || currentBtn.contains(e.target))) return;
    // 按钮位于目标外部上方：光标在「目标矩形向上扩展的条带」内时保持按钮，
    // 否则鼠标一离开目标边界按钮就会消失，根本来不及点
    if (inButtonZone(e.clientX, e.clientY)) return;
    const target = findMediaTargetAtPoint(e.clientX, e.clientY);
    if (target) showButtonIfSniffed(target);
    else hideButton();
  }

  // findMediaTargetAtPoint 坐标处找可下载目标：优先 video 元素（本 frame 内，
  // src 直链 + 分片目录信号都可用）；没有 video 再找大尺寸 iframe（跨域播放器，
  // video 在 frame 内拿不到，但 iframe.src 就是后台记录的 frameUrl，可直接匹配）。
  // 广告/统计类小 iframe 被尺寸门槛（MIN_W×MIN_H）挡掉。
  //
  // allowGeom=false 时只做命中测试（mouseover 高频路径，必须便宜）；
  // allowGeom=true 时再用几何扫描兜底（仅定时复查调用，700ms 一次）。
  //   站点在播放器上盖一层透明层（提示层 / 点击劫持广告层 / 弹幕 canvas）时，
  //   底下的 video / iframe 不是遮挡层的祖先，命中测试永远拿不到它们；几何扫描
  //   只比矩形，透过任何遮挡层都能命中。
  function findMediaTargetAtPoint(x, y, allowGeom = false) {
    let stack = [];
    try {
      stack = document.elementsFromPoint(x, y) || [];
    } catch {
      stack = [];
    }
    const direct = pickTargetFromStack(stack);
    if (direct) return direct;
    if (!allowGeom) return null;
    return pickTargetByRect(x, y);
  }

  // pickTargetFromStack 命中栈里找目标：video 优先（本 frame 直连），其次 iframe
  function pickTargetFromStack(stack) {
    for (const el of stack) {
      if (el instanceof HTMLVideoElement && isUsableMediaElement(el)) {
        return { el, src: el.currentSrc || el.src || "", pageUrl: location.href };
      }
    }
    for (const el of stack) {
      if (el instanceof HTMLIFrameElement && isUsableMediaElement(el)) {
        return { el, src: "", pageUrl: el.src };
      }
    }
    return null;
  }

  // pickTargetByRect 几何兜底：遍历文档里的 video/iframe，取「包含该坐标且面积
  // 最小」的一个（最小面积 = 最内层、最贴近该点的那块播放器）。
  function pickTargetByRect(x, y) {
    let best = null;
    let bestArea = Infinity;
    let nodes = [];
    try {
      nodes = document.querySelectorAll("video, iframe");
    } catch {
      return null;
    }
    for (const el of nodes) {
      if (!isUsableMediaElement(el)) continue;
      const r = el.getBoundingClientRect();
      if (x < r.left || x > r.right || y < r.top || y > r.bottom) continue;
      const area = r.width * r.height;
      if (area >= bestArea) continue;
      bestArea = area;
      best = el;
    }
    if (!best) return null;
    return best instanceof HTMLVideoElement
      ? { el: best, src: best.currentSrc || best.src || "", pageUrl: location.href }
      : { el: best, src: "", pageUrl: best.src };
  }

  // isUsableMediaElement 可下载媒体元素的基本门槛：在文档里、尺寸够大、可见、
  // iframe 必须是 http(s)（about:blank / javascript: 无匹配价值）。
  function isUsableMediaElement(el) {
    if (!el || !el.isConnected) return false;
    const rect = el.getBoundingClientRect();
    if (rect.width < MIN_W || rect.height < MIN_H) return false;
    const style = getComputedStyle(el);
    if (style.display === "none" || style.visibility === "hidden") return false;
    if (el instanceof HTMLIFrameElement && !/^https?:/i.test(el.src || "")) return false;
    return true;
  }

  function onDocumentClick(e) {
    if (panel && !panel.contains(e.target)) closePanel();
  }

  // ============================================================
  // 悬浮按钮
  // ============================================================
  // showButtonIfSniffed 先确认已嗅探到该目标（video 或 iframe 播放器）的链接再
  // 显示按钮：没有链接就不显示（而不是点了才报"暂未嗅探到该视频"）。
  // 判定用 getVideoSource 的候选匹配（与列表数据同源于同站嗅探记录）：
  //   video  → pageUrl = 本 frame URL（含跨域解析页 frame 内直接命中）
  //   iframe → pageUrl = iframe.src（顶层 frame，匹配后台记录的 frameUrl）
  //           另外把 src 里 ?url= 编码的目标地址作为 embedUrl 传下去，供后台
  //           精确命中（解析页跳转到别的主机播放时，同站匹配会全部落空）
  async function showButtonIfSniffed(target) {
    if (currentTarget && currentTarget.el === target.el) return;
    if (gatePending === target.el) return; // 同一目标的检查在途
    if (deniedEl === target.el && Date.now() - deniedAt < SRC_RECHECK_MS) return;

    const token = ++gateToken;
    gatePending = target.el;
    let found = false;
    try {
      if (chrome?.runtime?.id) {
        const resp = await chrome.runtime.sendMessage({
          type: "getVideoSource",
          src: target.src,
          pageUrl: target.pageUrl,
          embedUrl: extractEmbedUrl(target),
          segmentDir: target.src ? recentSegmentDir() : "",
          title: document.title || "",
        });
        found = !!(resp && resp.ok && resp.source);
      }
    } catch (e) {
      // 扩展上下文失效等异常：按无链接处理
      dbg("getVideoSource 查询异常", e);
    }
    if (gatePending === target.el) gatePending = null;
    if (token !== gateToken) return; // 期间鼠标已移到别的目标
    if (found) {
      dbg("命中链接，显示按钮", target.pageUrl);
      showButton(target);
    } else {
      dbg("未嗅探到该目标，暂不显示按钮", target.pageUrl);
      deniedEl = target.el;
      deniedAt = Date.now();
    }
  }

  // extractEmbedUrl 解析页 iframe 的 src 常形如
  //   https://parser.example/play/?url=<目标地址>
  // 取出 ?url= 里的目标地址（常见的苹果CMS 解析页签名）。解析页往往会跳到
  // 另一个主机去播放，后台按 frameUrl 同站匹配就会落空，这个参数是最可靠的锚点。
  function extractEmbedUrl(target) {
    if (!target || target.src || !target.pageUrl) return "";
    let u;
    try {
      u = new URL(target.pageUrl);
    } catch {
      return "";
    }
    for (const key of ["url", "v", "vid", "video"]) {
      const q = u.searchParams.get(key);
      if (q && /^https?:/i.test(q)) return q;
    }
    return "";
  }

  function showButton(target) {
    if (currentTarget && currentTarget.el === target.el) return;
    hideButton();
    currentTarget = target;

    const btn = document.createElement("div");
    btn.className = "m3u8-catcher-btn";
    btn.innerHTML = `<span class="ic">⬇</span><span class="txt">下载该视频</span>`;
    Object.assign(btn.style, {
      position: "fixed",
      zIndex: "2147483647",
      display: "flex",
      alignItems: "center",
      gap: "4px",
      padding: "7px 13px",
      background: "#3b82f6",
      color: "#fff",
      fontSize: "12.5px",
      fontFamily: `"Segoe UI","Microsoft YaHei",sans-serif`,
      fontWeight: "600",
      borderRadius: "9px",
      boxShadow: "0 2px 6px rgba(0,0,0,0.3)",
      cursor: "pointer",
      userSelect: "none",
      lineHeight: "1.4",
      pointerEvents: "auto",
      whiteSpace: "nowrap",
      transition: "filter .15s",
    });

    btn.addEventListener("mouseenter", () => (btn.style.filter = "brightness(1.15)"));
    btn.addEventListener("mouseleave", () => (btn.style.filter = ""));
    btn.addEventListener("click", async (e) => {
      e.preventDefault();
      e.stopPropagation();
      await onDownloadClick(target);
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
    currentTarget = null;
  }

  function repositionButton() {
    if (!currentBtn || !currentTarget) return;
    // 目标被页面重建（播放器重挂载）后引用会变成游离节点，矩形全为 0：
    // 先收起，交给 mediaRecheck 按坐标重新定位、重新显示
    if (!currentTarget.el.isConnected) {
      hideButton();
      return;
    }
    const rect = currentTarget.el.getBoundingClientRect();
    if (rect.width === 0 || rect.height === 0) {
      hideButton();
      return;
    }
    // 首选：视频左上角正上方（视频外部），不遮挡画面
    const btnH = currentBtn.offsetHeight || 0;
    const outsideTop = Math.max(rect.top, 0) - btnH - BTN_GAP;
    if (outsideTop >= 0) {
      currentBtn.style.top = `${outsideTop}px`;
      currentBtn.style.left = `${Math.max(rect.left, 0)}px`;
      return;
    }
    // 上方空间不足（视频贴近视口顶端）：回退到视频内部左上角
    currentBtn.style.top = `${Math.max(rect.top, 0) + BTN_GAP}px`;
    currentBtn.style.left = `${Math.max(rect.left, 0) + BTN_GAP}px`;
  }

  // inButtonZone 光标是否处于「目标矩形向上扩展一个按钮高度的条带」内。
  // 按钮放在目标外部上方后，这是鼠标从目标移到按钮的必经之路。
  function inButtonZone(x, y) {
    if (!currentBtn || !currentTarget) return false;
    const rect = currentTarget.el.getBoundingClientRect();
    const btnH = currentBtn.offsetHeight || 0;
    return (
      x >= rect.left - BTN_GAP &&
      x <= rect.right + BTN_GAP &&
      y >= rect.top - btnH - BTN_GAP * 2 &&
      y <= rect.bottom + BTN_GAP
    );
  }

  window.addEventListener("scroll", repositionButton, true);
  window.addEventListener("resize", repositionButton);

  // ============================================================
  // 点击下载：只分析"这个视频"
  // ============================================================
  async function onDownloadClick(target) {
    if (analyzing) return;
    analyzing = true;
    showPanel("loading");

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
      const src = target.src;
      const pageUrl = target.pageUrl;

      // IDM 式：取"该视频"的全部候选链接，再在页面主世界解析出各画质/格式
      const resp = await chrome.runtime.sendMessage({
        type: "getVideoSources",
        src,
        pageUrl,
        embedUrl: extractEmbedUrl(target),
      });
      if (!resp || !resp.ok || !Array.isArray(resp.sources) || !resp.sources.length) {
        showPanel("error", (resp && resp.error) || "未能识别该视频，请先播放一会儿再试。");
        return;
      }

      // iframe 场景拿不到 frame 内的 video 元素，画质特征匹配自动跳过
      const el = target.src ? target.el : null;
      const options = await expandVideoOptions(resp.sources, pageUrl, el);
      if (!options.length) {
        showPanel("error", "候选链接均无法解析（可能被 CDN 阻断），请稍后重试。");
        return;
      }

      // 单链接直接确认面板；多链接展示 IDM 式列表
      if (options.length === 1) {
        renderConfirmPanel(options[0]);
      } else {
        renderLinksPanel(options);
      }
    } catch (e) {
      console.error("M3U8 Video Catcher analyze error:", e);
      showPanel("error", {
        message: String(e && e.message ? e.message : e),
        m3u8Url: "",
        pageUrl: location.href,
        title: document.title || "",
      });
    } finally {
      analyzing = false;
    }
  }

  // activeSegmentDirs 取最近 SEGMENT_WINDOW_MS 内仍在拉分片的流目录集合（去重，上限 20）。
  // 主播放器持续拉分片 → 其目录在集合内，作为"正在播放的流"的强信号。
  function activeSegmentDirs() {
    try {
      const now = performance.now();
      const entries = performance.getEntriesByType("resource");
      const isPageAsset = /\.(m3u8|css|js|mjs|png|gif|webp|svg|ico|woff2?|ttf|json|html?|txt|xml)(\?|#|$)/i;
      const dirs = new Set();
      for (const e of entries) {
        if (!/^https?:/i.test(e.name)) continue;
        if (isPageAsset.test(e.name)) continue;
        if (now - e.startTime > SEGMENT_WINDOW_MS) continue;
        const m = e.name.match(/^(https?:\/\/[^?#]*\/)/);
        if (m) dirs.add(m[1]);
      }
      return [...dirs].slice(0, 20);
    } catch {
      return [];
    }
  }

  // recentSegmentDir 最近一条分片请求的目录（悬停门控的单选优先信号）。
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

  // latestMediaDir 最近 SEGMENT_WINDOW_MS 内**最新一条**媒体请求的目录
  //（分片 .ts/.m4s/.flv 或 .m3u8 均算——B 站直播的变体 playlist 持续刷新且与
  // 分片同目录，刷新请求本身也是"当前流"的可靠信号）。
  // 与 activeSegmentDirs 的区别：旧流切走后的"余波"请求（B 站切直播间时旧流
  // 会再拉几秒）落在活跃集合里会让旧流误判为正在播放；最新一条一定是
  // 当前正在播放的流。
  function latestMediaDir() {
    try {
      const now = performance.now();
      const entries = performance.getEntriesByType("resource");
      const isPageAsset = /\.(css|js|mjs|png|gif|webp|svg|ico|woff2?|ttf|json|html?|txt|xml)(\?|#|$)/i;
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
  //
  // ⚠ 这里是全项目**唯一一份解析拷贝**：content script 是经典脚本（非 ES 模块），
  //   无法 import src/background/m3u8-parse.js，只能自留一份。其余两处（service
  //   worker 与 downloader 页面）共用 m3u8-parse.js。改动这里的语义时必须同步
  //   改 m3u8-parse.js，否则出现"同一份播放列表，两个地方算出不同的档位"。
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

  function qualityFromURL(url) {
    const m = String(url).match(/(2160p|1440p|1080p|720p|480p|360p|240p)/i);
    return m ? m[1].toUpperCase() : "";
  }

  function qualityRank(q) {
    return (
      {
        "4K": 8,
        "2K": 7,
        "1080P": 6,
        "720P": 5,
        "480P": 4,
        "360P": 3,
        "240P": 2,
      }[String(q).toUpperCase()] || 0
    );
  }

  // ============================================================
  // IDM 式候选展开：全部候选 → 每个可下载的画质/格式选项
  // master m3u8 展开为各变体；媒体 playlist 补时长/分片数；mp4 透传。
  // 列表限定"悬停的那个视频"：src 直链隔离 → 分辨率/时长特征匹配 →
  // 活跃分片目录过滤，逐级空回退（宁多勿漏）。
  // playlist 解析优先走本地 Go 服务 /probe（服务端带 Referer），
  // 服务未启动时回退页面主世界 fetch。
  // ============================================================
  async function expandVideoOptions(candidates, pageUrl, video) {
    const fallbackTitle = document.title || "";
    // 预检优先走本地 Go 服务 /probe：浏览器对无 CORS 头的 CDN 只能拿到 opaque
    // 空响应（"未预检"的根源），服务端带 Referer 直连（uTLS 指纹）能拿到。
    // 本地服务未启动时回退页面主世界 fetch。
    let serverBase = null;
    try {
      const cfg = await chrome.storage.local.get({ port: 7891 });
      serverBase = `http://127.0.0.1:${cfg.port || 7891}`;
    } catch {
      serverBase = null;
    }
    // 本地服务要求访问令牌（挡住任意网页驱动本机服务）；令牌经 background 内部通道取得，
    // 不写进页面世界。取不到就退回页面主世界 fetch（下面 probeFetch 失败时会自动回退）。
    let apiToken = "";
    if (serverBase) {
      try {
        const r = await chrome.runtime.sendMessage({ type: "getApiToken" });
        if (r && r.ok) apiToken = r.token || "";
      } catch {
        apiToken = "";
      }
    }
    const probeFetch = async (url) => {
      if (!serverBase) return null;
      try {
        const resp = await fetch(
          `${serverBase}/probe?url=${encodeURIComponent(url)}&referer=${encodeURIComponent(pageUrl)}`
            + (apiToken ? `&t=${encodeURIComponent(apiToken)}` : "")
        );
        if (!resp.ok) return null;
        return (await resp.text()) || null;
      } catch {
        return null;
      }
    };
    const textCache = new Map(); // url → playlist 文本（null = 拉取失败）
    const fetchPlaylist = async (url) => {
      if (!textCache.has(url)) {
        let text = await probeFetch(url);
        if (text == null) {
          try {
            text = await pageFetch(url, "text");
          } catch {
            text = null;
          }
        }
        textCache.set(url, text);
      }
      return textCache.get(url);
    };

    // url → ts 选项元数据；group 标识"同一视频"（master 的全部变体同组）。
    // 嗅探到的变体与 master 展开的变体在此去重合并
    const tsMeta = new Map();
    const mp4Options = [];
    for (const c of candidates) {
      if (c.type === "mp4") {
        if (!mp4Options.some((o) => o.url === c.url)) {
          mp4Options.push({
            type: "mp4",
            url: c.url,
            title: c.title || fallbackTitle,
            pageUrl: c.pageUrl || pageUrl,
            quality: qualityFromURL(c.url),
            size: c.size || 0,
            group: c.url,
            fromSrc: !!c.fromSrc,
          });
        }
        continue;
      }
      if (!tsMeta.has(c.url)) {
        tsMeta.set(c.url, {
          type: "ts",
          url: c.url,
          title: c.title || fallbackTitle,
          pageUrl: c.pageUrl || pageUrl,
          quality: qualityFromURL(c.url),
          resolution: "",
          bandwidth: 0,
          group: c.url,
        });
      }
    }

    // master playlist → 用变体条目替代 master 本身（master 不可直接下载）
    await Promise.all(
      [...tsMeta.entries()].map(async ([url, meta]) => {
        const text = await fetchPlaylist(url);
        if (!text || !text.includes("#EXT-X-STREAM-INF")) return;
        const variants = parseVariants(text, url);
        if (!variants.length) return;
        for (const v of variants) {
          const existing = tsMeta.get(v.url);
          if (existing) {
            existing.quality = v.quality;
            existing.resolution = v.resolution;
            existing.bandwidth = v.bandwidth;
            existing.group = url; // 独立嗅探的变体并入 master 的组
          } else {
            tsMeta.set(v.url, {
              type: "ts",
              url: v.url,
              title: meta.title,
              pageUrl: meta.pageUrl,
              quality: v.quality,
              resolution: v.resolution,
              bandwidth: v.bandwidth,
              group: url,
            });
          }
        }
        tsMeta.delete(url);
      })
    );

    // 悬停视频自带 http src 时，其余 mp4 是页面上其他视频（推荐流卡片）的
    // 嗅探条目——全部剔除，列表只保留选中视频（IDM 同行为）
    const videoSrc = video ? String(video.currentSrc || video.src || "") : "";
    if (/^https?:/i.test(videoSrc)) {
      for (let i = mp4Options.length - 1; i >= 0; i--) {
        if (!mp4Options[i].fromSrc) mp4Options.splice(i, 1);
      }
    }

    // 每个媒体 playlist 补时长与分片数（预检提前到过滤前，过滤要用时长信号）。
    // 预检失败（页面 fetch 被 CDN 防盗链拦截）不代表下载失败——本地 Go 服务
    // 下载时带 Referer 仍可能成功，所以条目保留，仅标记"未预检"。
    const tsOptions = await Promise.all(
      [...tsMeta.values()].map(async (m) => {
        const text = await fetchPlaylist(m.url);
        if (!text) return { ...m, duration: 0, segments: 0, live: false, unchecked: true };
        let duration = 0;
        let segments = 0;
        let live = false;
        if (!text.includes("#EXT-X-STREAM-INF")) {
          duration = parseDuration(text);
          segments = parseSegments(text, m.url).length;
          live = !text.includes("#EXT-X-ENDLIST");
        }
        return { ...m, duration, segments, live };
      })
    );

    // 悬停视频自身的特征匹配：解码分辨率对应变体 RESOLUTION、video.duration
    // 对应 playlist 总时长（直播 duration=Infinity 跳过）。命中组即"选中视频"，
    // 两个信号都有时取交集（交集空退并集），剔光回退——宁多勿漏。
    const wantW = video ? video.videoWidth || 0 : 0;
    const wantH = video ? video.videoHeight || 0 : 0;
    const wantDur = video && isFinite(video.duration) ? video.duration : 0;
    if (wantW || wantDur) {
      const resGroups = new Set();
      if (wantW && wantH) {
        for (const m of tsOptions) {
          const [w, h] = String(m.resolution || "").split("x").map(Number);
          if (w === wantW && h === wantH) resGroups.add(m.group);
        }
      }
      const durGroups = new Set();
      if (wantDur) {
        for (const m of tsOptions) {
          if (m.duration && Math.abs(m.duration - wantDur) <= 3) durGroups.add(m.group);
        }
      }
      let featureGroups = null;
      if (resGroups.size && durGroups.size) {
        featureGroups = new Set([...resGroups].filter((g) => durGroups.has(g)));
        if (!featureGroups.size) featureGroups = new Set([...resGroups, ...durGroups]);
      } else if (resGroups.size) featureGroups = resGroups;
      else if (durGroups.size) featureGroups = durGroups;
      if (featureGroups) {
        const backupTS = tsOptions.slice();
        const backupMP4 = mp4Options.slice();
        for (let i = tsOptions.length - 1; i >= 0; i--) {
          if (!featureGroups.has(tsOptions[i].group)) tsOptions.splice(i, 1);
        }
        for (let i = mp4Options.length - 1; i >= 0; i--) {
          if (!mp4Options[i].fromSrc && !featureGroups.has(mp4Options[i].group)) {
            mp4Options.splice(i, 1);
          }
        }
        if (!tsOptions.length && !mp4Options.length) {
          tsOptions.push(...backupTS);
          mp4Options.push(...backupMP4);
        }
      }
    }

    // 只保留"正在播放的流"，三级严格→宽松→全集（每级剔光即回退）：
    // 1. 最新媒体目录严格匹配：真正在播放的流必然刚拉过 .ts/.m3u8/.flv，
    //    旧流切走后的余波请求（B 站切直播间旧流再拉几秒）目录更旧，被挤掉。
    // 2. 全部活跃目录宽松匹配：最新目录与候选 URL 前缀不一致（CDN 目录分属
    //    不同路径层级）或暂停播放时兜底。
    // 3. 无任何活跃信号（暂停）回退全集，宁多勿漏。
    const applyKeep = (groups) => {
      const backupTS = tsOptions.slice();
      const backupMP4 = mp4Options.slice();
      for (let i = tsOptions.length - 1; i >= 0; i--) {
        if (!groups.has(tsOptions[i].group)) tsOptions.splice(i, 1);
      }
      for (let i = mp4Options.length - 1; i >= 0; i--) {
        // video 元素自身的 src 直链必属于选中视频，豁免过滤
        if (!mp4Options[i].fromSrc && !groups.has(mp4Options[i].group)) {
          mp4Options.splice(i, 1);
        }
      }
      if (!tsOptions.length && !mp4Options.length) {
        tsOptions.push(...backupTS);
        mp4Options.push(...backupMP4);
        return false;
      }
      return true;
    };

    const latestDir = latestMediaDir();
    if (latestDir) {
      const latestGroups = new Set();
      for (const m of tsOptions) {
        if (m.url.startsWith(latestDir)) latestGroups.add(m.group);
      }
      for (const o of mp4Options) {
        if (o.url.startsWith(latestDir)) latestGroups.add(o.group);
      }
      if (latestGroups.size) applyKeep(latestGroups);
    }

    const activeDirs = activeSegmentDirs();
    if (activeDirs.length) {
      const isActiveURL = (url) => activeDirs.some((d) => url.startsWith(d));
      const activeGroups = new Set();
      for (const m of tsOptions) {
        if (isActiveURL(m.url)) activeGroups.add(m.group);
      }
      for (const o of mp4Options) {
        if (isActiveURL(o.url)) activeGroups.add(o.group);
      }
      if (activeGroups.size) applyKeep(activeGroups);
    }

    const options = [...tsOptions, ...mp4Options];
    // 组聚拢：同一视频（组）的条目相邻展示；
    // 组内直播优先，TS 次之，画质高→低，未预检垫底
    const groupOrder = new Map();
    options.forEach((o) => {
      if (!groupOrder.has(o.group)) groupOrder.set(o.group, groupOrder.size);
    });
    options.sort((a, b) => {
      const gd = groupOrder.get(a.group) - groupOrder.get(b.group);
      if (gd) return gd;
      if (!!a.unchecked !== !!b.unchecked) return a.unchecked ? 1 : -1;
      if (!!a.live !== !!b.live) return a.live ? -1 : 1;
      if (a.type !== b.type) return a.type === "ts" ? -1 : 1;
      const rank = qualityRank(a.quality) - qualityRank(b.quality);
      if (rank) return -rank;
      return (b.bandwidth || 0) - (a.bandwidth || 0) || (b.duration || 0) - (a.duration || 0);
    });
    return options;
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

  async function showPanel(state, payload) {
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

      const goCmd = buildGoCommand(m3u8Url, pageUrl, title, await getExePath());
      const outName = buildOutputName(m3u8Url, title);

      // 服务未启动：单独给出明确指引
      const serverDownBanner = code === "SERVER_DOWN"
        ? `<div style="background:#fef3c7;border-left:3px solid #f59e0b;padding:10px 14px;border-radius:4px;margin-bottom:14px;font-size:13px;color:#78350f;line-height:1.5;">
             <div style="font-weight:600;margin-bottom:4px;">⚠ 本地下载服务未启动</div>
             解决办法：打开 <code style="background:#fff;padding:1px 5px;border-radius:2px;font-family:Menlo,Consolas,monospace;">go-catcher.exe</code>（GoCatcher 客户端，打开即自动启动下载服务），之后视频下载就都是一键完成。
           </div>`
        : "";

      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:24px 28px 0;">
          ${serverDownBanner}
          <div style="color:#d32f2f;margin-bottom:12px;font-size:14px;line-height:1.5;word-break:break-all;overflow-wrap:anywhere;">下载失败: ${escapeHtml(errMsg)}</div>
          <div style="color:#374151;font-size:13px;margin-bottom:8px;">也可以手动复制下面的命令到终端运行（不需要服务）：</div>
          ${outName ? `<div style="color:#374151;font-size:13px;margin-bottom:6px;">输出文件：<code style="background:#f3f4f6;padding:1px 5px;border-radius:3px;">${escapeHtml(outName)}</code></div>` : ""}
        </div>
         <div style="padding:0 28px 20px;">
           <div style="background:#1f2937;color:#e5e7eb;padding:12px;border-radius:6px;font-family:Menlo,Consolas,monospace;font-size:12px;line-height:1.6;word-break:break-all;white-space:pre-wrap;" id="go-cmd-text">${escapeHtml(goCmd)}</div>
           <div style="margin-top:6px;color:#9ca3af;font-size:11.5px;line-height:1.5;">exe 路径由本地服务运行时自动检测；显示为裸文件名时，先打开一次 GoCatcher 客户端即可，或在扩展设置里手动配置。</div>
           <div style="margin-top:10px;display:flex;gap:8px;align-items:center;">
             <button id="copy-go-cmd" style="padding:6px 14px;font-size:13px;border:none;background:#3b82f6;color:#fff;border-radius:4px;cursor:pointer;font-weight:600;">复制命令</button>
             <span id="copy-feedback" style="color:#22c55e;font-size:12px;display:none;">✓ 已复制到剪贴板</span>
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
    } else if (state === "initiating") {
      // 正在请求 Go：弹文件夹框 + 启动 disk 下载（都是后台快速请求，理应很快返回）
      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:36px 28px;text-align:center;">
          <div style="font-size:14px;color:#374151;margin-bottom:14px;">正在准备…</div>
          <div style="display:inline-block;width:14px;height:14px;border:2px solid #3b82f6;border-right-color:transparent;border-radius:50%;animation:m3u8spin 0.8s linear infinite;"></div>
          <div style="margin-top:16px;font-size:12px;color:#6b7280;">即将弹出文件夹选择框，请选择保存位置</div>
        </div>
        <style>@keyframes m3u8spin { to { transform: rotate(360deg); } }</style>`
      );
    } else if (state === "initiated") {
      // Go server 正在把视频直接落盘到所选目录
      const fn = (payload && payload.filename) || "video.ts";
      const dir = (payload && payload.dir) || "";
      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:22px 28px;">
          <div style="color:#22c55e;font-weight:600;font-size:14px;margin-bottom:10px;">✓ 已开始下载，由本地 Go 服务保存到所选文件夹</div>
          ${dir ? `<div style="font-size:12px;color:#6b7280;margin-bottom:12px;word-break:break-all;">保存位置：${escapeHtml(dir)}</div>` : ""}
          <div style="background:#f3f4f6;padding:8px 10px;border-radius:4px;font-size:12px;color:#374151;margin-bottom:14px;">
            文件：<code style="font-family:Menlo,Consolas,monospace;">${escapeHtml(fn)}</code>
          </div>
          <div id="initiated-progress-text" style="margin-bottom:10px;color:#374151;font-size:13px;">正在连接本地 Go 服务…</div>
          <div style="width:100%;height:8px;background:#e5e7eb;border-radius:4px;overflow:hidden;">
            <div id="initiated-progress-fill" style="width:0%;height:100%;background:#3b82f6;transition:width .2s;"></div>
          </div>
          <div id="initiated-controls" style="margin-top:14px;display:flex;gap:8px;align-items:center;justify-content:flex-end;"></div>
          <div style="margin-top:8px;font-size:11px;color:#9ca3af;">下载过程不经过浏览器下载栏，可在此暂停/继续或取消。</div>
        </div>`
      );
    } else if (state === "completed") {
      const fn = (payload && payload.filename) || "video.ts";
      panel.innerHTML = panelShell(
        "下载该视频",
        `<div style="padding:32px 28px;text-align:center;">
          <div style="font-size:32px;margin-bottom:10px;">🎉</div>
          <div style="color:#22c55e;font-weight:600;font-size:16px;margin-bottom:8px;">下载完成</div>
          <div style="font-size:13px;color:#374151;margin-bottom:6px;">文件名：<code style="background:#f3f4f6;padding:2px 6px;border-radius:3px;font-family:Menlo,Consolas,monospace;">${escapeHtml(fn)}</code></div>
          ${payload && payload.finalPath ? `<div style="font-size:12px;color:#6b7280;margin-bottom:14px;word-break:break-all;">位置：${escapeHtml(payload.finalPath)}</div>` : ""}
          <button id="completed-close" style="margin-top:10px;padding:8px 20px;font-size:13px;border:none;background:#3b82f6;color:#fff;border-radius:5px;cursor:pointer;">完成</button>
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

  // ============================================================
  // IDM 式链接列表面板：多候选时列出全部可选链接（格式/画质/码率/时长）
  // ============================================================
  function optionMeta(o) {
    const parts = [];
    if (o.unchecked) parts.push("未预检");
    if (o.live) parts.push("直播");
    if (o.quality) parts.push(o.quality);
    if (o.resolution) parts.push(o.resolution);
    if (o.bandwidth) parts.push(`${(o.bandwidth / 1000).toFixed(0)} kbps`);
    if (o.duration) parts.push(`时长 ${fmtDur(o.duration)}`);
    else if (o.type === "ts" && o.segments) parts.push(`${o.segments} 分片`);
    if (o.size) {
      parts.push(`${(o.size / 1024 / 1024).toFixed(1)} MB`);
    } else if (o.bandwidth && o.duration) {
      // 变体码率 × 时长估算流量（bps × s / 8 = 字节）
      parts.push(`≈${((o.bandwidth * o.duration) / 8 / 1024 / 1024).toFixed(1)} MB`);
    }
    return parts.join(" · ") || "无附加信息";
  }

  // optionURLLabel：host + 路径（长路径中间省略），帮助用户区分不同 CDN/线路。
  function optionURLLabel(url) {
    try {
      const u = new URL(url);
      let path = u.pathname;
      try {
        path = decodeURIComponent(path);
      } catch {
        /* 保留原样 */
      }
      if (path.length > 40) path = path.slice(0, 19) + "…" + path.slice(-19);
      return u.hostname + path;
    } catch {
      return url.length > 72 ? url.slice(0, 34) + "…" + url.slice(-34) : url;
    }
  }

  function renderLinksPanel(options) {
    if (!panel) createPanel();

    const rows = options
      .map(
        (o, idx) => `
        <div class="m3u8-catcher-row" data-idx="${idx}" style="display:flex;align-items:center;padding:10px 14px;border-bottom:1px solid #f3f4f6;cursor:pointer;${o.unchecked ? "opacity:0.55;" : ""}">
          <div style="flex:none;width:44px;margin-right:12px;padding:3px 0;text-align:center;border-radius:4px;font-size:11px;font-weight:700;color:#fff;background:${o.type === "ts" ? "#3b82f6" : "#64748b"};">${o.type === "ts" ? "TS" : "MP4"}</div>
          <div style="flex:1;min-width:0;">
            <div style="font-size:13.5px;color:#111;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">${escapeHtml(o.title || "(无标题)")}</div>
            <div style="font-size:11px;color:#9ca3af;margin-top:2px;">${escapeHtml(optionMeta(o))}</div>
            <div style="font-size:10.5px;color:#c3cad4;margin-top:1px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">${escapeHtml(optionURLLabel(o.url))}</div>
          </div>
          <div style="margin-left:10px;padding:4px 10px;background:#3b82f6;color:#fff;font-size:12px;border-radius:4px;font-weight:600;">下载</div>
        </div>`
      )
      .join("");

    panel.innerHTML = panelShell(
      `下载该视频 - ${escapeHtml(options[0].title || document.title || "")}`,
      `<div style="overflow-y:auto;max-height:60vh;">${rows}</div>
       <div style="padding:12px 18px;border-top:1px solid #e5e7eb;background:#f8f9fb;color:#6b7280;font-size:12px;">共 ${options.length} 个链接，点击任一行选择该画质下载。</div>`
    );

    bindClose();
    panel.querySelectorAll(".m3u8-catcher-row").forEach((row) => {
      row.addEventListener("mouseenter", () => (row.style.background = "#f0f7ff"));
      row.addEventListener("mouseleave", () => (row.style.background = "transparent"));
      row.addEventListener("click", () => {
        const o = options[parseInt(row.dataset.idx, 10)];
        if (o) {
          // 选中链接 → 进入确认面板核验 URL，再触发下载
          renderConfirmPanel(o);
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
    const filename = buildOutputName(url, title) || "video.ts";

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
          <span id="confirm-copy-feedback" style="margin-left:8px;color:#22c55e;font-size:12px;display:none;">✓ 已复制</span>
        </div>

        <div style="margin-bottom:18px;">
          <div style="font-size:12px;color:#6b7280;margin-bottom:3px;">保存文件名</div>
          <div style="background:#fef3c7;padding:8px 10px;border-radius:4px;font-family:Menlo,Consolas,monospace;font-size:12px;color:#78350f;">${escapeHtml(filename)}</div>
          <div style="margin-top:4px;font-size:11px;color:#9ca3af;">下载时可改名字 / 改位置</div>
        </div>

        <div style="display:flex;gap:10px;">
          <button id="confirm-cancel" style="flex:1;padding:11px;border:1px solid #d1d5db;background:#fff;border-radius:5px;cursor:pointer;font-size:14px;color:#374151;">取消</button>
          <button id="confirm-go" style="flex:2;padding:11px;border:none;background:#3b82f6;color:#fff;border-radius:5px;cursor:pointer;font-size:14px;font-weight:600;">⬇ 确认下载</button>
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
        '<button data-ctl="resume" style="padding:6px 16px;font-size:13px;border:none;background:#3b82f6;color:#fff;border-radius:5px;cursor:pointer;font-weight:600;">▶ 继续下载</button>' +
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

  // 兜底命令的 exe 路径解析（优先级从高到低）：
  //   1. 设置里显式配置的路径（≠ 默认值才算"显式"）
  //   2. 服务自报的路径（/svc/info 自动探测缓存；没有缓存时让 background 实时问一次）
  //   3. 裸文件名 go-catcher.exe（依赖 PATH；服务从未运行过时才会走到这）
  async function getExePath() {
    let detected = "";
    try {
      const s = await chrome.storage.local.get({ exePath: "", detectedExePath: "" });
      const custom = ((s && s.exePath) || "").trim();
      if (custom && custom !== "go-catcher.exe") return custom;
      detected = (s && s.detectedExePath) || "";
    } catch {}
    if (detected) return detected;
    // 缓存为空（刚装扩展/刚换机器/服务一直没跑过）：问 background 要——
    // 它有 host 权限，能绕过页面 CORS 直接 fetch /svc/info
    try {
      const resp = await chrome.runtime.sendMessage({ type: "getExePath" });
      if (resp && resp.exePath) return resp.exePath;
    } catch {}
    return "go-catcher.exe";
  }

  // 生成跨 PowerShell / Git Bash / CMD 可直接粘贴运行的下载命令
  // 用 ; 而非 && 分隔：Windows PowerShell 5.1 不支持 && 作语句分隔符
  // 路径加双引号：bash 下不加引号会把位置参数按空白切片
  function buildGoCommand(m3u8Url, pageUrl, title, exePath) {
    // 直接调用单文件 exe（三种模式之一：--url= 直下，无需先起服务）
    const goArgs = [`"${exePath || "go-catcher.exe"}"`];
    if (m3u8Url) goArgs.push(`--url="${m3u8Url}"`);
    if (pageUrl) goArgs.push(`--referer="${pageUrl}"`);

    // -o 输出文件名：视频标题 + 画质（由 buildOutputName 统一构造）
    const outName = buildOutputName(m3u8Url, title);
    if (outName) goArgs.push(`-o "${outName}"`);

    return [
      "chcp 65001",
      goArgs.join(" "),
    ].join(" ; ");
  }

  // 拼输出文件名：<标题>_<画质>.ts
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

    // 画质后缀（输出 .ts：HLS 原始流直接落盘，Go 侧不做封装，PotPlayer/VLC 可播）
    const q = qualityFromUrl(m3u8Url);
    return name + (q ? `_${q}` : "") + ".ts";
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
