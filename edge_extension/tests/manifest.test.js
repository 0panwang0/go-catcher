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

// ---- host_permissions 冗余（评审 P3-10）----
//
// 为什么值得单独钉：冗余的窄模式（如 `http://127.0.0.1/*`）**不改变实际权限**，
// 却会让人看权限列表时以为"本扩展只申请了访问本机"——而真正生效的是同 scheme
// 的宽模式 `http://*/*`（它已经覆盖了本机与任意站点）。权限声明必须让人一眼看
// 出真实范围，有宽模式就不要留窄的同 scheme 项。
//
// 判据只做确定的包含关系（同 scheme 的 `scheme://*/*`、`<all_urls>`），不做
// 通配符语义的完整推导：宁可漏报也不误报——误报会挡住合法的窄化写法。
function parsePattern(p) {
  const i = p.indexOf("://");
  if (i < 0) return null;
  const rest = p.slice(i + 3);
  const j = rest.indexOf("/");
  if (j < 0) return null;
  return { scheme: p.slice(0, i), host: rest.slice(0, j), path: rest.slice(j) };
}

function findRedundantHostPermissions(list) {
  const out = [];
  const seen = new Set();
  for (const p of list) {
    if (seen.has(p)) out.push([p, "与另一条重复"]);
    seen.add(p);
  }
  for (const p of list) {
    if (p === "<all_urls>") continue;
    const parts = parsePattern(p);
    if (!parts) continue;
    const broad = `${parts.scheme}://*/*`;
    if (broad !== p && list.includes(broad)) out.push([p, `已被 ${broad} 覆盖`]);
    else if (list.includes("<all_urls>")) out.push([p, "已被 <all_urls> 覆盖"]);
  }
  return out;
}

const hosts = manifest.host_permissions || [];
const redundant = findRedundantHostPermissions(hosts);
check(
  "host_permissions 无冗余（宽模式已覆盖的窄模式必须删掉）",
  redundant.length === 0,
  { hosts, redundant },
);

// 反向自我校验：判据必须真的能抓到 P3-10 那一对，否则上面那条是空断言。
check(
  "冗余判据本身有效（能抓到 127.0.0.1/localhost 被 http://*/* 覆盖）",
  findRedundantHostPermissions([
    "http://*/*",
    "http://127.0.0.1/*",
    "http://localhost/*",
  ]).length === 2,
);

// 合法写法不得误报：只申请本机（没有 http://*/*）时一条都不算冗余。
check(
  "只申请本机时不误报",
  findRedundantHostPermissions(["http://127.0.0.1/*", "http://localhost/*"]).length === 0,
);

console.log(`\n${fail ? "FAIL" : "PASS"}: ${pass} ok, ${fail} failed`);
process.exitCode = fail ? 1 : 0;
