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

// TestPhase2DReceiptsPG 本轮四项交付在真库上的行为（迁移 0011）。
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestPhase2DReceiptsPG -v -count=1
//
// 为什么必须拿真库锁：
//   - event_consumptions 的主键幂等（ON CONFLICT DO NOTHING）与 RLS 隔离，
//     mem 驱动的 map 根本测不出「跨租户读不到也写不进」；
//   - PruneEvents 的 CTE DELETE ... RETURNING 计数这条 SQL 此前从未在真库跑过；
//   - 软删除（deleted_at）与物理删除（环境行 + envByShop 索引）在 PG 是两条 SQL，
//     mem 是两套内存操作——两边必须给出同样结论。
func TestPhase2DReceiptsPG(t *testing.T) {
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
		t.Fatalf("迁移失败（0011 未应用？）: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	tid := "t-p2d-" + suffix
	other := tid + "-other"
	for _, tc := range []struct{ id, slug string }{{tid, "p2d-" + suffix}, {other, "p2do-" + suffix}} {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
			VALUES($1,$2,$3,'active','pro',now())`, tc.id, tc.id, tc.slug); err != nil {
			t.Fatalf("建测试租户失败: %v", err)
		}
	}
	tctx := store.WithTenant(ctx, tid)
	octx := store.WithTenant(ctx, other)
	defer func() {
		for _, tc := range []struct{ id string }{{tid}, {other}} {
			tx, err := st.Pool().Begin(context.Background())
			if err != nil {
				t.Logf("清理失败: %v", err)
				continue
			}
			if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, tc.id); err != nil {
				_ = tx.Rollback(context.Background())
				continue
			}
			_, _ = tx.Exec(context.Background(), `DELETE FROM event_consumptions WHERE tenant_id=$1`, tc.id)
			_, _ = tx.Exec(context.Background(), `DELETE FROM egress_binding_history WHERE tenant_id=$1`, tc.id)
			_, _ = tx.Exec(context.Background(), `DELETE FROM usage_counters WHERE tenant_id=$1`, tc.id)
			_, _ = tx.Exec(context.Background(), `DELETE FROM devices WHERE tenant_id=$1`, tc.id)
			_, _ = tx.Exec(context.Background(), `DELETE FROM environments WHERE tenant_id=$1`, tc.id)
			_, _ = tx.Exec(context.Background(), `DELETE FROM shops WHERE tenant_id=$1`, tc.id)
			_, _ = tx.Exec(context.Background(), `DELETE FROM tenants WHERE id=$1`, tc.id)
			if err := tx.Commit(context.Background()); err != nil {
				t.Logf("清理提交失败: %v", err)
			}
		}
	}()

	// ---------- 1) 接收侧投递幂等（event_consumptions） ----------
	ob := st.Outbox()
	if ok, err := ob.Consumed(tctx, "ev-x", "audit"); err != nil || ok {
		t.Fatalf("初始应为未消费: %v %v", ok, err)
	}
	if err := ob.MarkConsumed(tctx, "ev-x", "audit"); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if err := ob.MarkConsumed(tctx, "ev-x", "audit"); err != nil {
		t.Fatalf("重复登记应幂等（ON CONFLICT DO NOTHING）: %v", err)
	}
	if ok, err := ob.Consumed(tctx, "ev-x", "audit"); err != nil || !ok {
		t.Fatalf("登记后应为已消费: %v %v", ok, err)
	}
	// 键位隔离：同一事件不同 handler 互不影响
	if ok, _ := ob.Consumed(tctx, "ev-x", "notify"); ok {
		t.Fatal("audit 的登记不应算到 notify 头上")
	}
	// 租户隔离：别的租户读不到这条登记
	if ok, err := ob.Consumed(octx, "ev-x", "audit"); err != nil || ok {
		t.Fatalf("跨租户不应读到消费登记: %v %v", ok, err)
	}
	// 与业务写同事务：登记回滚时业务写也回滚（audit.Sink 的原子性依赖这一点）
	err = st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Outbox().MarkConsumed(tctx, "ev-tx", "audit"); err != nil {
			return err
		}
		return context.DeadlineExceeded // 强制回滚
	})
	if ok, _ := ob.Consumed(tctx, "ev-tx", "audit"); ok {
		t.Fatal("事务回滚后登记不应残留（登记与业务写必须同生共死）")
	}

	// ---------- 2) 出口历史保留期裁剪（PruneEvents） ----------
	now := time.Now().UTC()
	hist := st.EgressBindings()
	for i, at := range []time.Time{now.AddDate(0, 0, -200), now.AddDate(0, 0, -10)} {
		if err := hist.AppendEvent(tctx, &types.EgressBindingEvent{
			ID: tid + "-h" + strconv.Itoa(i), EnvID: tid + "-env", Action: types.EgressActionBound,
			OccurredAt: at,
		}); err != nil {
			t.Fatalf("写历史失败: %v", err)
		}
	}
	n, err := hist.PruneEvents(tctx, tid, now.AddDate(0, 0, -180))
	if err != nil {
		t.Fatalf("裁剪失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只裁掉保留期外的 1 条，实际 %d", n)
	}
	if _, total, err := hist.ListEvents(tctx, store.EgressHistoryFilter{Limit: 10}); err != nil || total != 1 {
		t.Fatalf("保留期内的行应原样保留: total=%d err=%v", total, err)
	}
	// 幂等：再裁一遍是 0
	if n, _ = hist.PruneEvents(tctx, tid, now.AddDate(0, 0, -180)); n != 0 {
		t.Fatalf("重复裁剪应为 0，实际 %d", n)
	}

	// ---------- 3) 设备软删除 / 环境物理删除 / 计量回退 ----------
	if err := st.Shops().Create(tctx, &types.Shop{ID: tid + "-shop", TenantID: tid,
		Name: "店", Platform: "Amazon", Status: "正常运营", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("建店失败: %v", err)
	}
	dev := &types.Device{ID: tid + "-dev", TenantID: tid, Name: "设备", Type: types.DeviceCloudNetwork,
		Status: types.DeviceStatusRecycled, CreatedAt: now, UpdatedAt: now}
	if err := st.Devices().Create(tctx, dev); err != nil {
		t.Fatalf("建设备失败: %v", err)
	}
	if err := st.Devices().Delete(tctx, dev.ID); err != nil {
		t.Fatalf("软删除失败: %v", err)
	}
	if err := st.Devices().Delete(tctx, dev.ID); err != nil {
		t.Fatalf("重复软删除应幂等: %v", err)
	}
	if _, total, err := st.Devices().List(tctx, store.DeviceFilter{Limit: 10}); err != nil || total != 0 {
		t.Fatalf("软删除后列表应为空: total=%d err=%v", total, err)
	}
	// 计量回退（负 delta）：upsert 累加天然支持，读回要等值
	if v, err := st.Usage().Incr(tctx, tid, "device.count", 2); err != nil || v != 2 {
		t.Fatalf("计量递增失败: %d %v", v, err)
	}
	if v, err := st.Usage().Incr(tctx, tid, "device.count", -1); err != nil || v != 1 {
		t.Fatalf("计量回退失败: %d %v", v, err)
	}

	// ---------- 4) 内置角色权限随版本同步（并集补权限） ----------
	// 真库上要验的是 ON CONFLICT DO NOTHING 真的返回 0 影响行——
	// mem 的 map 天然幂等，测不出「SQL 写错导致每次启动都插一遍」。
	role := &types.Role{ID: tid + "-role", Code: "sync-" + suffix, Name: "同步测试", Builtin: true,
		Permissions: []string{"shop:read"}, DataScope: "all", CreatedAt: now}
	if err := st.Roles().Create(tctx, role); err != nil {
		t.Fatalf("建角色失败: %v", err)
	}
	if n, err := st.Roles().AddPermissions(tctx, role.ID, []string{"shop:read", "device:delete", "env:delete"}); err != nil || n != 2 {
		t.Fatalf("应新增 2 条（已有 shop:read 不重复计）: %d %v", n, err)
	}
	if n, err := st.Roles().AddPermissions(tctx, role.ID, []string{"shop:read", "device:delete"}); err != nil || n != 0 {
		t.Fatalf("重复补齐应为 0（幂等）: %d %v", n, err)
	}
	got, err := st.Roles().Get(tctx, role.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Permissions) != 3 {
		t.Fatalf("补齐后应为 3 条，实际 %v", got.Permissions)
	}

	env := &types.Environment{ID: tid + "-env", TenantID: tid, ShopID: tid + "-shop",
		ProfileID: "p1", BaselineHash: "h1", KernelMajor: 120, Status: "stopped",
		CreatedAt: now, UpdatedAt: now}
	if err := st.Environments().Create(tctx, env); err != nil {
		t.Fatalf("建环境失败: %v", err)
	}
	if err := st.Environments().Delete(tctx, env.ID); err != nil {
		t.Fatalf("物理删除失败: %v", err)
	}
	if got, err := st.Environments().GetByShop(tctx, tid+"-shop"); err == nil && got != nil {
		t.Fatal("物理删除后 GetByShop 不应命中")
	}
}
