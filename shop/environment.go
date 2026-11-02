package shop

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"ziniao/billing"
	"ziniao/eventbus"
	"ziniao/fingerprint"
	"ziniao/iam"
	"ziniao/network"
	"ziniao/platform/apierr"
	"ziniao/platform/crypto"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
	"ziniao/security"
)

// EnvService 店铺浏览器环境服务（M0.3 / M2.2）：
// 负责「按平台 × 站点采样画像 → 绑定内核与 TLS 基线 → 生成注入计划 → 自检报告」。
type EnvService struct {
	st       store.Store
	bus      *eventbus.Bus
	engine   *iam.Engine
	lib      *fingerprint.Library
	baseline *fingerprint.BaselineLibrary
	// pool 出口 IP 资源池（可选）。未配置时环境直连本机出口——
	// **本机单IP 无法做多账号隔离**，生产必须配置。
	pool *network.Allocator
	// sec 安全事件服务（可选）。出口 IP 变更要能被运营看见。
	sec *security.Service

	// M2：聚合创建「店铺 + 环境」需要的配额与计量（与 shop.Service 同一套口径，
	// 不在 EnvService 里另算一份，否则「店铺数超限」会出现两套答案）。
	ent   *billing.Entitlement
	meter *billing.Meter
	// profileRoot profile 根目录（data/profiles）。为空表示未装配启动器内核，
	// 建环境时跳过 profile 元数据落盘（M3 首次启动会补建）。
	profileRoot string
	// cipher 手工代理口令信封的解密器（M3：手工出口要真正生效，明文只在内存）。
	cipher *crypto.EnvelopeCipher
	// probe 启动前出口拨测配置（P3）。由装配层从 platform/config 注入：
	// EnvService 不读环境变量也不读配置文件，避免同一个开关出现第二处真相。
	probe EgressProbeConfig
	// kernelMajor 本机浏览器内核大版本（P1：画像 UA 不得声称比本机 Chrome 更新）。
	// 0 = 未探测到（未装配启动器内核），此时不做上限约束。
	kernelMajor int
	// log 服务日志。只用于「尽力而为」的动作（事件留痕失败等）——这类失败
	// 不能回滚主结果，但也不能一声不吭地吞掉。
	log *slog.Logger
}

// SetIPPool 接入 IP 资源池（由 -ip-base-url 等参数在启动时装配）。
func (s *EnvService) SetIPPool(a *network.Allocator) { s.pool = a }

// EgressProbeConfig 启动前出口拨测配置（P3）。
//
// 与 platform/config 的 ProxyTest* 一一对应，由 app 层装配时搬过来。
// 单独定义而不是直接持有 *config.Config：shop 包不该知道配置文件的结构，
// 而且启动链路的测试要能直接构造一个「拨测关闭」的服务。
type EgressProbeConfig struct {
	// Enabled 对应 ZINIAO_PROXY_TEST。false = 跳过拨测与健康校验。
	Enabled bool
	// Target 拨测目标（对应 ZINIAO_PROXY_TEST_TARGET）。
	Target string
	// CountryTarget 出口国家实测目标（对应 ZINIAO_PROXY_TEST_COUNTRY）。空 = 不实测。
	CountryTarget string
	// TimeoutMS 拨测超时上限，<=0 时取默认值。
	TimeoutMS int
}

// SetEgressProbe 装配启动前出口拨测配置（P3）。
func (s *EnvService) SetEgressProbe(c EgressProbeConfig) { s.probe = c }

// SetSecurity 接入安全事件服务（用于上报出口 IP 变更等账号风险）。
func (s *EnvService) SetSecurity(svc *security.Service) { s.sec = svc }

// SetCipher 接入信封密钥（M3：启动手工出口环境时解密代理口令）。
func (s *EnvService) SetCipher(c *crypto.EnvelopeCipher) { s.cipher = c }

// SetLogger 接入服务日志。
//
// P3 为什么需要它：出口拨测的失败原因要进日志，而「口令不进日志」这条红线
// 只能靠**真的把日志抓下来断言**来证明——不注入，测试就只能对着空气宣誓。
func (s *EnvService) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l
	}
}

// SetKernelMajor 接入本机浏览器内核大版本（P1：UA 版本对齐）。
//
// 由装配期用启动器内核的探测结论（browser.Locator.Detect().Major）注入：
// 画像模板里的 UA 是采集时的版本，本机 Chrome 可能比它老——不设上限就会出现
// 「本机跑 115、却声称 131」这种一眼假的组合。
func (s *EnvService) SetKernelMajor(major int) {
	if major > 0 {
		s.kernelMajor = major
	}
}

// KernelMajor 本机内核大版本（0 = 未探测）。
func (s *EnvService) KernelMajor() int { return s.kernelMajor }

// IPPool 当前出口 IP 池（可能为 nil，用于 healthz 展示编排状态）。
func (s *EnvService) IPPool() *network.Allocator { return s.pool }

// NewEnvService 构造环境服务。
func NewEnvService(st store.Store, bus *eventbus.Bus, engine *iam.Engine,
	lib *fingerprint.Library, baseline *fingerprint.BaselineLibrary) *EnvService {
	return &EnvService{st: st, bus: bus, engine: engine, lib: lib, baseline: baseline,
		log: slog.Default()}
}

// CreateEnvInput 创建环境入参。
type CreateEnvInput struct {
	ShopID   string
	Country  string // 目标站点国家（决定画像时区/语言与代理出口国）
	OSFamily string // Windows|macOS|Linux，空表示不限
	DeviceID string
	// StartURL 启动后自动打开的首个页面（0012）。为空则 about:blank。
	StartURL string
	// ProxyID 手工静态代理（0012）。为空表示走代理商池 / 直连，由启动链路决定。
	ProxyID  string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// CreateEnv 创建店铺环境：采样画像 → 自洽性体检 → 落库（含基线哈希）→ 事件。
func (s *EnvService) CreateEnv(ctx context.Context, in CreateEnvInput) (*types.Environment, *fingerprint.InjectionPlan, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:create", Path: "/shops"})
	if !d.Allow {
		return nil, nil, apierr.Forbidden("无权创建店铺环境（" + string(d.Layer) + "）")
	}
	// 数据级收口：own 范围下只能为本人（或被委托）店铺建环境
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	sp, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, in.Actor, sp, "为他人店铺创建环境"); err != nil {
		return nil, nil, err
	}
	env, plan, report, err := s.buildEnv(in)
	if err != nil {
		return nil, nil, err
	}
	alignNote := strings.Join(report.Notes, "；")
	now := time.Now().UTC()
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Environments().Create(tctx, env); err != nil {
			return err
		}
		// L4：审计只记画像 ID 与基线哈希，不记任何凭证；L8：基线哈希是离线校验的唯一凭据
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.created", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.create", Result: "success"},
			Data: map[string]any{
				"env_id": env.ID, "profile_id": env.ProfileID,
				"baseline_hash": env.BaselineHash, "kernel_major": env.KernelMajor,
				"consistency_score": report.Score,
				// 内核没对齐到本机版本时把原因写进审计：
				// 「为什么这个店的 Chrome 是 120」不能只存在于日志里。
				"kernel_note": alignNote,
			},
		})
	}); err != nil {
		return nil, nil, err
	}
	return env, plan, nil
}

