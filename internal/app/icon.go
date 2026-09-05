// 托盘图标（深底白色下载箭头，ICO 格式）—— 通过 go:embed 打进 exe。
// systray SetIcon 在 Windows 上调 LoadImageW(IMAGE_ICON, file)，所以必须传 .ico 格式。
// 现代 .ico 可以内嵌 PNG payload（Vista+）。
package app

import _ "embed"

//go:embed icon.ico
var iconPNG []byte
