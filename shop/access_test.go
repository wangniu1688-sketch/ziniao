package shop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/config"
	"ziniao/platform/crypto"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/shop"
)

// accessSuite 环境与凭证两条链路共用的测试装配。
type accessSuite struct {
	env   *shop.EnvService
	vault *shop.Vault
	st    *mem.Store
	bus   *eventbus.Bus
	cfg   *config.Config
}

func newAccessSuite(t *testing.T) *accessSuite {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		t.Fatal(err)
	}
	// 自定义 own 范围的凭证角色：内置角色里持有凭证权限的都是 all 范围，
	// 而数据级收口要防的正是「own 范围 + 凭证/环境权限」这个组合。
	vaultOps := &types.Role{
		ID: ids.New(), Code: "vault_ops", Name: "凭证操作", Builtin: false,
		Permissions: []string{
			"shop:read", "shop:credential.manage", "shop:credential.reveal",
			"env:check", "env:launch", "env:stop", "page:shops", "data:shop",
		},
		DataScope: "own", CreatedAt: time.Now().UTC(),
	}
	if err := st.Roles().Create(ctx, vaultOps); err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New(st, cfg, nil)
	kek, err := crypto.NewLocalKEK(nil, 0) // 测试用随机主密钥
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := crypto.NewEnvelope(kek)
	if err != nil {
		t.Fatal(err)
	}
	return &accessSuite{
		env:   shop.NewEnvService(st, bus, iam.NewEngine(cfg), nil, nil),
		vault: shop.NewVault(st, bus, iam.NewEngine(cfg), cipher),
		st:    st, bus: bus, cfg: cfg,
	}
}

func (s *accessSuite) subjectOf(t *testing.T, id, code string) iam.Subject {
	t.Helper()
	r, err := s.st.Roles().GetByCode(context.Background(), "", code)
	if err != nil || r == nil {
		t.Fatalf("取角色 %s 失败: %v", code, err)
	}
	return iam.Subject{UserID: id, TenantID: "t1", Username: id, Roles: []types.Role{*r}}
}

