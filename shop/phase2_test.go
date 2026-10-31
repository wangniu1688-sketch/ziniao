package shop_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ziniao/billing"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/shop"
)

func newShopEnv(t *testing.T) (*shop.Service, *mem.Store, iam.Subject) {
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
	bus := eventbus.New(st, cfg, nil)
	meter := billing.NewMeter(st)
	ent := billing.NewEntitlement(st, cfg)
	svc := shop.NewService(st, bus, cfg, iam.NewEngine(cfg), meter, ent)
	return svc, st, iam.Subject{UserID: "u1", TenantID: "t1", Username: "owner", Roles: []types.Role{*role}}
}

func subscribe(t *testing.T, st *mem.Store, tenantID, plan string) {
	t.Helper()
	now := time.Now()
	sub := types.Subscription{ID: "sub-" + tenantID, TenantID: tenantID, PlanCode: plan,
		Status: "active", StartedAt: now, ExpiresAt: now.AddDate(1, 0, 0)}
	if err := st.WithTx(context.Background(), func(tx store.Tx) error {
		return tx.Subscriptions().Create(context.Background(), &sub)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestBatchImportPartialSuccess 批量导入：逐行校验，配额超限的行给出可读原因（反例 11）。
func TestBatchImportPartialSuccess(t *testing.T) {
	svc, st, actor := newShopEnv(t)
	ctx := context.Background()
	subscribe(t, st, "t1", "free") // 免费版上限 3 家店

	rows := []shop.BatchImportRow{
		{Name: "店A", Platform: "Amazon", Site: "US"},
		{Name: "店B", Platform: "Shopee", Site: "MY"},
		{Name: "", Platform: "Amazon", Site: "US"}, // 空名称：校验失败
		{Name: "店C", Platform: "Amazon", Site: "JP", Group: "日本组"},
		{Name: "店D", Platform: "Amazon", Site: "DE"}, // 第 5 家：超配额
	}
	res, err := svc.BatchImport(ctx, shop.BatchImportInput{Rows: rows, Actor: actor, URL: "/shops"})
	if err != nil {
		t.Fatalf("批量导入失败: %v", err)
	}
	if res.Total != 5 || res.Created != 3 {
		t.Fatalf("期望 5 行中成功 3 行，实际 %+v", res)
	}
	if res.Failed != 2 || len(res.Failures) != 2 {
		t.Fatalf("应有 2 条失败明细，实际 %+v", res.Failures)
	}
	joined := strings.ToLower(res.Failures[0].Reason + res.Failures[1].Reason)
	if !strings.Contains(joined, "配额") && !strings.Contains(joined, "长度") {
		t.Fatalf("失败原因应可读（配额/长度），实际 %+v", res.Failures)
	}
}

// TestTrashAndPurge 回收站：未超留存期不清理，超期才彻底删除（M2.1）。
func TestTrashAndPurge(t *testing.T) {
	svc, st, actor := newShopEnv(t)
	ctx := context.Background()
	subscribe(t, st, "t1", "pro")

	sp, err := svc.Create(ctx, shop.CreateInput{Name: "待删店铺", Actor: actor, URL: "/shops"})
	if err != nil {
		t.Fatal(err)
	}
	// 未软删就物理删除 → 拒绝（防误删在营店铺）
	if err := svc.Purge(ctx, shop.PurgeInput{ShopID: sp.ID, Actor: actor, URL: "/shops"}); err == nil {
		t.Fatal("未进回收站的店铺不允许彻底删除")
	}
	if err := svc.Delete(ctx, actor, sp.ID, "/shops", eventbus.Terminal{IP: "1.2.3.4"}); err != nil {
		t.Fatal(err)
	}
	trash, total, err := svc.Trash(ctx, "t1", 50, 0)
	if err != nil || total != 1 {
		t.Fatalf("回收站应有 1 条，实际 %d err=%v", total, err)
	}
	// 未超 30 天留存期 → 不清
	n, err := svc.PurgeExpired(ctx, "t1", 30, eventbus.Terminal{Mode: "job"})
	if err != nil || n != 0 {
		t.Fatalf("未超留存期不应清理，实际 %d err=%v", n, err)
	}
	// 把软删时间回拨到留存期之前（模拟已在回收站躺了 40 天）
	tctx := store.WithTenant(ctx, "t1")
	old := time.Now().AddDate(0, 0, -40)
	sp.DeletedAt = &old
	if err := st.Shops().Update(tctx, sp); err != nil {
		t.Fatal(err)
	}
	n, err = svc.PurgeExpired(ctx, "t1", 30, eventbus.Terminal{Mode: "job"})
	if err != nil || n != 1 {
		t.Fatalf("超期应清理 1 条，实际 %d err=%v", n, err)
	}
	_, total, _ = svc.Trash(ctx, "t1", 50, 0)
	if total != 0 {
		t.Fatalf("清理后回收站应为空，实际 %d", total)
	}
	_ = trash
}

// TestRetentionDaysFromConfig 留存期来自配置中心（L10 禁止硬编码）。
func TestRetentionDaysFromConfig(t *testing.T) {
	cfg := config.Default()
	if got := shop.RetentionDays(cfg); got != cfg.Limits.ShopTrashRetentionDays || got != 30 {
		t.Fatalf("留存期应取配置值 30，实际 %d", got)
	}
	if got := shop.RetentionDays(nil); got != 30 {
		t.Fatalf("配置缺失时应有安全默认值，实际 %d", got)
	}
}
