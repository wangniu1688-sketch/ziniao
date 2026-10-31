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
	"syscall"
	"time"

	"ziniao/app"
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
