package approval_test

import (
	"context"
	"testing"
	"time"

	"ziniao/approval"
	"ziniao/audit"
	"ziniao/platform/apierr"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// seedRule 直接落一条规则（绕过服务层校验，用于构造被测数据）。
func seedRule(t *testing.T, e *env, r *types.ApprovalRouteRule) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), r.TenantID)
	now := time.Now().UTC()
	if r.ID == "" {
		r.ID = ids.New()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	r.Enabled = true
	if err := e.st.ApprovalRouteRules().Create(ctx, r); err != nil {
		t.Fatalf("种规则失败: %v", err)
	}
}

func ticketOf(typ string, amount float64, tenant string) *types.ApprovalTicket {
	if tenant == "" {
		tenant = "t1"
	}
	return &types.ApprovalTicket{TenantID: tenant, Type: typ,
		Detail: map[string]any{"amount": amount}}
}

// TestCustomRuleOverridesBuiltin 租户配置优先：改审批级别不必改代码。
func TestCustomRuleOverridesBuiltin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 批量删除从「owner/ops_manager」收紧到「仅老板」
	seedRule(t, e, &types.ApprovalRouteRule{TenantID: "t1",
		TicketType: types.ApprovalTypeBulkDelete, Roles: []string{"owner"},
		MinApprovers: 1, Reason: "本团队要求：批量删除一律老板批"})

	got := e.svc.RequiredFor(ctx, ticketOf(types.ApprovalTypeBulkDelete, 0, ""))
	if got.Source != "custom" {
		t.Fatalf("应命中租户规则，实际 source=%s", got.Source)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "owner" {
		t.Fatalf("要求角色应为 [owner]，实际 %v", got.Roles)
	}
	// 运营主管因此被挡在门外（内置规则下他是可以批的）
	if err := e.svc.CanApprove(ctx, &types.ApprovalTicket{TenantID: "t1",
		Type: types.ApprovalTypeBulkDelete, RequesterID: "u-owner"}, e.leader); err == nil {
		t.Fatal("收紧后运营主管不应能批批量删除")
	}
	// 未配置的类型不受影响（升级不改行为）
	other := e.svc.RequiredFor(ctx, ticketOf(types.ApprovalTypeCredReveal, 0, ""))
	if other.Source != "builtin" {
		t.Fatalf("未配置的类型应仍走内置，实际 %s", other.Source)
	}
}

// TestAmountThresholdConfigurable 金额门槛可配：这是本轮要解决的问题本身
// （改一个阈值不用发版），且必须同时驱动「要不要持证」与「要几个人签」。
func TestAmountThresholdConfigurable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const typ = types.ApprovalTypeDevicePurchase

	// 没配规则时：按配置中心阈值（默认 5 万）
	if e.svc.NeedsTicketForAmount(ctx, "t1", typ, 1000) {
		t.Fatal("未配置规则时小额申购不应要求持证")
	}
	if !e.svc.NeedsTicketForAmount(ctx, "t1", typ, 60000) {
		t.Fatal("未配置规则时超阈值申购应要求持证")
	}

	// 配成「≥1000 就要两位老板复核」
	seedRule(t, e, &types.ApprovalRouteRule{TenantID: "t1", TicketType: typ,
		MinAmount: 1000, Roles: []string{"owner"}, MinApprovers: 2, Reason: "团队自定门槛"})
	if !e.svc.NeedsTicketForAmount(ctx, "t1", typ, 5000) {
		t.Fatal("配了规则后 5000 应要求持证（门槛已降到 1000）")
	}
	if e.svc.NeedsTicketForAmount(ctx, "t1", typ, 500) {
		t.Fatal("500 未达配置的任何门槛，不应要求持证")
	}
	req := e.svc.RequiredFor(ctx, ticketOf(typ, 5000, ""))
	if req.MinApprovers != 2 || req.Source != "custom" {
		t.Fatalf("5000 的单应命中自定义会签规则: %+v", req)
	}
	// 金额没命中 → 回落内置，而不是「谁都能批」
	low := e.svc.RequiredFor(ctx, ticketOf(typ, 500, ""))
	if low.Source != "builtin" || len(low.Roles) == 0 {
		t.Fatalf("未命中的金额应回落内置规则，实际 %+v", low)
	}
}