// buildEnv 采样画像 → 自洽性体检 → 生成注入计划（**不落库**）。
//
// 与落库分开是为了让「建店铺 + 建环境」能在同一个事务里完成（M2 聚合创建接口）：
// 采样与体检是纯计算，放在事务里只会拉长持锁时间。
func (s *EnvService) buildEnv(in CreateEnvInput) (*types.Environment, *fingerprint.InjectionPlan, fingerprint.Report, error) {
	profile, err := s.lib.Sample(in.Country, in.OSFamily)
	if err != nil {
		return nil, nil, fingerprint.Report{}, apierr.Conflict(err.Error())
	}
	major, err := profile.ChromeMajor()
	if err != nil {
		return nil, nil, fingerprint.Report{}, apierr.Internal("画像 UA 无法解析内核版本")
	}
	// 内核版本对齐链（P1）：UA / UA-CH / TLS 握手必须同出一源，且不得高于本机内核。
	//
	// uTLS 基线滞后于 Chrome 正式版（v1.6.7 无 HelloChrome_131，131 复用 HelloChrome_120_PQ，
	// 即「发出的握手是 120」）。若 UA 仍保持 131，平台交叉比对 UA 与 JA4 立刻识破——
	// 这是致命不自洽。处理原则：**宁可用低版本但完全自洽的组合，也不要高版本混搭**。
	align := fingerprint.AlignKernel(profile, s.baseline, s.kernelMajor)
	tls := align.Baseline
	if align.Major > 0 {
		major = align.Major
	}
	if align.Note != "" && s.log != nil {
		s.log.Info("内核版本已对齐（UA / UA-CH / TLS 同源）",
			"local_major", align.LocalMajor, "kernel_major", major, "detail", align.Note)
	}

	// 自洽性体检：创建环境时就要体检，不合格直接拒绝（M0.3）
	report := fingerprint.NewEngine().Evaluate(profile, fingerprint.CheckContext{
		IPCountry: in.Country, ProxyCountry: in.Country, KernelMajor: major, TLS: tls,
	})
	// 内核没对齐到本机版本时，原因要跟着自检报告一起落库：
	// 运营在自检页看到「Chrome 120」时必须同时看到为什么是 120。
	if align.Note != "" {
		report.Notes = append(report.Notes, align.Note)
	}
	if len(report.Issues) > 0 && report.Score < report.Threshold {
		return nil, nil, fingerprint.Report{}, apierr.Conflict("画像自洽性体检未通过（score=" + itoa(report.Score) + "），已拒绝创建")
	}
	rawReport, err := json.Marshal(report)
	if err != nil {
		return nil, nil, fingerprint.Report{}, apierr.Internal("自洽性报告序列化失败")
	}
	plan, err := fingerprint.BuildInjectionPlan(ids.New(), profile, tls)
	if err != nil {
		return nil, nil, fingerprint.Report{}, apierr.Internal(err.Error())
	}

	now := time.Now().UTC()
	env := &types.Environment{
		ID: plan.EnvID, TenantID: in.Actor.TenantID, ShopID: in.ShopID, DeviceID: in.DeviceID,
		ProfileID: profile.ID, BaselineHash: plan.BaselineHash, KernelMajor: major,
		Status: EnvStatusCreated, Snapshot: fingerprint.Capture(profile).Values,
		Consistency: rawReport, InjectScript: plan.Script, CreatedAt: now, UpdatedAt: now,
		// 0012 新增列：国家 / 起始 URL / 手工代理在建环境时就定下来，
		// 这样「列表按国家筛」与「启动后打开哪个页面」不依赖 M3 的启动链路。
		Country:  strings.ToUpper(strings.TrimSpace(in.Country)),
		StartURL: strings.TrimSpace(in.StartURL),
		ProxyID:  in.ProxyID,
	}
	if tls != nil {
		env.TLSHelloID = tls.HelloID
	}
	return env, plan, report, nil
}

// 环境状态取值。
//
// EnvStatusRunning 必须由"关窗口"这条上报路径收回（StopEnv）：启动把它写成 running
// 之后如果没有回收路径，运营看到的就是一屏永远在跑的环境——窗口其实早关了，
// 代理端口与内存也没释放。「正在用」和「上次启动过」必须分得开，
// 否则环境列表本身就不可信（也就没法拿它判断"这个店铺现在有没有人在操作"）。
const (
	EnvStatusCreated = "created"
	EnvStatusRunning = "running"
	EnvStatusIdle    = "idle"
	// EnvStatusStopped 停用：环境退出服务（不参与启动），是 created/idle 与删除之间的中间态。
	// 与 running→idle 的「关窗口」不同——停用是管理动作，会释放出口绑定。
	EnvStatusStopped = "stopped"
)

