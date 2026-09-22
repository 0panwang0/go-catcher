// P0-2 守卫：innerHTML 插值必须转义 + quality 入口白名单（评审 P0-2）。
// 用法（在 edge_extension 目录下）：node tests/xss-guard.test.js
//
// 缺陷形态：downloader.js 的条目渲染里 title / host / url / pageUrl 都过了
// escapeHtml，**只有 info 那一处漏**；而 info 里含 item.quality，它的来源是
// qualityFromURL 的 ?quality= 分支 —— 内容由**被嗅探的页面**决定，于是构成
// 存储型 HTML 注入（先写进 storage，之后每次打开下载器页都重渲染）。
//
// 「同一段里只漏一处」说明的不是"谁手滑"，而是**没有机制在保证转义**：
// 浮层侧两个渲染点都转了（content.js:1071 / :1022），下载器页漏了。
// 所以本文件守两层，缺一层都不算收口：
//   1. 结构层：扫 downloader.js / content.js 的 innerHTML 模板，每个 ${…} 插值
//      必须被 escapeHtml 包裹，或在显式白名单里（白名单每条都要写清"为什么安全"）。
//   2. 入口层：qualityFromURL 对 ?quality= 的取值必须过字符白名单。
//
// 两条防退化门槛（本项目对"无区分力断言"的老要求）：
//   · 扫到的模板数低于下限直接 Fatal —— 解析器失效时不能退化成"什么都没验、然后全绿"；
//   · 白名单里每条都必须被实际命中 —— 陈旧条目会红，逼着人删，而不是攒一大坨挡箭牌。
const fs = require("fs");
const path = require("path");

const DIR = path.join(__dirname, "..");

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
// ============================================================
// 模板 / 插值解析
// ------------------------------------------------------------
// 要点：模板字符串**内部**（不在 ${} 里）的 " 与 ' 是普通字符（HTML 属性引号），
// 不是 JS 字符串定界符 —— 只有进入 ${} 之后才是。第一版没区分，被 HTML
// 属性引号整段吞掉，扫出来的插值数少一半。
// ============================================================

// 从开反引号处找闭合反引号
function findTplEnd(src, start) {
  let i = start + 1;
  let depth = 0;
  let inStr = null;
  while (i < src.length) {
    const c = src[i];
    if (inStr) {
      if (c === "\\") { i += 2; continue; }
      if (c === inStr) inStr = null;
      i++;
      continue;
    }
    if (depth > 0) {
      if (c === "\\") { i += 2; continue; }
      if (c === "'" || c === '"' || c === "`") { inStr = c; i++; continue; }
      if (c === "$" && src[i + 1] === "{") { depth++; i += 2; continue; }
      if (c === "}") { depth--; i++; continue; }
      i++;
      continue;
    }
    if (c === "$" && src[i + 1] === "{") { depth++; i += 2; continue; }
    if (c === "`") return i;
    i++;
  }
  return -1;
}

// 从 idx 起扫出该语句里的所有模板（遇到语句结束的分号就停）
function scanTemplates(src, from) {
  const out = [];
  let i = from;
  let depth = 0;
  while (i < src.length) {
    const c = src[i];
    if (c === "(" || c === "[") depth++;
    else if (c === ")" || c === "]") depth--;
    else if (c === ";" && depth <= 0) break;
    else if (c === "`") {
      const end = findTplEnd(src, i);
      if (end < 0) break;
      out.push({ raw: src.slice(i + 1, end), start: i });
      i = end + 1;
      continue;
    }
    i++;
  }
  return out;
}

// 从模板内容里抽 ${…} 表达式
function extractInterps(tpl) {
  const out = [];
  let i = 0;
  let depth = 0;
  let cur = "";
  let inStr = null;
  while (i < tpl.length) {
    const c = tpl[i];
    if (inStr) {
      if (c === "\\") { cur += c + (tpl[i + 1] ?? ""); i += 2; continue; }
      if (c === inStr) inStr = null;
      cur += c;
      i++;
      continue;
    }
    if (depth > 0) {
      if (c === "'" || c === '"' || c === "`") { inStr = c; cur += c; i++; continue; }
      if (c === "$" && tpl[i + 1] === "{") { depth++; cur += "${"; i += 2; continue; }
      if (c === "}") {
        depth--;
        if (depth === 0) { out.push(cur.trim()); cur = ""; } else cur += "}";
        i++;
        continue;
      }
      cur += c;
      i++;
      continue;
    }
    if (c === "$" && tpl[i + 1] === "{") { depth++; i += 2; continue; }
    i++;
  }
  return out;
}

