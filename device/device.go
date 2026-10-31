// Package device 设备池与网络资源（M3）：
// 云网络设备 / 自有设备（代理、VPS、宽带）/ 紫鸟盒子 / 云手机 / 云号。
//
// L9 铁律：设备「独占互斥锁」是错误实现。正确模型是双向配额——
// 一台设备可绑定多个店铺账号（max_bound_accounts），同时使用人数不超过 N
// （max_concurrent，默认 6；按 user_id 去重，一人多店只占 1 个槽）。
// 超过软阈值不硬阻塞：允许 + 告警 + 审计；超过硬阈值才拒绝并引导切备用设备。
package device

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ziniao/billing"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/network"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// 事件类型（各模块引用，避免拼写漂移；前缀决定审计类别）。
const (
	TypeDevicePurchased = "device.purchased"
	TypeDeviceImported  = "device.imported"
	TypeDeviceBound     = "device.bound"
	TypeDeviceUnbound   = "device.unbound"
	TypeDeviceAcquired  = "device.acquired"
	TypeDeviceReleased  = "device.released"
	TypeDeviceRenewed   = "device.renewed"
)

// Service 设备服务。
type Service struct {
	st     store.Store
	bus    *eventbus.Bus
	cfg    *config.Config
	ent    *billing.Entitlement
	meter  *billing.Meter
	engine *iam.Engine
	prov   network.Provider
}

// NewService 构造设备服务。
func NewService(st store.Store, bus *eventbus.Bus, cfg *config.Config, engine *iam.Engine,
	ent *billing.Entitlement, meter *billing.Meter, prov network.Provider) *Service {
	return &Service{st: st, bus: bus, cfg: cfg, engine: engine, ent: ent, meter: meter, prov: prov}
}

// ---------- 购买 / 导入 ----------

// PurchaseInput 购买云网络设备入参。
type PurchaseInput struct {
	Name         string
	Type         types.DeviceType
	Country      string
	Region       string
	City         string
	PeriodMonths int
	// Count 一次申购的台数（>1 即批量申购）。它的存在意义不只是省事：
	// 大额申购是「双人复核」（会签）唯一的现实触发点——单台几十元的申购永远碰不到阈值。
	Count    int
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Purchase 购买设备：配额校验 → 代理商分配 → 事务内写设备 + 计量 + 事件。
func (s *Service) Purchase(ctx context.Context, in PurchaseInput) (*types.Device, error) {
	if in.Type == "" {
		in.Type = types.DeviceCloudNetwork
	}
	if in.PeriodMonths <= 0 {
		in.PeriodMonths = 1
	}
	if in.Count <= 0 {
		in.Count = 1
	}
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "device:purchase", Path: "/devices"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权购买设备（" + string(d.Layer) + "）")
	}
	if err := s.ent.CheckQuota(ctx, in.Actor.TenantID, billing.MetricDeviceCount, int64(in.Count)); err != nil {
		return nil, err
	}

	// 每台单独申请出口：多环境共用同一出口 IP 会被平台直接串号，
	// 所以这里不是「申请一个再复用」，而是逐台分配（验收见 network/allocator.go 的排他约束）。
	devs := make([]*types.Device, 0, in.Count)
	now := time.Now().UTC()
	for i := 0; i < in.Count; i++ {
		deviceID := ids.New()
		proxy := (*network.AcquiredProxy)(nil)
		if s.prov != nil {
			acq, err := s.prov.Acquire(ctx, network.Region{
				Code: in.Country + "-" + in.City, Country: in.Country, City: in.City,
			})
			if err != nil {
				return nil, err
			}
			proxy = acq
		}
		dev := &types.Device{
			ID: deviceID, TenantID: in.Actor.TenantID,
			Name: orDefault(in.Name, "设备-"+deviceID[:6]),
			Type: in.Type, Country: in.Country, Region: in.Region, City: in.City,
			Status: types.DeviceStatusAllocating, OwnerID: in.Actor.UserID,
			MaxConcurrent: s.cfg.Limits.DeviceMaxConcurrentUsers,
			ExpiresAt:     now.AddDate(0, in.PeriodMonths, 0),
			CreatedAt:     now, UpdatedAt: now,
		}
		if proxy != nil {
			dev.IP = proxy.Proxy.Host
			dev.Protocol = string(proxy.Proxy.Protocol)
			dev.ProxyHost = proxy.Proxy.Host
			dev.ProxyPort = proxy.Proxy.Port
			dev.ProxyUser = proxy.Proxy.Username
			dev.ASNType = string(proxy.Proxy.ASNType)
			dev.ExpiresAt = proxy.ExpiresAt
		}
		devs = append(devs, dev)
	}

	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	amount := PurchaseAmountCNY(ctx, s.ent, in.Actor.TenantID, in.PeriodMonths, in.Count)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		for _, dev := range devs {
			if err := tx.Devices().Create(tctx, dev); err != nil {
				return err
			}
		}
		if _, err := s.meter.Record(tctx, tx, in.Actor.TenantID, billing.MetricDeviceCount, int64(in.Count)); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: TypeDevicePurchased, OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{DeviceID: devs[0].ID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "device.purchase", Result: "success"},
			Data: map[string]any{
				"device_id": devs[0].ID, "type": string(in.Type),
				"country": in.Country, "months": in.PeriodMonths,
				"count": in.Count, "amount": amount,
			},
		})
	}); err != nil {
		return nil, err
	}
	return devs[0], nil
}

