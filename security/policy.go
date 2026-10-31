// Package security 安全中心（M14）：事前拦截（访问策略）/ 事中告警 / 事后追溯。
//
// 三层模型：
//  1. 事前拦截 —— 页面级 + 元素级访问策略，模板一键套用；
//  2. 事中监管 —— 高危操作实时写入 security_events 并推送告警；
//  3. 事后追溯 —— 所有事件经 eventbus 与审计日志同源（唯一真相源）。
package security

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Service 安全服务。
type Service struct {
	st     store.Store
	bus    *eventbus.Bus
	engine *iam.Engine
}

// NewService 构造安全服务。
func NewService(st store.Store, bus *eventbus.Bus, engine *iam.Engine) *Service {
	return &Service{st: st, bus: bus, engine: engine}
}

// ---------- 策略模板库（M14.2 一键套用） ----------

// Template 访问策略模板。
type Template struct {
	Code  string             `json:"code"`
	Name  string             `json:"name"`
	Desc  string             `json:"desc"`
	Rules []types.PolicyRule `json:"rules"`
}

// Templates 内置策略模板库。
func Templates() []Template {
	return []Template{
		{
			Code: "high_risk_ops", Name: "高危操作防护",
			Desc: "屏蔽删除商品、异常改价、清库存按钮；命中即拦截并告警",
			Rules: []types.PolicyRule{
				{ID: "r1", Type: types.PolicyRuleElement, Pattern: `[data-el="product.delete_button"]`, Action: types.PolicyActionDeny, Severity: "high", Note: "删除商品"},
				{ID: "r2", Type: types.PolicyRuleElement, Pattern: `[data-el="price.change_button"]`, Action: types.PolicyActionDeny, Severity: "high", Note: "异常改价"},
				{ID: "r3", Type: types.PolicyRuleElement, Pattern: `[data-el="stock.clear_button"]`, Action: types.PolicyActionDeny, Severity: "high", Note: "清库存"},
			},
		},
		{
			Code: "pay_account_guard", Name: "收款账户保护",
			Desc: "拦截访问收款账户页（高危），命中即告警并留痕",
			Rules: []types.PolicyRule{
				{ID: "r1", Type: types.PolicyRulePage, Pattern: "*/payments/*", Action: types.PolicyActionAlert, Severity: "high", Note: "访问收款账户页"},
				{ID: "r2", Type: types.PolicyRuleAction, Pattern: "pay_account:change", Action: types.PolicyActionDeny, Severity: "high", Note: "修改收款账户"},
			},
		},
		{
			Code: "readonly_cs", Name: "客服只读",
			Desc: "屏蔽导出与删除入口，允许只读浏览",
			Rules: []types.PolicyRule{
				{ID: "r1", Type: types.PolicyRuleElement, Pattern: `[data-el="shop.export_button"]`, Action: types.PolicyActionDeny, Severity: "medium", Note: "导出"},
				{ID: "r2", Type: types.PolicyRuleElement, Pattern: `[data-el="shop.delete_button"]`, Action: types.PolicyActionDeny, Severity: "high", Note: "删除"},
			},
		},
		{
			Code: "finance_mask", Name: "财务字段遮蔽",
			Desc: "命中利润/成本/余额元素时替换为掩码，禁止导出明文",
			Rules: []types.PolicyRule{
				{ID: "r1", Type: types.PolicyRuleElement, Pattern: `[data-field="profit"]`, Action: types.PolicyActionMask, Severity: "medium", Note: "利润"},
				{ID: "r2", Type: types.PolicyRuleElement, Pattern: `[data-field="cost"]`, Action: types.PolicyActionMask, Severity: "medium", Note: "成本"},
				{ID: "r3", Type: types.PolicyRuleElement, Pattern: `[data-field="balance"]`, Action: types.PolicyActionMask, Severity: "medium", Note: "余额"},
			},
		},
	}
}

// TemplateOf 按 code 取模板。
func TemplateOf(code string) *Template {
	for i := range Templates() {
		if Templates()[i].Code == code {
			t := Templates()[i]
			return &t
		}
	}
	return nil
}

// ---------- 策略维护 ----------

