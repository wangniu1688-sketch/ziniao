package gateway_test

import (
	"net/http"
	"testing"
	"time"

	"ziniao/network"
)

// TestDeviceRetireEndpoint 设备退役接口：归还出口并停用，权限与申购同级。
//
// 接口存在的意义不止是「多个按钮」：退役是**唯一一条人工触发的出口归还路径**
// （另一条是到期巡检）。没有它，运营拆掉一台设备就只能等租约到期，
// 而出口是按量计费的——中间这段时间一直在烧钱。
func TestDeviceRetireEndpoint(t *testing.T) {
	a, srv := newTestServer(t)
	// 装上出口池：设备与环境共用同一张路由表（不装也能退役，只是没有出口可还）
	a.SetIPPool(network.NewAllocatorWithRouter(network.NewRouter(
		network.Vendor{
			Provider: &network.MockProvider{VendorName: "ipidea", Country: "US", Type: network.ASNResidential},
			Meta:     network.VendorMeta{Name: "ipidea", Countries: []string{"US"}},
		},
	), time.Hour))

	token, tenantID := registerAndLogin(t, srv.URL, "退役团队", "retire_owner")
	_ = tenantID

	st, body := do(t, "POST", srv.URL+"/api/v1/devices/purchase", token, map[string]any{
		"name": "待退役设备", "country": "US", "count": 1, "months": 1, "url": "/devices",
	})
	if st != http.StatusCreated {
		t.Fatalf("申购失败: %d %v", st, body)
	}
	devID, _ := body["id"].(string)
	if devID == "" {
		t.Fatalf("申购响应缺少设备 ID: %v", body)
	}
	if body["ip"] == "" || body["ip"] == nil {
		t.Fatalf("接了出口池就应该有出口（设备出口走分配器）: %v", body)
	}

	// 未认证：401（服务端硬拦，不靠前端藏按钮）
	if st, _ = do(t, "POST", srv.URL+"/api/v1/devices/"+devID+"/retire", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("未认证退役应 401，实际 %d", st)
	}
	st, _ = do(t, "POST", srv.URL+"/api/v1/devices/"+devID+"/retire", token, map[string]any{"url": "/devices"})
	if st != http.StatusOK {
		t.Fatalf("老板退役设备应成功: %d", st)
	}
	// 退役后再查：设备应已停用
	st, body = do(t, "GET", srv.URL+"/api/v1/devices?limit=50", token, nil)
	if st != http.StatusOK {
		t.Fatalf("查设备列表失败: %d", st)
	}
	for _, it := range body["items"].([]any) {
		m, _ := it.(map[string]any)
		if m["id"] == devID && m["status"] != "recycled" {
			t.Fatalf("退役后状态应为 recycled，实际 %v", m["status"])
		}
	}
	// 重复退役必须幂等（不能 500，也不能重复归还）
	if st, _ = do(t, "POST", srv.URL+"/api/v1/devices/"+devID+"/retire", token, map[string]any{"url": "/devices"}); st != http.StatusOK {
		t.Fatalf("重复退役应幂等返回 200，实际 %d", st)
	}
}
