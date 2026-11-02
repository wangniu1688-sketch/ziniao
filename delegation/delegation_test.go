package delegation_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"ziniao/delegation"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

// 脚手架：t1 = 委托方（店铺主），t2 = 受托方（代理机构）。
// u1 = 委托方 owner，u2 = 受托方 owner，u3 = 受托方无权成员（客服）。
type fixture struct {
	svc     *delegation.Service
	st      *mem.Store
	grantor iam.Subject // u1 @ t1
	agency  iam.Subject // u2 @ t2
	agency2 iam.Subject // u4 @ t3（第二家受托方，测试里避开 pending 唯一性闸门）
	cs      iam.Subject // u3 @ t2（客服，无委托权限）
	shop1   string
	shop2   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New(st, cfg, nil)
	svc := delegation.NewService(st, bus, cfg, iam.NewEngine(cfg))

	ownerRole, err := st.Roles().GetByCode(ctx, "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	csRole, err := st.Roles().GetByCode(ctx, "", "cs")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tenants().Create(ctx, &types.Tenant{ID: "t1", Name: "店铺主", Slug: "grantor-shop", Status: types.TenantStatusActive, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenants().Create(ctx, &types.Tenant{ID: "t2", Name: "代运营公司", Slug: "agency-shop", Status: types.TenantStatusActive, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenants().Create(ctx, &types.Tenant{ID: "t3", Name: "第二家代运营", Slug: "agency2-shop", Status: types.TenantStatusActive, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// 受托方 owner 要有 delegation:apply —— owner 角色是权限全集，天然满足
	f := &fixture{
		svc: svc, st: st,
		grantor: iam.Subject{UserID: "u1", TenantID: "t1", Username: "boss", Roles: []types.Role{*ownerRole}},
		agency:  iam.Subject{UserID: "u2", TenantID: "t2", Username: "agency", Roles: []types.Role{*ownerRole}},
		agency2: iam.Subject{UserID: "u4", TenantID: "t3", Username: "agency2", Roles: []types.Role{*ownerRole}},
		cs:      iam.Subject{UserID: "u3", TenantID: "t2", Username: "cs", Roles: []types.Role{*csRole}},
	}
	tctx := store.WithTenant(ctx, "t1")
	for i, id := range []string{"s1", "s2"} {
		if err := st.Shops().Create(tctx, &types.Shop{
			ID: id, TenantID: "t1", Name: "店铺" + strconv.Itoa(i+1), Status: "正常运营", OwnerID: "u0", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.shop1, f.shop2 = "s1", "s2"
	return f
}

func (f *fixture) submit(t *testing.T, actor iam.Subject, shopIDs ...string) *types.DelegationFlow {
	t.Helper()
	flow, err := f.svc.Submit(context.Background(), delegation.SubmitInput{
		GrantorSlug: "grantor-shop", ShopIDs: shopIDs, ValidDays: 30, Reason: "代运营合作",
		Actor: actor, URL: "/delegations",
	})
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

// submitAs 与 submit 相同（submit 以 agency 为默认主体；保留命名以区分用例意图）。
func (f *fixture) submitAs(t *testing.T, actor iam.Subject, shopIDs ...string) *types.DelegationFlow {
	return f.submit(t, actor, shopIDs...)
}

// TestSubmitValidations 提交侧的闸门：范围归属、自我委托、重复申请一个都不能漏。
func TestSubmitValidations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.svc.Submit(ctx, delegation.SubmitInput{GrantorSlug: "nope", ShopIDs: []string{"s1"}, Actor: f.agency}); err == nil {
		t.Fatal("不存在的委托方应被拒绝")
	}
	if _, err := f.svc.Submit(ctx, delegation.SubmitInput{GrantorSlug: "grantor-shop", ShopIDs: nil, Actor: f.agency}); err == nil {
		t.Fatal("空范围应被拒绝")
	}
	if _, err := f.svc.Submit(ctx, delegation.SubmitInput{GrantorSlug: "agency-shop", ShopIDs: []string{"s1"}, Actor: f.agency}); err == nil {
		t.Fatal("向自己团队提交委托应被拒绝")
	}
	// 混入不属于委托方的店铺：整体拒绝而不是悄悄缩小范围
	if _, err := f.svc.Submit(ctx, delegation.SubmitInput{GrantorSlug: "grantor-shop", ShopIDs: []string{"s1", "s999"}, Actor: f.agency}); err == nil {
		t.Fatal("范围含不存在店铺应整体拒绝")
	}
	if _, err := f.svc.Submit(ctx, delegation.SubmitInput{GrantorSlug: "grantor-shop", ShopIDs: []string{"s1"}, Actor: f.cs}); err == nil {
		t.Fatal("客服不应能提交委托申请")
	}
	flow := f.submit(t, f.agency, f.shop1, f.shop2)
	if flow.Status != types.DelegationPending {
		t.Fatalf("新申请应为 pending，实际 %s", flow.Status)
	}
	if _, err := f.svc.Submit(ctx, delegation.SubmitInput{GrantorSlug: "grantor-shop", ShopIDs: []string{"s1"}, Actor: f.agency}); !apierr.Is(err, apierr.CodeConflict) {
		t.Fatalf("重复申请应返回 409，实际 %v", err)
	}
}

// TestApproveEffect 批准 = 授权落地：成员身份 + agency_ops 角色 + 每店一条带到期的 DataGrant。
func TestApproveEffect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	flow := f.submit(t, f.agency, f.shop1, f.shop2)

	if _, err := f.svc.Approve(ctx, delegation.DecideInput{FlowID: flow.ID, Actor: f.grantor, URL: "/delegations"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Delegations().Get(store.WithTenant(ctx, "t1"), flow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.DelegationActive {
		t.Fatalf("批准后应为 active，实际 %s", got.Status)
	}
	if got.ValidUntil == nil || !got.ValidUntil.After(time.Now().UTC()) {
		t.Fatal("批准后必须写 valid_until（委托到期时间）")
	}
	if got.ApproverID != "u1" {
		t.Fatalf("审批人应记录在案: %q", got.ApproverID)
	}
	tctx := store.WithTenant(ctx, "t1")
	if ms, err := f.st.Memberships().Get(tctx, "t1", "u2"); err != nil || ms == nil {
		t.Fatalf("批准后受托方申请人应成为委托方成员: %v %v", ms, err)
	}
	role, err := f.st.Roles().GetByCode(tctx, "", "agency_ops")
	if err != nil {
		t.Fatal(err)
	}
	urs, err := f.st.UserRoles().ListByUser(tctx, "t1", "u2")
	if err != nil {
		t.Fatal(err)
	}
	hasRole := false
	for _, ur := range urs {
		if ur.RoleID == role.ID {
			hasRole = true
		}
	}
	if !hasRole {
		t.Fatal("批准后应授予 agency_ops 角色")
	}
	grants, err := f.st.DataGrants().ListByUser(tctx, "t1", "u2", "shop")
	if err != nil || len(grants) != 2 {
		t.Fatalf("应有 2 条店铺级授权: n=%d err=%v", len(grants), err)
	}
	for _, g := range grants {
		if g.ExpiresAt == nil || !g.ExpiresAt.Equal(*got.ValidUntil) {
			t.Fatal("店铺授权必须带与委托一致的到期时间")
		}
		if g.Scope != "delegation:"+flow.ID {
			t.Fatalf("授权 scope 应可溯源到委托单: %q", g.Scope)
		}
	}
	// 受托方不能批自己的申请（它手上也有 delegation:approve，必须按单据内容拦）
	flow2 := f.submit(t, f.agency, f.shop1)
	if _, err := f.svc.Approve(ctx, delegation.DecideInput{FlowID: flow2.ID, Actor: f.agency}); err == nil {
		t.Fatal("受托方不应能批准自己的申请")
	}
	// 无 owner/ops_manager 角色不能批
	if _, err := f.svc.Approve(ctx, delegation.DecideInput{FlowID: flow2.ID, Actor: iam.Subject{UserID: "u9", TenantID: "t1"}}); err == nil {
		t.Fatal("无 owner/ops_manager 角色不应能批准")
	}
	// 重复裁决
	if _, err := f.svc.Approve(ctx, delegation.DecideInput{FlowID: flow.ID, Actor: f.grantor}); !apierr.Is(err, apierr.CodeConflict) {
		t.Fatalf("已批准的委托单不能再次裁决: %v", err)
	}
}

// TestApproveDisabledMember 委托方停用了该成员：批准必须明确报错而不是发一份死授权。
func TestApproveDisabledMember(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.st.Memberships().Create(store.WithTenant(ctx, "t1"), &types.Membership{
		ID: "m1", TenantID: "t1", UserID: "u2", Status: "disabled", JoinedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	flow := f.submit(t, f.agency, f.shop1)
	if _, err := f.svc.Approve(ctx, delegation.DecideInput{FlowID: flow.ID, Actor: f.grantor}); !apierr.Is(err, apierr.CodeConflict) {
		t.Fatalf("停用成员的委托应拒绝批准，实际 %v", err)
	}
}

// TestRejectAndCancel 拒绝（委托方）与撤回（仅受托方申请人本人）。
func TestRejectAndCancel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	flow := f.submit(t, f.agency, f.shop1)

	if _, err := f.svc.Reject(ctx, delegation.DecideInput{FlowID: flow.ID, Comment: "暂不合作", Actor: f.grantor}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Delegations().Get(store.WithTenant(ctx, "t1"), flow.ID)
	if got.Status != types.DelegationRejected || got.Comment != "暂不合作" {
		t.Fatalf("拒绝态不符: %+v", got)
	}

	flow2 := f.submit(t, f.agency, f.shop1)
	if _, err := f.svc.Cancel(ctx, delegation.DecideInput{FlowID: flow2.ID, Actor: f.cs}); err == nil {
		t.Fatal("非申请人不应能撤回")
	}
	if _, err := f.svc.Cancel(ctx, delegation.DecideInput{FlowID: flow2.ID, Actor: f.agency}); err != nil {
		t.Fatal(err)
	}
	got2, _ := f.st.Delegations().Get(store.WithTenant(ctx, "t2"), flow2.ID)
	if got2.Status != types.DelegationCancelled {
		t.Fatalf("撤回后应为 cancelled，实际 %s", got2.Status)
	}
	// 受托方视角能读到已处理单据的状态（双主体可读的关键场景）
	if _, err := f.st.Delegations().Get(store.WithTenant(ctx, "t2"), flow.ID); err != nil {
		t.Fatalf("受托方应能读到已处理的单据状态: %v", err)
	}
}

// TestRevokeRevokesAccess 撤销生效中的委托：授权即时整批回收，不等巡检。
func TestRevokeRevokesAccess(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	flow := f.submit(t, f.agency, f.shop1, f.shop2)
	if _, err := f.svc.Approve(ctx, delegation.DecideInput{FlowID: flow.ID, Actor: f.grantor}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Revoke(ctx, delegation.DecideInput{FlowID: flow.ID, Comment: "合作终止", Actor: f.grantor}); err != nil {
		t.Fatal(err)
	}
	tctx := store.WithTenant(ctx, "t1")
	grants, _ := f.st.DataGrants().ListByUser(tctx, "t1", "u2", "shop")
	if len(grants) != 0 {
		t.Fatalf("撤销后店铺授权应清零: %d", len(grants))
	}
	role, _ := f.st.Roles().GetByCode(tctx, "", "agency_ops")
	urs, _ := f.st.UserRoles().ListByUser(tctx, "t1", "u2")
	for _, ur := range urs {
		if ur.RoleID == role.ID {
			t.Fatal("撤销后 agency_ops 角色应被回收")
		}
	}
	got, _ := f.st.Delegations().Get(tctx, flow.ID)
	if got.Status != types.DelegationRevoked || got.RevokedAt == nil {
		t.Fatalf("撤销态不符: %+v", got)
	}
	if _, err := f.svc.Revoke(ctx, delegation.DecideInput{FlowID: flow.ID, Actor: f.grantor}); err == nil {
		t.Fatal("重复撤销应报错")
	}
}

// TestExpireSweep 过期巡检：pending 超期 → expired；active 到期 → expired + 回收；未到期的不动。
func TestExpireSweep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	tctx := store.WithTenant(ctx, "t1")

	// 1) pending：已过申请有效期（先过期再提交下一张——同对（委托方,受托方）同时只允许一张 pending）
	f1 := f.submit(t, f.agency, f.shop1)
	past, _ := f.st.Delegations().Get(tctx, f1.ID)
	past.ExpiresAt = now.Add(-time.Minute)
	if err := f.st.Delegations().Update(tctx, past); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.ExpireSweep(ctx, "t1", now); err != nil || n != 1 {
		t.Fatalf("预跑巡检应把 f1 置为 expired: n=%d err=%v", n, err)
	}
	// 2) pending：还在有效期内（巡检不得触碰）。用另一家受托方提交，
	//    避开「同对（委托方,受托方）同时只允许一张 pending」的闸门。
	f2 := f.submitAs(t, f.agency2, f.shop1)
	// 3) active：委托已到期（批准后回拨 valid_until）
	f3 := f.submit(t, f.agency, f.shop2)
	past3, _ := f.st.Delegations().Get(tctx, f3.ID)
	past3.ExpiresAt = now.Add(time.Hour)
	if err := f.st.Delegations().Update(tctx, past3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(ctx, delegation.DecideInput{FlowID: f3.ID, Actor: f.grantor}); err != nil {
		t.Fatal(err)
	}
	got3, _ := f.st.Delegations().Get(tctx, f3.ID)
	pastUntil := now.Add(-time.Minute)
	got3.ValidUntil = &pastUntil
	if err := f.st.Delegations().Update(tctx, got3); err != nil {
		t.Fatal(err)
	}
	// 4) active：尚未到期（巡检不得触碰）
	f4 := f.submit(t, f.agency, f.shop2)
	past4, _ := f.st.Delegations().Get(tctx, f4.ID)
	past4.ExpiresAt = now.Add(time.Hour)
	if err := f.st.Delegations().Update(tctx, past4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(ctx, delegation.DecideInput{FlowID: f4.ID, Actor: f.grantor}); err != nil {
		t.Fatal(err)
	}

	n, err := f.svc.ExpireSweep(ctx, "t1", now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("巡检应处理 1 张（到期委托；超期申请已在预跑轮处理），实际 %d", n)
	}
	st1, _ := f.st.Delegations().Get(tctx, f1.ID)
	if st1.Status != types.DelegationExpired {
		t.Fatalf("超期申请应为 expired，实际 %s", st1.Status)
	}
	st2, _ := f.st.Delegations().Get(tctx, f2.ID)
	if st2.Status != types.DelegationPending {
		t.Fatalf("未到期申请不应被巡检触碰，实际 %s", st2.Status)
	}
	st3, _ := f.st.Delegations().Get(tctx, f3.ID)
	if st3.Status != types.DelegationExpired {
		t.Fatalf("到期委托应为 expired，实际 %s", st3.Status)
	}
	grants, _ := f.st.DataGrants().ListByUser(tctx, "t1", "u2", "shop")
	if len(grants) != 1 {
		// f3（到期）的授权应被回收；f4（未到期）的 1 条仍在
		t.Fatalf("到期委托授权应回收、未到期委托授权保留，实际 %d 条", len(grants))
	}
	if grants[0].ResourceID != f.shop2 || grants[0].Scope != "delegation:"+f4.ID {
		t.Fatalf("留下的应是 f4 的授权: %+v", grants[0])
	}
	st4, _ := f.st.Delegations().Get(tctx, f4.ID)
	if st4.Status != types.DelegationActive {
		t.Fatalf("未到期委托不应被巡检触碰，实际 %s", st4.Status)
	}
	// 幂等：再跑一轮，除了 f4 不动之外不重复处理
	n2, err := f.svc.ExpireSweep(ctx, "t1", now)
	if err != nil || n2 != 0 {
		t.Fatalf("第二轮巡检不应有新增处理: n=%d err=%v", n2, err)
	}
}
