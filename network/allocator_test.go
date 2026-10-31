package network

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stubbornProvider 无论申请多少次都返回同一个资源——用来验证「宁愿失败也不能共用出口」。
// 真实代理商确实会把同一个出口重复卖给你（尤其带粘性/端口池复用的套餐），
// 这种退化路径必须被显式拦截，否则两个店铺就悄悄共用 IP 了。
type stubbornProvider struct {
	seq   int
	uniq  bool
	hosts []string
}

func (p *stubbornProvider) Name() string { return "stubborn" }
func (p *stubbornProvider) ListRegions(context.Context) ([]Region, error) {
	return []Region{{Code: "US", Country: "US"}}, nil
}
func (p *stubbornProvider) Release(context.Context, string) error { return nil }
func (p *stubbornProvider) Health(context.Context) error          { return nil }
func (p *stubbornProvider) Acquire(_ context.Context, r Region) (*AcquiredProxy, error) {
	if p.uniq {
		p.seq++
	} else {
		p.seq = 1
	}
	host := "203.0.113.1"
	if p.uniq && p.seq > 1 {
		host = p.hosts[(p.seq-1)%len(p.hosts)]
	}
	return &AcquiredProxy{
		ID:        "res-" + string(rune('a'+p.seq-1)),
		Proxy:     ProxyConfig{ID: "res-x", Protocol: ProtoSOCKS5, Host: host, Port: 1080 + p.seq, Country: r.Country, ASNType: ASNResidential},
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

// TestAllocatorNeverSharesResource 核心不变量：两个环境绝不能拿到同一个出口资源。
func TestAllocatorNeverSharesResource(t *testing.T) {
	p := &stubbornProvider{uniq: true, hosts: []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"}}
	a := NewAllocator(p, time.Hour)
	ctx := context.Background()

	r1, err := a.Allocate(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("env-1 分配失败: %v", err)
	}
	r2, err := a.Allocate(ctx, "env-2", Region{Country: "US"})
	if err != nil {
		t.Fatalf("env-2 分配失败: %v", err)
	}
	if r1.Host == r2.Host && r1.Port == r2.Port {
		t.Fatalf("两个环境共用同一出口 %s:%d —— 多账号会被平台直接关联", r1.Host, r1.Port)
	}
	st := a.Stats()
	if st.Bound != 2 {
		t.Fatalf("应有 2 个绑定，实际 %d", st.Bound)
	}
}

// TestAllocatorRejectsNonExclusiveVendor 代理商始终返回同一资源时必须显式报错，
// 不能悄悄降级成共用出口。
func TestAllocatorRejectsNonExclusiveVendor(t *testing.T) {
	a := NewAllocator(&stubbornProvider{uniq: false}, time.Hour)
	ctx := context.Background()
	if _, err := a.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("首个环境应能拿到资源: %v", err)
	}
	_, err := a.Allocate(ctx, "env-2", Region{Country: "US"})
	if err == nil {
		t.Fatal("代理商始终复用同一出口时应报错，实际却分配成功了（= 两店共 IP）")
	}
	if !errors.Is(err, ErrNoExclusiveResource) {
		t.Fatalf("错误类型应为 ErrNoExclusiveResource，实际: %v", err)
	}
}

// TestAllocatorSticky 同一环境重复分配必须拿到同一个出口，
// 否则平台会看到「同一账号出口 IP 频繁跳变」，这本身也是关联信号。
func TestAllocatorSticky(t *testing.T) {
	a := NewAllocator(&stubbornProvider{uniq: true, hosts: []string{"h1", "h2"}}, time.Hour)
	ctx := context.Background()
	first, err := a.Allocate(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	for i := 0; i < 3; i++ {
		got, err := a.Allocate(ctx, "env-1", Region{Country: "US"})
		if err != nil {
			t.Fatalf("重复分配失败: %v", err)
		}
		if got.Host != first.Host || got.Port != first.Port {
			t.Fatalf("第 %d 次出口变了：%s:%d != %s:%d", i+1, got.Host, got.Port, first.Host, first.Port)
		}
	}
	if st := a.Stats(); st.Total != 1 {
		t.Fatalf("重复分配不应向厂商重复申请，资源总数 %d", st.Total)
	}
}

// TestAllocatorReleaseFreesSlot 释放后资源必须可被其它环境接管。
func TestAllocatorReleaseFreesSlot(t *testing.T) {
	p := &stubbornProvider{uniq: true, hosts: []string{"h1", "h2"}}
	a := NewAllocator(p, time.Hour)
	ctx := context.Background()
	if _, err := a.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if err := a.Release(ctx, "env-1"); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if st := a.Stats(); st.Total != 0 || st.Bound != 0 {
		t.Fatalf("释放后应无残留: %+v", st)
	}
	if _, err := a.Allocate(ctx, "env-2", Region{Country: "US"}); err != nil {
		t.Fatalf("释放后应可再分配: %v", err)
	}
	// 幂等：重复释放不应报错
	if err := a.Release(ctx, "env-1"); err != nil {
		t.Fatalf("重复释放不应报错: %v", err)
	}
}

// TestAllocatorReapsExpired 过期资源必须被巡检回收。
func TestAllocatorReapsExpired(t *testing.T) {
	p := &stubbornProvider{uniq: true, hosts: []string{"h1", "h2"}}
	a := NewAllocator(p, time.Nanosecond) // 兜底租约极短 -> 立刻过期
	ctx := context.Background()
	res, err := a.Allocate(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	// 覆写到期时间为过去
	a.mu.Lock()
	if r, ok := a.resources[a.byEnv["env-1"]]; ok {
		r.ExpiresAt = time.Now().Add(-time.Minute)
	}
	a.mu.Unlock()
	if n := a.Reap(ctx); n != 1 {
		t.Fatalf("应回收 1 个过期资源，实际 %d", n)
	}
	if st := a.Stats(); st.Total != 0 {
		t.Fatalf("回收后仍有资源: %+v", st)
	}
	// 过期后重新分配应拿到新资源（粘性不能把它钉在死掉的资源上）
	if _, err := a.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("过期后重新分配失败: %v", err)
	}
	_ = res
}

func TestAllocatorRequiresEnvID(t *testing.T) {
	a := NewAllocator(&stubbornProvider{uniq: true, hosts: []string{"h1"}}, time.Hour)
	if _, err := a.Allocate(context.Background(), "", Region{Country: "US"}); err == nil {
		t.Fatal("缺少 envID 必须报错")
	}
}
