# GoCatcher 构建脚本：Windows 资源（图标 / DPI 清单）动态生成 + GUI 子系统构建。
# rsrc_windows_*.syso 是构建产物（不入仓库），由 winres/ 源文件在构建前生成。
#
# 环境提示：请在 MSYS2 MinGW64 终端里跑 make。从 Git Bash 等非 MSYS2 环境启动时，
# MSYS2 的 make 会重建自己的环境并丢掉 TMP/TEMP/USERPROFILE/GOPATH/GOMODCACHE，
# go 于是报 "module cache not found" 或 "mkdir C:\WINDOWS\go-build: Access is denied"
# ——这不是 Makefile 的问题，直接在那种环境用 go 命令，或把变量显式传进来即可：
#   make check TMP="$TMP" TEMP="$TEMP" USERPROFILE="$USERPROFILE" GOPATH="$HOME/go"
# 实测本机（从 Git Bash 调 msys2 的 make）跑 build/all 时，除上面四个外还必须补
# LOCALAPPDATA 与 GOCACHE，否则 go 直接报：
#   build cache is required, but could not be located:
#   GOCACHE is not defined and %LocalAppData% is not defined
# 完整的可用命令（GOPROXY 走国内镜像，首次会拉 go1.24 工具链 ~100MB）：
#   make all TMP="$TMP" TEMP="$TEMP" USERPROFILE="$USERPROFILE" \
#     LOCALAPPDATA="$LOCALAPPDATA" GOCACHE="$LOCALAPPDATA/go-build" \
#     GOPATH="$HOME/go" GOPROXY=https://goproxy.cn,direct
# 例外：ext / ext-check 只用到 Node 与 git、不碰 go，任意 shell 下都能正常跑
# （打包与校验逻辑都在 Node 脚本里，Makefile 侧只有 sh/cmd 通用的 && 链）。

EXE       := go-catcher.exe
MAIN_PKG  := ./cmd/go-catcher
LDFLAGS   := -H=windowsgui -s -w

WINRES    := go run github.com/tc-hib/go-winres@latest
WINRES_IN := winres/winres.json
RSRC_OUT  := cmd/go-catcher/rsrc
ARCHES    := amd64,386,arm64

EXT_DIR   := edge_extension

.DEFAULT_GOAL := build
.PHONY: all build resources test cover check check-all clean ext ext-check

# 一次编译全部产物：服务器 exe（GUI / 无头服务 / CLI 三合一）+ Edge 扩展
all: build ext

# 构建 exe（先确保 Windows 资源存在，go build 自动链接主包目录的 .syso）
build: resources
	go build -ldflags "$(LDFLAGS)" -o $(EXE) $(MAIN_PKG)

# 从 winres/ 生成三个架构的资源对象；首次运行需联网拉取 go-winres 模块
resources:
	$(WINRES) make --in $(WINRES_IN) --out $(RSRC_OUT) --arch $(ARCHES)

# 全量测试（含竞态检测）
test:
	go test ./... -race -count=1

# 覆盖率报告（末行 total 汇总）
cover:
	go test ./... -coverprofile=cover.out -count=1
	go tool cover -func=cover.out

# 与 CI 对齐的本地质量门（Go job）：格式 → 静态检查 → 测试（含竞态）。
# 提交前跑这个，和 .github/workflows/ci.yml 的 go job 是同一套判定。
# 扩展侧的等价物是 ext-check；两边都跑用 check-all。
check:
	@unformatted="$$(gofmt -l ./cmd ./internal)"; \
	if [ -n "$$unformatted" ]; then \
		echo "以下文件未格式化，请执行 gofmt -w："; echo "$$unformatted"; exit 1; \
	fi; \
	echo "gofmt OK"
	go vet ./...
	go test ./... -race -count=1

# ============================================================
# Edge 扩展（与 CI 的 extension job 同一套判定）
# 源码是 src/background/ 下的 ES 模块，esbuild 打包成单文件 background.js；
# bundle 提交进仓库（用户"解压即加载"无需构建），所以改了 src/ 必须重新打包。
# 需要 Node 22+：downloader.js 是 ES 模块（import 共享解析源码），
# `node --check` 对 .js 的模块语法探测要 22.7+，Node 18 会误报语法错误。
# 首次会自动 npm install。
# ============================================================

# 打包：src/background/*.js → edge_extension/background.js
# 依赖自举在 build.mjs 里（node_modules 缺失才 npm install），这里不写任何
# sh 专属语法（`{ ... }` / `$$(...)`），cmd 与 sh 都能跑（&& 两种 shell 通用）。
ext:
	cd $(EXT_DIR) && node build.mjs

# 扩展侧完整校验：打包 → bundle 一致性 → 语法 → 回归测试。
# 校验逻辑在 scripts/check.mjs（Node 实现，跨 shell），一致性判据是
# 「打包前后 sha1 是否变化」而非 git diff：
#   本地改了 src/ 还没提交时 git diff 必然非空（那是正常的，不该报错）；
#   sha1 只在「改了 src/ 却忘了重新打包」时命中，两种场景都对。
#   CI 那边用的是 git diff --exit-code（对已提交的 blob 比对），fresh checkout 下等价。
# manifest.json 用同一判据：build.mjs 会把 package.json 版本注入 manifest，
# 改了版本没跑 build 时在这里被拦下（版本单一来源）。
ext-check:
	cd $(EXT_DIR) && node scripts/check.mjs

# 两边一起跑（等价于 CI 的两个 job）
check-all: check ext-check

clean:
	powershell -NoProfile -Command "Remove-Item -Force -ErrorAction SilentlyContinue '$(EXE)','cover.out'"
