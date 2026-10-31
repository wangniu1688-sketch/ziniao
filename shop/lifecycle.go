package shop

import (
	"context"
	"fmt"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// 事件类型（本模块新增；前缀 shop → 操作日志）。
const (
	TypeShopUpdated  = "shop.updated"
	TypeShopRestored = "shop.restored"
)

// 店铺生命周期状态（M2.2）：状态决定可执行操作、可绑定设备类型与指纹策略。
const (
	StateRegistering = "注册中"
	StateNurturing   = "养号期"
	StateOperating   = "正常运营"
	StateReviewing   = "审核中"
	StateRestricted  = "受限"
	StateAbandoned   = "已弃用"
)

// AllowedTransitions 状态机：只允许定义过的迁移（表驱动测试覆盖）。
var AllowedTransitions = map[string][]string{
	"":               {StateRegistering},
	StateRegistering: {StateNurturing, StateReviewing, StateAbandoned},
	StateNurturing:   {StateOperating, StateRestricted, StateAbandoned},
	StateOperating:   {StateReviewing, StateRestricted, StateNurturing, StateAbandoned},
	StateReviewing:   {StateOperating, StateRestricted, StateAbandoned},
	StateRestricted:  {StateOperating, StateAbandoned},
	StateAbandoned:   {StateRegistering},
}

// CanTransition 判定一次状态迁移是否合法。
func CanTransition(from, to string) error {
	if to == "" {
		return apierr.Validation("目标状态不能为空")
	}
	allowed, ok := AllowedTransitions[from]
	if !ok {
		return apierr.Validation("当前状态未知: " + from)
	}
	for _, s := range allowed {
		if s == to {
			return nil
		}
	}
	return apierr.Validation(fmt.Sprintf("不允许的状态迁移: %s → %s", orEmpty(from), to))
}

// RequiredEnvForState 状态 → 环境要求（注册环境必须用全新住宅 IP 与新号专用画像）。
func RequiredEnvForState(state string) (minPurity int, requireResidential bool) {
	switch state {
	case StateRegistering:
		return 85, true // 注册环境：住宅 IP + 高纯净度
	case StateNurturing:
		return 75, false
	default:
		return 60, false
	}
}

// TransitionInput 状态迁移入参。
type TransitionInput struct {
	ShopID   string
	To       string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
	Reason   string
}

// Transition 变更店铺生命周期：权限裁决 → 状态机校验 → 事务写 + 事件。
func (s *Service) Transition(ctx context.Context, in TransitionInput) (*types.Shop, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "shop:transition", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权变更店铺状态（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	sh, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if d.DataScope == "own" && sh.OwnerID != in.Actor.UserID {
		return nil, apierr.Forbidden("无权操作他人店铺")
	}
	if err := CanTransition(sh.Status, in.To); err != nil {
		return nil, err
	}

	from := sh.Status
	now := time.Now().UTC()
	sh.Status = in.To
	sh.UpdatedAt = now
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Shops().Update(tctx, sh); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: TypeShopUpdated, OccurredAt: now,
			Actor:    actorOf(in.Actor),
			Subject:  eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.transition", Result: "success"},
			Data:     map[string]any{"from": from, "to": in.To, "reason": in.Reason},
		})
	}); err != nil {
		return nil, err
	}
	return sh, nil
}

// SetGroupInput 分组/标签设置入参。
type SetGroupInput struct {
	ShopID   string
	Group    string
	Tags     []string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// SetGroup 设置店铺分组与标签（批量操作的基础）。
func (s *Service) SetGroup(ctx context.Context, in SetGroupInput) (*types.Shop, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "shop:update", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权修改店铺（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	sh, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return nil, err
	}
	if d.DataScope == "own" && sh.OwnerID != in.Actor.UserID {
		return nil, apierr.Forbidden("无权操作他人店铺")
	}
	now := time.Now().UTC()
	sh.Group = in.Group
	sh.Tags = in.Tags
	sh.UpdatedAt = now
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Shops().Update(tctx, sh); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: TypeShopUpdated, OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.group", Result: "success"},
			Data:     map[string]any{"group": in.Group, "tags": in.Tags},
		})
	}); err != nil {
		return nil, err
	}
	return sh, nil
}

// Restore 从回收站恢复（软删除可逆，审计留痕）。
func (s *Service) Restore(ctx context.Context, shopID string, actor iam.Subject, term eventbus.Terminal) (*types.Shop, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "shop:update", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权恢复店铺（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	sh, err := s.st.Shops().Get(tctx, shopID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sh.DeletedAt = nil
	sh.UpdatedAt = now
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Shops().Update(tctx, sh); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: TypeShopRestored, OccurredAt: now,
			Actor: actorOf(actor), Subject: eventbus.Subject{ShopID: shopID}, Terminal: term,
			Context: eventbus.ContextInfo{Action: "shop.restore", Result: "success"},
		})
	}); err != nil {
		return nil, err
	}
	return sh, nil
}

// Trash 回收站列表。
func (s *Service) Trash(ctx context.Context, tenantID string, limit, offset int) ([]types.Shop, int, error) {
	f := store.ShopFilter{Deleted: true, Limit: limit, Offset: offset}
	return s.st.Shops().List(store.WithTenant(ctx, tenantID), f)
}

// actorOf 统一的事件主体（与 shop.go 的 roleCodes 保持一致）。
func actorOf(s iam.Subject) eventbus.Actor {
	return eventbus.Actor{UserID: s.UserID, Username: s.Username, RoleCode: roleCodes(s)}
}

func orEmpty(s string) string {
	if s == "" {
		return "(空)"
	}
	return s
}
