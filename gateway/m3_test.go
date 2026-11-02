package gateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ---------- M3：启停生命周期 ----------

// TestM3StartStopPermission 启停是「动别人的窗口」级别的操作：
// env:launch / env:stop 缺一个都不行；只读角色一律 403。
func TestM3StartStopPermission(t *testing.T) {
	_, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "M3启停团队", "m3ssowner")
	st, body := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "启停店", "country": "US", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d %v", st, body)
	}
	envID := mustString(t, body["env_id"])

	finance := inviteAndGrant(t, srv.URL, tenantID, token, "m3ssfinance", "finance")
	operator := inviteAndGrant(t, srv.URL, tenantID, token, "m3ssoperator", "operator")

	for _, path := range []string{"start", "stop"} {
		st, body := do(t, "POST", srv.URL+"/api/v1/environments/"+envID+"/"+path, finance, nil)
		if st != http.StatusForbidden {
			t.Fatalf("finance 调 %s 应 403，实际 %d %v", path, st, body)
		}
	}
	// operator 有 env:launch/env:stop，权限层要放行（后面失败只能是本机原因，不是 403）。
	st, body = do(t, "POST", srv.URL+"/api/v1/environments/"+envID+"/start", operator, nil)
	if st == http.StatusForbidden || st == http.StatusUnauthorized {
		t.Fatalf("operator 有 env:launch，不应被权限层拒绝: %d %v", st, body)
	}
	// 随便一个不存在的环境：404（不是 500）。
	st, _ = do(t, "POST", srv.URL+"/api/v1/environments/not-exist/start", token, nil)
	if st != http.StatusNotFound {
		t.Fatalf("不存在的环境应 404，实际 %d", st)
	}
}

// TestM3StartResponseHasNoCredential 启动响应**不得**出现任何出口凭据字段（M3/T8）。
//
// 这条是硬红线：启动响应会进浏览器开发者工具、进日志、被截图发群里，
// 一旦含口令就等于把出口账号抄送出去。
func TestM3StartResponseHasNoCredential(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M3凭据团队", "m3credowner")
	st, body := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "凭据店", "country": "US", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d %v", st, body)
	}
	envID := mustString(t, body["env_id"])

	st, startBody := do(t, "POST", srv.URL+"/api/v1/environments/"+envID+"/start", token, nil)
	if st == http.StatusOK {
		for _, k := range []string{"password", "upstream", "proxy_password"} {
			if _, ok := startBody[k]; ok {
				t.Fatalf("启动响应含凭据字段 %s: %v", k, startBody)
			}
		}
	}
	// 本机运行时视图同样不含凭据。
	st, rtBody := do(t, "GET", srv.URL+"/api/v1/environments/local-runtime", token, nil)
	if st != http.StatusOK {
		t.Fatalf("本机运行时应 200: %d %v", st, rtBody)
	}
	raw, _ := json.Marshal(rtBody)
	if strings.Contains(string(raw), "password") {
		t.Fatalf("本机运行时不应出现 password 字段: %s", raw)
	}
	if _, ok := rtBody["process_state"]; !ok {
		t.Fatalf("本机运行时缺少 process_state: %v", rtBody)
	}
}

// ---------- M3/T7：本机 MITM 根证书 ----------

// TestM3CAEndpointsPermission 装/删 CA 改的是本机当前用户信任库，只有 ca:manage
// （owner / ops_manager）能做；只读状态登录即可看（启动按钮的可用性取决于它）。
//
// 这里**不**真的执行安装：那会往跑测试的机器信任库里塞证书。装/删的状态机由
// browser/ca_test.go 用假实现覆盖，真实安装留到 M3 手动闭环验证。
func TestM3CAEndpointsPermission(t *testing.T) {
	_, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "M3CA团队", "m3caowner")
	finance := inviteAndGrant(t, srv.URL, tenantID, token, "m3cafinance", "finance")
	operator := inviteAndGrant(t, srv.URL, tenantID, token, "m3caoperator", "operator")

	for name, tk := range map[string]string{"finance": finance, "operator": operator} {
		for _, path := range []string{"/api/v1/system/ca/install", "/api/v1/system/ca/uninstall"} {
			st, body := do(t, "POST", srv.URL+path, tk, nil)
			if st != http.StatusForbidden {
				t.Fatalf("%s 调用 %s 应 403，实际 %d %v", name, path, st, body)
			}
		}
	}

	// 只读状态：任何人登录都能看，且字段齐全（前端靠 present/ready/detail 画卡片）。
	for _, tk := range []string{token, finance, operator} {
		st, body := do(t, "GET", srv.URL+"/api/v1/system/ca", tk, nil)
		if st != http.StatusOK {
			t.Fatalf("CA 状态应 200，实际 %d %v", st, body)
		}
		for _, k := range []string{"present", "installed", "ready", "cert_path"} {
			if _, ok := body[k]; !ok {
				t.Fatalf("CA 状态缺少字段 %s: %v", k, body)
			}
		}
	}
}
