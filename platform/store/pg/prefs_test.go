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

// TestNotificationPrefsPG 通知偏好的真库验证（默认跳过）：
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestNotificationPrefsPG -v -count=1
//
// 为什么非要跑真库：
//
//	① jsonb CHECK (jsonb_typeof='array') 只有 PG 会拒绝（mem 存什么都行）；
//	② nil 切片写进 jsonb NOT NULL 列的问题与 text[] 完全同源，mem 发现不了；
//	③ UPSERT 与租户隔离（同一个人换团队的偏好必须互不相见）是纯 SQL 语义。
func TestNotificationPrefsPG(t *testing.T) {
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
		t.Fatalf("迁移失败（0008 是否能应用？）: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	t1, t2 := "tp1-"+suffix, "tp2-"+suffix
	for _, tc := range []struct{ id, slug string }{{t1, "p1-" + suffix}, {t2, "p2-" + suffix}} {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
			VALUES($1,$2,$3,'active','pro',now())`, tc.id, tc.id, tc.slug); err != nil {
			t.Fatalf("建测试租户失败: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM notification_prefs WHERE tenant_id=$1 OR tenant_id=$2`, t1, t2)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE id=$1 OR id=$2`, t1, t2)
	})

	repo := st.Notifications()
	c1 := store.WithTenant(ctx, t1)
	c2 := store.WithTenant(ctx, t2)
	const uid = "u-prefs"
	mk := func(tenantID string, chs []string) *types.NotificationPrefs {
		return &types.NotificationPrefs{TenantID: tenantID, UserID: uid, Channels: chs}
	}

	// 1) 未设置 = nil（上层据此走默认，不能当成 error）
	if p, err := repo.GetPrefs(c1, t1, uid); err != nil || p != nil {
		t.Fatalf("未设置应返回 (nil, nil)，实际 %v %v", p, err)
	}

	// 2) 写入 + 回读：nil 切片必须落成 []，而不是 SQL NULL
	if err := repo.SetPrefs(c1, mk(t1, nil)); err != nil {
		t.Fatalf("写入偏好失败（nil 切片被写成 SQL NULL？）: %v", err)
	}
	p, err := repo.GetPrefs(c1, t1, uid)
	if err != nil || p == nil {
		t.Fatalf("回读失败: %v %v", p, err)
	}
	if p.Channels == nil || len(p.Channels) != 0 {
		t.Fatalf("空偏好应落成空数组而非 NULL: %#v", p.Channels)
	}

	// 3) UPSERT 覆盖（不是主键冲突报错，也不是叠加）
	if err := repo.SetPrefs(c1, mk(t1, []string{"email"})); err != nil {
		t.Fatalf("覆盖偏好失败: %v", err)
	}
	p, _ = repo.GetPrefs(c1, t1, uid)
	if len(p.Channels) != 1 || p.Channels[0] != "email" {
		t.Fatalf("覆盖后应为 [email]，实际 %v", p.Channels)
	}

	// 4) 同一个人在另一个团队的偏好互不影响
	if err := repo.SetPrefs(c2, mk(t2, []string{})); err != nil {
		t.Fatalf("写入第二个团队失败: %v", err)
	}
	if p2, _ := repo.GetPrefs(c2, t2, uid); len(p2.Channels) != 0 {
		t.Fatalf("第二个团队应是独立的空偏好，实际 %v", p2.Channels)
	}
	if p1, _ := repo.GetPrefs(c1, t1, uid); len(p1.Channels) != 1 {
		t.Fatalf("写第二个团队不应改动第一个团队: %v", p1.Channels)
	}

	// 5) 跨租户读不到（RLS fail-closed）：拿 t2 的上下文去读 t1 的偏好
	if got, err := repo.GetPrefs(c2, t1, uid); err != nil || got != nil {
		t.Fatalf("跨租户读取偏好应视为未设置（RLS 挡住），实际 %v %v", got, err)
	}
}
