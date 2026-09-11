// 本地 Go HTTP 服务的访问层：
//   ping /health → /pickdir 弹原生文件夹框 → /download?mode=disk 落盘 → 轮询 /status。
// 服务对除 /health、/svc/info 之外的端点都要求访问令牌（挡住"任意网页驱动本机服务
// 写文件/执行程序"）。令牌由 /svc/info 握手取得后缓存在 storage.local；服务侧换机
// 或重装会变，所以 apiFetch 收到 401 时重新握手并重试一次，不让用户看到莫名其妙的失败。

// 扩展设置（downloader.html 设置卡片里可改，存 chrome.storage.local）：
//   exePath —— go-catcher.exe 本机路径，兜底"复制命令到终端"用；默认裸文件名（要求在 PATH）
//   port    —— 本地服务端口，与 GoCatcher 客户端「设置」里的端口保持一致
// 缓存 + onChanged 失效：service worker 随时可能被回收，重启后首次读取重建缓存
let settingsCache = null;

export async function getSettings() {
  if (!settingsCache) {
    settingsCache = await chrome.storage.local.get({ exePath: "go-catcher.exe", port: 7891, detectedExePath: "" });
  }
  return settingsCache;
}

chrome.storage.onChanged.addListener((changes, area) => {
  if (area === "local") settingsCache = null;
});

export async function serverBase() {
  const s = await getSettings();
  return `http://127.0.0.1:${s.port}`;
}

let apiTokenCache = null;
export async function getApiToken(force = false) {
  if (!force) {
    if (apiTokenCache) return apiTokenCache;
    const s = await chrome.storage.local.get({ apiToken: "" });
    if (s.apiToken) { apiTokenCache = s.apiToken; return apiTokenCache; }
  }
  try {
    const resp = await fetch(`${await serverBase()}/svc/info`, { cache: "no-store" });
    if (resp.ok) {
      const info = await resp.json().catch(() => null);
      if (info && info.token) {
        apiTokenCache = info.token;
        await chrome.storage.local.set({ apiToken: info.token });
        if (info.exe) await chrome.storage.local.set({ detectedExePath: info.exe });
        return apiTokenCache;
      }
    }
  } catch {}
  return apiTokenCache || "";
}

// apiFetch 自动补 ?t=<token>；401 时重新握手重试一次。
export async function apiFetch(pathname, opts) {
  const call = async (tok) => {
    const sep = pathname.includes("?") ? "&" : "?";
    const url = tok ? `${await serverBase()}${pathname}${sep}t=${encodeURIComponent(tok)}` : `${await serverBase()}${pathname}`;
    return fetch(url, opts);
  };
  const tok = await getApiToken();
  let resp = await call(tok);
  if (resp.status === 401) {
    const fresh = await getApiToken(true);
    if (fresh && fresh !== tok) resp = await call(fresh);
  }
  return resp;
}

export async function pingServer(timeoutMs = 2000) {
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeoutMs);
    const resp = await fetch(`${await serverBase()}/health`, { signal: ctrl.signal });
    clearTimeout(timer);
    if (!resp.ok) return false;
    const text = (await resp.text()).trim();
    if (text === "ok") {
      detectExePath(); // fire-and-forget：服务活着就顺手记下本机 exe 路径
      return true;
    }
    return false;
  } catch {
    return false;
  }
}

// 从 /svc/info 记录服务自报的 exe 绝对路径与访问令牌（同一个握手响应）。
// 兜底"复制命令到终端"需要完整路径，但每台机器路径不同不能写死；
// 服务跑过一次这里就缓存下来，content.js 生成兜底命令时直接用（用户零配置）。
export async function detectExePath() {
  await getApiToken(true);
}

