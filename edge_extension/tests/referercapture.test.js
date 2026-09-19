// 回归测试：分片 Referer 捕获与下载注入（白名单型防盗链把分片 403 掉的扩展侧修复面）。
// 用法（在 edge_extension 目录下）：node tests/referercapture.test.js
//
// 根因：一部分站点的分片防盗链是**白名单**语义 —— 浏览器对分片不带 Referer（页面声明
// no-referrer），空 Referer 与白名单域名放行，而旧实现在下载时一律塞页面 Referer，
// 于是必 403。修复：
//   1. referer-capture 经 onSendHeaders 按「分片主机」记录浏览器实际 Referer，
//      **包括空串**：空串 = 浏览器没带，是有效信号，不是「缺项」；
//   2. downloadViaServer 把 host→Referer 序列化成 segrefs 传给 Go；
//   3. Go 抓分片时按 host 选用（Go 侧见 internal/core/segrefs_test.go）。
// 本文件守 1、2 两点：抽取纯函数、持久化往返（含 SW 重启）、侦听接线、参数注入。
const fs = require("fs");
const path = require("path");

const noop = { addListener() {} };
const fetchCalls = [];

function fakeResp(body) {
  return {
    ok: true,
    status: 200,
    text: async () => (typeof body === "string" ? body : ""),
    json: async () => (typeof body === "object" ? body : {}),
  };
}

globalThis.fetch = async (url) => {
  const u = String(url);
  fetchCalls.push(u);
  if (u.includes("/health")) return fakeResp("ok");
  if (u.includes("/svc/info")) return fakeResp({ token: "tok", exe: "x" });
  if (u.includes("/pickdir")) return fakeResp({ dir: "C:\\dl" });
  if (u.includes("/download")) return fakeResp({ started: true, id: "t1" });
  return fakeResp({});
};

// makeChrome 造一份 chrome 桩：sessionStore 模拟 chrome.storage.session（跨 SW 重启
// 存活的会话内存），hooks.onSend 暴露 installRefererCapture 注册下来的回调。
function makeChrome(sessionStore) {
  const hooks = { onSend: null };
  const chrome = {
    action: { onClicked: noop },
    webRequest: {
      onCompleted: noop,
      onSendHeaders: { addListener(cb) { hooks.onSend = cb; } },
    },
    runtime: { onMessage: noop, getURL: (p) => p, lastError: undefined, sendNativeMessage() {} },
    tabs: {
      get() {},
      create() {},
      onRemoved: noop,
      query: async () => [],
    },
    declarativeNetRequest: {
      getDynamicRules: async () => [],
      updateDynamicRules: async () => {},
    },
    storage: {
      onChanged: noop,
      local: {
        async get(defaults) { return { ...defaults, apiToken: "tok" }; },
        async set() {},
      },
      session: {
        async get(key) { return { [key]: sessionStore[key] }; },
        async set(obj) { Object.assign(sessionStore, obj); },
      },
    },
  };
  return { chrome, hooks };
}

// loadApi 从打包产物里取一份**全新**的模块实例：模块级的 host→Referer 缓存各实例
// 独立，但可以共享同一份 sessionStore —— 这正是「service worker 被回收后重启」的模型，
// 用来验证从 storage.session 恢复时不会把空值条目丢掉。
function loadApi(chrome) {
  return new Function("chrome", src + "\nreturn __m3u8catcher.__test__;")(chrome);
}

const src = fs.readFileSync(path.join(__dirname, "..", "background.js"), "utf8");
const store = {};
const { chrome, hooks } = makeChrome(store);
const api = loadApi(chrome);

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

const TICK = () => new Promise((r) => setTimeout(r, 0));
const has = (obj, key) => Object.prototype.hasOwnProperty.call(obj, key);

// CDN_HOST 下的分片带解析站 Referer；MEDIA_HOST 下的分片浏览器根本不带 Referer。
const CDN_HOST = "cdn.example:8443";
const MEDIA_HOST = "media.example";
const PARSER_REF = "https://parser.example/";
const PAGE_REF = "https://page.example/";

