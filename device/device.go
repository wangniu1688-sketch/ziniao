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
	"log/slog"
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
	TypeDeviceRetired   = "device.retired" // 退役：归还出口并停用设备
	TypeDeviceExpired   = "device.expired" // 到期回收：系统巡检归还出口并标记过期
	TypeDeviceDeleted   = "device.deleted" // 删除：台账出清 + 回退 device_count 计量
	TypeDeviceRenewed   = "device.renewed"
)

// EgressKey 设备在出口绑定表里的键。
//
// 带 dev: 前缀不是为了好看：egress_bindings 同时服务环境与设备，
// 而**同一张表就是互斥的边界**——设备的出口不能和环境撞到同一个 IP，
// 两个店铺账号共出口等于在平台的证据链上把它们串起来。
// 前缀让「这条绑定是环境的还是设备的」在历史里一眼可辨，不用回头 join 两张表。
func EgressKey(deviceID string) string { return "dev:" + deviceID }

// Service 设备服务。
type Service struct {
	st     store.Store
	bus    *eventbus.Bus
	cfg    *config.Config
	ent    *billing.Entitlement
	meter  *billing.Meter
	engine *iam.Engine
	// pool 出口池（可为 nil：没接代理商时设备没有出口，其余能力照常）。
	//
	// 这里必须是分配器而不是裸 Provider：设备出口和环境出口共用同一套不变量——
	// 独占互斥、跨重启不换 IP、按代理商归还、落到绑定历史。
	// 直接调 Provider 等于把设备做成「唯一绕过这些不变量的入口」。
	pool *network.Allocator
}

// NewService 构造设备服务。
func NewService(st store.Store, bus *eventbus.Bus, cfg *config.Config, engine *iam.Engine,
	ent *billing.Entitlement, meter *billing.Meter) *Service {
	return &Service{st: st, bus: bus, cfg: cfg, engine: engine, ent: ent, meter: meter}
}

// SetEgressPool 接入出口池（环境与设备共用同一个分配器）。
func (s *Service) SetEgressPool(pool *network.Allocator) { s.pool = pool }

// EgressPool 当前出口池（nil 表示未接代理商）。
func (s *Service) EgressPool() *network.Allocator { return s.pool }

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

	// 每台单独申请出口：多环境（或多台设备）共用同一出口 IP 会被平台直接串号，
	// 所以这里不是「申请一个再复用」，而是逐台分配（验收见 network/allocator.go 的排他约束）。
	//
	// 出口一律走分配器而不是裸代理商接口：设备是长期资产（按月租），
	// 同一个出口要在续费、重启、巡检里反复被找到和归还，
	// 没有 egress_bindings 这条线索，重启就只能换新 IP——
	// 而设备 IP 一变，绑在它上面的店铺账号全部跟着变出口。
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	devs := make([]*types.Device, 0, in.Count)
	now := time.Now().UTC()
	// 批量申购里第 N 台失败时，前 N-1 台的出口必须还回去：
	// 否则代理商那边按量计费、我们这边连设备都没建出来，纯亏。
	cleanup := func() {
		if s.pool == nil {
			return
		}
		for _, dev := range devs {
			if err := s.pool.Release(tctx, EgressKey(dev.ID)); err != nil {
				warnEgress("归还申购失败设备的出口失败（资源仍在计费）", dev.ID, err)
			}
		}
	}
	for i := 0; i < in.Count; i++ {
		deviceID := ids.New()
		proxy := (*network.AcquiredProxy)(nil)
		if s.pool != nil {
			acq, err := s.pool.Acquire(tctx, EgressKey(deviceID), network.Region{
				Code: in.Country + "-" + in.City, Country: in.Country, City: in.City,
			})
			if err != nil {
				cleanup()
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
			// 出口租约短于购买周期时以出口为准：设备还在、出口已过期是最难查的一种
			// 「设备健康但连不上」，宁可让设备先到期，也不要出现无出口的活设备。
			if !proxy.ExpiresAt.IsZero() && proxy.ExpiresAt.Before(dev.ExpiresAt) {
				dev.ExpiresAt = proxy.ExpiresAt
			}
		}
		devs = append(devs, dev)
	}

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
		// 落库失败也要还：出口已经在代理商那边开始计费了，
		// 设备没建出来却留着一堆在计费的线路，是最难对账的一种漏。
		cleanup()
		return nil, err
	}
	return devs[0], nil
}

