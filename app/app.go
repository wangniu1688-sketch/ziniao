// Package app 组合根（composition root）：把所有模块装配成可运行的应用。
// 服务端与测试共用同一装配路径，避免「测试跑的和线上跑的不是一套」。
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"ziniao/approval"
	"ziniao/audit"
	"ziniao/billing"
	"ziniao/device"
	"ziniao/eventbus"
	"ziniao/fingerprint"
	"ziniao/gateway"
	"ziniao/iam"
	"ziniao/identity"
	"ziniao/jobs"
	"ziniao/network"
	"ziniao/notify"
	"ziniao/platform/config"
	"ziniao/platform/crypto"
	"ziniao/platform/store"
	"ziniao/security"
	"ziniao/shop"
	"ziniao/tenant"
)

// App 应用实例。
type App struct {
	Store       store.Store
	Bus         *eventbus.Bus
	Config      *config.Config
	Log         *slog.Logger
	IAM         *iam.Engine
	Identity    *identity.Service
	Tenant      *tenant.Service
	Shop        *shop.Service
	Audit       *audit.Service
	Notify      *notify.Service
	NotifySink  *notify.Sink // 事件 → 站内通知 + 外发（重投任务要拿它补发）
	Meter       *billing.Meter
	Entitlement *billing.Entitlement

	// Phase 1
	Device       *device.Service
	Security     *security.Service
	Environments *shop.EnvService
	Vault        *shop.Vault
	Fingerprints *fingerprint.Library
	Baselines    *fingerprint.BaselineLibrary
	Cipher       *crypto.EnvelopeCipher

	// Phase 2：审批工单与定时任务
	Approvals *approval.Service
	Jobs      *jobs.Scheduler

	Handler http.Handler
}

// New 装配应用：内置角色 → 事件总线（审计/通知同源）→ 各模块服务 → 网关。
func New(ctx context.Context, cfg *config.Config, st store.Store, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		return nil, err
	}
	bus := eventbus.New(st, cfg, log)
	bus.Register(audit.NewSink(st))
	// 外发通道（邮件/Webhook）按环境配置装配；配置非法时拒绝启动，
	// 而不是启动后发现 high 告警一条都没出去。
	senders, err := notify.BuildSenders(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("app: 通知通道装配失败: %w", err)
	}
	sink := notify.NewSink(st, log, senders...).UseConfig(cfg)
	bus.Register(sink)

	meter := billing.NewMeter(st)
	ent := billing.NewEntitlement(st, cfg)
	engine := iam.NewEngine(cfg)

	a := &App{
		Store: st, Bus: bus, Config: cfg, Log: log, IAM: engine,
		Identity:    identity.NewService(st, bus, cfg, ent),
		Audit:       audit.NewService(st),
		Notify:      notify.NewService(st),
		NotifySink:  sink,
		Meter:       meter,
		Entitlement: ent,
	}
	a.Tenant = tenant.NewService(st, bus, cfg, meter, ent)
	a.Shop = shop.NewService(st, bus, cfg, engine, meter, ent)

	a.wirePhase1()
	// Phase 2：审批工单 + 定时任务（依赖 Phase 1 的安全与设备服务）
	a.Approvals = approval.NewService(st, bus, cfg, engine)
	a.wireJobs()

	a.Handler = gateway.New(gateway.Deps{
		Store: st, Bus: bus, Config: cfg, IAM: engine,
		Identity: a.Identity, Tenant: a.Tenant, Shop: a.Shop,
		Audit: a.Audit, Notify: a.Notify, Entitlement: ent, Log: log,
		Device: a.Device, Security: a.Security, Environments: a.Environments,
		Vault: a.Vault, Fingerprints: a.Fingerprints, Baselines: a.Baselines,
		Approvals: a.Approvals, Jobs: a.Jobs, DemoSite: cfg.DemoSite,
	})
	return a, nil
}

// RunJobsOnce 手动跑一遍定时任务（CLI / 单测 / 演示脚本用）。
func (a *App) RunJobsOnce(ctx context.Context) []jobs.Result {
	if a.Jobs == nil {
		return nil
	}
	return a.Jobs.RunOnce(ctx)
}

