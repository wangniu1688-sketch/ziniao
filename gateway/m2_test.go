package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// M2 启动器内核测试。
//
// 覆盖顺序按「验收清单」排：代理台账 → 分组 → 环境聚合 → 运行台账 → 日志聚合 → 跨租户隔离。
// 用 mem 驱动跑是因为这些用例断言的是**服务层与网关的契约**，
// 驱动无关；跨租户隔离在 mem 侧由 tenant_id 谓词保证，pg 侧另由 RLS 保证
// （RLS 策略正文见 migrations/0012_launcher_core.up.sql，pg 用例在 store/pg 下带 DSN 时执行）。

// ---------- 代理台账 ----------

// TestM2ProxyLedger 代理台账：建 → 列表不含口令 → 改口令 → 被引用删不掉 → 解绑后可删。
func TestM2ProxyLedger(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M2代理团队", "m2owner1")

	st, body := do(t, "POST", srv.URL+"/api/v1/proxies", token, map[string]any{
		"name": "美西静态-01", "protocol": "socks5", "host": "1.2.3.4", "port": 1080,
		"username": "u01", "password": "p@ssw0rd", "country": "US", "url": "/proxies",
	})
	if st != http.StatusCreated {
		t.Fatalf("建代理失败: %d %v", st, body)
	}
	proxyID, _ := body["id"].(string)
	if proxyID == "" {
		t.Fatalf("缺少 id: %v", body)
	}
	// 口令只以布尔出现：下面是全模块最重要的断言，一旦有人往视图里塞了
	// password / password_enc，这个测试必须红。
	if _, leaked := body["password"]; leaked {
		t.Fatalf("建代理响应泄露明文字段: %v", body)
	}
	if got, _ := body["has_password"].(bool); !got {
		t.Fatalf("has_password 应为 true: %v", body)
	}

	st, list := do(t, "GET", srv.URL+"/api/v1/proxies?limit=50", token, nil)
	if st != http.StatusOK {
		t.Fatalf("代理列表失败: %d %v", st, list)
	}
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("代理列表应含 1 条: %v", list)
	}
	raw := mustJSON(t, items[0])
	for _, k := range []string{"password", "password_enc", "credential"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("列表响应出现凭据字段 %s: %v", k, raw)
		}
	}

	// 改口令（三态之「重设」）
	st, up := do(t, "PUT", srv.URL+"/api/v1/proxies/"+proxyID, token, map[string]any{
		"password": "new-pass", "url": "/proxies",
	})
	if st != http.StatusOK {
		t.Fatalf("改口令失败: %d %v", st, up)
	}
	if _, leaked := up["password"]; leaked {
		t.Fatalf("改代理响应泄露明文: %v", up)
	}
	// 清空口令
	st, up = do(t, "PUT", srv.URL+"/api/v1/proxies/"+proxyID, token, map[string]any{
		"password": "", "url": "/proxies",
	})
	if st != http.StatusOK {
		t.Fatalf("清口令失败: %d %v", st, up)
	}
	if got, _ := up["has_password"].(bool); got {
		t.Fatalf("清空后 has_password 应为 false: %v", up)
	}
	// 停用的代理不能绑到新环境
	st, _ = do(t, "PUT", srv.URL+"/api/v1/proxies/"+proxyID, token, map[string]any{
		"status": "disabled", "url": "/proxies",
	})
	if st != http.StatusOK {
		t.Fatalf("停用代理失败: %d", st)
	}
	st, body = do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "亚马逊A店", "country": "US", "platform": "Amazon",
		"proxy_id": proxyID, "url": "/environments",
	})
	if st != http.StatusBadRequest {
		t.Fatalf("停用代理应拒绝绑定，实际 %d %v", st, body)
	}
}

// TestM2ProxyDeleteBlocked 被环境引用的代理不能删除（409）。
func TestM2ProxyDeleteBlocked(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M2删除校验", "m2owner2")

	st, p := do(t, "POST", srv.URL+"/api/v1/proxies", token, map[string]any{
		"name": "静态-02", "protocol": "http", "host": "5.6.7.8", "port": 8080, "url": "/proxies",
	})
	if st != http.StatusCreated {
		t.Fatalf("建代理失败: %d %v", st, p)
	}
	proxyID, _ := p["id"].(string)

	st, env := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "Shopee店", "country": "GB", "platform": "Shopee", "proxy_id": proxyID,
		"url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d %v", st, env)
	}
	envID := envViewID(t, env)

	st, del := do(t, "DELETE", srv.URL+"/api/v1/proxies/"+proxyID, token, nil)
	if st != http.StatusConflict {
		t.Fatalf("被引用代理应 409，实际 %d %v", st, del)
	}
	// 解绑后可以删
	st, _ = do(t, "PUT", srv.URL+"/api/v1/environments/"+envID+"/proxy", token, map[string]any{
		"proxy_id": "", "url": "/environments",
	})
	if st != http.StatusOK {
		t.Fatalf("解绑失败: %d", st)
	}
	st, del = do(t, "DELETE", srv.URL+"/api/v1/proxies/"+proxyID, token, nil)
	if st != http.StatusOK {
		t.Fatalf("解绑后应可删除，实际 %d %v", st, del)
	}
}

