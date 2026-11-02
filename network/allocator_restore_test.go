package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"ziniao/platform/types"
)

// TestRestoreInflightRefillsQuota 重启后必须按库里的既存绑定回填路由名额。
//
// 这条不是性能优化：不回填就等于「重启后的一段时间里名额无限」——
// 代理商那边的并发其实已经满了（上一个进程留下的绑定都还在它身上），
// 我们却继续派单，对方限流/断流时这边的监控还是一片祥和。
func TestRestoreInflightRefillsQuota(t *testing.T) {
	ctx := tenantCtx()
	p := newNamedVendor("v", "198.51.100.1", "198.51.100.2", "198.51.100.3")
	a1, bs := newMultiAllocator(Vendor{Provider: p, Meta: VendorMeta{Name: "v", MaxConcurrent: 2}})
	for _, env := range []string{"env-1", "env-2"} {
		if _, err := a1.Allocate(ctx, env, Region{Country: "US"}); err != nil {
			t.Fatalf("%s 分配失败: %v", env, err)
		}
	}

	// 模拟重启：内存全空，只剩库。
	a2 := NewAllocatorWithRouter(NewRouter(Vendor{Provider: p, Meta: VendorMeta{Name: "v", MaxConcurrent: 2}}), time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	if got := a2.Router().Snapshot()[0].InUse; got != 0 {
		t.Fatalf("重启后内存计数应为 0（这正是要补的洞），实际 %d", got)
	}
	// 不回填的对照组：名额看着是空的，多出来的环境会被派给已经满额的代理商。
	if _, err := a2.Allocate(ctx, "env-x", Region{Country: "US"}); err != nil {
		t.Fatalf("未回填时第 3 个环境会被放行（这正是超卖窗口）: %v", err)
	}
	if err := a2.Release(ctx, "env-x"); err != nil {
		t.Fatalf("清理对照组失败: %v", err)
	}

	n, err := a2.RestoreInflight(ctx, []string{"t-1"})
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("库里有 2 条绑定，应回填 2 个名额，实际 %d", n)
	}
	if got := a2.Router().Snapshot()[0].InUse; got != 2 {
		t.Fatalf("回填后在用应为 2，实际 %d", got)
	}
	// 名额已满：第三个环境不该再派给这家
	if _, err := a2.Allocate(ctx, "env-3", Region{Country: "US"}); err == nil {
		t.Fatal("名额已按库回填为满，不应再往下派单（否则就是超卖）")
	}

	// 归还一条：这个坑必须真的让出来，否则一次重启 = 永久性的配额泄漏
	if err := a2.Release(ctx, "env-1"); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if got := a2.Router().Snapshot()[0].InUse; got != 1 {
		t.Fatalf("释放库里取回的绑定应让出名额，实际 %d（应为 1）", got)
	}
	if _, err := a2.Allocate(ctx, "env-3", Region{Country: "US"}); err != nil {
		t.Fatalf("让出名额后应能继续分配: %v", err)
	}
}

// TestRestoreInflightSkipsRetiredVendor 绑定指向已下架的代理商时不计数、不静默放行。
//
// 这类绑定对应的资源已经还不回去了（找不到家），但它仍然占着对方名额，
// 所以只告警：既不能假装没有，也不能拿它去顶新环境的名额。
func TestRestoreInflightSkipsRetiredVendor(t *testing.T) {
	ctx := tenantCtx()
	old := newNamedVendor("old", "198.51.100.1")
	a1, bs := newMultiAllocator(Vendor{Provider: old, Meta: VendorMeta{Name: "old"}})
	if _, err := a1.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("首次分配失败: %v", err)
	}

	// 重启后路由表里只剩 new：old 的绑定失联
	nv := newNamedVendor("new", "198.51.100.9")
	a2 := NewAllocatorWithRouter(NewRouter(Vendor{Provider: nv, Meta: VendorMeta{Name: "new"}}), time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	n, err := a2.RestoreInflight(ctx, []string{"t-1"})
	if err != nil {
		t.Fatalf("回填不应因为代理商下架而失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("已下架代理商的绑定不该计入名额，实际 %d", n)
	}
	for _, s := range a2.Router().Snapshot() {
		if s.Name == "new" && s.InUse != 0 {
			t.Fatalf("新代理商不该被旧绑定占坑，实际 %d", s.InUse)
		}
	}
}

// TestRestoreInflightFailsClosed 某个租户读不到绑定时必须报错，而不是半回填。
//
// 半回填比不回填更危险：它看起来「回填过了」，实际只有一部分代理商的配额是准的。
func TestRestoreInflightFailsClosed(t *testing.T) {
	ctx := tenantCtx()
	p := newNamedVendor("v", "198.51.100.1")
	a, bs := newMultiAllocator(Vendor{Provider: p, Meta: VendorMeta{Name: "v"}})
	if _, err := a.Allocate(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	a2 := NewAllocatorWithRouter(NewRouter(Vendor{Provider: p, Meta: VendorMeta{Name: "v"}}), time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	a2.SetBindingStore(brokenListStore{inner: bs.EgressBindings()})
	if _, err := a2.RestoreInflight(ctx, []string{"t-1"}); err == nil {
		t.Fatal("读不到绑定却返回成功：半回填会让配额看起来是准的")
	}
}

// brokenListStore 只让「按租户列出绑定」失败，用来验证 fail-closed。
type brokenListStore struct{ inner BindingStore }

func (s brokenListStore) Bind(ctx context.Context, b *types.EgressBinding) (bool, error) {
	return s.inner.Bind(ctx, b)
}

func (s brokenListStore) Get(ctx context.Context, envID string) (*types.EgressBinding, error) {
	return s.inner.Get(ctx, envID)
}

func (s brokenListStore) Release(ctx context.Context, envID string) error {
	return s.inner.Release(ctx, envID)
}

func (s brokenListStore) DeleteExpired(ctx context.Context, tenantID string, now time.Time) ([]types.EgressBinding, error) {
	return s.inner.DeleteExpired(ctx, tenantID, now)
}

func (s brokenListStore) ListByTenant(context.Context) ([]types.EgressBinding, error) {
	return nil, errors.New("db down")
}
