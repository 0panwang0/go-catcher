// CLI 直下模式：参数解析与单任务下载流程（不依赖 HTTP 服务与任务引擎）。
package core

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
// proxyAddr 的声明与并发安全访问在 net.go（getProxyAddr / setProxyAddr）。
var (
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0"
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
		Proxy:       getProxyAddr(),
		Output:      outputFile,
		Concurrency: concurrency,
	}
	fs := flag.NewFlagSet("go-catcher", flag.ExitOnError)
	fs.StringVar(&o.URL, "url", o.URL, "m3u8 URL (必填)")
	fs.StringVar(&o.Referer, "referer", "", "Referer（来源页 URL，部分站点必填）")
	fs.StringVar(&o.Proxy, "proxy", o.Proxy, "Proxy address（默认 system=跟随系统代理；direct/none = 直连；或 http://host:port）")
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
	fmt.Println("代理: --proxy=system 跟随系统代理（默认）| http://host:port 手动 | direct 直连")
	fmt.Println("其它: --server [--port=端口] 无头服务模式 | 不带任何参数启动 GUI 客户端")
}

// describeProxy 启动横幅用的代理描述：system 模式展开为注册表实际读数。
func describeProxy() string {
	p := strings.TrimSpace(getProxyAddr())
	if strings.EqualFold(p, "system") {
		if a := systemProxyAddr(); a != "" {
			return "system → " + a
		}
		return "system → 系统未启用代理（直连）"
	}
	return p
}

// RunCLI 执行单任务直下流程，返回退出码。
func RunCLI(o CLIOptions) int {
	referer = o.Referer
	setProxyAddr(o.Proxy)
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
	fmt.Println("  代理:    ", describeProxy())
	fmt.Println("  Referer: ", job.referer)
	fmt.Println("  输出:    ", outputFile)
	fmt.Println("  TLS指纹: Chrome Auto")
	fmt.Println("========================================")

	// 1. 获取 m3u8 / 直链识别
	fmt.Println("\n[1/4] 获取 m3u8 索引...")
	m3u8Content, baseURL, isDirect, err := job.fetchPlaylist()
	if err != nil {
		fmt.Printf("获取 m3u8 失败: %v\n", err)
		return 1
	}
	if isDirect {
		fmt.Println("识别为直链媒体文件（MP4 等），整体下载…")
		if err := job.downloadDirect(context.Background(), partPath); err != nil {
			fmt.Printf("下载直链文件失败: %v\n", err)
			return 1
		}
		if err := emitOutput(partPath, outputFile); err != nil {
			fmt.Printf("输出失败: %v\n", err)
			return 1
		}
		fmt.Printf("\n全部完成，耗时 %s\n", time.Since(start).Round(time.Second))
		return 0
	}
	fmt.Println("m3u8 获取成功")

	// 2. 解析分片（含 init 段与直播/点播判定）
	fmt.Println("\n[2/4] 解析分片列表...")
	pl := parsePlaylist(m3u8Content, baseURL)
	if len(pl.segments) == 0 {
		fmt.Println("播放列表中没有找到任何媒体分片（响应可能被加密或压缩）")
		return 1
	}
	segURLs := pl.segments
	// 加密流装配解密器（拉取 key 并按 METHOD 建解密器），失败即退出
	dlCtx := context.Background()
	if kerr := job.ensureDecryptor(dlCtx, pl.key); kerr != nil {
		fmt.Printf("装配解密器失败: %v\n", kerr)
		return 1
	}
	isLive := !pl.hasEndList
	if job.limit > 0 && job.limit < len(segURLs) {
		fmt.Printf("共 %d 个分片（试片模式：仅下载前 %d 个）\n", len(segURLs), job.limit)
		segURLs = segURLs[:job.limit]
		isLive = false // 试片强制一次性下载当前窗口
	} else if isLive {
		fmt.Printf("共 %d 个分片，识别为直播流（无 ENDLIST），进入跟随录制（Ctrl+C 停止并保存）\n", len(segURLs))
	} else {
		fmt.Printf("共 %d 个分片\n", len(segURLs))
	}

	// 3. 容器探测：拉取首片识别真实格式 → 修正输出扩展名 → 写 init 段
	finalOut := outputFile
	pre, perr := fetchSegment(dlCtx, job, segURLs[0])
	if perr != nil {
		fmt.Printf("探测视频格式失败（首个分片）: %v\n", perr)
		return 1
	}
	// 加密流：探测分片先解密（容器魔数在密文上看不出）
	if pre, perr = job.decryptSegmentIfAny(pl.mediaSeq, pre); perr != nil {
		fmt.Printf("解密首个分片失败: %v\n", perr)
		return 1
	}
	job.pre = pre
	job.preURL = segURLs[0]
	container := detectContainer(pre, pl.hasMap)
	// 加密流误判守卫（同 pipeline.go）：generic + 声明加密 + 解密后无媒体特征
	// → 判定解密失败，立即中止，避免把密文当成品落盘。
	if gerr := guardGenericMedia(container, pl.key, pre); gerr != nil {
		fmt.Printf("探测失败: %v\n", gerr)
		return 1
	}
	if container.Ext != "" && !strings.HasSuffix(strings.ToLower(finalOut), container.Ext) {
		finalOut = strings.TrimSuffix(outputFile, filepath.Ext(outputFile)) + container.Ext
		outAbs, _ = filepath.Abs(finalOut)
		partPath = outAbs + ".part"
		fmt.Printf("识别为 %s 容器，输出扩展名修正为 %s\n", container.ID, finalOut)
	}
	if container.Init == InitFromMap {
		if pl.mapURI == "" {
			fmt.Println("识别为 fMP4 分片流，但播放列表没有 #EXT-X-MAP 初始化段，无法生成可播放文件")
			return 1
		}
		initData, ferr := fetchSegment(dlCtx, job, pl.mapURI)
		if ferr != nil {
			fmt.Printf("获取 fMP4 init 段失败: %v\n", ferr)
			return 1
		}
		if werr := writeInitSegment(partPath, initData); werr != nil {
			fmt.Printf("写入 init 段失败: %v\n", werr)
			return 1
		}
	}

	// 4. 下载并写入文件（直播跟随 / 点播一次性；边下边写，写完即成品）
	fmt.Println("\n[3/4] 下载并写入文件…")
	written := 0
	if isLive {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		dlCtx = ctx
		job.live = true
		job.seen = make(map[string]bool)
		written, err = job.liveDownload(dlCtx, partPath, 0)
	} else {
		written, err = streamDownload(dlCtx, job, segURLs, 0, pl.mediaSeq, partPath)
	}
	if err != nil {
		fmt.Printf("\n下载失败（已写入 %d 个分片）: %v\n", written, err)
		return 1
	}

	// 5. 输出：.part 改名为正式文件（原始流直接落盘，不做封装）
	fmt.Println("\n[4/4] 输出文件...")
	if err := emitOutput(partPath, finalOut); err != nil {
		fmt.Printf("输出失败: %v\n", err)
		return 1
	}

	fmt.Printf("\n全部完成，耗时 %s\n", time.Since(start).Round(time.Second))
	return 0
}
