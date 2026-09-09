# GoCatcher 构建脚本：Windows 资源（图标 / DPI 清单）动态生成 + GUI 子系统构建。
# rsrc_windows_*.syso 是构建产物（不入仓库），由 winres/ 源文件在构建前生成。

EXE       := go-catcher.exe
MAIN_PKG  := ./cmd/go-catcher
LDFLAGS   := -H=windowsgui -s -w

WINRES    := go run github.com/tc-hib/go-winres@latest
WINRES_IN := winres/winres.json
RSRC_OUT  := cmd/go-catcher/rsrc
ARCHES    := amd64,386,arm64

.DEFAULT_GOAL := build
.PHONY: build resources test cover clean

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

clean:
	powershell -NoProfile -Command "Remove-Item -Force -ErrorAction SilentlyContinue '$(EXE)','cover.out'"