// wirePhase1 装配 Phase 1 能力：指纹库 / TLS 基线 / 凭证信封加密 / 设备 / 安全 / 环境。
func (a *App) wirePhase1() {
	st, cfg, bus, engine := a.Store, a.Config, a.Bus, a.IAM

	a.Fingerprints = fingerprint.NewLibrary(fingerprint.DefaultLibraryConfig(), time.Now().UnixNano())
	baselines, err := fingerprint.LoadBaselines()
	if err != nil {
		a.Log.Error("TLS 基线加载失败", "err", err)
		baselines, _ = fingerprint.LoadBaselines()
	}
	a.Baselines = baselines

	// 用 uTLS 真实构造的 ClientHello 计算每个内核基线的 JA3/JA4 并回写，
	// 保证「基线 == 代理实际发出的指纹」同源可比（L2）。与真实 Chrome 的一致性
	// 由采集器校准（source=unverified 表示尚未校准）。
	for _, major := range []int{115, 120, 131} {
		bl, ok := baselines.For(major)
		if !ok {
			continue
		}
		proxy := network.NewTLSProxy(network.TLSProxyConfig{HelloID: bl.HelloID, Baseline: bl})
		_, hash, err := proxy.JA3()
		if err != nil {
			a.Log.Warn("JA3 计算失败", "major", major, "err", err)
			continue
		}
		ja4, err := proxy.JA4()
		if err != nil {
			a.Log.Warn("JA4 计算失败", "major", major, "err", err)
			continue
		}
		baselines.RegisterComputed(major, "", hash, ja4, "utls:"+bl.HelloID)
	}

	// 凭证信封加密：本地 KEK（dev 随机）；生产由 SOPS/age 或云 KMS 注入（ADR-0011）
	kek, err := crypto.NewLocalKEK(nil, 0)
	if err != nil {
		a.Log.Error("KEK 初始化失败", "err", err)
	}
	if kek != nil && kek.CurrentVersion() > 0 && isDevKEK(cfg) {
		a.Log.Warn("使用临时随机 KEK（dev only）：重启后历史密文不可解，生产必须注入持久密钥")
	}
	cipher, err := crypto.NewEnvelope(kek)
	if err != nil {
		a.Log.Error("信封加密初始化失败", "err", err)
	}
	a.Cipher = cipher

	a.Security = security.NewService(st, bus, engine)
	a.Device = device.NewService(st, bus, cfg, engine, a.Entitlement, a.Meter,
		network.NewMockProvider("US", network.ASNResidential, "New York"))
	a.Environments = shop.NewEnvService(st, bus, engine, a.Fingerprints, baselines)
	a.Environments.SetSecurity(a.Security) // 出口 IP 变更等账号风险要能被运营看见
	a.Vault = shop.NewVault(st, bus, engine, cipher)
}

// SetIPPool 接入住宅代理商的出口 IP 池。
//
// 这是多账号防关联的**最后一块拼图**：不配池子时所有店铺都从本机公网 IP 出网，
// 平台一眼就能把账号串起来——画像再像、指纹再散都白费。
func (a *App) SetIPPool(alloc *network.Allocator) { a.Environments.SetIPPool(alloc) }

// StartIPReaper 周期回收过期的出口资源：既避免代理商持续计费，
// 也防止本地绑定表随环境反复启停而膨胀。
func (a *App) StartIPReaper(ctx context.Context, every time.Duration) {
	pool := a.Environments.IPPool()
	if pool == nil {
		return
	}
	if every <= 0 {
		every = 10 * time.Minute
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := pool.Reap(ctx); n > 0 {
					a.Log.Info("已回收过期出口资源", "count", n, "provider", pool.Provider().Name())
				}
			}
		}
	}()
}

func isDevKEK(cfg *config.Config) bool {
	return cfg == nil || cfg.StoreDriver == "" || cfg.StoreDriver == "memory"
}

// DispatchOnce 手动触发一次事件派发（测试与 CLI 用，避免异步等待）。
func (a *App) DispatchOnce(ctx context.Context) (int, error) {
	return a.Bus.DispatchOnce(ctx)
}
