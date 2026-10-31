package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ziniao/app"
	"ziniao/platform/config"
	"ziniao/platform/store/mem"
)

const testPassword = "Zn@12345678"

// newTestApp 用内存驱动装配应用（与生产同一条装配路径）。
func newTestApp(t *testing.T) *app.App {
	t.Helper()
	cfg := config.Default()
	cfg.StoreDriver = "memory"
	cfg.JWTSecret = "test-secret"
	a, err := app.New(context.Background(), cfg, mem.New(), nil)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	return a
}

func newTestServer(t *testing.T) (*app.App, *httptest.Server) {
	t.Helper()
	a := newTestApp(t)
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	return a, srv
}

// do 发起 HTTP 请求并返回状态码与解码后的 JSON。
func do(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// 终端信息：审计要能追到终端与 IP（L5）
	req.Header.Set("X-Terminal-FP", "fp-e2e-001")
	req.Header.Set("X-Client-Ver", "0.1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" && !bytes.HasPrefix([]byte(ct), []byte("application/json")) {
		return resp.StatusCode, map[string]any{"raw": ct}
	}
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// registerAndLogin 建租户并登录（专业版），返回 token 与租户 ID。
func registerAndLogin(t *testing.T, srv, name, username string) (string, string) {
	t.Helper()
	return registerAndLoginPlan(t, srv, name, username, "pro")
}

// registerAndLoginPlan 指定套餐建租户并登录。
func registerAndLoginPlan(t *testing.T, srv, name, username, plan string) (string, string) {
	t.Helper()
	st, body := do(t, "POST", srv+"/api/v1/auth/register", "", map[string]any{
		"name": name, "slug": username + "-shop", "plan": plan,
		"username": username, "password": testPassword, "nickname": username,
	})
	if st != http.StatusCreated {
		t.Fatalf("建租户失败: %d %v", st, body)
	}
	st, body = do(t, "POST", srv+"/api/v1/auth/login", "", map[string]any{
		"username": username, "password": testPassword,
	})
	if st != http.StatusOK {
		t.Fatalf("登录失败: %d %v", st, body)
	}
	token, _ := body["token"].(string)
	tenantID, _ := body["tenant_id"].(string)
	if token == "" || tenantID == "" {
		t.Fatalf("登录响应缺少 token/tenant_id: %v", body)
	}
	return token, tenantID
}

// TestPhase0Acceptance 验收路径：
// 建租户 → 建用户 → 授角色 → 一次写操作 → 必须产生带哈希链的审计记录。
func TestPhase0Acceptance(t *testing.T) {
	a, srv := newTestServer(t)
	ctx := context.Background()
	token, tenantID := registerAndLogin(t, srv.URL, "验收团队", "owner1")

	// 1) 写操作：新建店铺
	st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{
		"name": "亚马逊美国站", "platform": "Amazon", "site": "US", "url": "/shops",
	})
	if st != http.StatusCreated {
		t.Fatalf("建店失败: %d %v", st, body)
	}
	shopID, _ := body["id"].(string)
	if shopID == "" {
		t.Fatalf("建店响应缺少 id: %v", body)
	}

	// 2) 事件派发 → 审计落库（写 → eventbus → audit）
	if _, err := a.DispatchOnce(ctx); err != nil {
		t.Fatalf("事件派发失败: %v", err)
	}
	st, body = do(t, "GET", srv.URL+"/api/v1/audit/logs?action=shop.create", token, nil)
	if st != http.StatusOK {
		t.Fatalf("审计查询失败: %d %v", st, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应有 1 条 shop.create 审计记录，实际 %d", len(items))
	}
	entry := items[0].(map[string]any)
	if entry["shop_id"] != shopID {
		t.Fatalf("审计应能定位到店铺: %v", entry)
	}
	if entry["terminal_ip"] == nil || entry["terminal_ip"] == "" {
		t.Fatalf("审计应记录终端 IP（L5 人→店铺→终端→IP）: %v", entry)
	}
	if entry["hash"] == "" || entry["prev_hash"] == nil {
		t.Fatalf("审计应带哈希链字段: %v", entry)
	}

	// 3) 哈希链校验
	st, body = do(t, "GET", srv.URL+"/api/v1/audit/verify", token, nil)
	if st != http.StatusOK || body["ok"] != true {
		t.Fatalf("哈希链校验应通过: %d %v", st, body)
	}
	checked, _ := body["checked"].(float64)
	if checked < 3 { // tenant.created + user.registered + role.granted + login + shop.created
		t.Fatalf("审计条数偏少: %v", body)
	}

	// 4) 计量：店铺数 +1（统一走 billing.Meter）
	used, err := a.Meter.Current(ctx, tenantID, "shop.count")
	if err != nil || used != 1 {
		t.Fatalf("计量不正确: used=%d err=%v", used, err)
	}
}

// TestFinanceFieldMasking 财务角色看财务页：利润/成本/余额在服务端即被遮蔽（L6 第二层）。
func TestFinanceFieldMasking(t *testing.T) {
	a, srv := newTestServer(t)
	ctx := context.Background()
	ownerToken, tenantID := registerAndLogin(t, srv.URL, "财务演示团队", "boss1")

	// 建店
	if st, body := do(t, "POST", srv.URL+"/api/v1/shops", ownerToken, map[string]any{
		"name": "Shopee马来站", "platform": "Shopee", "site": "MY",
	}); st != http.StatusCreated {
		t.Fatalf("建店失败: %d %v", st, body)
	}

	// 邀请财务成员并授角色
	st, body := do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members", ownerToken, map[string]any{
		"username": "fin1", "password": testPassword, "nickname": "财务小李",
	})
	if st != http.StatusCreated {
		t.Fatalf("邀请成员失败: %d %v", st, body)
	}
	userID, _ := body["id"].(string)
	st, body = do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members/"+userID+"/roles", ownerToken,
		map[string]any{"role": "finance"})
	if st != http.StatusOK {
		t.Fatalf("授权失败: %d %v", st, body)
	}
	_, _ = a.DispatchOnce(ctx)

	// 财务登录查看店铺列表
	st, body = do(t, "POST", srv.URL+"/api/v1/auth/login", "", map[string]any{
		"username": "fin1", "password": testPassword,
	})
	if st != http.StatusOK {
		t.Fatalf("财务登录失败: %d %v", st, body)
	}
	finToken, _ := body["token"].(string)

	st, body = do(t, "GET", srv.URL+"/api/v1/shops", finToken, nil)
	if st != http.StatusOK {
		t.Fatalf("财务查店铺失败: %d %v", st, body)
	}
	items, _ := body["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("财务应能看到店铺（数据范围 all）: %v", body)
	}
	shop := items[0].(map[string]any)
	masked, _ := shop["masked_fields"].([]any)
	if len(masked) != 3 {
		t.Fatalf("财务应被遮蔽 profit/cost/balance，实际 %v", masked)
	}
	if shop["profit"] != "***" {
		t.Fatalf("利润必须替换为掩码，实际 %v", shop["profit"])
	}

	// DOM 注入层策略（L6 第三层）
	st, body = do(t, "GET", srv.URL+"/api/v1/iam/dom-policy?path=/finance", finToken, nil)
	if st != http.StatusOK {
		t.Fatalf("DOM 策略查询失败: %d %v", st, body)
	}
	deny, _ := body["deny_selectors"].([]any)
	if len(deny) != 2 {
		t.Fatalf("财务页应屏蔽导出按钮与利润字段元素，实际 %v", deny)
	}
}