// 收集一个文件里所有 innerHTML 模板的插值
function collect(fname) {
  const src = fs.readFileSync(path.join(DIR, fname), "utf8");
  const re = /\.innerHTML\s*(\+?=)/g;
  const interps = [];
  let tplCount = 0;
  let m;
  while ((m = re.exec(src))) {
    for (const t of scanTemplates(src, m.index + m[0].length)) {
      tplCount++;
      for (const e of extractInterps(t.raw)) {
        interps.push({ expr: e, norm: e.replace(/\s+/g, " ").trim(), line: src.slice(0, t.start).split("\n").length });
      }
    }
  }
  return { interps, tplCount };
}

// ============================================================
// 白名单：未经 escapeHtml 包裹、但**确实安全**的插值
// ------------------------------------------------------------
// 键 = 规范化空白后的整条表达式。写在这里的每一条，都要能回答
// "这个值为什么不可能带 HTML"；答不上来就该去转义，而不是往这里加。
// ============================================================
const SAFE_EXPR = new Map([
  [
    'kind ? `<span class="badge ${kindClass}">${kind}</span>` : ""',
    "kind 与 kindClass 都是本地字面量（主视频·main / 预览·preview / 分析失败·fail）",
  ],
  [
    '!item.analyzed ? \'<span class="badge">分析中…</span>\' : ""',
    "条件是布尔判断，插入内容是固定字面量，不含任何外部文本",
  ],
  ["srcLink", "上游构造：href 与文本两处都已 escapeHtml（downloader.js 的 srcLink 拼接）"],
  ["serverDownBanner", "上游构造：本地固定文案 + 端口号（来自本机服务应答，同源可信）"],
  ["rows", "上游构造：每行经 P.escapeHtml(optionMeta(o)) 后拼进模板（content.js:1022）"],
  ["options.length", "数字，无 HTML 语义"],
  ["b.ctl", "controlButtons() 的本地常量（动作名）"],
  ["b.label", "controlButtons() 的本地常量（按钮文案）"],
  ["CTL_STYLE[b.kind] || CTL_STYLE.plain", "本地样式常量表，未知 kind 落到 plain"],
]);

console.log("结构层：innerHTML 模板里的每个插值都要转义或在白名单里");
const scanned = new Map();
for (const f of ["downloader.js", "content.js"]) scanned.set(f, collect(f));

// 防退化门槛 1：模板数下限（解析器坏了要立刻知道，而不是静默全绿）
const MIN_TPL = { "downloader.js": 1, "content.js": 8 };
for (const [f, { tplCount }] of scanned) {
  check(`${f} 扫到 innerHTML 模板 ${tplCount} 个（≥ ${MIN_TPL[f]}）`, tplCount >= MIN_TPL[f], tplCount);
}

