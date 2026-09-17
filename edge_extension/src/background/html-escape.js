// HTML 文本转义：把用户可控文本放进 innerHTML 之前必须过一遍。
//
// 为什么单独成模块（评审 P3-6）：浮层（content.js，经 content-shared.js 取用）与
// 下载器页面（downloader.js，直接 import）各有一份，靠注释提醒"保持一致"挡不住
// 漂移；漏一处就是一处 XSS/串版。两侧现在共用这一份。
//
// 覆盖五个在 HTML 里有语义的字符（含单引号：属性值可能用单引号包裹）。
export function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}