// TestTenantIsolation 跨团队不可见：B 团队账号查不到 A 团队店铺（L12）。
func TestTenantIsolation(t *testing.T) {
	_, srv := newTestServer(t)
	_, _ = registerAndLogin(t, srv.URL, "团队A", "a_owner")
	tokenB, _ := registerAndLogin(t, srv.URL, "团队B", "b_owner")

	st, body := do(t, "GET", srv.URL+"/api/v1/shops", tokenB, nil)
	if st != http.StatusOK {
		t.Fatalf("查询失败: %d %v", st, body)
	}
	if total, _ := body["total"].(float64); total != 0 {
		t.Fatalf("B 团队不应看到 A 团队数据，实际 total=%v", total)
	}
}

// TestEntitlementAndApproval 免费/付费边界与二次审批：不能只灰按钮（反例 11 / 反例 8）。
func TestEntitlementAndApproval(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLoginPlan(t, srv.URL, "配额团队", "quota_owner", "free")

	// 免费版上限 3 家店：第 4 家必须被服务端强校验拒绝（UI 灰按钮不算控制）
	for i := 0; i < 3; i++ {
		if st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{
			"name": "店" + string(rune('A'+i)),
		}); st != http.StatusCreated {
			t.Fatalf("第 %d 家店应创建成功: %d %v", i+1, st, body)
		}
	}
	st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{"name": "超限店"})
	if st != http.StatusPaymentRequired {
		t.Fatalf("超过套餐配额应返回 402，实际 %d %v", st, body)
	}
	if body["code"] != "quota_exceeded" {
		t.Fatalf("错误码应为 quota_exceeded，实际 %v", body)
	}

	// 审计导出未携带审批单号：必须要求二次审批
	st, body = do(t, "GET", srv.URL+"/api/v1/audit/logs/export", token, nil)
	if st != http.StatusAccepted {
		t.Fatalf("未审批的明文导出应返回 202，实际 %d %v", st, body)
	}
	if body["code"] != "approval_required" {
		t.Fatalf("错误码应为 approval_required，实际 %v", body)
	}
}

