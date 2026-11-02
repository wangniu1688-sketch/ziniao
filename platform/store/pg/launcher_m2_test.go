package pg

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/migrate"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TestLauncherM2IsolationPG M2 启动器内核在真 PG 上的跨租户隔离。
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestLauncherM2IsolationPG -v -count=1
//
// 为什么必须跑真库：0012 给 proxies / groups 建的 RLS 策略只有 PG 会执行，
// mem 驱动的隔离是 Go 代码里的 tenant_id 判断——那是**另一条**防线。
// 两条防线必须分别被验证：只有「谓词对」而 RLS 没生效时，一旦有人用
// superuser 连维护库，「忘了带 tenant_id」就从零行变成全表。
func TestLauncherM2IsolationPG(t *testing.T) {
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
		t.Fatalf("迁移失败（0012 未应用？）: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	tA, tB := "t-m2a-"+suffix, "t-m2b-"+suffix
	for _, tc := range []struct{ id, slug string }{{tA, "m2a-" + suffix}, {tB, "m2b-" + suffix}} {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
			VALUES($1,$2,$3,'active','pro',now())`, tc.id, tc.id, tc.slug); err != nil {
			t.Fatalf("建测试租户失败: %v", err)
		}
	}
	aCtx := store.WithTenant(ctx, tA)
	bCtx := store.WithTenant(ctx, tB)
	t.Cleanup(func() {
		bg := context.Background()
		for _, tid := range []string{tA, tB} {
			_, _ = st.Pool().Exec(bg, `DELETE FROM environments WHERE tenant_id=$1`, tid)
			_, _ = st.Pool().Exec(bg, `DELETE FROM shops WHERE tenant_id=$1`, tid)
			_, _ = st.Pool().Exec(bg, `DELETE FROM groups WHERE tenant_id=$1`, tid)
			_, _ = st.Pool().Exec(bg, `DELETE FROM proxies WHERE tenant_id=$1`, tid)
			_, _ = st.Pool().Exec(bg, `DELETE FROM tenants WHERE id=$1`, tid)
		}
	})

	// ---------- 代理台账：B 看不到 A 的代理，也拿不到它的密文 ----------
	now := time.Now().UTC()
	proxy := &types.Proxy{
		ID: "px-" + suffix, TenantID: tA, Name: "A的代理", Protocol: "http",
		Host: "10.0.0.1", Port: 8080, Username: "u", PasswordEnc: "cipher:xxxx",
		KeyVersion: 1, Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Proxies().Create(aCtx, proxy); err != nil {
		t.Fatalf("建代理失败: %v", err)
	}
	if list, total, err := st.Proxies().List(bCtx, store.ProxyFilter{Limit: 50}); err != nil {
		t.Fatalf("B 查代理失败: %v", err)
	} else if total != 0 || len(list) != 0 {
		t.Fatalf("B 不应看到 A 的代理: total=%d list=%d", total, len(list))
	}
	if _, err := st.Proxies().Get(bCtx, proxy.ID); err == nil {
		t.Fatalf("B 直读 A 的代理应 not found")
	} else if !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("B 直读 A 的代理应 not_found，实际 %v", err)
	}
	// 密文同样拿不到：这是「凭据只进不出」在存储层的最后一道
	if enc, err := st.Proxies().PasswordEnc(bCtx, proxy.ID); err == nil {
		t.Fatalf("B 不应读到 A 的代理密文: %q", enc)
	}

	// ---------- 分组：B 看不到 A 的分组 ----------
	g := &types.Group{ID: "gp-" + suffix, TenantID: tA, Name: "A的分组", Sort: 1, CreatedAt: now, UpdatedAt: now}
	if err := st.Groups().Create(aCtx, g); err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	list, err := st.Groups().List(bCtx)
	if err != nil {
		t.Fatalf("B 查分组失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("B 不应看到 A 的分组: %d 条", len(list))
	}
	if _, err := st.Groups().Get(bCtx, g.ID); err == nil {
		t.Fatalf("B 直读 A 的分组应 not found")
	}

	// ---------- 环境聚合：B 搜不到 A 的环境 ----------
	shop := &types.Shop{
		ID: "sp-" + suffix, TenantID: tA, Name: "A的店铺", Platform: "Amazon",
		Status: "注册中", GroupID: g.ID, OwnerID: "u-" + suffix, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Shops().Create(aCtx, shop); err != nil {
		t.Fatalf("建店铺失败: %v", err)
	}
	env := &types.Environment{
		ID: "env-" + suffix, TenantID: tA, ShopID: shop.ID, ProfileID: "prof-1",
		BaselineHash: "bl-" + suffix, KernelMajor: 131, Status: "created",
		Snapshot: map[string]string{}, Consistency: []byte("{}"),
		ProxyID: proxy.ID, Country: "US", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Environments().Create(aCtx, env); err != nil {
		t.Fatalf("建环境失败: %v", err)
	}
	if _, total, err := st.Environments().Search(bCtx, store.EnvironmentFilter{Limit: 50}); err != nil {
		t.Fatalf("B 搜环境失败: %v", err)
	} else if total != 0 {
		t.Fatalf("B 不应搜到 A 的环境: total=%d", total)
	}
	if _, err := st.Environments().Get(bCtx, env.ID); err == nil {
		t.Fatalf("B 直读 A 的环境应 not found")
	}
	// 引用计数也隔离：B 视角下 A 的代理引用数为 0（否则「被引用不能删」的判据会误伤）
	if n, err := st.Proxies().CountBound(bCtx, proxy.ID); err != nil || n != 0 {
		t.Fatalf("B 视角引用数应为 0，实际 %d err=%v", n, err)
	}

	// ---------- 过滤条件下推：A 自己按分组 / 平台 / 关键词都能命中 ----------
	cases := []struct {
		name string
		f    store.EnvironmentFilter
		want int
	}{
		{"按分组", store.EnvironmentFilter{GroupID: g.ID, Limit: 50}, 1},
		{"按平台", store.EnvironmentFilter{Platform: "Amazon", Limit: 50}, 1},
		{"按国家", store.EnvironmentFilter{Country: "US", Limit: 50}, 1},
		{"按代理", store.EnvironmentFilter{ProxyID: proxy.ID, Limit: 50}, 1},
		{"按状态", store.EnvironmentFilter{Status: "created", Limit: 50}, 1},
		{"按店铺白名单", store.EnvironmentFilter{ShopIDs: []string{shop.ID}, Limit: 50}, 1},
		{"白名单不含自己", store.EnvironmentFilter{ShopIDs: []string{"ghost"}, Limit: 50}, 0},
		{"关键词命中店名", store.EnvironmentFilter{Keyword: "A的店铺", Limit: 50}, 1},
		{"关键词不命中", store.EnvironmentFilter{Keyword: "不存在的店", Limit: 50}, 0},
	}
	for _, tc := range cases {
		_, total, err := st.Environments().Search(aCtx, tc.f)
		if err != nil {
			t.Fatalf("%s 过滤失败: %v", tc.name, err)
		}
		if total != tc.want {
			t.Fatalf("%s 应返回 %d 条，实际 %d", tc.name, tc.want, total)
		}
	}

	// ---------- 软删店铺的环境不出现在聚合列表（幽灵行收口） ----------
	// 直接置 deleted_at：软删是「写 deleted_at」，仓储没有单独的 SoftDelete 方法，
	// 这里直接用 SQL 是为了让断言只落在 Search 的过滤口径上。
	if _, err := st.Pool().Exec(aCtx, `UPDATE shops SET deleted_at=now() WHERE id=$1 AND tenant_id=$2`,
		shop.ID, tA); err != nil {
		t.Fatalf("软删店铺失败: %v", err)
	}
	if _, total, err := st.Environments().Search(aCtx, store.EnvironmentFilter{Limit: 50}); err != nil {
		t.Fatalf("软删后搜索失败: %v", err)
	} else if total != 0 {
		t.Fatalf("已软删店铺的环境不应出现在聚合列表: total=%d", total)
	}
}

// TestLauncherM2GroupRenameSyncPG 分组改名后成员店铺的 group_name 必须同步。
//
// 这条断言的是**仓储过滤**是否真的能按 group_id 取到成员（服务层同步依赖它）：
// ShopFilter.GroupID 是 0012 之后新加的谓词，真库上没跑过。
func TestLauncherM2GroupRenameSyncPG(t *testing.T) {
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
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	tid := "t-m2g-" + suffix
	if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
		VALUES($1,$2,$3,'active','pro',now())`, tid, tid, "m2g-"+suffix); err != nil {
		t.Fatalf("建测试租户失败: %v", err)
	}
	tctx := store.WithTenant(ctx, tid)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = st.Pool().Exec(bg, `DELETE FROM shops WHERE tenant_id=$1`, tid)
		_, _ = st.Pool().Exec(bg, `DELETE FROM groups WHERE tenant_id=$1`, tid)
		_, _ = st.Pool().Exec(bg, `DELETE FROM tenants WHERE id=$1`, tid)
	})

	now := time.Now().UTC()
	g := &types.Group{ID: "gp-" + suffix, TenantID: tid, Name: "改名前的组", Sort: 1, CreatedAt: now, UpdatedAt: now}
	if err := st.Groups().Create(tctx, g); err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	shop := &types.Shop{
		ID: "sp-" + suffix, TenantID: tid, Name: "待同步店铺", Platform: "Amazon",
		Status: "注册中", GroupID: g.ID, Group: g.Name, OwnerID: "u-" + suffix,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Shops().Create(tctx, shop); err != nil {
		t.Fatalf("建店铺失败: %v", err)
	}
	members, _, err := st.Shops().List(tctx, store.ShopFilter{GroupID: g.ID, Limit: 50})
	if err != nil {
		t.Fatalf("按分组查成员失败: %v", err)
	}
	if len(members) != 1 || members[0].ID != shop.ID {
		t.Fatalf("按分组应命中 1 个成员，实际 %d", len(members))
	}
	// 模拟服务层同步：改名后逐条写回 group_name
	g.Name = "改名后的组"
	if err := st.Groups().Update(tctx, g); err != nil {
		t.Fatalf("改分组失败: %v", err)
	}
	for i := range members {
		members[i].Group = g.Name
		members[i].UpdatedAt = time.Now().UTC()
		if err := st.Shops().Update(tctx, &members[i]); err != nil {
			t.Fatalf("同步店铺分组名失败: %v", err)
		}
	}
	got, err := st.Shops().Get(tctx, shop.ID)
	if err != nil {
		t.Fatalf("回读店铺失败: %v", err)
	}
	if got.Group != "改名后的组" || got.GroupID != g.ID {
		t.Fatalf("改名未同步: group=%q group_id=%q", got.Group, got.GroupID)
	}
}
