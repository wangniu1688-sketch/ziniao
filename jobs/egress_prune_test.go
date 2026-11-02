package jobs_test

import (
	"context"
	"testing"
	"time"

	"ziniao/jobs"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

// prunerAdapter 把 mem 仓储适配成任务的窄接口（与 app/jobs.go 的 egressHistoryPruner 同构）。
type prunerAdapter struct{ st *mem.Store }

func (p prunerAdapter) PruneEvents(ctx context.Context, tenantID string, before time.Time) (int64, error) {
	return p.st.EgressBindings().PruneEvents(ctx, tenantID, before)
}

func appendHist(t *testing.T, st *mem.Store, tenantID, id string, at time.Time) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), tenantID)
	if err := st.EgressBindings().AppendEvent(ctx, &types.EgressBindingEvent{
		ID: id, EnvID: "env-" + id, Action: types.EgressActionBound, OccurredAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

func histCount(t *testing.T, st *mem.Store, tenantID string) int {
	t.Helper()
	items, total, err := st.EgressBindings().ListEvents(store.WithTenant(context.Background(), tenantID),
		store.EgressHistoryFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	_ = items
	return total
}

// TestEgressHistoryPruneJob 保留期裁剪：只删本租户、严格早于边界的行。
// 多删一条边界上的事件，共用 ASN 的证据链就断一截——所以用 Before 而不是 <=。
func TestEgressHistoryPruneJob(t *testing.T) {
	st := mem.New()
	now := time.Now().UTC()
	appendHist(t, st, "t1", "old-1", now.AddDate(0, 0, -200))
	appendHist(t, st, "t1", "new-1", now.AddDate(0, 0, -10))
	appendHist(t, st, "t2", "other-1", now.AddDate(0, 0, -10)) // 别家的数据不能被 t1 的裁剪碰到

	j := jobs.NewEgressHistoryPruneJob(prunerAdapter{st: st}, 180)
	res := j.Run(context.Background(), jobs.Context{
		Tenants: []jobs.TenantRef{{ID: "t1"}, {ID: "t2"}}, Now: now,
	})
	if len(res) != 2 {
		t.Fatalf("应逐租户出结果，实际 %d", len(res))
	}
	for _, r := range res {
		if r.Err != "" {
			t.Fatalf("裁剪不应报错: %v", r.Err)
		}
	}
	if got := histCount(t, st, "t1"); got != 1 {
		t.Fatalf("t1 应只剩保留期内 1 条，实际 %d", got)
	}
	if got := histCount(t, st, "t2"); got != 1 {
		t.Fatalf("t2 的历史不应被 t1 的裁剪碰到，实际 %d", got)
	}
}

// TestEgressHistoryPruneDisabled 保留期 <= 0 表示不裁剪（历史是资产，想永远留着就配 0）。
func TestEgressHistoryPruneDisabled(t *testing.T) {
	st := mem.New()
	now := time.Now().UTC()
	appendHist(t, st, "t1", "old-1", now.AddDate(0, 0, -2000))
	j := jobs.NewEgressHistoryPruneJob(prunerAdapter{st: st}, 0)
	res := j.Run(context.Background(), jobs.Context{Tenants: []jobs.TenantRef{{ID: "t1"}}, Now: now})
	if len(res) != 1 || res[0].Affected != 0 {
		t.Fatalf("未启用时不应删任何行: %+v", res)
	}
	if got := histCount(t, st, "t1"); got != 1 {
		t.Fatalf("未启用时历史应原样保留，实际 %d", got)
	}
}
