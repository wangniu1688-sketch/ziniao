package network

import (
	"context"
	"errors"
	"testing"
	"time"
)

// namedVendor 可命名、可注入失败的假代理商（多代理商路由的测试替身）。
type namedVendor struct {
	attachableVendor
	name   string
	acqErr error
	// released / attached 用来验证「归还与复用找的是不是当初那一家」。
	released []string
	attached []string
}

func newNamedVendor(name string, hosts ...string) *namedVendor {
	return &namedVendor{attachableVendor: attachableVendor{newFakeVendor(hosts...)}, name: name}
}

func (p *namedVendor) Name() string { return p.name }

func (p *namedVendor) Acquire(ctx context.Context, r Region) (*AcquiredProxy, error) {
	if p.acqErr != nil {
		return nil, p.acqErr
	}
	return p.attachableVendor.Acquire(ctx, r)
}

func (p *namedVendor) Attach(ctx context.Context, id string) (*AcquiredProxy, error) {
	p.attached = append(p.attached, id)
	return p.attachableVendor.Attach(ctx, id)
}

func (p *namedVendor) Release(ctx context.Context, id string) error {
	p.released = append(p.released, id)
	return p.attachableVendor.Release(ctx, id)
}

func ctxBg() context.Context { return context.Background() }

// TestRouterPrefersPrimaryVendor 主备语义：主供应商健康时，备用一家都不该被用到。
// 备用的意义是「主用不了才上」，不是「两家一起分摊」——后者会让成本与配额都失控。
func TestRouterPrefersPrimaryVendor(t *testing.T) {
	rt := NewRouter(
		Vendor{Provider: newNamedVendor("primary"), Meta: VendorMeta{Name: "primary", Priority: 0}},
		Vendor{Provider: newNamedVendor("backup"), Meta: VendorMeta{Name: "backup", Priority: 10}},
	)
	for i := 0; i < 5; i++ {
		sel, err := rt.Select(ctxBg(), Region{Country: "US"}, nil)
		if err != nil {
			t.Fatalf("第 %d 次选择失败: %v", i, err)
		}
		if sel.Name != "primary" {
			t.Fatalf("主供应商可用时不该走到备用，实际选了 %s", sel.Name)
		}
	}
}

// TestRouterSpreadsByWeight 同优先级按权重分摊：权重 3:1 应大致分到 3:1。
func TestRouterSpreadsByWeight(t *testing.T) {
	rt := NewRouter(
		Vendor{Provider: newNamedVendor("a"), Meta: VendorMeta{Name: "a", Weight: 3}},
		Vendor{Provider: newNamedVendor("b"), Meta: VendorMeta{Name: "b", Weight: 1}},
	)
	count := map[string]int{}
	for i := 0; i < 8; i++ {
		sel, err := rt.Select(ctxBg(), Region{Country: "US"}, nil)
		if err != nil {
			t.Fatalf("选择失败: %v", err)
		}
		count[sel.Name]++
	}
	if count["a"] != 6 || count["b"] != 2 {
		t.Fatalf("权重 3:1 应分摊为 6:2，实际 %v", count)
	}
}

// TestRouterFiltersByCountry 只把单子派给卖这个国家的代理商。
func TestRouterFiltersByCountry(t *testing.T) {
	rt := NewRouter(
		Vendor{Provider: newNamedVendor("us"), Meta: VendorMeta{Name: "us", Countries: []string{"US"}}},
		Vendor{Provider: newNamedVendor("jp"), Meta: VendorMeta{Name: "jp", Countries: []string{"JP"}}},
	)
	sel, err := rt.Select(ctxBg(), Region{Country: "JP"}, nil)
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if sel.Name != "jp" {
		t.Fatalf("JP 应命中 jp，实际 %s", sel.Name)
	}
	if _, err := rt.Select(ctxBg(), Region{Country: "DE"}, nil); !errors.Is(err, ErrNoVendorAvailable) {
		t.Fatalf("没有代理商卖 DE 时应报 ErrNoVendorAvailable，实际 %v", err)
	}
}

// TestRouterHonoursConcurrencyCap 并发名额用满后必须溢出到下一家，
// 而不是继续向同一家申请（超配额会被代理商限流甚至断流）。
func TestRouterHonoursConcurrencyCap(t *testing.T) {
	rt := NewRouter(
		// small 是主供应商（priority 0），big 是备用：名额用满才溢出
		Vendor{Provider: newNamedVendor("small"), Meta: VendorMeta{Name: "small", MaxConcurrent: 1}},
		Vendor{Provider: newNamedVendor("big"), Meta: VendorMeta{Name: "big", Priority: 10}},
	)
	first, err := rt.Select(ctxBg(), Region{Country: "US"}, nil)
	if err != nil {
		t.Fatalf("首次选择失败: %v", err)
	}
	if first.Name != "small" {
		t.Fatalf("未满时应先走 small，实际 %s", first.Name)
	}
	second, err := rt.Select(ctxBg(), Region{Country: "US"}, nil)
	if err != nil {
		t.Fatalf("第二次选择失败: %v", err)
	}
	if second.Name != "big" {
		t.Fatalf("small 名额用满后应溢出到 big，实际 %s", second.Name)
	}
	// 归还后 small 重新可用
	rt.Done("small")
	third, _ := rt.Select(ctxBg(), Region{Country: "US"}, nil)
	if third.Name != "small" {
		t.Fatalf("归还名额后应回到 small，实际 %s", third.Name)
	}
}