// ---------- 分组 ----------

// TestM2GroupRenameAndDelete 分组：建 → 环境入组 → 改名同步店铺 group_name → 非空 409 → 空可删。
func TestM2GroupRenameAndDelete(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M2分组团队", "m2owner3")

	st, g := do(t, "POST", srv.URL+"/api/v1/groups", token, map[string]any{
		"name": "美西组", "sort": 1, "url": "/groups",
	})
	if st != http.StatusCreated {
		t.Fatalf("建分组失败: %d %v", st, g)
	}
	groupID, _ := g["id"].(string)

	st, env := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "亚马逊B店", "country": "US", "platform": "Amazon", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d %v", st, env)
	}
	envID := envViewID(t, env)

	// 环境移动分组
	st, mv := do(t, "PUT", srv.URL+"/api/v1/environments/"+envID+"/group", token, map[string]any{
		"group_id": groupID, "url": "/environments",
	})
	if st != http.StatusOK {
		t.Fatalf("移动分组失败: %d %v", st, mv)
	}
	if name, _ := mv["group_name"].(string); name != "美西组" {
		t.Fatalf("移动后分组名不对: %v", mv)
	}

	// 改名 → 成员店铺的 group_name 必须同步（否则列表筛得出、卡片显示旧名）
	st, up := do(t, "PUT", srv.URL+"/api/v1/groups/"+groupID, token, map[string]any{
		"name": "美西主力组", "url": "/groups",
	})
	if st != http.StatusOK {
		t.Fatalf("改名失败: %d %v", st, up)
	}
	st, view := do(t, "GET", srv.URL+"/api/v1/environments/"+envID, token, nil)
	if st != http.StatusOK {
		t.Fatalf("查环境失败: %d %v", st, view)
	}
	if name, _ := view["group_name"].(string); name != "美西主力组" {
		t.Fatalf("改名未同步到店铺: %v", view)
	}

	// 非空分组不能删
	st, del := do(t, "DELETE", srv.URL+"/api/v1/groups/"+groupID, token, nil)
	if st != http.StatusConflict {
		t.Fatalf("非空分组应 409，实际 %d %v", st, del)
	}
	// 移出分组后可删
	st, _ = do(t, "PUT", srv.URL+"/api/v1/environments/"+envID+"/group", token, map[string]any{
		"group_id": "", "url": "/environments",
	})
	if st != http.StatusOK {
		t.Fatalf("移出分组失败: %d", st)
	}
	st, del = do(t, "DELETE", srv.URL+"/api/v1/groups/"+groupID, token, nil)
	if st != http.StatusOK {
		t.Fatalf("空分组应可删，实际 %d %v", st, del)
	}
}

// ---------- 环境聚合视图 ----------

