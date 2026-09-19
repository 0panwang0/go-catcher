#!/usr/bin/env node
// 断言纪律静态检查 —— 恒真断言扫描（2026-09-18 立，来自 self-review 的教训）。
//
// 缺陷形态：**断言通过的那一刻不产生任何信息**。典型是两侧同一个表达式的比较
//   check("...", shared.f(u) === shared.f(u));   // 永远为真
// 写的人想的是"守住 f 的参数序"，手写出来的是个恒真式。它绿着，于是没人再看
// 第二眼；而真正的缺陷（参数序漂移）恰好从它旁边走过去。
//
// 静态检查能做的不多，但这一条正好能做：**两侧文本完全相同**是可判定的。
// 扫不出来的是"看起来不同、实际等价"的断言（那要靠反向验证：扰动→必红→还原）。
// 两者是互补的，本工具只负责前一半，且**不自称完整**。
//
// 三条规则：
//   FAIL  恒真比较 —— 一次比较的两侧表达式逐字相同（`==` / `==` / `!=` / `!==`）
//   FAIL  恒真条件 —— `check("名", true)` / `if true {` 这类断言条件本身就是常量
//   WARN  跳过的测试 —— `t.Skip` / `test.skip`：评审红线要求"跳过不许伪装成通过"，
//         必须能一眼看到自己跳了什么（仍可跳过，但要有理由）
//
// 确有必要的写法（例如故意测自反性的 `v != v`）在有问题的**同一行或上一行**
// 加 `assert-lint-ok: 理由`，规则即让路 —— 理由随豁免留在代码里。
//
// 本工具在跑之前会先跑内置夹具自检（必报 / 不报 各若干例）。**自检不过直接退出**：
// 一个没区分力的检查器比没有检查器更坏（它给的是假绿）。
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const SKIP_DIRS = new Set([".git", "node_modules", "cover", "winres", "docs"]);
const MAX_OPERAND = 400;

