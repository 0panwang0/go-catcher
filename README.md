# GoCatcher

通用网络资源下载器：m3u8/HLS 流媒体、直链文件，一键下载。单可执行文件，无需安装任何运行时。

![release](https://img.shields.io/github/v/release/0panwang0/go-catcher)

## 功能特性

- **单文件三模式**：GUI 客户端 / 无头服务 / 命令行直下，一个 exe 全搞定
- **m3u8/HLS 流媒体**：自动解析 master playlist、多码率选择、边下边写（无需合并），原始 TS 流直接落盘
- **HLS AES-128 解密**：自动识别 `#EXT-X-KEY` 加密流，拉取密钥并逐分片解密（显式 IV / 按分片序号派生 IV），落盘即明文可播
- **断点续传**：任务状态持久化到本地，服务重启后自动恢复未完成任务
- **并发与重试**：分片并发下载、失败自动重试、`--limit` 试片模式
- **代理支持**：默认跟随 Windows 系统代理（Clash 等工具开「系统代理」即自动生效），设置页可切换手动指定 / 直连，保存即生效；CLI 模式用 `--proxy` 指定
- **端口可配置**：监控页设置里可改服务端口（解决端口冲突），重启服务生效
- **Edge 浏览器扩展**：网页内一键发起下载，自动回填页面 URL 作为 Referer
- **本地优先**：服务只监听 `127.0.0.1`，监控页与 API 不对外暴露

## 快速开始

1. 从 [Releases](https://github.com/0panwang0/go-catcher/releases) 下载 `go-catcher.exe`
2. 双击运行 —— 打开即自动启动下载服务，窗口常驻系统托盘
3. 在浏览器（配合 Edge 扩展）或监控页 `http://127.0.0.1:7891/` 添加下载任务

## 使用模式

| 模式 | 启动方式 | 说明 |
|---|---|---|
| GUI 客户端 | 双击 `go-catcher.exe` | 打开即自动启动服务，托盘右键可停止/重启服务、退出 |
| 无头服务 | `go-catcher.exe --server [--port=端口]` | 只有下载服务，无界面；用浏览器访问监控页管理任务 |
| 命令行直下 | `go-catcher.exe --url=<地址> [选项]` | 单任务直接下载，不依赖服务，适合脚本调用 |

### CLI 直下示例

```powershell
go-catcher.exe --url="https://cdn.example.com/video/1080p/video.m3u8" --referer="https://example.com/watch/123" -o "视频名.ts"
```

## 命令行参数

| 参数 | 说明 |
|---|---|
| `--url` | 下载地址（m3u8 流地址或直链文件） |
| `--referer` | 来源页 URL，部分站点必填 |
| `--proxy` | 代理：`system` 跟随 Windows 系统代理（默认）、`http://127.0.0.1:7890` 手动指定、`direct`/`none` 直连 |
| `-c` | 分片并发数，默认 10 |
| `-o` | 输出文件名（默认 output.ts；HLS 原始流直接落盘，不做封装转换） |
| `--limit` | 只下载前 N 个分片（0 = 全部，用于试片） |
| `--server` | 无头服务模式 |
| `--port` | 服务监听端口（仅 `--server` 时有效；不指定则用监控页设置里配置的端口） |

## Edge 浏览器扩展

扩展位于 `edge_extension/` 目录，支持在网页内一键把视频流交给 GoCatcher 下载。

安装步骤：

1. 打开 Edge，访问 `edge://extensions`
2. 开启右上角 **开发者模式**
3. 点击 **加载解压缩的扩展**，选择本项目的 `edge_extension` 目录
4. 打开任意视频页面，点击页面上的 GoCatcher 按钮即可发起下载

> 首次使用需先启动 GoCatcher（双击 `go-catcher.exe`），扩展会自动检测服务状态并给出提示。

### 扩展开发

扩展源码在 `edge_extension/src/background/`（ES 模块），由 esbuild 打包为 `background.js`：
`background.js` 仍是单文件 IIFE 且**提交进仓库**，因此"加载解压缩的扩展"无需任何构建步骤；
改了 `src/` 之后需要重新构建再提交：

```
make ext          # 等价于 cd edge_extension && node build.mjs（首次自动 npm install）
```

或手工执行：

```
cd edge_extension
npm install
node build.mjs
```

CI 会校验 `background.js` 与 `src/` 是否一致（不一致即失败）；提交前跑 `make ext-check`
可以在本地先拦下"改了 `src/` 忘了重新打包"。

## 技术架构

单进程架构：下载引擎与 GUI 外壳合并在一个可执行文件内，进程内通过生命周期 API 协同。

```
go-catcher.exe
├── internal/core       # 下载引擎：HTTP 服务（127.0.0.1:7891）、任务调度、m3u8 解析、持久化
│   └── web/            # 内嵌监控页（go:embed）
├── internal/platform   # 平台层：Windows 控制台挂接、文件夹对话框、日志落盘、系统代理、Shell 操作
└── internal/app        # GUI 外壳：WebView2 窗口（内嵌监控页）、系统托盘、单实例互斥
```

- **引擎**（`internal/core`）：核心下载逻辑，独立于 UI，可被 GUI、无头服务、CLI 三种模式复用；
  全部可变运行状态收在 `Runtime` 结构（`runtime.go`）上，Engine / CLI 各持一份实例，
  可多实例化或作为库引用
- **平台层**（`internal/platform`）：Windows 特定能力的独立包（`//go:build windows` 隔离），
  core 与 app 只依赖其导出函数，不直接触碰系统 API——未来移植到其它系统时只需替换此包
- **外壳**（`internal/app`）：基于 WebView2（系统自带 Edge 运行时，不打包浏览器）+ 系统托盘，与引擎通过 `Start/Stop/Running` 交互
- **监控页**：同一份 Web UI 供客户端内嵌、浏览器直访、无头模式共用

## 数据与隐私

- 所有数据仅存本地：`gocatcher_config.json`（运行配置）、`gocatcher_state.json`（任务历史）、
  `gocatcher.log`（诊断日志，超过 2MB 自动轮转为 `gocatcher.log.1`），均位于 exe 同目录
- 下载服务只绑定 `127.0.0.1`，外部无法访问
- **访问令牌**：服务首次启动会生成一枚随机令牌存进 `gocatcher_config.json`，之后所有请求
  （`/health`、`/svc/info`、页面本身除外）都必须带上它——`?t=<token>` 或 `X-GoCatcher-Token` 头。
  原因：「只监听 127.0.0.1」并不等于安全，浏览器里任何网页都能向 `127.0.0.1` 发简单请求，
  而本服务具备「按给定路径写文件」和「执行程序」两种能力。令牌之外还有两道：
  Host 头必须是本机地址（挡 DNS rebinding），未通过令牌校验的响应不带 CORS 头（网页读不到响应体）。
  内嵌监控页由服务端把令牌注入 HTML，浏览器扩展则通过 `/svc/info` 握手自动获取，用户无需配置。
- **诊断日志**：GUI 模式编译为 `windowsgui` 子系统、没有控制台，所有诊断输出（重试、回退、
  落盘失败原因等）会带时间戳写入 `gocatcher.log`。查看方式：直接打开该文件，或请求
  受令牌保护的 `GET /log`（返回最后 200 行）。

## 已知限制

拿到一个流之前，先对照这张表可以少走弯路：

| 限制 | 说明 |
|---|---|
| 只支持 HLS(m3u8) 与直链 MP4 | 不支持 DASH(`.mpd`)、HLS over WebSocket |
| 加密只支持 AES-128 | SAMPLE-AES / Widevine 会明确报错（不支持的 METHOD） |
| 全程必须同一把密钥 | 播放列表中途换 key（key rotation）会明确报错而不是产出损坏文件 |
| 明文 `http://` 同样走代理 | 已修正：此前只有 https 走 CONNECT 隧道，明文请求会绕过代理直连 |
| 代理只支持 `http://` | `socks5://` 系统代理会在启动横幅与 `/config` 的 `systemProxyWarning` 里提示"已按直连处理" |
| 强制 HTTP/1.1 | 覆写 ALPN，牺牲部分吞吐换兼容性（应对仅支持 1.1 的 CDN） |
| 仅 Windows | 依赖 WebView2、注册表、ShellExecute（`_windows.go` 已按 build tag 隔离） |

## 从源码构建

需要 Go 1.24+（Makefile 依赖 GNU Make，其余构建工具由 `go run` 按需拉取）。

```powershell
make all     # exe + Edge 扩展一起编；只想要 exe 用 make build
```

> `make build` 等价于：先生成 Windows 资源，再 `go build -ldflags "-H=windowsgui -s -w" -o go-catcher.exe ./cmd/go-catcher`。`-H=windowsgui` 用于 GUI 模式不弹出终端窗口。
> `make all` = `make build` + `make ext`（扩展打包成 `edge_extension/background.js`）。

其他目标：

| 目标 | 作用 |
|---|---|
| `make all` | **一次编译全部产物**：exe（GUI / 无头服务 / CLI 三合一）+ Edge 扩展 |
| `make build` | 只编 exe |
| `make test` | 全量测试（含竞态检测） |
| `make check` | Go 侧质量门，与 CI 的 go job 同一套判定：gofmt / go vet / go test -race |
| `make ext` | 打包 Edge 扩展：`src/background/*.js` → `edge_extension/background.js` |
| `make ext-check` | 扩展侧质量门，与 CI 的 extension job 同一套判定：打包 → bundle 一致性 → 语法 → 回归测试 |
| `make check-all` | `check` + `ext-check`，等价于 CI 两个 job |
| `make cover` | 覆盖率报告 |
| `make clean` | 清理构建产物 |

> 注意：请在 MSYS2 MinGW64 终端里跑 `make`。从 Git Bash 等非 MSYS2 环境启动时，MSYS2 的 make
> 会重建自己的环境并丢掉 `TMP`/`TEMP`/`USERPROFILE`/`GOPATH`，go 会报 `module cache not found`、
> `mkdir C:\WINDOWS\go-build: Access is denied`，或 `GOCACHE is not defined and %LocalAppData% is not defined`；
> 此时直接跑 go 命令，或把变量显式传进来：
>
> ```powershell
> make all TMP="$TMP" TEMP="$TEMP" USERPROFILE="$USERPROFILE" `
>          LOCALAPPDATA="$LOCALAPPDATA" GOCACHE="$LOCALAPPDATA/go-build" `
>          GOPATH="$HOME/go" GOPROXY=https://goproxy.cn,direct
> ```
>
> （`GOPROXY` 走国内镜像；`ext`/`ext-check` 只用 Node 和 git、不碰 go，不受此影响。）

### Windows 资源（图标 / 清单）

`cmd/go-catcher/` 下的 `rsrc_windows_*.syso` 是构建产物（`.gitignore` 已排除，不入仓库），`make build` 每次从 `winres/` 源文件重新生成，`go build` 自动链接主包目录的 `.syso`。手工生成：

```powershell
go run github.com/tc-hib/go-winres@latest make --arch amd64,386,arm64 --in winres/winres.json --out cmd/go-catcher/rsrc
```

- 图标源：`winres/icon.png`（托盘用 `internal/app/icon.ico`，两者同源，改动时保持同步）
- 清单：`winres/app.manifest`（Per-Monitor V2 DPI 感知）
- 配置：`winres/winres.json`

> 图标资源 ID 必须是 `#1`：go-webview2 的 `WindowOptions.IconId` 按 exe 资源 ID 加载窗口类图标（`internal/app/app.go`），改 ID 需同步改 `IconId`，否则任务栏回落为通用图标。
