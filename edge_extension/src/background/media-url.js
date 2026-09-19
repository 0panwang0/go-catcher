// 媒体 URL 判定：集中放「这条 URL 属不属于可下载的媒体」这类纯判定。
//
// 为什么单独成模块（评审 P3-6）：同一套判定曾在 background（page-match.js）与
// 下载器页面（downloader.js）各写一份，靠注释互相提醒"保持一致"——注释拦不住漂移，
// 而漂移的症状是「浮层能下的链接，扩展页下不了」（或反过来）。现在两侧 import
// 同一份实现，改一处即两处生效。
//
// 纯函数、不碰任何扩展 API：content script 侧经 content-shared.js 取用（挂到
// globalThis.__m3u8Shared），扩展页侧直接 import。

// isCandidateURL 过滤历史上被误录的解析页 URL（路径无媒体扩展名、靠 ?url= 跳转
// 参数尾部伪装 .m3u8）：这类链接点下去拉回的是 HTML 网页，任务必然失败。
// 路径带媒体扩展名、或不含 ?url= 参数的记录才算候选（?url= 是解析页的通用签名）。
// 非法 URL 返回 false 而不抛：调用方是在过滤列表，一条坏数据不该毁掉整次渲染。
export function isCandidateURL(u) {
  try {
    const p = new URL(u);
    return /\.(m3u8|mp4)$/i.test(p.pathname) || !p.searchParams.has("url");
  } catch {
    return false;
  }
}

// isPlaylistURL 是否是 m3u8 播放列表地址。用它把「要不要先解析档位」和「直接下
// MP4 直链」分开：对 MP4 直链去 fetchText 会把整个视频当文本读进内存，纯粹是浪费。
export function isPlaylistURL(u) {
  try {
    return /\.m3u8$/i.test(new URL(u).pathname);
  } catch {
    return false;
  }
}
