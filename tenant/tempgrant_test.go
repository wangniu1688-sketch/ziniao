package tenant_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"ziniao/billing"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/notify"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/tenant"
)

type grantEnv struct {
	svc  *tenant.Service
	st   *mem.Store
	bus  *eventbus.Bus
	role *types.Role
}

func newGrantEnv(t *testing.T) *grantEnv {
	t.Helper()
	ctx := context.Background()
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		t.Fatal(err)
	}
	role, err := st.Roles().GetByCode(ctx, "", "ops_manager")
	if err != nil {
		t.Fatalf("取角色失败: %v", err)
	}
	cfg := config.Default()
	bus := eventbus.New(st, cfg, slog.Default())
	bus.Register(notify.NewSink(st, slog.Default()))
	svc := tenant.NewService(st, bus, cfg, billing.NewMeter(st), billing.NewEntitlement(st, cfg))
	return &grantEnv{svc: svc, st: st, bus: bus, role: role}
}

// grant 写入一笔授权；userRoleAt 非空表示同时写入 user_roles 行（正式/临时的载体）。
func (e *grantEnv) grant(t *testing.T, userID string, expiresAt *time.Time, userRoleAt *time.Time) types.DataGrant {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if userRoleAt != nil {
		if err := e.st.UserRoles().Assign(ctx, &types.UserRole{
			ID: ids.New(), TenantID: "t1", UserID: userID, RoleID: e.role.ID,
			GrantedBy: "u-owner", GrantedAt: *userRoleAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	g := types.DataGrant{
		ID: ids.New(), TenantID: "t1", UserID: userID, ResourceType: "role",
		ResourceID: e.role.ID, Scope: "temporary", GrantedBy: "u-owner",
		ExpiresAt: expiresAt, CreatedAt: now,
	}
	if err := e.st.DataGrants().Create(ctx, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func past(hours int) *time.Time {
	t := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	return &t
}
func future(hours int) *time.Time {
	t := time.Now().UTC().Add(time.Duration(hours) * time.Hour)
	return &t
}
func agoHours(hours int) time.Time { return time.Now().UTC().Add(-time.Duration(hours) * time.Hour) }

func grantsOf(t *testing.T, st *mem.Store, userID string) []types.DataGrant {
	t.Helper()
	out, err := st.DataGrants().ListByUser(context.Background(), "t1", userID, "")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRevokeExpiredGrantNotifiesHolder 到期授权被清理，且**当事人**收到通知。
//
// 反向锁定两件事：一是收权必须落到 "明天过期行依旧在库" 的反面——行要真的没了；
// 二是必须有通知，否则用户只会看到某个操作忽然 403（行为规范：静默收权不算回收）。
func TestRevokeExpiredGrantNotifiesHolder(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	e.grant(t, "u-a", past(1), ptrTime(time.Now().UTC()))

	n, err := e.svc.RevokeExpiredGrants(ctx, "t1", time.Now().UTC())
	if err != nil || n != 1 {
		t.Fatalf("应回收 1 条，实际 %d err=%v", n, err)
	}
	if len(grantsOf(t, e.st, "u-a")) != 0 {
		t.Fatalf("到期授权未被清理")
	}
	if _, err := e.bus.DispatchOnce(ctx); err != nil {
		t.Fatalf("事件派发失败: %v", err)
	}
	list, err := e.st.Notifications().List(ctx, "t1", "u-a", 20)
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for _, x := range list {
		if x.EventType == eventbus.TypeTempGrantExpired {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("当事人应收到回收通知（风险等级若写成 low 会被 notify.Sink 丢掉），实际: %+v", list)
	}
	// 定向：没有权限的人不该被这条打扰
	others, _ := e.st.Notifications().List(ctx, "t1", "u-owner", 20)
	for _, x := range others {
		if x.EventType == eventbus.TypeTempGrantExpired {
			t.Fatalf("回收通知应只给当事人，管理员 u-owner 收到了")
		}
	}
}

// TestRevokeKeepsUnexpiredGrants 未到期与正式授权（expires_at 为空）一行都不能少。
func TestRevokeKeepsUnexpiredGrants(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	e.grant(t, "u-b", future(24), &now) // 未到期
	e.grant(t, "u-c", nil, &now)        // 正式授权，没有到期时间

	n, err := e.svc.RevokeExpiredGrants(ctx, "t1", now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("不应回收任何授权，实际 %d", n)
	}
	if len(grantsOf(t, e.st, "u-b")) != 1 || len(grantsOf(t, e.st, "u-c")) != 1 {
		t.Fatalf("未到期/正式授权被误删")
	}
	urs, err := e.st.UserRoles().ListByUser(ctx, "t1", "u-b")
	if err != nil || len(urs) != 1 {
		t.Fatalf("未到期临时授权对应的角色不应被撤销: %v %v", urs, err)
	}
}

// TestRevokeRevokesTempRoleWithIt 临时角色到期必须连带撤掉 user_roles 行，
// 否则会出现「授权记录清了、权限还在」的假回收。
func TestRevokeRevokesTempRoleWithIt(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	e.grant(t, "u-d", past(1), &now) // user_roles 与授权同一时刻写入

	if _, err := e.svc.RevokeExpiredGrants(ctx, "t1", now); err != nil {
		t.Fatal(err)
	}
	urs, err := e.st.UserRoles().ListByUser(ctx, "t1", "u-d")
	if err != nil {
		t.Fatal(err)
	}
	if len(urs) != 0 {
		t.Fatalf("临时角色到期后 user_roles 应一并撤销，仍有 %d 行", len(urs))
	}
}

// TestRevokeKeepsEarlierPermanentRole 关键判据：先正式授予、后又追加临时授权时
// 两者共用一行 user_roles（唯一约束所致）。临时到期**不能**把原有正式角色捎走。
//
// 这条最容易写错：不加 tinestamp 判断直接 Revoke，测试依然全绿，
// 但生产上会有人「临时权限到期后连正式权限一起没了」。
func TestRevokeKeepsEarlierPermanentRole(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	// 正式授予发生在 3 天前
	permanentAt := agoHours(72)
	e.grant(t, "u-e", nil, &permanentAt)
	// 临时授权此刻追加（grant 写入，user_roles 行因唯一约束仍是 3 天前那行）
	e.grant(t, "u-e", past(1), nil)

	if n, err := e.svc.RevokeExpiredGrants(ctx, "t1", time.Now().UTC()); err != nil || n != 1 {
		t.Fatalf("应只回收到期的那一笔，实际 %d err=%v", n, err)
	}
	urs, err := e.st.UserRoles().ListByUser(ctx, "t1", "u-e")
	if err != nil {
		t.Fatal(err)
	}
	if len(urs) != 1 {
		t.Fatalf("原有的正式角色必须保留（不能因临时到期被捎走），实际剩余 %d 行", len(urs))
	}
}

// TestGrantRepoReturnsExpiredRows 契约：仓储必须把过期记录**一并返回**。
//
// identity 判定「临时授权到期即失效」的依据就是这些过期记录；仓储若像以前那样
// 提前过滤掉，上层将永远看不到它们——结果是到期不失效、权限照旧。
func TestGrantRepoReturnsExpiredRows(t *testing.T) {
	e := newGrantEnv(t)
	e.grant(t, "u-f", past(1), nil)

	rows := grantsOf(t, e.st, "u-f")
	if len(rows) != 1 {
		t.Fatalf("过期记录必须仍能被读到（否则身份层的失效判定失去依据），实际 %d 条", len(rows))
	}
	if rows[0].ExpiresAt == nil || rows[0].ExpiresAt.After(time.Now()) {
		t.Fatalf("读到的应是已过期的那条: %+v", rows[0])
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
