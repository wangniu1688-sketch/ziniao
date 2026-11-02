package network

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

// brokenHistory 只用来注入「历史写失败」：历史是可观测性，
// 写不进去不该让环境起不来（否则一次库抖动就是全站停摆）。
type brokenHistory struct{}

func (brokenHistory) AppendEvent(context.Context, *types.EgressBindingEvent) error {
	return errors.New("history down")
}

func listHistory(t *testing.T, bs *mem.Store, f store.EgressHistoryFilter) []types.EgressBindingEvent {
	t.Helper()
	items, _, err := bs.EgressBindings().ListEvents(tenantCtx(), f)
	if err != nil {
		t.Fatalf("读取出口历史失败: %v", err)
	}
	return items
}

// mustEvent 按动作取事件。
//
// 不按数组下标断言顺序：同一毫秒内连续发生的事件（作废→重新申请、到期→回收）
// 时间戳可能完全相同，此时「谁在上面」由 id 决定，对人类没有意义。
// 事件的语义（有哪些动作、带没带上成因）才是这条链路的验收标准。
func mustEvent(t *testing.T, events []types.EgressBindingEvent, action string) types.EgressBindingEvent {
	t.Helper()
	for _, ev := range events {
		if ev.Action == action {
			return ev
		}
	}
	t.Fatalf("缺少 %s 事件，实际 %+v", action, events)
	return types.EgressBindingEvent{}
}

// TestHistoryRecordsLifecycle 绑定与释放都要留档，且释放之后历史仍在。
//
// 这条链路的唯一价值就是「当前绑定没了以后还查得到」：
// 如果 Release 顺手把历史清掉，那和没做历史留档完全一样。
func TestHistoryRecordsLifecycle(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1")
	bs := mem.New()

	a := NewAllocator(v, time.Hour)
	a.SetBindingStore(bs.EgressBindings())
	a.SetHistoryStore(bs.EgressBindings())

	res, err := a.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	events := listHistory(t, bs, store.EgressHistoryFilter{EnvID: "env-1"})
	if len(events) != 1 || events[0].Action != types.EgressActionBound {
		t.Fatalf("首次分配应产生 1 条 bound 事件，实际 %+v", events)
	}
	if got := events[0].ExitAddr; got != res.Proxy.Addr() {
		t.Fatalf("事件里的出口应为 %s，实际 %q", res.Proxy.Addr(), got)
	}
	if events[0].ASN != "AS13335" {
		t.Fatalf("ASN 必须留档（关联检测的唯一判据）：%+v", events[0])
	}
	if events[0].Reason == "" {
		t.Fatal("事件必须带成因：历史里只有动作没有原因是无法排障的")
	}
	if events[0].TenantID != "t-1" {
		t.Fatalf("事件必须归属租户，实际 %q", events[0].TenantID)
	}

	if err := a.Release(ctx, "env-1"); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if cur, _ := bs.EgressBindings().Get(ctx, "env-1"); cur != nil {
		t.Fatalf("释放后当前绑定应已解除: %+v", cur)
	}
	events = listHistory(t, bs, store.EgressHistoryFilter{EnvID: "env-1"})
	if len(events) != 2 {
		t.Fatalf("释放后应有 bound + released 两条事件，实际 %+v", events)
	}
	mustEvent(t, events, types.EgressActionReleased)
	if ev := mustEvent(t, events, types.EgressActionBound); ev.ExitAddr != res.Proxy.Addr() {
		t.Fatalf("释放不应抹掉 bound 历史，实际 %+v", ev)
	}
}

// TestHistoryRecordsReboundWithReason 重启后取不回凭据 → 绑定作废 → 重新申请，
// 历史里必须能读到「dropped(为什么)」与「rebound(从哪个出口到哪个出口)」。
func TestHistoryRecordsReboundWithReason(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1", "198.51.100.2") // 不支持 Attach
	bs := mem.New()

	a1 := NewAllocator(v, time.Hour)
	a1.SetBindingStore(bs.EgressBindings())
	a1.SetHistoryStore(bs.EgressBindings())
	first, err := a1.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("首次分配失败: %v", err)
	}

	// 模拟重启：内存全空，只有库与历史。
	a2 := NewAllocator(v, time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	a2.SetHistoryStore(bs.EgressBindings())
	second, err := a2.AllocateEx(ctx, "env-1", Region{Country: "US"})
	if err != nil {
		t.Fatalf("重启后分配失败: %v", err)
	}
	if second.Proxy.Addr() == first.Proxy.Addr() {
		t.Fatal("用例前提是出口必须变化（厂商不支持取回凭据）")
	}

	events := listHistory(t, bs, store.EgressHistoryFilter{EnvID: "env-1"})
	if len(events) != 3 {
		t.Fatalf("应留下 bound / dropped / rebound 三条事件，实际 %+v", events)
	}
	rebound := mustEvent(t, events, types.EgressActionRebound)
	if rebound.PrevExit != first.Proxy.Addr() || rebound.ExitAddr != second.Proxy.Addr() {
		t.Fatalf("rebound 必须写清从哪变到哪：prev=%q cur=%q", rebound.PrevExit, rebound.ExitAddr)
	}
	if !strings.Contains(rebound.Reason, "取回凭据") {
		t.Fatalf("重新申请的原因要传递下来（否则只剩「重新申请」四个字）：%q", rebound.Reason)
	}
	if dropped := mustEvent(t, events, types.EgressActionDropped); !strings.Contains(dropped.Reason, "取回凭据") {
		t.Fatalf("旧绑定应记为 dropped 并说明原因，实际 %+v", dropped)
	}
}

