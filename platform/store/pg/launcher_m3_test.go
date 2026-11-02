package pg

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"ziniao/platform/migrate"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// M3 启动链路在真 PG 上的状态机用例。
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestLauncher -v -count=1
//
// 为什么必须在真库上跑：M3 阶段 8 的事故是 **PG 特有的** ——整行 UPDATE 的
// 占位符（18）与实参（17）对不上，PG 只回一句 "mismatched param and argument count"，
// 而 mem 驱动根本没有占位符这回事。也就是说：mem 全绿 ≠ pg 可用。
// 状态机三条路径（启动置 running / 失败复位 idle / 重复启动幂等）必须在
// 真实占位符语义下各跑一遍。

// newLauncherPGStore 连真库并建好一个隔离租户（清理动作注册在 t.Cleanup）。
func newLauncherPGStore(t *testing.T) (*Store, context.Context, string, func()) {
	t.Helper()
	dsn := os.Getenv("ZINIAO_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 ZINIAO_PG_DSN，跳过 PG 实跑用例")
	}
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	if _, err := migrate.Up(ctx, st.Pool()); err != nil {
		_ = st.Close()
		t.Fatalf("迁移失败（0004 inject_script 列未应用？）: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	tid := "t-m3-" + suffix
	if _, err := st.Pool().Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,created_at)
		VALUES($1,$2,$3,'active','pro',now())`, tid, tid, "m3-"+suffix); err != nil {
		_ = st.Close()
		t.Fatalf("建测试租户失败: %v", err)
	}
	cleanup := func() {
		bg := context.Background()
		_, _ = st.Pool().Exec(bg, `DELETE FROM environments WHERE tenant_id=$1`, tid)
		_, _ = st.Pool().Exec(bg, `DELETE FROM shops WHERE tenant_id=$1`, tid)
		_, _ = st.Pool().Exec(bg, `DELETE FROM tenants WHERE id=$1`, tid)
		_ = st.Close()
	}
	return st, store.WithTenant(ctx, tid), tid, cleanup
}

// newLauncherPGEnv 建一个带注入脚本的环境（脚本非空是断言的关键：
// 任何一次「写回」把它清空，指纹就丢了，而窗口照开、排查时看不见）。
func newLauncherPGEnv(t *testing.T, st *Store, ctx context.Context, tid, suffix string) *types.Environment {
	t.Helper()
	now := time.Now().UTC()
	shop := &types.Shop{
		ID: "sp-" + suffix, TenantID: tid, Name: "M3店铺", Platform: "Amazon",
		Status: "注册中", OwnerID: "u-" + suffix, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Shops().Create(ctx, shop); err != nil {
		t.Fatalf("建店铺失败: %v", err)
	}
	env := &types.Environment{
		ID: "env-" + suffix, TenantID: tid, ShopID: shop.ID, ProfileID: "prof-m3",
		BaselineHash: "bl-" + suffix, KernelMajor: 131, Status: "created",
		Snapshot: map[string]string{"platform": "Win32"}, Consistency: []byte("{}"),
		Country: "US", CreatedAt: now, UpdatedAt: now,
		InjectScript: "/*fp*/navigator.webdriver=undefined;",
	}
	if err := st.Environments().Create(ctx, env); err != nil {
		t.Fatalf("建环境失败: %v", err)
	}
	return env
}

// TestLauncherEnvStatusFlowPG 三条启动路径在真库上的状态机：
//
//  1. 启动成功 → status=running，且 inject_script 不被写空；
//  2. 启动失败 → 复位回 idle（不滞留），且 running 绝不被复位覆盖；
//  3. 重复启动 → 幂等（状态与 last_started_at 稳定，脚本不丢）。
func TestLauncherEnvStatusFlowPG(t *testing.T) {
	st, ctx, tid, cleanup := newLauncherPGStore(t)
	defer cleanup()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	env := newLauncherPGEnv(t, st, ctx, tid, suffix)

	const script = "/*fp*/navigator.webdriver=undefined;"
	repos := st.Environments()

	// ---------- 路径 1：启动成功置 running（阶段 8 的主提交）----------
	//
	// 修复前这一步就是 "mismatched param and argument count"：
	// envUpdateSQL 有 18 个占位符，实参只传了 17 个（inject_script 加了列名没加实参）。
	startedAt := time.Now().UTC().Truncate(time.Millisecond)
	cur, err := repos.Get(ctx, env.ID)
	if err != nil {
		t.Fatalf("读环境失败: %v", err)
	}
	cur.Status = "running"
	cur.LastStartedAt = &startedAt
	if err := repos.Update(ctx, cur); err != nil {
		t.Fatalf("路径1 提交 running 失败（阶段8 主提交）: %v", err)
	}
	got, err := repos.Get(ctx, env.ID)
	if err != nil {
		t.Fatalf("路径1 回读失败: %v", err)
	}
	if got.Status != "running" {
		t.Fatalf("路径1 应为 running，实际 %s", got.Status)
	}
	if got.LastStartedAt == nil {
		t.Fatalf("路径1 last_started_at 未落库")
	}
	if got.InjectScript != script {
		t.Fatalf("路径1 inject_script 被写坏: %q（应原样保留）", got.InjectScript)
	}

	// ---------- 路径 2a：铁律——running 绝不被失败复位覆盖 ----------
	//
	// 主提交超时但环境其实已经跑起来的情况下，把它改回 idle 会留下
	// 「库里说没跑、窗口却开着」的相反悬空态，比原样留着更糟。
	changed, err := repos.SetStatusIfIn(ctx, env.ID, []string{"created", "idle"}, "idle")
	if err != nil {
		t.Fatalf("路径2a 复位调用失败: %v", err)
	}
	if changed {
		t.Fatalf("路径2a 不应复位 running（changed=true 说明覆盖了运行中的环境）")
	}
	if got, _ = repos.Get(ctx, env.ID); got.Status != "running" {
		t.Fatalf("路径2a running 被覆盖成 %s", got.Status)
	}

	// ---------- 路径 2b：启动失败复位回 idle（不滞留）----------
	//
	// 关键：复位走的是**独立的一条只写 status 的 UPDATE**。主提交那条 SQL
	// 再出问题，复位也必须照样成功——否则环境就悬在中间态没人收。
	cur, _ = repos.Get(ctx, env.ID)
	cur.Status = "created" // 回到「未启动」再走一次启动失败
	if err := repos.Update(ctx, cur); err != nil {
		t.Fatalf("路径2b 置回 created 失败: %v", err)
	}
	changed, err = repos.SetStatusIfIn(ctx, env.ID, []string{"created", "idle"}, "idle")
	if err != nil {
		t.Fatalf("路径2b 失败复位失败: %v", err)
	}
	if !changed {
		t.Fatalf("路径2b 复位未命中（created 应被复位为 idle）")
	}
	if got, _ = repos.Get(ctx, env.ID); got.Status != "idle" {
		t.Fatalf("路径2b 应复位为 idle，实际 %s（状态滞留）", got.Status)
	}
	if got.InjectScript != script {
		t.Fatalf("路径2b inject_script 被写坏: %q", got.InjectScript)
	}

	// ---------- 路径 3：重复启动幂等 ----------
	//
	// 对账重挂 / 运营连点两次不该把一次成功启动记成两次：
	// last_started_at 保持第一次的时刻，脚本与店铺不变。
	first := startedAt
	for i := 0; i < 2; i++ {
		cur, _ = repos.Get(ctx, env.ID)
		cur.Status = "running"
		if cur.LastStartedAt == nil {
			cur.LastStartedAt = &first
		}
		if err := repos.Update(ctx, cur); err != nil {
			t.Fatalf("路径3 第 %d 次提交失败: %v", i+1, err)
		}
	}
	got, _ = repos.Get(ctx, env.ID)
	if got.Status != "running" {
		t.Fatalf("路径3 重复提交后应为 running，实际 %s", got.Status)
	}
	if got.LastStartedAt == nil || !got.LastStartedAt.Equal(first) {
		t.Fatalf("路径3 last_started_at 被改写: %v（应恒为 %v）", got.LastStartedAt, first)
	}
	if got.InjectScript != script {
		t.Fatalf("路径3 inject_script 被写坏: %q", got.InjectScript)
	}
	if got.ShopID != env.ShopID {
		t.Fatalf("路径3 shop_id 被写坏: %q", got.ShopID)
	}

	// ---------- 故障注入 + 对账兜底：主提交 SQL 坏了，复位也必须照样成功 ----------
	//
	// 这一段复现的就是 M3 阶段 8 的事故形态：主提交那条整行 UPDATE 参数对不上
	// （这里用少一个实参的调用模拟），**同时**失败复位也被它拖垮，环境悬在 running。
	//
	// 修复后两条语句物理分离：复位走 envSetStatusSQL，成败与主 SQL 无关。
	// 库里残留的 running（此刻正是 running）也就一定能被对账收回来。
	if _, err := execStmt(ctx, st.Pool(), envUpdateSQL,
		env.ID, tid, nullStr(env.ShopID), "", env.ProfileID, env.BaselineHash,
		env.KernelMajor, "", "running", []byte("{}"), []byte("{}"), time.Now(),
		"", "US", "", nil, ""); err == nil {
		t.Fatalf("模拟主提交故障失败：少一个实参时 envUpdateSQL 应报错")
	}
	changed, err = repos.SetStatusIfIn(ctx, env.ID, []string{"running"}, "idle")
	if err != nil {
		t.Fatalf("主提交 SQL 故障后，复位仍应成功（独立性被破坏）: %v", err)
	}
	if !changed {
		t.Fatalf("对账复位未命中（running 应被收回 idle）")
	}
	if got, _ = repos.Get(ctx, env.ID); got.Status != "idle" {
		t.Fatalf("对账后应为 idle，实际 %s（残留 running 没清掉）", got.Status)
	}
	if got.InjectScript != script {
		t.Fatalf("对账后 inject_script 被写坏: %q", got.InjectScript)
	}
}

// TestLauncherEnvSQLArityPG SQL 占位符数量与实参数量的一致性。
//
// 这是把「M3 阶段 8 事故」钉死在测试里的那颗钉子：写 environments 的每条语句，
// 占位符个数必须等于调用处实参个数。少一个参数在 PG 上就是整句执行不了，
// 而且是**特定分支才走到**（失败复位那条平时根本不执行），靠手点永远发现不了。
//
// 不需要连库（纯静态核对），但要跟着仓储一起跑，所以放在 pg 包内。
func TestLauncherEnvSQLArityPG(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"envInsertSQL", envInsertSQL, 19},
		{"envUpdateSQL", envUpdateSQL, 18},
		{"envSetStatusSQL", envSetStatusSQL, 5},
		{"envDeleteSQL", envDeleteSQL, 2},
	}
	for _, tc := range cases {
		if got := maxPlaceholder(tc.sql); got != tc.want {
			t.Fatalf("%s 占位符应为 %d 个，实际 %d（改列名必须同步改实参列表）",
				tc.name, tc.want, got)
		}
	}

	// inject_script 必须在 INSERT 与整行 UPDATE 里**成对**出现：
	// 只加列名不加实参 = 启动链路写不进状态（阶段 8 的原始根因）。
	for _, tc := range []struct {
		name string
		sql  string
	}{{"envInsertSQL", envInsertSQL}, {"envUpdateSQL", envUpdateSQL}} {
		if !strings.Contains(tc.sql, "inject_script") {
			t.Fatalf("%s 缺少 inject_script 列", tc.name)
		}
	}

	// ---------- 守卫本身：参数数量不匹配必须在打到数据库之前失败 ----------
	//
	// 这条断言的是 execStmt 这道防线：将来谁再漏写一个实参，测试立刻红，
	// 而且线上报错会带上「哪句 SQL、占位符几个、实参几个」。
	q := &arityQuerier{}
	if _, err := execStmt(context.Background(), q, envUpdateSQL,
		"env-x", "t-x", "", "", "prof", "bl", 131, "", "running", []byte("{}"), []byte("{}"),
		time.Now(), "", "", "", nil, ""); err == nil {
		t.Fatalf("实参少一个时应报错（正是阶段 8 的事故形态）")
	} else if !strings.Contains(err.Error(), "参数数量不匹配") {
		t.Fatalf("错误信息应说明是参数数量不匹配，实际: %v", err)
	}
	if q.calls != 0 {
		t.Fatalf("参数不匹配时不应打到数据库，实际执行 %d 次", q.calls)
	}
	if _, err := execStmt(context.Background(), q, envSetStatusSQL, "env-x", "t-x", "idle", time.Now(),
		[]string{"created"}); err != nil {
		t.Fatalf("参数匹配时不该报错: %v", err)
	}
	if q.calls != 1 {
		t.Fatalf("参数匹配时应执行一次，实际 %d 次", q.calls)
	}
}

// arityQuerier 只数调用次数的 querier 替身（用于断言守卫在打到库之前就拦下）。
type arityQuerier struct{ calls int }

func (q *arityQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	q.calls++
	return pgconn.CommandTag{}, nil
}

func (q *arityQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("arityQuerier: 未预期的 Query")
}

func (q *arityQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
