// 回归测试：原生消息唤起（本地服务没跑时把客户端拉起来）。
// 用法（在 edge_extension 目录下）：
//   node tests/nativehost.test.js
//
// 这条通道的失败形态很隐蔽：宿主名写错、断开不处理、超时不清、并发重复拉起，
// 表现统统是"点了下载没反应"。所以在桩上把成功 / 报错 / 断开 / 超时 / 并发
// 五条路径都钉住。
const fs = require("fs");
const path = require("path");

const noop = { addListener() {} };

// 每次测试替换这个行为，模拟 connectNative 返回什么
let connectBehavior = () => {
  throw new Error("测试未设置 connectBehavior");
};

const chrome = {
  action: { onClicked: noop, setBadgeText() {}, setBadgeBackgroundColor() {} },
  webRequest: { onCompleted: noop },
  runtime: {
    onMessage: noop,
    getURL: (p) => p,
    lastError: undefined,
    connectNative: (name) => connectBehavior(name),
  },
  tabs: { get() {}, create() {}, onRemoved: noop, query: async () => [] },
  declarativeNetRequest: {
    getDynamicRules: async () => [],
    updateDynamicRules: async () => {},
  },
  storage: {
    onChanged: noop,
    local: { get: async (d) => d, set: async () => {} },
  },
};

const src = fs.readFileSync(path.join(__dirname, "..", "background.js"), "utf8");
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

// 假的 NativePort：记录 postMessage / disconnect，并能手动触发 onMessage / onDisconnect
function fakePort() {
  const msgListeners = [];
  const discListeners = [];
  const port = {
    posted: [],
    disconnected: 0,
    onMessage: { addListener: (f) => msgListeners.push(f) },
    onDisconnect: { addListener: (f) => discListeners.push(f) },
    postMessage(m) {
      port.posted.push(m);
    },
    disconnect() {
      port.disconnected++;
    },
    emitMessage(m) {
      msgListeners.forEach((f) => f(m));
    },
    emitDisconnect() {
      discListeners.forEach((f) => f());
    },
  };
  return port;
}

(async () => {
  console.log("宿主正常应答：");
  {
    let held = null;
    connectBehavior = () => {
      held = fakePort();
      setTimeout(() => held.emitMessage({ ok: true, port: 7891 }), 0);
      return held;
    };
    const r = await api.requestWake(1000);
    check("带回宿主响应（含端口）", r && r.ok === true && r.port === 7891, r);
    check(
      "发出的是 ensure-server 请求",
      held.posted.length === 1 && held.posted[0].type === "ensure-server",
      held.posted
    );
    check("拿到响应后主动断开连接", held.disconnected === 1, held.disconnected);
  }

  console.log("宿主报错但活着：");
  {
    connectBehavior = () => {
      const p = fakePort();
      setTimeout(() => p.emitMessage({ ok: false, error: "客户端已启动但服务未就绪" }), 0);
      return p;
    };
    const r = await api.requestWake(1000);
    check("原样带回失败响应（文案留给上层决定）", r && r.ok === false && /未就绪/.test(r.error), r);
  }

  console.log("宿主不可用（未登记 / 被策略拦下）：");
  {
    connectBehavior = () => {
      const p = fakePort();
      setTimeout(() => p.emitDisconnect(), 0);
      return p;
    };
    const r = await api.requestWake(1000);
    check("返回 null 而不是抛错", r === null, r);
  }
  {
    connectBehavior = () => {
      throw new Error("Specified native messaging host not found.");
    };
    const r = await api.requestWake(1000);
    check("connectNative 抛错时也返回 null", r === null, r);
  }
  {
    // 断开时的 lastError 是"宿主没登记 / 被策略拦下 / 宿主一起步就退出"的唯一线索，
    // 必须记进控制台（service worker 的 console 是用户唯一能看到的地方）。
    const warns = [];
    const realWarn = console.warn;
    console.warn = (...a) => warns.push(a.map(String).join(" "));
    try {
      chrome.runtime.lastError = { message: "Specified native messaging host not found." };
      connectBehavior = () => {
        const p = fakePort();
        setTimeout(() => p.emitDisconnect(), 0);
        return p;
      };
      const r = await api.requestWake(1000);
      check(
        "把 lastError 原文记进控制台（排障唯一线索）",
        r === null && warns.some((w) => /not found/.test(w)),
        warns
      );
    } finally {
      console.warn = realWarn;
      chrome.runtime.lastError = undefined;
    }
  }

  console.log("宿主不回话：");
  {
    connectBehavior = () => fakePort(); // 永不回话，也不断开
    const t0 = Date.now();
    const r = await api.requestWake(120);
    const ms = Date.now() - t0;
    check("按时超时并返回 null（不挂住调用方）", r === null && ms >= 100, { r, ms });
  }

  console.log("并发唤起：");
  {
    let created = 0;
    connectBehavior = () => {
      created++;
      const p = fakePort();
      setTimeout(() => p.emitMessage({ ok: true }), 10);
      return p;
    };
    const [a, b] = await Promise.all([api.requestWake(1000), api.requestWake(1000)]);
    check("同时发起只建一条连接（不让用户连点重复拉进程）", created === 1, { created });
    check("两个调用拿到同一结果", a && b && a.ok === true && b.ok === true, { a, b });
  }

  console.log("常量一致性：");
  // 与 Go 侧常量真身逐字对照，而不是测试内的硬编码字面量 —— 那样只证明
  // "JS 侧没改"，防不了两侧漂移（评审自审 #15）。两处漂移的症状是
  // "唤起永远失败且无任何报错"，这正是本文件要防的头号故障。
  const goSrc = fs.readFileSync(
    path.join(__dirname, "..", "..", "internal", "platform", "nativehost_windows.go"),
    "utf8"
  );
  const goMatch = goSrc.match(/NativeHostName\s*=\s*"([^"]+)"/);
  check("Go 侧 NativeHostName 常量存在且可提取", !!goMatch, goMatch && goMatch[1]);
  check(
    "宿主名与 Go 侧登记的 NATIVE_HOST_NAME 一致",
    !!goMatch && api.NATIVE_HOST_NAME === goMatch[1],
    { js: api.NATIVE_HOST_NAME, go: goMatch && goMatch[1] }
  );

  console.log(`\n${pass} passed, ${fail} failed`);
  process.exit(fail === 0 ? 0 : 1);
})();