// TestHistoryRecordsExpiryOnSweep 到期回收是最容易被忽略的一条：
// 行被 DELETE 掉之后，历史是唯一还记得「它曾经用过什么出口」的地方。
func TestHistoryRecordsExpiryOnSweep(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1")
	bs := mem.New()

	a := NewAllocator(v, time.Hour)
	a.SetBindingStore(bs.EgressBindings())
	a.SetHistoryStore(bs.EgressBindings())
	if _, err := a.AllocateEx(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	row, _ := bs.EgressBindings().Get(ctx, "env-1")
	row.ExpiresAt = time.Now().Add(-time.Minute)
	if ok, err := bs.EgressBindings().Bind(ctx, row); err != nil || !ok {
		t.Fatalf("准备到期绑定失败: ok=%v err=%v", ok, err)
	}

	// 另一个进程收拾残局（内存里没有 env-1）。
	a2 := NewAllocator(v, time.Hour)
	a2.SetBindingStore(bs.EgressBindings())
	a2.SetHistoryStore(bs.EgressBindings())
	if n, err := a2.SweepTenant(ctx, "t-1", time.Now()); err != nil || n != 1 {
		t.Fatalf("回收失败: n=%d err=%v", n, err)
	}
	events := listHistory(t, bs, store.EgressHistoryFilter{EnvID: "env-1"})
	if len(events) != 2 {
		t.Fatalf("应新增 expired 事件并保留 bound，实际 %+v", events)
	}
	mustEvent(t, events, types.EgressActionBound)
	expired := mustEvent(t, events, types.EgressActionExpired)
	if expired.ResourceID == "" || expired.ExitAddr == "" {
		t.Fatalf("到期事件也要带资源与出口，否则查不出「谁用过这个出口」：%+v", expired)
	}
}

// TestHistoryFailureDoesNotBlockAllocation 历史写失败只降级为告警。
// 反过来说：如果让它阻塞分配，一次历史库抖动就会让所有环境起不来。
func TestHistoryFailureDoesNotBlockAllocation(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1")
	bs := mem.New()

	a := NewAllocator(v, time.Hour)
	a.SetBindingStore(bs.EgressBindings())
	a.SetHistoryStore(brokenHistory{})
	if _, err := a.AllocateEx(ctx, "env-1", Region{Country: "US"}); err != nil {
		t.Fatalf("历史写失败不应阻塞分配: %v", err)
	}
}

// TestASNSharingDetectsCrossEnvReuse 「同一 ASN 被多个环境用过」是关联线索：
// 判定必须按不同环境计数——一个店自己在同一 ASN 下换绑多次不算共用。
func TestASNSharingDetectsCrossEnvReuse(t *testing.T) {
	ctx := tenantCtx()
	v := newFakeVendor("198.51.100.1", "198.51.100.2", "198.51.100.3")
	bs := mem.New()

	a := NewAllocator(v, time.Hour)
	a.SetBindingStore(bs.EgressBindings())
	a.SetHistoryStore(bs.EgressBindings())
	for _, env := range []string{"env-1", "env-1", "env-2"} {
		if _, err := a.AllocateEx(ctx, env, Region{Country: "US"}); err != nil {
			t.Fatalf("分配 %s 失败: %v", env, err)
		}
		// 同一环境第二次申请会复用，不会新增事件——这正是「换绑」与「共用」的区别。
	}

	shares, err := bs.EgressBindings().ASNSharing(ctx, time.Now().Add(-time.Hour), 2)
	if err != nil {
		t.Fatalf("关联检测失败: %v", err)
	}
	if len(shares) != 1 {
		t.Fatalf("应识别出 1 组共用 ASN，实际 %+v", shares)
	}
	if len(shares[0].EnvIDs) != 2 {
		t.Fatalf("共用 ASN 的环境数应为 2，实际 %v", shares[0].EnvIDs)
	}
	// 只有 1 个环境时不应报（避免把单店漂移误报成关联）
	shares, err = bs.EgressBindings().ASNSharing(ctx, time.Now().Add(-time.Hour), 3)
	if err != nil || len(shares) != 0 {
		t.Fatalf("min_envs=3 时不应有结果: %+v err=%v", shares, err)
	}
	// 时间窗之外不应出现（历史查询必须能按时间收口）
	shares, err = bs.EgressBindings().ASNSharing(ctx, time.Now().Add(time.Hour), 2)
	if err != nil || len(shares) != 0 {
		t.Fatalf("since 在未来时不应有结果: %+v err=%v", shares, err)
	}
}

// TestHistoryRejectsUnknownAction 枚举拼错必须报错：
// 一个拼错的 action 在 mem 里会静默存下来，而 PG 那边会被 CHECK 拒绝——
// 两种驱动给出不同结论，是上一轮 PG 踩过的同一类坑。
func TestHistoryRejectsUnknownAction(t *testing.T) {
	bs := mem.New()
	err := bs.EgressBindings().AppendEvent(tenantCtx(), &types.EgressBindingEvent{
		EnvID: "env-1", Action: "bounded",
	})
	if err == nil {
		t.Fatal("未知动作必须被拒绝（拼错一个字母的事件等于不存在）")
	}
}
