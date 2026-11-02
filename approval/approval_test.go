package approval_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"ziniao/approval"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/notify"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

type env struct {
	svc    *approval.Service
	st     *mem.Store
	bus    *eventbus.Bus
	owner  iam.Subject
	leader iam.Subject
	boss2  iam.Subject
	staff  iam.Subject
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		t.Fatal(err)
	}
	ownerRole, err := st.Roles().GetByCode(ctx, "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	opsRole, err := st.Roles().GetByCode(ctx, "", "ops_manager") // 运营主管
	if err != nil {
		t.Fatal(err)
	}
	opRole, err := st.Roles().GetByCode(ctx, "", "operator") // 普通运营（无高危审批权）
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New(st, cfg, slog.Default())
	engine := iam.NewEngine(cfg)
	return &env{
		svc: approval.NewService(st, bus, cfg, engine),
		st:  st, bus: bus,
		owner: iam.Subject{
			UserID: "u-owner", TenantID: "t1", Username: "owner", Roles: []types.Role{*ownerRole},
		},
		leader: iam.Subject{
			UserID: "u-lead", TenantID: "t1", Username: "leader", Roles: []types.Role{*opsRole},
		},
		// boss2 第二位老板：老板自己申请的审计导出单不能自己批，需要另一位老板批
		boss2: iam.Subject{
			UserID: "u-boss2", TenantID: "t1", Username: "boss2", Roles: []types.Role{*ownerRole},
		},
		// staff 普通运营：有 approval:approve 权限但不是管理员，
		// 用来验证「不是有权限就能批，还得是这条单要求的角色」
		staff: iam.Subject{
			UserID: "u-staff", TenantID: "t1", Username: "staff", Roles: []types.Role{*opRole},
		},
	}
}

func (e *env) ticket(t *testing.T, requester iam.Subject) *types.ApprovalTicket {
	t.Helper()
	tk, err := e.svc.Create(context.Background(), approval.CreateInput{
		Type: types.ApprovalTypeCredReveal, SubjectType: "shop", SubjectID: "shop-1",
		Reason: "排查登录异常", Actor: requester, URL: "/shops",
	})
	if err != nil {
		t.Fatalf("提交审批单失败: %v", err)
	}
	return tk
}

// TestSelfApprovalRejected L3 职责分离：自己不能批自己（表驱动覆盖各状态边界）。
func TestSelfApprovalRejected(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk := e.ticket(t, e.owner)

	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.owner, URL: "/shops"}); err == nil {
		t.Fatal("自己审批自己必须被拒绝")
	} else if !apierr.Is(err, apierr.CodeForbidden) {
		t.Fatalf("错误码应为 forbidden，实际 %v", err)
	}

	// 他人审批 → 通过
	got, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.leader, URL: "/shops"})
	if err != nil {
		t.Fatalf("他人审批应放行: %v", err)
	}
	if got.Status != types.ApprovalApproved || got.ApproverID != e.leader.UserID {
		t.Fatalf("审批结果异常: %+v", got)
	}
	// 重复审批必须拒绝（幂等保护）
	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.leader, URL: "/shops"}); err == nil {
		t.Fatal("已处理的单据不能再次审批")
	}
}

// TestVerifyAndOneTimeUse 校验规则 + 一次性核销（表驱动）。
func TestVerifyAndOneTimeUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk := e.ticket(t, e.owner)

	cases := []struct {
		name     string
		ticketID string
		typ      string
		who      string
		wantErr  bool
	}{
		{"未审批不能用于明文查看", tk.ID, types.ApprovalTypeCredReveal, e.owner.UserID, true},
		{"空单号被拒", "", types.ApprovalTypeCredReveal, e.owner.UserID, true},
	}
	for _, tc := range cases {
		if _, err := e.svc.Verify(ctx, "t1", tc.ticketID, tc.typ, tc.who); (err != nil) != tc.wantErr {
			t.Fatalf("%s: 期望 err=%v，实际 %v", tc.name, tc.wantErr, err)
		}
	}

	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.leader, URL: "/shops"}); err != nil {
		t.Fatal(err)
	}
	// 类型不匹配
	if _, err := e.svc.Verify(ctx, "t1", tk.ID, types.ApprovalTypeAuditExport, e.owner.UserID); err == nil {
		t.Fatal("类型不匹配应拒绝")
	}
	// 换人使用（不是申请人）应拒绝
	if _, err := e.svc.Verify(ctx, "t1", tk.ID, types.ApprovalTypeCredReveal, e.leader.UserID); err == nil {
		t.Fatal("非申请人使用应拒绝")
	}
	// 正常使用
	if _, err := e.svc.Verify(ctx, "t1", tk.ID, types.ApprovalTypeCredReveal, e.owner.UserID); err != nil {
		t.Fatalf("已通过的单据应可用: %v", err)
	}
	// 核销后不可再用
	if err := e.svc.Consume(ctx, "t1", tk.ID, e.owner, eventbus.Terminal{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Verify(ctx, "t1", tk.ID, types.ApprovalTypeCredReveal, e.owner.UserID); err == nil {
		t.Fatal("核销后不应再可用（一次性）")
	}
}

// TestExpiry 过期：未决单据超时自动失效，已过期不能审批也不能使用。
func TestExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc = approval.NewService(e.st, eventbus.New(e.st, config.Default(), slog.Default()),
		func() *config.Config { c := config.Default(); c.Limits.ApprovalTTLSeconds = 1; return c }(), iam.NewEngine(config.Default()))
	tk := e.ticket(t, e.owner)
	time.Sleep(1100 * time.Millisecond)

	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.leader, URL: "/shops"}); err == nil {
		t.Fatal("过期单据不能审批")
	}
	if n, err := e.svc.ExpireSweep(ctx, "t1", time.Now().UTC()); err != nil || n != 1 {
		t.Fatalf("过期扫描应命中 1 条，实际 %d err=%v", n, err)
	}
	got, _, err := e.svc.List(ctx, e.owner, store.ApprovalFilter{Limit: 10}, "/shops")
	if err != nil || len(got) != 1 || got[0].Status != types.ApprovalExpired {
		t.Fatalf("状态应为 expired: %+v err=%v", got, err)
	}
}

