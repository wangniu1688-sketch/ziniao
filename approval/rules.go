package approval

import (
	"context"
	"strconv"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TypeRuleUpdated 路由规则变更事件。
//
// 改的是「谁能批什么」，属于控制变更而不是业务数据变更——必须和审批单本身一样
// 可追溯：否则「为什么这张单只要运营主管批了」事后无从回答。
const TypeRuleUpdated = "iam.approval.rule.updated"

// 规则取值边界（DB 侧有同名 CHECK，这里是 Go 侧的第二道——mem 驱动没有 CHECK）。
const (
	maxMinApprovers = 10
	defaultPriority = 100
)

// RuleInput 新建/更新一条路由规则。
type RuleInput struct {
	ID         string   // 空 = 新建
	TicketType string   // 见 types.ApprovalType*；空 = 兜底规则（适用于所有类型）
	MinAmount  float64  // CNY 金额下限；0 = 不卡金额
	Roles      []string // 合格审批人角色码；空集 = 任意有审批权的人
	// MinApprovers 会签法定人数，0/负数按 1 处理。
	MinApprovers int
	Reason       string
	Priority     int
	Enabled      *bool // nil：新建默认启用、更新保持原值
	Actor        iam.Subject
	Terminal     eventbus.Terminal
	URL          string
}

// BuiltinRuleView 内置规则（只读）：后台用它展示「没配置时走什么」，
// 也让运营看得见回落口径——配了半套规则时，未命中的情形仍然走这里。
type BuiltinRuleView struct {
	TicketType   string   `json:"ticket_type"`
	MinAmount    float64  `json:"min_amount"`
	Roles        []string `json:"roles"`
	MinApprovers int      `json:"min_approvers"`
	Reason       string   `json:"reason"`
}

// BuiltinRules 内置规则清单（金额阈值按当前配置展开）。
func (s *Service) BuiltinRules() []BuiltinRuleView {
	out := make([]BuiltinRuleView, 0)
	for _, r := range builtinRules(s.threshold()) {
		out = append(out, BuiltinRuleView{
			TicketType:   r.ticketType,
			MinAmount:    r.minAmount,
			Roles:        append([]string{}, r.roles...),
			MinApprovers: r.minApprovers,
			Reason:       r.reason,
		})
	}
	return out
}

// ListRules 列出本租户的路由规则（含停用行，界面上要能看见被停用的是什么）。
func (s *Service) ListRules(ctx context.Context, actor iam.Subject, path string) ([]types.ApprovalRouteRule, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "approval:route.read", Path: path})
	if !dec.Allow {
		return nil, apierr.Forbidden("无权查看审批路由规则（" + string(dec.Layer) + "）")
	}
	return s.st.ApprovalRouteRules().ListByTenant(store.WithTenant(ctx, actor.TenantID), actor.TenantID)
}