// TestDisabledAndPriority 停用行不参与判定；多条命中时取优先级最高（数值最小）的。
func TestDisabledAndPriority(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedRule(t, e, &types.ApprovalRouteRule{TenantID: "t1", TicketType: types.ApprovalTypeCredReveal,
		Roles: []string{"owner"}, MinApprovers: 3, Priority: 10, Reason: "先配的严格规则"})
	e.st.ApprovalRouteRules().Update(store.WithTenant(ctx, "t1"), &types.ApprovalRouteRule{
		ID: "nope", TenantID: "t1"}) // 不存在 → 忽略错误即可
	// 停用上面那条
	list, err := e.st.ApprovalRouteRules().ListByTenant(store.WithTenant(ctx, "t1"), "t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("规则列表异常: n=%d err=%v", len(list), err)
	}
	off := list[0]
	off.Enabled = false
	if err := e.st.ApprovalRouteRules().Update(store.WithTenant(ctx, "t1"), &off); err != nil {
		t.Fatal(err)
	}
	got := e.svc.RequiredFor(ctx, ticketOf(types.ApprovalTypeCredReveal, 0, ""))
	if got.Source != "builtin" {
		t.Fatalf("停用后的规则不应参与判定，实际 %s", got.Source)
	}

	// 两条都命中：priority 小的那条生效
	seedRule(t, e, &types.ApprovalRouteRule{TenantID: "t1", TicketType: types.ApprovalTypeCredReveal,
		Roles: []string{"ops_manager"}, MinApprovers: 1, Priority: 100, Reason: "宽松"})
	seedRule(t, e, &types.ApprovalRouteRule{TenantID: "t1", TicketType: types.ApprovalTypeCredReveal,
		Roles: []string{"owner"}, MinApprovers: 1, Priority: 5, Reason: "严格"})
	got = e.svc.RequiredFor(ctx, ticketOf(types.ApprovalTypeCredReveal, 0, ""))
	if len(got.Roles) != 1 || got.Roles[0] != "owner" {
		t.Fatalf("应取优先级最高的规则（owner），实际 %v", got.Roles)
	}
}

// TestRulesAreTenantScoped 规则是租户级的：别家团队的规则不影响本团队。
func TestRulesAreTenantScoped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedRule(t, e, &types.ApprovalRouteRule{TenantID: "t-other",
		TicketType: types.ApprovalTypeBulkDelete, Roles: []string{"owner"}, MinApprovers: 9})
	got := e.svc.RequiredFor(ctx, ticketOf(types.ApprovalTypeBulkDelete, 0, "t1"))
	if got.Source != "builtin" {
		t.Fatalf("别家团队的规则不应影响本团队，实际 %s", got.Source)
	}
}

// TestUpsertRuleValidation 配置错误必须在写入时就挡掉：
// 一条永远匹配不到审批人的规则 = 单据卡死无人可批。
func TestUpsertRuleValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	in := func(mut func(*approval.RuleInput)) approval.RuleInput {
		r := approval.RuleInput{
			TicketType: types.ApprovalTypeBulkDelete, Roles: []string{"owner"},
			MinApprovers: 1, Actor: e.owner, URL: "/approvals",
		}
		mut(&r)
		return r
	}
	if _, err := e.svc.UpsertRule(ctx, in(func(r *approval.RuleInput) { r.TicketType = "shop.something" })); err == nil {
		t.Fatal("未知单据类型应被拒")
	}
	if _, err := e.svc.UpsertRule(ctx, in(func(r *approval.RuleInput) { r.Roles = []string{"boss"} })); err == nil {
		t.Fatal("不存在的角色码应被拒（否则这条规则永远匹配不到审批人）")
	}
	if _, err := e.svc.UpsertRule(ctx, in(func(r *approval.RuleInput) { r.MinApprovers = 99 })); err == nil {
		t.Fatal("会签人数超上限应被拒")
	}
	if _, err := e.svc.UpsertRule(ctx, in(func(r *approval.RuleInput) { r.MinAmount = -1 })); err == nil {
		t.Fatal("负金额应被拒")
	}
	// 普通运营不能改规则（改级别 = 给自己开门）
	if _, err := e.svc.UpsertRule(ctx, in(func(r *approval.RuleInput) { r.Actor = e.staff })); err == nil ||
		!apierr.Is(err, apierr.CodeForbidden) {
		t.Fatalf("普通运营不应能维护路由规则: %v", err)
	}
}

