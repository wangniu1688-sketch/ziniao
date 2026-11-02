package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// mustMarshal 把响应对象整体序列化后做「子串扫描」断言。
//
// 只逐字段检查是不够的：新增字段时最容易出现的漏网之鱼就是某个新字段
// 顺手把明文带了出来。整串扫描让这种错误当场暴露。
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return b
}

// P3 上游代理接入的网关层测试：开关 on/off × 绑定校验 × 掩码 × skipped 标记。
//
// 与 shop 层的分工：这里断言的是**接口契约**（状态码、响应字段、有没有口令），
// 拨测本身的落库语义由 shop/proxy_probe_test.go 覆盖。

// TestP3BindRequiresProbeOKToggle 同一个绑定动作在开关 on/off 下的两种结局。
//
// 这条用例的意义在于证明 off 不是「放宽到没有校验」，而是「明确地跳过并留痕」：
// off 时允许绑定是拍板结论，被误判成 fail 会让内网部署永远绑不上代理。
func TestP3BindRequiresProbeOKToggle(t *testing.T) {
	aOn, srvOn := newTestServer(t)
	tokenOn, tenantOn := registerAndLogin(t, srvOn.URL, "P3开关On", "p3on")
	envOn := createEnvForBind(t, srvOn.URL, tokenOn)
	proxyOn := createTestProxy(t, srvOn.URL, tokenOn)

	// on：unknown（从没拨测过）→ 400。
	if st, body := do(t, "PUT", srvOn.URL+"/api/v1/environments/"+envOn+"/proxy", tokenOn,
		map[string]any{"proxy_id": proxyOn, "url": "/environments"}); st != http.StatusBadRequest {
		t.Fatalf("on 时未拨测的代理应 400，实际 %d %v", st, body)
	}
	// 标记成 ok → 放行，且审计 Data 里带 check_status。
	markProxyCheck(t, aOn.Store, tenantOn, proxyOn, types.ProxyCheckOK)
	st, bind := do(t, "PUT", srvOn.URL+"/api/v1/environments/"+envOn+"/proxy", tokenOn,
		map[string]any{"proxy_id": proxyOn, "url": "/environments"})
	if st != http.StatusOK {
		t.Fatalf("on 时 ok 的代理应放行，实际 %d %v", st, bind)
	}
	// warnings 用 omitempty：没有告警时字段直接不出现（前端按 undefined 处理即可）。
	if got := mustString(t, bind["exit_mode"]); got != "manual" {
		t.Fatalf("绑定后 exit_mode 应为 manual，实际 %q", got)
	}

	// off：unknown 也允许绑定。
	_, srvOff := newTestServer(t, func(c *config.Config) { c.ProxyTestEnabled = false })
	tokenOff, _ := registerAndLogin(t, srvOff.URL, "P3开关Off", "p3off")
	envOff := createEnvForBind(t, srvOff.URL, tokenOff)
	proxyOff := createTestProxy(t, srvOff.URL, tokenOff)
	if st, body := do(t, "PUT", srvOff.URL+"/api/v1/environments/"+envOff+"/proxy", tokenOff,
		map[string]any{"proxy_id": proxyOff, "url": "/environments"}); st != http.StatusOK {
		t.Fatalf("off 时不得因健康状态拒绝绑定，实际 %d %v", st, body)
	}
}

