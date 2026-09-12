// 扩展打包脚本：把 src/background/ 下的 ESM 模块打包成单文件 background.js
// （MV3 经典 service worker 不支持 ESM，必须打成 IIFE）。
//
// 用法：npm run build（即 node build.mjs）
// 产物 background.js 提交进仓库：用户"下载即可加载"的体验不变，CI 会校验
// 产物与源码同步（build 后 git diff --exit-code -- background.js）。
import { build } from "esbuild";
import { statSync, readFileSync, writeFileSync } from "node:fs";

// 版本单一来源（评审 P2-2）：package.json → manifest.json。
// 只改 package.json 忘了同步时，build 会改写 manifest.json，CI 的
// git diff 校验随之挂掉——版本漂移在提交前就会被拦下。
//
// readJSON 剥掉 UTF-8 BOM：这两个文件常被人手编辑（记事本 / PowerShell
// 管道都可能写出 BOM），JSON.parse 对 BOM 只会抛一句难懂的
// "Unexpected token" ——在这里消化掉，别让报错离根因十万八千里。
function readJSON(p) {
  return JSON.parse(readFileSync(p, "utf8").replace(/^\uFEFF/, ""));
}

function syncManifestVersion() {
  const pkg = readJSON("package.json");
  const manifest = readJSON("manifest.json");
  if (manifest.version === pkg.version) return;
  manifest.version = pkg.version;
  writeFileSync("manifest.json", JSON.stringify(manifest, null, 2) + "\n");
  console.log(`版本同步：manifest.json → ${pkg.version}（单一来源 package.json）`);
}

syncManifestVersion();

const outfile = "background.js";
const before = safeMtime(outfile);

await build({
  entryPoints: ["src/background/main.js"],
  outfile,
  bundle: true,
  format: "iife",
  globalName: "__m3u8catcher",
  target: ["es2020"],
  platform: "browser",
  // MV3 禁止远程代码：不设 external，全部内联；不允许任何网络求值
  minify: false,
  sourcemap: false,
  legalComments: "none",
  logLevel: "info",
});

const after = safeMtime(outfile);
console.log(
  before === after
    ? `警告：${outfile} 未被更新？`
    : `构建完成：${outfile}（${statSync(outfile).size} 字节）`
);

function safeMtime(p) {
  try {
    return statSync(p).mtimeMs;
  } catch {
    return -1;
  }
}
