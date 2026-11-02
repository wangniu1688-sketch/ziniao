package shop_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ziniao/eventbus"
	"ziniao/fingerprint"
	"ziniao/iam"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/shop"
)

// newEnvSvcWithLib 装配一个能真正采画像 / 查 TLS 基线的环境服务。
func newEnvSvcWithLib(t *testing.T) (*shop.EnvService, *mem.Store, iam.Subject) {
	t.Helper()
	// 租户域仓储一律从 ctx 取 tenant_id（fail-closed），建店铺就得带上租户上下文
	ctx := store.WithTenant(context.Background(), "t1")
	cfg := config.Default()
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		t.Fatal(err)
	}
	role, err := st.Roles().GetByCode(ctx, "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.Shops().Create(ctx, &types.Shop{ID: "shop-1", TenantID: "t1",
		Name: "店-1", Platform: "Amazon", Status: "正常运营",
		OwnerID: "u1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	bl, err := fingerprint.LoadBaselines()
	if err != nil {
		t.Fatalf("TLS 基线加载失败: %v", err)
	}
	svc := shop.NewEnvService(st, bus(), iam.NewEngine(cfg),
		fingerprint.NewLibrary(fingerprint.DefaultLibraryConfig(), 7), bl)
	return svc, st, iam.Subject{UserID: "u1", TenantID: "t1", Username: "owner", Roles: []types.Role{*role}}
}

func bus() *eventbus.Bus { return eventbus.New(mem.New(), config.Default(), nil) }

// TestCreateEnvAlignsKernelToLocalBrowser 建环境时必须完成内核对齐：
// UA / UA-CH / TLS 握手同出一源，且不得高于本机 Chrome（本机 156 → 对齐到 120）。
func TestCreateEnvAlignsKernelToLocalBrowser(t *testing.T) {
	svc, _, actor := newEnvSvcWithLib(t)
	svc.SetKernelMajor(156)

	env, plan, err := svc.CreateEnv(context.Background(), shop.CreateEnvInput{
		ShopID: "shop-1", Country: "JP", Actor: actor,
	})
	if err != nil {
		t.Fatalf("建环境失败: %v", err)
	}
	if env.KernelMajor != 120 {
		t.Fatalf("生效内核应为 120（本机 156 高于基线库上限），实际 %d", env.KernelMajor)
	}
	if env.TLSHelloID != "HelloChrome_120" {
		t.Fatalf("TLS 基线必须与 UA 同源，实际 hello=%q", env.TLSHelloID)
	}
	// 桌面 Chrome 的 UA 只出大版本；完整版本留给 UA-CH
	if !strings.Contains(env.Snapshot["ua"], "Chrome/120.0.0.0") {
		t.Fatalf("快照 UA 未对齐: %q", env.Snapshot["ua"])
	}
	// 时区名必须进脚本，否则页面里 Date#toString 仍是本机时区
	if !strings.Contains(plan.Script, "日本標準時") {
		t.Fatalf("注入脚本缺少画像时区名（Date#toString 会暴露本机时区）")
	}
	// 原因要跟着自检报告落库：运营看到 Chrome 120 时必须同时看到为什么
	var rep fingerprint.Report
	if err := json.Unmarshal(env.Consistency, &rep); err != nil {
		t.Fatalf("自检报告解析失败: %v", err)
	}
	joined := strings.Join(rep.Notes, "；")
	if !strings.Contains(joined, "156") {
		t.Fatalf("自检报告未说明内核对齐原因: %+v", rep.Notes)
	}
}

// TestLaunchRebuildsStaleScript P1 加固前落库的老环境必须能自动吃上新版脚本：
// 否则升级后运营看到的是「明明修了、browserleaks 还是 GMT+0800」。
func TestLaunchRebuildsStaleScript(t *testing.T) {
	svc, st, actor := newEnvSvcWithLib(t)
	ctx := store.WithTenant(context.Background(), "t1")
	env, _, err := svc.CreateEnv(ctx, shop.CreateEnvInput{
		ShopID: "shop-1", Country: "JP", Actor: actor,
	})
	if err != nil {
		t.Fatalf("建环境失败: %v", err)
	}
	// 模拟 M3 期的旧版脚本：没有版本标记、没有时区串覆盖
	env.InjectScript = "try{window.__ZINIAO_COMPAT__=true;}catch(e){}"
	if err := st.Environments().Update(ctx, env); err != nil {
		t.Fatalf("回写旧脚本失败: %v", err)
	}

	spec, err := svc.Launch(ctx, shop.LaunchInput{ShopID: "shop-1", Actor: actor})
	if err != nil {
		t.Fatalf("启动环境失败: %v", err)
	}
	if !fingerprint.ScriptHasVersion(spec.Script, fingerprint.ScriptVersion) {
		t.Fatal("老环境的脚本未按快照重建为新版（缺版本标记）")
	}
	if !strings.Contains(spec.Script, "日本標準時") {
		t.Fatal("重建脚本缺少画像时区名，Date#toString 仍会暴露本机时区")
	}
}

// TestCreateEnvKernelUnlimitedWhenBrowserUnknown 未探测到本机浏览器版本时不设上限，
// 行为与对齐前一致（画像版本 + 握手回落），保证「对齐」只是增强。
func TestCreateEnvKernelUnlimitedWhenBrowserUnknown(t *testing.T) {
	svc, _, actor := newEnvSvcWithLib(t)
	env, _, err := svc.CreateEnv(context.Background(), shop.CreateEnvInput{
		ShopID: "shop-1", Country: "US", Actor: actor,
	})
	if err != nil {
		t.Fatalf("建环境失败: %v", err)
	}
	if env.KernelMajor != 120 || !strings.Contains(env.Snapshot["ua"], "Chrome/120.0.0.0") {
		t.Fatalf("未探测到本机内核时应沿用画像版本并做握手回落: major=%d ua=%q",
			env.KernelMajor, env.Snapshot["ua"])
	}
}
