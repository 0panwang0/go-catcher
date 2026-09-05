// GoCatcher 单文件入口。
//
// 一个 exe 三种模式，按启动参数分发：
//   - 无参数        → GUI 客户端（internal/app 外壳 + 进程内 core.Engine 服务）
//   - --server      → 无头服务模式（只有 core.Engine，供 bat/终端手动起服务）
//   - --url=...     → CLI 直下模式（单任务下载，输出挂回父终端）
//
// 架构：internal/core 是纯下载引擎（HTTP API + 任务管线，不依赖任何 GUI），
// internal/app 是 GUI 外壳（WebView2 + 托盘），二者只通过 Engine 的
// Start/Stop/Running 交互——未来加 CLI/TUI/其它前端时 core 原样复用。
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/0panwang0/go-catcher/internal/app"
	"github.com/0panwang0/go-catcher/internal/core"
)

func main() {
	// 带 参数启动的都不是 GUI 路径：先把标准输出挂回父终端（flag 报错、CLI 进度都要可见）。
	if len(os.Args) > 1 {
		core.AttachParentConsole()
	}
	opts := core.ParseCLI(os.Args[1:])

	switch {
	case opts.ServerMode:
		runHeadless(opts.Port)
	case len(os.Args) == 1:
		app.Run()
	case opts.URL == "":
		core.PrintUsage()
		os.Exit(1)
	default:
		os.Exit(core.RunCLI(opts))
	}
}

// runHeadless 无头服务模式：起引擎后阻塞，直到 /svc/stop 或 Ctrl+C。
func runHeadless(port int) {
	eng := core.NewEngine(port)
	if err := eng.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-eng.Done():
	case <-sig:
		eng.Stop()
	}
}
