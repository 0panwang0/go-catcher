// M3U8 Video Catcher - MAIN world fetch proxy（S3 收敛后的窄桥）
// 在页面主世界执行，请求头（Origin/Referer/Cookie）与页面本身完全一致。
// CDN 通常不发 Access-Control-Allow-Origin，所以 cors 模式 + arrayBuffer 必失败。
// 解决：先试 cors（拿到 status），失败则退 no-cors + ReadableStream 拼接 chunks
// —— opaque response 没有 status/headers，但 body stream 仍可读（HLS 播放器就是这样播的）。
//
// ============================================================
// S3：为什么这个桥必须收窄
// ------------------------------------------------------------
// 这个脚本跑在页面主世界，而 CustomEvent 是 document 级广播——页面自己的
// 脚本同样能派发 __m3u8_catcher_fetch_req__、监听 __m3u8_catcher_fetch_res__，
// 等于页面可以借用这里的 fetch 能力。原本的代理接受任意 URL + 任意
// responseType，其中 no-cors + body-stream 读取是页面自己做不到的能力
// （opaque response 的 body 正常情况下页面读不到）——这就是评审 S3 指出的
// 「页面可读跨源响应」放大面。
//
// nonce 方案被否：声明式 world:"MAIN" 注入下，注入脚本与 ISOLATED content
// script 之间没有对页面保密的共享密钥通道（页面能看到 document 上的所有
// 事件 detail，也能 hook dispatchEvent），口令藏不住。
//
// 因此改为白名单收敛，让「页面借用代理」与「页面自己动手」能力等价：
//   1. responseType 只认 "text"（原 blob/arraybuffer 路径删除——本桥唯一
//      调用方 content.js 只拉 playlist 文本）；
//   2. 自定义 headers 支持（detail.headers）——原本就无人使用，移除；
//   3. URL 必须是 http(s)，且满足其一：
//        a. 与当前文档同源（页面自己本来就能读，借用无增益）；
//        b. 路径以 .m3u8 结尾（跨源 playlist 是公开 CDN 资产，泄露面可忽略）；
//   4. 响应体上限 MAX_BRIDGE_BYTES，超限中断并报错——堵住"借道拉大文件"。
// 残余风险（已知、可接受）：恶意页面可借道读跨源 .m3u8 文本（≤4MB）。
// playlist 不含用户凭据，与"能读 gmail 首页"不是一个量级。
// ============================================================

(function () {
  if (window.__m3u8_catcher_main_injected__) return;
  window.__m3u8_catcher_main_injected__ = true;

  const utf8 = new TextDecoder("utf-8");
  const MAX_BRIDGE_BYTES = 4 * 1024 * 1024; // playlist 文本远小于此；堵"借道拉大文件"

  document.addEventListener("__m3u8_catcher_fetch_req__", async (e) => {
    const detail = e.detail || {};
    const { id, url, responseType } = detail;
    if (!id || !url) return;
    // S3 参数面校验：不满足直接回错误，不做任何网络请求
    if (responseType !== "text" || !isAllowedURL(url)) {
      dispatch(id, { ok: false, error: "bridge: 请求被拒绝（仅支持同源或跨源 .m3u8 的文本拉取）" });
      return;
    }

    try {
      const data = await fetchAnyMode(url);
      dispatch(id, { ok: true, data });
    } catch (err) {
      dispatch(id, {
        ok: false,
        error: err && err.message ? err.message : String(err),
      });
    }
  });

  function dispatch(id, payload) {
    document.dispatchEvent(
      new CustomEvent("__m3u8_catcher_fetch_res__", {
        detail: { id, ...payload },
      })
    );
  }

  // isAllowedURL 白名单：同源，或跨源但路径以 .m3u8 结尾。
  function isAllowedURL(url) {
    let u;
    try {
      u = new URL(url, location.href);
    } catch {
      return false;
    }
    if (u.protocol !== "http:" && u.protocol !== "https:") return false;
    if (u.origin === location.origin) return true;
    return /\.m3u8$/i.test(u.pathname);
  }

  async function fetchAnyMode(url) {
    // 第一轮：标准 cors，可读 status
    try {
      const resp = await fetch(url, {
        credentials: "include",
        cache: "no-store",
        redirect: "follow",
      });
      if (resp.ok) return await streamTo(resp);
      // 401/403/404 等不重试，直接报错让上层知道
      throw new Error(`HTTP ${resp.status}`);
    } catch (e) {
      // 第二轮：no-cors，去掉 CORS 硬约束。opaque response body 仍可读
      let resp;
      try {
        resp = await fetch(url, {
          mode: "no-cors",
          credentials: "include",
          cache: "no-store",
          redirect: "follow",
        });
      } catch (e2) {
        // 真正的网络错误（代理未连、DNS、屏蔽）
        throw new Error(
          "fetch failed: " + (e2 && e2.message ? e2.message : String(e2))
        );
      }
      if (!resp.body) {
        throw new Error("fetch failed: response has no body (CORS + opaque blocked)");
      }
      return await streamTo(resp);
    }
  }

  // 不论透明 / opaque，统一走流式读 → 避免 .text()/.arrayBuffer() 对 opaque 抛错。
  // 超过 MAX_BRIDGE_BYTES 立即中断（S3 体积上限）。
  async function streamTo(resp) {
    const reader = resp.body.getReader();
    const chunks = [];
    let total = 0;
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (value) {
        total += value.byteLength;
        if (total > MAX_BRIDGE_BYTES) {
          try { await reader.cancel(); } catch { /* 已中断，忽略 */ }
          throw new Error("bridge: 响应超过体积上限（仅代理 playlist 文本）");
        }
        chunks.push(value);
      }
    }
    const buf = new Uint8Array(total);
    let off = 0;
    for (const c of chunks) {
      buf.set(c, off);
      off += c.byteLength;
    }
    return utf8.decode(buf);
  }
})();
