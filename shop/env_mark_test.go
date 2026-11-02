package shop_test

import (
	"context"
	"testing"
	"time"

	"ziniao/platform/store"
	"ziniao/shop"
)

// TestLaunchDoesNotCommitRunning M3/T1 的核心断言：
// Launch 只生成启动单，**绝不**写 running——事务里等浏览器（CDP 十几秒）会把一次
// 浏览器卡顿拖垮整库。running 的唯一提交点是 MarkRunning。
func TestLaunchDoesNotCommitRunning(t *testing.T) {
	svc, st, actor := newEnvSvc(t)
	ctx := context.Background()
	seedEnv(t, st, "m1", shop.EnvStatusCreated, time.Now().UTC())

	if _, err := svc.Launch(ctx, shop.LaunchInput{ShopID: "shop-m1", Actor: actor}); err != nil {
		t.Fatalf("启动单生成失败: %v", err)
	}
	tctx := store.WithTenant(ctx, "t1")
	after, err := st.Environments().Get(tctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != shop.EnvStatusCreated {
		t.Fatalf("Launch 不得写 running，实际 %s", after.Status)
	}
	if after.LastStartedAt != nil {
		t.Fatalf("Launch 不得写 last_started_at，实际 %v", after.LastStartedAt)
	}

	// 浏览器起来 + CDP 连通之后才提交。
	started := time.Now().UTC().Truncate(time.Second)
	env, err := svc.MarkRunning(ctx, shop.MarkRunningInput{
		EnvID: "m1", Actor: actor, StartedAt: started,
	})
	if err != nil {
		t.Fatalf("MarkRunning 失败: %v", err)
	}
	if env.Status != shop.EnvStatusRunning {
		t.Fatalf("MarkRunning 后应为 running，实际 %s", env.Status)
	}
	if env.LastStartedAt == nil || !env.LastStartedAt.Equal(started) {
		t.Fatalf("last_started_at 应写入启动时刻，实际 %v", env.LastStartedAt)
	}
}

// TestMarkLaunchFailedKeepsRunning 失败复位**绝不覆盖 running**：
// 把已经跑起来的环境复位成 idle 会留下「库里说没跑、窗口却开着」的相反悬空态——
// 那比原样留着更糟，运营会以为启动失败再点一次，撞上「已在本机运行」。
func TestMarkLaunchFailedKeepsRunning(t *testing.T) {
	svc, st, actor := newEnvSvc(t)
	ctx := context.Background()
	seedEnv(t, st, "m2", shop.EnvStatusRunning, time.Now().UTC())

	env, err := svc.MarkLaunchFailed(ctx, shop.MarkLaunchFailedInput{
		EnvID: "m2", Actor: actor, Stage: "6", Reason: "cdp 连接超时",
	})
	if err != nil {
		t.Fatalf("失败复位不应报错: %v", err)
	}
	if env.Status != shop.EnvStatusRunning {
		t.Fatalf("running 不得被失败复位覆盖，实际 %s", env.Status)
	}

	// created / idle 才是可复位的状态。
	seedEnv(t, st, "m3", shop.EnvStatusCreated, time.Now().UTC())
	env2, err := svc.MarkLaunchFailed(ctx, shop.MarkLaunchFailedInput{
		EnvID: "m3", Actor: actor, Stage: "2", Reason: "MITM 根证书未就绪",
	})
	if err != nil {
		t.Fatalf("失败复位失败: %v", err)
	}
	if env2.Status != shop.EnvStatusIdle {
		t.Fatalf("未启动的环境失败后应复位 idle，实际 %s", env2.Status)
	}
	// stopped 同样不动：停用是管理动作，不能被一次启动失败悄悄改回来。
	seedEnv(t, st, "m4", shop.EnvStatusStopped, time.Now().UTC())
	env3, err := svc.MarkLaunchFailed(ctx, shop.MarkLaunchFailedInput{EnvID: "m4", Actor: actor, Stage: "3"})
	if err != nil {
		t.Fatalf("失败复位失败: %v", err)
	}
	if env3.Status != shop.EnvStatusStopped {
		t.Fatalf("stopped 不应被启动失败改动，实际 %s", env3.Status)
	}
}

// TestMarkRunningIdempotent 对账重挂 / 重复提交不能把一次成功启动记成两次。
func TestMarkRunningIdempotent(t *testing.T) {
	svc, st, actor := newEnvSvc(t)
	ctx := context.Background()
	seedEnv(t, st, "m5", shop.EnvStatusIdle, time.Now().UTC())

	if _, err := svc.MarkRunning(ctx, shop.MarkRunningInput{EnvID: "m5", Actor: actor}); err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	first, err := st.Environments().Get(store.WithTenant(ctx, "t1"), "m5")
	if err != nil {
		t.Fatal(err)
	}
	prev := first.UpdatedAt
	time.Sleep(time.Millisecond)
	if _, err := svc.MarkRunning(ctx, shop.MarkRunningInput{EnvID: "m5", Actor: actor}); err != nil {
		t.Fatalf("重复提交不应报错: %v", err)
	}
	again, err := st.Environments().Get(store.WithTenant(ctx, "t1"), "m5")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != shop.EnvStatusRunning {
		t.Fatalf("重复提交后仍应为 running，实际 %s", again.Status)
	}
	if !again.UpdatedAt.Equal(prev) {
		t.Fatalf("重复提交不应刷新 updated_at（否则对账会误判为一次新启动）: %v → %v", prev, again.UpdatedAt)
	}
}

// TestStopEnvFromCreated 没启动过的环境点停止也应收回 idle：
// 留在「已创建」会让运营以为停止没生效（与关窗口同义：现在没人在用）。
func TestStopEnvFromCreated(t *testing.T) {
	svc, st, actor := newEnvSvc(t)
	seedEnv(t, st, "m6", shop.EnvStatusCreated, time.Now().UTC())
	env, err := svc.StopEnv(context.Background(), shop.StopInput{ShopID: "shop-m6", Actor: actor})
	if err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if env.Status != shop.EnvStatusIdle {
		t.Fatalf("created 停止后应回到 idle，实际 %s", env.Status)
	}
}