export async function downloadViaServer({ m3u8Url, referer, title, filename } = {}) {
  if (!m3u8Url) return { ok: false, error: "缺少 m3u8Url" };

  const healthy = await pingServer();
  if (!healthy) {
    return {
      ok: false,
      error: "本地下载服务未启动。请打开 go-catcher.exe（GoCatcher 客户端，打开即自动启动下载服务），然后重试。",
      code: "SERVER_DOWN",
    };
  }

  try {
    // 1) 弹 Windows 原生"选择文件夹"框（由本地 Go 服务弹出，返回绝对路径）
    const pkRes = await apiFetch("/pickdir", {
      method: "GET",
      cache: "no-store",
    });
    if (!pkRes.ok) {
      return { ok: false, error: `目录选择失败 (HTTP ${pkRes.status})` };
    }
    const pk = await pkRes.json().catch(() => null);
    if (!pk || pk.cancelled) {
      return { ok: false, error: "用户取消了文件夹选择", code: "USER_CANCEL" };
    }
    const dir = pk.dir;
    if (!dir) {
      return { ok: false, error: "未获取到有效目录路径" };
    }

    // 2) 请求 Go 直接落盘到所选目录（异步：立即返回，扩展轮询 /status）
    const params = new URLSearchParams({ m3u8: m3u8Url, mode: "disk", dir });
    if (referer) params.set("referer", referer);
    const safeFn = filename || `${title || "video"}.ts`;
    params.set("filename", safeFn);

    const dlRes = await apiFetch(`/download?${params.toString()}`, {
      method: "GET",
      cache: "no-store",
    });
    if (!dlRes.ok) {
      const txt = await dlRes.text().catch(() => "");
      return { ok: false, error: `下载启动失败 (HTTP ${dlRes.status})：${txt}` };
    }
    const dl = await dlRes.json().catch(() => ({}));

    // 多任务并发：server 返回本次任务的 id，扩展用它轮询该任务进度
    return { ok: true, dir, filename: safeFn, started: !!(dl && dl.started), taskId: dl && dl.id };
  } catch (e) {
    if (/canceled|cancelled|user.*cancel/i.test(String(e && e.message))) {
      return { ok: false, error: "用户取消了文件夹选择", code: "USER_CANCEL" };
    }
    return { ok: false, error: String(e && e.message ? e.message : e) };
  }
}

export async function queryDownload(msg) {
  // disk 模式：查 Go server 的任务状态。带 taskId 查指定任务；无 taskId 则取最新一个。
  try {
    let t = null;
    const taskId = msg && msg.taskId;
    if (taskId) {
      // 多任务：按任务 id 精确查
      const resp = await apiFetch(`/status?id=${encodeURIComponent(taskId)}`, { cache: "no-store" });
      if (!resp.ok) return { exists: false, notFound: resp.status === 404, error: `status HTTP ${resp.status}` };
      t = await resp.json().catch(() => null);
    } else {
      // 兜底：无 id（旧调用方）→ 取列表里最新一个（优先运行中/排队，否则最近完成）
      const resp = await apiFetch("/status", { cache: "no-store" });
      if (!resp.ok) return { exists: false, error: `status HTTP ${resp.status}` };
      const d = await resp.json().catch(() => null);
      if (d && Array.isArray(d.tasks) && d.tasks.length > 0) {
        const active = d.tasks.find((x) => x.queued || x.running) || d.tasks[d.tasks.length - 1];
        t = active;
      }
    }
    if (!t) return { exists: false };
    // 归一化字段，供 content.js 统一处理
    if (t.done) {
      if (t.finalPath) {
        return {
          exists: true,
          id: t.id,
          state: "complete",
          finalPath: t.finalPath,
          filename: t.finalPath ? t.finalPath.split(/[\\/]/).pop() : "",
          pct: 100,
          stage: t.stage || "已保存",
        };
      }
      if (t.canceled) {
        return {
          exists: true,
          id: t.id,
          state: "canceled",
          stage: t.stage || "已取消",
        };
      }
      return {
        exists: true,
        id: t.id,
        state: "error",
        error: t.error || "下载失败",
        stage: t.stage || "失败",
      };
    }
    return {
      exists: true,
      id: t.id,
      state: t.paused ? "paused" : "inProgress",
      stage: t.queued ? "排队中" : (t.paused ? "已暂停" : (t.stage || "下载中")),
      pct: t.pct || 0,
      segDone: t.segDone || 0,
      segTot: t.segTot || 0,
      queued: !!t.queued,
      running: !!t.running,
      paused: !!t.paused,
      canceled: !!t.canceled,
    };
  } catch (e) {
    return { exists: false, error: String(e) };
  }
}

// 对 Go server 上某任务执行控制动作：pause / resume / cancel
// action 直接作为 URL 路径段，id 作为查询参数（与 server 路由一致）
export async function controlTask({ taskId, action } = {}) {
  if (!taskId) return { ok: false, error: "缺少 taskId" };
  const actionMap = { pause: "pause", resume: "resume", cancel: "cancel" };
  const path = actionMap[action];
  if (!path) return { ok: false, error: `未知动作: ${action}` };

  const healthy = await pingServer();
  if (!healthy) {
    return { ok: false, error: "本地 Go 下载服务未启动", code: "SERVER_DOWN" };
  }
  try {
    const resp = await apiFetch(
      `/${path}?id=${encodeURIComponent(taskId)}`,
      { cache: "no-store" }
    );
    const body = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      return { ok: false, error: body && body.error ? body.error : `HTTP ${resp.status}`, notFound: resp.status === 404 };
    }
    // 成功：返回 server 回传的状态字段（含新 stage/paused/canceled/done）
    return { ok: true, id: taskId, action, ...(body || {}) };
  } catch (e) {
    return { ok: false, error: String(e) };
  }
}
