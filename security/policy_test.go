package security_test

import (
	"context"
	"log/slog"
	"testing"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/security"
)

func newTestBus(t *testing.T, st *mem.Store, cfg *config.Config) *eventbus.Bus {
	t.Helper()
	return eventbus.New(st, cfg, slog.Default())
}

func contextWithTenant(ctx context.Context, tenantID string) context.Context {
	return store.WithTenant(ctx, tenantID)
}

func securityFilter() store.SecurityEventFilter { return store.SecurityEventFilter{Limit: 50} }

func newSvc(t *testing.T) (*security.Service, iam.Subject, string) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		t.Fatal(err)
	}
	role, err := st.Roles().GetByCode(ctx, "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New(st, cfg, slog.Default())
	svc := security.NewService(st, bus, iam.NewEngine(cfg))
	actor := iam.Subject{UserID: "u1", TenantID: "t1", Username: "owner", Roles: []types.Role{*role}}
	return svc, actor, "t1"
}

// TestInterceptDeny 事前拦截：命中 deny 规则必须拒绝，并写入安全事件（M14）。
func TestInterceptDeny(t *testing.T) {
	ctx := context.Background()
	svc, actor, tenant := newSvc(t)
	shopID := "shop-1"
	if _, err := svc.Upsert(ctx, security.UpsertInput{
		Name: "高危防护", Scope: "shop", ScopeID: shopID, Enabled: true,
		Rules: []types.PolicyRule{
			{ID: "r1", Type: types.PolicyRuleElement, Pattern: `[data-el="shop.delete_button"]`, Action: types.PolicyActionDeny, Severity: "high"},
			{ID: "r2", Type: types.PolicyRulePage, Pattern: "*/payments/*", Action: types.PolicyActionAlert, Severity: "high"},
			{ID: "r3", Type: types.PolicyRuleElement, Pattern: `[data-field="profit"]`, Action: types.PolicyActionMask, Severity: "medium"},
		},
		Actor: actor,
	}); err != nil {
		t.Fatalf("策略写入失败: %v", err)
	}

	cases := []struct {
		name        string
		in          security.InterceptInput
		wantAllowed bool
		wantAction  types.PolicyAction
	}{
		{"删除按钮被拒", security.InterceptInput{ShopID: shopID, Element: `[data-el="shop.delete_button"]`}, false, types.PolicyActionDeny},
		{"收款页告警但放行", security.InterceptInput{ShopID: shopID, URL: "/shops/payments/edit"}, true, types.PolicyActionAlert},
		{"利润字段遮蔽但放行", security.InterceptInput{ShopID: shopID, Element: `[data-field="profit"]`}, true, types.PolicyActionMask},
		{"普通页面不受影响", security.InterceptInput{ShopID: shopID, URL: "/shops/list"}, true, types.PolicyActionAllow},
		{"其他店铺不套用本策略", security.InterceptInput{ShopID: "shop-2", Element: `[data-el="shop.delete_button"]`}, true, types.PolicyActionAllow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.TenantID = tenant
			in.Actor = actor
			got, err := svc.Intercept(ctx, in)
			if err != nil {
				t.Fatalf("拦截判定失败: %v", err)
			}
			if got.Allowed != tc.wantAllowed || got.Action != tc.wantAction {
				t.Fatalf("want allowed=%v action=%s, got %+v", tc.wantAllowed, tc.wantAction, got)
			}
		})
	}

	// 拦截与告警都必须进入安全事件（事后追溯）
	events, total, err := svc.ListEvents(ctx, tenant, securityFilter())
	if err != nil {
		t.Fatal(err)
	}
	if total < 3 {
		t.Fatalf("至少应有 3 条安全事件，实际 %d (%+v)", total, events)
	}
}

// TestTemplates 策略模板库可一键套用。
func TestTemplates(t *testing.T) {
	list := security.Templates()
	if len(list) < 3 {
		t.Fatalf("模板数量过少: %d", len(list))
	}
	found := security.TemplateOf("high_risk_ops")
	if found == nil || len(found.Rules) == 0 {
		t.Fatal("高危操作防护模板缺失")
	}
	if security.TemplateOf("not-exist") != nil {
		t.Fatal("未知模板应返回 nil")
	}
	for _, tpl := range list {
		for _, r := range tpl.Rules {
			if r.Action == "" || r.Type == "" {
				t.Fatalf("模板 %s 规则不完整: %+v", tpl.Code, r)
			}
		}
	}
}

// TestRaiseEvent 事中告警：高危事件落库并可检索。
func TestRaiseEvent(t *testing.T) {
	ctx := context.Background()
	svc, actor, tenant := newSvc(t)
	actorCopy := actor
	if err := svc.Raise(ctx, security.RaiseInput{
		TenantID: tenant, Category: "alert", Level: "high", ShopID: "s1",
		Action: "pay_account:change", Actor: &actorCopy,
		Detail: map[string]any{"url": "/shops/payments/edit"},
	}); err != nil {
		t.Fatal(err)
	}
	items, total, err := svc.ListEvents(ctx, tenant, securityFilter())
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || items[0].Level != "high" || items[0].Category != "alert" {
		t.Fatalf("事件落库异常: %+v", items)
	}
	if items[0].ActorUserID != actor.UserID {
		t.Fatalf("事件应记录发起人: %+v", items[0])
	}
}