// TestM2EnvironmentAggregate 环境聚合：建 → 列表/过滤 → 详情 → 绑代理 → 解绑回落。
func TestM2EnvironmentAggregate(t *testing.T) {
	a, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "M2环境团队", "m2owner4")

	st, g := do(t, "POST", srv.URL+"/api/v1/groups", token, map[string]any{"name": "欧洲组", "url": "/groups"})
	if st != http.StatusCreated {
		t.Fatalf("建分组失败: %d %v", st, g)
	}
	groupID, _ := g["id"].(string)

	st, e1 := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "亚马逊德站", "platform": "Amazon", "site": "DE", "country": "DE",
		"code": "AMZ-DE-01", "group_id": groupID, "start_url": "https://sellercentral.amazon.de",
		"fingerprint_mode": "auto", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d %v", st, e1)
	}
	if _, leaked := e1["inject_script"]; leaked {
		t.Fatalf("建环境响应泄露注入脚本: %v", e1)
	}
	envID := envViewID(t, e1)
	// 聚合创建必须同时有店铺与环境（原子性）
	if s, ok := e1["shop"].(map[string]any); !ok || s["id"] == "" {
		t.Fatalf("聚合创建缺少店铺: %v", e1)
	}

	st, e2 := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "英站二店", "platform": "Shopee", "country": "GB", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建第二个环境失败: %d %v", st, e2)
	}

	// 列表：总数 2
	st, list := do(t, "GET", srv.URL+"/api/v1/environments?limit=50", token, nil)
	if st != http.StatusOK {
		t.Fatalf("环境列表失败: %d %v", st, list)
	}
	if total := intOf(list["total"]); total != 2 {
		t.Fatalf("总数应为 2，实际 %v", list["total"])
	}
	rows, _ := list["items"].([]any)
	for _, r := range rows {
		m := mustJSON(t, r)
		if _, bad := m["inject_script"]; bad {
			t.Fatalf("聚合列表泄露注入脚本正文: %v", m)
		}
		if _, bad := m["password"]; bad {
			t.Fatalf("聚合列表泄露凭据: %v", m)
		}
	}

	// 过滤：按分组
	st, byGroup := do(t, "GET", srv.URL+"/api/v1/environments?group_id="+groupID, token, nil)
	if st != http.StatusOK {
		t.Fatalf("按分组过滤失败: %d %v", st, byGroup)
	}
	if total := intOf(byGroup["total"]); total != 1 {
		t.Fatalf("按分组过滤应为 1 条，实际 %v", byGroup["total"])
	}
	// 过滤：按平台
	st, byPlat := do(t, "GET", srv.URL+"/api/v1/environments?platform=Shopee", token, nil)
	if st != http.StatusOK {
		t.Fatalf("按平台过滤失败: %d %v", st, byPlat)
	}
	if total := intOf(byPlat["total"]); total != 1 {
		t.Fatalf("按平台过滤应为 1 条，实际 %v", byPlat["total"])
	}
	// 过滤：关键词（店铺名 / 编号）
	st, byKw := do(t, "GET", srv.URL+"/api/v1/environments?keyword=AMZ-DE-01", token, nil)
	if st != http.StatusOK {
		t.Fatalf("关键词过滤失败: %d %v", st, byKw)
	}
	if total := intOf(byKw["total"]); total != 1 {
		t.Fatalf("按编号过滤应为 1 条，实际 %v", byKw["total"])
	}
	// 过滤：不存在的分组 → 0 条（不是全量）
	st, none := do(t, "GET", srv.URL+"/api/v1/environments?group_id=not-exist", token, nil)
	if st != http.StatusOK {
		t.Fatalf("无效分组过滤失败: %d %v", st, none)
	}
	if total := intOf(none["total"]); total != 0 {
		t.Fatalf("不存在的分组应返回 0 条，实际 %v", none["total"])
	}

	// 绑定代理
	st, p := do(t, "POST", srv.URL+"/api/v1/proxies", token, map[string]any{
		"name": "德静态-01", "protocol": "http", "host": "9.9.9.9", "port": 3128, "url": "/proxies",
	})
	if st != http.StatusCreated {
		t.Fatalf("建代理失败: %d %v", st, p)
	}
	proxyID, _ := p["id"].(string)
	// P3：绑定前必须拨测 ok。刚建的代理是 unknown，直接绑必须被拒——
	// 「没验证过」不是「可以先用着」，放行等于让这条校验形同虚设。
	st, denied := do(t, "PUT", srv.URL+"/api/v1/environments/"+envID+"/proxy", token, map[string]any{
		"proxy_id": proxyID, "url": "/environments",
	})
	if st != http.StatusBadRequest {
		t.Fatalf("未拨测的代理应拒绝绑定（400），实际 %d %v", st, denied)
	}
	// 造出「已拨测通过」的事实：真实路径是 POST /proxies/{id}/test 打真实上游，
	// 这里没有上游可用，直接按仓储契约落库。
	if err := a.Store.Proxies().SaveCheckResult(store.WithTenant(context.Background(), tenantID),
		proxyID, store.ProxyCheckResult{
			CheckStatus: types.ProxyCheckOK, ExitIP: "9.9.9.9", ExitCountry: "DE",
			LatencyMS: 120, CheckedAt: time.Now().UTC(),
		}); err != nil {
		t.Fatalf("标记拨测通过失败: %v", err)
	}
	st, bind := do(t, "PUT", srv.URL+"/api/v1/environments/"+envID+"/proxy", token, map[string]any{
		"proxy_id": proxyID, "url": "/environments",
	})
	if st != http.StatusOK {
		t.Fatalf("绑定代理失败: %d %v", st, bind)
	}
	if mode, _ := bind["exit_mode"].(string); mode != "manual" {
		t.Fatalf("绑定后 exit_mode 应为 manual: %v", bind)
	}
	// 解绑 → 回落（无代理商池时为 direct）
	st, unbind := do(t, "PUT", srv.URL+"/api/v1/environments/"+envID+"/proxy", token, map[string]any{
		"proxy_id": "", "url": "/environments",
	})
	if st != http.StatusOK {
		t.Fatalf("解绑失败: %d %v", st, unbind)
	}
	if mode, _ := unbind["exit_mode"].(string); mode != "direct" && mode != "pool" {
		t.Fatalf("解绑后 exit_mode 应为 direct/pool: %v", unbind)
	}
	// 不存在的代理 → 404（不能静默写成空）
	st, bad := do(t, "PUT", srv.URL+"/api/v1/environments/"+envID+"/proxy", token, map[string]any{
		"proxy_id": "ghost", "url": "/environments",
	})
	if st != http.StatusNotFound {
		t.Fatalf("绑定不存在的代理应 404，实际 %d %v", st, bad)
	}
}