const hits = new Set();
let interpTotal = 0;
let escapedTotal = 0;
for (const [f, { interps }] of scanned) {
  const bad = [];
  for (const { expr, norm, line } of interps) {
    interpTotal++;
    if (/escapeHtml\s*\(/.test(expr)) { escapedTotal++; continue; }
    if (SAFE_EXPR.has(norm)) { hits.add(norm); continue; }
    bad.push(`L${line}: ${norm.slice(0, 70)}`);
  }
  check(`${f}：未转义的插值全在白名单里`, bad.length === 0, bad);
}
check(`插值总数 ${interpTotal} 已覆盖（>0）`, interpTotal > 0, interpTotal);

// 防退化门槛 2：白名单不许有陈旧条目
const stale = [...SAFE_EXPR.keys()].filter((k) => !hits.has(k));
check("白名单里没有陈旧条目（每条都被实际命中）", stale.length === 0, stale);
check(`白名单规模克制（${SAFE_EXPR.size} 条 ≤ 12）`, SAFE_EXPR.size <= 12, SAFE_EXPR.size);

// ============================================================
// 入口层：qualityFromURL 的 ?quality= 必须过字符白名单
// ------------------------------------------------------------
// downloader.js 是 ESM、本文件是 CommonJS，扫源码即可；
// 行为断言走 content-shared.js 里的同一份实现（src/background/m3u8-parse.js）。
// ============================================================
console.log("入口层：?quality= 的取值白名单");
require(path.join(DIR, "content-shared.js"));
const shared = globalThis.__m3u8Shared;
check("content-shared.js 挂出了 __m3u8Shared.qualityFromURL", !!shared && typeof shared.qualityFromURL === "function");

const PAYLOAD = "<img src=x onerror=alert(1)>";
const qURL = (v) => `https://cdn.x/a/index.m3u8?quality=${encodeURIComponent(v)}`;

check("escapeHtml 把 <img 变成 &lt;img（浏览器不会再当标签解析）",
  !/<img/.test(shared.escapeHtml(PAYLOAD)) && /&lt;img/.test(shared.escapeHtml(PAYLOAD)),
  shared.escapeHtml(PAYLOAD));
check("escapeHtml 覆盖单引号（属性值可能用单引号包裹）",
  shared.escapeHtml("' onerror='x") === "&#39; onerror=&#39;x", shared.escapeHtml("' onerror='x"));

check("qualityFromURL 拒绝含 HTML 的 ?quality=（返回空串而不是原样带回）",
  shared.qualityFromURL(qURL(PAYLOAD)) === "", shared.qualityFromURL(qURL(PAYLOAD)));
check("qualityFromURL 拒绝带引号的值",
  shared.qualityFromURL(qURL('1080p" onload="x')) === "", shared.qualityFromURL(qURL('1080p" onload="x')));
check("qualityFromURL 依然认得合法短 token",
  shared.qualityFromURL(qURL("1080p")) === "1080p" &&
  shared.qualityFromURL(qURL("1440")) === "1440" &&
  shared.qualityFromURL(qURL("HD")) === "HD");
check("qualityFromURL 放行 variantQuality 产出的带宽形态（带空格，别误杀）",
  shared.qualityFromURL(qURL("5.0 Mbps")) === "5.0 Mbps", shared.qualityFromURL(qURL("5.0 Mbps")));
check("qualityFromURL 拒绝超长值（白名单有长度上限）",
  shared.qualityFromURL(qURL("a".repeat(64))) === "");
check("qualityFromURL 的路径分支不受影响（1080p 目录仍命中）",
  shared.qualityFromURL("https://cdn.x/a/1080p/v.m3u8") === "1080P");

// ---- 回灌路径（二次入口）----
// openDownloader 把 item.quality 拼进 downloader.html 的 URL，下载器页读回时
// 必须过**同一道**白名单；否则"下载器页 → 再开一个下载器页"就绕过了收窄。
check("sanitizeQuality 拒含 HTML 的值", shared.sanitizeQuality(PAYLOAD) === "", shared.sanitizeQuality(PAYLOAD));
check("sanitizeQuality 放行合法短 token 与带宽形态",
  shared.sanitizeQuality("1080p") === "1080p" && shared.sanitizeQuality("5.0 Mbps") === "5.0 Mbps");
check("sanitizeQuality 对 null / undefined 安全（URL 上没带该参数时）",
  shared.sanitizeQuality(null) === "" && shared.sanitizeQuality(undefined) === "");

const dlSrc = fs.readFileSync(path.join(DIR, "downloader.js"), "utf8");
check("downloader.js 引入 sanitizeQuality",
  /import\s*\{[^}]*sanitizeQuality[^}]*\}\s*from\s*"\.\/src\/background\/m3u8-parse\.js"/.test(dlSrc));
check("downloader.js 不再裸取 URL 上的 quality（autoQuality 必须包 sanitizeQuality）",
  !/autoQuality\s*=\s*params\.get\("quality"\)/.test(dlSrc) &&
  /autoQuality\s*=\s*sanitizeQuality\(params\.get\("quality"\)\)/.test(dlSrc));

console.log(`\n${pass} ok / ${fail} FAIL`);
process.exit(fail === 0 ? 0 : 1);
