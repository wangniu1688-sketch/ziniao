package pg

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"ziniao/network"
	"ziniao/platform/migrate"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TestEgressBindingPG 出口绑定的真库验证（默认跳过，只在给了 DSN 时跑）：
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestEgressBindingPG -v -count=1
//
// 为什么必须有一条真库用例：这两条互斥约束是**数据库**保证的，内存实现对不上
// 也只是「单测全绿、上线两店共 IP」——这种缺陷只有把 SQL 真的发出去才看得见。
func TestEgressBindingPG(t *testing.T) {
	dsn := os.Getenv("ZINIAO_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 ZINIAO_PG_DSN，跳过 PG 实跑用例")
	}
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := migrate.Up(ctx, st.Pool()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	tid := "t-egress-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	tctx := store.WithTenant(ctx, tid)
	defer func() {
		if _, err := st.Pool().Exec(context.Background(),
			`DELETE FROM egress_bindings WHERE tenant_id=$1`, tid); err != nil {
			t.Logf("清理测试数据失败: %v", err)
		}
	}()
	repo := st.EgressBindings()

	now := time.Now().UTC().Truncate(time.Second)
	mk := func(env, resID, host string, port int, exp time.Time) *types.EgressBinding {
		return &types.EgressBinding{
			EnvID: env, Provider: "vendor-pg", ResourceID: resID,
			Protocol: "socks5", Host: host, Port: port,
			Country: "US", ASNType: "residential", RegionCode: "US-NY",
			LastExit: host + ":" + strconv.Itoa(port),
			BoundAt:  now, ExpiresAt: exp, UpdatedAt: now,
		}
	}

	// 1) 首次绑定落库，读回字段完整
	if ok, err := repo.Bind(tctx, mk("env-a", "res-1", "203.0.113.10", 1080, now.Add(time.Hour))); err != nil || !ok {
		t.Fatalf("env-a 绑定失败: ok=%v err=%v", ok, err)
	}
	got, err := repo.Get(tctx, "env-a")
	if err != nil || got == nil {
		t.Fatalf("读回绑定失败: row=%v err=%v", got, err)
	}
	if got.ResourceID != "res-1" || got.TenantID != tid || got.LastExit != "203.0.113.10:1080" {
		t.Fatalf("读回内容不符: %+v", got)
	}

	// 2) 同一资源被另一个环境抢：必须返回 false，且不落库
	if ok, err := repo.Bind(tctx, mk("env-b", "res-1", "203.0.113.10", 1080, now.Add(time.Hour))); err != nil {
		t.Fatalf("冲突绑定不应报错: %v", err)
	} else if ok {
		t.Fatal("(provider,resource_id) 唯一索引失效：同一资源被两个环境绑定")
	}
	if row, _ := repo.Get(tctx, "env-b"); row != nil {
		t.Fatalf("冲突时不得留下半条绑定: %+v", row)
	}

	// 3) 同一出口地址、不同订单号：平台看到的是同一个 IP，必须同样被挡住
	if ok, err := repo.Bind(tctx, mk("env-c", "res-2", "203.0.113.10", 1080, now.Add(time.Hour))); err != nil {
		t.Fatalf("地址冲突绑定不应报错: %v", err)
	} else if ok {
		t.Fatal("(provider,host,port) 唯一索引失效：两个环境共用同一出口地址")
	}

	// 4) 同一环境换资源：旧行被替换，环境始终只有一条绑定
	if ok, err := repo.Bind(tctx, mk("env-a", "res-3", "203.0.113.11", 1081, now.Add(time.Hour))); err != nil || !ok {
		t.Fatalf("env-a 换绑失败: ok=%v err=%v", ok, err)
	}
	rows, err := repo.ListByTenant(tctx)
	if err != nil {
		t.Fatalf("列出绑定失败: %v", err)
	}
	if len(rows) != 1 || rows[0].ResourceID != "res-3" {
		t.Fatalf("一个环境只应有一条绑定，实际 %+v", rows)
	}

	// 5) 到期回收：只删到期行，未到期的一行不少
	if ok, err := repo.Bind(tctx, mk("env-d", "res-4", "203.0.113.12", 1082, now.Add(-time.Minute))); err != nil || !ok {
		t.Fatalf("env-d 绑定失败: ok=%v err=%v", ok, err)
	}
	doomed, err := repo.DeleteExpired(tctx, tid, now)
	if err != nil {
		t.Fatalf("到期回收失败: %v", err)
	}
	if len(doomed) != 1 || doomed[0].EnvID != "env-d" || doomed[0].ResourceID != "res-4" {
		t.Fatalf("应精确回收 env-d/res-4，实际 %+v", doomed)
	}
	if left, _ := repo.Get(tctx, "env-a"); left == nil {
		t.Fatal("未到期的绑定被误删")
	}

	// 6) 解除绑定是幂等的
	if err := repo.Release(tctx, "env-a"); err != nil {
		t.Fatalf("解除绑定失败: %v", err)
	}
	if err := repo.Release(tctx, "env-a"); err != nil {
		t.Fatalf("重复解除不应报错: %v", err)
	}
	if row, _ := repo.Get(tctx, "env-a"); row != nil {
		t.Fatalf("解除后仍能读到绑定: %+v", row)
	}
}