// TestM2RuntimeLedger 设备与网络只读台账：只给库侧事实，不含进程态。
func TestM2RuntimeLedger(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M2台账团队", "m2owner5")

	st, e := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "德站二店", "platform": "Lazada", "country": "DE", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d %v", st, e)
	}
	st, rt := do(t, "GET", srv.URL+"/api/v1/environments/runtimes?limit=50", token, nil)
	if st != http.StatusOK {
		t.Fatalf("运行台账失败: %d %v", st, rt)
	}
	rows, _ := rt["items"].([]any)
	if len(rows) != 1 {
		t.Fatalf("台账应有 1 条: %v", rt)
	}
	row := mustJSON(t, rows[0])
	// M2 边界：PID / CDP 端口 / 运行时长一律不出现
	for _, k := range []string{"pid", "cdp_port", "uptime_sec", "running_since"} {
		if _, bad := row[k]; bad {
			t.Fatalf("M2 台账不应含进程态字段 %s: %v", k, row)
		}
	}
	if _, ok := row["baseline_hash"]; !ok {
		t.Fatalf("台账缺少基线哈希: %v", row)
	}
}

// ---------- 日志聚合 ----------

// TestM2LogAggregate 日志聚合：三源可查、detail 默认不返回、非法来源被拒。
func TestM2LogAggregate(t *testing.T) {
	a, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M2日志团队", "m2owner6")

	// 造一条审计：建环境会产生 env.created / shop.created
	st, _ := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "TikTok英站", "platform": "TikTok", "country": "GB", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d", st)
	}
	// 事件 → 审计落库是异步派发（写路径不等它），测试里手动推一次
	if _, err := a.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("事件派发失败: %v", err)
	}
	st, logs := do(t, "GET", srv.URL+"/api/v1/logs?limit=50&sources=audit,security", token, nil)
	if st != http.StatusOK {
		t.Fatalf("日志聚合失败: %d %v", st, logs)
	}
	rows, _ := logs["items"].([]any)
	if len(rows) == 0 {
		t.Fatalf("应至少有一条审计日志: %v", logs)
	}
	for _, r := range rows {
		m := mustJSON(t, r)
		if _, bad := m["detail"]; bad {
			t.Fatalf("detail 默认不应返回: %v", m)
		}
		if _, ok := m["source"]; !ok {
			t.Fatalf("日志缺少 source: %v", m)
		}
	}
	// 带 include_detail 时 detail 仍不得含凭据键（精确键清单见 logs_mask_test.go）
	st, withDetail := do(t, "GET", srv.URL+"/api/v1/logs?limit=50&include_detail=1", token, nil)
	if st != http.StatusOK {
		t.Fatalf("带 detail 查询失败: %d %v", st, withDetail)
	}
	detailRows, _ := withDetail["items"].([]any)
	if len(detailRows) == 0 {
		t.Fatalf("带 detail 查询不应丢条目: %v", withDetail)
	}
	for _, r := range detailRows {
		m := mustJSON(t, r)
		d, ok := m["detail"].(map[string]any)
		if !ok {
			continue
		}
		for _, k := range []string{"password", "password_enc", "proxy_password", "username",
			"access_token", "cookie", "authorization", "private_key", "secret"} {
			if _, bad := d[k]; bad {
				t.Fatalf("include_detail 泄露凭据键 %s: %v", k, d)
			}
		}
	}
	// 非法来源
	st, bad := do(t, "GET", srv.URL+"/api/v1/logs?sources=evil", token, nil)
	if st != http.StatusBadRequest {
		t.Fatalf("非法 sources 应 400，实际 %d %v", st, bad)
	}
	// 非法时间格式
	st, badTime := do(t, "GET", srv.URL+"/api/v1/logs?from=昨天", token, nil)
	if st != http.StatusBadRequest {
		t.Fatalf("非法 from 应 400，实际 %d %v", st, badTime)
	}
}

// ---------- 跨租户隔离 ----------

