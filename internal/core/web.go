package core

import _ "embed"

// 监控页 HTML 独立放 web/index.html，编译期注入。
// 设置页独立放 web/settings.html（点击监控页 ⚙ 进入，同一窗口内跳转）。

//go:embed web/index.html
var homePageHTML string

//go:embed web/settings.html
var settingsPageHTML string
