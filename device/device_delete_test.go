package device_test

import (
	"context"
	"testing"

	"ziniao/billing"
	"ziniao/device"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/store"
)

// TestDeleteReleasesQuota 删除退役设备必须回退 device_count 计量（口径定案：
// 过期/退役不回退，删除才回退——台账出清 = 释放配额，一次操作一个语义）。
func TestDeleteReleasesQuota(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}

	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{
		Name: "待删除设备", Country: "US", Actor: e.owner, Terminal: term,
	})
	if err != nil {
		t.Fatal(err)
	}
	cur, err := e.app.Meter.Current(ctx, e.tenant, billing.MetricDeviceCount)
	if err != nil || cur != 1 {
		t.Fatalf("申购后计量应为 1: %d %v", cur, err)
	}

	// active 设备不能直接删：先退役（归还出口有专门路径），防「删了设备线路还在计费」
	if err := e.app.Device.Delete(ctx, dev.ID, e.owner, term); err == nil {
		t.Fatal("active 设备直接删除应被拒绝")
	} else if !apierr.Is(err, apierr.CodeConflict) {
		t.Fatalf("应返回冲突错误，实际 %v", err)
	}

	if err := e.app.Device.Retire(ctx, dev.ID, e.owner, term); err != nil {
		t.Fatalf("退役失败: %v", err)
	}
	// 退役不回退计量（口径）：设备行还在台账里
	if cur, _ = e.app.Meter.Current(ctx, e.tenant, billing.MetricDeviceCount); cur != 1 {
		t.Fatalf("退役不应回退计量（口径=台账占用），实际 %d", cur)
	}

	if err := e.app.Device.Delete(ctx, dev.ID, e.owner, term); err != nil {
		t.Fatalf("删除退役设备失败: %v", err)
	}
	if cur, _ = e.app.Meter.Current(ctx, e.tenant, billing.MetricDeviceCount); cur != 0 {
		t.Fatalf("删除后计量应回退到 0，实际 %d", cur)
	}
	// 软删除后列表不再可见
	list, total, err := e.app.Device.List(ctx, e.tenant, store.DeviceFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(list) != 0 {
		t.Fatalf("删除后列表应为空: total=%d len=%d", total, len(list))
	}
}

// TestDeleteRequiresPermission 删除与退役同属高风险：普通运营不能删。
func TestDeleteRequiresPermission(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{
		Name: "设备", Country: "US", Actor: e.owner, Terminal: term,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.app.Device.Retire(ctx, dev.ID, e.owner, term); err != nil {
		t.Fatal(err)
	}
	// 无角色的主体：功能权限层直接拒绝
	nobody := iam.Subject{UserID: "u-x", TenantID: e.tenant, Username: "x"}
	if err := e.app.Device.Delete(ctx, dev.ID, nobody, term); err == nil {
		t.Fatal("无权限主体删除设备应被拒绝")
	}
}
