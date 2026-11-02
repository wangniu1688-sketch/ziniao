package pg

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"ziniao/platform/migrate"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TestEgressBindingMultiVendorPG 多代理商在真库上的互斥口径：
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestEgressBindingMultiVendorPG -v -count=1
//
// 为什么必须拿真库锁这条：资源 ID 只在各家代理商内部唯一（两家都会签发 "res-1"），
// 唯一索引是 (provider, resource_id) 而不是 (resource_id)。
// 分配器侧也按 (代理商, 资源ID) 判重——两边口径一旦不一致，
// 要么两家共用 ID 时误判冲突（明明能用却报「申请不到出口」），
// 要么放过一个真实的共用出口（两店同 IP，防关联直接破防）。
func TestEgressBindingMultiVendorPG(t *testing.T) {
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

	tid := "t-egress-mv-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	tctx := store.WithTenant(ctx, tid)
	defer func() {
		if _, err := st.Pool().Exec(context.Background(),
			`DELETE FROM egress_bindings WHERE tenant_id=$1`, tid); err != nil {
			t.Logf("清理测试数据失败: %v", err)
		}
	}()
	repo := st.EgressBindings()

	now := time.Now().UTC().Truncate(time.Second)
	mk := func(env, provider, resID, host string, port int, exp time.Time) *types.EgressBinding {
		return &types.EgressBinding{
			EnvID: env, Provider: provider, ResourceID: resID,
			Protocol: "socks5", Host: host, Port: port,
			Country: "US", ASNType: "residential", RegionCode: "US-NY",
			LastExit: host + ":" + strconv.Itoa(port),
			BoundAt:  now, ExpiresAt: exp, UpdatedAt: now,
		}
	}
	future := now.Add(time.Hour)

	// 1) 两家各签一条 res-1：不算冲突（ID 只在各家内部唯一）
	if ok, err := repo.Bind(tctx, mk("env-a", "vendor-a", "res-1", "203.0.113.10", 1080, future)); err != nil || !ok {
		t.Fatalf("vendor-a 绑定失败: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.Bind(tctx, mk("env-b", "vendor-b", "res-1", "203.0.113.20", 1080, future)); err != nil || !ok {
		t.Fatalf("不同代理商的相同资源 ID 不该冲突: ok=%v err=%v", ok, err)
	}
	// 2) 同一家内的相同 ID 仍然冲突
	if ok, err := repo.Bind(tctx, mk("env-c", "vendor-a", "res-1", "203.0.113.30", 1080, future)); err != nil {
		t.Fatalf("冲突绑定不应报错: %v", err)
	} else if ok {
		t.Fatal("(provider,resource_id) 唯一索引失效：同一家内相同资源 ID 被绑定了两次")
	}
	// 3) 跨代理商的相同出口地址允许（不同代理商不会真的持有同一段 IP，
	//    但库侧口径是 (provider, host, port)，分配器必须与它一致）
	if ok, err := repo.Bind(tctx, mk("env-d", "vendor-b", "res-2", "203.0.113.10", 1080, future)); err != nil || !ok {
		t.Fatalf("(provider,host,port) 口径下跨代理商同址应允许: ok=%v err=%v", ok, err)
	}

	// 4) 到期清理要带出 provider：归还必须找当初签发它的那一家
	if ok, err := repo.Bind(tctx, mk("env-exp", "vendor-a", "res-9", "203.0.113.99", 1080, now.Add(-time.Minute))); err != nil || !ok {
		t.Fatalf("写入到期绑定失败: ok=%v err=%v", ok, err)
	}
	doomed, err := repo.DeleteExpired(tctx, tid, now)
	if err != nil {
		t.Fatalf("清理到期绑定失败: %v", err)
	}
	found := false
	for _, b := range doomed {
		if b.EnvID == "env-exp" {
			found = true
			if b.Provider != "vendor-a" {
				t.Fatalf("到期行必须带出 provider 才能按家归还，实际 %q", b.Provider)
			}
		}
	}
	if !found {
		t.Fatalf("到期绑定未被清理: %+v", doomed)
	}
}