// TestExpiryNotifiesRequester 过期必须「被看见」：投递 iam.approval.expired 并定向通知申请人。
//
// 这条断言防的是一类很难靠肉眼发现的回归——以前 ExpireSweep 只改状态不发事件，
// 而通知模板早已写好，结果申请人永远不知道自己的单据悄悄失效了。
// 同时验证风险等级不能用 low：notify.Sink 会直接丢弃 low 风险事件。
func TestExpiryNotifiesRequester(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bus := eventbus.New(e.st, config.Default(), slog.Default())
	bus.Register(notify.NewSink(e.st, slog.Default()))
	cfg := config.Default()
	cfg.Limits.ApprovalTTLSeconds = 1
	svc := approval.NewService(e.st, bus, cfg, iam.NewEngine(config.Default()))

	if _, err := svc.Create(ctx, approval.CreateInput{
		Type: types.ApprovalTypeCredReveal, SubjectType: "shop", SubjectID: "shop-1",
		Reason: "排查登录异常", Actor: e.owner, URL: "/shops",
	}); err != nil {
		t.Fatalf("提交审批单失败: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := svc.ExpireSweep(ctx, "t1", time.Now().UTC()); err != nil || n != 1 {
		t.Fatalf("过期扫描应命中 1 条，实际 %d err=%v", n, err)
	}

	// 派发：注意 mem 的 Claim 会把事件置为 claimed，故这里直接派发、不预先查发件箱，
	// 用「通知最终落在谁头上」来反证事件确实投递了。
	if _, err := bus.DispatchOnce(ctx); err != nil {
		t.Fatalf("事件派发失败: %v", err)
	}

	// 申请人必须收到过期通知——这条同时证明：
	//  1. 事件真的发出去了（不再是只改状态）；
	//  2. 风险等级没设置在 low（notify.Sink 会丢弃 low）；
	//  3. 通知定向到申请人本人。
	list, err := e.st.Notifications().List(ctx, "t1", e.owner.UserID, 20)
	if err != nil {
		t.Fatalf("读取通知失败: %v", err)
	}
	hit := false
	for _, n := range list {
		if n.EventType == approval.TypeExpired {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("申请人应收到过期通知，实际收到 %d 条：%+v", len(list), list)
	}

	// 定向而非广播：审批人（运营主管）不该被这条过期通知打扰。
	lead, err := e.st.Notifications().List(ctx, "t1", e.leader.UserID, 20)
	if err != nil {
		t.Fatalf("读取审批人通知失败: %v", err)
	}
	for _, n := range lead {
		if n.EventType == approval.TypeExpired {
			t.Fatal("过期通知应只给申请人，审批人不应收到")
		}
	}
}

// TestCancelOnlyByRequester 只有申请人能撤销自己的单据。
func TestCancelOnlyByRequester(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk := e.ticket(t, e.owner)
	if _, err := e.svc.Cancel(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.leader, URL: "/shops"}); err == nil {
		t.Fatal("他人不能撤销申请人的单据")
	}
	got, err := e.svc.Cancel(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.owner, URL: "/shops"})
	if err != nil || got.Status != types.ApprovalCancelled {
		t.Fatalf("申请人应可撤销: %+v err=%v", got, err)
	}
}

// TestRequiredActions 需要二次审批的动作由配置中心决定（不硬编码，L10）。
func TestRequiredActions(t *testing.T) {
	e := newEnv(t)
	for _, a := range []string{types.ApprovalTypeAuditExport, types.ApprovalTypeCredReveal, types.ApprovalTypeBulkDelete} {
		if !e.svc.Required(a) {
			t.Fatalf("%s 应需要二次审批", a)
		}
	}
	if e.svc.Required("shop.create") {
		t.Fatal("建店不应需要二次审批")
	}
}

// TestUnknownTypeRejected 未知审批类型不允许提交（防绕过）。
func TestUnknownTypeRejected(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Create(context.Background(), approval.CreateInput{
		Type: "hack.my_way", Actor: e.owner, URL: "/shops",
	}); err == nil {
		t.Fatal("未知审批类型应拒绝")
	}
}