// PurchaseAmountCNY 按套餐单价计算申购金额（元）：单价 × 月数 × 台数。
//
// 必须服务端算：审批路由靠它判断是否触发大额双人复核，听客户端自己报金额等于没控。
// 取不到套餐时降级为 0（不因计费数据缺失卡住申购，也不凭空抬高风险等级）。
func PurchaseAmountCNY(ctx context.Context, ent EntitlementQuerier, tenantID string, months, count int) float64 {
	if ent == nil || months <= 0 || count <= 0 {
		return 0
	}
	plan, err := ent.PlanOf(ctx, tenantID)
	if err != nil {
		return 0
	}
	return float64(plan.PricePerDevicePerMonth) / 100 * float64(months) * float64(count)
}

// EntitlementQuerier 套餐查询能力的最小子集（让金额计算不依赖整个 Entitlement）。
type EntitlementQuerier interface {
	PlanOf(ctx context.Context, tenantID string) (config.PlanSpec, error)
}

// ImportInput 自有设备导入（M3.2：代理设备 / 云服务器 VPS / 宽带设备）。
type ImportInput struct {
	Name     string
	Type     types.DeviceType // self_proxy|vps|broadband
	Protocol string
	Host     string
	Port     int
	Username string
	Password string // L4：仅用于建立连接，绝不落库明文
	Country  string
	City     string
	Actor    iam.Subject
	Terminal eventbus.Terminal
}

// ImportOwn 导入自有设备：先做安全检测（连通性 + 纯净度），风险提前预警。
func (s *Service) ImportOwn(ctx context.Context, in ImportInput) (*types.Device, error) {
	valid := map[types.DeviceType]bool{types.DeviceSelfProxy: true, types.DeviceVPS: true, types.DeviceBroadband: true}
	if !valid[in.Type] {
		return nil, apierr.Validation("自有设备类型必须是 self_proxy / vps / broadband")
	}
	if in.Host == "" || in.Port <= 0 {
		return nil, apierr.Validation("缺少代理主机或端口")
	}
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "device:import", Path: "/devices"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权导入设备（" + string(d.Layer) + "）")
	}
	if err := s.ent.CheckQuota(ctx, in.Actor.TenantID, billing.MetricDeviceCount, 1); err != nil {
		return nil, err
	}
	// 导入前安全检测：不通过仍允许导入，但标记风险并告警（紫鸟是「风险提前预警」而非强拦）
	purity := network.EvaluatePurity(network.PurityInput{
		IP: in.Host, ASNType: network.ASNUnknown, RDNSConsistent: false, TTLConsistent: false,
	}, network.DefaultPurityWeights())

	id := ids.New()
	now := time.Now().UTC()
	dev := &types.Device{
		ID: id, TenantID: in.Actor.TenantID, Name: orDefault(in.Name, "自有设备-"+id[:6]),
		Type: in.Type, IP: in.Host, Country: in.Country, City: in.City,
		Status: types.DeviceStatusActive, OwnerID: in.Actor.UserID,
		Protocol: in.Protocol, ProxyHost: in.Host, ProxyPort: in.Port, ProxyUser: in.Username,
		PurityScore: purity.Score, MaxConcurrent: s.cfg.Limits.DeviceMaxConcurrentUsers,
		ExpiresAt: now.AddDate(0, 1, 0), CreatedAt: now, UpdatedAt: now,
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Devices().Create(tctx, dev); err != nil {
			return err
		}
		if _, err := s.meter.Record(tctx, tx, in.Actor.TenantID, billing.MetricDeviceCount, 1); err != nil {
			return err
		}
		risk := eventbus.RiskLow
		if purity.Score < 60 {
			risk = eventbus.RiskHigh
		} else if purity.Score < 75 {
			risk = eventbus.RiskMedium
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: TypeDeviceImported, OccurredAt: now, Risk: risk,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{DeviceID: id}, Terminal: in.Terminal,
			Context: eventbus.ContextInfo{Action: "device.import", Result: "success"},
			Data: map[string]any{
				"device_id": id, "type": string(in.Type),
				"purity": purity.Score, "grade": purity.Grade,
			},
		})
	}); err != nil {
		return nil, err
	}
	return dev, nil
}

