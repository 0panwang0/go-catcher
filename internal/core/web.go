package core

import _ "embed"

// 监控页 HTML 独立放 web/index.html，编译期注入。

//go:embed web/index.html
var homePageHTML string