// pgVendor 支持 Attach 的假代理商：用来在真库上验证「重启复用同一出口」。
type pgVendor struct {
	mu       sync.Mutex
	seq      int
	acquires int
	issued   map[string]*network.AcquiredProxy
}

func (p *pgVendor) Name() string { return "vendor-pg" }
func (p *pgVendor) ListRegions(context.Context) ([]network.Region, error) {
	return []network.Region{{Code: "US", Country: "US"}}, nil
}
func (p *pgVendor) Health(context.Context) error { return nil }
func (p *pgVendor) Acquire(_ context.Context, r network.Region) (*network.AcquiredProxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	p.acquires++
	id := "res-e2e-" + strconv.Itoa(p.seq)
	res := &network.AcquiredProxy{
		ID: id,
		Proxy: network.ProxyConfig{
			ID: id, Protocol: network.ProtoSOCKS5, Host: "203.0.113.50", Port: 1080 + p.seq,
			Username: "u", Password: "p", Country: r.Country, ASNType: network.ASNResidential,
		},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	p.issued[id] = res
	return res, nil
}
func (p *pgVendor) Release(_ context.Context, _ string) error { return nil }
func (p *pgVendor) Attach(_ context.Context, id string) (*network.AcquiredProxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.issued[id]
	if !ok {
		return nil, errors.New("资源不存在")
	}
	cp := *res
	return &cp, nil
}

// TestAllocatorRestartOnPG 重启不换出口的端到端验证（真库 + 真分配器）。
// 单测里 mem 与 pg 各自都能过，但「重启后真的取回同一条绑定」只有真库能证。
func TestAllocatorRestartOnPG(t *testing.T) {
	dsn := os.Getenv("ZINIAO_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 ZINIAO_PG_DSN，跳过 PG 实跑用例")
	}
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := migrate.Up(ctx, st.Pool()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	tid := "t-egress-e2e-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	tctx := store.WithTenant(ctx, tid)
	defer func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM egress_bindings WHERE tenant_id=$1`, tid)
	}()

	v := &pgVendor{issued: map[string]*network.AcquiredProxy{}}
	a1 := network.NewAllocator(v, time.Hour)
	a1.SetBindingStore(st.EgressBindings())
	first, err := a1.AllocateEx(tctx, "env-e2e", network.Region{Country: "US"})
	if err != nil {
		t.Fatalf("首次分配失败: %v", err)
	}

	a2 := network.NewAllocator(v, time.Hour) // 模拟进程重启：内存全空，只剩库
	a2.SetBindingStore(st.EgressBindings())
	again, err := a2.AllocateEx(tctx, "env-e2e", network.Region{Country: "US"})
	if err != nil {
		t.Fatalf("重启后分配失败: %v", err)
	}
	if again.Proxy.Addr() != first.Proxy.Addr() {
		t.Fatalf("重启后出口变了：%s -> %s", first.Proxy.Addr(), again.Proxy.Addr())
	}
	if again.Rotated {
		t.Fatalf("复用同一出口不该告警: prev=%s", again.PrevAddr)
	}
	if v.acquires != 1 {
		t.Fatalf("重启后应复用既有资源，实际申请了 %d 次", v.acquires)
	}
}