(async () => {
  console.log("refererFromDetails：从 onSendHeaders details 抽取分片 host 与 Referer");
  check("媒体 xhr 命中且带上 host：port",
    (() => {
      const r = api.refererFromDetails({
        tabId: 7,
        type: "xmlhttprequest",
        url: `http://${CDN_HOST}/seg-1.ts`,
        requestHeaders: [
          { name: "Accept", value: "*/*" },
          { name: "Referer", value: PARSER_REF },
        ],
      });
      return r && r.host === CDN_HOST && r.referer === PARSER_REF;
    })());
  check("Referer 头大小写不敏感",
    (() => {
      const r = api.refererFromDetails({
        tabId: 7, type: "xmlhttprequest", url: "http://a.com/x.ts",
        requestHeaders: [{ name: "referer", value: "https://b.com/" }],
      });
      return r && r.referer === "https://b.com/";
    })());
  check("非媒体类型（main_frame/script 等）不捕获",
    api.refererFromDetails({ tabId: 7, type: "script", url: "http://a.com/x.js", requestHeaders: [{ name: "Referer", value: "https://b.com/" }] }) === null);
  check("<video> 直连分片（type=media）捕获",
    (() => {
      const r = api.refererFromDetails({ tabId: 7, type: "media", url: "http://a.com/x.ts", requestHeaders: [{ name: "Referer", value: "https://b.com/" }] });
      return r && r.referer === "https://b.com/";
    })());
  // 站点声明 no-referrer 时浏览器对分片**根本不发** Referer 头。旧实现把它当 null 丢掉，
  // 于是下载层回退页面 Referer ⇒ 白名单型 CDN 403。这里必须返回 host + 空串。
  check("无 Referer 头：仍返回 host，referer 为空串（不是 null）",
    (() => {
      const r = api.refererFromDetails({ tabId: 7, type: "xmlhttprequest", url: `http://${MEDIA_HOST}/seg-1.ts`, requestHeaders: [] });
      return r && r.host === MEDIA_HOST && r.referer === "";
    })());
  check("tabId<0（无关联页签）不捕获",
    api.refererFromDetails({ tabId: -1, type: "xmlhttprequest", url: "http://a.com/x.ts", requestHeaders: [{ name: "Referer", value: "https://b.com/" }] }) === null);

  console.log("capture / segmentReferers：host→Referer 持久化往返");
  await api.captureSegmentReferer(CDN_HOST, PARSER_REF);
  await TICK();
  let map = await api.segmentReferers();
  check("写入后可读到映射", map[CDN_HOST] === PARSER_REF, map);
  check("同值重复写入不丢（幂等）",
    (await api.captureSegmentReferer(CDN_HOST, PARSER_REF), (await api.segmentReferers())[CDN_HOST] === PARSER_REF));
  await api.captureSegmentReferer("b.com", "https://site.example/");
  await TICK();
  map = await api.segmentReferers();
  check("多 host 并存", map["b.com"] === "https://site.example/" && map[CDN_HOST] === PARSER_REF, map);
  // 空串是有效值：它让 Go 侧知道「这个 host 不要塞页面 Referer」。
  await api.captureSegmentReferer(MEDIA_HOST, "");
  await TICK();
  map = await api.segmentReferers();
  check("空值条目落表（表示浏览器没带 Referer）",
    has(map, MEDIA_HOST) && map[MEDIA_HOST] === "", map);

  console.log("侦听接线：onSendHeaders 回调把真实分片 flows 进映射");
  check("onSend 侦听已注册", typeof hooks.onSend === "function");
  hooks.onSend({
    tabId: 9,
    type: "xmlhttprequest",
    url: `http://${CDN_HOST}/seg-2.ts`,
    requestHeaders: [{ name: "Referer", value: PARSER_REF }],
  });
  await TICK();
  map = await api.segmentReferers();
  check("经 onSend 喂入的 host 进映射（且不重复）", map[CDN_HOST] === PARSER_REF, map);
  hooks.onSend({
    tabId: 9,
    type: "xmlhttprequest",
    url: `http://${MEDIA_HOST}/seg-2.ts`,
    requestHeaders: [{ name: "Accept", value: "*/*" }], // 无 Referer 头
  });
  await TICK();
  map = await api.segmentReferers();
  check("经 onSend 喂入的「无 Referer」请求，空值也进映射",
    has(map, MEDIA_HOST) && map[MEDIA_HOST] === "", map);

  console.log("SW 重启：从 storage.session 恢复时不丢空值条目");
  const restarted = loadApi(makeChrome(store).chrome);
  const restored = await restarted.segmentReferers();
  check("重启后空值条目仍在",
    has(restored, MEDIA_HOST) && restored[MEDIA_HOST] === "", restored);
  check("重启后非空条目仍在", restored[CDN_HOST] === PARSER_REF, restored);

  console.log("downloadViaServer：把 captured segrefs 注入 /download");
  const res = await api.downloadViaServer({
    m3u8Url: "https://parser.example/secure.php?x=1",
    referer: PAGE_REF,
    title: "样例",
    filename: "样例.ts",
  });
  check("下载启动返回 ok", res && res.ok === true, res);
  const dlCall = fetchCalls.find((u) => u.includes("/download"));
  check("发出了 /download 请求", !!dlCall, fetchCalls);
  const segRefsParam = new URL(dlCall).searchParams.get("segrefs");
  check("segrefs 参数存在", !!segRefsParam, dlCall);
  const decoded = segRefsParam ? JSON.parse(decodeURIComponent(segRefsParam)) : {};
  check("segrefs 含分片 host→解析站 Referer", decoded[CDN_HOST] === PARSER_REF, decoded);
  check("segrefs 保留空值条目（Go 据此不设 Referer 头）",
    has(decoded, MEDIA_HOST) && decoded[MEDIA_HOST] === "", decoded);
  check("referer 参数仍是页面域名（m3u8/secure.php 走页面 Referer）",
    new URL(dlCall).searchParams.get("referer") === PAGE_REF, dlCall);

  // 只捕获到空值条目的站点：segrefs 也必须照样发出去（旧实现里空值被丢光 ⇒
  // 映射为空 ⇒ 参数根本不带 ⇒ 下载层回退页面 Referer ⇒ 403）。
  console.log("只有空值条目的会话：segrefs 仍必须发出");
  const onlyEmpty = { segRefByHost: { [MEDIA_HOST]: "" } };
  const iso = loadApi(makeChrome(onlyEmpty).chrome);
  const before = fetchCalls.length;
  const isoRes = await iso.downloadViaServer({
    m3u8Url: "https://parser.example/secure.php?x=2",
    referer: PAGE_REF,
    title: "样例2",
    filename: "样例2.ts",
  });
  check("下载启动返回 ok（仅空值条目）", isoRes && isoRes.ok === true, isoRes);
  const isoCall = fetchCalls.slice(before).find((u) => u.includes("/download"));
  const isoParam = isoCall ? new URL(isoCall).searchParams.get("segrefs") : null;
  const isoDecoded = isoParam ? JSON.parse(decodeURIComponent(isoParam)) : {};
  check("segrefs 参数带上了空值条目",
    has(isoDecoded, MEDIA_HOST) && isoDecoded[MEDIA_HOST] === "" && Object.keys(isoDecoded).length === 1, isoDecoded);

  console.log(`\n${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
})().catch((e) => {
  console.error("测试异常:", e);
  process.exit(1);
});