// TestP3ProxyDetailReturnsMaskOnly 详情只给掩码，任何接口都不给明文。
func TestP3ProxyDetailReturnsMaskOnly(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "P3掩码", "p3mask")
	const pass = "Mask-Me-But-Never-Show-Me-77"

	st, p := do(t, "POST", srv.URL+"/api/v1/proxies", token, map[string]any{
		"name": "掩码代理", "protocol": "http", "host": "1.2.3.4", "port": 8080,
		"username": "isp_user", "password": pass, "country": "US", "city": "Los Angeles",
		"type": "isp", "url": "/proxies",
	})
	if st != http.StatusCreated {
		t.Fatalf("建代理失败: %d %v", st, p)
	}
	// 建代理的响应里既没有明文也没有掩码字段之外的东西。
	if _, leaked := p["password"]; leaked {
		t.Fatalf("建代理响应泄露明文: %v", p)
	}
	if got := mustString(t, p["type"]); got != "isp" {
		t.Fatalf("type 未回填: %v", p)
	}
	if got := mustString(t, p["check_status"]); got != types.ProxyCheckUnknown {
		t.Fatalf("新建应为 check_status=unknown，实际 %q", got)
	}
	id := mustString(t, p["id"])

	st, detail := do(t, "GET", srv.URL+"/api/v1/proxies/"+id, token, nil)
	if st != http.StatusOK {
		t.Fatalf("取详情失败: %d %v", st, detail)
	}
	raw := string(mustMarshal(t, detail))
	if strings.Contains(raw, pass) {
		t.Fatalf("详情响应泄露明文口令: %s", raw)
	}
	if got := mustString(t, detail["password_mask"]); got == "" || got == pass {
		t.Fatalf("详情应带掩码且不等于明文，实际 %q", got)
	}
	if ok, _ := detail["has_password"].(bool); !ok {
		t.Fatal("已设口令却回 has_password=false")
	}
	if got := mustString(t, detail["city"]); got != "Los Angeles" {
		t.Fatalf("city 未回填: %v", detail)
	}
	// 列表不带掩码（列表要为 N 条做 N 次解密不值得）。
	st, list := do(t, "GET", srv.URL+"/api/v1/proxies?limit=50", token, nil)
	if st != http.StatusOK {
		t.Fatalf("列表失败: %d %v", st, list)
	}
	if rows, _ := list["items"].([]any); len(rows) == 1 {
		if _, hasMask := mustJSON(t, rows[0])["password_mask"]; hasMask {
			t.Fatal("列表不该带 password_mask")
		}
	}
}

// TestP3ProxyTestSkippedFlagWhenOff off 时必须回 skipped=true，而不是光秃秃的 ok=false。
func TestP3ProxyTestSkippedFlagWhenOff(t *testing.T) {
	_, srv := newTestServer(t, func(c *config.Config) {
		c.ProxyTestEnabled = false
		c.ProxyTestTarget = "http://127.0.0.1:9/ip"
	})
	token, _ := registerAndLogin(t, srv.URL, "P3跳过", "p3skip")
	id := createTestProxy(t, srv.URL, token)

	st, res := do(t, "POST", srv.URL+"/api/v1/proxies/"+id+"/test", token,
		map[string]any{"timeout_ms": 1000})
	if st != http.StatusOK {
		t.Fatalf("拨测应 200: %d %v", st, res)
	}
	if skipped, _ := res["skipped"].(bool); !skipped {
		t.Fatalf("off 时必须回 skipped=true: %v", res)
	}
	// off 不能把「没测过」记成「测过且失败」。
	_, p := do(t, "GET", srv.URL+"/api/v1/proxies/"+id, token, nil)
	if got := mustString(t, p["check_status"]); got != types.ProxyCheckUnknown {
		t.Fatalf("off 时 check_status 应保持 unknown（不是 fail），实际 %q", got)
	}
}

// ---------- 小工具 ----------

func createEnvForBind(t *testing.T, srv, token string) string {
	t.Helper()
	st, e := do(t, "POST", srv+"/api/v1/environments", token, map[string]any{
		"name": "绑定用店铺", "platform": "Amazon", "country": "US", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d %v", st, e)
	}
	return envViewID(t, e)
}

// markProxyCheck 直接按仓储契约造出「已拨测」的事实（测试里没有真实上游可用）。
func markProxyCheck(t *testing.T, st store.Store, tenant, id, status string) {
	t.Helper()
	if err := st.Proxies().SaveCheckResult(store.WithTenant(context.Background(), tenant),
		id, store.ProxyCheckResult{
			CheckStatus: status, ExitIP: "1.2.3.4", ExitCountry: "US",
			LatencyMS: 42, CheckedAt: time.Now().UTC(),
		}); err != nil {
		t.Fatalf("标记拨测状态失败: %v", err)
	}
}
