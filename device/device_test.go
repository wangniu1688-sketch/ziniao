package device_test

import (
	"context"
	"testing"

	"ziniao/app"
	"ziniao/device"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/shop"
	"ziniao/tenant"
)

type env struct {
	app    *app.App
	owner  iam.Subject
	tenant string
}

// newEnv 装配应用并建一个团队（含老板）。
func newEnv(t *testing.T, tweak func(c *config.Config)) *env {
	t.Helper()
	cfg := config.Default()
	cfg.StoreDriver = "memory"
	cfg.JWTSecret = "test-secret"
	cfg.DataDir = t.TempDir() // 避免测试在包目录下建 ./data 并互相抢实例锁
	if tweak != nil {
		tweak(cfg)
	}
	ctx := context.Background()
	a, err := app.New(ctx, cfg, mem.New(), nil)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	res, err := a.Tenant.Create(ctx, tenant.CreateInput{
		Name: "设备团队", Slug: "dev-team", PlanCode: "pro",
		Owner:    tenant.OwnerInput{Username: "dev_owner", Password: "Zn@12345678", Nickname: "老板"},
		Terminal: eventbus.Terminal{IP: "1.2.3.4", UserAgent: "test"},
	})
	if err != nil {
		t.Fatalf("建团队失败: %v", err)
	}
	return &env{
		app:    a,
		owner:  iam.Subject{UserID: res.User.ID, TenantID: res.Tenant.ID, Username: res.User.Username, Roles: []types.Role{*res.Role}},
		tenant: res.Tenant.ID,
	}
}

// TestDeviceQuotaIsNotMutex L9：设备不是互斥锁——一设备可绑多店、多人可并发使用。
func TestDeviceQuotaIsNotMutex(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4", UserAgent: "test"}

	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{
		Name: "US 住宅设备", Country: "US", Actor: e.owner, Terminal: term,
	})
	if err != nil {
		t.Fatalf("购买设备失败: %v", err)
	}
	if dev.MaxConcurrent != 6 {
		t.Fatalf("默认并发上限应为 6（规格明确值），实际 %d", dev.MaxConcurrent)
	}

	// 建两家店铺并绑定到同一台设备（一设备多账号，L9）
	shopIDs := []string{}
	for i := 0; i < 2; i++ {
		sp, err := e.app.Shop.Create(ctx, shop.CreateInput{
			Name: "店" + string(rune('A'+i)), Actor: e.owner, Terminal: term,
		})
		if err != nil {
			t.Fatalf("建店失败: %v", err)
		}
		if _, err := e.app.Device.Bind(ctx, device.BindInput{
			DeviceID: dev.ID, ShopID: sp.ID, Actor: e.owner, Terminal: term,
		}); err != nil {
			t.Fatalf("绑定失败: %v", err)
		}
		shopIDs = append(shopIDs, sp.ID)
	}

	// 同一用户占用两家店：只算 1 个并发槽（一人多店）
	res1, err := e.app.Device.Acquire(ctx, dev.ID, shopIDs[0], e.owner, term)
	if err != nil {
		t.Fatalf("占用失败: %v", err)
	}
	res2, err := e.app.Device.Acquire(ctx, dev.ID, shopIDs[1], e.owner, term)
	if err != nil {
		t.Fatalf("同一用户占用第二家店应放行: %v", err)
	}
	if res2.ConcurrentUsers != res1.ConcurrentUsers || res2.ConcurrentUsers != 1 {
		t.Fatalf("一人多店应只占 1 个槽: %d / %d", res1.ConcurrentUsers, res2.ConcurrentUsers)
	}
}

// TestConcurrentHardLimit 超过硬阈值才拒绝（软阈值只是告警，不阻塞）。
func TestConcurrentHardLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Limits.DeviceHardLimitUsers = 1 })
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "设备", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := e.app.Shop.Create(ctx, shop.CreateInput{Name: "店", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Device.Bind(ctx, device.BindInput{DeviceID: dev.ID, ShopID: sp.ID, Actor: e.owner, Terminal: term}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Device.Acquire(ctx, dev.ID, sp.ID, e.owner, term); err != nil {
		t.Fatalf("第一个用户应放行: %v", err)
	}
	other := e.owner
	other.UserID = "user-2"
	if _, err := e.app.Device.Acquire(ctx, dev.ID, sp.ID, other, term); err == nil {
		t.Fatal("超过硬上限应拒绝")
	} else if !apierr.Is(err, apierr.CodeConflict) {
		t.Fatalf("错误码应为 conflict，实际 %v", err)
	}
}

// TestBindQuota 绑定账号数上限（不是互斥锁，是配额）。
func TestBindQuota(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Limits.DeviceMaxBoundAccounts = 1 })
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "设备", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		sp, err := e.app.Shop.Create(ctx, shop.CreateInput{Name: "店" + string(rune('A'+i)), Actor: e.owner, Terminal: term})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.app.Device.Bind(ctx, device.BindInput{DeviceID: dev.ID, ShopID: sp.ID, Actor: e.owner, Terminal: term})
		if i == 0 {
			if err != nil {
				t.Fatalf("首次绑定应成功: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatal("超过绑定账号上限应拒绝")
		}
		if !apierr.Is(err, apierr.CodeQuotaExceeded) {
			t.Fatalf("错误码应为 quota_exceeded，实际 %v", err)
		}
	}
}

// TestImportBoxAndRenew 紫鸟盒子导入与续费（含审计事件）。
func TestImportBoxAndRenew(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	box, err := e.app.Device.ImportBox(ctx, device.BoxInput{
		SN: "SN123456", BindCode: "BIND-CODE-001", Name: "美版盒子",
		Region: "美版", Actor: e.owner, Terminal: term,
	})
	if err != nil {
		t.Fatalf("盒子导入失败: %v", err)
	}
	if box.Type != types.DeviceBox || box.SN != "SN123456" {
		t.Fatalf("盒子属性不符: %+v", box)
	}
	if err := e.app.Device.Renew(ctx, box.ID, 12, e.owner, term); err != nil {
		t.Fatalf("续费失败: %v", err)
	}
	got, err := e.app.Device.Get(ctx, e.owner.TenantID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.DeviceStatusActive {
		t.Fatalf("续费后状态应为 active，实际 %s", got.Status)
	}
	// 事件必须进入审计（L5：所有写操作落审计）
	if _, err := e.app.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	items, _, err := e.app.Audit.List(store.WithTenant(ctx, e.owner.TenantID), store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.Action == "device.renew" {
			found = true
		}
	}
	if !found {
		t.Fatalf("续费事件未进入审计日志，实际 %d 条", len(items))
	}
}

// TestDeviceHealth 健康评分可计算。
func TestDeviceHealth(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "设备", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.app.Device.Health(ctx, e.owner.TenantID, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.Score < 0 || h.Score > 100 {
		t.Fatalf("健康分越界: %d", h.Score)
	}
}
