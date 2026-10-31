// Command server 平台服务端入口：一条命令启动 Phase 0 全部能力。
//
//	go run ./cmd/server -store memory -seed        # 零依赖本地启动（默认）
//	go run ./cmd/server -store postgres -migrate   # 生产：先跑迁移再启动
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ziniao/app"
	"ziniao/network"
	"ziniao/platform/config"
)

func main() {
	addr := flag.String("addr", "", "监听地址，覆盖配置")
	driver := flag.String("store", "", "存储驱动：memory|postgres")
	doMigrate := flag.Bool("migrate", false, "启动时执行数据库迁移（仅 postgres）")
	doSeed := flag.Bool("seed", false, "写入演示数据（租户/成员/店铺）")
	noDispatch := flag.Bool("no-dispatch", false, "不启动事件派发循环（测试用）")
	noJobs := flag.Bool("no-jobs", false, "不启动定时巡检任务（测试用）")
	demoSite := flag.Bool("demo-site", false, "挂载账号隔离实测站点 /demo/*（仅本地验证用，生产严禁开启）")
	ipBase := flag.String("ip-base-url", "", "住宅代理商 API 基址；为空表示不启用逐环境出口 IP（生产必填）")
	ipAcquire := flag.String("ip-acquire-path", "/api/v1/proxies/acquire", "代理商申请出口资源接口路径")
	ipRelease := flag.String("ip-release-path", "/api/v1/proxies/release", "代理商归还出口资源接口路径")
	ipAttach := flag.String("ip-attach-path", "", "代理商按 ID 取回出口的接口路径（支持 {id} 占位）；不配则重启后无法复用既有出口")
	ipAuth := flag.String("ip-auth", "bearer", "代理商鉴权方式：bearer|query|basic")
	ipProto := flag.String("ip-protocol", "socks5", "代理商出口协议：socks5|http|https")
	ipLease := flag.Duration("ip-lease", 24*time.Hour, "资源未返回到期时间时的兜底租约")
	flag.Parse()

	cfg := config.Load()
	if *addr != "" {
		cfg.HTTPAddr = *addr
	}
	if *driver != "" {
		cfg.StoreDriver = *driver
	}
	if *demoSite {
		cfg.DemoSite = true
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, cfg, *doMigrate, log)
	if err != nil {
		log.Error("存储初始化失败", "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	a, err := app.New(ctx, cfg, st, log)
	if err != nil {
		log.Error("应用装配失败", "err", err)
		os.Exit(1)
	}
	wireIPPool(ctx, log, a, *ipBase, *ipAcquire, *ipRelease, *ipAttach, *ipAuth, *ipProto, *ipLease)

	if !*noDispatch {
		go a.Bus.Start(ctx)
	}
	// 定时巡检：设备健康 / 回收站清理 / 审批单过期（Phase 2）
	if !*noJobs && a.Jobs != nil {
		go a.Jobs.Start(ctx, cfg.JobInterval)
		log.Info("定时任务已启动", "jobs", a.Jobs.Names(), "interval", cfg.JobInterval.String())
	}
	if *doSeed {
		if err := seed(ctx, a, log); err != nil {
			log.Error("演示数据写入失败", "err", err)
			os.Exit(1)
		}
	}

	httpSrv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("服务启动", "addr", cfg.HTTPAddr, "store", cfg.StoreDriver, "phase", "2")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务异常", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Info("服务已停止")
}

// wireIPPool 装配住宅代理商出口池（每个环境一个独立出口 IP）。
//
// 这是多账号防关联的最后一块拼图：不配池子时所有店铺共用本机公网出口，
// 平台一眼就能把账号串起来——画像再像、指纹再散都白费。
//
// 代理商密钥**只从环境变量读取**（ZINIAO_IP_TOKEN）：口令进 argv 会被任意进程
// 用 wmic 读到，这一点与上游代理凭据同样处理。
//
// 出口绑定会写入 egress_bindings（重启/发布不换 IP）。AttachPath 决定重启后能否
// 「取回」同一出口：凭据不落库（L4），厂商不支持取回时只能重新申请，
// 那时会走轮换告警——让运营看见，而不是静默换 IP。
func wireIPPool(ctx context.Context, log *slog.Logger, a *app.App,
	base, acquirePath, releasePath, attachPath, auth, proto string, lease time.Duration) {
	if strings.TrimSpace(base) == "" {
		log.Warn("未配置住宅代理商：所有环境共用本机公网出口，多账号存在关联风险",
			"hint", "-ip-base-url + ZINIAO_IP_TOKEN")
		return
	}
	token := strings.TrimSpace(os.Getenv("ZINIAO_IP_TOKEN"))
	if token == "" {
		log.Error("已配置代理商但未设置 ZINIAO_IP_TOKEN，出口 IP 池未启用")
		return
	}
	p := network.NewHTTPProvider(network.VendorConfig{
		Name: "ip-vendor", BaseURL: base, Token: token,
		AuthStyle: auth, AcquirePath: acquirePath, ReleasePath: releasePath,
		AttachPath: attachPath,
		Protocol:   network.Protocol(proto), Timeout: 15 * time.Second,
	})
	if err := p.Health(ctx); err != nil {
		log.Error("代理商健康检查失败，出口 IP 池未启用", "err", err.Error())
		return
	}
	alloc := network.NewAllocator(p, lease)
	alloc.SetLogger(log)
	// 绑定落库：没有它，每次重启都会给全部店铺换一次出口 IP。
	alloc.SetBindingStore(a.Store.EgressBindings())
	a.SetIPPool(alloc)
	a.StartIPReaper(ctx, 10*time.Minute)
	if strings.TrimSpace(attachPath) == "" {
		log.Warn("未配置 -ip-attach-path：重启后无法向代理商取回既有出口，" +
			"环境会被重新分配（每次发布都会触发一次出口轮换告警）")
	}
	log.Info("住宅代理商出口池已就绪（逐环境独立出口，绑定已落库）",
		"protocol", proto, "lease", lease.String(), "attach", attachPath != "")
}
