package gateway_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// getRaw 带令牌取原始响应体（敏感字段扫描必须看原文，不能看解码后的结构）。
func getRaw(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSystemInfoRequiresAuth(t *testing.T) {
	_, srv := newTestServer(t)
	if st, _ := getRaw(t, srv.URL+"/api/v1/system/info", ""); st != http.StatusUnauthorized {
		t.Fatalf("未带令牌应 401，实际 %d", st)
	}
	if st, _ := getRaw(t, srv.URL+"/api/v1/system/stats", ""); st != http.StatusUnauthorized {
		t.Fatalf("未带令牌应 401，实际 %d", st)
	}
}

// TestSystemInfoNoSecrets 是「敏感字段遮蔽」这条红线的自动化证据：
// 自描述接口最容易被当成「配置 dump」越写越多，最后把凭据一起送出去。
func TestSystemInfoNoSecrets(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "自描述团队", "sysinfo_owner")
	st, body := getRaw(t, srv.URL+"/api/v1/system/info", token)
	if st != http.StatusOK {
		t.Fatalf("system/info 应 200，实际 %d: %s", st, body)
	}
	for _, field := range []string{"password", "dsn", "jwt_secret", "ip_token", "smtp_pass", "app_secret"} {
		if strings.Contains(strings.ToLower(body), field) {
			t.Fatalf("system/info 响应出现敏感字段 %q: %s", field, body)
		}
	}
	for _, want := range []string{`"ca_ready"`, `"capabilities"`, `"installed"`, `"listen"`, `"profiles"`, `"uptime_sec"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("system/info 缺少字段 %s: %s", want, body)
		}
	}
}

func TestSystemStatsShape(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "统计团队", "stats_owner")
	st, body := do(t, http.MethodGet, srv.URL+"/api/v1/system/stats", token, nil)
	if st != http.StatusOK {
		t.Fatalf("system/stats 应 200，实际 %d: %v", st, body)
	}
	for _, k := range []string{"shops", "devices", "proxies", "groups", "approvals", "generated_at"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("system/stats 缺少 %s: %v", k, body)
		}
	}
	shops, ok := body["shops"].(map[string]any)
	if !ok {
		t.Fatalf("shops 段类型不符: %v", body["shops"])
	}
	// 新租户还没建店铺：总数为 0，但字段必须存在（前端直接读 total，缺字段会显示 undefined）。
	if _, ok := shops["total"]; !ok {
		t.Fatalf("shops.total 缺失: %v", shops)
	}
	if _, ok := shops["running_envs"]; !ok {
		t.Fatalf("shops.running_envs 缺失: %v", shops)
	}
}