// LaunchSpec 环境启动单（M3 起为**内部**启动单：由浏览器管理器消费，不下发给客户端）。
//
// 红线：
//   - 注入脚本、UA、时区、TLS 基线全部由服务端生成，客户端随机化 = 与画像库脱钩（L2 致命项）；
//   - 不返回任何凭证（L4）：Upstream 的 Password 永不参与序列化；
//   - 本地代理端口由**浏览器管理器动态分配**（方案 A），不再有静态端口池。
//
// M3 变更点（T1/T8）：
//   - 去掉 ProxyPort 与 proxyPortFor 的 20000-39999 静态哈希区间（已废弃）；
//   - Upstream.Password 标为 `json:"-"`：旧 launch 响应不再含上游口令。
type LaunchSpec struct {
	EnvID        string            `json:"env_id"`
	ShopID       string            `json:"shop_id"`
	ShopName     string            `json:"shop_name,omitempty"`
	BaselineHash string            `json:"baseline_hash"`
	KernelMajor  int               `json:"kernel_major"`
	UserAgent    string            `json:"user_agent"`
	TLSHelloID   string            `json:"tls_hello_id,omitempty"`
	Country      string            `json:"country,omitempty"`
	Script       string            `json:"script"`
	ScriptBytes  int               `json:"script_bytes"`
	OpenURL      string            `json:"open_url"`
	Snapshot     map[string]string `json:"snapshot,omitempty"`
	// StartURL 环境配置的首个页面（M3 启动后自动打开；为空则 about:blank）。
	StartURL string `json:"start_url,omitempty"`
	// ExitMode 出口模式：pool（代理商池）/ manual（手工代理）/ direct（直连）。
	// 前端与日志都按它显示「代理状态」，不再靠猜。
	ExitMode string `json:"exit_mode"`
	// Upstream **本环境专属**的出口代理凭据。
	// 为什么必须逐环境下发：同一客户端进程里若所有窗口共用一组 ZINIAO_UPSTREAM_*
	// 环境变量，就等于所有店铺共用同一个出口 IP——平台据此可直接把账号串起来。
	// 凭据不经审计、不进 argv，只由浏览器管理器注入本地 TLSProxy 的上游配置（Go 内存）。
	Upstream *UpstreamCredential `json:"upstream,omitempty"`
}

