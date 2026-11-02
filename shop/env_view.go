package shop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ziniao/billing"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ---------- M2：环境聚合视图 ----------

// SetBilling 接入配额与计量（M2 聚合创建接口要同时建店铺，必须走同一套配额与计量）。
func (s *EnvService) SetBilling(ent *billing.Entitlement, meter *billing.Meter) {
	s.ent, s.meter = ent, meter
}

// SetProfileRoot 设置 profile 根目录（data/profiles）。
//
// 建环境时只在这里落「profile 元数据」，不拉起 Chrome（M2 边界）：
// M3 启动链路读同一份元数据，保证「建环境」与「起浏览器」看到的是同一个目录。
func (s *EnvService) SetProfileRoot(dir string) { s.profileRoot = strings.TrimSpace(dir) }

// EnvironmentView 环境聚合视图（M2 店铺网格页 / 设备台账只读视图）。
//
// 刻意**不含**：注入脚本正文（只给 script_bytes）、上游代理商凭据、
// 本机 PID / CDP 端口 / 进程运行时长（后三项属 M3 进程态，不在库里）。
// 排障页面会被截图发到群里，少给一个字段就少一条外泄路径。
type EnvironmentView struct {
	ID            string     `json:"id"`
	ShopID        string     `json:"shop_id"`
	ShopName      string     `json:"shop_name,omitempty"`
	Code          string     `json:"code,omitempty"`
	Platform      string     `json:"platform,omitempty"`
	Site          string     `json:"site,omitempty"`
	GroupID       string     `json:"group_id,omitempty"`
	GroupName     string     `json:"group_name,omitempty"`
	Status        string     `json:"status"`
	Country       string     `json:"country,omitempty"`
	KernelMajor   int        `json:"kernel_major"`
	TLSHelloID    string     `json:"tls_hello_id,omitempty"`
	BaselineHash  string     `json:"baseline_hash"`
	ScriptBytes   int        `json:"script_bytes"`
	StartURL      string     `json:"start_url,omitempty"`
	ExitMode      string     `json:"exit_mode"`
	ProxyID       string     `json:"proxy_id,omitempty"`
	ProxyName     string     `json:"proxy_name,omitempty"`
	ProxyStatus   string     `json:"proxy_status,omitempty"`
	// 以下 3 项由 P3 引入：环境详情/工作台要能直接看到「这个店现在从哪出门」。
	// 它们来自代理台账最近一次拨测，不是本次启动的实测值——展示时必须说清
	// 是「最近一次实测」而不是「此刻」，否则排查问题时会误导。
	ProxyCheckStatus string     `json:"proxy_check_status,omitempty"`
	ProxyExitIP      string     `json:"proxy_exit_ip,omitempty"`
	ProxyExitCountry string     `json:"proxy_exit_country,omitempty"`
	DeviceID         string     `json:"device_id,omitempty"`
	LastStartedAt *time.Time `json:"last_started_at,omitempty"`
	ThumbnailPath string     `json:"thumbnail_path,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// EnvListFilter 环境聚合列表过滤（网关查询参数的落地形态）。
type EnvListFilter struct {
	GroupID  string
	Platform string
	Status   string
	Country  string
	Keyword  string
	Limit    int
	Offset   int
}

// exitModeOf 出口模式：手工代理 > 代理商池 > 直连。
//
// 直连也走本地 TLSProxy（批复方案 A），只是没有上游；这里只表达「出口从哪来」。
func (s *EnvService) exitModeOf(env *types.Environment) string {
	if env.ProxyID != "" {
		return "manual"
	}
	if s.pool != nil {
		return "pool"
	}
	return "direct"
}

// viewOf 把环境与它的店铺/代理拼成聚合视图。
//
// 店铺逐个 Get 而不是先拉全量再建 map：店铺规模可能大于一页环境数，
// 按页取永远是准的；一页最多 200 条，多几次主键查询比「名字取不到」划算。
func (s *EnvService) viewOf(tctx context.Context, env *types.Environment, proxyCache map[string]*types.Proxy) EnvironmentView {
	v := EnvironmentView{
		ID: env.ID, ShopID: env.ShopID, Status: env.Status, Country: env.Country,
		KernelMajor: env.KernelMajor, TLSHelloID: env.TLSHelloID, BaselineHash: env.BaselineHash,
		ScriptBytes: len(env.InjectScript), StartURL: env.StartURL,
		ExitMode: s.exitModeOf(env), ProxyID: env.ProxyID, DeviceID: env.DeviceID,
		LastStartedAt: env.LastStartedAt, ThumbnailPath: env.ThumbnailPath,
		CreatedAt: env.CreatedAt, UpdatedAt: env.UpdatedAt,
	}
	if sp, err := s.st.Shops().Get(tctx, env.ShopID); err == nil && sp != nil {
		v.ShopName = sp.Name
		v.Code = sp.Code
		v.Platform = sp.Platform
		v.Site = sp.Site
		v.GroupID = sp.GroupID
		v.GroupName = sp.Group
	}
	if env.ProxyID != "" {
		p, ok := proxyCache[env.ProxyID]
		if !ok {
			p, _ = s.st.Proxies().Get(tctx, env.ProxyID)
			proxyCache[env.ProxyID] = p
		}
		if p != nil {
			v.ProxyName = p.Name
			v.ProxyStatus = p.Status
			v.ProxyCheckStatus = checkStatusOf(p)
			v.ProxyExitIP = p.ExitIP
			v.ProxyExitCountry = p.ExitCountry
		}
	}
	return v
}

// ListViews 环境聚合列表（跨店铺，受数据范围约束）。
func (s *EnvService) ListViews(ctx context.Context, actor iam.Subject, f EnvListFilter) ([]EnvironmentView, int, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "env:read", Path: "/environments"})
	if !d.Allow {
		return nil, 0, apierr.Forbidden("无权查看环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	// 数据级收口：own 范围只放行本人（或被委托）店铺的环境，且**下推到仓储**
	// ——先查后筛会让第 N 页的条数与总数对不上。
	var allowed []string
	if d.DataScope == "own" {
		shops, _, err := s.st.Shops().List(tctx, store.ShopFilter{
			OwnerID: actor.UserID, Scope: "own", DelegatedFor: actor.UserID, Limit: 1000,
		})
		if err != nil {
			return nil, 0, err
		}
		allowed = make([]string, 0, len(shops))
		for _, sp := range shops {
			allowed = append(allowed, sp.ID)
		}
	}
	limit, offset := f.Limit, f.Offset
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	envs, total, err := s.st.Environments().Search(tctx, store.EnvironmentFilter{
		ShopIDs: allowed, GroupID: f.GroupID, Platform: f.Platform, Status: f.Status,
		Country: f.Country, Keyword: f.Keyword, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	cache := map[string]*types.Proxy{}
	out := make([]EnvironmentView, 0, len(envs))
	for i := range envs {
		out = append(out, s.viewOf(tctx, &envs[i], cache))
	}
	return out, total, nil
}

// GetView 单条环境聚合视图（含数据级收口）。
func (s *EnvService) GetView(ctx context.Context, actor iam.Subject, envID string) (*EnvironmentView, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "env:read", Path: "/environments"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权查看环境（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	env, err := s.st.Environments().Get(tctx, envID)
	if err != nil {
		return nil, err
	}
	sp, err := s.st.Shops().Get(tctx, env.ShopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, actor, sp, "查看他人店铺的环境"); err != nil {
		return nil, err
	}
	v := s.viewOf(tctx, env, map[string]*types.Proxy{})
	return &v, nil
}

// ---------- M2：新建环境聚合接口（店铺 + 环境 + profile 元数据，同一事务） ----------

// CreateShopEnvInput 一次建好「店铺 + 环境」的入参。
type CreateShopEnvInput struct {
	Name     string
	Platform string
	Site     string
	Code     string // 运营自定义编号（如 AMZ-001）
	GroupID  string
	Country  string
	OSFamily string
	StartURL string
	ProxyID  string
	DeviceID string
	Status   string // 店铺生命周期状态，空 = 注册中
	// FingerprintMode 指纹模式。本期只支持 auto（服务端采样画像）；
	// 传其它值直接 400——「自定义指纹」是后面里程碑的事，现在放行等于埋一个
	// 客户端自行随机化的口子（L2 致命项）。
	FingerprintMode string
	Actor           iam.Subject
	Terminal        eventbus.Terminal
	URL             string
}

// ProfileMeta profile 初始化元数据（M2 只落盘，不拉起 Chrome）。
//
// 落这份文件的意义：M3 启动链路据此知道「这个 profile 属于哪个环境、基线是哪个」，
// 换机器/重装后也能把 data/profiles 与库里的环境对上。
type ProfileMeta struct {
	EnvID        string    `json:"env_id"`
	ShopID       string    `json:"shop_id"`
	KernelMajor  int       `json:"kernel_major"`
	BaselineHash string    `json:"baseline_hash"`
	Country      string    `json:"country,omitempty"`
	StartURL     string    `json:"start_url,omitempty"`
	ProfileDir   string    `json:"profile_dir"`
	CreatedAt    time.Time `json:"created_at"`
}

// CreateShopEnvResult 聚合创建结果。
type CreateShopEnvResult struct {
	Shop        *types.Shop        `json:"shop"`
	Environment *types.Environment `json:"environment"`
	View        EnvironmentView    `json:"view"`
	// Profile 为 nil 表示未装配启动器内核（profileRoot 为空），本期允许——
	// 建环境本身已经成功，profile 目录会在 M3 首次启动时创建。
	Profile *ProfileMeta `json:"profile,omitempty"`
}

// CreateShopEnv 原子创建店铺 + 环境 + profile 元数据。
//
// 为什么要一个接口而不是「先建店再建环境」两次调用：两次调用之间失败会留下
// 一个没有环境的店铺，运营看到的是「店铺建了但打不开」，而列表里没有任何提示。
// 配额校验、计量、两条审计都在同一个事务里，要么全成要么全回滚。
//
// 本期**不拉起 Chrome**（M2 边界：只写库与初始化元数据）。
func (s *EnvService) CreateShopEnv(ctx context.Context, in CreateShopEnvInput) (*CreateShopEnvResult, error) {
	for _, act := range []string{"shop:create", "env:create"} {
		d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: act, Path: "/environments"})
		if !d.Allow {
			return nil, apierr.Forbidden("无权新建店铺环境（" + string(d.Layer) + "）")
		}
	}
	if mode := strings.TrimSpace(in.FingerprintMode); mode != "" && mode != "auto" {
		return nil, apierr.Validation("本期指纹模式只支持 auto（服务端采样画像）")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, apierr.Validation("店铺名称需为 1-64 个字符")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if s.ent != nil {
		if err := s.ent.CheckQuota(ctx, in.Actor.TenantID, billing.MetricShopCount, 1); err != nil {
			return nil, err
		}
	}
	// 分组与代理都必须属于本租户（Get 走 RLS + tenant_id 谓词，跨租户即 404）。
	groupID, groupName := "", ""
	if in.GroupID != "" {
		g, err := s.st.Groups().Get(tctx, in.GroupID)
		if err != nil {
			return nil, err
		}
		groupID, groupName = g.ID, g.Name
	}
	if in.ProxyID != "" {
		p, err := s.st.Proxies().Get(tctx, in.ProxyID)
		if err != nil {
			return nil, err
		}
		if p.Status != "active" {
			return nil, apierr.Validation("该代理已停用，不能绑定到新环境")
		}
	}
	env, _, report, err := s.buildEnv(CreateEnvInput{
		ShopID: "", Country: in.Country, OSFamily: in.OSFamily,
		StartURL: in.StartURL, ProxyID: in.ProxyID, Actor: in.Actor,
		Terminal: in.Terminal, URL: in.URL,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "注册中"
	}
	sp := &types.Shop{
		ID: ids.New(), TenantID: in.Actor.TenantID, Name: name,
		Platform: strings.TrimSpace(in.Platform), Site: strings.TrimSpace(in.Site),
		Status: status, Group: groupName, GroupID: groupID,
		Code: strings.TrimSpace(in.Code), DeviceID: strings.TrimSpace(in.DeviceID),
		OwnerID: in.Actor.UserID, CreatedAt: now, UpdatedAt: now,
	}
	env.ShopID = sp.ID
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Shops().Create(tctx, sp); err != nil {
			return err
		}
		if s.meter != nil {
			if _, err := s.meter.Record(tctx, tx, in.Actor.TenantID, billing.MetricShopCount, 1); err != nil {
				return err
			}
		}
		if err := s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: eventbus.TypeShopCreated, OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: sp.ID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.create", Result: "success"},
			Data: map[string]any{
				"shop_id": sp.ID, "name": sp.Name, "platform": sp.Platform,
				"site": sp.Site, "group_id": sp.GroupID, "code": sp.Code,
			},
		}); err != nil {
			return err
		}
		if err := tx.Environments().Create(tctx, env); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.created", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: sp.ID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.create", Result: "success"},
			Data: map[string]any{
				"env_id": env.ID, "profile_id": env.ProfileID,
				"baseline_hash": env.BaselineHash, "kernel_major": env.KernelMajor,
				"consistency_score": report.Score, "proxy_id": env.ProxyID,
			},
		})
	}); err != nil {
		return nil, err
	}
	out := &CreateShopEnvResult{Shop: sp, Environment: env}
	// profile 元数据在事务提交之后落盘：文件系统不参与数据库事务，
	// 放在事务里会出现「库里回滚了、目录却留下了」的孤儿 profile。
	meta, err := s.initProfileMeta(env)
	if err != nil {
		return nil, err
	}
	out.Profile = meta
	out.View = s.viewOf(tctx, env, map[string]*types.Proxy{})
	return out, nil
}

// initProfileMeta 初始化 profile 目录与元数据文件（M2 只落盘，不拉起浏览器）。
func (s *EnvService) initProfileMeta(env *types.Environment) (*ProfileMeta, error) {
	if s.profileRoot == "" {
		return nil, nil
	}
	dir := filepath.Join(s.profileRoot, env.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, apierr.Internal("环境 profile 目录初始化失败: " + err.Error())
	}
	meta := ProfileMeta{
		EnvID: env.ID, ShopID: env.ShopID, KernelMajor: env.KernelMajor,
		BaselineHash: env.BaselineHash, Country: env.Country, StartURL: env.StartURL,
		ProfileDir: dir, CreatedAt: time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, apierr.Internal("profile 元数据序列化失败")
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), raw, 0o600); err != nil {
		return nil, apierr.Internal("profile 元数据写入失败: " + err.Error())
	}
	return &meta, nil
}

// ---------- M2：环境绑定 / 解绑手工代理 ----------

// BindProxyInput 环境绑定手工代理入参（ProxyID 为空 = 解绑）。
type BindProxyInput struct {
	EnvID    string
	ProxyID  string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// BindProxyResult 绑定结果。
type BindProxyResult struct {
	EnvID    string `json:"env_id"`
	ShopID   string `json:"shop_id"`
	ProxyID  string `json:"proxy_id,omitempty"`
	ExitMode string `json:"exit_mode"`
	// Warnings 软校验告警（P3）。**不阻断**绑定，只是把运营该知道的事说出来：
	// 出口国家与指纹国家不一致、同一条代理被多个环境共用等。
	// 做成 []string 而不是布尔集合，是因为这些话最终要原样显示给人看。
	Warnings []string `json:"warnings,omitempty"`
}

// BindProxy 绑定 / 解绑手工代理。
//
// 解绑不是「删除出口」：它把环境交还给代理商池（或直接本机出口），
// 下一次启动会重新分配出口——可能换 IP，那条告警由启动链路的 exit_ip_rotated 负责。
// 事件里只有 proxy_id，**不含任何凭据**（凭据不进审计是 L4 的硬要求）。
func (s *EnvService) BindProxy(ctx context.Context, in BindProxyInput) (*BindProxyResult, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "env:update", Path: "/environments"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权变更环境出口（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	env, err := s.st.Environments().Get(tctx, in.EnvID)
	if err != nil {
		return nil, err
	}
	sp, err := s.st.Shops().Get(tctx, env.ShopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, in.Actor, sp, "变更他人店铺的环境出口"); err != nil {
		return nil, err
	}
	proxyID := strings.TrimSpace(in.ProxyID)
	var warnings []string
	var probeSkipped bool
	var countryMismatch bool
	var boundProxy *types.Proxy
	if proxyID != "" {
		p, perr := s.st.Proxies().Get(tctx, proxyID)
		if perr != nil {
			return nil, perr // 404：代理不存在或不属于本租户
		}
		if p.Status != "active" {
			return nil, apierr.Validation("该代理已停用，请先启用再绑定")
		}
		boundProxy = p
		// P3：拨测开关打开时才要求「拨测必须 ok」。
		//
		// off 时不强制（否则内网部署永远绑不上），但仍会在审计里留
		// probe_skipped=true —— 「没验证过」和「验证过且失败」必须能区分开。
		if s.probe.Enabled {
			if p.CheckStatus != types.ProxyCheckOK {
				return nil, apierr.Validation(bindCheckError(p))
			}
		} else {
			probeSkipped = true
		}
		warnings = s.bindWarnings(tctx, env, p)
		for _, w := range warnings {
			if strings.Contains(w, "国家") {
				countryMismatch = true
			}
		}
	}
	// 运行中的环境不允许换出口：窗口里的连接已经走旧出口，
	// 中途换等于把「同一个会话两个 IP」送到平台眼前。
	if env.Status == EnvStatusRunning {
		return nil, apierr.Conflict("环境正在运行，请先关闭窗口再变更出口")
	}
	now := time.Now().UTC()
	env.ProxyID = proxyID
	env.UpdatedAt = now
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Environments().Update(tctx, env); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.proxy_bound", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: env.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.proxy.bind", Result: "success"},
			// L4：只有 id 与健康结论，**没有任何凭据**。
			// probe_skipped / country_mismatch 留在 Data 里，是为了让「绑的
			// 时候到底验证过没有」成为可追溯的事实，而不是靠人回忆。
			Data: map[string]any{
				"env_id": env.ID, "proxy_id": proxyID, "bound": proxyID != "",
				"check_status":     checkStatusOf(boundProxy),
				"probe_skipped":    probeSkipped,
				"country_mismatch": countryMismatch,
				"warnings":         len(warnings),
			},
		})
	}); err != nil {
		return nil, err
	}
	return &BindProxyResult{
		EnvID: env.ID, ShopID: env.ShopID, ProxyID: proxyID,
		ExitMode: s.exitModeOf(env), Warnings: warnings,
	}, nil
}

// bindCheckError 未通过拨测的绑定拒绝文案（P3）。
//
// unknown 与 fail 分开说：前者是「从没测过」，后者是「测过且不通」，
// 运营要做的动作完全不同（先点拨测 vs 改地址/口令/换线）。
func bindCheckError(p *types.Proxy) string {
	switch p.CheckStatus {
	case types.ProxyCheckFail:
		return "该代理最近一次拨测未通过" + proxyLastErrorSuffix(p) + "，请修正后重新拨测再绑定"
	default:
		return "该代理尚未通过拨测（check_status=" + checkStatusOf(p) + "），请先在代理管理页拨测成功再绑定"
	}
}

func proxyLastErrorSuffix(p *types.Proxy) string {
	if p.LastError == "" {
		return ""
	}
	return "（" + p.LastError + "）"
}

func checkStatusOf(p *types.Proxy) string {
	if p == nil {
		return ""
	}
	if p.CheckStatus == "" {
		return types.ProxyCheckUnknown
	}
	return p.CheckStatus
}

// bindWarnings 绑定前的软校验（P3）：只产出告警，不阻断。
//
// 两条告警指向同一个风险——「平台看到的东西和运营以为的不一样」：
//   - 一号多环境：同一出口 IP 下挂多个店铺，是最直接的关联信号；
//   - 国家不一致：US 画像从 JP 出门，语言/时区/地理全都对不上。
//
// 不强制的理由：转运、跨区测试、临时借线都是合法场景，硬拦会误杀。
func (s *EnvService) bindWarnings(ctx context.Context, env *types.Environment, p *types.Proxy) []string {
	out := []string{}
	// 一号一 IP：本次绑定尚未落库，要把「本环境自己」从计数里扣掉。
	if n, err := s.st.Proxies().CountBound(ctx, p.ID); err == nil {
		shared := n
		if env.ProxyID == p.ID {
			shared--
		}
		if shared > 0 {
			msg := "该代理还被 " + itoa(shared) + " 个其它环境绑定"
			if running, rerr := s.st.Proxies().CountBoundRunning(ctx, p.ID); rerr == nil && running > 0 {
				msg += "（其中 " + itoa(running) + " 个正在运行）"
			}
			out = append(out, msg+"：一号一 IP 场景下存在账号关联风险")
		}
	}
	// 国家一致性：实测值优先，没有实测就退回台账录入值。
	envCountry := strings.ToUpper(strings.TrimSpace(env.Country))
	if envCountry == "" {
		envCountry = strings.ToUpper(strings.TrimSpace(env.Snapshot["country"]))
	}
	proxyCountry := strings.ToUpper(strings.TrimSpace(p.ExitCountry))
	if proxyCountry == "" {
		proxyCountry = strings.ToUpper(strings.TrimSpace(p.Country))
	}
	if envCountry != "" && proxyCountry != "" && envCountry != proxyCountry {
		out = append(out, "出口国家（"+proxyCountry+"）与环境指纹国家（"+envCountry+"）不一致")
	}
	return out
}

// ---------- M2：设备与网络只读视图（只给数据库侧事实） ----------

// RuntimeLedgerItem 环境运行台账条目（纯库侧，不含任何本机进程态）。
//
// 与 M3 的 /environments/runtime 的差别：后者带 pid / cdp_port / 运行时长，
// 读的是本机注册表；这一份只回答「库里登记了什么」——M2 不展示进程态，
// 因为那时还没有进程（不拉起 Chrome），显示了就是假数据。
type RuntimeLedgerItem struct {
	EnvID         string     `json:"env_id"`
	ShopID        string     `json:"shop_id"`
	ShopName      string     `json:"shop_name,omitempty"`
	Status        string     `json:"status"`
	Country       string     `json:"country,omitempty"`
	KernelMajor   int        `json:"kernel_major"`
	TLSHelloID    string     `json:"tls_hello_id,omitempty"`
	BaselineHash  string     `json:"baseline_hash"`
	ExitMode      string     `json:"exit_mode"`
	ProxyID       string     `json:"proxy_id,omitempty"`
	ProxyName     string     `json:"proxy_name,omitempty"`
	ProxyProtocol string     `json:"proxy_protocol,omitempty"`
	ProxyAddr     string     `json:"proxy_addr,omitempty"`
	ProxyUsername string     `json:"proxy_username,omitempty"`
	ProxyHasPwd   bool       `json:"proxy_has_password"`
	DeviceID      string     `json:"device_id,omitempty"`
	LastStartedAt *time.Time `json:"last_started_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// RuntimeLedger 环境运行台账（M2 设备与网络只读视图）。
func (s *EnvService) RuntimeLedger(ctx context.Context, actor iam.Subject, limit, offset int) ([]RuntimeLedgerItem, int, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "env:read", Path: "/environments"})
	if !d.Allow {
		return nil, 0, apierr.Forbidden("无权查看环境运行台账（" + string(d.Layer) + "）")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	var allowed []string
	if d.DataScope == "own" {
		shops, _, err := s.st.Shops().List(tctx, store.ShopFilter{
			OwnerID: actor.UserID, Scope: "own", DelegatedFor: actor.UserID, Limit: 1000,
		})
		if err != nil {
			return nil, 0, err
		}
		for _, sp := range shops {
			allowed = append(allowed, sp.ID)
		}
	}
	envs, total, err := s.st.Environments().Search(tctx, store.EnvironmentFilter{
		ShopIDs: allowed, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	cache := map[string]*types.Proxy{}
	out := make([]RuntimeLedgerItem, 0, len(envs))
	for i := range envs {
		env := envs[i]
		item := RuntimeLedgerItem{
			EnvID: env.ID, ShopID: env.ShopID, Status: env.Status, Country: env.Country,
			KernelMajor: env.KernelMajor, TLSHelloID: env.TLSHelloID, BaselineHash: env.BaselineHash,
			ExitMode: s.exitModeOf(&env), ProxyID: env.ProxyID, DeviceID: env.DeviceID,
			LastStartedAt: env.LastStartedAt, UpdatedAt: env.UpdatedAt,
		}
		if sp, serr := s.st.Shops().Get(tctx, env.ShopID); serr == nil && sp != nil {
			item.ShopName = sp.Name
		}
		if env.ProxyID != "" {
			p, ok := cache[env.ProxyID]
			if !ok {
				p, _ = s.st.Proxies().Get(tctx, env.ProxyID)
				cache[env.ProxyID] = p
			}
			if p != nil {
				item.ProxyName = p.Name
				item.ProxyProtocol = p.Protocol
				item.ProxyAddr = p.Host + ":" + itoa(p.Port)
				item.ProxyUsername = p.Username
				// 只有 has_password 布尔：口令永不出现在任何响应里。
				item.ProxyHasPwd = p.PasswordEnc != ""
			}
		}
		out = append(out, item)
	}
	return out, total, nil
}
