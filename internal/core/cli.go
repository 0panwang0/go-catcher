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

// CLI 运行参数（userAgent/outputFile/limit/bindAddr/referer）现为 Runtime
// 字段（见 runtime.go）：RunCLI 创建一次性 Runtime 并把参数写进去，下载引擎
// 各模块经 job.rt 读取。proxyAddr 的声明与并发安全访问在 net.go
// （getProxyAddr / setProxyAddr）。分片并发与重试次数是 /config 可热改的
// 运行时参数，为 Runtime 上的原子字段（concurrencyNow / maxRetriesNow /
// setDownloadTuning）。

// ParseCLI 解析命令行（未识别参数由 flag 包报错退出）。
func ParseCLI(args []string) CLIOptions {
	o := CLIOptions{
		Proxy:       "system",
		Output:      "output.ts",
		Concurrency: defaultConfig().SegConcurrency,
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
	fmt.Println("注意: CLI 不做断点续传；若输出文件旁存在 .part 半成品，会被丢弃后重下（需续传请用 GUI 客户端）")
	fmt.Println("其它: --server [--port=端口] 无头服务模式 | 不带任何参数启动 GUI 客户端")
}

// describeProxy 启动横幅用的代理描述：system 模式展开为注册表实际读数。
func (r *Runtime) describeProxy() string {
	p := strings.TrimSpace(r.getProxyAddr())
	if strings.EqualFold(p, "system") {
		if a := r.systemProxyAddr(); a != "" {
			return "system → " + a
		}
		return "system → 系统未启用代理（直连）"
	}
	return p
}

// RunCLI 执行单任务直下流程，返回退出码。
func RunCLI(o CLIOptions) int {
	// CLI 拥有独立的一次性运行时：不与任何 Engine 共享状态
	rt := newRuntime()
	rt.setProxyAddr(o.Proxy)
	rt.setDownloadTuning(o.Concurrency, -1) // CLI 只暴露分片并发，不动重试次数
	rt.outputFile = o.Output
	rt.outputFile = rt.normalizeOutput()
	outAbs, _ := filepath.Abs(rt.outputFile)

	job := &dlJob{
		rt:      rt,
		m3u8URL: o.URL,
		referer: o.Referer,
		saveDir: filepath.Dir(outAbs),
		fname:   filepath.Base(outAbs),
		limit:   o.Limit,
	}
	partPath := outAbs + ".part"

	start := time.Now()
	fmt.Println("========================================")
	fmt.Println("  m3u8 视频下载器 (uTLS + Clash)")
	fmt.Println("  URL:     ", job.m3u8URL)
	fmt.Println("  代理:    ", rt.describeProxy())
	fmt.Println("  Referer: ", job.referer)
	fmt.Println("  输出:    ", rt.outputFile)
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
		if err := emitOutput(partPath, rt.outputFile); err != nil {
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
	if verr := validatePlaylist(pl); verr != nil {
		fmt.Printf("%v\n", verr)
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

	// 3. 容器探测：拉取首片识别真实格式 → 修正输出扩展名 → 写 init 段。
	//    探测同时把 job.container / job.norm 装配好，后续分片写入才会做容器
	//    规范化（fMP4 时间戳归一化）——这一步以前 CLI 漏了，导致同一份源
	//    在命令行下产出的文件与界面下载的不是同一个东西。
	finalOut := rt.outputFile
	container, cerr := probeContainer(dlCtx, job, &pl, segURLs[0])
	if cerr != nil {
		fmt.Printf("%v\n", cerr)
		return 1
	}
	if nf := correctExtName(finalOut, container); nf != finalOut {
		finalOut = nf
		outAbs, _ = filepath.Abs(finalOut)
		partPath = outAbs + ".part"
		fmt.Printf("识别为 %s 容器，输出扩展名修正为 %s\n", container.ID, finalOut)
	}
	if resetErr := resetPartForRerun(partPath); resetErr != nil {
		fmt.Printf("%v\n", resetErr)
		return 1
	}
	if werr := writeInitSegmentFor(dlCtx, job, &pl, partPath); werr != nil {
		fmt.Printf("%v\n", werr)
		return 1
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

	// 5. 落盘前抽样校验 + 容器收尾（fMP4 回填总时长），再 .part 改名为正式文件
	fmt.Println("\n[4/4] 输出文件...")
	valSegs := len(segURLs)
	if isLive {
		valSegs = 0 // 直播窗口大小不代表总量，体量合理性检查不适用
	}
	if verr := validateOutput(job.container, partPath, ProbeInfo{
		Encrypted: pl.key != nil,
		Segments:  valSegs,
		MinBytes:  int64(job.initLen),
	}); verr != nil {
		fmt.Printf("产物校验失败: %v\n", verr)
		return 1
	}
	if jerr := job.backfill(partPath); jerr != nil {
		fmt.Printf("[warn] 容器收尾处理失败（不影响文件可播性）: %v\n", jerr)
	}
	if err := emitOutput(partPath, finalOut); err != nil {
		fmt.Printf("输出失败: %v\n", err)
		return 1
	}

	fmt.Printf("\n全部完成，耗时 %s\n", time.Since(start).Round(time.Second))
	return 0
}

// resetPartForRerun CLI 的 HLS 路径不做断点续传：`<输出>.part` 一旦存在，
// 必须丢弃后重下。
//
// 为什么不能直接续跑（2026-09-11 评审 P1-2）：streamWriter 以 O_APPEND 打开
// 目标文件、startIdx 恒为 0（见 newStreamWriter），而磁盘上可能还留着上一次失败
// 写下的几百 MB。旧代码既不截断也不推进 startIdx，新一轮内容会从文件末尾再写
// 一遍 —— 产物是一个"视频播两遍"的合法 TS，validateOutput 的同步字节判据发现
// 不了，用户拿到的是静默损坏、体积翻倍的文件。
//
// 选择"丢弃"而不是"按长度反推断点"：分片边界在字节层面不可复原（没有持久化
// 已写分片数），猜错就是错位。清掉至少是确定的正确。
//
// GUI 路径无此问题：新任务经 uniquePath 拿到不冲突的文件名，不会复用旧 .part。
func resetPartForRerun(partPath string) error {
	info, err := os.Stat(partPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("检查临时文件失败: %w", err)
	}
	if info.IsDir() {
		return nil
	}
	if info.Size() > 0 {
		fmt.Printf("检测到上次未完成的临时文件（%s，%s）——CLI 不做断点续传，已丢弃并重新下载\n",
			partPath, humanBytes(info.Size()))
	}
	// 删除失败必须中止：留着旧内容继续写就是"内容写两遍"那个 bug。
	if rmErr := os.Remove(partPath); rmErr != nil && !os.IsNotExist(rmErr) {
		return fmt.Errorf("丢弃临时文件失败（请手动删除后重试）: %s: %w", partPath, rmErr)
	}
	// 分片位图是直链分片续传的元数据，与本次全新下载无关，一并清掉避免误用
	if rmErr := os.Remove(chunkMetaPath(partPath)); rmErr != nil && !os.IsNotExist(rmErr) {
		fmt.Printf("[warn] 清理分片位图失败 %s: %v\n", chunkMetaPath(partPath), rmErr)
	}
	return nil
}

// humanBytes 把字节数格式化成便于阅读的 GB/MB（仅用于提示信息）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(int64(1)<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(int64(1)<<20))
	default:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
}
