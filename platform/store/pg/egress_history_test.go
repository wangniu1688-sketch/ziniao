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

// TestEgressHistoryPG 出口历史留档在真库上的行为（迁移 0009）。
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestEgressHistoryPG -v -count=1
//
// 为什么必须拿真库锁：
//   - GROUP BY + HAVING COUNT(DISTINCT env_id) 与 array_agg 的扫描类型（text[]）
//     在 mem 里怎么写都能过，真库上一个类型不匹配就是 500；
//   - RLS 双租户可见性、CHECK 约束这些「只在库里存在」的护栏，mem 驱动根本没有。
func TestEgressHistoryPG(t *testing.T) {
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
		t.Fatalf("迁移失败（0009 未应用？）: %v", err)
	}

	tid := "t-egress-hist-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	other := tid + "-other"
	tctx := store.WithTenant(ctx, tid)
	defer func() {
		// 清理放在带租户上下文的事务里做：万一以后给这张表加了 FORCE RLS，
		// 裸 DELETE 会「成功但零行」——静默残留，下一次跑用例时 id 冲突，
		// ON CONFLICT DO NOTHING 把全部插入吞掉（本轮已经踩过一次）。
		tx, err := st.Pool().Begin(context.Background())
		if err != nil {
			t.Logf("清理测试数据失败: %v", err)
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(context.Background(),
			`SELECT set_config('app.tenant_id', $1, true)`, tid); err != nil {
			t.Logf("清理测试数据失败: %v", err)
			return
		}
		if _, err := tx.Exec(context.Background(),
			`DELETE FROM egress_binding_history WHERE tenant_id = ANY($1)`, []string{tid, other}); err != nil {
			t.Logf("清理测试数据失败: %v", err)
			return
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Logf("清理测试数据失败: %v", err)
		}
	}()
	repo := st.EgressBindings()
	now := time.Now().UTC().Truncate(time.Second)

	ev := func(id, env, action, asn string, port int, at time.Time) *types.EgressBindingEvent {
		return &types.EgressBindingEvent{
			// id 带租户前缀：主键是全局的，写死 "h1" 会让上一轮残留行
			// 把这一轮的插入全部 ON CONFLICT DO NOTHING 吞掉（假绿/假红都难查）。
			ID: tid + "-" + id, EnvID: env, Action: action, Provider: "vendor-a", ResourceID: "res-" + env,
			Protocol: "socks5", Host: "203.0.113.10", Port: port,
			Country: "US", ASN: asn, ASNType: "residential",
			ExitAddr: "203.0.113.10:" + strconv.Itoa(port), Reason: "测试", OccurredAt: at,
		}
	}

	// 1) 两个环境落在同一 ASN 上（时间错开），env-a 已释放：历史仍要查得到。
	rows := []*types.EgressBindingEvent{
		ev("h1", "env-a", types.EgressActionBound, "AS64500", 1080, now.Add(-2*time.Hour)),
		ev("h2", "env-a", types.EgressActionReleased, "AS64500", 1080, now.Add(-90*time.Minute)),
		ev("h3", "env-b", types.EgressActionBound, "AS64500", 1081, now.Add(-time.Hour)),
		// 同一个环境重复绑定同一 ASN：不算共用（必须按不同环境计数）
		ev("h4", "env-b", types.EgressActionRebound, "AS64500", 1082, now.Add(-30*time.Minute)),
		// 没有 ASN 的事件不参与关联检测
		ev("h5", "env-c", types.EgressActionBound, "", 1083, now.Add(-20*time.Minute)),
	}
	for _, r := range rows {
		if err := repo.AppendEvent(tctx, r); err != nil {
			t.Fatalf("写入历史失败: %v", err)
		}
	}
	// 2) 幂等：同一 id 重投不应产生第二行（否则统计虚高）
	if err := repo.AppendEvent(tctx, ev("h1", "env-a", types.EgressActionBound, "AS64500", 1080, now.Add(-2*time.Hour))); err != nil {
		t.Fatalf("重投应被幂等吞掉: %v", err)
	}
	list, total, err := repo.ListEvents(tctx, store.EgressHistoryFilter{EnvID: "env-a"})
	if err != nil {
		t.Fatalf("查历史失败: %v", err)
	}
	if total != 2 || len(list) != 2 {
		t.Fatalf("env-a 应有 2 条历史（重投被吞），实际 total=%d len=%d", total, len(list))
	}
	if list[0].Action != types.EgressActionReleased {
		t.Fatalf("历史应按时间倒序，首条应为 released，实际 %+v", list[0])
	}
	if list[0].TenantID != tid {
		t.Fatalf("事件必须归属租户，实际 %q", list[0].TenantID)
	}
	if list[1].ExitAddr != "203.0.113.10:1080" || list[1].ASN != "AS64500" {
		t.Fatalf("出口与 ASN 必须完整落库: %+v", list[1])
	}

	// 3) 关联检测：env-a + env-b 共用 AS64500；env-b 自己换绑两次不算
	shares, err := repo.ASNSharing(tctx, now.Add(-24*time.Hour), 2)
	if err != nil {
		t.Fatalf("关联检测失败（text[] 扫描口径？）: %v", err)
	}
	if len(shares) != 1 || shares[0].ASN != "AS64500" {
		t.Fatalf("应只有 AS64500 这一组，实际 %+v", shares)
	}
	if len(shares[0].EnvIDs) != 2 || len(shares[0].Exits) != 3 {
		t.Fatalf("环境数应为 2（去重）、出口数应为 3，实际 %+v", shares[0])
	}
	if shares[0].Events != 4 {
		t.Fatalf("事件数应为 4（h1..h4，h5 无 ASN 不算），实际 %d", shares[0].Events)
	}
	// 时间窗收口：只看最近 40 分钟时 env-a 不在窗内，不该再报「共用」
	shares, err = repo.ASNSharing(tctx, now.Add(-40*time.Minute), 2)
	if err != nil {
		t.Fatalf("关联检测失败: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("时间窗外的记录不应参与关联检测: %+v", shares)
	}

	// 4) action 是枚举：Go 侧先拦，DB 的 CHECK 也必须独立成立（防有人绕过服务直连库）
	if err := repo.AppendEvent(tctx, ev("h6", "env-a", "bounded", "AS64500", 1080, now)); err == nil {
		t.Fatal("未知 action 必须被拒绝")
	}
	if _, err := st.Pool().Exec(ctx, `INSERT INTO egress_binding_history(id,tenant_id,env_id,action)
		VALUES($1,$2,$3,'bounded')`, tid+"-h7", tid, "env-a"); err == nil {
		t.Fatal("DB 的 action CHECK 约束缺失：绕过服务就能写进脏数据")
	}

	// 5) 租户隔离：换一个租户查不到别人的历史（RLS + 显式 tenant_id 双保险）
	otherList, otherTotal, err := repo.ListEvents(store.WithTenant(ctx, other), store.EgressHistoryFilter{})
	if err != nil {
		t.Fatalf("跨租户查询失败: %v", err)
	}
	if otherTotal != 0 || len(otherList) != 0 {
		t.Fatalf("跨租户不应读到历史: total=%d %+v", otherTotal, otherList)
	}
}