// UpsertInput 策略写入入参。
type UpsertInput struct {
	ID       string
	Name     string
	Scope    string // shop|member|team
	ScopeID  string
	Template string
	Enabled  bool
	Rules    []types.PolicyRule
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Upsert 新建/更新访问策略（管理变动类 → 必须落权限日志，M15）。
func (s *Service) Upsert(ctx context.Context, in UpsertInput) (*types.AccessPolicy, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "security:policy.manage", Path: "/security"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权维护访问策略（" + string(d.Layer) + "）")
	}
	if in.Scope == "" {
		in.Scope = "shop"
	}
	now := time.Now().UTC()
	id := in.ID
	if id == "" {
		id = ids.New()
	}
	p := &types.AccessPolicy{
		ID: id, TenantID: in.Actor.TenantID, Name: in.Name, Scope: in.Scope,
		ScopeID: in.ScopeID, Template: in.Template, Enabled: in.Enabled,
		Rules: in.Rules, UpdatedAt: now,
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if in.ID == "" {
		if err := s.st.Policies().Create(tctx, p); err != nil {
			return nil, err
		}
	} else if err := s.st.Policies().Update(tctx, p); err != nil {
		return nil, err
	}
	_ = s.st.WithTx(tctx, func(tx store.Tx) error {
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "security.policy.updated", OccurredAt: now,
			Risk: eventbus.RiskMedium, Actor: actorOf(in.Actor),
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "security.policy.upsert", Result: "success"},
			Data:     map[string]any{"policy_id": p.ID, "scope": in.Scope, "rules": len(in.Rules)},
		})
	})
	return p, nil
}

// List 策略列表。
func (s *Service) List(ctx context.Context, tenantID, scope, scopeID string) ([]types.AccessPolicy, error) {
	return s.st.Policies().List(store.WithTenant(ctx, tenantID), scope, scopeID)
}

// Delete 删除策略。
func (s *Service) Delete(ctx context.Context, tenantID, id string, actor iam.Subject, term eventbus.Terminal) error {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "security:policy.manage", Path: "/security"})
	if !d.Allow {
		return apierr.Forbidden("无权维护访问策略（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, tenantID)
	if err := s.st.Policies().Delete(tctx, id); err != nil {
		return err
	}
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: tenantID, Type: "security.policy.updated", OccurredAt: time.Now().UTC(),
			Risk: eventbus.RiskMedium, Actor: actorOf(actor), Terminal: term,
			Context: eventbus.ContextInfo{Action: "security.policy.delete", Result: "success"},
			Data:    map[string]any{"policy_id": id},
		})
	})
}

// ---------- 事前拦截 ----------

// InterceptInput 拦截判定入参。
type InterceptInput struct {
	TenantID string
	ShopID   string
	URL      string
	Action   string // 动作标识，如 shop.delete
	Element  string // DOM 选择器，如 [data-el="shop.delete_button"]
	Actor    iam.Subject
	Terminal eventbus.Terminal
}

// InterceptResult 拦截结果。
type InterceptResult struct {
	Allowed  bool               `json:"allowed"`
	Action   types.PolicyAction `json:"action"`
	RuleID   string             `json:"rule_id,omitempty"`
	PolicyID string             `json:"policy_id,omitempty"`
	Reason   string             `json:"reason,omitempty"`
}

// Intercept 事前拦截：按店铺级/团队级策略判定 URL、元素与高危动作。
// 命中 deny 即拒绝；命中 mask / alert 则放行但记录（客户端据此做遮蔽与提示）。
func (s *Service) Intercept(ctx context.Context, in InterceptInput) (InterceptResult, error) {
	empty := InterceptResult{Allowed: true, Action: types.PolicyActionAllow}
	tctx := store.WithTenant(ctx, in.TenantID)
	policies, err := s.st.Policies().List(tctx, "", "")
	if err != nil {
		return empty, err
	}
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		if p.Scope == "shop" && p.ScopeID != "" && p.ScopeID != in.ShopID {
			continue
		}
		for _, r := range p.Rules {
			if !matchRule(r, in) {
				continue
			}
			res := InterceptResult{PolicyID: p.ID, RuleID: r.ID, Action: r.Action, Reason: r.Note}
			switch r.Action {
			case types.PolicyActionDeny:
				res.Allowed = false
				// 事前拦截也要留痕（M14 三层模型的第 1 层）
				_ = s.Raise(ctx, RaiseInput{
					TenantID: in.TenantID, Category: "intercept", Level: r.Severity,
					ShopID: in.ShopID, Actor: &in.Actor, Action: in.Action,
					Detail: map[string]any{"url": in.URL, "element": in.Element, "rule": r.ID, "policy": p.ID},
				})
				return res, nil
			case types.PolicyActionMask, types.PolicyActionAlert:
				res.Allowed = true
				_ = s.Raise(ctx, RaiseInput{
					TenantID: in.TenantID, Category: "alert", Level: r.Severity,
					ShopID: in.ShopID, Actor: &in.Actor, Action: in.Action,
					Detail: map[string]any{"url": in.URL, "element": in.Element, "rule": r.ID, "policy": p.ID, "policy_action": string(r.Action)},
				})
				return res, nil
			}
		}
	}
	return empty, nil
}

