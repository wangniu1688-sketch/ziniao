package shop_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/shop"
)

func newEnvSvc(t *testing.T) (*shop.EnvService, *mem.Store, iam.Subject) {
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
	// lib/baseline 传 nil：关闭与回收两条路径不碰画像库，不需要构造指纹环境。
	svc := shop.NewEnvService(st, bus, iam.NewEngine(cfg), nil, nil)
	return svc, st, iam.Subject{UserID: "u1", TenantID: "t1", Username: "owner", Roles: []types.Role{*role}}
}

func seedEnv(t *testing.T, st *mem.Store, id, status string, updatedAt time.Time) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "t1")
	// 环境归属店铺：数据级校验收口后 Launch/StopEnv 必须能取到店铺行（all 范围
	// 才不需要归属判定，但店铺行本身必须存在），种环境时连店铺一起种。
	now := time.Now().UTC()
	if err := st.Shops().Create(ctx, &types.Shop{ID: "shop-" + id, TenantID: "t1",
		Name: "店-" + id, Platform: "Amazon", Status: "正常运营",
		OwnerID: "u1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	env := types.Environment{ID: id, TenantID: "t1", ShopID: "shop-" + id,
		Status: status, KernelMajor: 120, CreatedAt: updatedAt, UpdatedAt: updatedAt}
	if err := st.Environments().Create(ctx, &env); err != nil {
		t.Fatal(err)
	}
}

// TestStopEnvReturnsRunningToIdle 关窗口必须把状态收回来。
func TestStopEnvReturnsRunningToIdle(t *testing.T) {
	svc, st, actor := newEnvSvc(t)
	now := time.Now().UTC().Add(-time.Hour)
	seedEnv(t, st, "e1", shop.EnvStatusRunning, now)

	env, err := svc.StopEnv(context.Background(), shop.StopInput{ShopID: "shop-e1", Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if env.Status != shop.EnvStatusIdle {
		t.Fatalf("关闭后应为 idle，实际 %s", env.Status)
	}

	// 幂等：再关一次不报错、不重复写（UpdatedAt 不该被第二次调用刷新）
	again, err := svc.StopEnv(context.Background(), shop.StopInput{ShopID: "shop-e1", Actor: actor})
	if err != nil {
		t.Fatalf("重复关闭不应报错: %v", err)
	}
	if again.Status != shop.EnvStatusIdle {
		t.Fatalf("重复关闭后仍应为 idle，实际 %s", again.Status)
	}
	got, err := st.Environments().GetByShop(store.WithTenant(context.Background(), "t1"), "shop-e1")
	if err != nil || got == nil {
		t.Fatalf("环境读取失败: %v", err)
	}
	if !got.UpdatedAt.Equal(env.UpdatedAt) {
		t.Fatal("重复关闭不应再次写库（否则审计里会多出一条关闭记录）")
	}
}

// TestStopEnvRequiresPermission 关闭也是受管控的操作，不是谁都能报。
func TestStopEnvRequiresPermission(t *testing.T) {
	svc, st, _ := newEnvSvc(t)
	seedEnv(t, st, "e2", shop.EnvStatusRunning, time.Now().UTC())
	ctx := context.Background()
	cs, err := st.Roles().GetByCode(ctx, "", "cs") // 客服只有 shop:read
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.StopEnv(ctx, shop.StopInput{ShopID: "shop-e2",
		Actor: iam.Subject{UserID: "u2", TenantID: "t1", Roles: []types.Role{*cs}}})
	if err == nil || !strings.Contains(err.Error(), "无权") {
		t.Fatalf("无权限时应拒绝，实际: %v", err)
	}
}

// TestStopEnvMissingEnvironment 没有环境不能当成成功（否则前端永远显示"已关闭"）。
func TestStopEnvMissingEnvironment(t *testing.T) {
	svc, _, actor := newEnvSvc(t)
	if _, err := svc.StopEnv(context.Background(), shop.StopInput{ShopID: "shop-none", Actor: actor}); err == nil {
		t.Fatal("不存在的店铺应返回错误")
	}
}

// TestReclaimIdleOnlyAfterMaxAge 巡检只回收"真的没人动过"的环境。
func TestReclaimIdleOnlyAfterMaxAge(t *testing.T) {
	svc, st, _ := newEnvSvc(t)
	now := time.Now().UTC()
	seedEnv(t, st, "e-old", shop.EnvStatusRunning, now.Add(-13*time.Hour)) // 超阈值
	seedEnv(t, st, "e-new", shop.EnvStatusRunning, now.Add(-1*time.Hour))  // 刚启动
	seedEnv(t, st, "e-off", shop.EnvStatusIdle, now.Add(-48*time.Hour))    // 已经是空闲
	seedEnv(t, st, "e-crt", shop.EnvStatusCreated, now.Add(-48*time.Hour)) // 从未启动

	n, err := svc.ReclaimIdle(context.Background(), "t1", now, 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只回收超阈值的那一个，实际 %d", n)
	}
	ctx := store.WithTenant(context.Background(), "t1")
	for id, want := range map[string]string{"e-old": shop.EnvStatusIdle, "e-new": shop.EnvStatusRunning,
		"e-off": shop.EnvStatusIdle, "e-crt": shop.EnvStatusCreated} {
		got, err := st.Environments().Get(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("%s 读取失败: %v", id, err)
		}
		if got.Status != want {
			t.Fatalf("%s 期望 %s，实际 %s", id, want, got.Status)
		}
	}
}

// TestReclaimIdleDisabled 阈值为 0 = 不启用，不能退化成"立刻回收所有 running"。
func TestReclaimIdleDisabled(t *testing.T) {
	svc, st, _ := newEnvSvc(t)
	seedEnv(t, st, "e-x", shop.EnvStatusRunning, time.Now().UTC().Add(-99*time.Hour))
	n, err := svc.ReclaimIdle(context.Background(), "t1", time.Now().UTC(), 0)
	if err != nil || n != 0 {
		t.Fatalf("未启用时不应回收，实际 n=%d err=%v", n, err)
	}
	got, _ := st.Environments().Get(store.WithTenant(context.Background(), "t1"), "e-x")
	if got.Status != shop.EnvStatusRunning {
		t.Fatalf("未启用时状态不应变化，实际 %s", got.Status)
	}
}
