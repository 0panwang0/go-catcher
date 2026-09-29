// server-api.js 的 queryDownload 归一化回归测试（R2）。
// 用法（在 edge_extension 目录下）：node tests/serverapi.test.js
//
// 缺陷形态（本项目头号）：后端把"直播故障中断收尾"表达成
//   done=true + interrupted=true + 有效 finalPath
// —— 成品**已保存但没录完**。归一化时若只认 finalPath，就把它读成
// state:"complete"，扩展面板弹 🎉「下载完成」，把"产物不完整"藏起来。
//
// 守三件事（前两条成对）：
//   1. interrupted 必须归一化成 state:"interrupted"，且带出 gapSeconds/fileMissing；
//   2. 正常完成（无 interrupted）必须仍是 state:"complete" —— 少了这条，
//      把 done 分支整个删掉也能让第 1 条"绿"（恒真）；
//   3. content.js 结构守卫：error 分支不得再吞 interrupted，且存在 interrupted 面板。
//
// 加载方式：server-api.js 是 ESM（src/background/），CJS 用动态 import() 加载**源码**
// （不是 background.js 产物）。顶层只碰 chrome.storage.onChanged.addListener，
// 给最小桩即可；fetch 桩按 URL 返回固定 JSON。
const fs = require("fs");
const path = require("path");
const { pathToFileURL } = require("url");

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

function jsonResponse(obj) {
  return {
    ok: true,
    status: 200,
    json: async () => obj,
    text: async () => JSON.stringify(obj),
  };
}

async function main() {
  let statusTask = {}; // 各用例设定：/status 返回哪个任务对象

  globalThis.chrome = {
    storage: {
      // server-api 的 getSettings 用对象默认值调用；原样回填即得可用 port。
      local: {
        get: async (d) => (d && typeof d === "object" ? { ...d } : {}),
        set: async () => {},
      },
      onChanged: { addListener: () => {} },
    },
    runtime: {
      lastError: null,
      connectNative: () => ({ onDisconnect: { addListener() {} }, postMessage() {}, disconnect() {} }),
      onMessage: { addListener: () => {} },
    },
  };
  globalThis.fetch = async (url) => {
    const u = String(url);
    if (u.includes("/svc/info")) return jsonResponse({ token: "tok" });
    if (u.includes("/status")) return jsonResponse(statusTask);
    return jsonResponse({});
  };

  const modUrl = pathToFileURL(path.join(__dirname, "..", "src", "background", "server-api.js")).href;
  const { queryDownload } = await import(modUrl);

  // 1) 中断收尾：done + interrupted + 有效 finalPath ⇒ state:"interrupted"
  statusTask = {
    id: "t1",
    done: true,
    canceled: false,
    interrupted: true,
    finalPath: "D:/out/live.mp4",
    stage: "录制中断 · 已保存",
    error: "拉流失败",
    gapSeconds: 42,
    fileMissing: false,
  };
  let r = await queryDownload({ taskId: "t1" });
  check("中断收尾归一化为 state='interrupted'", r.state === "interrupted", r);
  check("中断带出 finalPath", r.finalPath === "D:/out/live.mp4", r.finalPath);
  check("中断带出 gapSeconds", r.gapSeconds === 42, r.gapSeconds);
  check("中断带出 fileMissing", r.fileMissing === false, r.fileMissing);

  // 中断 + 文件已失效：fileMissing 必须为真（消费侧据此提示"已被移动或删除"）
  statusTask = { id: "t1", done: true, interrupted: true, finalPath: "D:/out/live.mp4", fileMissing: true };
  r = await queryDownload({ taskId: "t1" });
  check("中断且文件失效时 fileMissing=true", r.state === "interrupted" && r.fileMissing === true, r);

  // 2) 正常完成（无 interrupted）⇒ 仍是 state:"complete"
  statusTask = { id: "t2", done: true, canceled: false, interrupted: false, finalPath: "D:/out/v.mp4", stage: "已保存" };
  r = await queryDownload({ taskId: "t2" });
  check("正常完成仍是 state='complete'", r.state === "complete", r);

  // 3) R3：停止且无内容（done + 无 finalPath + 非取消/中断 + 无 error）⇒ stoppedEmpty
  statusTask = { id: "t3", done: true, canceled: false, interrupted: false, finalPath: "", error: "", stage: "已停止（无内容）" };
  r = await queryDownload({ taskId: "t3" });
  check("停止且无内容归一化为 state='stoppedEmpty'", r.state === "stoppedEmpty", r);

  // 成对：真失败（有 error）必须仍是 state='error' —— 否则"什么都算 stoppedEmpty"也能绿
  statusTask = { id: "t4", done: true, canceled: false, finalPath: "", error: "拉流失败", stage: "失败" };
  r = await queryDownload({ taskId: "t4" });
  check("失败任务仍是 state='error'", r.state === "error", r);

  // 4) content.js 结构守卫：error 分支不得再吞 interrupted，且存在 interrupted 面板
  const contentSrc = fs.readFileSync(path.join(__dirname, "..", "content.js"), "utf8");
  check(
    "content.js 的 error 分支不再包含 interrupted",
    !/state\s*===\s*"error"\s*\|\|\s*state\s*===\s*"interrupted"/.test(contentSrc),
    null
  );
  check("content.js 有 interrupted 面板分支", contentSrc.includes('showPanel("interrupted"'), null);
  check("content.js 有 stoppedEmpty 面板分支", contentSrc.includes('showPanel("stoppedEmpty"'), null);
}

main()
  .then(() => {
    console.log(`\n${pass} ok / ${fail} FAIL`);
    process.exit(fail ? 1 : 0);
  })
  .catch((e) => {
    console.error("测试异常：", e);
    process.exit(1);
  });