func seedAccessShop(t *testing.T, st *mem.Store, id, ownerID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.Shops().Create(store.WithTenant(context.Background(), "t1"), &types.Shop{
		ID: id, TenantID: "t1", Name: "店-" + id, Platform: "Amazon", Status: "正常运营",
		OwnerID: ownerID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func grantShop(t *testing.T, st *mem.Store, userID, shopID string, expires *time.Time) {
	t.Helper()
	err := st.DataGrants().Create(store.WithTenant(context.Background(), "t1"), &types.DataGrant{
		ID: ids.New(), TenantID: "t1", UserID: userID,
		ResourceType: "shop", ResourceID: shopID, Scope: "delegation:test",
		GrantedBy: "u1", ExpiresAt: expires, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func wantDenied(t *testing.T, err error, why string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "数据级") {
		t.Fatalf("%s: 应被数据级校验拒绝，实际: %v", why, err)
	}
}

// TestEnvEntriesDataLevelGate own 范围成员不能启动/关闭/查看/自检他人店铺的环境；
// 持有有效店铺级 DataGrant（代理机构委托批准后写入）才放行；到期授权随读失效。
func TestEnvEntriesDataLevelGate(t *testing.T) {
	s := newAccessSuite(t)
	ctx := context.Background()
	op := s.subjectOf(t, "u2", "operator") // own 范围，店铺都是 u1 的

	seedEnv(t, s.st, "e1", shop.EnvStatusRunning, time.Now().UTC()) // 连带种 shop-e1（owner=u1）
	seedEnv(t, s.st, "e2", shop.EnvStatusRunning, time.Now().UTC())

	// 无授权：环境四个入口全部数据级拒绝
	_, err := s.env.Launch(ctx, shop.LaunchInput{ShopID: "shop-e1", Actor: op})
	wantDenied(t, err, "无授权启动")
	_, err = s.env.StopEnv(ctx, shop.StopInput{ShopID: "shop-e1", Actor: op})
	wantDenied(t, err, "无授权关闭")
	_, err = s.env.Get(ctx, op, "shop-e1")
	wantDenied(t, err, "无授权查看")
	_, err = s.env.Check(ctx, op, "shop-e1", nil)
	wantDenied(t, err, "无授权自检")

	// 有效授权：启动 → 自检 → 关闭全链路放行
	grantShop(t, s.st, "u2", "shop-e1", nil)
	spec, err := s.env.Launch(ctx, shop.LaunchInput{ShopID: "shop-e1", Actor: op})
	if err != nil {
		t.Fatalf("持有效授权应可启动: %v", err)
	}
	if spec.ShopID != "shop-e1" || spec.EnvID == "" {
		t.Fatalf("启动单不完整: %+v", spec)
	}
	if _, err := s.env.Check(ctx, op, "shop-e1", nil); err != nil {
		t.Fatalf("持有效授权应可自检: %v", err)
	}
	if _, err := s.env.StopEnv(ctx, shop.StopInput{ShopID: "shop-e1", Actor: op}); err != nil {
		t.Fatalf("持有效授权应可关闭: %v", err)
	}

	// 到期授权：随读即失效，不等巡检清行
	expired := time.Now().UTC().Add(-time.Minute)
	grantShop(t, s.st, "u2", "shop-e2", &expired)
	_, err = s.env.Launch(ctx, shop.LaunchInput{ShopID: "shop-e2", Actor: op})
	wantDenied(t, err, "到期授权启动")
}

// TestVaultDataLevelGate own 范围成员不能维护/列出/查看他人店铺的托管凭证；
// 委托授权只放开被委托的那几家店。
func TestVaultDataLevelGate(t *testing.T) {
	s := newAccessSuite(t)
	ctx := context.Background()
	vop := s.subjectOf(t, "u3", "vault_ops") // own 范围 + 凭证权限

	seedAccessShop(t, s.st, "shop-v1", "u1")
	seedAccessShop(t, s.st, "shop-v2", "u1")

	in := shop.PutInput{ShopID: "shop-v1", Kind: types.CredKindPassword,
		Username: "seller_a", Secret: "s3cret!", Actor: vop}
	wantDenied(t, s.vault.Put(ctx, in), "无授权写入凭证")
	if _, err := s.vault.ListKinds(ctx, vop, "shop-v1"); err == nil || !strings.Contains(err.Error(), "数据级") {
		t.Fatalf("无授权列凭证类型应被数据级拒绝: %v", err)
	}
	if _, err := s.vault.Reveal(ctx, shop.RevealInput{ShopID: "shop-v1",
		Kind: types.CredKindPassword, Actor: vop}); err == nil || !strings.Contains(err.Error(), "数据级") {
		t.Fatalf("无授权查看明文应被数据级拒绝: %v", err)
	}

	// 授权 shop-v1：写入 → 列类型 → 揭示明文全链路放行；shop-v2 仍拒绝
	grantShop(t, s.st, "u3", "shop-v1", nil)
	if err := s.vault.Put(ctx, in); err != nil {
		t.Fatalf("持有效授权应可写入凭证: %v", err)
	}
	kinds, err := s.vault.ListKinds(ctx, vop, "shop-v1")
	if err != nil || len(kinds) != 1 || kinds[0] != string(types.CredKindPassword) {
		t.Fatalf("列凭证类型异常: kinds=%v err=%v", kinds, err)
	}
	plain, err := s.vault.Reveal(ctx, shop.RevealInput{ShopID: "shop-v1",
		Kind: types.CredKindPassword, Actor: vop})
	if err != nil || plain != "s3cret!" {
		t.Fatalf("持有效授权应可查看明文: plain=%q err=%v", plain, err)
	}
	in.ShopID = "shop-v2"
	wantDenied(t, s.vault.Put(ctx, in), "授权只覆盖被委托的店铺")

	// RotateKey：授权店可轮换，未授权店拒绝
	n, err := s.vault.RotateKey(ctx, "shop-v1", vop, eventbus.Terminal{})
	if err != nil || n != 1 {
		t.Fatalf("轮换应成功且覆盖 1 条: n=%d err=%v", n, err)
	}
	if _, err := s.vault.RotateKey(ctx, "shop-v2", vop, eventbus.Terminal{}); err == nil ||
		!strings.Contains(err.Error(), "数据级") {
		t.Fatalf("未授权店轮换应被数据级拒绝: %v", err)
	}
}

// ---------- fail-closed ----------

type failingGrantRepo struct{ store.DataGrantRepo }

func (failingGrantRepo) ListByUser(context.Context, string, string, string) ([]types.DataGrant, error) {
	return nil, errors.New("db down")
}

type failingGrantStore struct {
	store.Store
}

func (failingGrantStore) DataGrants() store.DataGrantRepo { return failingGrantRepo{} }

// TestDataAccessFailsClosed 授权查询失败 = 当没有授权处理（fail-closed）。
// 半回填/半可读比完全拒绝危险：拒绝是显式的，半开是沉默的越权。
func TestDataAccessFailsClosed(t *testing.T) {
	s := newAccessSuite(t)
	ctx := context.Background()
	op := s.subjectOf(t, "u2", "operator")
	seedAccessShop(t, s.st, "shop-f1", "u1")
	seedEnv(t, s.st, "f1", shop.EnvStatusCreated, time.Now().UTC())
	grantShop(t, s.st, "u2", "shop-f1", nil) // 授权在库里，但查询层坏了

	broken := shop.NewEnvService(failingGrantStore{Store: s.st}, s.bus, iam.NewEngine(s.cfg), nil, nil)
	if _, err := broken.Launch(ctx, shop.LaunchInput{ShopID: "shop-f1", Actor: op}); err == nil ||
		!strings.Contains(err.Error(), "数据级校验失败") {
		t.Fatalf("授权查询失败应 fail-closed: %v", err)
	}
}