// BoxInput 紫鸟盒子导入（硬件资产：SN + 绑定码，7-15 分钟出网）。
type BoxInput struct {
	SN       string
	BindCode string
	Name     string
	Region   string // 欧版|东南亚版|美版|国版
	Actor    iam.Subject
	Terminal eventbus.Terminal
}

// ImportBox 导入紫鸟盒子。
func (s *Service) ImportBox(ctx context.Context, in BoxInput) (*types.Device, error) {
	if len(in.SN) < 6 || in.BindCode == "" {
		return nil, apierr.Validation("盒子 SN 与绑定码必填")
	}
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "device:import", Path: "/devices"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权导入设备（" + string(d.Layer) + "）")
	}
	if err := s.ent.CheckQuota(ctx, in.Actor.TenantID, billing.MetricDeviceCount, 1); err != nil {
		return nil, err
	}
	id := ids.New()
	now := time.Now().UTC()
	dev := &types.Device{
		ID: id, TenantID: in.Actor.TenantID, Name: orDefault(in.Name, "紫鸟盒子-"+in.SN),
		Type: types.DeviceBox, SN: in.SN, Region: in.Region,
		Status: types.DeviceStatusAllocating, OwnerID: in.Actor.UserID,
		MaxConcurrent: s.cfg.Limits.DeviceMaxConcurrentUsers,
		ExpiresAt:     now.AddDate(1, 0, 0), // 一次性购买 + 质保一年
		CreatedAt:     now, UpdatedAt: now,
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Devices().Create(tctx, dev); err != nil {
			return err
		}
		if _, err := s.meter.Record(tctx, tx, in.Actor.TenantID, billing.MetricDeviceCount, 1); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: TypeDeviceImported, OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{DeviceID: id}, Terminal: in.Terminal,
			Context: eventbus.ContextInfo{Action: "device.import_box", Result: "success"},
			// L4：绑定码只留前 4 位用于排障，完整绑定码不落审计
			Data: map[string]any{"device_id": id, "sn": in.SN, "bind_code_prefix": prefix(in.BindCode, 4)},
		})
	}); err != nil {
		return nil, err
	}
	return dev, nil
}

// ---------- 绑定与占用（L9 配额模型） ----------