// TestM2CrossTenantIsolation 两个租户互不可见（代理 / 分组 / 环境 / 运行台账）。
func TestM2CrossTenantIsolation(t *testing.T) {
	_, srv := newTestServer(t)
	tokenA, _ := registerAndLogin(t, srv.URL, "租户A", "m2tenantA")
	tokenB, _ := registerAndLogin(t, srv.URL, "租户B", "m2tenantB")

	// A 建代理 / 分组 / 环境
	st, p := do(t, "POST", srv.URL+"/api/v1/proxies", tokenA, map[string]any{
		"name": "A的代理", "protocol": "http", "host": "10.0.0.1", "port": 8080, "url": "/proxies",
	})
	if st != http.StatusCreated {
		t.Fatalf("A 建代理失败: %d %v", st, p)
	}
	proxyID, _ := p["id"].(string)

	st, g := do(t, "POST", srv.URL+"/api/v1/groups", tokenA, map[string]any{"name": "A的分组", "url": "/groups"})
	if st != http.StatusCreated {
		t.Fatalf("A 建分组失败: %d %v", st, g)
	}
	groupID, _ := g["id"].(string)

	st, e := do(t, "POST", srv.URL+"/api/v1/environments", tokenA, map[string]any{
		"name": "A的店铺", "country": "US", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("A 建环境失败: %d %v", st, e)
	}
	envID := envViewID(t, e)
	_, _ = do(t, "PUT", srv.URL+"/api/v1/environments/"+envID+"/proxy", tokenA,
		map[string]any{"proxy_id": proxyID, "url": "/environments"})

	// B 看不到 A 的任何东西
	st, bProxies := do(t, "GET", srv.URL+"/api/v1/proxies?limit=50", tokenB, nil)
	if st != http.StatusOK {
		t.Fatalf("B 查代理失败: %d %v", st, bProxies)
	}
	if rows, _ := bProxies["items"].([]any); len(rows) != 0 {
		t.Fatalf("B 不应看到 A 的代理: %v", bProxies)
	}
	st, bGroups := do(t, "GET", srv.URL+"/api/v1/groups", tokenB, nil)
	if st != http.StatusOK {
		t.Fatalf("B 查分组失败: %d %v", st, bGroups)
	}
	if rows, _ := bGroups["items"].([]any); len(rows) != 0 {
		t.Fatalf("B 不应看到 A 的分组: %v", bGroups)
	}
	st, bEnvs := do(t, "GET", srv.URL+"/api/v1/environments?limit=50", tokenB, nil)
	if st != http.StatusOK {
		t.Fatalf("B 查环境失败: %d %v", st, bEnvs)
	}
	if total := intOf(bEnvs["total"]); total != 0 {
		t.Fatalf("B 不应看到 A 的环境: %v", bEnvs)
	}
	st, bRt := do(t, "GET", srv.URL+"/api/v1/environments/runtimes?limit=50", tokenB, nil)
	if st != http.StatusOK {
		t.Fatalf("B 查台账失败: %d %v", st, bRt)
	}
	if total := intOf(bRt["total"]); total != 0 {
		t.Fatalf("B 的台账应为空: %v", bRt)
	}

	// B 拿 A 的 ID 直接读 / 写，一律 404（不是 403：
	// 「不存在」比「存在但无权」泄露的信息更少，且与服务层口径一致）
	if st, _ := do(t, "GET", srv.URL+"/api/v1/environments/"+envID, tokenB, nil); st != http.StatusNotFound {
		t.Fatalf("B 读 A 的环境应 404，实际 %d", st)
	}
	for _, req := range []struct {
		m, u string
		b    map[string]any
	}{
		{"PUT", "/api/v1/proxies/" + proxyID, map[string]any{"name": "B改的名"}},
		{"PUT", "/api/v1/environments/" + envID + "/proxy", map[string]any{"proxy_id": proxyID}},
		{"PUT", "/api/v1/environments/" + envID + "/group", map[string]any{"group_id": groupID}},
		{"PUT", "/api/v1/groups/" + groupID, map[string]any{"name": "B改的名"}},
		{"DELETE", "/api/v1/proxies/" + proxyID, nil},
	} {
		if st, _ := do(t, req.m, srv.URL+req.u, tokenB, req.b); st != http.StatusNotFound {
			t.Fatalf("B 对 %s %s 应 404，实际 %d", req.m, req.u, st)
		}
	}
}

// ---------- 系统自描述 / 统计 ----------

// TestM2SystemInfoFields 系统设置页依赖的自描述字段必须齐全（M1 补查项）。
func TestM2SystemInfoFields(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M2系统", "m2owner7")
	st, info := do(t, "GET", srv.URL+"/api/v1/system/info", token, nil)
	if st != http.StatusOK {
		t.Fatalf("system/info 失败: %d %v", st, info)
	}
	for _, k := range []string{"product_version", "data_dir", "server_addr", "profile_root",
		"extension_root", "ca_ready", "browser"} {
		if _, ok := info[k]; !ok {
			t.Fatalf("system/info 缺少字段 %s: %v", k, info)
		}
	}
	b, _ := info["browser"].(map[string]any)
	for _, k := range []string{"name", "path", "version"} {
		if _, ok := b[k]; !ok {
			t.Fatalf("browser 段缺少 %s: %v", k, b)
		}
	}
	if s := mustString(t, info["data_dir"]); strings.Contains(s, "ZINIAO") {
		t.Fatalf("自描述不应回显环境变量内容: %v", s)
	}
}

// TestM2SystemStatsFields 首页统计必须给环境口径与最近环境列表（M1 补查项）。
func TestM2SystemStatsFields(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M2统计", "m2owner8")
	st, _ := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "统计用店铺", "country": "US", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d", st)
	}
	st, stats := do(t, "GET", srv.URL+"/api/v1/system/stats", token, nil)
	if st != http.StatusOK {
		t.Fatalf("system/stats 失败: %d %v", st, stats)
	}
	for _, k := range []string{"environment_total", "running", "offline", "recent"} {
		if _, ok := stats[k]; !ok {
			t.Fatalf("system/stats 缺少 %s: %v", k, stats)
		}
	}
	if intOf(stats["environment_total"]) != 1 {
		t.Fatalf("环境总数应为 1: %v", stats)
	}
	recent, _ := stats["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("最近环境应有 1 条: %v", stats)
	}
	row := mustJSON(t, recent[0])
	for _, k := range []string{"pid", "cdp_port"} {
		if _, bad := row[k]; bad {
			t.Fatalf("最近环境不应含进程态 %s: %v", k, row)
		}
	}
}