// TestUpsertRuleKeepsEnabledAndAudits 局部更新不该顺手停用规则；变更必须落审计。
func TestUpsertRuleKeepsEnabledAndAudits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 事件 → 审计由 audit.Sink 落库（生产在 app.New 装配），单测要显式接上
	e.bus.Register(audit.NewSink(e.st))

	rule, err := e.svc.UpsertRule(ctx, approval.RuleInput{
		TicketType: types.ApprovalTypeBulkDelete, Roles: []string{"owner"}, MinApprovers: 1,
		Reason: "测试", Actor: e.owner, URL: "/approvals",
	})
	if err != nil {
		t.Fatalf("新建规则失败: %v", err)
	}
	if !rule.Enabled {
		t.Fatal("新建规则应默认启用")
	}
	// 局部更新（不传 enabled）后仍应启用
	again, err := e.svc.UpsertRule(ctx, approval.RuleInput{
		ID: rule.ID, TicketType: types.ApprovalTypeBulkDelete, Roles: []string{"owner"},
		MinApprovers: 2, Actor: e.owner, URL: "/approvals",
	})
	if err != nil {
		t.Fatalf("更新规则失败: %v", err)
	}
	if !again.Enabled {
		t.Fatal("未传 enabled 的更新不应把规则停用")
	}
	if again.MinApprovers != 2 {
		t.Fatalf("会签人数应更新为 2，实际 %d", again.MinApprovers)
	}
	// 改的是「谁能批什么」：必须留痕（事件先入发件箱，派发后落成审计）
	if _, err := e.bus.DispatchOnce(ctx); err != nil {
		t.Fatalf("事件派发失败: %v", err)
	}
	entries, _, err := e.st.Audit().List(store.WithTenant(ctx, "t1"), store.AuditFilter{Limit: 200})
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	found := false
	for _, en := range entries {
		if en.Action == "approval.rule.create" || en.Action == "approval.rule.update" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("规则变更必须落审计（%s），实际 %d 条审计里没有", approval.TypeRuleUpdated, len(entries))
	}
}

// TestCustomQuorumDrivesCosign 配置出来的会签人数要真的驱动会签引擎，
// 否则「可配置」只是多了一张没人读的表。
func TestCustomQuorumDrivesCosign(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedRule(t, e, &types.ApprovalRouteRule{TenantID: "t1",
		TicketType: types.ApprovalTypeBulkDelete, Roles: []string{"owner"}, MinApprovers: 2})

	tk, err := e.svc.Create(ctx, approval.CreateInput{
		Type: types.ApprovalTypeBulkDelete, SubjectType: "shop", SubjectID: "shop-1",
		Reason: "清理废弃店铺", Actor: e.leader, URL: "/shops",
	})
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	// 运营主管在内置规则下能批，配了规则后不行
	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.staff, URL: "/shops"}); err == nil {
		t.Fatal("配了规则后普通运营不应能批")
	}
	first, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.owner, URL: "/shops"})
	if err != nil {
		t.Fatalf("首位老板签署失败: %v", err)
	}
	if first.Status != types.ApprovalPending {
		t.Fatalf("未达法定人数应 pending，实际 %s", first.Status)
	}
	second, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.boss2, URL: "/shops"})
	if err != nil {
		t.Fatalf("第二位老板签署失败: %v", err)
	}
	if second.Status != types.ApprovalApproved {
		t.Fatalf("达到法定人数应 approved，实际 %s", second.Status)
	}
}
