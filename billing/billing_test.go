package billing_test

import (
	"context"
	"testing"
	"time"

	"ziniao/billing"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

func newEnv(t *testing.T) (store.Store, *billing.Meter, *billing.Entitlement) {
	t.Helper()
	st := mem.New()
	cfg := config.Default()
	return st, billing.NewMeter(st), billing.NewEntitlement(st, cfg)
}

// TestMeterRecords 计量：统一入口自增，且可与业务写同事务。
func TestMeterRecords(t *testing.T) {
	st, meter, _ := newEnv(t)
	ctx := context.Background()
	if err := st.WithTx(ctx, func(tx store.Tx) error {
		_, err := meter.Record(ctx, tx, "t1", billing.MetricShopCount, 2)
		return err
	}); err != nil {
		t.Fatalf("计量失败: %v", err)
	}
	used, err := meter.Current(ctx, "t1", billing.MetricShopCount)
	if err != nil || used != 2 {
		t.Fatalf("计量值应为 2，实际 %d err=%v", used, err)
	}
}

// TestQuotaExceeded 配额：免费版第 4 家店必须被拒（不能只靠 UI 灰按钮）。
func TestQuotaExceeded(t *testing.T) {
	st, meter, ent := newEnv(t)
	ctx := context.Background()
	sub1 := subscription("t1", "free")
	if err := st.WithTx(ctx, func(tx store.Tx) error {
		return tx.Subscriptions().Create(ctx, &sub1)
	}); err != nil {
		t.Fatalf("订阅写入失败: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := ent.CheckQuota(ctx, "t1", billing.MetricShopCount, 1); err != nil {
			t.Fatalf("第 %d 次应在配额内: %v", i+1, err)
		}
		if err := st.WithTx(ctx, func(tx store.Tx) error {
			_, err := meter.Record(ctx, tx, "t1", billing.MetricShopCount, 1)
			return err
		}); err != nil {
			t.Fatalf("计量失败: %v", err)
		}
	}
	err := ent.CheckQuota(ctx, "t1", billing.MetricShopCount, 1)
	if err == nil {
		t.Fatal("超过免费版上限应被拒绝")
	}
	if !isCode(err, "quota_exceeded") {
		t.Fatalf("错误码应为 quota_exceeded，实际 %v", err)
	}
}

// TestFeatureGate 功能开关：Agent / 取证包导出 属付费能力。
func TestFeatureGate(t *testing.T) {
	st, _, ent := newEnv(t)
	ctx := context.Background()
	sub2 := subscription("t2", "free")
	if err := st.WithTx(ctx, func(tx store.Tx) error {
		return tx.Subscriptions().Create(ctx, &sub2)
	}); err != nil {
		t.Fatalf("订阅写入失败: %v", err)
	}
	if err := ent.RequireFeature(ctx, "t2", "agent"); err == nil {
		t.Fatal("免费版不应包含 Agent")
	}
	sub3 := subscription("t3", "pro")
	if err := st.WithTx(ctx, func(tx store.Tx) error {
		return tx.Subscriptions().Create(ctx, &sub3)
	}); err != nil {
		t.Fatalf("订阅写入失败: %v", err)
	}
	if err := ent.RequireFeature(ctx, "t3", "agent"); err != nil {
		t.Fatalf("专业版应包含 Agent: %v", err)
	}
}

func subscription(tenantID, plan string) types.Subscription {
	now := time.Now()
	return types.Subscription{
		ID: "sub-" + tenantID, TenantID: tenantID, PlanCode: plan,
		Status: "active", StartedAt: now, ExpiresAt: now,
	}
}

func isCode(err error, code apierr.Code) bool { return apierr.Is(err, code) }
