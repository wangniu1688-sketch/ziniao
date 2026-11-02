package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ziniao/app"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/notify"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/security"
)

// TestHighAlertDeliveryEndToEnd high 告警的最后一公里全链路：
//
//	config.Load() → BuildSenders → Sink → Webhook → 失败留痕 → RunJobsOnce 补投
//
// 为什么要这么长的一条用例：外发通道的正确性恰恰死于「每处都对、拼起来不对」
// ——环境变量名写错、装配函数忘了调、偏序/过滤在 store 层被吃掉。任何一环单测
// 单独跑都是绿的，只有把真实 App 装起来、真的发一个 HTTP 请求，才能证明
// 「运营凌晨不在控制台时，high 告警照样能到达人」。
func TestHighAlertDeliveryEndToEnd(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	hits := 0
	shouldFail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		hits++
		fail := shouldFail
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 配置只经环境变量注入（与生产同一口径，顺带验证 env 名没有拼错）
	t.Setenv("ZINIAO_WEBHOOK_URL", srv.URL)
	t.Setenv("ZINIAO_WEBHOOK_SECRET", "s3cr3t-end2end")
	t.Setenv("ZINIAO_WEBHOOK_ALLOW_LOOPBACK", "true") // 回环目标仅测试允许
	t.Setenv("ZINIAO_WEBHOOK_MAX_RETRY", "0")         // 关掉即时重试，确保首次必写出 failed
	t.Setenv("ZINIAO_NOTIFY_MAX_ATTEMPTS", "3")

	cfg := config.Load()
	cfg.DataDir = t.TempDir() // 避免测试在包目录下建 ./data 并互相抢实例锁
	st := mem.New()
	a, err := app.New(ctx, cfg, st, slog.Default())
	if err != nil {
		t.Fatalf("装配应用失败: %v", err)
	}

	now := time.Now().UTC()
	tctx := store.WithTenant(ctx, "t1")
	if err := st.Tenants().Create(ctx, &types.Tenant{
		ID: "t1", Name: "紫鸟演示租户", Slug: "t1", Status: "active", PlanCode: "pro", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().Create(ctx, &types.User{
		ID: "u-1", Username: "ops", Email: "ops@example.com", Status: "active", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	role, err := st.Roles().GetByCode(ctx, "", "owner")
	if err != nil {
		t.Fatalf("取内置 owner 角色失败: %v", err)
	}
	if err := st.UserRoles().Assign(ctx, &types.UserRole{
		ID: ids.New(), TenantID: "t1", UserID: "u-1", RoleID: role.ID,
		GrantedBy: "system", GrantedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// 触发一条 high 告警（出口 IP 轮换同级别）
	if err := a.Security.Raise(tctx, security.RaiseInput{
		TenantID: "t1", Category: "risk", Level: "high", Action: "env.exit_ip_rotated",
		Actor:    &iam.Subject{UserID: "u-1", TenantID: "t1", Roles: []types.Role{*role}},
		Detail:   map[string]any{"prev_exit": "1.2.3.4:39101", "current_exit": "1.2.3.4:39102"},
		Terminal: eventbusTerminal(),
	}); err != nil {
		t.Fatalf("上报安全事件失败: %v", err)
	}
	if _, err := a.DispatchOnce(ctx); err != nil {
		t.Fatalf("事件派发失败: %v", err)
	}

	list, err := st.Notifications().List(tctx, "t1", "u-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应入库 1 条通知，实际 %d", len(list))
	}
	n := list[0]
	if strings.Join(n.Channels, ",") != "inapp,webhook" {
		t.Fatalf("按环境配置只应启用 webhook：%v", n.Channels)
	}
	if n.DeliveryStatus != notify.StatusFailed {
		t.Fatalf("目标 500 时应留下 failed 供重投，实际 %s", n.DeliveryStatus)
	}
	if hits != 1 {
		t.Fatalf("首次应尝试投递 1 次，实际 %d", hits)
	}

	// 目标恢复，跑一轮定时任务：补投应由 notify.delivery_retry 完成
	mu.Lock()
	shouldFail = false
	mu.Unlock()

	results := a.RunJobsOnce(ctx)
	sawRetry := false
	for _, r := range results {
		if r.Job == "notify.delivery_retry" && r.Affected > 0 {
			sawRetry = true
		}
	}
	if !sawRetry {
		t.Fatalf("重投任务未见生效结果: %+v", results)
	}
	afterList, err := st.Notifications().List(tctx, "t1", "u-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterList) != 1 {
		t.Fatalf("重投不得产生新通知，实际 %d 条", len(afterList))
	}
	if afterList[0].DeliveryStatus != notify.StatusSent {
		t.Fatalf("补投后应为 sent，实际 %s note=%s", afterList[0].DeliveryStatus, afterList[0].DeliveryNote)
	}
	if hits != 2 {
		t.Fatalf("补投应只再请求 1 次，累计 %d", hits)
	}

	// 再跑一轮：已送达的通知不该被反复投
	_ = a.RunJobsOnce(ctx)
	if hits != 2 {
		t.Fatalf("已送达后不得重复投递，累计 %d", hits)
	}
}

func eventbusTerminal() eventbus.Terminal { return eventbus.Terminal{Mode: "api", ClientVer: "test/1"} }