// TestRouterCircuitBreaksAndRecovers 连续失败熔断，冷却结束后恢复：
// 一家接口挂了时，每次启动都等满超时的代价谁也受不了。
func TestRouterCircuitBreaksAndRecovers(t *testing.T) {
	rt := NewRouter(
		Vendor{Provider: newNamedVendor("flaky"), Meta: VendorMeta{Name: "flaky"}},
		Vendor{Provider: newNamedVendor("solid"), Meta: VendorMeta{Name: "solid", Priority: 10}},
	)
	now := time.Now()
	rt.now = func() time.Time { return now }
	rt.SetCircuit(2, time.Minute)

	for i := 0; i < 2; i++ {
		rt.Fail("flaky", errors.New("boom"))
		rt.Done("flaky")
	}
	sel, err := rt.Select(ctxBg(), Region{Country: "US"}, nil)
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if sel.Name != "solid" {
		t.Fatalf("熔断后应切到 solid，实际 %s", sel.Name)
	}
	if !rt.Snapshot()[0].CircuitOpen {
		t.Fatalf("快照应标记 flaky 处于熔断中: %+v", rt.Snapshot())
	}
	// 冷却结束：重新参与选择
	now = now.Add(2 * time.Minute)
	sel, err = rt.Select(ctxBg(), Region{Country: "US"}, nil)
	if err != nil {
		t.Fatalf("冷却后选择失败: %v", err)
	}
	if sel.Name != "flaky" {
		t.Fatalf("冷却结束后应回到 flaky，实际 %s", sel.Name)
	}
	rt.Success("flaky")
	rt.Done("flaky")
	if rt.Snapshot()[0].CircuitOpen {
		t.Fatalf("成功后应解除熔断")
	}
}

// TestRouterDisabledVendorNeverSelected 手工摘除的家一次都不能被选中。
func TestRouterDisabledVendorNeverSelected(t *testing.T) {
	rt := NewRouter(
		Vendor{Provider: newNamedVendor("bad"), Meta: VendorMeta{Name: "bad", Disabled: true}},
		Vendor{Provider: newNamedVendor("good"), Meta: VendorMeta{Name: "good", Priority: 10}},
	)
	sel, err := rt.Select(ctxBg(), Region{Country: "US"}, nil)
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if sel.Name != "good" {
		t.Fatalf("已摘除的代理商不该被选中，实际 %s", sel.Name)
	}
}

// TestRouterGetUnknownReturnsNil 未知名字返回真 nil。
//
// 这条专门盯着一个坑：直接把 nil 的内部指针装进 Provider 接口会得到一个非空接口值，
// 调用方 p == nil 判不出来，紧接着就在空指针上调 Release 把进程打崩。
func TestRouterGetUnknownReturnsNil(t *testing.T) {
	rt := NewRouter(Vendor{Provider: newNamedVendor("a"), Meta: VendorMeta{Name: "a"}})
	if p := rt.Get("nope"); p != nil {
		t.Fatalf("未知代理商应返回 nil，实际 %#v", p)
	}
	if p := rt.Get(""); p != nil {
		t.Fatalf("空名字应返回 nil，实际 %#v", p)
	}
	if p := rt.Get("a"); p == nil {
		t.Fatalf("已登记代理商应能取到")
	}
}

// TestRouterRejectsDuplicateName 重名会让「按名字归还资源」变成抽奖，必须拒绝。
func TestRouterRejectsDuplicateName(t *testing.T) {
	rt := NewRouter(Vendor{Provider: newNamedVendor("a"), Meta: VendorMeta{Name: "a"}})
	if err := rt.Add(Vendor{Provider: newNamedVendor("a"), Meta: VendorMeta{Name: "a"}}); err == nil {
		t.Fatalf("重名登记应报错")
	}
	if err := rt.Add(Vendor{Provider: nil, Meta: VendorMeta{Name: "x"}}); err == nil {
		t.Fatalf("空适配器登记应报错")
	}
}

// TestRouterKeepsOptionalAttacher 路由不能把代理商的可选能力（Attach）弄丢：
// 丢了它，「重启后复用同一出口」这条路径会对所有代理商静默失效。
func TestRouterKeepsOptionalAttacher(t *testing.T) {
	v := newNamedVendor("a")
	rt := NewRouter(Vendor{Provider: v, Meta: VendorMeta{Name: "a"}})
	p := rt.Get("a")
	if _, ok := p.(Attacher); !ok {
		t.Fatalf("取回的代理商丢失了 Attacher 能力（类型 %T）", p)
	}
	sel, err := rt.Select(ctxBg(), Region{Country: "US"}, nil)
	if err != nil {
		t.Fatalf("选择失败: %v", err)
	}
	if _, ok := sel.Provider.(Attacher); !ok {
		t.Fatalf("Select 返回的适配器丢失了 Attacher 能力（类型 %T）", sel.Provider)
	}
}