// ---------- M2 补查 / T12：拨测开关、越权 403、旧接口双写 ----------

// TestM2ProxyTestToggle T12/Q3：拨测出网开关与目标地址可配置。
func TestM2ProxyTestToggle(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("1.2.3.4"))
	}))
	defer target.Close()

	// off：路由仍在，返回明确的「拨测已禁用」，且目标服务一次都没被访问。
	_, srvOff := newTestServer(t, func(c *config.Config) {
		c.ProxyTestEnabled = false
		c.ProxyTestTarget = target.URL
	})
	tokenOff, _ := registerAndLogin(t, srvOff.URL, "M2拨测Off", "m2toggleoff")
	offID := createTestProxy(t, srvOff.URL, tokenOff)
	st, res := do(t, "POST", srvOff.URL+"/api/v1/proxies/"+offID+"/test", tokenOff,
		map[string]any{"timeout_ms": 1000})
	if st != http.StatusOK {
		t.Fatalf("禁用时拨测应 200: %d %v", st, res)
	}
	if ok, _ := res["ok"].(bool); ok {
		t.Fatalf("禁用时 ok 必须为 false: %v", res)
	}
	if msg := mustString(t, res["error"]); !strings.Contains(msg, "拨测已禁用") {
		t.Fatalf("禁用时应给出明确原因: %v", res)
	}
	if mustString(t, res["target"]) != target.URL {
		t.Fatalf("响应 target 应取配置值: %v", res)
	}
	if hits.Load() != 0 {
		t.Fatalf("禁用时不应发出任何请求，实际 %d 次", hits.Load())
	}

	// on（默认）：走到真实拨测，错误原因不再是「拨测已禁用」；目标配置仍生效。
	_, srvOn := newTestServer(t, func(c *config.Config) {
		c.ProxyTestTarget = target.URL
	})
	tokenOn, _ := registerAndLogin(t, srvOn.URL, "M2拨测On", "m2toggleon")
	onID := createTestProxy(t, srvOn.URL, tokenOn)
	st, resOn := do(t, "POST", srvOn.URL+"/api/v1/proxies/"+onID+"/test", tokenOn,
		map[string]any{"timeout_ms": 1000})
	if st != http.StatusOK {
		t.Fatalf("开启时拨测应 200: %d %v", st, resOn)
	}
	if msg := mustString(t, resOn["error"]); strings.Contains(msg, "拨测已禁用") {
		t.Fatalf("默认开启不应被开关拦截: %v", resOn)
	}
	if mustString(t, resOn["target"]) != target.URL {
		t.Fatalf("开启时 target 应取配置值: %v", resOn)
	}
	// 目标协议白名单：非 http/https 不能作为服务端出网入口。
	st, bad := do(t, "POST", srvOn.URL+"/api/v1/proxies/"+onID+"/test", tokenOn,
		map[string]any{"timeout_ms": 1000, "target": "file:///etc/passwd"})
	if st != http.StatusOK {
		t.Fatalf("非法目标应 200 + ok=false: %d %v", st, bad)
	}
	if ok, _ := bad["ok"].(bool); ok {
		t.Fatalf("非法目标不应判成功: %v", bad)
	}
	if msg := mustString(t, bad["error"]); !strings.Contains(msg, "目标地址非法") {
		t.Fatalf("非法目标应给出原因: %v", bad)
	}
}

