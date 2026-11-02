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
	"ziniao/browser"
	"ziniao/delegation"
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
	Approvals   *approval.Service
	Delegations *delegation.Service
	Jobs        *jobs.Scheduler

	// M1：启动器内核（本机浏览器编排 / 运行时注册表 / CA 就绪）
	Browser *browser.Manager

	// M2：手工代理台账与店铺分组
	Proxies *shop.ProxyService
	Groups  *shop.GroupService

	Handler http.Handler
}

// New 装配应用：内置角色 → 事件总线（审计/通知同源）→ 各模块服务 → 网关。
func New(ctx context.Context, cfg *config.Config, st store.Store, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.Default()
	}
	// 内置角色：缺的建、已有的把新版本新增的权限码并集补齐。
	// 补齐数量要打出来——「升级后运营主管能用删除入口了」这件事在日志里
	// 没有痕迹的话，排查「为什么这台机器不行」只能靠猜部署时间。
	synced, err := iam.EnsureBuiltinRolesWithReport(ctx, st)
	if err != nil {
		return nil, err
	}
	if len(synced) > 0 {
		log.Info("内置角色权限已同步", "added", synced)
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
	// Phase 2：审批工单 + 代理机构委托 + 定时任务（依赖 Phase 1 的安全与设备服务）
	a.Approvals = approval.NewService(st, bus, cfg, engine)
	a.Delegations = delegation.NewService(st, bus, cfg, engine)
	a.wireJobs()
	// M1：启动器内核。装配失败不阻断启动——它会让「启动环境」不能用，
	// 但店铺/审批/审计这些管理台能力不该因为本机没装 Chrome 而一起挂掉。
	a.wireBrowser()
	// profile 根目录：建环境时落 profile 元数据（M2 不拉起浏览器，M3 启动复用同一目录）
	if a.Browser != nil {
		a.Environments.SetProfileRoot(a.Browser.Layout().ProfilesDir)
		// M3：启动/停止链路要能下发启动单、提交 running、失败复位、对账复位。
		a.Browser.SetEnvControl(a.Environments)
		// P1：画像 UA 不得声称比本机 Chrome 还新（内核对齐的上限）。
		// 探测不到版本时不设上限——宁可沿用画像版本，也不要把 0 当成「本机 0」。
		if m := a.Browser.Browser().Major; m > 0 {
			a.Environments.SetKernelMajor(m)
		} else {
			a.Log.Warn("未探测到本机浏览器版本：内核对齐不做上限约束（画像 UA 原样使用）")
		}
	}

	a.Handler = gateway.New(gateway.Deps{
		Store: st, Bus: bus, Config: cfg, IAM: engine,
		Identity: a.Identity, Tenant: a.Tenant, Shop: a.Shop,
		Audit: a.Audit, Notify: a.Notify, Entitlement: ent, Log: log,
		Device: a.Device, Security: a.Security, Environments: a.Environments,
		Vault: a.Vault, Fingerprints: a.Fingerprints, Baselines: a.Baselines,
		Approvals: a.Approvals, Jobs: a.Jobs, DemoSite: cfg.DemoSite,
		Delegations: a.Delegations, NotifySink: a.NotifySink,
		Browser: a.Browser, Proxies: a.Proxies, Groups: a.Groups,
	})
	return a, nil
}

// wireBrowser 装配启动器内核：数据目录 + 单实例锁 + 运行时注册表 + 浏览器探测 + CA 状态。
//
// 与 SetIPPool 一样放在装配期而不是首次请求时：CA 未就绪、浏览器缺失这类问题
// 必须在**服务起来那一刻**就在日志里说清楚，而不是等运营点了「启动」才发现。
func (a *App) wireBrowser() {
	m, err := browser.New(browser.Options{
		DataDir:    a.Config.DataDir,
		CADir:      a.Config.CADir,
		ChromePath: a.Config.ChromePath,
		Sidepanel:  a.Config.Sidepanel,
		Log:        a.Log,
	})
	if err != nil {
		a.Log.Error("启动器内核装配失败：环境启动不可用", "err", err.Error())
		return
	}
	a.Browser = m
	if !m.CAReady() {
		a.Log.Warn("MITM 根证书未安装：环境启动会被阻断（先执行 CA 安装）",
			"ca_dir", m.Layout().CADir, "detail", m.CA().Detail)
	}
}

