package network

import (
	"errors"
	"testing"
	"time"

	"ziniao/platform/store/mem"
)

func newMultiAllocator(vs ...Vendor) (*Allocator, *mem.Store) {
	bs := mem.New()
	a := NewAllocatorWithRouter(NewRouter(vs...), time.Hour)
	a.SetBindingStore(bs.EgressBindings())
	return a, bs
}

// TestAllocatorFailsOverToNextVendor 一家挂了必须自动换下一家：
// 接多家代理商的全部意义就在这条路径上，否则多接几家只是一张好看的配置表。
func TestAllocatorFailsOverToNextVendor(t *testing.T) {
	down := newNamedVendor("down")
	down.acqErr = errors.New("代理商接口 500")
	up := newNamedVendor("up", "198.51.100.9")
	a, bs := newMultiAllocator(
		Vendor{Provider: down, Meta: VendorMeta{Name: "down"}},
		Vendor{Provider: up, Meta: VendorMeta{Name: "up", Priority: 10}},
	)
	ctx := tenantCtx()
	res, err := a.Allocate(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("一家挂掉后应自动换家，实际失败: %v", err)
	}
	if res == nil || res.Host == "" {
		t.Fatalf("未拿到可用出口")
	}
	b, err := bs.EgressBindings().Get(ctx, "env-1")
	if err != nil || b == nil {
		t.Fatalf("绑定未落库: %v", err)
	}
	if b.Provider != "up" {
		t.Fatalf("绑定应记录实际签发方 up，实际 %s", b.Provider)
	}
}

// TestAllocatorAllVendorsDownReportsError 全挂时必须报错，而不是把出口悄悄发下去。
func TestAllocatorAllVendorsDownReportsError(t *testing.T) {
	down := newNamedVendor("down")
	down.acqErr = errors.New("boom")
	a, _ := newMultiAllocator(Vendor{Provider: down, Meta: VendorMeta{Name: "down"}})
	if _, err := a.Allocate(tenantCtx(), "env-1", Region{Country: "US"}); err == nil {
		t.Fatalf("全部代理商不可用时必须报错")
	}
}

// TestAllocatorReturnsResourceToIssuingVendor 归还必须找当初签发它的那一家。
//
// 找错家的后果不是「报错」这么简单：资源 ID 只在签发方内部有意义，
// 拿 A 家的 ID 去 B 家归还，若 B 家恰好有同名订单，退掉的就是别人正在用的出口。
func TestAllocatorReturnsResourceToIssuingVendor(t *testing.T) {
	small := newNamedVendor("small", "198.51.100.1")
	big := newNamedVendor("big", "198.51.100.2")
	a, bs := newMultiAllocator(
		// small 只有 1 个名额：env-1 走 small，env-2 溢出到 big
		Vendor{Provider: small, Meta: VendorMeta{Name: "small", MaxConcurrent: 1}},
		Vendor{Provider: big, Meta: VendorMeta{Name: "big", Priority: 10}},
	)
	ctx := tenantCtx()
	if _, err := a.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("env-1 分配失败: %v", err)
	}
	if _, err := a.Allocate(ctx, "env-2", Region{Country: "US"}); err != nil {
		t.Fatalf("env-2 分配失败: %v", err)
	}
	b1, _ := bs.EgressBindings().Get(ctx, "env-1")
	b2, _ := bs.EgressBindings().Get(ctx, "env-2")
	if b1.Provider != "small" || b2.Provider != "big" {
		t.Fatalf("两家应各签一单，实际 %s / %s", b1.Provider, b2.Provider)
	}
	if err := a.Release(ctx, "env-1"); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if len(small.released) != 1 || small.released[0] != b1.ResourceID {
		t.Fatalf("env-1 的资源应还给 small，实际 small 收到 %v", small.released)
	}
	if len(big.released) != 0 {
		t.Fatalf("big 不该收到归还，实际 %v", big.released)
	}
	// 名额已释放：下一个环境应该又能用上 small
	if _, err := a.Allocate(ctx, "env-3", Region{Country: "US"}); err != nil {
		t.Fatalf("env-3 分配失败: %v", err)
	}
	b3, _ := bs.EgressBindings().Get(ctx, "env-3")
	if b3.Provider != "small" {
		t.Fatalf("名额归还后应回到 small，实际 %s", b3.Provider)
	}
}