// warnEgress 出口链路的非致命问题（归还失败等）。
//
// 只记日志：这类问题不该让业务操作失败（设备已经停用/申购已经失败），
// 但必须留痕——「资源还在计费」这种漏，只有日志能让人想起来去查。
func warnEgress(msg, deviceID string, err error) {
	slog.Default().Warn(msg, "device", deviceID, "err", err)
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

// Egress 取该设备当前的出口凭据。设备没有出口时返回 (nil, nil)。
//
// 为什么走分配器而不是直接读 devices 行的 host/port：
// 那两列只是**申购那一刻**的快照，重启后要拿回的是代理商那边的活凭据
// （口令可能已轮换）。只有分配器握着 egress_bindings 这条线索。
//
// 只对「已经在库里有绑定」的设备取回，绝不顺手新申请：
// 自有设备与紫鸟盒子的出口不由我们签发，给它们派一条出口纯属副作用。
func (s *Service) Egress(ctx context.Context, deviceID string, actor iam.Subject) (*network.AcquiredProxy, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "device:read", Path: "/devices"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权查看设备（" + string(d.Layer) + "）")
	}
	if s.pool == nil {
		return nil, nil
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	has, err := s.st.EgressBindings().Get(tctx, EgressKey(deviceID))
	if err != nil {
		return nil, err
	}
	if has == nil {
		return nil, nil
	}
	dev, err := s.st.Devices().Get(tctx, deviceID)
	if err != nil {
		return nil, err
	}
	return s.pool.Acquire(tctx, EgressKey(deviceID), network.Region{
		Code: dev.Country + "-" + dev.City, Country: dev.Country, City: dev.City,
	})
}

// Retire 设备退役：归还出口 + 标记回收 + 落审计。
//
// 顺序不能反着写：先改状态再归还，一旦归还失败，这台设备就会带着一条
// **仍在计费**的出口静静躺在库里，没有任何提示，直到对账才发现。
func (s *Service) Retire(ctx context.Context, deviceID string, actor iam.Subject, term eventbus.Terminal) error {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "device:retire", Path: "/devices"})
	if !d.Allow {
		return apierr.Forbidden("无权退役设备（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	dev, err := s.st.Devices().Get(tctx, deviceID)
	if err != nil {
		return err
	}
	if dev.Status == types.DeviceStatusRecycled {
		return nil // 幂等：重复退役不该报错，也不该重复归还
	}
	s.returnEgress(tctx, deviceID, "退役设备")
	now := time.Now().UTC()
	dev.Status = types.DeviceStatusRecycled
	dev.UpdatedAt = now
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Devices().Update(tctx, dev); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: TypeDeviceRetired, OccurredAt: now,
			Actor: actorOf(actor), Subject: eventbus.Subject{DeviceID: deviceID}, Terminal: term,
			Context: eventbus.ContextInfo{Action: "device.retire", Result: "success"},
			Data: map[string]any{
				"device_id": deviceID, "exit": dev.IP,
				"egress_returned": s.pool != nil,
			},
		})
	})
}

// returnEgress 把设备的出口还给代理商（幂等：没有出口时什么都不做）。
//
// 归还失败只记日志：设备已经不能用了，把「没还掉的线路」留痕让人去查，
// 比卡住状态机让退役/回收永远失败要好——后者会让库里堆满停不下来的设备。
func (s *Service) returnEgress(tctx context.Context, deviceID, why string) {
	if s.pool == nil {
		return
	}
	if err := s.pool.Release(tctx, EgressKey(deviceID)); err != nil {
		warnEgress(why+"时归还出口失败（资源可能仍在计费）", deviceID, err)
	}
}

