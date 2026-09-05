// CLI 直下模式：参数解析与单任务下载流程（不依赖 HTTP 服务与任务引擎）。
package core

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"time"
)

// CLIOptions 一次 CLI 调用的全部参数（root main.go 解析后传给 RunCLI）。
type CLIOptions struct {
	URL         string
	Referer     string
	Proxy       string
	Output      string
	Concurrency int
	Limit       int
	ServerMode  bool
	Port        int
}

// 全局运行参数：下载引擎各模块直接读，RunCLI 赋值、/config 端点可调后三项。
var (
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0"
	proxyAddr   = "http://127.0.0.1:7890"
	concurrency = 10
	maxRetries  = 3
	outputFile  = "output.ts"
	limit       int
	bindAddr    = "127.0.0.1"
	referer     string
)

// ParseCLI 解析命令行（未识别参数由 flag 包报错退出）。
func ParseCLI(args []string) CLIOptions {
	o := CLIOptions{
		Proxy:       proxyAddr,
		Output:      outputFile,
		Concurrency: concurrency,
	}
	fs := flag.NewFlagSet("go-catcher", flag.ExitOnError)
	fs.StringVar(&o.URL, "url", o.URL, "m3u8 URL (必填)")
	fs.StringVar(&o.Referer, "referer", "", "Referer（来源页 URL，部分站点必填）")
	fs.StringVar(&o.Proxy, "proxy", o.Proxy, "Proxy address（空/direct/none = 直连）")
	fs.IntVar(&o.Concurrency, "c", o.Concurrency, "Concurrency")
	fs.StringVar(&o.Output, "o", o.Output, "Output file (默认 output.ts，HLS 原始流)")
	fs.IntVar(&o.Limit, "limit", 0, "只下载前 N 个分片（0 = 全部，用于试片）")
	fs.BoolVar(&o.ServerMode, "server", false, "无头 HTTP 服务模式（监听 127.0.0.1，供浏览器扩展调用）")
	fs.IntVar(&o.Port, "port", 0, "服务模式监听端口（仅 --server 时有效；不指定则用设置里配置的端口）")
	_ = fs.Parse(args)
	return o
}

// PrintUsage 打印 CLI 用法（无参数 GUI 启动的说明一并给出）。
func PrintUsage() {
	fmt.Println("用法: go-catcher.exe --url=<m3u8地址> [--referer=<来源页>] [--proxy=<代理>] [-c 并发数] [-o 输出文件]")
	fmt.Println("示例: go-catcher.exe --url=https://cdn.example.com/xxx/1080p/video.m3u8 --referer=https://example.com/watch/123 -o \"视频名.ts\"")
	fmt.Println("可选: --limit=N 只下前 N 片试片")
	fmt.Println("其它: --server [--port=端口] 无头服务模式 | 不带任何参数启动 GUI 客户端")
}

// RunCLI 执行单任务直下流程，返回退出码。
func RunCLI(o CLIOptions) int {
	referer = o.Referer
	proxyAddr = o.Proxy
	concurrency = o.Concurrency
	outputFile = o.Output
	limit = o.Limit

	outputFile = normalizeOutput()

	// CLI 单任务：直接流式写入目标文件旁的 .part，无需临时目录
	outAbs, _ := filepath.Abs(outputFile)
	job := &dlJob{
		m3u8URL: o.URL,
		referer: referer,
		saveDir: filepath.Dir(outAbs),
		fname:   filepath.Base(outAbs),
		limit:   limit,
	}
	partPath := outAbs + ".part"

	start := time.Now()
	fmt.Println("========================================")
	fmt.Println("  m3u8 视频下载器 (uTLS + Clash)")
	fmt.Println("  URL:     ", job.m3u8URL)
	fmt.Println("  代理:    ", proxyAddr)
	fmt.Println("  Referer: ", job.referer)
	fmt.Println("  输出:    ", outputFile)
	fmt.Println("  TLS指纹: Chrome Auto")
	fmt.Println("========================================")

	// 1. 获取 m3u8
	fmt.Println("\n[1/4] 获取 m3u8 索引...")
	m3u8Content, err := job.fetchM3U8()
	if err != nil {
		fmt.Printf("获取 m3u8 失败: %v\n", err)
		return 1
	}
	fmt.Println("m3u8 获取成功")

	// 2. 解析 ts 分片
	fmt.Println("\n[2/4] 解析分片列表...")
	segURLs, err := job.parseTSSegments(m3u8Content)
	if err != nil {
		fmt.Printf("解析失败: %v\n", err)
		return 1
	}
	if job.limit > 0 && job.limit < len(segURLs) {
		fmt.Printf("共 %d 个分片（试片模式：仅下载前 %d 个）\n", len(segURLs), job.limit)
		segURLs = segURLs[:job.limit]
	} else {
		fmt.Printf("共 %d 个分片\n", len(segURLs))
	}

	// 3. 边下边写（分片按序 append，写完即成品，无独立合并阶段）
	fmt.Println("\n[3/4] 下载并写入文件…")
	written, err := streamDownload(context.Background(), job, segURLs, 0, partPath)
	if err != nil {
		fmt.Printf("\n下载失败（已写入 %d/%d 个分片）: %v\n", written, len(segURLs), err)
		return 1
	}

	// 4. 输出：.part 改名为正式文件（HLS 原始流直接落盘，不做封装）
	fmt.Println("\n[4/4] 输出文件...")
	if err := emitOutput(partPath, outputFile); err != nil {
		fmt.Printf("输出失败: %v\n", err)
		return 1
	}

	fmt.Printf("\n全部完成，耗时 %s\n", time.Since(start).Round(time.Second))
	return 0
}
