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

// TestProxyUpstreamColumnsPG P3 上游代理接入在真 PG 上的落库与约束。
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestProxyUpstream -v -count=1
//
// 为什么必须跑真库：0013 给 proxies 加的列带 CHECK（type / check_status /
// latency_ms 与 last_checked_at 成对），mem 驱动没有 CHECK——
// 「本地能存、线上报错」这类错配只有真库能抓到。
func TestProxyUpstreamColumnsPG(t *testing.T) {
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
		t.Fatalf("迁移失败（0013 未应用？）: %v", err)
	}
	tid := "t-p3-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
		VALUES($1,$2,$3,'active','pro',now())`, tid, tid, "p3-"+tid); err != nil {
		t.Fatalf("建测试租户失败: %v", err)
	}
	tctx := store.WithTenant(ctx, tid)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = st.Pool().Exec(bg, `DELETE FROM environments WHERE tenant_id=$1`, tid)
		_, _ = st.Pool().Exec(bg, `DELETE FROM proxies WHERE tenant_id=$1`, tid)
		_, _ = st.Pool().Exec(bg, `DELETE FROM tenants WHERE id=$1`, tid)
	})

	now := time.Now().UTC()
	p := &types.Proxy{
		ID: "px-" + tid, TenantID: tid, Name: "美西 ISP-01", Protocol: "http",
		Host: "162.244.210.235", Port: 12323, Username: "isp_user",
		PasswordEnc: "v1.fake-ciphertext.never-real", KeyVersion: 1,
		Country: "US", City: "Los Angeles", Type: types.ProxyTypeISP,
		Status: types.ProxyCheckUnknown, CreatedAt: now, UpdatedAt: now,
	}
	// Status 是启停列（active|disabled），CheckStatus 才是拨测健康。
	p.Status = "active"
	if err := st.Proxies().Create(tctx, p); err != nil {
		t.Fatalf("建代理失败: %v", err)
	}

	// 默认值：新建即 unknown（没拨测过就是没拨测过，不许替运营断言它通）。
	got, err := st.Proxies().Get(tctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CheckStatus != types.ProxyCheckUnknown {
		t.Fatalf("新建代理 check_status 应为 unknown，实际 %q", got.CheckStatus)
	}
	if got.Type != types.ProxyTypeISP || got.City != "Los Angeles" {
		t.Fatalf("type/city 未落库: %q / %q", got.Type, got.City)
	}
	if got.HasPassword != true {
		t.Fatal("has_password 派生列应为 true（密文已写入）")
	}
	if got.LatencyMS != nil || got.LastCheckedAt != nil {
		t.Fatal("未拨测时延迟与时刻必须为空")
	}

	// 拨测成功：出口实测值落库。
	lat := 128
	if err := st.Proxies().SaveCheckResult(tctx, p.ID, store.ProxyCheckResult{
		CheckStatus: types.ProxyCheckOK, ExitIP: "162.244.210.235", ExitCountry: "US",
		LatencyMS: lat, CheckedAt: now,
	}); err != nil {
		t.Fatalf("保存拨测结论失败: %v", err)
	}
	got, _ = st.Proxies().Get(tctx, p.ID)
	if got.CheckStatus != types.ProxyCheckOK || got.ExitIP != "162.244.210.235" ||
		got.ExitCountry != "US" || got.LatencyMS == nil || *got.LatencyMS != lat {
		t.Fatalf("拨测结论未正确落库: %+v", got)
	}
	if got.LastError != "" {
		t.Fatalf("成功时 last_error 应为空，实际 %q", got.LastError)
	}

	// 红线：拨测落库**绝不**碰 password_enc。
	//
	// 这是把 SaveCheckResult 独立成方法（而不是复用整行 Update）的唯一理由：
	// 常规读路径拿到的 PasswordEnc 恒为空，整行写回会把口令抹掉，
	// 表现为「环境每启动一次，上游就多一次 407」。
	if enc, eerr := st.Proxies().PasswordEnc(tctx, p.ID); eerr != nil || enc != p.PasswordEnc {
		t.Fatalf("拨测落库把口令密文改掉了: enc=%q err=%v", enc, eerr)
	}

	// 拨测失败：last_error 落库且状态翻 fail。
	if err := st.Proxies().SaveCheckResult(tctx, p.ID, store.ProxyCheckResult{
		CheckStatus: types.ProxyCheckFail, LatencyMS: 3000, CheckedAt: now,
		LastError: "代理 CONNECT 失败: HTTP/1.1 407",
	}); err != nil {
		t.Fatalf("保存失败结论失败: %v", err)
	}
	got, _ = st.Proxies().Get(tctx, p.ID)
	if got.CheckStatus != types.ProxyCheckFail {
		t.Fatalf("check_status 应翻为 fail，实际 %q", got.CheckStatus)
	}
	if got.LastError == "" {
		t.Fatal("last_error 应落库")
	}
	if got.ExitIP != "" {
		t.Fatalf("失败时应清掉上次的出口 IP，实际 %q", got.ExitIP)
	}

	// 一号一 IP 告警的计数口径：总数 vs 运行中。
	for i, status := range []string{"idle", "idle", "running"} {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO environments(id,tenant_id,profile_id,baseline_hash,
			status,snapshot,consistency,proxy_id,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,'{}','{}',$6,now(),now())`,
			"env-"+tid+"-"+strconv.Itoa(i), tid, "prof-"+tid+"-"+strconv.Itoa(i),
			"hash-"+tid+"-"+strconv.Itoa(i),
			status, p.ID); err != nil {
			t.Fatalf("种环境失败: %v", err)
		}
	}
	if n, err := st.Proxies().CountBound(tctx, p.ID); err != nil || n != 3 {
		t.Fatalf("CountBound 应为 3，实际 %d (err=%v)", n, err)
	}
	if n, err := st.Proxies().CountBoundRunning(tctx, p.ID); err != nil || n != 1 {
		t.Fatalf("CountBoundRunning 应为 1，实际 %d (err=%v)", n, err)
	}

	// CHECK 约束：非法枚举在 DB 侧也拦得住（Go 侧另有白名单）。
	for _, bad := range []string{
		`UPDATE proxies SET type='unknown_type' WHERE id='` + p.ID + `'`,
		`UPDATE proxies SET check_status='maybe' WHERE id='` + p.ID + `'`,
	} {
		if _, err := st.Pool().Exec(ctx, bad); err == nil {
			t.Fatalf("非法枚举应被 CHECK 拒绝: %s", bad)
		}
	}
}

// TestProxyUpstreamCheckPairConstraintPG latency_ms 与 last_checked_at 必须成对。
//
// 半个结果（有延迟没时刻）会让「最近一次是什么时候通的」没有答案——
// 这条 CHECK 就是为了防止某个只写一半的调用混进来。
func TestProxyUpstreamCheckPairConstraintPG(t *testing.T) {
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
	tid := "t-p3b-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
		VALUES($1,$2,$3,'active','pro',now())`, tid, tid, "p3b-"+tid); err != nil {
		t.Fatalf("建测试租户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM proxies WHERE tenant_id=$1`, tid)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE id=$1`, tid)
	})
	id := "px-pair-" + tid
	if _, err := st.Pool().Exec(ctx, `INSERT INTO proxies(id,tenant_id,name,protocol,host,port,status,created_at,updated_at)
		VALUES($1,$2,'成对校验','http','1.2.3.4',8080,'active',now(),now())`, id, tid); err != nil {
		t.Fatalf("种代理失败: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE proxies SET latency_ms=10, last_checked_at=NULL WHERE id=$1`, id); err == nil {
		t.Fatal("latency_ms 有值而 last_checked_at 为空应被 CHECK 拒绝")
	}
}