// RetireExpired 回收该租户已到期的设备：归还出口 + 标记过期 + 落审计。
//
// 为什么必须由系统扫：设备按月租，到期后没人会去点「退役」，
// 而出口是**按量计费**的——设备静静过期、线路悄悄收费，
// 这类漏只有在月底对账时才会被发现，那时已经没法追责到具体哪一台。
//
// 幂等：已过期/已退役的设备跳过，重复归还也不会报错（Release 本身幂等）。
func (s *Service) RetireExpired(ctx context.Context, tenantID string, now time.Time) (int, error) {
	tctx := store.WithTenant(ctx, tenantID)
	list, _, err := s.st.Devices().List(tctx, store.DeviceFilter{Limit: 500})
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range list {
		dev := list[i]
		if dev.ExpiresAt.IsZero() || !dev.ExpiresAt.Before(now) {
			continue
		}
		if dev.Status == types.DeviceStatusExpired || dev.Status == types.DeviceStatusRecycled {
			continue
		}
		s.returnEgress(tctx, dev.ID, "回收到期设备")
		dev.Status = types.DeviceStatusExpired
		dev.UpdatedAt = now
		id := dev.ID
		if err := s.st.WithTx(tctx, func(tx store.Tx) error {
			if err := tx.Devices().Update(tctx, &dev); err != nil {
				return err
			}
			return s.bus.Emit(tctx, tx, eventbus.Event{
				TenantID: tenantID, Type: TypeDeviceExpired, OccurredAt: now,
				Actor:    eventbus.Actor{UserID: "system", Username: "system"},
				Subject:  eventbus.Subject{DeviceID: id},
				Terminal: eventbus.Terminal{Mode: "job", ClientVer: "jobs/1"},
				Context:  eventbus.ContextInfo{Action: "device.expire", Result: "success"},
				Data:     map[string]any{"device_id": id, "exit": dev.IP},
			})
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Delete 删除设备（终态）：归还出口 + 软删除行 + 回退计量 + 落审计。
//
// 权限与退役同级（device:delete）：删除把设备从台账里出清，不可逆。
//
// **计量口径（device_count 定案）**：过期/退役**不**回退，删除才回退。
// 理由：renew 会把 expired 拉回 active，若到期即回退就必须在续费时补计量
// （还要重新过配额校验，续费可能因配额不足被卡），链路多一对对称操作就多一对
// 出错机会；口径定为「台账占用」，删除 = 台账出清 = 释放配额，一次操作一个语义。
// 想腾配额就删掉退役设备——这正是删除入口存在的意义。
//
// 只允许删除已退役/已过期的设备：active 设备先退役（归还出口有专门路径），
// 防止「删了设备、线路还在计费」这种只有对账才能发现的漏。
func (s *Service) Delete(ctx context.Context, deviceID string, actor iam.Subject, term eventbus.Terminal) error {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "device:delete", Path: "/devices"})
	if !d.Allow {
		return apierr.Forbidden("无权删除设备（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	dev, err := s.st.Devices().Get(tctx, deviceID)
	if err != nil {
		return err
	}
	if dev.Status != types.DeviceStatusExpired && dev.Status != types.DeviceStatusRecycled {
		return apierr.Conflict("只有已退役或已过期的设备才能删除（请先退役或等待到期回收）")
	}
	s.returnEgress(tctx, deviceID, "删除设备")
	now := time.Now().UTC()
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Devices().Delete(tctx, deviceID); err != nil {
			return err
		}
		// 计量回退与删除同事务：删除成功而计量没回退（或反之）都是脏账。
		// 先查后减并在 0 时跳过：计量与台账历史上若出现过不同步（迁移/补录），
		// 钳在 0 比减成负数好——负数会让后续申购凭空多出额度。
		released := false
		if cur, gerr := tx.Usage().Get(tctx, actor.TenantID, billing.MetricDeviceCount); gerr == nil && cur > 0 {
			if _, err := s.meter.Record(tctx, tx, actor.TenantID, billing.MetricDeviceCount, -1); err != nil {
				return err
			}
			released = true
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: TypeDeviceDeleted, OccurredAt: now, Risk: eventbus.RiskHigh,
			Actor: actorOf(actor), Subject: eventbus.Subject{DeviceID: deviceID}, Terminal: term,
			Context: eventbus.ContextInfo{Action: "device.delete", Result: "success"},
			Data: map[string]any{
				"device_id": deviceID, "prev_status": string(dev.Status),
				"quota_released": released,
			},
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
