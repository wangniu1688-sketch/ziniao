package iam_test

import (
	"context"
	"testing"
	"time"

	"ziniao/iam"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

func has(perm string, list []string) bool {
	for _, p := range list {
		if p == perm {
			return true
		}
	}
	return false
}

// TestBuiltinRolePermissionsSyncOnUpgrade 老部署升级：运营主管拿到新版本新增的权限码。
//
// 场景还原：库里有一份「上个版本写入」的 ops_manager（缺 device:delete /
// env:deactivate / env:delete，与 approval:route.* 同源的老部署），升级后启动
// 必须自动补齐——否则控制台看不到删除按钮、调用才 403，升级日志里还没有任何痕迹。
func TestBuiltinRolePermissionsSyncOnUpgrade(t *testing.T) {
	st := mem.New()
	ctx := context.Background()
	// 种一份「老版本写入」的运营主管：权限是当时的快照，缺本轮新增的三个码
	old := &types.Role{
		ID: "role-ops-manager-old", Code: "ops_manager", Name: "运营主管", Builtin: true,
		Permissions: []string{
			"shop:read", "shop:update", "device:read", "device:purchase",
			"env:create", "env:launch", "env:stop", "page:shops",
		},
		DataScope: "all", CreatedAt: time.Now().UTC(),
	}
	if err := st.Roles().Create(ctx, old); err != nil {
		t.Fatal(err)
	}

	report, err := iam.EnsureBuiltinRolesWithReport(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if report["ops_manager"] <= 0 {
		t.Fatalf("应报告补齐数量，实际 %v", report)
	}
	role, err := st.Roles().GetByCode(ctx, "", "ops_manager")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"device:delete", "env:deactivate", "env:delete", "device:retire", "approval:route.manage"} {
		if !has(k, role.Permissions) {
			t.Fatalf("升级后应补上 %s，实际权限: %v", k, role.Permissions)
		}
	}
	// 原有的码一个都不能丢（并集：只补不删）
	for _, k := range []string{"shop:read", "device:purchase", "page:shops"} {
		if !has(k, role.Permissions) {
			t.Fatalf("原有权限 %s 不应丢失", k)
		}
	}
}

// TestBuiltinRoleSyncIsIdempotent 同步必须幂等：第二次启动报告 0 新增。
func TestBuiltinRoleSyncIsIdempotent(t *testing.T) {
	st := mem.New()
	ctx := context.Background()
	if _, err := iam.EnsureBuiltinRolesWithReport(ctx, st); err != nil {
		t.Fatal(err)
	}
	report, err := iam.EnsureBuiltinRolesWithReport(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 0 {
		t.Fatalf("二次同步不应再新增任何权限，实际 %v", report)
	}
}

// TestBuiltinRoleSyncKeepsDeploymentExtras 部署侧额外加的码要保留（并集不删）。
func TestBuiltinRoleSyncKeepsDeploymentExtras(t *testing.T) {
	st := mem.New()
	ctx := context.Background()
	role := &types.Role{
		ID: "role-cs", Code: "cs", Name: "客服", Builtin: true,
		Permissions: []string{"shop:read", "page:shops", "data:shop", "shop:special.local"},
		DataScope:   "own", CreatedAt: time.Now().UTC(),
	}
	if err := st.Roles().Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	if _, err := iam.EnsureBuiltinRolesWithReport(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := st.Roles().GetByCode(ctx, "", "cs")
	if err != nil {
		t.Fatal(err)
	}
	if !has("shop:special.local", got.Permissions) {
		t.Fatalf("部署侧额外加的码不应被抹掉，实际 %v", got.Permissions)
	}
}

// TestCustomRoleNeverSynced 自定义角色不参与同步：租户自己定义的角色一个码都不动。
func TestCustomRoleNeverSynced(t *testing.T) {
	st := mem.New()
	ctx := context.Background()
	custom := &types.Role{
		ID: "role-custom", Code: "ops_manager", Name: "本租户的运营主管（自建）", Builtin: false,
		TenantID:    "t1",
		Permissions: []string{"shop:read", "page:shops"},
		DataScope:   "own", CreatedAt: time.Now().UTC(),
	}
	if err := st.Roles().Create(ctx, custom); err != nil {
		t.Fatal(err)
	}
	if _, err := iam.EnsureBuiltinRolesWithReport(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := st.Roles().GetByCode(ctx, "t1", "ops_manager")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Permissions) != 2 {
		t.Fatalf("自定义角色不应被内置定义污染，实际 %v", got.Permissions)
	}
}