// UpstreamCredential 出口代理凭据。
//
// Password 只存在于 Go 进程内存：它不进 argv、不进日志、不进审计 detail，
// **也不进任何 HTTP 响应**（`json:"-"`，M3/T8）。
type UpstreamCredential struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"-"`
	Country  string `json:"country,omitempty"`
	City     string `json:"city,omitempty"`
}

// Addr 返回 host:port（仅用于告警文案；注意不含口令）。
func (c UpstreamCredential) Addr() string {
	return c.Host + ":" + strconv.Itoa(c.Port)
}

// Launch 下发环境启动单（真实浏览器窗口的能力源）。
//
// 裁决仍在服务端（L3）：无 env:launch 权限直接拒绝；启动写审计但不含凭证。
func (s *EnvService) Launch(ctx context.Context, in LaunchInput) (*LaunchSpec, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:launch", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权启动店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	// 数据级收口：own 范围下只能启动本人（或被委托）店铺的环境
	sp, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, in.Actor, sp, "启动他人店铺的环境"); err != nil {
		return nil, err
	}
	env, err := s.st.Environments().GetByShop(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	// 停用的环境不参与启动：把状态拦在这里而不是让窗口开出来之后才发现指纹库已下线。
	if env.Status == EnvStatusStopped {
		return nil, apierr.Conflict("环境已停用，请先启用再启动")
	}
	script := env.InjectScript
	// 兼容历史环境：建环境时尚未落脚本，或落的是加固前的旧版脚本
	// （缺 P1 的「Date 时区串 / Intl locale」覆盖）。不重建的话老环境永远吃不到修复，
	// 运营看到的就是「明明升级了、两个环境还是一模一样」。
	if script == "" || !fingerprint.ScriptHasVersion(script, fingerprint.ScriptVersion) {
		rebuilt := rebuildScript(env)
		if rebuilt != "" {
			script = rebuilt
		}
	}
	// 注入运行时标记：实测页/自检用（基线哈希是离线启动校验的唯一凭据，L8）
	script += "\n;window.__ZINIAO_BASELINE__=" + strconv.Quote(env.BaselineHash) + ";"
	shopName := sp.Name
	// 出口 IP 分配：**失败必须拒绝启动，不能退回直连**。
	// 直连意味着所有店铺共用本机这一个公网 IP，比共用代理商 IP 还糟；
	// 宁可让这个环境起不来，也不能把账号暴露在关联风险里。
	var upstream *UpstreamCredential
	var rotated bool
	var prevAddr string
	// exitMode：手工代理优先（M2 绑定了静态出口就绝不能偷偷退回代理商池——
	// 那会让「这个店走固定出口」的承诺悄悄失效，而运营看不出来）。
	exitMode := "direct"
	switch {
	case env.ProxyID != "":
		up, err := s.manualUpstream(tctx, env.ProxyID)
		if err != nil {
			return nil, err
		}
		// P3：启动前出口拨测。
		//
		// 放在阶段 1 是有意为之——此刻本机一个资源都还没建（没有端口、没有
		// Chrome、没有 CDP），失败时**没有任何东西需要逆序回滚**，直接返回
		// 结构化错误即可；环境状态由调用方走既有的 MarkLaunchFailed 复位，
		// 不会留下悬空 running。
		//
		// 失败**绝不退回直连**：直连意味着这个店铺拿本机公网 IP 出门，
		// 比用一条不通的代理更糟（同一机器上所有店铺共用出口 = 直接关联）。
		if perr := s.checkEgressBeforeLaunch(ctx, tctx, env, up); perr != nil {
			return nil, perr
		}
		upstream, exitMode = up, "manual"
	case s.pool != nil:
		country := env.Snapshot["country"]
		// 必须带租户上下文：出口绑定要按租户落库（缺租户时分配器直接拒绝，
		// 不会降级成内存绑定——那等于重启即换 IP）。
		res, allocErr := s.pool.AllocateEx(tctx, env.ID, network.Region{
			Country: country, ASNType: network.ASNResidential,
		})
		if allocErr != nil {
			return nil, apierr.Internal("出口 IP 分配失败: " + allocErr.Error())
		}
		rotated, prevAddr = res.Rotated, res.PrevAddr
		p := res.Proxy
		upstream = &UpstreamCredential{
			Protocol: string(p.Protocol),
			Host:     p.Host,
			Port:     p.Port,
			Username: p.Username,
			// 口令只随本次启动单下发，不进审计、不落库（egress_bindings 里只有
			// 资源标识；重启后由分配器向代理商按 ID 取回凭据）。
			Password: p.Password,
			Country:  p.Country,
			City:     p.City,
		}
	}
	spec := &LaunchSpec{
		EnvID:        env.ID,
		ShopID:       env.ShopID,
		ShopName:     shopName,
		BaselineHash: env.BaselineHash,
		KernelMajor:  env.KernelMajor,
		UserAgent:    env.Snapshot["ua"],
		TLSHelloID:   env.TLSHelloID,
		Country:      env.Snapshot["country"],
		Script:       script,
		ScriptBytes:  len(script),
		OpenURL:      in.OpenURL,
		Snapshot:     env.Snapshot,
		StartURL:     env.StartURL,
		Upstream:     upstream,
		ExitMode:     exitMode,
	}
	// 这里**不写 running**（M3/T1）。
	//
	// 原因只有一条：绝不在数据库事务里等浏览器。CDP 连通要等十几秒，把它塞进事务
	// 等于用一次浏览器卡顿拖垮整库（mem 驱动是全局串行锁、PG 是长事务持锁）。
	// 所以 running 的**唯一提交点**是 CDP 连通成功之后的 MarkRunning；
	// 启动失败走 MarkLaunchFailed 复位。在此之前数据库里保持原状，
	// 任何时刻都不会出现「status=running 但本机没有进程」的悬空态。
	//
	// 出口 IP 变了：必须让运营看见。同一个店铺账号从新 IP 登录，
	// 在平台眼里是典型的账号风险信号；静默更换等于把运营蒙在鼓里。
	// 注意：告警失败不应阻断启动（环境本身是可用的），只写事件不抛错。
	if rotated && s.sec != nil {
		_ = s.sec.Raise(ctx, security.RaiseInput{
			TenantID: in.Actor.TenantID,
			Category: "risk",
			Level:    "high",
			ShopID:   in.ShopID,
			Action:   "env.exit_ip_rotated",
			Actor:    &in.Actor,
			Detail: map[string]any{
				"env_id":       env.ID,
				"prev_exit":    prevAddr,
				"current_exit": upstream.Addr(),
				"country":      upstream.Country,
				"reason":       "资源到期或代理商重新指派",
			},
			Terminal: in.Terminal,
		})
	}
	return spec, nil
}

// StopInput 关闭环境入参。
type StopInput struct {
	ShopID   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// StopEnv 关闭环境（客户端关窗口 / 进程退出时上报），把状态收回 idle。
//
// 幂等是硬要求而不是顺手做的：客户端做不到"恰好上报一次"——关窗口报一次、
// 进程退出还会再报一次，网络失败重投也可能再来一次。已经不是 running 就原样返回，
// 不报错也不重复写审计；重复审计会让"今天启动/关闭了几次"这种问题没法回答。
//
// 这里**不动出口绑定**：egress_bindings 的意义正是"重启不换 IP"，
// 关一次窗口就把 IP 换掉，等于把一个稳定的店铺出口变成每次都变的风险信号。
func (s *EnvService) StopEnv(ctx context.Context, in StopInput) (*types.Environment, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:stop", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权关闭店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	// 数据级收口：own 范围下只能关闭本人（或被委托）店铺的环境。
	// 判定放在幂等早退**之前**——无权就是无权，与窗口当前是不是 running 无关。
	sp, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, in.Actor, sp, "关闭他人店铺的环境"); err != nil {
		return nil, err
	}
	env, err := s.st.Environments().GetByShop(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if env == nil {
		return nil, apierr.NotFound("该店铺没有环境")
	}
	// created 也一起收：没启动过的环境点了「停止」，留在「已创建」会让运营以为
	// 还没生效；它与关窗口同义——现在没人在用。stopped / idle 一律原样返回。
	if env.Status != EnvStatusRunning && env.Status != EnvStatusCreated {
		return env, nil
	}
	now := time.Now().UTC()
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		env.Status = EnvStatusIdle
		env.UpdatedAt = now
		if err := tx.Environments().Update(tctx, env); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.stopped", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.stop", Result: "success"},
			Data:     map[string]any{"env_id": env.ID, "kernel_major": env.KernelMajor},
		})
	}); err != nil {
		return nil, err
	}
	return env, nil
}

// Deactivate 停用环境：退出服务 + 释放出口绑定（Allocator.Release 的第二个触发点）。
//
// 为什么停用要归还出口而 StopEnv 不还：出口绑定存在的意义是「重启不换 IP」——
// 但停用是环境退出服务，没有"下一次启动"可言（再启用就是新的生命周期）。
// 占着不还等于线路继续按量计费 + 占着代理商名额（新环境申请不到独占 IP，
// 表现为莫名的 ErrNoExclusiveResource）。
//
// 顺序是先归还出口再改状态（与设备退役同口径）：反过来，归还失败会留下一个
// 「已停用但线路仍在计费」的环境，且没有任何提示。归还失败时整体失败：
// 环境还是可用的，用户可以重试；静默停用成功反而把漏掉的线路藏起来了。
//
// 幂等：已停用的环境原样返回，不重复归还、不重复写审计。
func (s *EnvService) Deactivate(ctx context.Context, in StopInput) (*types.Environment, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:deactivate", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权停用店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	sp, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, in.Actor, sp, "停用他人店铺的环境"); err != nil {
		return nil, err
	}
	env, err := s.st.Environments().GetByShop(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if env == nil {
		return nil, apierr.NotFound("该店铺没有环境")
	}
	if env.Status == EnvStatusStopped {
		return env, nil // 幂等
	}
	if env.Status == EnvStatusRunning {
		return nil, apierr.Conflict("环境正在运行，请先关闭窗口再停用")
	}
	if s.pool != nil {
		if err := s.pool.Release(tctx, env.ID); err != nil {
			return nil, apierr.Internal("出口归还失败，已中止停用（请重试）: " + err.Error())
		}
	}
	now := time.Now().UTC()
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		env.Status = EnvStatusStopped
		env.UpdatedAt = now
		if err := tx.Environments().Update(tctx, env); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.deactivated", OccurredAt: now, Risk: eventbus.RiskMedium,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.deactivate", Result: "success"},
			Data:     map[string]any{"env_id": env.ID, "egress_released": s.pool != nil},
		})
	}); err != nil {
		return nil, err
	}
	return env, nil
}

// Activate 重新启用已停用的环境（stopped → idle）。低风险：只是恢复可用，
// 下一次启动会重新分配出口（可能换 IP，那条告警由启动链路负责）。
func (s *EnvService) Activate(ctx context.Context, in StopInput) (*types.Environment, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:deactivate", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权启用店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	sp, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, in.Actor, sp, "启用他人店铺的环境"); err != nil {
		return nil, err
	}
	env, err := s.st.Environments().GetByShop(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if env == nil {
		return nil, apierr.NotFound("该店铺没有环境")
	}
	if env.Status != EnvStatusStopped {
		return env, nil // 幂等：非停用状态原样返回
	}
	now := time.Now().UTC()
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		env.Status = EnvStatusIdle
		env.UpdatedAt = now
		if err := tx.Environments().Update(tctx, env); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.activated", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.activate", Result: "success"},
			Data:     map[string]any{"env_id": env.ID},
		})
	}); err != nil {
		return nil, err
	}
	return env, nil
}

// DeleteEnv 删除环境（终态）：释放出口 + 物理删除行 + 落审计。
//
// 与停用的差别：停用可逆（Activate 回来），删除不可逆。running 的环境必须先关闭
// ——窗口还开着就把行删了，客户端的关闭上报会落空，代理进程也没人管。
//
// profile 在客户端本机（profiles/<env_id>），服务端删不到：env.deleted 事件带
// env_id 下发，客户端据此清理本地 profile（尽力而为，不阻塞删除）。
//
// 归还失败只告警不阻断：删除本身不可逆，卡住状态机会让库里堆满删不掉的环境；
// 「资源可能仍在计费」由 warnEgress 同源思路留痕（出口历史 + 日志）。
func (s *EnvService) DeleteEnv(ctx context.Context, in StopInput) error {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:delete", Path: "/shops"})
	if !d.Allow {
		return apierr.Forbidden("无权删除店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	sp, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return err
	}
	if err := checkShopAccess(s.st, tctx, d, in.Actor, sp, "删除他人店铺的环境"); err != nil {
		return err
	}
	env, err := s.st.Environments().GetByShop(tctx, in.ShopID)
	if err != nil {
		return err
	}
	if env == nil {
		return apierr.NotFound("该店铺没有环境")
	}
	if env.Status == EnvStatusRunning {
		return apierr.Conflict("环境正在运行，请先关闭窗口再删除")
	}
	if s.pool != nil {
		// 复用分配器归还：解绑 + 向代理商退还资源 + 写 released 历史（幂等）。
		// 失败只告警：环境行已经决定要删了，把「没还掉的线路」留痕让人去查。
		if err := s.pool.Release(tctx, env.ID); err != nil {
			s.logReleaseFailure(env.ID, err)
		}
	}
	now := time.Now().UTC()
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Environments().Delete(tctx, env.ID); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.deleted", OccurredAt: now, Risk: eventbus.RiskHigh,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.delete", Result: "success"},
			Data: map[string]any{
				"env_id": env.ID, "kernel_major": env.KernelMajor,
				"baseline_hash": env.BaselineHash, "egress_released": s.pool != nil,
			},
		})
	})
}

// logReleaseFailure 出口归还失败的留痕：删除/停用已成功，但资源可能仍在计费。
func (s *EnvService) logReleaseFailure(envID string, err error) {
	if s.sec != nil {
		_ = s.sec.Raise(context.Background(), security.RaiseInput{
			Category: "risk", Level: "medium", Action: "env.egress_release_failed",
			Detail:   map[string]any{"env_id": envID, "reason": err.Error()},
			Terminal: eventbus.Terminal{Mode: "job", ClientVer: "server/1"},
		})
	}
}

// ReclaimIdle 回收卡在 running 的环境（客户端崩溃 / 断电，来不及上报关闭）。
//
// 这是**启发式兜底**而非精确判定，判据只有"多久没人动过它"。误判的代价是可控的：
// 只把状态显示成空闲，不删 profile、不放出口绑定——正在开着的窗口不会被搞坏，
// 下次启动会重新置 running。精确的关闭始终由 StopEnv 完成。
func (s *EnvService) ReclaimIdle(ctx context.Context, tenantID string, now time.Time, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	tctx := store.WithTenant(ctx, tenantID)
	list, _, err := s.st.Environments().List(tctx, 500, 0)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range list {
		env := list[i]
		if env.Status != EnvStatusRunning || now.Sub(env.UpdatedAt) < maxAge {
			continue
		}
		if err := s.st.WithTx(tctx, func(tx store.Tx) error {
			env.Status = EnvStatusIdle
			env.UpdatedAt = now
			if err := tx.Environments().Update(tctx, &env); err != nil {
				return err
			}
			return s.bus.Emit(tctx, tx, eventbus.Event{
				TenantID: tenantID, Type: "env.reclaimed", OccurredAt: now,
				Subject: eventbus.Subject{ShopID: env.ShopID},
				Context: eventbus.ContextInfo{URL: "/jobs", Action: "env.reclaim", Result: "success"},
				Data:    map[string]any{"env_id": env.ID, "idle_hours": int(maxAge.Hours())},
			})
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ThumbnailInput 记录窗口缩略图入参（M3/T10）。
type ThumbnailInput struct {
	EnvID string
	// Path 缩略图落盘路径（data/ 下）。
	Path  string
	Actor iam.Subject
}

// SaveThumbnail 记录停止前的窗口缩略图路径。
//
// 缩略图不是业务数据（丢了不影响任何功能），所以失败只降级、不报错：
// 关窗口才是主流程，绝不能因为写不进一行路径就把环境卡在 running。
func (s *EnvService) SaveThumbnail(ctx context.Context, in ThumbnailInput) (*types.Environment, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:launch", Path: "/environments"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权更新店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	env, err := s.st.Environments().Get(tctx, in.EnvID)
	if err != nil {
		return nil, err
	}
	if env == nil {
		return nil, apierr.NotFound("环境不存在")
	}
	env.ThumbnailPath = in.Path
	env.UpdatedAt = time.Now().UTC()
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		return tx.Environments().Update(tctx, env)
	}); err != nil {
		return nil, err
	}
	return env, nil
}

// manualUpstream 读取手工代理行并解密出口口令，组装成本环境专属的出口凭据。
//
// 明文口令只在 Go 内存里活到「注入本地代理」那一刻：不进 argv、不进审计、
// 不进 LaunchSpec 的 JSON（UpstreamCredential.Password 是 `json:"-"`）。
// 没有装配信封密钥时宁可失败——把密文当明文发出去会得到一个「看起来能连、
// 实则认证失败」的代理，那比直接拒绝启动更难排查。
func (s *EnvService) manualUpstream(ctx context.Context, proxyID string) (*UpstreamCredential, error) {
	p, err := s.st.Proxies().Get(ctx, proxyID)
	if err != nil || p == nil {
		return nil, apierr.NotFound("手工代理不存在或已删除，请重新绑定出口")
	}
	enc, err := s.st.Proxies().PasswordEnc(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	password := ""
	if enc != "" {
		if s.cipher == nil {
			return nil, apierr.Internal("手工代理口令信封未装配，无法解密出口凭据")
		}
		plaintext, err := s.cipher.Open(enc)
		if err != nil {
			return nil, apierr.Internal("手工代理口令解密失败（信封可能已被替换）")
		}
		password = string(plaintext)
	}
	return &UpstreamCredential{
		Protocol: p.Protocol, Host: p.Host, Port: p.Port,
		Username: p.Username, Password: password, Country: p.Country,
	}, nil
}

// ---------- P3：启动前出口拨测（阶段 1） ----------

// checkEgressBeforeLaunch 启动绑定了手工代理的环境之前，先验证这条上游真的通。
//
// 口径（P3 拍板）：
//   - ZINIAO_PROXY_TEST=on（默认）：必须拨测 ok；fail 直接返回错误阻断启动，
//     结论（含 last_error）落回台账，运营在代理管理页就能看到原因；
//   - off：跳过拨测与健康校验，只在日志与安全事件里留 WARN，**允许启动**。
//
// 阻断而不是降级：带一条坏代理起环境，表现是「窗口开着、页面全白」，
// 运营会以为是站点问题；在这里明确拒绝，反而省掉一轮排障。
func (s *EnvService) checkEgressBeforeLaunch(ctx, tctx context.Context,
	env *types.Environment, up *UpstreamCredential) error {
	if !s.probe.Enabled {
		s.warnEgressProbeSkipped(ctx, env, up)
		return nil
	}
	p, err := s.st.Proxies().Get(tctx, env.ProxyID)
	if err != nil {
		return err
	}
	if p == nil {
		return apierr.NotFound("手工代理不存在或已删除，请重新绑定出口")
	}
	res := probeProxy(tctx, s.st, s.cipher, p, proxyProbeInput{
		Target:        s.probe.Target,
		CountryTarget: s.probe.CountryTarget,
		TimeoutMS:     s.probe.TimeoutMS,
	})
	if res == nil {
		// 只在「读不到凭据」这种仓储级故障下发生：那不是代理不通，
		// 不能伪装成拨测失败写进台账。
		return apierr.Internal("出口代理凭据读取失败，无法验证出口")
	}
	persistProxyCheck(tctx, s.st, s.log, p.ID, res)
	if !res.OK {
		reason := res.Error
		if reason == "" {
			reason = "未知原因"
		}
		// 只记不含凭据的结论文本：up.Addr() 是 host:port，res.Error 已过 trimErr。
		s.log.Warn("启动前出口拨测未通过，已阻断启动",
			"env", env.ID, "proxy", p.ID, "exit", up.Addr(), "reason", reason)
		return apierr.Conflict("出口代理拨测未通过（" + reason + "），已阻断启动；请检查该代理或重新绑定出口")
	}
	s.log.Info("启动前出口拨测通过", "env", env.ID, "proxy", p.ID,
		"exit_ip", res.ExitIP, "country", res.ExitCountry, "latency_ms", res.ElapsedMS)
	s.warnCountryMismatch(ctx, env, p, res)
	return nil
}

// warnCountryMismatch 出口国家与环境指纹国家不一致的软校验（只 WARN，不阻断）。
//
// 不强制的理由很实在：转运、跨区测试、临时借线都是合法场景，硬拦会误杀；
// 而「环境 US 却从 JP 出门」这件事对账号风险的影响，运营看见就够了。
func (s *EnvService) warnCountryMismatch(ctx context.Context, env *types.Environment,
	p *types.Proxy, res *ProxyTestResult) {
	envCountry := strings.ToUpper(strings.TrimSpace(env.Country))
	if envCountry == "" {
		// 权威国家在快照里（Country 列只是为了列表筛选，见 types.Environment 注释）。
		envCountry = strings.ToUpper(strings.TrimSpace(env.Snapshot["country"]))
	}
	exit := strings.ToUpper(strings.TrimSpace(res.ExitCountry))
	if envCountry == "" || exit == "" || envCountry == exit {
		return
	}
	s.log.Warn("出口国家与环境指纹国家不一致", "env", env.ID, "proxy", p.ID,
		"env_country", envCountry, "exit_country", exit, "measured", res.CountryMeasured)
	if s.sec == nil {
		return
	}
	_ = s.sec.Raise(ctx, security.RaiseInput{
		TenantID: env.TenantID, Category: "risk", Level: "low",
		ShopID: env.ShopID, Action: "env.exit_country_mismatch",
		Detail: map[string]any{
			"env_id": env.ID, "proxy_id": p.ID,
			"env_country": envCountry, "exit_country": exit,
			"exit_ip": res.ExitIP, "measured": res.CountryMeasured,
		},
		Terminal: eventbus.Terminal{Mode: "job", ClientVer: "server/1"},
	})
}

// warnEgressProbeSkipped TEST=off 下跳过出口拨测的留痕（P3）。
//
// 这条 WARN 的意义是让「出口未经验证」成为可追溯的事实，而不是悄悄放行：
// 事后查「这个店当时从哪个 IP 出去的」时，它解释了为什么台账上没有任何实测值。
func (s *EnvService) warnEgressProbeSkipped(ctx context.Context,
	env *types.Environment, up *UpstreamCredential) {
	s.log.Warn("ZINIAO_PROXY_TEST=off：跳过启动前出口拨测，代理可用性未验证",
		"env", env.ID, "proxy", env.ProxyID, "exit", up.Addr())
	if s.sec == nil {
		return
	}
	_ = s.sec.Raise(ctx, security.RaiseInput{
		TenantID: env.TenantID, Category: "risk", Level: "low",
		ShopID: env.ShopID, Action: "env.egress_probe_skipped",
		Detail: map[string]any{
			"env_id": env.ID, "proxy_id": env.ProxyID,
			"exit": up.Addr(), "reason": "ZINIAO_PROXY_TEST=off",
		},
		Terminal: eventbus.Terminal{Mode: "job", ClientVer: "server/1"},
	})
}

// ResetStaleRunning 对账复位（M3/T4）：把「库里还写着 running、本机却已没有进程」
// 的环境收回 idle。
//
// 为什么必须做：启动器崩溃后，窗口没了但库里还是 running。前端据此一直显示
// 「运行中」，运营点「停止」也停不到任何东西——最后只能改数据库。
// 复位只针对 running，其它状态一律不动（created/idle/stopped 各有各的语义）。
//
// 事件写 env.reconciled 且不带 actor：这是本机事实的纠正，不是某个人做的操作。
func (s *EnvService) ResetStaleRunning(ctx context.Context, tenantIDs []string, keep map[string]bool) (int, error) {
	if len(tenantIDs) == 0 {
		return 0, nil
	}
	n := 0
	now := time.Now().UTC()
	for _, tenantID := range tenantIDs {
		if tenantID == "" {
			continue
		}
		tctx := store.WithTenant(ctx, tenantID)
		envs, _, err := s.st.Environments().Search(tctx, store.EnvironmentFilter{
			Status: EnvStatusRunning, Limit: 500,
		})
		if err != nil {
			continue // 单租户失败不影响其它租户：对账是尽力而为
		}
		for i := range envs {
			env := envs[i]
			if keep[env.ID] {
				continue
			}
			// 条件置位（只认 running → idle）：与启动失败复位同一条独立语句，
			// 不整行写回。对账常在服务刚起来时跑，这时整行 UPDATE 若因某列
			// 出问题，残留 running 就永远清不掉——那正是要求 7 要消灭的东西。
			changed, err := s.st.Environments().SetStatusIfIn(tctx, env.ID,
				[]string{EnvStatusRunning}, EnvStatusIdle)
			if err != nil {
				s.log.Error("对账：残留 running 复位失败", "env", env.ID, "err", err.Error())
				continue
			}
			if !changed {
				continue // 状态已经不是 running（有人抢先处理了），不重复计数
			}
			n++
			// 事件留痕失败只记：状态已经收回，事件缺失不影响可用性。
			if eerr := s.st.WithTx(tctx, func(tx store.Tx) error {
				return s.bus.Emit(tctx, tx, eventbus.Event{
					TenantID: tenantID, Type: "env.reconciled", OccurredAt: now,
					Subject: eventbus.Subject{ShopID: env.ShopID},
					Context: eventbus.ContextInfo{Action: "env.reconcile", Result: "success"},
					Data: map[string]any{
						"env_id": env.ID, "from": EnvStatusRunning, "to": EnvStatusIdle,
					},
				})
			}); eerr != nil {
				s.log.Error("对账事件写入失败（状态已复位）", "env", env.ID, "err", eerr.Error())
			}
		}
	}
	return n, nil
}

// ---------- M3/T1：running 的唯一提交点 ----------

// MarkRunningInput 启动成功后的状态提交入参。
type MarkRunningInput struct {
	EnvID    string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
	// StartedAt 写入 last_started_at；零值时取当前时间。
	StartedAt time.Time
}

// MarkRunning 提交 running 状态（**唯一提交点**：Chrome 起来 + CDP 连通之后）。
//
// 为什么必须单独一个方法而不是在 Launch 里写：Launch 到这一步之间要等浏览器十几秒，
// 事务里等待会把一次浏览器卡顿放大成整库抖动。把它拆出来后，事务里只剩毫秒级写入。
//
// 幂等：已经是 running 就只补 last_started_at 并原样返回——对账重挂、重复提交
// 都不该把一次成功启动记成两次。
func (s *EnvService) MarkRunning(ctx context.Context, in MarkRunningInput) (*types.Environment, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:launch", Path: "/environments"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权启动店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	env, err := s.st.Environments().Get(tctx, in.EnvID)
	if err != nil {
		return nil, err
	}
	if env == nil {
		return nil, apierr.NotFound("环境不存在")
	}
	now := time.Now().UTC()
	started := in.StartedAt
	if started.IsZero() {
		started = now
	}
	// 已经在运行且启动时刻已记录 = 一次提交的重放（对账重挂 / 重试），
	// 连写都不写：刷新 updated_at 会让对账误判成「又启动了一次」，
	// 而覆盖 last_started_at 会丢掉本次启动的真实时刻。
	if env.Status == EnvStatusRunning && env.LastStartedAt != nil {
		return env, nil
	}
	emit := env.Status != EnvStatusRunning
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		env.Status = EnvStatusRunning
		if env.LastStartedAt == nil {
			env.LastStartedAt = &started
		}
		env.UpdatedAt = now
		if err := tx.Environments().Update(tctx, env); err != nil {
			return err
		}
		if !emit {
			return nil
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.launched", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: env.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.start", Result: "success"},
			Data: map[string]any{
				"env_id": env.ID, "kernel_major": env.KernelMajor,
				"baseline_hash": env.BaselineHash,
			},
		})
	}); err != nil {
		return nil, err
	}
	return env, nil
}

// MarkLaunchFailedInput 启动失败复位入参。
type MarkLaunchFailedInput struct {
	EnvID    string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
	// Stage 失败发生在哪个阶段（1-8），Reason 失败原因（不含任何凭据）。
	Stage  string
	Reason string
}

// MarkLaunchFailed 启动失败后的状态复位。
//
// 铁律：**绝不覆盖 running**。只有 created/idle 才复位——
// 如果这一步的错误发生在「其实已经跑起来了」之后（例如 MarkRunning 事务超时但
// 环境已经是 running），把它改回 idle 会留下「库里说没跑、窗口却开着」的相反悬空态。
// 那比原样留着更糟：运营会以为启动失败而再点一次，撞上「已在本机运行」。
func (s *EnvService) MarkLaunchFailed(ctx context.Context, in MarkLaunchFailedInput) (*types.Environment, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:launch", Path: "/environments"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权启动店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	env, err := s.st.Environments().Get(tctx, in.EnvID)
	if err != nil {
		return nil, err
	}
	if env == nil {
		return nil, apierr.NotFound("环境不存在")
	}
	now := time.Now().UTC()

	// ---------- 第一步：复位状态（独立事务，只写 status 一列）----------
	//
	// 走 SetStatusIfIn 而不是整行 Update，理由有两条：
	//   1. 复位是止损动作，它的成功只能依赖「状态」这一列。整行 UPDATE 涉及 18 列，
	//      任一列写错（M3 阶段 8 就是 inject_script 列名与实参没配对）都会让主提交
	//      与复位**同时**崩，环境就悬在 running 上没人收；
	//   2. 「绝不覆盖 running」由 SQL 的 status = ANY(...) 谓词保证，而不是靠
	//      几百毫秒前读出来的那个状态——读到的值可能已经过期。
	changed, err := s.st.Environments().SetStatusIfIn(tctx, in.EnvID,
		[]string{EnvStatusCreated, EnvStatusIdle}, EnvStatusIdle)
	if err != nil {
		// 复位失败也要如实上报：调用方（browser.failStart）靠返回值记
		// 「启动失败复位未生效（状态可能滞留）」这条告警，吞掉就等于没有告警。
		//
		// 但**不改变本方法的返回语义**：启动失败这个结论是确定的，复位只是善后。
		return env, err
	}
	if !changed {
		// 状态不在 created/idle（多半已经是 running）：原样返回，不发失败事件。
		return env, nil
	}
	env.Status = EnvStatusIdle
	env.UpdatedAt = now

	// ---------- 第二步：事件留痕（失败只记不回滚）----------
	//
	// 状态已经复位成功了，事件写不进去绝不能把状态一起回滚掉——
	// 那会让一次「复位成功」变成「复位失败」，环境又悬在中间态。
	if eerr := s.st.WithTx(tctx, func(tx store.Tx) error {
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.launch_failed", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: env.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.start", Result: "failed"},
			// 只有 env_id / stage / reason：**不含**出口地址、端口、账号、口令。
			Data: map[string]any{
				"env_id": env.ID, "stage": in.Stage, "reason": in.Reason,
			},
		})
	}); eerr != nil {
		s.log.Error("启动失败事件写入失败（状态已复位，不影响可用性）",
			"env", env.ID, "stage", in.Stage, "err", eerr.Error())
	}
	return env, nil
}

// rebuildScript 历史环境补脚本（建环境时尚未落脚本 / 旧库漏写 inject_script 列）。
//
// 必须补出**完整可执行的注入脚本**，而不是只落一个标记对象：
// 标记不覆盖任何 navigator 属性，页面里读到的仍是本机真实指纹，
// 两个环境对比就会全同——这正是实测中「只有 UA 不同」的原因。
func rebuildScript(env *types.Environment) string {
	if env == nil {
		return ""
	}
	script := fingerprint.ScriptFromSnapshot(env.ProfileID, env.Snapshot)
	// 快照里既没有 UA、profile_id 又反查不到模板时，重建出来的是「把
	// navigator.userAgent 写成空串」的脚本——那比不注入更糟（页面直接报错）。
	// 这时返回空串，调用方保留原脚本。
	if !strings.Contains(script, `"ua":"Mozilla/`) {
		return ""
	}
	return script
}

