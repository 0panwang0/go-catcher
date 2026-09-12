// 回归测试：manifest.json 与 package.json 的版本一致性（评审 P2-2）。
// 用法（在 edge_extension 目录下）：
//   node tests/manifest.test.js
//
// 背景：manifest "1.0.0" 与产品版本各自漂移过。现以 package.json 为单一
// 来源，build.mjs 构建时注入 manifest.json。本测试在"改了 package.json
// 却没跑 build 提交"时红灯——CI 的 git diff 校验是同一道闸的第二重。
const fs = require("fs");
const path = require("path");

const pkg = JSON.parse(fs.readFileSync(path.join(__dirname, "..", "package.json"), "utf8"));
const manifest = JSON.parse(
  fs.readFileSync(path.join(__dirname, "..", "manifest.json"), "utf8"),
);

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

check(
  "manifest.json 版本与 package.json 一致（单一来源）",
  manifest.version === pkg.version,
  { manifest: manifest.version, package: pkg.version },
);
check(
  "版本号形如 x.y.z",
  /^\d+\.\d+\.\d+$/.test(pkg.version || ""),
  pkg.version,
);

console.log(`\n${fail ? "FAIL" : "PASS"}: ${pass} ok, ${fail} failed`);
process.exitCode = fail ? 1 : 0;
