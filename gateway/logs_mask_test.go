package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestMaskMapSensitiveKeys 覆盖 include_detail=1 的凭据键清单：
// 代理用户名/口令、token、cookie、authorization、secret、私钥及常见变体。
func TestMaskMapSensitiveKeys(t *testing.T) {
	sensitive := map[string]any{
		"password": "p", "passwd": "p", "pwd": "p",
		"proxy_username": "u", "user_name": "u", "login": "u",
		"access_token": "t", "refresh_token": "t", "client_secret": "s",
		"credential": "s", "api_key": "k", "app_key": "k",
		"private_key": "k", "private-key": "k", "privatekey": "k",
		"cookie": "c", "set_cookie": "c", "authorization": "a",
		"proxy_authorization": "a", "bearer": "b", "session": "s",
	}
	if out := maskMap(sensitive); len(out) != 0 {
		t.Fatalf("所有敏感键都应被剔除，残留: %v", out)
	}
	// 非敏感字段必须保留，不能一刀切把 detail 清空。
	kept := maskMap(map[string]any{
		"shop_id": "sp-1", "action": "env.created", "country": "US",
		"password": "p@ss",
	})
	if len(kept) != 3 {
		t.Fatalf("非敏感键应保留 3 个: %v", kept)
	}
}

// TestMaskMapNested 嵌套对象/数组里的凭据键同样剔除；URL 里的 userinfo 整段打码。
func TestMaskMapNested(t *testing.T) {
	raw := json.RawMessage(`{"proxy_id":"px-1","upstream":{"password":"p@ss","username":"u01"},` +
		`"headers":[{"authorization":"Basic xx"},{"x-country":"US"}],` +
		`"url":"https://u:p@host/path","note":"正常备注"}`)
	got := maskDetail(raw)
	if got["proxy_id"] != "px-1" || got["note"] != "正常备注" {
		t.Fatalf("普通字段应原样保留: %v", got)
	}
	if up, _ := got["upstream"].(map[string]any); len(up) != 0 {
		t.Fatalf("嵌套凭据键应被剔除: %v", up)
	}
	hs, _ := got["headers"].([]any)
	if len(hs) != 2 {
		t.Fatalf("headers 数组应保留 2 项: %v", hs)
	}
	if h0, _ := hs[0].(map[string]any); len(h0) != 0 {
		t.Fatalf("数组内凭据键应被剔除: %v", h0)
	}
	if got["url"] != "[redacted]" {
		t.Fatalf("URL userinfo 应打码: %v", got["url"])
	}
}

func TestMaskDetailInvalidJSON(t *testing.T) {
	if got := maskDetail(json.RawMessage(`not-json`)); got != nil {
		t.Fatalf("解析失败应整段丢弃: %v", got)
	}
	if got := maskDetail(nil); got != nil {
		t.Fatalf("空 detail 应返回 nil: %v", got)
	}
}

// TestMaskMapEgressWhitelist egress 源也走同一个 maskMap（三源统一）。
func TestMaskMapEgressWhitelist(t *testing.T) {
	got := maskMap(map[string]any{
		"provider": "isp", "exit_addr": "1.2.3.4", "password": "p",
	})
	want := map[string]any{"provider": "isp", "exit_addr": "1.2.3.4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("egress 白名单过遮蔽后应为 %v，实际 %v", want, got)
	}
}
