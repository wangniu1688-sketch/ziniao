// Command ziniao-proxy 每个店铺环境一个本地前置代理进程。
//
// 它的职责是把环境窗口的出站 TLS 握手换成「该环境声称的内核版本」对应的
// ClientHello——否则 JS 层指纹再怎么改，电商平台在 TLS 层看到的仍是同一个
// WebView2 指纹，多账号照样被判同一台设备（L2 致命项）。
//
// 用法（客户端自动拉起，无需人工执行）：
//
//	ziniao-proxy --listen 127.0.0.1:20001 --kernel 120
//	ziniao-proxy --listen 127.0.0.1:20001 --hello-id HelloChrome_120_PQ
//	ziniao-proxy --listen 127.0.0.1:20001 --kernel 120 \
//	    --upstream-host proxy.example.com --upstream-port 8080 \
//	    --upstream-proto http --upstream-user u --upstream-pass p
//
// 红线：
//   - 默认校验证书（生产严禁关闭校验）；
//   - 启动后 GET /healthz 返回该进程实际会发出的 JA3/JA4（仅指纹，不含凭证）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ziniao/fingerprint"
	"ziniao/network"
)

// applyEnvFallbacks 命令行未显式给出时，从环境变量补齐（凭证不要走 argv）。
func applyEnvFallbacks(m map[string]*string) {
	for k, dst := range m {
		if v := os.Getenv(k); v != "" && *dst == "" {
			*dst = v
		}
	}
}

func main() {
	var (
		listen   = flag.String("listen", "127.0.0.1:20000", "本地监听地址")
		kernel   = flag.Int("kernel", 0, "Chromium 内核大版本（按基线库推导 ClientHello）")
		helloID  = flag.String("hello-id", "", "直接指定 uTLS ClientHelloID（优先于 --kernel）")
		upHost   = flag.String("upstream-host", "", "上游代理主机（留空=本机直连）")
		upPort   = flag.Int("upstream-port", 0, "上游代理端口")
		upProto  = flag.String("upstream-proto", "http", "上游代理协议：http|https|socks5|direct")
		upUser   = flag.String("upstream-user", "", "上游代理用户名（L4：不入日志）")
		upPass   = flag.String("upstream-pass", "", "上游代理口令（L4：不入日志）")
		caDir    = flag.String("ca-dir", "", "本地 CA 目录（默认 %APPDATA%\\ziniao\\ca）")
		installC = flag.Bool("install-ca", true, "首次运行时把本地 CA 装入当前用户信任库（certutil -user）")
		exportCA = flag.String("export-ca", "", "把本地根证书导出到指定路径后退出（给用户手动信任/排障用）")
		uninstCA = flag.Bool("uninstall-ca", false, "从当前用户信任库移除本地根证书（卸载/合规要求）")
		insecure = flag.Bool("insecure", false, "跳过证书校验（仅本地自签探针自测用）")
		timeout  = flag.Duration("timeout", 20*time.Second, "单次连接超时")
	)
	flag.Parse()

	// 凭证优先从环境变量取：命令行参数在 Windows 上可被任意进程通过
	// `wmic process get commandline` 读到，把代理口令写在 argv 里等于泄密。
	// 客户端拉起本进程时应只传 --listen/--kernel，凭证走环境变量注入。
	applyEnvFallbacks(map[string]*string{
		"ZINIAO_UPSTREAM_HOST":  upHost,
		"ZINIAO_UPSTREAM_USER":  upUser,
		"ZINIAO_UPSTREAM_PASS":  upPass,
		"ZINIAO_UPSTREAM_PROTO": upProto,
	})
	if upPortEnv := os.Getenv("ZINIAO_UPSTREAM_PORT"); upPortEnv != "" && *upPort == 0 {
		if v, err := strconv.Atoi(upPortEnv); err == nil {
			*upPort = v
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// ---- 1. 决定 ClientHello：显式指定优先，否则按内核基线推导 ----
	name := strings.TrimSpace(*helloID)
	if name == "" && *kernel > 0 {
		lib, err := fingerprint.LoadBaselines()
		if err != nil {
			logger.Error("基线库加载失败", "err", err)
			os.Exit(2)
		}
		if bl, ok := lib.For(*kernel); ok && bl.HelloID != "" {
			name = bl.HelloID
		} else {
			logger.Warn("基线库无该内核条目，回退 Chrome Auto", "kernel", *kernel)
		}
	}
	if name == "" {
		name = "HelloChrome_Auto"
	}

	// ---- 2. 构造 upstream（不打印口令） ----
	up := network.ProxyConfig{
		Protocol: network.ProtoDirect,
		Host:     *upHost,
		Port:     *upPort,
		Username: *upUser,
		Password: *upPass,
	}
	if *upHost != "" {
		switch strings.ToLower(*upProto) {
		case "socks5":
			up.Protocol = network.ProtoSOCKS5
		case "https":
			up.Protocol = network.ProtoHTTPS
		case "direct":
			up.Protocol = network.ProtoDirect
		default:
			up.Protocol = network.ProtoHTTP
		}
	}

	if *caDir == "" {
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = "."
		}
		*caDir = filepath.Join(appdata, "ziniao", "ca")
	}
	proxy := network.NewTLSProxy(network.TLSProxyConfig{
		Upstream: up,
		HelloID:  name,
		Timeout:  *timeout,
		CADir:    *caDir,
		Insecure: *insecure,
	})
	// CA 维护类命令：导出 / 卸载（都只动当前用户信任库，不碰机器级）
	if *exportCA != "" {
		data, err := os.ReadFile(proxy.CA().CertPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "导出失败: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*exportCA, data, 0o600); err != nil { //nolint:gosec // 公钥可 0600
			fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已导出本地根证书到 %s（该文件仅含公钥，可安全分发给需要信任的人员）\n", *exportCA)
		return
	}
	if *uninstCA {
		if err := proxy.CA().Uninstall(); err != nil {
			fmt.Fprintf(os.Stderr, "卸载失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("已从当前用户信任库移除本地根证书")
		return
	}
	if *installC {
		// 幂等：已安装时 certutil 报已存在会被忽略。CA 不进机器级信任库。
		if err := proxy.CA().EnsureInstalled(); err != nil {
			logger.Warn("本地根证书未能自动安装（HTTPS 页面会报证书错误，需手动信任）", "err", err.Error(), "ca", proxy.CA().CertPath())
		} else {
			logger.Info("本地根证书已就绪（用于本机 TLS 隧道重建 ClientHello，仅装在当前用户信任库）", "ca", proxy.CA().CertPath())
		}
	}

	_, hash, err := proxy.JA3()
	if err != nil {
		logger.Error("ClientHello 构造失败", "err", err)
		os.Exit(2)
	}
	ja4, err := proxy.JA4()
	if err != nil {
		logger.Error("JA4 构造失败", "err", err)
		os.Exit(2)
	}
	// 只输出排障必需的指纹信息，不含任何凭证（L4）
	logger.Info("uTLS 前置代理已就绪",
		"listen", *listen, "hello_id", name, "ja3", hash, "ja4", ja4)
	if *insecure {
		logger.Warn("已跳过证书校验（仅本地自测，生产严禁开启）")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := proxy.Serve(ctx, *listen); err != nil {
		fmt.Fprintln(os.Stderr, "代理退出:", err)
		os.Exit(1)
	}
}
