// 兜底命令的参数净化（新-1）。
// 用法（在 edge_extension 目录下）：node tests/cli-args.test.js
//
// 缺陷形态：quoteArg 只剔控制字符与双引号，而**双引号内仍会被 shell 特殊解释**的
// 字符一个都没剔 —— `$`、反引号、`!`。其中 `$` 在 PowerShell / bash 的双引号内会
// 触发子表达式，`$(...)` / `${...}` 是**真的执行命令**。而这个值有真实可达路径：
// 浮层的 `-o` 由页面 <title> 派生（站点完全可控），用户在「服务没起来」时复制那条
// 兜底命令去终端跑 —— 于是页面标题变成一条本地命令。
//
// 同一函数里还有第二处漏：`-o "${outName}"` 是**唯一没过 quoteArg 的参数**，而同文件
// 上方注释恰好写着"每个值先过 quoteArg 再包双引号"（注释与代码自相矛盾）。
//
// 本文件成对守两件事（缺一样都不算收口）：
//   1. 该剔的剔了：注入字符进不了命令；
//   2. 该留的留了：URL 的 `%`、Windows 路径的 `\`、中文与空格必须原样保留 ——
//      否则"修好了注入、修坏了命令"，用户拿到的仍是一条跑不通的命令。
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

// cli-args.js 是 ES 模块，但没有 import（只有 export function）。去掉 `export `
// 前缀后就是普通声明，可以直接交给 new Function —— 与 downloader.test.js 的
// "极简展开"同一路子，不依赖 package.json 的 type。
const raw = fs.readFileSync(path.join(DIR, "src/background/cli-args.js"), "utf8");
const src = raw.replace(/^export\s+/gm, "");
const api = new Function(
  src + "\nreturn { quoteArg, sanitizeFileName, assembleGoCommand };"
)();

// ------------------------------------------------------------
// 1) 该剔的剔了
// ------------------------------------------------------------
console.log("quoteArg：会执行命令 / 突破引号的字符必须剔除");
const INJECTIONS = [
  ["$", "a$(whoami)b"],
  ["反引号", "a`whoami`b"],
  ["双引号", 'a"b'],
  ["!（bash 历史展开）", "a!ls b"],
  ["控制字符（换行）", "a\nb"],
];
for (const [label, input] of INJECTIONS) {
  const out = api.quoteArg(input);
  const bad = /["`$!\u0000-\u001f]/.test(out);
  check(`剔除 ${label}`, !bad, out);
}

// ------------------------------------------------------------
// 2) 该留的留了（否则是把可用命令改成不可用命令）
// ------------------------------------------------------------
console.log("quoteArg：合法值必须原样保留");
check("保留 %（URL 百分号编码）", api.quoteArg("v/%E4%B8%AD%20x.m3u8") === "v/%E4%B8%AD%20x.m3u8", api.quoteArg("v/%E4%B8%AD%20x.m3u8"));
check("保留 \\（Windows 路径必需）", api.quoteArg("C:\\Program Files\\go-catcher.exe") === "C:\\Program Files\\go-catcher.exe", api.quoteArg("C:\\Program Files\\go-catcher.exe"));
check("保留 & ? =（URL 查询串）", api.quoteArg("?a=1&b=2") === "?a=1&b=2", api.quoteArg("?a=1&b=2"));
check("保留中文与空格", api.quoteArg("我的 视频 1080P.ts") === "我的 视频 1080P.ts", api.quoteArg("我的 视频 1080P.ts"));
check("保留单引号（双引号内是字面量）", api.quoteArg("it's ok.ts") === "it's ok.ts", api.quoteArg("it's ok.ts"));

// ------------------------------------------------------------
// 3) assembleGoCommand：四个参数全部净化（-o 曾经漏过）
// ------------------------------------------------------------
console.log("assembleGoCommand：每个值都过净化，且结构不变");
const TITLE = 'a$(calc)b`c`d"e';
const cmd = api.assembleGoCommand(
  "https://cdn.example.com/%E4%B8%AD/v.m3u8",
  "https://page.example.com/watch?t=1&s=2",
  TITLE,
  "C:\\Program Files\\go-catcher.exe"
);
check("命令里不出现 $（注入被剔除）", !cmd.includes("$"), cmd);
check("命令里不出现反引号", !cmd.includes("`"), cmd);
check("没有第二个双引号进入值（引号数仍为偶数）", (cmd.match(/"/g) || []).length % 2 === 0, cmd);
check("--url 保留百分号编码", cmd.includes("--url=\"https://cdn.example.com/%E4%B8%AD/v.m3u8\""), cmd);
check("--referer 保留查询串", cmd.includes("--referer=\"https://page.example.com/watch?t=1&s=2\""), cmd);
check("exe 路径保留反斜杠", cmd.includes('"C:\\Program Files\\go-catcher.exe"'), cmd);
// `$` 剔掉后剩下的 `(calc)` 原样保留 —— 这不是漏网：圆括号在**双引号内**是字面量
// （三家 shell 皆然），不会构成语法。断言钉住"只剔了那些真危险的"。
check("标题的注入字符已消失、合法字符留下", cmd.includes('-o "a(calc)bcde"'), cmd);
check("仍是 PowerShell 5.1 认的 ; 分隔", cmd.startsWith("chcp 65001 ; "), cmd);

// ------------------------------------------------------------
// 4) 结构性守卫：-o 必须过 quoteArg（防止有人改回裸拼）
// ------------------------------------------------------------
console.log("结构性守卫：-o 的插值必须过 quoteArg");
const fn = (raw.match(/export function assembleGoCommand[\s\S]*?\n\}/) || [""])[0];
check("assembleGoCommand 里 -o 用 quoteArg(outName)", /-o "\$\{quoteArg\(outName\)\}"/.test(fn), fn);
check("assembleGoCommand 里每个 -- 参数都用 quoteArg", (fn.match(/quoteArg\(/g) || []).length >= 4, (fn.match(/quoteArg\(/g) || []).length);

// ------------------------------------------------------------
// 5) content.js 的接线：解析失败时兜底命令仍要带 --url（A-4）
// ------------------------------------------------------------
// 缺陷形态：候选明明拿到了（resp.sources 非空），只是展开画质失败，但错误分支
// 只传了一句字符串 ⇒ showPanel 的 error 分支拿不到 m3u8Url ⇒ 复制的命令缺 --url，
// 必跑不通。而这条命令是"本地服务没起来时用户唯一的手段"。
console.log("接线：解析失败分支必须把候选 URL 交给错误面板");
const contentSrc = fs.readFileSync(path.join(DIR, "content.js"), "utf8");
const noOpts = (contentSrc.match(/if \(!options\.length\) \{[\s\S]*?\n      \}/) || [""])[0];
check("找到解析失败分支（解析失效时不能退化成恒绿）", noOpts.length > 0);
check("payload 带 m3u8Url（缺它则兜底命令没有 --url）",
  /m3u8Url:\s*first\.url/.test(noOpts), noOpts);
check("不再只传一句字符串（那种写法拿不到 URL）",
  !/showPanel\("error",\s*"[^"]*"\s*\)/.test(noOpts), noOpts);

if (fail) {
  console.log(`\n${fail} 项失败，${pass} 项通过`);
  process.exit(1);
}
console.log(`\n全部通过（${pass} 项）`);
