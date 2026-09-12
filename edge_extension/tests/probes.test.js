// 回归测试：嗅探探测器的 webRequest 注册参数 + 无扩展名通道的判据纯函数。
// 用法（在 edge_extension 目录下）：
//   node tests/probes.test.js
//
// 背景（2026-09-11 线上真实故障）：MV3 里 chrome.webRequest.onCompleted 的
// extraInfoSpec 只接受 "extraHeaders" / "responseHeaders" 两个值，写成
// ["responseHeaders", "requestHeaders"]（"requestHeaders" 是 onBeforeSendHeaders
// 才认的值）时，浏览器不会在 addListener 处抛错到调用点，而是让**整个探测器
// 静默安装失败**——扩展面板里只有一条错误、功能悄无声息地少一条通道。
// 本测试用桩捕获注册参数，按浏览器自己的校验规则逐个断言，把这类错误拦在提交前。
const fs = require("fs");
const path = require("path");

// ============================================================
// chrome 桩：记录 webRequest 注册参数
// ============================================================
const registrations = [];
const noop = { addListener() {} };

const chrome = {
  action: { onClicked: noop, setBadgeText() {}, setBadgeBackgroundColor() {} },
  webRequest: {
    onCompleted: {
      addListener(fn, filter, extraInfoSpec) {
        registrations.push({ fn, filter, extraInfoSpec: extraInfoSpec || [] });
      },
    },
  },
  runtime: { onMessage: noop, getURL: (p) => p, lastError: undefined },
  tabs: { get() {}, create() {}, onRemoved: noop, query: async () => [] },
  declarativeNetRequest: {
    getDynamicRules: async () => [],
    updateDynamicRules: async () => {},
  },
  storage: {
    onChanged: noop,
    local: {
      get: async () => ({}),
      set: async () => {},
    },
  },
};

const src = fs.readFileSync(path.join(__dirname, "..", "background.js"), "utf8");
// 加载 bundle 会立刻跑 installSniffProbes()，注册参数即被上面的桩捕获。
const api = new Function("chrome", src + "\nreturn __m3u8catcher.__test__;")(chrome);

let pass = 0;
let fail = 0;
function check(name, cond, extra) {
  if (cond) {
    pass++;
    console.log("  ok  -", name);
  } else {
    fail++;
    console.log("  FAIL-", name, extra === undefined ? "" : JSON.stringify(extra));
  }
}

// 浏览器对 webRequest.onCompleted 的 extraInfoSpec 白名单（错误信息原文：
// "Value must be one of extraHeaders, responseHeaders"）。
const ON_COMPLETED_ALLOWED = new Set(["extraHeaders", "responseHeaders"]);

// res() 直接返回 responseHeaders 数组（[["Name","value"]] → [{name,value}]）
const res = (pairs) => pairs.map(([name, value]) => ({ name, value }));
const det = (opts = {}) => ({
  url: "https://cdn.example.com/videoplayback?id=abc",
  statusCode: 200,
  responseHeaders: [],
  type: "xmlhttprequest",
  ...opts,
});

// ============================================================

console.log(`探测器注册（共 ${registrations.length} 个）：`);
check("三个通道都已注册（m3u8 / mp4 / contenttype）", registrations.length === 3, registrations.length);

for (const [i, reg] of registrations.entries()) {
  const label = `#${i + 1} ${JSON.stringify(reg.filter.urls)}`;
  const illegal = reg.extraInfoSpec.filter((v) => !ON_COMPLETED_ALLOWED.has(v));
  check(`${label} extraInfoSpec 在白名单内`, illegal.length === 0, { illegal, got: reg.extraInfoSpec });
  check(`${label} filter.urls 是非空字符串数组`, Array.isArray(reg.filter.urls) && reg.filter.urls.length > 0 && reg.filter.urls.every((u) => typeof u === "string" && u));
}

check(
  "没有任何探测器在 onCompleted 上要 requestHeaders（会静默装不上）",
  registrations.every((r) => !r.extraInfoSpec.includes("requestHeaders")),
  registrations.map((r) => r.extraInfoSpec)
);
check(
  "恰好一个探测器覆盖 *://*/*（无扩展名兜底通道）",
  registrations.filter((r) => r.filter.urls.length === 1 && r.filter.urls[0] === "*://*/*").length === 1
);
check(
  "兜底通道必须拿得到响应头（否则 Content-Type 判据失效）",
  registrations
    .filter((r) => r.filter.urls.length === 1 && r.filter.urls[0] === "*://*/*")
    .every((r) => r.extraInfoSpec.includes("responseHeaders"))
);

console.log("\ncontentTypeMediaType（Content-Type → 媒体类型）：");
check("application/vnd.apple.mpegurl → m3u8", api.contentTypeMediaType(det({ responseHeaders: res([["Content-Type", "application/vnd.apple.mpegurl"]]) })) === "m3u8");
check("application/x-mpegURL; charset=utf-8 → m3u8（带参数 + 大小写）", api.contentTypeMediaType(det({ responseHeaders: res([["content-type", "application/x-mpegURL; charset=utf-8"]]) })) === "m3u8");
check("audio/mpegurl → m3u8", api.contentTypeMediaType(det({ responseHeaders: res([["Content-Type", "audio/mpegurl"]]) })) === "m3u8");
check("video/mp4 → mp4", api.contentTypeMediaType(det({ responseHeaders: res([["Content-Type", "video/mp4"]]) })) === "mp4");
check("video/mp2t 分片 → null（不收，否则灌满列表）", api.contentTypeMediaType(det({ responseHeaders: res([["Content-Type", "video/mp2t"]]) })) === null);
check("text/html → null", api.contentTypeMediaType(det({ responseHeaders: res([["Content-Type", "text/html"]]) })) === null);
check("无响应头 → null", api.contentTypeMediaType(det()) === null);

console.log("\nisLikelyFullFile（video/mp4 二次确认，挡 DASH 分片）：");
check("200 + 5MB + 无 Content-Range → 收", api.isLikelyFullFile(det({ statusCode: 200, responseHeaders: res([["Content-Length", String(5 * 1024 * 1024)]]) })) === true);
check("206 Partial Content → 拒（DASH/分段播放器分片）", api.isLikelyFullFile(det({ statusCode: 206, responseHeaders: res([["Content-Length", String(5 * 1024 * 1024)]]) })) === false);
check("200 但带 Content-Range → 拒", api.isLikelyFullFile(det({ statusCode: 200, responseHeaders: res([["Content-Range", "bytes 0-1048575/9999999"], ["Content-Length", String(5 * 1024 * 1024)]]) })) === false);
check("200 + 512KB → 拒（小分片/预览片）", api.isLikelyFullFile(det({ statusCode: 200, responseHeaders: res([["Content-Length", String(512 * 1024)]]) })) === false);
check("200 但拿不到 Content-Length → 拒（保守）", api.isLikelyFullFile(det({ statusCode: 200, responseHeaders: res([["Content-Type", "video/mp4"]]) })) === false);

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
