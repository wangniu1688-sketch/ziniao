package network

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

// fakeVendor 可挂载点的假代理商：每次 Acquire 发一个新资源，可指定是否支持 Attach。
type fakeVendor struct {
	mu    sync.Mutex
	seq   int
	hosts []string
	// sameExit 让每次申请都返回同一个 host:port（不同订单号）：
	// 真实代理商确实会这么干，而平台看到的是地址，不是我们的订单号。
	sameExit bool
	issued   map[string]*AcquiredProxy
	// released 记录被归还的资源 ID：回收/作废必须真的还给厂商，否则持续计费。
	released []string
	acquires int
}

func newFakeVendor(hosts ...string) *fakeVendor {
	return &fakeVendor{hosts: hosts, issued: map[string]*AcquiredProxy{}}
}

func (p *fakeVendor) Name() string { return "fake-vendor" }
func (p *fakeVendor) ListRegions(context.Context) ([]Region, error) {
	return []Region{{Code: "US", Country: "US"}}, nil
}

func (p *fakeVendor) Acquire(_ context.Context, r Region) (*AcquiredProxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	p.acquires++
	id := "res-" + itoaTest(p.seq)
	host := "198.51.100.1"
	if n := len(p.hosts); n > 0 {
		host = p.hosts[(p.seq-1)%n]
	}
	port := 1080 + p.seq
	if p.sameExit {
		port = 1080
	}
	res := &AcquiredProxy{
		ID: id,
		Proxy: ProxyConfig{
			ID: id, Protocol: ProtoSOCKS5, Host: host, Port: port,
			Username: "u", Password: "p", Country: r.Country, ASNType: ASNResidential,
		},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	p.issued[id] = res
	return res, nil
}

func (p *fakeVendor) Release(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.released = append(p.released, id)
	delete(p.issued, id)
	return nil
}

func (p *fakeVendor) Health(context.Context) error { return nil }

func (p *fakeVendor) acquireCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acquires
}

func (p *fakeVendor) releasedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.released))
	copy(out, p.released)
	return out
}

// attachableVendor 支持按 ID 取回凭据的代理商（多数住宅代理商都有这个接口）。
type attachableVendor struct{ *fakeVendor }

func (p attachableVendor) Attach(_ context.Context, id string) (*AcquiredProxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.issued[id]
	if !ok {
		return nil, errors.New("资源不存在")
	}
	cp := *res
	return &cp, nil
}

// brokenBindings 只用来注入「落库失败」，验证 fail-closed。
type brokenBindings struct{ BindingStore }

func (b brokenBindings) Bind(context.Context, *types.EgressBinding) (bool, error) {
	return false, errors.New("db down")
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [8]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

func tenantCtx() context.Context {
	return store.WithTenant(context.Background(), "t-1")
}

// TestRestartKeepsSameExit 本次落库的核心目的：进程重启后环境必须还是同一个出口。
//
// 没有这条，每次发布/deploy 都会给全部店铺换一次登录 IP——对平台来说
// 「同一账号忽然换 IP」是最强的关联信号之一，等于自己给自己制造风险。
func TestRestartKeepsSameExit(t *testing.T) {
	ctx := tenantCtx()
	v := attachableVendor{newFakeVendor("198.51.100.1", "198.51.100.2")}
	bs := mem.New()

	a1 := NewAllocator(v, time.Hour)
	a1.SetBindingStore(bs.EgressBindings())
	first, err := a1.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("首次分配失败: %v", err)
	}

	// 模拟重启：新分配器、内存全空，只有库还在。
	a2 := NewAllocator(v, time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	again, err := a2.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("重启后分配失败: %v", err)
	}
	if again.Proxy.Addr() != first.Proxy.Addr() {
		t.Fatalf("重启后出口变了：%s -> %s（这就是要消灭的行为）", first.Proxy.Addr(), again.Proxy.Addr())
	}
	if again.Rotated {
		t.Fatalf("复用同一出口不该标记轮换: prev=%s cur=%s", again.PrevAddr, again.Proxy.Addr())
	}
	if n := v.acquireCount(); n != 1 {
		t.Fatalf("重启后应复用既有资源，实际向厂商申请了 %d 次", n)
	}

	row, err := bs.EgressBindings().Get(ctx, "env-1")
	if err != nil || row == nil {
		t.Fatalf("绑定应已落库: row=%v err=%v", row, err)
	}
	if row.LastExit != first.Proxy.Addr() {
		t.Fatalf("last_exit 应记录实际出口，实际 %q", row.LastExit)
	}
	if row.ExpiresAt.IsZero() {
		t.Fatal("到期时间必须落库，否则回收任务无从判断")
	}
}

