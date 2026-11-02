package device_test

import (
	"context"
	"testing"
	"time"

	"ziniao/device"
	"ziniao/eventbus"
	"ziniao/network"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// recordingVendor 记录被归还的资源 ID：出口还不回代理商就会一直计费，
// 而「设备已经停用了、线路还在收费」是最难对账的一种漏。
type recordingVendor struct {
	*network.MockProvider
	released []string
}

func (p *recordingVendor) Release(ctx context.Context, id string) error {
	p.released = append(p.released, id)
	return p.MockProvider.Release(ctx, id)
}

// newEgressEnv 装配带出口池的应用：设备与环境共用同一张路由表与同一套不变量。
func newEgressEnv(t *testing.T) (*env, *recordingVendor) {
	t.Helper()
	e := newEnv(t, nil)
	// 必须走 NewMockProvider：issued 表在构造函数里初始化，
	// 直接字面量构造会在第一次 Acquire 时写 nil map（panic）。
	base := network.NewMockProvider("US", network.ASNResidential, "")
	base.VendorName = "egress-mock"
	p := &recordingVendor{MockProvider: base}
	alloc := network.NewAllocatorWithRouter(network.NewRouter(
		network.Vendor{Provider: p, Meta: network.VendorMeta{Name: "egress-mock"}},
	), time.Hour)
	alloc.SetBindingStore(e.app.Store.EgressBindings())
	alloc.SetHistoryStore(e.app.Store.EgressBindings())
	e.app.SetIPPool(alloc)
	return e, p
}

// TestDevicePurchaseGoesThroughAllocator 设备申购必须走分配器：
// 逐台独占出口、绑定落库（重启不换 IP）、并留下历史。
//
// 以前这里是直接 `prov.Acquire`：不判重、不落库、不归还，
// 等于给「两个店铺共用出口」这条最硬的红线留了一个谁也看不到的后门。
func TestDevicePurchaseGoesThroughAllocator(t *testing.T) {
	e, _ := newEgressEnv(t)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	binds := e.app.Store.EgressBindings()
	tctx := store.WithTenant(ctx, e.tenant)

	dev1, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "设备A", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatalf("申购失败: %v", err)
	}
	dev2, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "设备B", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatalf("第二次申购失败: %v", err)
	}
	if dev1.IP == "" || dev2.IP == "" {
		t.Fatalf("接了出口池就必须有出口: %q / %q", dev1.IP, dev2.IP)
	}
	if dev1.ProxyPort == dev2.ProxyPort {
		t.Fatalf("两台设备必须各占一个出口（同端口=共用出口），实际 %d / %d", dev1.ProxyPort, dev2.ProxyPort)
	}
	for _, dev := range []*types.Device{dev1, dev2} {
		b, err := binds.Get(tctx, device.EgressKey(dev.ID))
		if err != nil || b == nil {
			t.Fatalf("设备出口未落库（重启就会换 IP）: %v", err)
		}
		if b.Provider != "egress-mock" {
			t.Fatalf("绑定要记清签发方，实际 %s", b.Provider)
		}
		if b.Host != dev.IP {
			t.Fatalf("绑定与设备上的出口不一致: %s vs %s", b.Host, dev.IP)
		}
	}
	// 历史里也能查到：设备的出口同样是「谁用过这个 ASN」的证据链一环
	events, _, err := binds.ListEvents(tctx, store.EgressHistoryFilter{EnvID: device.EgressKey(dev1.ID)})
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == types.EgressActionBound && ev.EnvID == device.EgressKey(dev1.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("设备出口未留档，实际 %+v", events)
	}
}

// TestDeviceRetireReturnsEgress 退役必须把出口还给代理商（顺序：先归还再改状态）。
func TestDeviceRetireReturnsEgress(t *testing.T) {
	e, p := newEgressEnv(t)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	tctx := store.WithTenant(ctx, e.tenant)

	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "设备", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatalf("申购失败: %v", err)
	}
	// 出口凭据要能取回（重启后靠的是绑定 + 代理商的 Attach，不是设备行里的快照）
	acq, err := e.app.Device.Egress(ctx, dev.ID, e.owner)
	if err != nil {
		t.Fatalf("取出口失败: %v", err)
	}
	if acq == nil || acq.Proxy.Host != dev.IP {
		t.Fatalf("取回的出口应与设备一致: %+v（设备 IP=%s）", acq, dev.IP)
	}
	resID := ""
	if b, err := e.app.Store.EgressBindings().Get(tctx, device.EgressKey(dev.ID)); err == nil && b != nil {
		resID = b.ResourceID
	}
	if resID == "" {
		t.Fatalf("找不到设备的出口资源 ID")
	}

	if err := e.app.Device.Retire(ctx, dev.ID, e.owner, term); err != nil {
		t.Fatalf("退役失败: %v", err)
	}
	if len(p.released) != 1 || p.released[0] != resID {
		t.Fatalf("退役必须把出口还给代理商，实际归还 %v（应为 %s）", p.released, resID)
	}
	got, err := e.app.Device.Get(ctx, e.tenant, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.DeviceStatusRecycled {
		t.Fatalf("退役后状态应为 recycled，实际 %s", got.Status)
	}
	// 绑定已解除：再取出口应为空（且不能顺手给已退役的设备新申请一条）
	if acq, err = e.app.Device.Egress(ctx, dev.ID, e.owner); err != nil || acq != nil {
		t.Fatalf("退役后不应再有出口: %+v %v", acq, err)
	}
	// 重复退役必须幂等（不能重复归还、不能报错）
	if err := e.app.Device.Retire(ctx, dev.ID, e.owner, term); err != nil {
		t.Fatalf("重复退役应幂等: %v", err)
	}
	if len(p.released) != 1 {
		t.Fatalf("重复退役不该再归还一次: %v", p.released)
	}
}

