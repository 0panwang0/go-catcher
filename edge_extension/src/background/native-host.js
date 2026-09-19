// 原生消息宿主客户端：让扩展在本地服务没跑时把客户端唤起来。
//
// 为什么需要这条通道：扩展与客户端之间走本地 HTTP 服务，而 HTTP 是请求-响应协议，
// **扩展没有"启动服务端"的能力**——服务不在就只能报错。原生消息通道把"启动进程"
// 这件事交给浏览器（它按注册表清单启动宿主程序），于是扩展可以主动唤起客户端。
//
// 宿主进程只负责"确保本地服务在跑"，不下载任何东西；它把服务拉起来后就退场，
// 后续下载仍走原有的 HTTP 通道。
//
// 本模块刻意不 import server-api.js：那边要 import 这里来组合出"先探活、再唤起"，
// 反向依赖会形成循环。这里只做纯粹的原生消息收发。

// NATIVE_HOST_NAME 必须与宿主清单里的 name、注册表子项名逐字一致，
// 也要与 Go 侧 platform.NativeHostName 一致。
export const NATIVE_HOST_NAME = "com.gocatcher.browser_host";

// 唤起后等待宿主回话的上限。宿主内部自己会等本地服务就绪（上限 20 秒），
// 这里留出余量，别在宿主还没回话时就断开。
export const WAKE_TIMEOUT_MS = 25000;

// 并发去重：用户连点几下时不要让多条连接同时去拉进程。
let waking = null;

// requestWake 请宿主确保服务在跑，返回宿主响应对象；宿主不可用或超时返回 null。
//
// 宿主不可用的常见原因：还没运行过客户端（注册表项缺失）、或浏览器策略拦截了
// 原生消息宿主。这两种情况都只能回退到"提示用户手动打开"的既有路径。
export function requestWake(timeoutMs = WAKE_TIMEOUT_MS) {
  if (waking) return waking;
  waking = doRequestWake(timeoutMs).finally(() => {
    waking = null;
  });
  return waking;
}

// reasonOf 从几种"错误形状"里抽出一句可读文本（Error / {message} / 字符串）。
function reasonOf(e) {
  if (!e) return "";
  if (typeof e === "string") return e;
  return e.message || String(e);
}

function doRequestWake(timeoutMs) {
  return new Promise((resolve) => {
    let port = null;
    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      try {
        if (port) port.disconnect();
      } catch {
        /* 已经断开，忽略 */
      }
      resolve(value);
    };
    const timer = setTimeout(() => finish(null), timeoutMs);

    try {
      port = chrome.runtime.connectNative(NATIVE_HOST_NAME);
    } catch (e) {
      // connectNative 本身抛错（宿主名不合法、扩展缺 nativeMessaging 权限等）
      clearTimeout(timer);
      console.warn("[go-catcher] connectNative 失败:", reasonOf(e));
      resolve(null);
      return;
    }

    port.onMessage.addListener((msg) => {
      clearTimeout(timer);
      finish(msg && typeof msg === "object" ? msg : null);
    });

    port.onDisconnect.addListener(() => {
      clearTimeout(timer);
      // 必须读一下 lastError，否则浏览器会在控制台打一条未处理错误；
      // 它也正是"宿主没登记 / 被策略拦下 / 宿主进程一起步就退出"的唯一提示来源，
      // 所以记下来而不是丢掉——排查"点了没反应"时这条就是全部线索。
      const why = reasonOf(chrome.runtime.lastError);
      if (why) console.warn("[go-catcher] 原生消息宿主不可用:", why);
      finish(null);
    });

    try {
      port.postMessage({ type: "ensure-server" });
    } catch {
      clearTimeout(timer);
      finish(null);
    }
  });
}
