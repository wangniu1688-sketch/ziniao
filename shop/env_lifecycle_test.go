package shop_test

import (
	"context"
	"testing"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/shop"
)

// TestDeactivateReleasesEgressAndBlocksLaunch 停用：退出服务 + 释放出口 + 拒绝启动。
//
// Allocator.Release 此前只有「到期 sweep」一条触发路径；停用入口是第二条——
// 环境退出服务时没有"下一次启动"可言，占着出口只会烧钱 + 占名额。
func TestDeactivateReleasesEgressAndBlocksLaunch(t *testing.T) {
	svc, st, actor := newEnvSvc(t)
	ctx := context.Background()
	seedEnv(t, st, "e10", shop.EnvStatusIdle, time.Now().UTC().Add(-time.Hour))

	env, err := svc.Deactivate(ctx, shop.StopInput{ShopID: "shop-e10", Actor: actor})
	if err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if env.Status != shop.EnvStatusStopped {
		t.Fatalf("停用后应为 stopped，实际 %s", env.Status)
	}
	// 幂等：重复停用不报错、不重复写
	if _, err = svc.Deactivate(ctx, shop.StopInput{ShopID: "shop-e10", Actor: actor}); err != nil {
		t.Fatalf("重复停用应幂等: %v", err)
	}
	// 停用后不允许启动
	if _, err = svc.Launch(ctx, shop.LaunchInput{ShopID: "shop-e10", Actor: actor}); err == nil {
		t.Fatal("停用的环境不应能启动")
	}
	// 启用回来
	env, err = svc.Activate(ctx, shop.StopInput{ShopID: "shop-e10", Actor: actor})
	if err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if env.Status != shop.EnvStatusIdle {
		t.Fatalf("启用后应为 idle，实际 %s", env.Status)
	}
}

// TestDeactivateRunningEnvRejected 运行中的环境必须先关闭才能停用：
// 窗口还开着就把环境停了，客户端的关闭上报会落空。
func TestDeactivateRunningEnvRejected(t *testing.T) {
	svc, st, actor := newEnvSvc(t)
	seedEnv(t, st, "e11", shop.EnvStatusRunning, time.Now().UTC())
	if _, err := svc.Deactivate(context.Background(), shop.StopInput{ShopID: "shop-e11", Actor: actor}); !apierr.Is(err, apierr.CodeConflict) {
		t.Fatalf("运行中的环境停用应返回冲突，实际 %v", err)
	}
}

// TestDeleteEnvRemovesRow 删除环境：物理删行 + 索引清干净（GetByShop 不再命中幽灵行）。
func TestDeleteEnvRemovesRow(t *testing.T) {
	svc, st, actor := newEnvSvc(t)
	ctx := context.Background()
	seedEnv(t, st, "e12", shop.EnvStatusIdle, time.Now().UTC().Add(-time.Hour))

	if err := svc.DeleteEnv(ctx, shop.StopInput{ShopID: "shop-e12", Actor: actor}); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	tctx := store.WithTenant(ctx, "t1")
	if got, err := st.Environments().GetByShop(tctx, "shop-e12"); err == nil && got != nil {
		t.Fatal("删除后 GetByShop 不应再命中")
	}
	// 重复删除：店铺已无环境 → 404（删除不可逆，重试不能假装成功）
	if err := svc.DeleteEnv(ctx, shop.StopInput{ShopID: "shop-e12", Actor: actor}); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("重复删除应返回 not_found，实际 %v", err)
	}
}
