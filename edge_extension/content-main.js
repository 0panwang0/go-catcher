// M3U8 Video Catcher - MAIN world fetch proxy
// 在页面主世界执行，请求头（Origin/Referer/Cookie）与页面本身完全一致。
// CDN 通常不发 Access-Control-Allow-Origin，所以 cors 模式 + arrayBuffer 必失败。
// 解决：先试 cors（拿到 status），失败则退 no-cors + ReadableStream 拼接 chunks
// —— opaque response 没有 status/headers，但 body stream 仍可读（HLS 播放器就是这样播的）。

(function () {
  if (window.__m3u8_catcher_main_injected__) return;
  window.__m3u8_catcher_main_injected__ = true;

  const utf8 = new TextDecoder("utf-8");

  document.addEventListener("__m3u8_catcher_fetch_req__", async (e) => {
    const detail = e.detail || {};
    const { id, url, responseType, headers } = detail;
    if (!id || !url) return;

    try {
      const data = await fetchAnyMode(url, responseType || "text", headers);
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

  async function fetchAnyMode(url, responseType, customHeaders) {
    // 第一轮：标准 cors，可读 status
    try {
      const resp = await fetch(url, {
        credentials: "include",
        cache: "no-store",
        redirect: "follow",
        headers: customHeaders || undefined,
      });
      if (resp.ok) return await streamTo(resp, responseType);
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
      return await streamTo(resp, responseType);
    }
  }

  // 不论透明 / opaque，统一走流式读 → 避免 .text()/.arrayBuffer() 对 opaque 抛错
  async function streamTo(resp, responseType) {
    const reader = resp.body.getReader();
    const chunks = [];
    let total = 0;
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (value) {
        chunks.push(value);
        total += value.byteLength;
      }
    }
    const buf = new Uint8Array(total);
    let off = 0;
    for (const c of chunks) {
      buf.set(c, off);
      off += c.byteLength;
    }
    if (responseType === "text") return utf8.decode(buf);
    if (responseType === "blob") return new Blob([buf]);
    return buf.buffer;
  }
})();