// TestM2ProxyTestConfigEnv 开关与目标的默认值/环境变量解析（on/off 不能被 ParseBool 吞掉）。
func TestM2ProxyTestConfigEnv(t *testing.T) {
	if d := config.Default(); !d.ProxyTestEnabled || d.ProxyTestTarget != "https://api.ipify.org" {
		t.Fatalf("默认应为 on / https://api.ipify.org: %+v", d)
	}
	t.Setenv("ZINIAO_PROXY_TEST", "off")
	t.Setenv("ZINIAO_PROXY_TEST_TARGET", "http://127.0.0.1:9/ip")
	if c := config.Load(); c.ProxyTestEnabled || c.ProxyTestTarget != "http://127.0.0.1:9/ip" {
		t.Fatalf("off 与目标配置未生效: %+v", c)
	}
	t.Setenv("ZINIAO_PROXY_TEST", "on")
	if c := config.Load(); !c.ProxyTestEnabled {
		t.Fatalf("on 应重新开启: %+v", c)
	}
}

// TestM2PermissionDenied M2 新权限码：默认无角色成员与 finance 不能读代理台账/环境聚合/日志；
// operator 只有只读，写与拨测仍 403。
func TestM2PermissionDenied(t *testing.T) {
	_, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "M2越权团队", "m2permowner")
	// 先造数据：确保低权限拿到的是 403 而不是「恰好空列表」。
	proxyID := createTestProxy(t, srv.URL, token)
	st, _ := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "越权校验店", "country": "US", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d", st)
	}
	finance := inviteAndGrant(t, srv.URL, tenantID, token, "m2finance", "finance")
	operator := inviteAndGrant(t, srv.URL, tenantID, token, "m2operator", "operator")

	// 新邀请但未授角色的成员 = 默认普通成员（无任何功能权限）。
	st, inv := do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members", token, map[string]any{
		"username": "m2norole", "password": testPassword, "nickname": "m2norole",
	})
	if st != http.StatusCreated {
		t.Fatalf("邀请成员失败: %d %v", st, inv)
	}
	st, lg := do(t, "POST", srv.URL+"/api/v1/auth/login", "", map[string]any{
		"username": "m2norole", "password": testPassword,
	})
	noRole := mustString(t, lg["token"])
	if st != http.StatusOK || noRole == "" {
		t.Fatalf("无角色成员登录失败: %d %v", st, lg)
	}

	for _, path := range []string{
		"/api/v1/proxies?limit=10",
		"/api/v1/environments?limit=10",
		"/api/v1/environments/runtimes?limit=10",
		"/api/v1/groups",
		"/api/v1/logs?limit=10",
	} {
		for name, tk := range map[string]string{"finance": finance, "no_role": noRole} {
			st, body := do(t, "GET", srv.URL+path, tk, nil)
			if st != http.StatusForbidden {
				t.Fatalf("%s 访问 %s 应 403，实际 %d %v", name, path, st, body)
			}
		}
	}
	// operator 有 proxy:read / env:read，但无 proxy:manage / group:manage。
	st, body := do(t, "GET", srv.URL+"/api/v1/proxies?limit=10", operator, nil)
	if st != http.StatusOK {
		t.Fatalf("operator 只读代理台账应 200: %d %v", st, body)
	}
	st, body = do(t, "POST", srv.URL+"/api/v1/proxies", operator, map[string]any{
		"name": "越权新建", "protocol": "http", "host": "9.9.9.9", "port": 8080,
	})
	if st != http.StatusForbidden {
		t.Fatalf("operator 新建代理应 403: %d %v", st, body)
	}
	st, body = do(t, "POST", srv.URL+"/api/v1/proxies/"+proxyID+"/test", operator,
		map[string]any{"timeout_ms": 1000})
	if st != http.StatusForbidden {
		t.Fatalf("operator 拨测应 403: %d %v", st, body)
	}
}

// TestM2PagePermLayerDoesNotOverrideResourceCode M2 终审第 2.2 节：
//  1. 缺 page:* 不影响资源码裁决（cs 有 env:read 但无 page:environments ⇒ 仍然 200）；
//  2. 资源码被拒 ⇒ 403（finance 既无码也无页面码）；
//  3. /api/v1/me 的 nav[] 按资源码给出可见性，资源码被拒的项 visible=false。
func TestM2PagePermLayerDoesNotOverrideResourceCode(t *testing.T) {
	_, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "M2双层口径", "m2layerowner")
	st, _ := do(t, "POST", srv.URL+"/api/v1/environments", token, map[string]any{
		"name": "双层口径店", "country": "US", "url": "/environments",
	})
	if st != http.StatusCreated {
		t.Fatalf("建环境失败: %d", st)
	}
	cs := inviteAndGrant(t, srv.URL, tenantID, token, "m2cs", "cs")
	finance := inviteAndGrant(t, srv.URL, tenantID, token, "m2finance2", "finance")

	// 1) cs：env:read 有、page:environments 无 ⇒ 只读接口仍 200（页面码不得反向拦截）。
	st, body := do(t, "GET", srv.URL+"/api/v1/environments?limit=10", cs, nil)
	if st != http.StatusOK {
		t.Fatalf("cs 只读环境应 200（页面码缺失不得拦截），实际 %d %v", st, body)
	}
	// 2) finance：资源码缺失 ⇒ 403，与页面码无关。
	st, body = do(t, "GET", srv.URL+"/api/v1/environments?limit=10", finance, nil)
	if st != http.StatusForbidden {
		t.Fatalf("finance 读环境应 403，实际 %d %v", st, body)
	}

	// 3) nav 可见性随资源码走。
	navOf := func(tk string) map[string]bool {
		st, me := do(t, "GET", srv.URL+"/api/v1/me", tk, nil)
		if st != http.StatusOK {
			t.Fatalf("/api/v1/me 应 200: %d %v", st, me)
		}
		raw, ok := me["nav"].([]any)
		if !ok {
			t.Fatalf("/api/v1/me 缺少 nav[]: %v", me)
		}
		out := map[string]bool{}
		for _, it := range raw {
			m, _ := it.(map[string]any)
			out[mustString(t, m["id"])] = m["visible"] == true
		}
		return out
	}
	fnav := navOf(finance)
	for _, id := range []string{"proxies", "groups", "logs", "devices"} {
		if fnav[id] {
			t.Fatalf("finance 的 %s 导航项应不可见: %+v", id, fnav)
		}
	}
	onav := navOf(token)
	for _, id := range []string{"dashboard", "shops", "proxies", "groups", "logs", "devices", "settings"} {
		if !onav[id] {
			t.Fatalf("owner 的 %s 导航项应可见: %+v", id, onav)
		}
	}
}

