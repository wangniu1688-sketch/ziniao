package gateway_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ziniao/network"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TestEgressVendorsVisibility 出口路由的可见性：老板看得到、普通成员看不到、匿名 401。
//
// 为什么这条要写测试：多代理商之后「这个店铺的出口走的哪家」是排障第一问。
// 而供应商名单属于采购信息，不能让每个账号都能拉——所以授权限定在
// egress:read（老板 + 运营主管），其余一律 403（服务端硬拦，不是前端藏按钮）。
func TestEgressVendorsVisibility(t *testing.T) {
	a, srv := newTestServer(t)
	// 装上两家代理商的路由表（mock，不触网）
	rt := network.NewRouter(
		network.Vendor{
			Provider: &network.MockProvider{VendorName: "ipidea", Country: "US", Type: network.ASNResidential},
			Meta:     network.VendorMeta{Name: "ipidea", Weight: 3, Countries: []string{"US"}},
		},
		network.Vendor{
			Provider: &network.MockProvider{VendorName: "backup", Country: "US", Type: network.ASNResidential, PortOffset: 100},
			Meta:     network.VendorMeta{Name: "backup", Priority: 10},
		},
	)
	a.SetIPPool(network.NewAllocatorWithRouter(rt, time.Hour))

	token, tenantID := registerAndLogin(t, srv.URL, "出口团队", "egress_owner")

	st, body := do(t, "GET", srv.URL+"/api/v1/egress/vendors", token, nil)
	if st != http.StatusOK {
		t.Fatalf("老板应能看到路由表: %d %v", st, body)
	}
	if body["enabled"] != true {
		t.Fatalf("已装池应返回 enabled=true: %v", body)
	}
	vs, _ := body["vendors"].([]any)
	if len(vs) != 2 {
		t.Fatalf("应返回两家代理商，实际 %d", len(vs))
	}
	first, _ := vs[0].(map[string]any)
	if first["name"] != "ipidea" {
		t.Fatalf("路由表应按优先级排序（ipidea 在前）：%v", vs)
	}
	// 凭据绝不能出现在响应里（L4：Token 只存在于内存与环境变量）
	raw := ""
	for _, v := range vs {
		m, _ := v.(map[string]any)
		for _, k := range []string{"token", "password", "secret", "username"} {
			if _, ok := m[k]; ok {
				t.Fatalf("路由表泄漏了凭据字段 %s", k)
			}
		}
		raw += "x"
	}

	// 普通成员：403
	st, body = do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members", token, map[string]any{
		"username": "egress_member", "password": testPassword, "nickname": "运营",
	})
	if st != http.StatusCreated {
		t.Fatalf("邀请成员失败: %d %v", st, body)
	}
	st, body = do(t, "POST", srv.URL+"/api/v1/auth/login", "", map[string]any{
		"username": "egress_member", "password": testPassword,
	})
	if st != http.StatusOK {
		t.Fatalf("成员登录失败: %d %v", st, body)
	}
	memberToken, _ := body["token"].(string)
	if st, _ = do(t, "GET", srv.URL+"/api/v1/egress/vendors", memberToken, nil); st != http.StatusForbidden {
		t.Fatalf("未授权成员不应看到出口路由表，实际 %d", st)
	}

	// 未认证
	if st, _ = do(t, "GET", srv.URL+"/api/v1/egress/vendors", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，实际 %d", st)
	}
}

// TestEgressHistoryAndASNSharing 出口历史与 ASN 关联线索的接口口径。
//
// 重点不在「能读到数据」，而在两件容易出事的语义：
//  1. 历史必须在**当前绑定已解绑之后**还查得到（否则等于没做留档）；
//  2. 「共用 ASN」按不同环境计数，而且只统计时间窗内的记录。
func TestEgressHistoryAndASNSharing(t *testing.T) {
	a, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "出口历史团队", "hist_owner")
	ctx := store.WithTenant(context.Background(), tenantID)
	repo := a.Store.EgressBindings()

	for i, env := range []string{"env-a", "env-b"} {
		if err := repo.AppendEvent(ctx, &types.EgressBindingEvent{
			EnvID: env, Action: types.EgressActionBound, Provider: "ipidea",
			ResourceID: "res-" + env, Protocol: "socks5",
			Host: "203.0.113.1", Port: 1080 + i,
			ASN: "AS64500", ASNType: "residential",
			ExitAddr: "203.0.113.1:1080",
		}); err != nil {
			t.Fatalf("写入历史失败: %v", err)
		}
	}
	// env-a 释放：当前绑定没了，历史必须还在。
	if err := repo.AppendEvent(ctx, &types.EgressBindingEvent{
		EnvID: "env-a", Action: types.EgressActionReleased, Provider: "ipidea",
		ResourceID: "res-env-a", Protocol: "socks5", Host: "203.0.113.1", Port: 1080,
		ASN: "AS64500", ASNType: "residential", ExitAddr: "203.0.113.1:1080",
	}); err != nil {
		t.Fatalf("写入历史失败: %v", err)
	}

	st, body := do(t, "GET", srv.URL+"/api/v1/egress/history?env_id=env-a", token, nil)
	if st != http.StatusOK {
		t.Fatalf("查历史失败: %d %v", st, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 || body["total"] != float64(2) {
		t.Fatalf("env-a 应有 bound + released 两条历史，实际 %v（total=%v）", items, body["total"])
	}
	first, _ := items[0].(map[string]any)
	if first["asn"] != "AS64500" || first["exit_addr"] != "203.0.113.1:1080" {
		t.Fatalf("历史必须带 ASN 与出口（关联检测的唯一输入）：%v", first)
	}

	// 筛可选动作：非法值必须 400，而不是「筛了个不存在的动作，返回空列表」。
	if st, _ = do(t, "GET", srv.URL+"/api/v1/egress/history?action=bounded", token, nil); st != http.StatusBadRequest {
		t.Fatalf("未知 action 应 400，实际 %d", st)
	}
	if st, _ = do(t, "GET", srv.URL+"/api/v1/egress/history?from=yesterday", token, nil); st != http.StatusBadRequest {
		t.Fatalf("非法 from 应 400，实际 %d", st)
	}

	st, body = do(t, "GET", srv.URL+"/api/v1/egress/asn-sharing", token, nil)
	if st != http.StatusOK {
		t.Fatalf("关联检测失败: %d %v", st, body)
	}
	shares, _ := body["items"].([]any)
	if len(shares) != 1 {
		t.Fatalf("应识别出 1 组共用 ASN，实际 %v", shares)
	}
	share, _ := shares[0].(map[string]any)
	if envs, _ := share["env_ids"].([]any); len(envs) != 2 {
		t.Fatalf("共用 ASN 的环境数应为 2，实际 %v", share["env_ids"])
	}
	// min_envs 高于实际时不应报（避免把单店漂移误报成关联）
	if st, body = do(t, "GET", srv.URL+"/api/v1/egress/asn-sharing?min_envs=3", token, nil); st != http.StatusOK {
		t.Fatalf("关联检测失败: %d %v", st, body)
	}
	if shares, _ = body["items"].([]any); len(shares) != 0 {
		t.Fatalf("min_envs=3 时不应有结果: %v", shares)
	}

	// 历史同样只给 egress:read：匿名 401（服务端硬拦，不靠前端藏入口）。
	if st, _ = do(t, "GET", srv.URL+"/api/v1/egress/history", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("未认证查历史应 401，实际 %d", st)
	}
}
