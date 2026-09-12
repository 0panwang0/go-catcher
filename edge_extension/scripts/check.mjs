// 扩展侧完整校验（Makefile ext-check / CI extension job 的本地等价物）：
//   打包 → bundle 一致性 → 语法 → 回归测试。
//
// 为什么用 Node 实现而不是 Makefile 里的 sh 语法：Windows 上的 mingw32-make
// 找不到 sh 时默认用 cmd.exe 当 shell，`{ ... }` / `$$(...)` / `[ -x ]` 等
// sh 语法在 cmd 下全部失效（实测 '{' is not recognized）。把这些逻辑搬进
// Node，Makefile 只剩一句 sh/cmd 通用的 `node scripts/check.mjs`。
//
// 一致性判据是「打包前后 sha1 是否变化」而非 git diff：
//   本地改了 src/ 还没提交时 git diff 必然非空（那是正常的，不该报错）；
//   sha1 只在「改了 src/ 却忘了重新打包」时命中，两种场景都对。
// manifest.json 用同一判据：build.mjs 会把 package.json 版本注入 manifest，
// 改了版本没跑 build 时在这里被拦下（版本单一来源，评审 P2-2）。
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));

function run(args, opts = {}) {
  const r = spawnSync(process.execPath, args, { stdio: "inherit", cwd: root, ...opts });
  return r.status ?? 1;
}

function sha1(file) {
  return createHash("sha1").update(readFileSync(path.join(root, file))).digest("hex");
}

function fail(msg) {
  console.error(`ext-check 失败：${msg}`);
  process.exit(1);
}

// 1) 打包（顺带完成 manifest 版本注入）
const bundleBefore = sha1("background.js");
const mfBefore = sha1("manifest.json");
if (run(["build.mjs"]) !== 0) fail("node build.mjs 未通过");
const bundleAfter = sha1("background.js");
const mfAfter = sha1("manifest.json");
if (bundleBefore !== bundleAfter) {
  fail("background.js 与 src/ 不一致（重新打包后内容有变），请提交更新后的 bundle");
}
if (mfBefore !== mfAfter) {
  fail("manifest.json 版本与 package.json 不一致，请提交 build.mjs 同步后的 manifest");
}
console.log("bundle / manifest 一致性 OK");

// 2) 语法检查：content script（经典脚本无法 import）与打包产物一并校验
const syntaxFiles = ["content.js", "background.js", "content-main.js", "downloader.js"];
for (const f of syntaxFiles) {
  if (run(["--check", f]) !== 0) fail(`node --check ${f} 未通过`);
}
const manifest = JSON.parse(readFileSync(path.join(root, "manifest.json"), "utf8"));
if (!manifest.version) fail("manifest.json 缺少 version");
console.log("语法 / manifest JSON OK");

// 3) 回归测试
for (const t of readdirSync(path.join(root, "tests")).filter((f) => f.endsWith(".test.js"))) {
  console.log(`---- ${t}`);
  if (run([path.join("tests", t)]) !== 0) fail(`测试 ${t} 未通过`);
}
console.log("extension OK");