// TestNoPermissionWrite 无权限写操作必须被拒绝，且事件不会误投递。
func TestNoPermissionWrite(t *testing.T) {
	a, srv := newTestServer(t)
	ctx := context.Background()
	token, tenantID := registerAndLogin(t, srv.URL, "只读团队", "ro_owner")

	// 建一个客服（只读）成员
	st, body := do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members", token, map[string]any{
		"username": "cs1", "password": testPassword, "nickname": "客服小王",
	})
	if st != http.StatusCreated {
		t.Fatalf("邀请失败: %d %v", st, body)
	}
	uid, _ := body["id"].(string)
	if st, body := do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members/"+uid+"/roles", token,
		map[string]any{"role": "cs"}); st != http.StatusOK {
		t.Fatalf("授权失败: %d %v", st, body)
	}
	st, body = do(t, "POST", srv.URL+"/api/v1/auth/login", "", map[string]any{
		"username": "cs1", "password": testPassword,
	})
	if st != http.StatusOK {
		t.Fatalf("客服登录失败: %d %v", st, body)
	}
	csToken, _ := body["token"].(string)

	st, body = do(t, "POST", srv.URL+"/api/v1/shops", csToken, map[string]any{"name": "越权店"})
	if st != http.StatusForbidden {
		t.Fatalf("客服不应能建店，实际 %d %v", st, body)
	}
	if body["code"] != "forbidden" {
		t.Fatalf("错误码应为 forbidden，实际 %v", body)
	}

	// 无权限操作不应产生审计写记录
	_, _ = a.DispatchOnce(ctx)
	st, body = do(t, "GET", srv.URL+"/api/v1/audit/logs?action=shop.create", token, nil)
	if st != http.StatusOK {
		t.Fatalf("审计查询失败: %d %v", st, body)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("越权被拦截不应产生 shop.create 审计，实际 %d 条", len(items))
	}
}

// TestOpenAPIDocumented 交付物要求：/openapi.json 必须可访问且包含受保护路由。
func TestOpenAPIDocumented(t *testing.T) {
	_, srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("openapi 请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("openapi 状态码: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(b, []byte("/api/v1/shops")) {
		t.Fatalf("openapi 缺少店铺路由: %s", b)
	}
}
