# M3U8 Video Catcher (Edge 扩展)

把之前的 Go 版 m3u8 下载器做成了 Edge（Chromium）扩展。扩展的请求走浏览器自身的网络栈，TLS 指纹天然真实，**不再需要 uTLS 伪装**；代理直接跟随 Edge 的系统代理设置（Clash 开系统代理即可生效）。

**2026-09-05 更新**：项目合并为 **单可执行文件 `go-catcher.exe`**（一个 exe 三种模式：GUI 客户端 / `--server` 无头服务 / `--url=` 命令行直下），彻底告别"复制命令到终端"的繁琐步骤：
- 视频悬停按钮 → 弹出确认面板（展示链接供核验）
- 确认 → 浏览器原生 **Save As 对话框**让用户选保存位置
- 选好位置 → 自动调用本地下载服务下载到指定路径，全程无需终端

## 安装（免打包，开发者模式加载）

1. 打开 Edge，地址栏输入 `edge://extensions`
2. 打开左侧/底部的 **开发人员模式** 开关
3. 点击 **加载解压缩的扩展**，选择本目录（`edge_extension` 文件夹）
4. 工具栏会出现 M3U8 Video Catcher 图标

## 启动本地下载服务（一次性）

第一次使用前需要启动本地服务，让浏览器能"一键下载"：

1. 双击 `D:\projects\go_practice\workspace\go-catcher\go-catcher.exe`（GoCatcher 客户端）
2. 客户端打开后会**自动启动下载服务**（托盘常驻，关窗口只是缩到托盘，下载继续）
3. 之后所有视频下载都自动通过这个服务完成，无需再手动开终端

> **日常保持**：客户端可一直挂在托盘跑着，开机后再次双击即可。
> 想停止服务：托盘右键菜单"启动/停止服务"，或退出客户端。
> 只想要服务不要界面：终端里运行 `go-catcher.exe --server`（无头模式）。
> Clash 必须开着（系统代理模式），下载服务走的是 `http://127.0.0.1:7890`。

服务没启动也能用——扩展会检测到并提示"请先打开 go-catcher.exe"，并仍提供"复制命令到终端"兜底。

## 使用流程

1. 在浏览器里打开视频页并播放视频
2. **鼠标悬停到某个视频上**，其左上角会出现绿色 **"⬇ 下载该视频"** 按钮
3. 点击后弹出**确认面板**：
   - 显示视频名称、画质、资源链接（可复制核验）
   - 显示建议的保存文件名
   - 点 **确认下载** → 浏览器弹出原生 **Save As 对话框**
4. 在 Save As 对话框选好保存位置（如 `D:\Downloads\视频名_1080P.mp4`）→ 点保存
5. 浏览器自动从本地 Go 服务拉取视频数据，下载完成后 Chrome 右下角有提示
6. 下载进度实时显示在 GoCatcher 客户端的任务列表里（或浏览器打开 `http://127.0.0.1:7891/`）

> 若该视频是 master playlist（多档位），会先弹出该视频的画质选择面板，选完画质后再进入确认面板。

## 输出格式说明（MP4）

合并后的 HLS 流保存为 `.mp4`。这里分两种情况：

- **未安装 ffmpeg（默认）**：文件是 MPEG-TS 流套 `.mp4` 扩展名。
  PotPlayer / VLC 靠内容嗅探能正常播放；但剪辑软件、手机相册、Windows 自带播放器可能不识别。
- **已安装 ffmpeg**：Go 下载器会自动调用它做一次 **`-c copy` 无损重封装**（不重新编码，秒级完成），
  产出标准 MP4 容器，兼容性最好。封装失败会自动回退，不会中断交付。

Go 下载器按以下顺序查找 ffmpeg：`--ffmpeg` 参数指定 → PATH → 常见安装位置
（WinGet Links / scoop / chocolatey / `C:\ffmpeg\bin` / `D:\ffmpeg\bin`）。

相关参数：
- `-o <文件名>`：输出路径，默认 `output.mp4`；不带扩展名时自动补 `.mp4`；显式写 `.ts` 则输出 `.ts`
- `--no-remux`：跳过 ffmpeg 封装，TS 流直接写入输出文件
- `--ffmpeg=<路径>`：手动指定 ffmpeg
- `-limit N`：只下载前 N 个分片（试片/调试用）
- `--server`：以无头 HTTP 服务模式运行（不带界面，仅服务；GUI 客户端打开时无需此参数）

> 重封装期间需要临时双份磁盘空间（3GB 视频会短暂占用约 6GB）。

## 兜底：手动下载（命令）

如果不想启动服务、或服务异常，仍可用旧流程——错误面板会显示完整命令：

```cmd
chcp 65001 ; "D:\projects\go_practice\workspace\go-catcher\go-catcher.exe" --url="..." --referer="..." -o "视频名_1080P.mp4"
```

粘贴到 PowerShell / Git Bash / cmd 即可（用 `;` 分隔，三终端通用）。
这是单 exe 的 CLI 直下模式，不依赖客户端/服务是否在跑。

> PowerShell 5.1 不支持 `&&` 作语句分隔符。
> exe 路径必须加双引号，否则 Git Bash 会把参数按空白切片。

Go 二进制（`go-catcher.exe`）已经预设好：
- uTLS 伪造 Chrome TLS 指纹（解决 JA3 检测）
- 走 Clash 代理 127.0.0.1:7890
- 10 并发 + 3 次重试 + 自动合并为 `.mp4`
- 已端到端验证可下载 2.7GB 1080p 视频（约 1 分钟）

## 代理说明（重要）

扩展的网络请求走 **Edge 浏览器自己的代理设置**，无法在代码里强制指定 `127.0.0.1:7890`。
因此 Clash 需满足其一：

- 开启 **系统代理** 模式（Edge 默认跟随系统代理）
- 或开启 **TUN/虚拟网卡** 模式（接管全部流量）
- 或在 Edge 设置里手动配置代理服务器指向 Clash 端口

如果 Clash 只开了混合端口但没设系统代理，扩展请求不会走 Clash，会 403 或超时。

## 403 说明

旧版本在扩展页（`chrome-extension://...`）直接 fetch，Origin 头会被 CDN 识别为非页面请求而 403。

当前版本已改为：嗅探/识别在后台完成，**实际下载请求通过 MAIN world content script 在页面主世界发出**，与视频正常播放时的请求完全一致，403 问题已从根本上解决。若仍遇到 403，请检查 Clash 是否已开启系统代理/TUN 模式。

## 与 Go 版的对应关系

| Go 版 | 扩展版 |
|---|---|
| uTLS 伪造 Chrome TLS 指纹 | 浏览器原生指纹，无需伪造 |
| Clash 代理 `127.0.0.1:7890` | 跟随 Edge 系统代理 |
| `referer` / `userAgent` 伪造 | `declarativeNetRequest` 按来源页注入 Referer；UA 即浏览器本身 |
| 10 worker 并发 + 3 次重试 | 8 并发 + 3 次重试 |
| master playlist 选最高码率 | 同样支持 |
| 合并为 output.mp4 | 合并后浏览器保存为 `.mp4`，文件名自动从 URL/标题生成 |

## 文件结构

```
edge_extension/
├── manifest.json      # MV3 清单
├── background.js      # service worker：嗅探 m3u8/mp4 + 视频源识别
├── content.js         # 页面内 IDM 式悬停下载按钮 + 完整下载/合并逻辑
├── content-main.js    # MAIN world fetch 代理（请求头与页面一致，绕过 403）
├── downloader.html    # 下载器页面（备用/手动下载）
└── downloader.js      # 下载器页面逻辑
```