// CloseBrowser 释放启动器实例锁（进程退出路径调用）。
func (a *App) CloseBrowser() error {
	if a.Browser == nil {
		return nil
	}
	return a.Browser.Close()
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
	var kek *crypto.LocalKEK
	var kekErr error
	if len(cfg.KEKKeys) > 0 {
		kek, kekErr = crypto.NewLocalKEK(cfg.KEKKeys, cfg.KEKCurrentVersion)
	} else if isDevKEK(cfg) {
		// 仅内存开发环境允许随机 KEK；重启后密文不可解是预期的开发限制。
		kek, kekErr = crypto.NewLocalKEK(nil, 0)
	} else {
		kekErr = fmt.Errorf("持久化 KEK 未配置：请设置 ZINIAO_KEK_KEYS")
	}
	if kekErr != nil {
		a.Log.Error("KEK 初始化失败", "err", kekErr)
	}
	if kek != nil && kek.CurrentVersion() > 0 && isDevKEK(cfg) && len(cfg.KEKKeys) == 0 {
		a.Log.Warn("使用临时随机 KEK（dev only）：重启后历史密文不可解")
	}
	cipher, cipherErr := crypto.NewEnvelope(kek)
	if cipherErr != nil {
		a.Log.Error("信封加密初始化失败", "err", cipherErr)
	}
	a.Cipher = cipher

	a.Security = security.NewService(st, bus, engine)
	// 设备不再自带一个写死的 mock 代理商：设备出口必须和环境走同一条分配链
	// （互斥 / 跨重启不换 IP / 按代理商归还 / 留历史）。出口池由 SetIPPool 注入，
	// 没接代理商时设备就是「没有出口」，其余能力照常可用。
	a.Device = device.NewService(st, bus, cfg, engine, a.Entitlement, a.Meter)
	a.Environments = shop.NewEnvService(st, bus, engine, a.Fingerprints, baselines)
	a.Environments.SetSecurity(a.Security) // 出口 IP 变更等账号风险要能被运营看见
	// M3：环境绑了手工代理时，启动要能把出口口令解出来（明文只在内存，不进 argv/日志/响应）。
	a.Environments.SetCipher(cipher)
	// P3：启动前出口拨测配置。开关与目标只有一个来源（platform/config），
	// EnvService 不自己读环境变量——否则同一件事会有两处真相。
	a.Environments.SetEgressProbe(shop.EgressProbeConfig{
		Enabled:       cfg.ProxyTestEnabled,
		Target:        cfg.ProxyTestTarget,
		CountryTarget: cfg.ProxyTestCountryTarget,
	})
	a.Vault = shop.NewVault(st, bus, engine, cipher)
	// M2：代理台账与店铺分组。聚合创建「店铺 + 环境」要走与店铺服务同一套配额与计量，
	// 否则「店铺数超限」会出现两套答案。
	a.Proxies = shop.NewProxyService(st, bus, engine, cipher)
	a.Groups = shop.NewGroupService(st, bus, engine)
	a.Environments.SetBilling(a.Entitlement, a.Meter)
}

// SetIPPool 接入住宅代理商的出口 IP 池。
//
// 这是多账号防关联的**最后一块拼图**：不配池子时所有店铺都从本机公网 IP 出网，
// 平台一眼就能把账号串起来——画像再像、指纹再散都白费。
func (a *App) SetIPPool(alloc *network.Allocator) {
	a.Environments.SetIPPool(alloc)
	// 同一张路由表、同一套不变量给设备用：设备的出口若另开一条链路，
	// 「两个店铺共用出口」这条最硬的红线就会出现一个谁也看不到的后门。
	if a.Device != nil {
		a.Device.SetEgressPool(alloc)
	}
}

// RestoreEgressQuota 启动期按库里仍存在的绑定回填路由的并发名额。
//
// 不做这件事的代价是**重启后的一段时间内名额看起来无限**：
// 代理商那边的并发已经满了（上一个进程留下的绑定还在），
// 我们却继续向它派单，对方限流/断流时这边的监控还是一片祥和。
//
// 逐租户读而不做跨租户全表扫描：仓储只暴露带租户上下文的读口，
// 不给「绕过 RLS 的全局读」开先例。
func (a *App) RestoreEgressQuota(ctx context.Context) (int, error) {
	pool := a.Environments.IPPool()
	if pool == nil {
		return 0, nil
	}
	ids, err := a.allTenantIDs(ctx)
	if err != nil {
		return 0, err
	}
	n, err := pool.RestoreInflight(ctx, ids)
	if err != nil {
		return n, err
	}
	if n > 0 && a.Log != nil {
		a.Log.Info("已按库里的既存绑定回填出口名额（重启不丢配额）", "count", n, "tenants", len(ids))
	}
	return n, nil
}

// allTenantIDs 分页取全量租户 ID（启动期一次性遍历，量级可控）。
func (a *App) allTenantIDs(ctx context.Context) ([]string, error) {
	out := []string{}
	const pageSize = 200
	for offset := 0; ; offset += pageSize {
		list, total, err := a.Store.Tenants().List(ctx, pageSize, offset)
		if err != nil {
			return out, err
		}
		for _, t := range list {
			if t.ID != "" {
				out = append(out, t.ID)
			}
		}
		if len(list) < pageSize || offset+pageSize >= total {
			break
		}
	}
	return out, nil
}

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
