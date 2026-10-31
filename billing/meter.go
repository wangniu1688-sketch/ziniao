// Package billing 计量与配额（entitlement）。
// 铁律：所有涉及计量的动作走 billing.Meter，禁止各模块自建计数器；
// 免费/付费边界必须落成 entitlement 检查，而不是 UI 上灰个按钮（反例 11）。
package billing

import (
	"context"
	"fmt"

	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/store"
)

// 计量项常量。
const (
	MetricShopCount    = "shop.count"
	MetricDeviceCount  = "device.count"
	MetricMemberCount  = "member.count"
	MetricAPICall      = "api.call"
	MetricAICredit     = "ai.credit"
	MetricStorageBytes = "storage.bytes"
)

// Meter 统一计量入口：在业务写事务内调用，保证「业务与计量」原子一致。
type Meter struct {
	st store.Store
}

// NewMeter 构造计量器。
func NewMeter(st store.Store) *Meter { return &Meter{st: st} }

// Record 增加计量值，返回最新累计值。
func (m *Meter) Record(ctx context.Context, tx store.Tx, tenantID, metric string, delta int64) (int64, error) {
	if tenantID == "" {
		tid, err := store.TenantFrom(ctx)
		if err != nil {
			return 0, err
		}
		tenantID = tid
	}
	return tx.Usage().Incr(ctx, tenantID, metric, delta)
}

// Current 读取当前用量（供配额校验与账单）。
func (m *Meter) Current(ctx context.Context, tenantID, metric string) (int64, error) {
	return m.st.Usage().Get(ctx, tenantID, metric)
}

// Entitlement 配额与功能开关检查器。
type Entitlement struct {
	st  store.Store
	cfg *config.Config
}

// NewEntitlement 构造 entitlement 检查器。
func NewEntitlement(st store.Store, cfg *config.Config) *Entitlement {
	return &Entitlement{st: st, cfg: cfg}
}

// PlanOf 返回租户当前套餐规格。
func (e *Entitlement) PlanOf(ctx context.Context, tenantID string) (config.PlanSpec, error) {
	ctx = store.WithTenant(ctx, tenantID)
	sub, err := e.st.Subscriptions().GetActive(ctx, tenantID)
	if err != nil {
		// 无订阅记录时降级为免费版（fail-safe，不让用户卡死，但受限）
		return config.PlanCatalog(e.cfg.Limits)["free"], nil
	}
	if spec, ok := config.PlanCatalog(e.cfg.Limits)[sub.PlanCode]; ok {
		return spec, nil
	}
	return config.PlanCatalog(e.cfg.Limits)["free"], nil
}

// CheckQuota 校验配额：current + delta 不得超过套餐上限（-1 表示不限）。
func (e *Entitlement) CheckQuota(ctx context.Context, tenantID, metric string, delta int64) error {
	plan, err := e.PlanOf(ctx, tenantID)
	if err != nil {
		return err
	}
	var limit int64
	switch metric {
	case MetricShopCount:
		limit = int64(plan.MaxShops)
	case MetricDeviceCount:
		limit = int64(plan.MaxDevices)
	case MetricMemberCount:
		limit = int64(plan.MaxMembers)
	default:
		return nil
	}
	if limit < 0 {
		return nil
	}
	used, err := e.st.Usage().Get(store.WithTenant(ctx, tenantID), tenantID, metric)
	if err != nil {
		return err
	}
	if used+delta > limit {
		return apierr.Quota(fmt.Sprintf("套餐「%s」%s 配额不足（已用 %d，上限 %d），请升级套餐",
			plan.Name, metricLabel(metric), used, limit))
	}
	return nil
}

// RequireFeature 功能开关检查（Agent / 取证包导出 / 自定义插件）。
func (e *Entitlement) RequireFeature(ctx context.Context, tenantID, feature string) error {
	plan, err := e.PlanOf(ctx, tenantID)
	if err != nil {
		return err
	}
	ok := false
	switch feature {
	case "agent":
		ok = plan.HasAgent
	case "forensic_export":
		ok = plan.HasForensicExport
	case "custom_plugin":
		ok = plan.HasCustomPlugin
	default:
		return apierr.Entitlement("未知功能项: " + feature)
	}
	if !ok {
		return apierr.Entitlement(fmt.Sprintf("套餐「%s」不含 %s，请升级套餐（UI 灰按钮不等于接口放行，此处为服务端强校验）",
			plan.Name, feature))
	}
	return nil
}

func metricLabel(m string) string {
	switch m {
	case MetricShopCount:
		return "店铺数"
	case MetricDeviceCount:
		return "设备数"
	case MetricMemberCount:
		return "成员数"
	}
	return m
}