// TestAllocatorReusesExitFromRecordedVendor 重启后必须向「当初那家」取回凭据。
// 找错家的后果是这个环境被塞一个别人的出口——换 IP 的告警还会因此发不出来。
func TestAllocatorReusesExitFromRecordedVendor(t *testing.T) {
	jpOnly := newNamedVendor("jp-only", "198.51.100.7")
	usShop := newNamedVendor("us-shop", "198.51.100.8")
	a1, bs := newMultiAllocator(
		Vendor{Provider: jpOnly, Meta: VendorMeta{Name: "jp-only", Countries: []string{"JP"}}},
		Vendor{Provider: usShop, Meta: VendorMeta{Name: "us-shop", Priority: 10}},
	)
	ctx := tenantCtx()
	first, err := a1.Allocate(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("首次分配失败: %v", err)
	}
	// 模拟重启：内存全空，只剩库
	a2, _ := newMultiAllocator(
		Vendor{Provider: jpOnly, Meta: VendorMeta{Name: "jp-only", Countries: []string{"JP"}}},
		Vendor{Provider: usShop, Meta: VendorMeta{Name: "us-shop", Priority: 10}},
	)
	a2.SetBindingStore(bs.EgressBindings())
	again, err := a2.Allocate(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("重启后分配失败: %v", err)
	}
	if again.Addr() != first.Addr() {
		t.Fatalf("重启后出口变了：%s -> %s", first.Addr(), again.Addr())
	}
	if len(usShop.attached) != 1 {
		t.Fatalf("应向 us-shop 取回凭据，实际 attached=%v", usShop.attached)
	}
	if len(jpOnly.attached) != 0 {
		t.Fatalf("jp-only 不该被调用: %v", jpOnly.attached)
	}
}

// TestSameResourceIDFromTwoVendorsStaysSeparate 两家代理商会签发同名资源 ID
// （各家的订单号互不相干，内网里 "res-1" 撞车是常态）。
//
// 进程内若用裸 resource ID 作键，第二条会把第一条覆盖掉：归还时可能退掉
// **另一家正在用的线路**，名额也会记到别人头上。库侧唯一索引是
// (provider, resource_id)，这里必须同口径——本轮就是从这条用例里发现
// resources/provOf 仍是裸键的。
func TestSameResourceIDFromTwoVendorsStaysSeparate(t *testing.T) {
	va := newNamedVendor("va", "198.51.100.1")
	vb := newNamedVendor("vb", "198.51.100.2")
	a, _ := newMultiAllocator(
		Vendor{Provider: va, Meta: VendorMeta{Name: "va", MaxConcurrent: 1}},
		Vendor{Provider: vb, Meta: VendorMeta{Name: "vb", Priority: 10, MaxConcurrent: 1}},
	)
	ctx := tenantCtx()
	if _, err := a.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("env-1 分配失败: %v", err)
	}
	if _, err := a.Allocate(ctx, "env-2", Region{Country: "US"}); err != nil {
		t.Fatalf("env-2 分配失败: %v", err)
	}
	// 前提校验：两家确实签发同一个资源 ID（否则这条用例测不到东西）
	if len(va.released)+len(vb.released) != 0 || va.issued["res-1"] == nil || vb.issued["res-1"] == nil {
		t.Fatalf("用例前提不成立：两家应各自签发 res-1")
	}

	inUse := func(name string) int {
		for _, s := range a.Router().Snapshot() {
			if s.Name == name {
				return s.InUse
			}
		}
		return -1
	}
	if inUse("va") != 1 || inUse("vb") != 1 {
		t.Fatalf("两家应各占 1 个名额，实际 va=%d vb=%d", inUse("va"), inUse("vb"))
	}

	if err := a.Release(ctx, "env-1"); err != nil {
		t.Fatalf("释放 env-1 失败: %v", err)
	}
	if len(va.released) != 1 {
		t.Fatalf("env-1 的资源应还给 va，实际 %v", va.released)
	}
	if len(vb.released) != 0 {
		t.Fatalf("不该拿同名的 res-1 去 vb 归还（那是退掉别人的线路），实际 %v", vb.released)
	}
	if got := inUse("vb"); got != 1 {
		t.Fatalf("释放 env-1 不该影响 vb 的名额，实际 in_use=%d", got)
	}
	if got := inUse("va"); got != 0 {
		t.Fatalf("va 的名额应归零，实际 %d", got)
	}

	if err := a.Release(ctx, "env-2"); err != nil {
		t.Fatalf("释放 env-2 失败: %v", err)
	}
	if len(vb.released) != 1 {
		t.Fatalf("env-2 的资源应还给 vb，实际 %v", vb.released)
	}
}

