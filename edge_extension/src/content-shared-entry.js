// content script 侧共享实现的构建入口。
//
// content.js 是经典 content script，不能 import ESM，历史上因此自留了一份解析
// 拷贝，与 src/background/m3u8-parse.js 漂移出 5 处差异（评审 P2-7 / F11）。
// build.mjs 用本文件把「唯一实现」打成 content-shared.js，挂到 globalThis 上，
// 由 manifest 的 content_scripts 在 content.js **之前**注入同一 isolated world。
//
// 这里只做汇总，不要写业务逻辑——加功能请加在被 import 的模块里。
//
// ⚠ 各模块的导出名必须互不重复：下面用对象展开汇总，重名会被后展开的**静默覆盖**，
//   症状是"某一侧的函数行为变了"而没有任何报错（评审 P3-6 把 escapeHtml 收进来时
//   就踩在这个口子上）。tests/contentparse.test.js 有一条断言守着这件事。
import * as parse from "./background/m3u8-parse.js";
import * as cli from "./background/cli-args.js";
import * as mediaUrl from "./background/media-url.js";
import * as htmlText from "./background/html-escape.js";

globalThis.__m3u8Shared = { ...parse, ...cli, ...mediaUrl, ...htmlText };