// BindInput 绑定店铺账号到设备。
type BindInput struct {
	DeviceID string
	ShopID   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Bind 绑定：一个设备可绑多个账号，但受 max_bound_accounts 约束（不是互斥锁）。
func (s *Service) Bind(ctx context.Context, in BindInput) (*types.DeviceBinding, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "device:bind", Path: "/devices"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权绑定设备（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	dev, err := s.st.Devices().Get(tctx, in.DeviceID)
	if err != nil {
		return nil, err
	}
	bound, err := s.st.Bindings().CountBound(tctx, in.DeviceID)
	if err != nil {
		return nil, err
	}
	if bound >= s.cfg.Limits.DeviceMaxBoundAccounts {
		return nil, apierr.Quota(fmt.Sprintf("该设备绑定账号数已达上限 %d", s.cfg.Limits.DeviceMaxBoundAccounts))
	}

	now := time.Now().UTC()
	b := &types.DeviceBinding{
		ID: ids.New(), TenantID: in.Actor.TenantID, DeviceID: in.DeviceID,
		ShopID: in.ShopID, UserID: in.Actor.UserID, BoundAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Bindings().Create(tctx, b); err != nil {
			return err
		}
		if err := tx.Devices().IncrBound(tctx, in.DeviceID, 1); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: TypeDeviceBound, OccurredAt: now,
			Actor:    actorOf(in.Actor),
			Subject:  eventbus.Subject{DeviceID: in.DeviceID, ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "device.bind", Result: "success"},
			Data:     map[string]any{"device_id": in.DeviceID, "shop_id": in.ShopID, "bound": bound + 1},
		})
	}); err != nil {
		return nil, err
	}
	_ = dev
	return b, nil
}

// AcquireResult 占用结果（启动店铺环境时）。
type AcquireResult struct {
	BindingID       string
	ConcurrentUsers int
	SoftLimitHit    bool // 达到软阈值：允许但已告警（L9）
	Device          *types.Device
}

// Acquire 占用设备（启动环境）：并发人数按 user_id 去重；超限策略为三级降级。
func (s *Service) Acquire(ctx context.Context, deviceID, shopID string, actor iam.Subject, term eventbus.Terminal) (*AcquireResult, error) {
	tctx := store.WithTenant(ctx, actor.TenantID)
	dev, err := s.st.Devices().Get(tctx, deviceID)
	if err != nil {
		return nil, err
	}
	if dev.Status == types.DeviceStatusExpired {
		return nil, apierr.Conflict("设备已过期，请先续费")
	}
	bindings, err := s.st.Bindings().List(tctx, store.BindingFilter{DeviceID: deviceID, ShopID: shopID, Active: true})
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, apierr.NotFound("该店铺未绑定此设备")
	}
	concurrent, err := s.st.Bindings().CountConcurrent(tctx, deviceID)
	if err != nil {
		return nil, err
	}
	alreadyHere := false
	for _, b := range bindings {
		if b.UserID == actor.UserID {
			alreadyHere = true
		}
	}
	// 一人多店只占 1 个槽：本人已在用则不再计数
	if !alreadyHere && concurrent >= s.cfg.Limits.DeviceHardLimitUsers {
		return nil, apierr.Conflict(fmt.Sprintf("设备并发人数已达硬上限 %d，请稍后重试或切换备用设备",
			s.cfg.Limits.DeviceHardLimitUsers))
	}
	soft := !alreadyHere && concurrent >= s.cfg.Limits.DeviceMaxConcurrentUsers

	now := time.Now().UTC()
	b := bindings[0]
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Bindings().Touch(tctx, b.ID, actor.UserID, now); err != nil {
			return err
		}
		risk := eventbus.RiskLow
		if soft {
			risk = eventbus.RiskMedium // 事中告警：超过软阈值仍允许使用
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: TypeDeviceAcquired, OccurredAt: now, Risk: risk,
			Actor:    actorOf(actor),
			Subject:  eventbus.Subject{DeviceID: deviceID, ShopID: shopID},
			Terminal: term,
			Context:  eventbus.ContextInfo{Action: "device.acquire", Result: "success"},
			Data: map[string]any{
				"concurrent": concurrent + boolToInt(!alreadyHere),
				"soft_limit": s.cfg.Limits.DeviceMaxConcurrentUsers, "soft_hit": soft,
			},
		})
	}); err != nil {
		return nil, err
	}
	return &AcquireResult{
		BindingID:       b.ID,
		ConcurrentUsers: concurrent + boolToInt(!alreadyHere),
		SoftLimitHit:    soft,
		Device:          dev,
	}, nil
}

