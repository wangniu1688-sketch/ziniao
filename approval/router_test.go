package approval_test

import (
	"context"
	"testing"

	"ziniao/approval"
	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TestRequiredForRouting 路由规则：单据类型与金额决定审批级别。
func TestRequiredForRouting(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name      string
		ticket    *types.ApprovalTicket
		wantRoles []string
		wantMin   int
	}{
		{"揭示店铺口令需管理员", &types.ApprovalTicket{Type: types.ApprovalTypeCredReveal}, []string{"owner", "ops_manager"}, 1},
		{"明文导出全量审计仅老板", &types.ApprovalTicket{Type: types.ApprovalTypeAuditExport}, []string{"owner"}, 1},
		{"批量删除需管理员", &types.ApprovalTicket{Type: types.ApprovalTypeBulkDelete}, []string{"owner", "ops_manager"}, 1},
		{"设备申购小额走管理员",
			&types.ApprovalTicket{Type: types.ApprovalTypeDevicePurchase, Detail: map[string]any{"amount": 1000}},
			[]string{"owner", "ops_manager"}, 1},
		{"设备申购超阈值需两位老板复核",
			&types.ApprovalTicket{Type: types.ApprovalTypeDevicePurchase, Detail: map[string]any{"amount": 999999}},
			[]string{"owner"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := e.svc.RequiredFor(context.Background(), tc.ticket)
			if len(got.Roles) != len(tc.wantRoles) {
				t.Fatalf("要求角色应为 %v，实际 %v", tc.wantRoles, got.Roles)
			}
			for i, r := range tc.wantRoles {
				if got.Roles[i] != r {
					t.Fatalf("第 %d 个角色应 %s，实际 %s", i, r, got.Roles[i])
				}
			}
			if got.MinApprovers != tc.wantMin {
				t.Fatalf("最少签署人应 %d，实际 %d", tc.wantMin, got.MinApprovers)
			}
			if got.Reason == "" {
				t.Fatal("必须给出可读原因：申请人需要知道这条单为什么要求这个级别")
			}
			// 未配置规则时必须走内置：老租户升级后行为不变，这条是最容易被改坏的
			if got.Source != "builtin" {
				t.Fatalf("无租户规则时应走内置规则，实际 source=%s", got.Source)
			}
		})
	}
}

// TestCosignRequiresQuorum 会签：大额申购需两位老板签字，
// 第一人签署后单据仍是 pending，达到法定人数才通过。
func TestCosignRequiresQuorum(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	tk, err := e.svc.Create(ctx, approval.CreateInput{
		Type: types.ApprovalTypeDevicePurchase, SubjectType: "device", SubjectID: "dev-1",
		Reason: "扩容换机", Detail: map[string]any{"amount": 999999},
		Actor: e.leader, URL: "/devices",
	})
	if err != nil {
		t.Fatalf("提交申购单失败: %v", err)
	}
	// 运营主管不该能批这条（要求 owner）
	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.leader, URL: "/devices"}); err == nil {
		t.Fatal("申请人与审批人不能是同一人，且该单要求 owner 角色")
	}
	// 第一位老板签署：应仍是 pending
	first, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.owner, URL: "/devices"})
	if err != nil {
		t.Fatalf("首位老板签署失败: %v", err)
	}
	if first.Status != types.ApprovalPending {
		t.Fatalf("未达法定人数时应保持 pending，实际 %s", first.Status)
	}
	// 同一人重复签署：必须被拒（否则一人可以刷够票数）
	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.owner, URL: "/devices"}); err == nil {
		t.Fatal("同一人重复签署不能累计票数")
	}
	// 第二位老板签署：达到法定人数 -> 通过
	second, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.boss2, URL: "/devices"})
	if err != nil {
		t.Fatalf("第二位老板签署失败: %v", err)
	}
	if second.Status != types.ApprovalApproved {
		t.Fatalf("达到法定人数后应 approved，实际 %s", second.Status)
	}
}

// TestStaffCannotApproveHighRiskTicket 加固的核心：
// 普通运营即使持有 approval:approve 权限，也不能批准揭示店铺口令。
// 这条如果被绕过，等于店铺主账号可以被团队内任意运营直接窃取。
func TestStaffCannotApproveHighRiskTicket(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	tk, err := e.svc.Create(ctx, approval.CreateInput{
		Type: types.ApprovalTypeCredReveal, SubjectType: "shop", SubjectID: "shop-1",
		Reason: "排查登录异常", Actor: e.owner, URL: "/shops",
	})
	if err != nil {
		t.Fatalf("提交审批单失败: %v", err)
	}
	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.staff, URL: "/shops"}); err == nil {
		t.Fatal("普通运营不该能批准揭示店铺口令")
	} else if !apierr.Is(err, apierr.CodeForbidden) {
		t.Fatalf("错误码应为 forbidden，实际 %v", err)
	}
	// 被拒后单据必须仍是 pending，不能被误当成已处理
	cur, err := e.st.Approvals().Get(store.WithTenant(ctx, "t1"), tk.ID)
	if err != nil {
		t.Fatalf("读取审批单失败: %v", err)
	}
	if cur.Status != types.ApprovalPending {
		t.Fatalf("审批被拒后状态应仍为 pending，实际 %s", cur.Status)
	}
}

// TestAuditExportRequiresOwnerNotManager 导出全量审计日志级别最高：运营主管也不行。
func TestAuditExportRequiresOwnerNotManager(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	tk, err := e.svc.Create(ctx, approval.CreateInput{
		Type: types.ApprovalTypeAuditExport, SubjectType: "tenant", SubjectID: "t1",
		Reason: "合规自查", Actor: e.boss2, URL: "/audit",
	})
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if _, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.leader, URL: "/audit"}); err == nil {
		t.Fatal("明文导出全量审计日志不该由运营主管批准（影响面覆盖全团队操作记录）")
	}
	// 换成老板审批应通过
	got, err := e.svc.Approve(ctx, approval.DecideInput{TicketID: tk.ID, Actor: e.owner, URL: "/audit"})
	if err != nil {
		t.Fatalf("老板审批应通过: %v", err)
	}
	if got.Status != types.ApprovalApproved {
		t.Fatalf("状态应为 approved，实际 %s", got.Status)
	}
}