// UpsertRule 新建或更新一条路由规则。
func (s *Service) UpsertRule(ctx context.Context, in RuleInput) (*types.ApprovalRouteRule, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "approval:route.manage", Path: in.URL})
	if !dec.Allow {
		return nil, apierr.Forbidden("无权维护审批路由规则（" + string(dec.Layer) + "）")
	}
	if in.TicketType != "" && !knownTypes[in.TicketType] {
		return nil, apierr.Validation("未知的审批类型：" + in.TicketType)
	}
	if in.MinAmount < 0 {
		return nil, apierr.Validation("金额门槛不能为负")
	}
	quorum := in.MinApprovers
	if quorum <= 0 {
		quorum = 1
	}
	if quorum > maxMinApprovers {
		return nil, apierr.Validation("会签人数最多 " + strconv.Itoa(maxMinApprovers) + " 人")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	// 角色码必须真实存在：拼错一个角色 = 这条规则永远匹配不到审批人 =
	// 单据卡在 pending 且谁也批不了。这种配置错误必须在写入时就挡掉。
	roles := make([]string, 0, len(in.Roles))
	for _, code := range in.Roles {
		if code == "" {
			continue
		}
		role, err := s.st.Roles().GetByCode(tctx, in.Actor.TenantID, code)
		if err != nil || role == nil {
			return nil, apierr.Validation("角色不存在：" + code)
		}
		roles = append(roles, code)
	}
	priority := in.Priority
	if priority == 0 {
		priority = defaultPriority
	}
	now := time.Now().UTC()

	rule := &types.ApprovalRouteRule{
		TenantID: in.Actor.TenantID, TicketType: in.TicketType, MinAmount: in.MinAmount,
		Roles: roles, MinApprovers: quorum, Reason: in.Reason, Priority: priority,
		Enabled: in.Enabled == nil || *in.Enabled, UpdatedAt: now,
	}
	action := "approval.rule.create"
	if in.ID == "" {
		rule.ID = ids.New()
		rule.CreatedAt = now
	} else {
		old, err := s.st.ApprovalRouteRules().Get(tctx, in.ID)
		if err != nil {
			return nil, err
		}
		if in.Enabled == nil {
			rule.Enabled = old.Enabled // 未传 = 保持原值（否则一次局部更新会顺手把规则停用）
		}
		rule.ID = in.ID
		rule.CreatedAt = old.CreatedAt
		action = "approval.rule.update"
	}
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if in.ID == "" {
			if err := tx.ApprovalRouteRules().Create(tctx, rule); err != nil {
				return err
			}
		} else if err := tx.ApprovalRouteRules().Update(tctx, rule); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: TypeRuleUpdated, OccurredAt: now, Risk: eventbus.RiskHigh,
			Actor:    eventbus.Actor{UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor)},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: action, Result: "success"},
			Data: map[string]any{
				"rule_id": rule.ID, "ticket_type": rule.TicketType, "min_amount": rule.MinAmount,
				"roles": rule.Roles, "min_approvers": rule.MinApprovers, "enabled": rule.Enabled,
			},
		})
	}); err != nil {
		return nil, err
	}
	return rule, nil
}

// DeleteRule 删除一条规则（删除 = 该类型回落到内置规则，而不是「不用审批」）。
func (s *Service) DeleteRule(ctx context.Context, actor iam.Subject, id string,
	term eventbus.Terminal, url string) error {
	dec := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "approval:route.manage", Path: url})
	if !dec.Allow {
		return apierr.Forbidden("无权维护审批路由规则（" + string(dec.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	now := time.Now().UTC()
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.ApprovalRouteRules().Delete(tctx, id); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: TypeRuleUpdated, OccurredAt: now, Risk: eventbus.RiskHigh,
			Actor:    eventbus.Actor{UserID: actor.UserID, Username: actor.Username, RoleCode: roleCodes(actor)},
			Terminal: term,
			Context:  eventbus.ContextInfo{URL: url, Action: "approval.rule.delete", Result: "success"},
			Data:     map[string]any{"rule_id": id},
		})
	})
}

// threshold 金额阈值的唯一取值口径（与网关「要不要持证」共用）。
func (s *Service) threshold() float64 {
	if s.cfg != nil {
		return s.cfg.Limits.PurchaseOwnerThresholdCNY()
	}
	return config.Default().Limits.PurchaseOwnerThresholdCNY()
}

// loadRules 读取本租户已启用的规则（按优先级升序）。
func (s *Service) loadRules(ctx context.Context, tenantID string) ([]types.ApprovalRouteRule, error) {
	if tenantID == "" {
		return nil, nil
	}
	return s.st.ApprovalRouteRules().ListByTenant(store.WithTenant(ctx, tenantID), tenantID)
}

// NeedsTicketForAmount 金额型单据（目前只有设备申购）是否必须持证。
//
// 与 RequiredFor **同一数据源**：租户配了该类型的规则就完全按规则判定，
// 没配才回落配置中心的阈值。两边各读一次配置 = 「网关说不用持证、单据却要求
// 双人签署」这种看起来像随机抽风的错（配置被读了两次，改配置只有一半生效）。
func (s *Service) NeedsTicketForAmount(ctx context.Context, tenantID, ticketType string, amount float64) bool {
	rules, err := s.loadRules(ctx, tenantID)
	typed := false
	if err == nil {
		for _, r := range rules {
			if !r.Enabled || r.TicketType != ticketType {
				continue
			}
			typed = true
			if amount >= r.MinAmount {
				return true
			}
		}
	}
	if typed {
		return false // 配了规则但金额没到任何一条的门槛
	}
	return amount >= s.threshold()
}
