// content script 侧共享实现的构建入口。
//
// content.js 是经典 content script，不能 import ESM，历史上因此自留了一份解析
// 拷贝，与 src/background/m3u8-parse.js 漂移出 5 处差异（评审 P2-7 / F11）。
// build.mjs 用本文件把「唯一实现」打成 content-shared.js，挂到 globalThis 上，
// 由 manifest 的 content_scripts 在 content.js **之前**注入同一 isolated world。
//
// 这里只做汇总，不要写业务逻辑——加功能请加在被 import 的那两个模块里。
import * as parse from "./background/m3u8-parse.js";
import * as cli from "./background/cli-args.js";

globalThis.__m3u8Shared = { ...parse, ...cli };
