// Package shop 店铺（Phase 0 最小实体，用于跑通「带审计的写操作」闭环）。
// M2 的完整实现（生命周期状态机、账密托管、注册环境模板、批量导入等）在 Phase 2。
package shop

import (
	"context"
	"fmt"
	"time"

	"ziniao/billing"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Service 店铺服务。
type Service struct {
	st     store.Store
	bus    *eventbus.Bus
	cfg    *config.Config
	engine *iam.Engine
	meter  *billing.Meter
	ent    *billing.Entitlement
}

// NewService 构造店铺服务。
func NewService(st store.Store, bus *eventbus.Bus, cfg *config.Config, engine *iam.Engine,
	meter *billing.Meter, ent *billing.Entitlement) *Service {
	return &Service{st: st, bus: bus, cfg: cfg, engine: engine, meter: meter, ent: ent}
}

// CreateInput 创建店铺入参。
type CreateInput struct {
	Name     string
	Platform string
	Site     string
	Status   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Create 创建店铺：权限裁决 → 配额校验 → 事务内写业务 + 计量 + 投递事件。
// 这是 Phase 0 的验收路径：一次写操作必须产生一条带哈希链的审计记录。
func (s *Service) Create(ctx context.Context, in CreateInput) (*types.Shop, error) {
	if len(in.Name) == 0 || len([]rune(in.Name)) > s.cfg.Limits.ShopNameMaxLen {
		return nil, apierr.Validation(fmt.Sprintf("店铺名长度需在 1-%d 之间", s.cfg.Limits.ShopNameMaxLen))
	}
	decision := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "shop:create", Path: "/shops"})
	if !decision.Allow {
		return nil, denyErr(decision)
	}
	if err := s.ent.CheckQuota(ctx, in.Actor.TenantID, billing.MetricShopCount, 1); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	status := in.Status
	if status == "" {
		status = "注册中"
	}
	sp := &types.Shop{
		ID: ids.New(), TenantID: in.Actor.TenantID, Name: in.Name,
		Platform: in.Platform, Site: in.Site, Status: status, OwnerID: in.Actor.UserID,
		CreatedAt: now, UpdatedAt: now,
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Shops().Create(tctx, sp); err != nil {
			return err
		}
		if _, err := s.meter.Record(tctx, tx, in.Actor.TenantID, billing.MetricShopCount, 1); err != nil {
			return err
		}
		data := map[string]any{
			"shop_id": sp.ID, "name": sp.Name,
			"platform": sp.Platform, "site": sp.Site, "status": sp.Status,
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: eventbus.TypeShopCreated, OccurredAt: now,
			Actor: eventbus.Actor{
				UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor),
			},
			Subject:  eventbus.Subject{ShopID: sp.ID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.create", Result: "success"},
			Data:     data,
		})
	}); err != nil {
		return nil, err
	}
	return sp, nil
}

// View 店铺视图：字段级遮蔽后返回给调用方（L6 第二层：服务层就不下发明文）。
type View struct {
	types.Shop
	Profit  any      `json:"profit,omitempty"`
	Cost    any      `json:"cost,omitempty"`
	Balance any      `json:"balance,omitempty"`
	Masked  []string `json:"masked_fields,omitempty"`
}

// List 列表：受数据级权限（own/team/all）约束 + 字段级遮蔽。
func (s *Service) List(ctx context.Context, actor iam.Subject, path string, limit, offset int) ([]View, int, error) {
	decision := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "shop:read", Path: path})
	if !decision.Allow {
		return nil, 0, denyErr(decision)
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	shops, total, err := s.st.Shops().List(tctx, store.ShopFilter{
		OwnerID: actor.UserID, Scope: decision.DataScope,
		DelegatedFor: actor.UserID, // own 范围下放行「委托给我的店铺」（代理机构委托）
		Limit:        limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]View, 0, len(shops))
	for _, sp := range shops {
		out = append(out, toView(sp, decision.MaskFields))
	}
	return out, total, nil
}

// Get 详情（同样受数据范围与遮蔽约束）。
func (s *Service) Get(ctx context.Context, actor iam.Subject, id, path string) (*View, error) {
	decision := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "shop:read", Path: path})
	if !decision.Allow {
		return nil, denyErr(decision)
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	sp, err := s.st.Shops().Get(tctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, decision, actor, sp, "查看该店铺"); err != nil {
		return nil, err
	}
	v := toView(*sp, decision.MaskFields)
	return &v, nil
}

// Delete 删除（软删至回收站，高风险 → 审计 + 告警）。
func (s *Service) Delete(ctx context.Context, actor iam.Subject, id, path string, term eventbus.Terminal) error {
	decision := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "shop:delete", Path: path})
	if !decision.Allow {
		return denyErr(decision)
	}
	if contains(decision.DenyElements, "element:shop.delete_button") {
		return apierr.Forbidden("元素级策略已屏蔽删除操作")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	sp, err := s.st.Shops().Get(tctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	sp.DeletedAt = &now
	sp.UpdatedAt = now
	if err := checkShopAccess(s.st, tctx, decision, actor, sp, "删除他人店铺"); err != nil {
		return err
	}
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Shops().Update(tctx, sp); err != nil {
			return err
		}
		if _, err := s.meter.Record(tctx, tx, actor.TenantID, billing.MetricShopCount, -1); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: eventbus.TypeShopDeleted, OccurredAt: now,
			Risk: eventbus.RiskHigh,
			Actor: eventbus.Actor{
				UserID: actor.UserID, Username: actor.Username, RoleCode: roleCodes(actor),
			},
			Subject:  eventbus.Subject{ShopID: id},
			Terminal: term,
			Context:  eventbus.ContextInfo{Action: "shop.delete", Result: "success"},
			Data:     map[string]any{"shop_id": id, "name": sp.Name},
		})
	})
}

// ---------- 辅助 ----------

const maskPlaceholder = "***"

func toView(sp types.Shop, mask []string) View {
	v := View{Shop: sp}
	v.Profit, v.Cost, v.Balance = sp.Profit, sp.Cost, sp.Balance
	for _, f := range mask {
		switch f {
		case "profit":
			v.Profit, v.Masked = maskPlaceholder, append(v.Masked, "profit")
		case "cost":
			v.Cost, v.Masked = maskPlaceholder, append(v.Masked, "cost")
		case "balance":
			v.Balance, v.Masked = maskPlaceholder, append(v.Masked, "balance")
		}
	}
	return v
}

func roleCodes(s iam.Subject) []string {
	codes := make([]string, 0, len(s.Roles))
	for _, r := range s.Roles {
		codes = append(codes, r.Code)
	}
	return codes
}

func contains(list []string, v string) bool {
	for _, it := range list {
		if it == v {
			return true
		}
	}
	return false
}

// denyErr 统一拒绝错误：不向客户端泄露内部层级细节（Layer/Reason 只进审计日志）。
func denyErr(d iam.Decision) error {
	return apierr.Forbidden("无权执行该操作（" + string(d.Layer) + "）")
}