// LaunchInput 启动环境入参。
type LaunchInput struct {
	ShopID   string
	OpenURL  string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// CheckReport 环境自检报告（M0.3 环境自检页）。
type CheckReport struct {
	EnvID           string                  `json:"env_id"`
	BaselineHash    string                  `json:"baseline_hash"`
	Consistency     fingerprint.Report      `json:"consistency"`
	Drift           fingerprint.DriftReport `json:"drift"`
	BaselineChecked bool                    `json:"baseline_checked"`
	BaselineMatch   bool                    `json:"baseline_match"`
	BaselineNote    string                  `json:"baseline_note,omitempty"`
	TLSMatch        bool                    `json:"tls_match"`
	Score           int                     `json:"score"`
	Passed          bool                    `json:"passed"`
}

// Check 环境自检：自洽性 + 漂移 + 基线校验（离线启动前必做，L8）。
//
// observed 为客户端上报的观测值：
//   - 与快照同名的键参与漂移检测；
//   - baseline_hash：离线启动校验用的基线哈希；
//   - tls_ja3：实际 TLS JA3，与内核基线比对。
func (s *EnvService) Check(ctx context.Context, actor iam.Subject, shopID string, observed map[string]string) (*CheckReport, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "env:check", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权自检店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	// 数据级收口：own 范围下只能自检本人（或被委托）店铺的环境
	sp, err := s.st.Shops().Get(tctx, shopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, actor, sp, "自检他人店铺的环境"); err != nil {
		return nil, err
	}
	env, err := s.st.Environments().GetByShop(tctx, shopID)
	if err != nil {
		return nil, err
	}
	if observed == nil {
		observed = map[string]string{}
	}
	out := &CheckReport{EnvID: env.ID, BaselineHash: env.BaselineHash}
	if len(env.Consistency) > 0 {
		_ = json.Unmarshal(env.Consistency, &out.Consistency)
	}
	// 漂移检测：观测值 vs 创建时基线快照
	out.Drift = fingerprint.DetectDrift(fingerprint.Snapshot{Values: env.Snapshot}, observed)

	// 离线启动校验（L8）：客户端必须上报与创建时一致的基线哈希
	if reported, ok := observed["baseline_hash"]; ok && reported != "" {
		out.BaselineChecked = true
		out.BaselineMatch = reported == env.BaselineHash
		if !out.BaselineMatch {
			out.BaselineNote = "基线哈希与服务端不一致，禁止离线启动"
		}
	} else {
		out.BaselineNote = "未上报基线哈希（离线启动前必须上报）"
	}
	// TLS 指纹校验：实际 JA3 必须与内核基线一致（TLS 与声称版本脱钩是致命项）
	out.TLSMatch = true
	if ja3, ok := observed["tls_ja3"]; ok && ja3 != "" {
		if bl, ok2 := s.baseline.For(env.KernelMajor); ok2 && bl.JA3 != "" && bl.JA3 != ja3 {
			out.TLSMatch = false
			out.BaselineNote = "TLS JA3 与内核基线不一致"
		}
	}

	score := out.Consistency.Score - (100-out.Drift.Score)/2
	if out.BaselineChecked && !out.BaselineMatch {
		score -= 20
	}
	if !out.TLSMatch {
		score -= 20
	}
	if score < 0 {
		score = 0
	}
	out.Score = score
	out.Passed = score >= 80 && out.Drift.Risk != "high" && out.TLSMatch &&
		(!out.BaselineChecked || out.BaselineMatch)
	return out, nil
}

// Get 取环境（按店铺）。数据级收口：own 范围下只能查看本人（或被委托）店铺的环境。
func (s *EnvService) Get(ctx context.Context, actor iam.Subject, shopID string) (*types.Environment, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "env:check", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权查看店铺环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	sp, err := s.st.Shops().Get(tctx, shopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, actor, sp, "查看他人店铺的环境"); err != nil {
		return nil, err
	}
	return s.st.Environments().GetByShop(tctx, shopID)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	if neg {
		out = "-" + out
	}
	return out
}
