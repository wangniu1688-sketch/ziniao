package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func ctxBackground() context.Context { return context.Background() }

// TestPhase1EnvironmentVaultAndIntercept 端到端：指纹基线 → 建环境 → 自检 → 账密托管 → 事前拦截 → 审计留痕。
func TestPhase1EnvironmentVaultAndIntercept(t *testing.T) {
	a, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "指纹团队", "fp_owner")

	// 1) TLS 指纹基线：必须由 uTLS 真实 ClientHello 计算（source 可查）
	st, bl := do(t, "GET", srv.URL+"/api/v1/fingerprint/tls-baselines", token, nil)
	if st != http.StatusOK {
		t.Fatalf("基线查询失败: %d %v", st, bl)
	}
	baseItems, _ := bl["items"].([]any)
	if len(baseItems) == 0 {
		t.Fatalf("基线不应为空: %v", bl)
	}
	if bl["baseline_hash"] == "" || bl["baseline_hash"] == nil {
		t.Fatal("基线库哈希缺失（L8 离线校验凭据）")
	}

	// 2) 建店
	st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{
		"name": "亚马逊美国站", "platform": "Amazon", "site": "US", "url": "/shops",
	})
	if st != http.StatusCreated {
		t.Fatalf("建店失败: %d %v", st, body)
	}
	shopID, _ := body["id"].(string)

	// 3) 创建店铺环境（画像 + 内核 + TLS 基线绑定）
	st, envBody := do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment", token, map[string]any{
		"country": "US", "url": "/shops",
	})
	if st != http.StatusCreated {
		t.Fatalf("创建环境失败: %d %v", st, envBody)
	}
	env, _ := envBody["environment"].(map[string]any)
	if env == nil {
		t.Fatalf("响应缺少 environment: %v", envBody)
	}
	baseline, _ := env["baseline_hash"].(string)
	if baseline == "" {
		t.Fatal("环境必须带基线哈希（L8）")
	}
	if _, ok := env["kernel_major"].(float64); !ok {
		t.Fatalf("环境必须绑定内核版本: %v", env)
	}
	plan, _ := envBody["plan"].(map[string]any)
	if plan == nil || plan["baseline_hash"] != baseline {
		t.Fatalf("注入计划基线哈希必须与环境一致: %v", plan)
	}

	// 4) 环境自检：上报正确基线哈希 → 零漂移 + 通过
	st, rep := do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/check", token, map[string]any{
		"baseline_hash": baseline,
	})
	if st != http.StatusOK {
		t.Fatalf("自检失败: %d %v", st, rep)
	}
	if passed, _ := rep["passed"].(bool); !passed {
		t.Fatalf("自检应通过: %v", rep)
	}
	if match, _ := rep["baseline_match"].(bool); !match {
		t.Fatalf("基线哈希应匹配: %v", rep)
	}
	drift, _ := rep["drift"].(map[string]any)
	if drift != nil {
		if risk, _ := drift["risk"].(string); risk != "low" {
			t.Fatalf("与基线一致时不应有漂移: %v", drift)
		}
	}
	// 上报错误基线哈希 → 必须禁止离线启动（L8）
	_, repBad := do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/environment/check", token, map[string]any{
		"baseline_hash": "tampered-hash",
	})
	if passed, _ := repBad["passed"].(bool); passed {
		t.Fatalf("基线不一致时不应通过: %v", repBad)
	}

	// 5) 账密托管：写入后可解密；列表响应绝不含密文/明文（L4）
	secret := "ShopeeSuperSecret#2026"
	st, _ = do(t, "PUT", srv.URL+"/api/v1/shops/"+shopID+"/credentials", token, map[string]any{
		"kind": "password", "username": "seller1", "secret": secret, "url": "/shops",
	})
	if st != http.StatusOK {
		t.Fatalf("凭证写入失败: %d", st)
	}
	st, kinds := do(t, "GET", srv.URL+"/api/v1/shops/"+shopID+"/credentials", token, nil)
	if st != http.StatusOK {
		t.Fatalf("凭证列表失败: %d", st)
	}
	// L4：列表响应只能看到凭证类型，绝不能出现明文或密文
	kindList, _ := kinds["kinds"].([]any)
	if len(kindList) != 1 || kindList[0] != "password" {
		t.Fatalf("凭证类型列表异常: %v", kinds)
	}
	rawKinds, _ := json.Marshal(kinds)
	if strings.Contains(strings.ToLower(string(rawKinds)), strings.ToLower(secret)) {
		t.Fatal("列表响应不得包含明文")
	}
	// Phase 2 起：查看明文必须持审批单（完整流程见 TestPhase2ApprovalAndExport）
	st, rev := do(t, "GET", srv.URL+"/api/v1/shops/"+shopID+"/credentials/password", token, nil)
	if st == http.StatusOK {
		t.Fatalf("未审批不应返回明文: %v", rev)
	}
	if rev["code"] != "approval_required" {
		t.Fatalf("错误码应为 approval_required，实际 %v", rev)
	}

	// 6) 事前拦截：策略命中 deny → 拒绝 + 落安全事件
	st, pol := do(t, "POST", srv.URL+"/api/v1/security/policies", token, map[string]any{
		"name": "高危操作防护", "scope": "shop", "scope_id": shopID, "enabled": true,
		"rules": []any{map[string]any{
			"id": "r1", "type": "element", "pattern": `[data-el="shop.delete_button"]`,
			"action": "deny", "severity": "high",
		}},
		"url": "/security",
	})
	if st != http.StatusOK {
		t.Fatalf("策略写入失败: %d %v", st, pol)
	}
	st, res := do(t, "POST", srv.URL+"/api/v1/security/intercept", token, map[string]any{
		"shop_id": shopID, "element": `[data-el="shop.delete_button"]`, "url": "/shops",
	})
	if st != http.StatusOK {
		t.Fatalf("拦截判定失败: %d %v", st, res)
	}
	if allowed, _ := res["allowed"].(bool); allowed {
		t.Fatalf("命中 deny 规则应拒绝: %v", res)
	}
	if res["action"] != "deny" {
		t.Fatalf("动作应为 deny: %v", res)
	}

	// 7) 安全事件已落库（事后追溯）
	st, ev := do(t, "GET", srv.URL+"/api/v1/security/events?limit=20", token, nil)
	if st != http.StatusOK {
		t.Fatalf("安全事件查询失败: %d %v", st, ev)
	}
	if total, _ := ev["total"].(float64); total < 1 {
		t.Fatalf("拦截应产生安全事件: %v", ev)
	}

	// 8) 生命周期状态机：注册中 → 养号期 → 正常运营 合法；注册中 → 正常运营 非法
	st, tr := do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/status", token, map[string]any{"to": "养号期"})
	if st != http.StatusOK {
		t.Fatalf("注册中→养号期应放行: %d %v", st, tr)
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/status", token, map[string]any{"to": "正常运营"}); st != http.StatusOK {
		t.Fatal("养号期→正常运营应放行")
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopID+"/status", token, map[string]any{"to": "注册中"}); st == http.StatusOK {
		t.Fatal("正常运营→注册中属于非法迁移，应拒绝")
	}

	// 9) 所有写操作必须落审计（L5）
	if _, err := a.DispatchOnce(ctxBackground()); err != nil {
		t.Fatal(err)
	}
	st, logs := do(t, "GET", srv.URL+"/api/v1/audit/logs?limit=50", token, nil)
	if st != http.StatusOK {
		t.Fatalf("审计查询失败: %d", st)
	}
	items, _ := logs["items"].([]any)
	actions := map[string]bool{}
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			actions[m["action"].(string)] = true
		}
	}
	for _, want := range []string{"env.create", "shop.credential.put", "security.policy.upsert"} {
		if !actions[want] {
			t.Fatalf("审计日志缺少 %s，实际: %v", want, actions)
		}
	}
}