// TestReleaseOfRestoredBindingKeepsOtherSlots 归还「重启后按库取回」的绑定时，
// 不能把别的环境在本进程里占的并发名额一起还回去。
//
// 这条不是理论风险：Restore 走的是 Attach，从未经过 Router.Select，
// 归还路径若无条件 Done 一次，in_use 会出现「负数式」的假空位——
// 代理商实际额度已满，系统却继续往它身上派单，最后表现为对方限流/断流。
func TestReleaseOfRestoredBindingKeepsOtherSlots(t *testing.T) {
	ctx := tenantCtx()
	v := newNamedVendor("v", "198.51.100.1", "198.51.100.2")
	a1, bs := newMultiAllocator(Vendor{Provider: v, Meta: VendorMeta{Name: "v"}})
	if _, err := a1.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("首次分配失败: %v", err)
	}

	// 模拟重启：新分配器、内存全空，只有库。
	a2 := NewAllocatorWithRouter(NewRouter(Vendor{Provider: v, Meta: VendorMeta{Name: "v"}}), time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	// env-2 在本进程里真占一个名额（Select 计数 = 1）
	if _, err := a2.Allocate(ctx, "env-2", Region{Country: "US"}); err != nil {
		t.Fatalf("env-2 分配失败: %v", err)
	}
	if got := a2.Router().Snapshot()[0].InUse; got != 1 {
		t.Fatalf("env-2 应在路由里占 1 个名额，实际 %d", got)
	}

	// env-1 是重启取回的：释放它不该动 env-2 的名额。
	if err := a2.Release(ctx, "env-1"); err != nil {
		t.Fatalf("释放 env-1 失败: %v", err)
	}
	if got := a2.Router().Snapshot()[0].InUse; got != 1 {
		t.Fatalf("释放重启取回的绑定扣掉了别人的名额：in_use=%d（应为 1）", got)
	}

	// 本进程申请的 env-2 释放后必须真的归还名额，否则额度会越用越少。
	if err := a2.Release(ctx, "env-2"); err != nil {
		t.Fatalf("释放 env-2 失败: %v", err)
	}
	if got := a2.Router().Snapshot()[0].InUse; got != 0 {
		t.Fatalf("本进程申请的出口释放后名额应归零，实际 %d", got)
	}
}

// TestAllocatorDropsBindingOfRetiredVendor 代理商已下架（改名/摘除）时，
// 宁可重新申请并触发轮换告警，也不能拿它的资源 ID 去别家碰运气。
func TestAllocatorDropsBindingOfRetiredVendor(t *testing.T) {
	old := newNamedVendor("old", "198.51.100.20")
	a1, bs := newMultiAllocator(Vendor{Provider: old, Meta: VendorMeta{Name: "old"}})
	ctx := tenantCtx()
	if _, err := a1.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("首次分配失败: %v", err)
	}
	// 重启后路由表里只剩 new：old 的绑定失联
	nv := newNamedVendor("new", "198.51.100.21")
	a2, _ := newMultiAllocator(Vendor{Provider: nv, Meta: VendorMeta{Name: "new"}})
	a2.SetBindingStore(bs.EgressBindings())
	res, err := a2.Allocate(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("代理商下架后应重新申请而不是失败: %v", err)
	}
	if res.Host == "198.51.100.20" {
		t.Fatalf("不该复用已下架代理商的出口")
	}
	if len(nv.released) != 0 {
		t.Fatalf("新代理商不该收到 old 的资源归还，实际 %v", nv.released)
	}
}