// TestRestartWithoutAttachReportsRotation 厂商不支持按 ID 取回凭据时，出口必然变，
// 但绝不能静默变：必须带出上一次出口并标记轮换（运维据此判断要不要紧急处理）。
func TestRestartWithoutAttachReportsRotation(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1", "198.51.100.2")
	bs := mem.New()

	a1 := NewAllocator(v, time.Hour)
	a1.SetBindingStore(bs.EgressBindings())
	first, err := a1.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("首次分配失败: %v", err)
	}

	a2 := NewAllocator(v, time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	again, err := a2.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("重启后分配失败: %v", err)
	}
	if !again.Rotated {
		t.Fatal("出口已变却未标记轮换，运营会毫不知情")
	}
	if again.PrevAddr != first.Proxy.Addr() {
		t.Fatalf("prev 应是重启前的出口 %s，实际 %s", first.Proxy.Addr(), again.PrevAddr)
	}
	// 旧资源必须归还：凭据取不回来，留着只会继续计费。
	released := v.releasedIDs()
	if len(released) != 1 || released[0] != "res-1" {
		t.Fatalf("旧资源应被归还，实际归还 %v", released)
	}
}

// TestBindingFailureBlocksAllocation 落库失败必须拒绝分配（fail-closed）：
// 发一个「重启后必然换 IP」的出口，比这次起不来贵得多。
func TestBindingFailureBlocksAllocation(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1")
	all := NewAllocator(v, time.Hour)
	all.SetBindingStore(brokenBindings{mem.New().EgressBindings()})

	_, err := all.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err == nil {
		t.Fatal("落库失败时不得下发出口")
	}
	if !errors.Is(err, ErrBindingUnavailable) {
		t.Fatalf("错误类型应为 ErrBindingUnavailable，实际: %v", err)
	}
	if got := v.releasedIDs(); len(got) != 1 {
		t.Fatalf("落库失败时必须把申请到的资源还回去，实际归还 %v", got)
	}
}

// TestPersistedBindingNeedsTenant 有落库就必须有租户上下文。
// 缺租户时静默降级成内存绑定，等于悄悄退化成「重启即换 IP」。
func TestPersistedBindingNeedsTenant(t *testing.T) {
	v := attachableVendor{newFakeVendor("198.51.100.1")}
	a := NewAllocator(v, time.Hour)
	a.SetBindingStore(mem.New().EgressBindings())

	_, err := a.AllocateEx(context.Background(), "env-1", Region{Country: "US"})
	if err == nil {
		t.Fatal("缺租户上下文时应拒绝分配")
	}
	if !errors.Is(err, ErrBindingUnavailable) {
		t.Fatalf("错误类型应为 ErrBindingUnavailable，实际: %v", err)
	}
}

