// 注释归属守卫：函数头上的注释块必须属于**紧随其后**的那个函数。
// 用法（在 edge_extension 目录下）：node tests/comment-ownership.test.js
//
// 缺陷形态（2026-09-23 质量审查发现）：在既有函数之间插入新函数时，新函数自带的
// 注释块会把上一个函数的 doc comment 顶开 ⇒ 注释描述的是 A、却挂在 B 头上，
// 而 A 自己没了注释。m3u8-parse.js 的 qualityFromURL / sanitizeQuality 就这样
// 换过头（sanitizeQuality 的注释块首行写着"qualityFromURL 从 URL 推断画质…"）。
//
// 它不影响任何行为，所以**任何行为测试都抓不到**；代价在下一次有人照着注释改
// 代码时兑现。判据必须够强才不误报：
//   **注释块首词恰好是本文件里另一个已定义函数的名字，且不是紧随其后的函数**
//   ⇒ 几乎必然是错位。
// 弱判据（"首词像个函数名"）假阳性太高。用同一判据扫过全项目 475 个函数时只
// 命中当时那一处，说明它不是系统性问题 —— 这条守卫的成本只是一个便宜的正则。
//
// 扫的是**手写源码**，不含 background.js / content-shared.js（打包产物，注释已被
// esbuild 剥掉，扫了也只会得到 0 命中那种"恒真绿"）。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");

function walk(dir, acc) {
  for (const e of fs.readdirSync(path.join(DIR, dir), { withFileTypes: true })) {
    if (e.name === "node_modules") continue;
    const rel = path.posix.join(dir, e.name);
    if (e.isDirectory()) walk(rel, acc);
    else if (rel.endsWith(".js")) acc.push(rel);
  }
  return acc;
}

const FILES = ["content.js", "content-main.js", "downloader.js"].concat(walk("src", []));

// 函数定义（允许缩进：content.js 的函数都在 IIFE 里）。只认 function 声明 ——
// 箭头函数常量没有稳定的"注释归属"语义，不纳入。
const FUNC_RE = /^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\(/;

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

console.log("A. 注释块首词 == 本文件另一个函数名（且不是紧随的函数）⇒ 判为错位");
let totalFuncs = 0;
const hits = [];
for (const rel of FILES) {
  const lines = fs.readFileSync(path.join(DIR, rel), "utf8").split("\n");
  const defs = [];
  lines.forEach((l, i) => {
    const m = l.match(FUNC_RE);
    if (m) defs.push({ name: m[1], line: i });
  });
  totalFuncs += defs.length;
  const names = new Set(defs.map((d) => d.name));
  for (const d of defs) {
    // 往上收集紧邻的注释块
    let j = d.line - 1;
    const block = [];
    while (j >= 0 && /^\s*(\/\/|\*|\/\*)/.test(lines[j])) {
      block.unshift(lines[j]);
      j--;
    }
    if (!block.length) continue;
    const first = block[0].replace(/^\s*\/\/\s*/, "").trim();
    const m = first.match(/^([A-Za-z_$][\w$]*)[\s(（:：]/);
    if (!m) continue;
    const word = m[1];
    if (word !== d.name && names.has(word)) {
      hits.push({ file: rel, line: d.line + 1, defined: d.name, mentioned: word });
    }
  }
}

// 防退化下限：枚举失效（目录遍历空掉、正则失灵）时上面会静默得 0 命中 ——
// 那时"没有错位"是假的。三个手写文件 + src/ 目前有三位数的函数。
check("扫到的函数数 ≥ 100（枚举/正则失效时这条会先红）", totalFuncs >= 100, totalFuncs);
check(
  "没有函数头顶着别人的注释块",
  hits.length === 0,
  hits.map((h) => `${h.file}:${h.line} 定义=${h.defined} 注释首词=${h.mentioned}`)
);

console.log(`\n${fail === 0 ? "全部通过" : "有失败"}：${pass} ok / ${fail} FAIL`);
process.exit(fail === 0 ? 0 : 1);