func matchRule(r types.PolicyRule, in InterceptInput) bool {
	switch r.Type {
	case types.PolicyRulePage:
		return in.URL != "" && wildcard(r.Pattern, in.URL)
	case types.PolicyRuleElement:
		return in.Element != "" && wildcard(r.Pattern, in.Element)
	case types.PolicyRuleAction:
		return in.Action != "" && wildcard(r.Pattern, in.Action)
	}
	return false
}

// wildcard 通配匹配：支持 * 出现在任意位置（如 */payments/*），按段顺序匹配。
func wildcard(pattern, value string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	rest := value
	for i, p := range parts {
		if p == "" {
			continue
		}
		idx := strings.Index(rest, p)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			return false // 首段必须前缀匹配
		}
		rest = rest[idx+len(p):]
	}
	if last := parts[len(parts)-1]; last != "" && !strings.HasSuffix(value, last) {
		return false
	}
	return true
}

// ---------- 事中告警 / 事后追溯 ----------

// RaiseInput 事件上报入参。
type RaiseInput struct {
	TenantID string
	Category string // intercept|alert|risk|drift
	Level    string // high|medium|low
	ShopID   string
	DeviceID string
	Action   string
	Actor    *iam.Subject
	Detail   map[string]any
	Terminal eventbus.Terminal // 自动化（定时任务）触发时用于标记来源
}

// Raise 写安全事件 + 经事件总线派发告警（事中监管）。
func (s *Service) Raise(ctx context.Context, in RaiseInput) error {
	if in.Category == "" {
		in.Category = "risk"
	}
	if in.Level == "" {
		in.Level = "medium"
	}
	detail, err := json.Marshal(in.Detail)
	if err != nil {
		detail = []byte("{}")
	}
	now := time.Now().UTC()
	e := &types.SecurityEvent{
		ID: ids.New(), TenantID: in.TenantID, Category: in.Category, Level: in.Level,
		ShopID: in.ShopID, DeviceID: in.DeviceID, Action: in.Action,
		Detail: detail, CreatedAt: now,
	}
	if in.Actor != nil {
		e.ActorUserID = in.Actor.UserID
	}
	tctx := store.WithTenant(ctx, in.TenantID)
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.SecurityEvents().Create(tctx, e); err != nil {
			return err
		}
		risk := eventbus.RiskMedium
		if in.Level == "high" {
			risk = eventbus.RiskHigh
		} else if in.Level == "low" {
			risk = eventbus.RiskLow
		}
		actor := eventbus.Actor{}
		if in.Actor != nil {
			actor = actorOf(*in.Actor)
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.TenantID, Type: "risk.alert", OccurredAt: now, Risk: risk,
			Actor: actor, Subject: eventbus.Subject{ShopID: in.ShopID, DeviceID: in.DeviceID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{Action: in.Action, Result: in.Category},
			Data: map[string]any{
				"level": in.Level, "category": in.Category,
				"shop_id": in.ShopID, "device_id": in.DeviceID,
			},
		})
	})
}

// ListEvents 安全事件查询（事后追溯）。
func (s *Service) ListEvents(ctx context.Context, tenantID string, f store.SecurityEventFilter) ([]types.SecurityEvent, int, error) {
	return s.st.SecurityEvents().List(store.WithTenant(ctx, tenantID), f)
}

func actorOf(s iam.Subject) eventbus.Actor {
	codes := make([]string, 0, len(s.Roles))
	for _, r := range s.Roles {
		codes = append(codes, r.Code)
	}
	return eventbus.Actor{UserID: s.UserID, Username: s.Username, RoleCode: codes}
}
