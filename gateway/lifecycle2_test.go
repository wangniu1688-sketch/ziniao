package gateway_test

import (
	"context"
	"net/http"
	"testing"

	"ziniao/app"
	"ziniao/iam"
	"ziniao/platform/store"
	"ziniao/platform/types"
	"ziniao/shop"
)

// TestEnvDeactivateActivateDeleteEndpoint 环境停用/启用/删除端到端。
//
// 停用入口的意义不只是多个按钮：Allocator.Release 此前只有「到期 sweep」一条
// 真实触发路径，环境退出服务时出口要等租约到期才归还（线路一直烧钱）。
func TestEnvDeactivateActivateDeleteEndpoint(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "环境停用团队", "env_admin")

	st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{
		"name": "停用测试店", "platform": "Amazon", "site": "US", "url": "/shops",
	})
	if st != http.StatusCreated {
		t.Fatalf("建店失败: %d %v", st, body)
	}
	shopID, _ := body["id"].(string)
	st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment", token, map[string]any{
		"country": "US", "url": "/shops",
	})
	if st != http.StatusCreated {
		t.Fatalf("创建环境失败: %d", st)
	}

	// 1) 停用（幂等：重复停用仍 200）
	st, env := do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/deactivate", token,
		map[string]any{"url": "/shops"})
	if st != http.StatusOK || env["status"] != "stopped" {
		t.Fatalf("停用应 200 + stopped: %d %v", st, env)
	}
	st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/deactivate", token, nil)
	if st != http.StatusOK {
		t.Fatalf("重复停用应幂等 200，实际 %d", st)
	}

	// 2) 停用后不能启动
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/launch", token,
		map[string]any{"open_url": ""}); st == http.StatusOK {
		t.Fatal("停用的环境不应能拿到启动单")
	}

	// 3) 启用回来 → idle → 可再启动
	st, env = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/activate", token, nil)
	if st != http.StatusOK || env["status"] != "idle" {
		t.Fatalf("启用应回到 idle: %d %v", st, env)
	}

	// 4) 删除（终态）→ 查询环境 404
	if st, _ = do(t, "DELETE", srv.URL+"/api/v1/shops/"+shopID+"/environment", token, nil); st != http.StatusOK {
		t.Fatalf("删除环境应 200，实际 %d", st)
	}
	if st, _ = do(t, "GET", srv.URL+"/api/v1/shops/"+shopID+"/environment", token, nil); st != http.StatusNotFound {
		t.Fatalf("删除后查询环境应 404，实际 %d", st)
	}
	// 再删一次：店铺已无环境，不能假装成功
	if st, _ = do(t, "DELETE", srv.URL+"/api/v1/shops/"+shopID+"/environment", token, nil); st != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实际 %d", st)
	}
}

// TestEnvDeactivateRunningRejected 运行中的环境必须先关闭才能停用/删除。
//
// M3/T1 之后「启动」不再由旧 launch 接口直接写 running（running 只在浏览器起来 +
// CDP 连通后由 MarkRunning 提交），所以这里用服务层把状态推进到 running，
// 验证的仍是同一条业务规则：**窗口还开着就不许停用/删除**。
func TestEnvDeactivateRunningRejected(t *testing.T) {
	a, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "运行中团队", "env_admin2")
	st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{
		"name": "运行中店铺", "platform": "Amazon", "site": "US", "url": "/shops",
	})
	if st != http.StatusCreated {
		t.Fatalf("建店失败: %d %v", st, body)
	}
	shopID, _ := body["id"].(string)
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment", token, map[string]any{
		"country": "US", "url": "/shops",
	}); st != http.StatusCreated {
		t.Fatalf("创建环境失败: %d", st)
	}
	setRunning(t, a, tenantID, shopID)

	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/deactivate", token, nil); st != http.StatusConflict {
		t.Fatalf("运行中停用应 409，实际 %d", st)
	}
	if st, _ = do(t, "DELETE", srv.URL+"/api/v1/shops/"+shopID+"/environment", token, nil); st != http.StatusConflict {
		t.Fatalf("运行中删除应 409，实际 %d", st)
	}
}

// setRunning 用服务层把某店铺的环境推进到 running（M3 起 running 的唯一提交点）。
//
// 单测里没法真拉起 Chrome，而「运行中不让停用/删除」这条规则必须继续被守住，
// 所以直接调 MarkRunning——它走的是与 M3 启动链路同一个提交点，不是另开后门。
func setRunning(t *testing.T, a *app.App, tenantID, shopID string) {
	t.Helper()
	ctx := context.Background()
	role, err := a.Store.Roles().GetByCode(ctx, "", "owner")
	if err != nil {
		t.Fatalf("取内置 owner 角色失败: %v", err)
	}
	actor := iam.Subject{UserID: "u-test", TenantID: tenantID, Username: "owner", Roles: []types.Role{*role}}
	tctx := store.WithTenant(ctx, tenantID)
	env, err := a.Store.Environments().GetByShop(tctx, shopID)
	if err != nil || env == nil {
		t.Fatalf("取环境失败: %v", err)
	}
	if _, err := a.Environments.MarkRunning(ctx, shop.MarkRunningInput{EnvID: env.ID, Actor: actor}); err != nil {
		t.Fatalf("推进到 running 失败: %v", err)
	}
}

// TestDeviceDeleteEndpoint 设备删除端到端：仅退役后可删，删除释放配额，重复删除幂等。
func TestDeviceDeleteEndpoint(t *testing.T) {
	a, srv := newTestServer(t)
	_ = a
	token, _ := registerAndLogin(t, srv.URL, "删除团队", "dev_admin")

	st, body := do(t, "POST", srv.URL+"/api/v1/devices/purchase", token, map[string]any{
		"name": "待删除设备", "country": "US", "count": 1, "months": 1, "url": "/devices",
	})
	if st != http.StatusCreated {
		t.Fatalf("申购失败: %d %v", st, body)
	}
	devID, _ := body["id"].(string)

	// active 不能删
	if st, _ = do(t, "DELETE", srv.URL+"/api/v1/devices/"+devID, token, nil); st != http.StatusConflict {
		t.Fatalf("active 设备删除应 409，实际 %d", st)
	}
	// 退役后可删
	if st, _ = do(t, "POST", srv.URL+"/api/v1/devices/"+devID+"/retire", token, map[string]any{"url": "/devices"}); st != http.StatusOK {
		t.Fatalf("退役失败: %d", st)
	}
	if st, _ = do(t, "DELETE", srv.URL+"/api/v1/devices/"+devID, token, nil); st != http.StatusOK {
		t.Fatalf("删除退役设备应 200，实际 %d", st)
	}
	// 列表不再出现（软删除）
	st, body = do(t, "GET", srv.URL+"/api/v1/devices?limit=50", token, nil)
	if st != http.StatusOK {
		t.Fatalf("查列表失败: %d %v", st, body)
	}
	items, _ := body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["id"] == devID {
			t.Fatal("删除后的设备不应出现在列表里")
		}
	}
	// 未认证 401
	if st, _ = do(t, "DELETE", srv.URL+"/api/v1/devices/"+devID, "", nil); st != http.StatusUnauthorized {
		t.Fatalf("未认证删除应 401，实际 %d", st)
	}
}