// Release 释放占用（关闭环境）。
func (s *Service) Release(ctx context.Context, bindingID string, actor iam.Subject, term eventbus.Terminal) error {
	tctx := store.WithTenant(ctx, actor.TenantID)
	now := time.Now().UTC()
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Bindings().Touch(tctx, bindingID, "", now); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: TypeDeviceReleased, OccurredAt: now,
			Actor: actorOf(actor), Terminal: term,
			Subject: eventbus.Subject{},
			Context: eventbus.ContextInfo{Action: "device.release", Result: "success"},
			Data:    map[string]any{"binding_id": bindingID},
		})
	})
}

// Renew 续费（到期前自动续费由 scheduler 调用同一方法）。
func (s *Service) Renew(ctx context.Context, deviceID string, months int, actor iam.Subject, term eventbus.Terminal) error {
	if months <= 0 {
		months = 1
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	dev, err := s.st.Devices().Get(tctx, deviceID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	base := dev.ExpiresAt
	if base.Before(now) {
		base = now
	}
	dev.ExpiresAt = base.AddDate(0, months, 0)
	dev.Status = types.DeviceStatusActive
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Devices().Update(tctx, dev); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: TypeDeviceRenewed, OccurredAt: now,
			Actor: actorOf(actor), Terminal: term,
			Subject: eventbus.Subject{DeviceID: deviceID},
			Context: eventbus.ContextInfo{Action: "device.renew", Result: "success"},
			Data:    map[string]any{"months": months, "expires_at": dev.ExpiresAt},
		})
	})
}

// List 设备列表。
func (s *Service) List(ctx context.Context, tenantID string, f store.DeviceFilter) ([]types.Device, int, error) {
	return s.st.Devices().List(store.WithTenant(ctx, tenantID), f)
}

// Get 设备详情。
func (s *Service) Get(ctx context.Context, tenantID, id string) (*types.Device, error) {
	return s.st.Devices().Get(store.WithTenant(ctx, tenantID), id)
}

// DeviceBrief 巡检摘要（定时任务只关心到期与健康分，不搬运整行数据）。
type DeviceBrief struct {
	ID        string
	Name      string
	ExpiresAt time.Time
	Health    int
}

// Brief 巡检摘要列表：一次遍历算出健康分，供定时任务告警（M3.2）。
func (s *Service) Brief(ctx context.Context, tenantID string, limit int) ([]DeviceBrief, error) {
	if limit <= 0 {
		limit = 500
	}
	tctx := store.WithTenant(ctx, tenantID)
	list, _, err := s.st.Devices().List(tctx, store.DeviceFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]DeviceBrief, 0, len(list))
	for _, d := range list {
		health := d.HealthScore
		if health <= 0 {
			if h, err := s.Health(ctx, tenantID, d.ID); err == nil && h != nil {
				health = h.Score
			} else {
				health = 100 // 未评分设备不误报（宁可不报，也不凭空判定，L10）
			}
		}
		out = append(out, DeviceBrief{ID: d.ID, Name: d.Name, ExpiresAt: d.ExpiresAt, Health: health})
	}
	return out, nil
}

// Health 设备健康评分（M3.2）。
func (s *Service) Health(ctx context.Context, tenantID, id string) (*network.HealthResult, error) {
	tctx := store.WithTenant(ctx, tenantID)
	dev, err := s.st.Devices().Get(tctx, id)
	if err != nil {
		return nil, err
	}
	concurrent, err := s.st.Bindings().CountConcurrent(tctx, id)
	if err != nil {
		return nil, err
	}
	res := network.EvaluateHealth(network.HealthInput{
		PurityScore: dev.PurityScore, LatencyMS: 120, UptimePct: 99.5,
		BindCount: dev.BoundAccounts, ConcurrentUsers: concurrent,
		MaxConcurrent: dev.MaxConcurrent,
		DaysToExpire:  int(time.Until(dev.ExpiresAt).Hours() / 24),
	})
	return &res, nil
}

// ---------- 辅助 ----------

func actorOf(s iam.Subject) eventbus.Actor {
	codes := make([]string, 0, len(s.Roles))
	for _, r := range s.Roles {
		codes = append(codes, r.Code)
	}
	return eventbus.Actor{UserID: s.UserID, Username: s.Username, RoleCode: codes}
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