// TestM2ShopGroupDoubleWrite 旧接口 PUT /shops/{id}/group 必须双写 group_id/group_name：
// 同名分组要同步 group_id，自由文本组名不得残留旧 group_id。
func TestM2ShopGroupDoubleWrite(t *testing.T) {
	_, srv := newTestServer(t)
	token, _ := registerAndLogin(t, srv.URL, "M2双写团队", "m2dwowner")
	st, sp := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{
		"name": "双写校验店", "platform": "Amazon", "site": "US",
	})
	if st != http.StatusCreated {
		t.Fatalf("建店铺失败: %d %v", st, sp)
	}
	shopID := mustString(t, sp["id"])
	st, g := do(t, "POST", srv.URL+"/api/v1/groups", token, map[string]any{"name": "甲组"})
	if st != http.StatusCreated {
		t.Fatalf("建分组失败: %d %v", st, g)
	}
	groupID := mustString(t, g["id"])

	st, up := do(t, "PUT", srv.URL+"/api/v1/shops/"+shopID+"/group", token,
		map[string]any{"group": "甲组", "tags": []string{"t1"}})
	if st != http.StatusOK {
		t.Fatalf("旧接口设置分组失败: %d %v", st, up)
	}
	if mustString(t, up["group"]) != "甲组" || mustString(t, up["group_id"]) != groupID {
		t.Fatalf("旧接口应双写 group/group_id: %v", up)
	}
	// 改成一个尚不存在的自由文本组名：group_id 必须清空，不能继续指向甲组。
	st, up = do(t, "PUT", srv.URL+"/api/v1/shops/"+shopID+"/group", token,
		map[string]any{"group": "自由文本组"})
	if st != http.StatusOK {
		t.Fatalf("旧接口设置文本分组失败: %d %v", st, up)
	}
	if mustString(t, up["group"]) != "自由文本组" || mustString(t, up["group_id"]) != "" {
		t.Fatalf("自由文本组不得残留旧 group_id: %v", up)
	}
}

// createTestProxy 建一个指向本机不存在端口的代理，用于拨测用例（不会真出网）。
func createTestProxy(t *testing.T, srv, token string) string {
	t.Helper()
	st, p := do(t, "POST", srv+"/api/v1/proxies", token, map[string]any{
		"name": "拨测用代理", "protocol": "http", "host": "127.0.0.1", "port": 1,
	})
	if st != http.StatusCreated {
		t.Fatalf("建代理失败: %d %v", st, p)
	}
	id := mustString(t, p["id"])
	if id == "" {
		t.Fatalf("缺少代理 id: %v", p)
	}
	return id
}

// ---------- 助手 ----------

func mustJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("期望对象，实际 %T %v", v, v)
	}
	return m
}

func mustString(t *testing.T, v any) string {
	t.Helper()
	s, _ := v.(string)
	return s
}

func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

// envViewID 从聚合创建响应里取环境 ID（响应同时带 shop / environment / view）。
func envViewID(t *testing.T, body map[string]any) string {
	t.Helper()
	if v, ok := body["view"].(map[string]any); ok {
		if id, _ := v["id"].(string); id != "" {
			return id
		}
	}
	if e, ok := body["environment"].(map[string]any); ok {
		if id, _ := e["id"].(string); id != "" {
			return id
		}
	}
	if id, _ := body["id"].(string); id != "" {
		return id
	}
	t.Fatalf("响应里取不到环境 ID: %v", body)
	return ""
}