// TestExclusivitySurvivesRestart 两个实例（滚动发布的新旧进程）各自向厂商申请，
// 拿到同一个出口时，只能有一个落库成功：互斥的真相源必须是数据库唯一索引，
// 而不是各自的进程内 map。
func TestExclusivitySurvivesRestart(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1") // 厂商只卖这一个出口，且每次给不同订单号
	v.sameExit = true
	bs := mem.New()

	a1 := NewAllocator(v, time.Hour)
	a1.SetBindingStore(bs.EgressBindings())
	if _, err := a1.AllocateEx(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("实例 1 分配失败: %v", err)
	}

	a2 := NewAllocator(v, time.Hour) // 另一个进程，内存里没有 env-1 的任何痕迹
	a2.SetBindingStore(bs.EgressBindings())
	_, err := a2.AllocateEx(ctx, "env-2", Region{Country: "US"})
	if err == nil {
		t.Fatal("两个环境拿到了同一个出口 IP，数据库互斥失效")
	}
	if !errors.Is(err, ErrNoExclusiveResource) {
		t.Fatalf("错误类型应为 ErrNoExclusiveResource，实际: %v", err)
	}
}

// TestSweepTenantReleasesExpired 重启后内存是空的，只有库知道还占着哪些资源；
// 到期必须归还，否则持续计费并占着代理商名额。
func TestSweepTenantReleasesExpired(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1")
	bs := mem.New()

	a := NewAllocator(v, time.Hour)
	a.SetBindingStore(bs.EgressBindings())
	if _, err := a.AllocateEx(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	// 让绑定到期（等价于时间流逝到租约之后）
	row, _ := bs.EgressBindings().Get(ctx, "env-1")
	row.ExpiresAt = time.Now().Add(-time.Minute)
	if ok, err := bs.EgressBindings().Bind(ctx, row); err != nil || !ok {
		t.Fatalf("准备到期绑定失败: ok=%v err=%v", ok, err)
	}

	// 换一个全新分配器：模拟「重启之后由定时任务收拾残局」。
	a2 := NewAllocator(v, time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	n, err := a2.SweepTenant(ctx, "t-1", time.Now())
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应回收 1 条到期绑定，实际 %d", n)
	}
	if got := v.releasedIDs(); len(got) != 1 || got[0] != "res-1" {
		t.Fatalf("到期资源必须归还厂商，实际归还 %v", got)
	}
	if left, _ := bs.EgressBindings().Get(ctx, "env-1"); left != nil {
		t.Fatalf("到期绑定应已删除，仍残留: %+v", left)
	}
}

// TestExpiredBindingReplacedOnLaunch 库里的绑定已过期时，启动要重新申请，
// 并且必须把「从哪变到哪」说清楚（prev 来自库里的 last_exit）。
func TestExpiredBindingReplacedOnLaunch(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.7")
	bs := mem.New()

	old := &types.EgressBinding{
		EnvID: "env-1", Provider: v.Name(), ResourceID: "res-old",
		Protocol: "socks5", Host: "198.51.100.1", Port: 1080,
		Country: "US", ASNType: "residential",
		LastExit:  "198.51.100.1:1080",
		BoundAt:   time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	if ok, err := bs.EgressBindings().Bind(ctx, old); err != nil || !ok {
		t.Fatalf("准备过期绑定失败: ok=%v err=%v", ok, err)
	}

	a := NewAllocator(v, time.Hour)
	a.SetBindingStore(bs.EgressBindings())
	res, err := a.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if res.Proxy.Addr() == old.LastExit {
		t.Fatalf("过期绑定不应被复用: %s", old.LastExit)
	}
	if !res.Rotated || res.PrevAddr != old.LastExit {
		t.Fatalf("必须标记轮换并带出旧出口: rotated=%v prev=%s", res.Rotated, res.PrevAddr)
	}
	if got := v.releasedIDs(); len(got) != 1 || got[0] != "res-old" {
		t.Fatalf("过期资源应归还厂商，实际归还 %v", got)
	}
	row, err := bs.EgressBindings().Get(ctx, "env-1")
	if err != nil || row == nil || row.ResourceID == "res-old" {
		t.Fatalf("绑定应被新资源替换: row=%+v err=%v", row, err)
	}
}
