package pg

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"ziniao/platform/migrate"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TestApprovalRouteRulesPG 审批路由规则在真库上的行为（迁移 0010）。
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestApprovalRouteRulesPG -v -count=1
//
// 为什么必须拿真库锁：
//   - numeric(14,2) 与 float64 的相互转换（写入要能塞、读回要等值是
//     `min_amount::float8` 那句 CAST 才成立的东西）mem 根本不涉及；
//   - text[] NOT NULL 的 nil 切片陷阱（PG 那轮踩过的 NULL 坑）由 strArr 兜底，
//     但只有真库能证明它真的没写进 NULL；
//   - ticket_type / min_approvers 的 CHECK、RLS 租户隔离，mem 驱动一概没有。
func TestApprovalRouteRulesPG(t *testing.T) {
	dsn := os.Getenv("ZINIAO_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 ZINIAO_PG_DSN，跳过 PG 实跑用例")
	}
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := migrate.Up(ctx, st.Pool()); err != nil {
		t.Fatalf("迁移失败（0010 未应用？）: %v", err)
	}

	tid := "t-rules-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	other := tid + "-other"
	// 本表 tenant_id 带 REFERENCES tenants(id)，测试租户必须先存在
	for _, tc := range []struct{ id, slug string }{{tid, "r-" + tid}, {other, "ro-" + tid}} {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
			VALUES($1,$2,$3,'active','pro',now())`, tc.id, tc.id, tc.slug); err != nil {
			t.Fatalf("建测试租户失败: %v", err)
		}
	}
	tctx := store.WithTenant(ctx, tid)
	defer func() {
		// 清理放在带租户上下文的事务里：将来若给这张表加 FORCE RLS，
		// 裸 DELETE 会「成功但零行」，残留行让下一次跑用例时主键冲突。
		tx, err := st.Pool().Begin(context.Background())
		if err != nil {
			t.Logf("清理测试数据失败: %v", err)
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(context.Background(),
			`SELECT set_config('app.tenant_id', $1, true)`, tid); err != nil {
			t.Logf("清理测试数据失败: %v", err)
			return
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM approval_route_rules WHERE tenant_id = ANY($1)`, []string{tid, other}); err != nil {
			t.Logf("清理测试数据失败: %v", err)
			return
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM tenants WHERE id = ANY($1)`, []string{tid, other}); err != nil {
			t.Logf("清理测试租户失败: %v", err)
			return
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Logf("清理测试数据失败: %v", err)
		}
	}()

	repo := st.ApprovalRouteRules()
	now := time.Now().UTC().Truncate(time.Second)

	// 1) 写入 + 读回：金额与角色数组必须等值（numeric::float8 与 text[] 的扫描）
	r1 := &types.ApprovalRouteRule{
		ID: tid + "-r1", TenantID: tid, TicketType: types.ApprovalTypeDevicePurchase,
		MinAmount: 12345.67, Roles: []string{"owner"}, MinApprovers: 2,
		Reason: "大额申购双人复核", Priority: 10, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(tctx, r1); err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}
	got, err := repo.Get(tctx, r1.ID)
	if err != nil {
		t.Fatalf("读取规则失败: %v", err)
	}
	if got.MinAmount != 12345.67 {
		t.Fatalf("金额读回不等值: 期望 12345.67，实际 %v", got.MinAmount)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "owner" {
		t.Fatalf("角色数组读回异常: %v", got.Roles)
	}
	if got.MinApprovers != 2 || !got.Enabled || got.Priority != 10 {
		t.Fatalf("规则字段读回异常: %+v", got)
	}

	// 2) 空角色集：nil 切片不能被写成 NULL（text[] NOT NULL）
	empty := &types.ApprovalRouteRule{
		ID: tid + "-r2", TenantID: tid, TicketType: types.ApprovalTypeBulkDelete,
		Roles: nil, MinApprovers: 1, Priority: 20, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(tctx, empty); err != nil {
		t.Fatalf("空角色集写入失败（nil 切片被编码成 NULL？）: %v", err)
	}
	got2, err := repo.Get(tctx, empty.ID)
	if err != nil {
		t.Fatalf("读取空角色集规则失败: %v", err)
	}
	if got2.Roles == nil {
		t.Fatal("空角色集读回应为非 nil 空切片")
	}
	if len(got2.Roles) != 0 {
		t.Fatalf("空角色集读回应为空，实际 %v", got2.Roles)
	}

	// 3) 列表按优先级升序
	list, err := repo.ListByTenant(tctx, tid)
	if err != nil {
		t.Fatalf("列规则失败: %v", err)
	}
	if len(list) != 2 || list[0].ID != r1.ID {
		t.Fatalf("应按优先级升序返回，实际 %+v", list)
	}

	// 4) CHECK：未知单据类型不得落库（双保险里的库侧那一道）
	bad := &types.ApprovalRouteRule{
		ID: tid + "-bad", TenantID: tid, TicketType: "shop.whatever",
		Roles: []string{"owner"}, MinApprovers: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(tctx, bad); err == nil {
		t.Fatal("未知单据类型应被 CHECK 拒绝")
	}
	// 会签人数上限同理（配成 99 = 这张单永远批不出来）
	bad2 := *bad
	bad2.ID, bad2.TicketType, bad2.MinApprovers = tid+"-bad2", types.ApprovalTypeBulkDelete, 99
	if err := repo.Create(tctx, &bad2); err == nil {
		t.Fatal("会签人数超上限应被 CHECK 拒绝")
	}

	// 5) 租户隔离：别家团队的规则既读不到也删不掉
	otctx := store.WithTenant(ctx, other)
	if _, err := repo.Get(otctx, r1.ID); err == nil {
		t.Fatal("跨租户读取规则应失败")
	}
	if err := repo.Delete(otctx, r1.ID); err == nil {
		t.Fatal("跨租户删除规则应失败")
	}

	// 6) 更新与删除
	r1.MinApprovers = 3
	r1.UpdatedAt = now.Add(time.Second)
	if err := repo.Update(tctx, r1); err != nil {
		t.Fatalf("更新规则失败: %v", err)
	}
	if got, _ = repo.Get(tctx, r1.ID); got.MinApprovers != 3 {
		t.Fatalf("更新未生效: %+v", got)
	}
	if err := repo.Delete(tctx, r1.ID); err != nil {
		t.Fatalf("删除规则失败: %v", err)
	}
	if err := repo.Delete(tctx, r1.ID); err == nil {
		t.Fatal("重复删除应报不存在（不能假成功）")
	}
}