// ============================================================
// 1) 把字符串 / 注释 / 正则字面量"挖空"成等长空白
// ------------------------------------------------------------
// 等长（且保留换行）是刻意的：掩码串上的下标可以直接回到原串取原文，
// 报错时能贴出真实代码，而判据（哪里是运算符）又不受字面量干扰。
// ============================================================
// keepStrings=true 只挖注释（字符串保留）—— 判"断言条件是不是常量"时必须看得见
// 字符串，否则 `check("名", true)` 里那个被挖空的名字会让模式匹配不上（自检抓到过）。
function maskLiterals(code, isJS, keepStrings = false) {
  const out = code.split("");
  const blank = (from, to) => {
    for (let k = from; k < to && k < out.length; k++) if (out[k] !== "\n") out[k] = " ";
  };
  const prevSig = (k) => {
    for (let j = k - 1; j >= 0; j--) {
      if (out[j] === " " || out[j] === "\n" || out[j] === "\t") continue;
      return out[j];
    }
    return "\n";
  };
  let i = 0;
  while (i < code.length) {
    const c = code[i];
    if (c === "/" && code[i + 1] === "/") {
      const nl = code.indexOf("\n", i);
      const end = nl < 0 ? code.length : nl;
      blank(i, end);
      i = end;
      continue;
    }
    if (c === "/" && code[i + 1] === "*") {
      const close = code.indexOf("*/", i + 2);
      const end = close < 0 ? code.length : close + 2;
      blank(i, end);
      i = end;
      continue;
    }
    if (c === '"' || c === "'" || c === "`") {
      if (keepStrings) {
        // 只标记"这里有个字面量"，内容照留；跳过它避免把里面的引号当代码
        let j = i + 1;
        while (j < code.length) {
          if (code[j] === "\\") {
            j += 2;
            continue;
          }
          if (code[j] === "\n" && c !== "`") break;
          if (code[j] === c) break;
          j++;
        }
        i = Math.min(j + 1, code.length);
        continue;
      }
      let j = i + 1;
      let closed = false;
      while (j < code.length) {
        if (code[j] === "\\") {
          j += 2;
          continue;
        }
        if (code[j] === "\n" && c !== "`") break; // 单/双引号不跨行：未闭合就别吞了
        if (code[j] === c) {
          closed = true;
          break;
        }
        j++;
      }
      const end = closed ? j + 1 : i + 1; // 未闭合只吞一个字符，绝不把整份源码吃掉
      blank(i, end);
      i = end;
      continue;
    }
    if (isJS && c === "/" && /[\n([,;:!&|?=+\-*%{}$>~^\\]/.test(prevSig(i))) {
      // 正则字面量：只在"这里应该是表达式起点"时才当正则，`a / b` 的除号不动它。
      let j = i + 1;
      let closed = false;
      let inClass = false;
      while (j < code.length) {
        const d = code[j];
        if (d === "\n") break;
        if (d === "\\") {
          j += 2;
          continue;
        }
        if (d === "[") inClass = true;
        else if (d === "]") inClass = false;
        else if (d === "/" && !inClass) {
          closed = true;
          break;
        }
        j++;
      }
      if (closed) {
        blank(i, j + 1);
        i = j + 1;
        continue;
      }
    }
    i++;
  }
  return out.join("");
}

// ============================================================
// 2) 从比较运算符两侧取出操作数
// ------------------------------------------------------------
// 判据走掩码串（字面量已被挖空 ⇒ 括号/逗号的计数不会被字符串里的符号带偏），
// 文本取原串（证据可读）。
// ============================================================
const LEFT_STOP = new Set([",", ";", "?", "=", "<", ">", "!", "&", "|", "+", "-", "*", "/", "%", "^", "~", ":"]);
const RIGHT_STOP = LEFT_STOP;
const KEYWORD_LEAD = /^\s*(?:if|else|return|for|case|switch|while|var|let|const|go|defer)\b\s*/;

function expandLeft(code, masked, end) {
  let depth = 0;
  let j = end - 1;
  for (; j >= 0; j--) {
    const c = masked[j];
    if (c === ")" || c === "]" || c === "}") {
      depth++;
      continue;
    }
    if (c === "(" || c === "[" || c === "{") {
      if (depth === 0) break;
      depth--;
      continue;
    }
    if (depth === 0 && LEFT_STOP.has(c)) break;
    if (end - j > MAX_OPERAND) break;
  }
  let text = code.slice(j + 1, end);
  for (let k = 0; k < 8; k++) {
    const next = text.replace(KEYWORD_LEAD, "");
    if (next === text) break;
    text = next;
  }
  return text;
}

function expandRight(code, masked, start) {
  let depth = 0;
  let j = start;
  for (; j < masked.length; j++) {
    const c = masked[j];
    if (c === "(" || c === "[") {
      depth++;
      continue;
    }
    if (c === ")" || c === "]") {
      if (depth === 0) break;
      depth--;
      continue;
    }
    if (depth === 0 && (RIGHT_STOP.has(c) || c === "{" || c === "}")) break;
    if (j - start > MAX_OPERAND) break;
  }
  return code.slice(start, j);
}

// 去外层括号 + 抹掉所有空白后比较：`f(a)` 与 `f( a )` 是同一个表达式。
function stripOuterParens(s) {
  let t = s.trim();
  for (;;) {
    if (t[0] !== "(" || t[t.length - 1] !== ")") return t;
    let depth = 0;
    let balancedEarly = false;
    for (let i = 0; i < t.length; i++) {
      if (t[i] === "(") depth++;
      else if (t[i] === ")") {
        depth--;
        if (depth === 0 && i !== t.length - 1) {
          balancedEarly = true;
          break;
        }
      }
    }
    if (balancedEarly) return t;
    t = t.slice(1, -1).trim();
  }
}

const norm = (s) => stripOuterParens(s).replace(/\s+/g, "");
const CMP_RE = /===|!==|==|!=/g;

function findTautologies(code, masked) {
  const out = [];
  CMP_RE.lastIndex = 0;
  let m;
  while ((m = CMP_RE.exec(masked)) !== null) {
    const op = m[0];
    const left = expandLeft(code, masked, m.index);
    const right = expandRight(code, masked, m.index + op.length);
    const nl = norm(left);
    const nr = norm(right);
    if (!nl || nl !== nr) continue;
    out.push({ kind: "恒真比较", index: m.index, evidence: `${left.trim()} ${op} ${right.trim()}` });
  }
  return out;
}

// 条件本身就是常量的断言。字符串名里可能有逗号，所以先匹配引号字面量再要求 `, true`。
const TRUE_COND_PATTERNS = [
  /check\(\s*(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`(?:[^`\\]|\\.)*`)\s*,\s*true\s*[,)]/g,
  /\.(?:ok|equal)\(\s*true\s*[,)]/g,
  /assert\.True\(\s*t\s*,\s*true\s*\)/g,
];
const GO_TRUE_COND = /^[ \t]*if\s+true\s*\{/gm;
const SKIP_PATTERNS = [/\bt\.Skip(?:f|Now)?\(/g, /\b(?:test|it|describe)\.skip\(/g];

function lineStarts(code) {
  const starts = [0];
  for (let i = 0; i < code.length; i++) if (code[i] === "\n") starts.push(i + 1);
  return starts;
}

function lineOf(starts, idx) {
  let lo = 0;
  let hi = starts.length - 1;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (starts[mid] <= idx) lo = mid;
    else hi = mid - 1;
  }
  return lo + 1;
}

// 豁免：命中问题的同一行或上一行写了 `assert-lint-ok: 理由` 就放行。
function suppressed(code, starts, line) {
  const lines = code.split("\n");
  const at = (n) => (lines[n - 1] || "").includes("assert-lint-ok");
  if (at(line) || at(line + 1)) return true;
  // 多行表达式：往上最多找三行，够覆盖"断言跨行书写"的形态
  for (let n = line - 2; n >= Math.max(1, line - 3); n--) if (at(n)) return true;
  return false;
}

function lintSource(code, isJS) {
  const masked = maskLiterals(code, isJS);
  const noComments = maskLiterals(code, isJS, true);
  const starts = lineStarts(code);
  const fails = [];
  const warns = [];
  for (const t of findTautologies(code, masked)) {
    const line = lineOf(starts, t.index);
    if (suppressed(code, starts, line)) continue;
    fails.push({ kind: t.kind, line, evidence: t.evidence });
  }
  const condPats = isJS ? TRUE_COND_PATTERNS : [];
  const all = isJS ? condPats.map((re) => [re, noComments]) : [[GO_TRUE_COND, noComments]];
  for (const [re, hay] of all) {
    re.lastIndex = 0;
    let m;
    while ((m = re.exec(hay)) !== null) {
      const line = lineOf(starts, m.index);
      if (suppressed(code, starts, line)) continue;
      fails.push({ kind: "恒真条件", line, evidence: code.slice(m.index, m.index + m[0].length).trim() });
    }
  }
  for (const re of SKIP_PATTERNS) {
    re.lastIndex = 0;
    let m;
    while ((m = re.exec(masked)) !== null) {
      // 掩码过的串里带引号的内容已变空白，跳过语句本身不含字面量，掩码不影响判定
      const line = lineOf(starts, m.index);
      warns.push({ kind: "跳过的测试", line, evidence: code.split("\n")[line - 1].trim() });
    }
  }
  return { fails, warns };
}

// ============================================================
// 3) 自检夹具：先证明自己有区分力，再去检查别人
// ============================================================
const FIXTURES = [
  // —— 必报 ——
  { lang: "js", want: 1, code: `check("t", f("u") === f("u"));` },
  {
    lang: "js",
    want: 1,
    code: `check("参数序一致",\n  shared.variantLabel(u, "", 0) === shared.variantLabel(u, "", 0) &&\n  shared.variantQuality(u, "", 0) === "1080P");`,
  },
  { lang: "js", want: 1, code: `if (a.b !== a.b) { boom(); }` },
  { lang: "js", want: 1, code: `const same = obj["k"] == obj["k"];` },
  { lang: "js", want: 1, code: `check("t", true);` },
  { lang: "js", want: 1, code: `check("t", 1 === 1);` },
  { lang: "go", want: 1, code: `if got != got {\n\tt.Fatal("impossible")\n}` },
  { lang: "go", want: 1, code: `if true {\n\tt.Fatal("x")\n}` },
  { lang: "go", want: 1, code: `if !(x == x) {\n\tt.Fatal("x")\n}` },
  // —— 不许报 ——
  { lang: "js", want: 0, code: `check("t", f("u") === f("v"));` },
  { lang: "js", want: 0, code: `check("t", a === b && c === d);` },
  { lang: "js", want: 0, code: `check("t", /a==b/.test(s));` },
  { lang: "js", want: 0, code: `// check("t", f("u") === f("u"));` },
  { lang: "js", want: 0, code: `/* check("t", f("u") === f("u")); */` },
  { lang: "js", want: 0, code: `check("分隔符是 ===", name === "a===b");` },
  { lang: "js", want: 0, code: `const isSame = (a, b) => a === b;` },
  { lang: "js", want: 0, code: `check("t", f("u") === f("u")); // assert-lint-ok: 测自反性` },
  { lang: "js", want: 0, code: `check("t", shared.parseSegments(text, base)[0] === "https://cdn.x/a/seg0.ts");` },
  { lang: "go", want: 0, code: `if got != want {\n\tt.Fatalf("got %v", got)\n}` },
  { lang: "go", want: 0, code: `if tc.want != tc.got {` },
  { lang: "go", want: 0, code: `// if x == x {\n// \tt.Fatal("x")\n// }` },
  { lang: "go", want: 0, code: `if x != x { // assert-lint-ok: 故意测 NaN 自反性\n}` },
  { lang: "go", want: 0, code: "s := `if x == x {`" },
];

function selfTest() {
  let bad = 0;
  for (const fx of FIXTURES) {
    const { fails } = lintSource(fx.code, fx.lang === "js");
    if (fails.length !== fx.want) {
      bad++;
      console.error(
        `  自检失败（期望 ${fx.want} 处、实得 ${fails.length} 处）：${JSON.stringify(fx.code)}`
      );
      for (const f of fails) console.error(`      -> ${f.kind} ${JSON.stringify(f.evidence)}`);
    }
  }
  if (bad) {
    console.error(`assert-lint 自检未通过（${bad}/${FIXTURES.length} 例）——检查器本身不可信，停止。`);
    process.exit(2);
  }
  const must = FIXTURES.filter((f) => f.want > 0).length;
  console.log(`assert-lint 自检 OK（${FIXTURES.length} 例：必报 ${must} / 不报 ${FIXTURES.length - must}）`);
}

// ============================================================
// 4) 遍历仓库
// ============================================================
function collect(dir, acc) {
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    if (SKIP_DIRS.has(e.name)) continue;
    const p = path.join(dir, e.name);
    if (e.isDirectory()) collect(p, acc);
    else if (/_test\.go$/.test(e.name) || /\.test\.(?:js|mjs)$/.test(e.name)) acc.push(p);
  }
  return acc;
}

function main() {
  selfTest();
  const targets = process.argv.slice(2);
  const files = targets.length ? targets : collect(ROOT, []);
  let failCount = 0;
  let warnCount = 0;
  for (const abs of files) {
    const rel = path.relative(ROOT, abs).split(path.sep).join("/");
    const code = readFileSync(abs, "utf8");
    const { fails, warns } = lintSource(code, /\.(?:js|mjs)$/.test(abs));
    for (const f of fails) {
      failCount++;
      console.error(`${rel}:${f.line}  FAIL  ${f.kind}：${f.evidence}`);
    }
    for (const w of warns) {
      warnCount++;
      console.log(`${rel}:${w.line}  warn  ${w.kind}：${w.evidence}`);
    }
  }
  console.log(`assert-lint 扫了 ${files.length} 个测试文件：FAIL ${failCount} / warn ${warnCount}`);
  if (failCount) {
    console.error("恒真断言等于没有断言：它绿着，却不产生任何信息。");
    console.error("改成有区分力的断言（两侧不同），或加 `assert-lint-ok: 理由` 说明为何必须自比。");
    process.exit(1);
  }
}

main();
