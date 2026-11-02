package pg

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/ids"
	"ziniao/platform/migrate"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TestDelegationFlowPG 委托审批仓储的真库验证（默认跳过）：
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestDelegationFlowPG -v -count=1
//
// 为什么非要跑真库：这次新增的东西有三处只有 PG 才管得住——
//
//	① approval_flows 的**双主体 RLS**（OR agency_tenant_id）策略是否真的放行两方、
//	  挡住第三方（mem 无 RLS，测不出来）；
//	② 迁移 0007 能否在真库上应用（外键 / 索引 / 部分索引）；
//	③ 仓储谓词与 RLS 口径是否一致（Get/Update 的 OR 匹配、HasPendingBetween 的显式双 ID）。
func TestDelegationFlowPG(t *testing.T) {
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
		t.Fatalf("迁移失败（0007 是否能应用？）: %v", err)
	}

	// 三个租户：g=委托方，a=受托方，x=无关方（验证 RLS 挡第三方）
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	gT, aT, xT := "tg-"+suffix, "ta-"+suffix, "tx-"+suffix
	for _, tc := range []struct{ id, slug string }{{gT, "g-" + suffix}, {aT, "a-" + suffix}, {xT, "x-" + suffix}} {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
			VALUES($1,$2,$3,'active','pro',now())`, tc.id, tc.id, tc.slug); err != nil {
			t.Fatalf("建测试租户失败: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, tid := range []string{gT, aT, xT} {
			_, _ = st.Pool().Exec(context.Background(), `DELETE FROM approval_flows WHERE tenant_id=$1 OR agency_tenant_id=$1`, tid)
			_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE id=$1`, tid)
		}
	})

	repo := st.Delegations()
	now := time.Now().UTC().Truncate(time.Second)
	flow := &types.DelegationFlow{
		ID: ids.New(), AgencyTenantID: aT, AgencyName: "代运营公司",
		GranteeID: "u-a", GranteeName: "agency",
		ShopIDs: []string{"s1", "s2"}, Reason: "代运营合作",
		Status: types.DelegationPending, Detail: map[string]any{"valid_days": 30},
		ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now,
	}
	// 1) 委托方上下文写入（生产路径：Submit 在委托方上下文创建单据，tenant_id=委托方）
	if err := repo.Create(store.WithTenant(ctx, gT), flow); err != nil {
		t.Fatalf("委托方上下文写入失败: %v", err)
	}
	// 2) 双方都能读（RLS 双主体：OR agency_tenant_id 放行受托方）
	if got, err := repo.Get(store.WithTenant(ctx, gT), flow.ID); err != nil || got == nil {
		t.Fatalf("委托方读取失败: %v %v", got, err)
	}
	gotA, err := repo.Get(store.WithTenant(ctx, aT), flow.ID)
	if err != nil || gotA == nil {
		t.Fatalf("受托方读取失败（RLS 双主体未放行受托方？）: %v %v", gotA, err)
	}
	if len(gotA.ShopIDs) != 2 {
		t.Fatalf("scope jsonb 回读 shop_ids 应为 2 条: %v", gotA.ShopIDs)
	}
	// 3) 第三方读不到（RLS fail-closed）
	if _, err := repo.Get(store.WithTenant(ctx, xT), flow.ID); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("无关租户读取应返回 NotFound（RLS 未挡住？）: %v", err)
	}
	// 4) HasPendingBetween：双方各查一次
	if ok, err := repo.HasPendingBetween(store.WithTenant(ctx, aT), gT, aT); err != nil || !ok {
		t.Fatalf("受托方上下文查询 pending 失败: %v %v", ok, err)
	}
	if ok, _ := repo.HasPendingBetween(store.WithTenant(ctx, aT), gT, xT); ok {
		t.Fatal("不同受托方不应命中 pending")
	}
	// 5) 受托方上下文更新（撤回场景：谓词与 RLS 都走 OR agency_tenant_id 分支）
	gotA.Status = types.DelegationCancelled
	gotA.DecidedAt = &now
	if err := repo.Update(store.WithTenant(ctx, aT), gotA); err != nil {
		t.Fatalf("受托方上下文更新失败: %v", err)
	}
	// 6) 列表：委托方视角（默认）与受托方视角（party=agency）各自可见
	listG, totalG, err := repo.List(store.WithTenant(ctx, gT), store.DelegationFilter{})
	if err != nil || totalG != 1 {
		t.Fatalf("委托方列表失败: total=%d err=%v", totalG, err)
	}
	if listG[0].ID != flow.ID {
		t.Fatalf("委托方列表应含该申请: %v", listG)
	}
	listA, totalA, err := repo.List(store.WithTenant(ctx, aT), store.DelegationFilter{Party: "agency"})
	if err != nil || totalA != 1 {
		t.Fatalf("受托方列表失败: total=%d err=%v", totalA, err)
	}
	if listA[0].ID != flow.ID {
		t.Fatalf("受托方列表应含自己的申请: %v", listA)
	}
	// 7) 状态筛选
	if _, total, _ := repo.List(store.WithTenant(ctx, gT), store.DelegationFilter{Status: types.DelegationPending}); total != 0 {
		t.Fatal("cancelled 单不应出现在 pending 筛选里")
	}
}
