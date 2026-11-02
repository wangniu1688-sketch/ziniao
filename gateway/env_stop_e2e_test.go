package gateway_test

import (
	"net/http"
	"testing"
)

// TestEnvLifecycleLaunchThenStop 端到端：启动 → 关窗口上报 → 状态从 running 收回 idle。
//
// 这条链路的意义：环境列表是运营判断「这个店铺现在有没有人在操作」的依据。
// 启动把状态写成 running 之后如果没有关闭上报路径，它就永远是 running——
// 那一列信息等于作废。这里把「启动—关闭—重复关闭」整条跑通，锁住这个闭环。
func TestEnvLifecycleLaunchThenStop(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "环境团队", "env_owner")

	st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{
		"name": "亚马逊美国站", "platform": "Amazon", "site": "US", "url": "/shops",
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

	// 1) 启动（服务端裁决并下发启动单，状态置 running）
	st, launch := do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/launch", token,
		map[string]any{"open_url": ""})
	if st != http.StatusOK {
		t.Fatalf("启动失败: %d %v", st, launch)
	}
	if launch["env_id"] == "" || launch["env_id"] == nil {
		t.Fatalf("启动单必须带 env_id: %v", launch)
	}

	// 2) 关窗口上报：状态必须回到 idle
	st, stopped := do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/stop", token,
		map[string]any{"url": "/shops"})
	if st != http.StatusOK {
		t.Fatalf("关闭上报失败: %d %v", st, stopped)
	}
	if stopped["status"] != "idle" {
		t.Fatalf("关闭后应为 idle，实际 %v", stopped)
	}

	// 3) 幂等：客户端会重复上报（关窗口一次、进程退出再一次），不能报错也不能重复写
	st, again := do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/stop", token, nil)
	if st != http.StatusOK || again["status"] != "idle" {
		t.Fatalf("重复关闭应仍是 idle: %d %v", st, again)
	}

	// 4) 不存在的店铺不能当成"关闭成功"——否则前端会永远显示已关闭
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/shop-none/environment/stop", token, nil); st == http.StatusOK {
		t.Fatalf("不存在的店铺不应返回 200，实际 %d", st)
	}
}

// TestEnvStopCrossTenantDenied 别的团队不能把别人的环境关掉（跨租户隔离）。
func TestEnvStopCrossTenantDenied(t *testing.T) {
	_, srv := newTestServer(t)
	owner, _ := registerAndLogin(t, srv.URL, "环境团队", "env_owner2")
	st, body := do(t, "POST", srv.URL+"/api/v1/shops", owner, map[string]any{
		"name": "店铺B", "platform": "Amazon", "site": "US", "url": "/shops",
	})
	if st != http.StatusCreated {
		t.Fatalf("建店失败: %d %v", st, body)
	}
	shopID, _ := body["id"].(string)
	other, _ := registerAndLogin(t, srv.URL, "别的团队", "other_owner")
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/stop", other, nil); st == http.StatusOK {
		t.Fatalf("跨租户关闭不应放行，实际 %d", st)
	}
}