// TestDeviceExpiryReturnsEgress 到期设备由系统巡检归还出口。
//
// 设备按月租、出口按量计费：没人点「退役」时，这是唯一的归还路径。
// 缺了它，设备会静静过期而线路悄悄收费——月底对账才发现，且已追不到是哪一台。
func TestDeviceExpiryReturnsEgress(t *testing.T) {
	e, p := newEgressEnv(t)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	tctx := store.WithTenant(ctx, e.tenant)

	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "设备", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatalf("申购失败: %v", err)
	}
	b, err := e.app.Store.EgressBindings().Get(tctx, device.EgressKey(dev.ID))
	if err != nil || b == nil {
		t.Fatalf("设备出口未落库: %v", err)
	}
	resID := b.ResourceID

	// 把设备改成已到期：直接改库比等一个月现实得多
	past := time.Now().UTC().Add(-time.Hour)
	dev.ExpiresAt = past
	if err := e.app.Store.Devices().Update(tctx, dev); err != nil {
		t.Fatalf("改到期时间失败: %v", err)
	}
	n, err := e.app.Device.RetireExpired(ctx, e.tenant, time.Now().UTC())
	if err != nil {
		t.Fatalf("到期回收失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应回收 1 台到期设备，实际 %d", n)
	}
	if len(p.released) != 1 || p.released[0] != resID {
		t.Fatalf("到期回收必须归还出口，实际 %v（应为 %s）", p.released, resID)
	}
	got, err := e.app.Device.Get(ctx, e.tenant, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.DeviceStatusExpired {
		t.Fatalf("到期后状态应为 expired，实际 %s", got.Status)
	}
	// 再跑一次必须幂等：不能重复归还，也不该报错
	if n, err = e.app.Device.RetireExpired(ctx, e.tenant, time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("到期回收应幂等，实际 n=%d err=%v", n, err)
	}
	if len(p.released) != 1 {
		t.Fatalf("重复巡检不该再归还一次: %v", p.released)
	}
}

// TestDeviceWithoutPoolDegradesGracefully 没接代理商时设备照常可申购（只是没有出口）。
// 不能让「出口池没配」变成设备功能不可用——那等于把可选项做成硬依赖。
func TestDeviceWithoutPoolDegradesGracefully(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "无出口设备", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatalf("未接出口池时也应能申购: %v", err)
	}
	if dev.IP != "" {
		t.Fatalf("没有出口池就不该凭空出现出口: %s", dev.IP)
	}
	if acq, err := e.app.Device.Egress(ctx, dev.ID, e.owner); err != nil || acq != nil {
		t.Fatalf("无出口设备取出口应返回空: %+v %v", acq, err)
	}
	// 退役同样不能因为「没有出口可还」而失败
	if err := e.app.Device.Retire(ctx, dev.ID, e.owner, term); err != nil {
		t.Fatalf("无出口设备退役失败: %v", err)
	}
}

// TestDeviceRetireNeedsPermission 退役是高风险动作：普通运营不能执行。
func TestDeviceRetireNeedsPermission(t *testing.T) {
	e, _ := newEgressEnv(t)
	ctx := context.Background()
	term := eventbus.Terminal{IP: "1.2.3.4"}
	dev, err := e.app.Device.Purchase(ctx, device.PurchaseInput{Name: "设备", Country: "US", Actor: e.owner, Terminal: term})
	if err != nil {
		t.Fatalf("申购失败: %v", err)
	}
	operator := e.owner
	operator.Roles = nil // 无角色 = 无权限（老板权限来自角色，不来自身份）
	if err := e.app.Device.Retire(ctx, dev.ID, operator, term); err == nil {
		t.Fatal("无权限者退役设备必须被拒绝")
	}
}
