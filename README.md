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

## 技术架构

单进程架构：下载引擎与 GUI 外壳合并在一个可执行文件内，进程内通过生命周期 API 协同。

```
go-catcher.exe
├── internal/core   # 下载引擎：HTTP 服务（127.0.0.1:7891）、任务调度、m3u8 解析、持久化
│   └── web/        # 内嵌监控页（go:embed）
└── internal/app    # GUI 外壳：WebView2 窗口（内嵌监控页）、系统托盘、单实例互斥
```

- **引擎**（`internal/core`）：核心下载逻辑，独立于 UI，可被 GUI、无头服务、CLI 三种模式复用
- **外壳**（`internal/app`）：基于 WebView2（系统自带 Edge 运行时，不打包浏览器）+ 系统托盘，与引擎通过 `Start/Stop/Running` 交互
- **监控页**：同一份 Web UI 供客户端内嵌、浏览器直访、无头模式共用

## 数据与隐私

- 所有数据仅存本地：`gocatcher_config.json`（运行配置）、`gocatcher_state.json`（任务历史），位于 exe 同目录
- 下载服务只绑定 `127.0.0.1`，外部无法访问

## 从源码构建

需要 Go 1.24+（Makefile 依赖 GNU Make，其余构建工具由 `go run` 按需拉取）。

```powershell
make build
```

> 等价于：先生成 Windows 资源，再 `go build -ldflags "-H=windowsgui -s -w" -o go-catcher.exe ./cmd/go-catcher`。`-H=windowsgui` 用于 GUI 模式不弹出终端窗口。

其他目标：`make test`（全量测试，含竞态检测）、`make cover`（覆盖率报告）、`make clean`。

### Windows 资源（图标 / 清单）

`cmd/go-catcher/` 下的 `rsrc_windows_*.syso` 是构建产物（`.gitignore` 已排除，不入仓库），`make build` 每次从 `winres/` 源文件重新生成，`go build` 自动链接主包目录的 `.syso`。手工生成：

```powershell
go run github.com/tc-hib/go-winres@latest make --arch amd64,386,arm64 --in winres/winres.json --out cmd/go-catcher/rsrc
```

- 图标源：`winres/icon.png`（托盘用 `internal/app/icon.ico`，两者同源，改动时保持同步）
- 清单：`winres/app.manifest`（Per-Monitor V2 DPI 感知）
- 配置：`winres/winres.json`

> 图标资源 ID 必须是 `#1`：go-webview2 的 `WindowOptions.IconId` 按 exe 资源 ID 加载窗口类图标（`internal/app/app.go`），改 ID 需同步改 `IconId`，否则任务栏回落为通用图标。
