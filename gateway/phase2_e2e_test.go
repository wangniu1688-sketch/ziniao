package gateway_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// httpGetRaw 取原始响应体（导出是 CSV，不是 JSON）。
func httpGetRaw(t *testing.T, url, token string) string {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPhase2ApprovalAndExport 端到端：提交审批 → 他人审批 → 凭证明文一次有效 →
// 重复被拒 → 审计导出带水印 → 批量导入部分成功（207）。
func TestPhase2ApprovalAndExport(t *testing.T) {
	a, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "审批团队", "appr_owner")

	// 建店
	st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{"name": "审批演示店", "platform": "Amazon", "site": "US"})
	if st != http.StatusCreated {
		t.Fatalf("建店失败: %d %v", st, body)
	}
	shopID, _ := body["id"].(string)

	// 邀请运营成员（审批人）并授权
	st, body = do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members", token, map[string]any{
		"username": "appr_lead", "password": testPassword, "nickname": "审批主管",
	})
	if st != http.StatusCreated {
		t.Fatalf("邀请成员失败: %d %v", st, body)
	}
	leadID, _ := body["id"].(string)
	if st, body = do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members/"+leadID+"/roles", token,
		// 审批主管必须名副其实：普通运营（operator）按路由规则无权批高危单据
		map[string]any{"role": "ops_manager"}); st != http.StatusOK {
		t.Fatalf("授权失败: %d %v", st, body)
	}
	st, body = do(t, "POST", srv.URL+"/api/v1/auth/login", "", map[string]any{
		"username": "appr_lead", "password": testPassword,
	})
	if st != http.StatusOK {
		t.Fatalf("审批人登录失败: %d %v", st, body)
	}
	leadToken, _ := body["token"].(string)

	// 1) 写入凭证
	if st, _ = do(t, "PUT", srv.URL+"/api/v1/shops/"+shopID+"/credentials", token, map[string]any{
		"kind": "password", "username": "seller1", "secret": "ApprSecret#2026", "url": "/shops",
	}); st != http.StatusOK {
		t.Fatalf("凭证写入失败: %d", st)
	}

	// 2) 未提交审批直接看明文 → 被拒（反例 8：不是灰按钮，是服务端硬拦）
	st, body = do(t, "GET", srv.URL+"/api/v1/shops/"+shopID+"/credentials/password", token, nil)
	if st == http.StatusOK {
		t.Fatalf("未审批不应看到明文: %v", body)
	}
	if body["code"] != "approval_required" {
		t.Fatalf("错误码应为 approval_required，实际 %v", body)
	}

	// 3) 提交审批单
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals", token, map[string]any{
		"type": "shop.credential.reveal", "subject_type": "shop", "subject_id": shopID,
		"reason": "排查登录异常", "url": "/approvals",
	})
	if st != http.StatusCreated {
		t.Fatalf("提交审批单失败: %d %v", st, body)
	}
	ticketID, _ := body["id"].(string)
	if ticketID == "" || body["status"] != "pending" {
		t.Fatalf("审批单应处于 pending: %v", body)
	}

	// 4) 自己审批自己 → 被拒（L3 职责分离）
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", token, map[string]any{"comment": "我自己批"})
	if st == http.StatusOK {
		t.Fatalf("自己审批自己不应通过: %v", body)
	}

	// 5) 他人审批 → 通过
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", leadToken, map[string]any{"comment": "同意"})
	if st != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("他人审批应通过: %d %v", st, body)
	}

	// 6) 凭审批单查看明文 → 成功
	st, body = do(t, "GET", srv.URL+"/api/v1/shops/"+shopID+"/credentials/password?ticket="+ticketID, token, nil)
	if st != http.StatusOK || body["secret"] != "ApprSecret#2026" {
		t.Fatalf("持审批单应看到明文: %d %v", st, body)
	}
	// 7) 同一张单再用一次 → 被拒（一次性核销）
	if st, body = do(t, "GET", srv.URL+"/api/v1/shops/"+shopID+"/credentials/password?ticket="+ticketID, token, nil); st == http.StatusOK {
		t.Fatalf("审批单不应重复使用: %v", body)
	}

	// 8) 审计导出：需审批单 + 带水印
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals", token, map[string]any{
		"type": "audit.export.plain", "subject_type": "audit", "reason": "合规检查", "url": "/approvals",
	})
	if st != http.StatusCreated {
		t.Fatalf("提交导出审批单失败: %d %v", st, body)
	}
	exportTicket, _ := body["id"].(string)
	// 明文导出全量审计日志是最高危：运营主管也不够，且申请人自己不能批，
	// 所以需要另一位老板。
	st, body = do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members", token, map[string]any{
		"username": "appr_boss2", "password": testPassword, "nickname": "第二位老板",
	})
	if st != http.StatusCreated {
		t.Fatalf("邀请第二老板失败: %d %v", st, body)
	}
	boss2ID, _ := body["id"].(string)
	if st, body = do(t, "POST", srv.URL+"/api/v1/tenants/"+tenantID+"/members/"+boss2ID+"/roles", token,
		map[string]any{"role": "owner"}); st != http.StatusOK {
		t.Fatalf("第二老板授权失败: %d %v", st, body)
	}
	st, body = do(t, "POST", srv.URL+"/api/v1/auth/login", "", map[string]any{
		"username": "appr_boss2", "password": testPassword,
	})
	if st != http.StatusOK {
		t.Fatalf("第二老板登录失败: %d %v", st, body)
	}
	boss2Token, _ := body["token"].(string)
	if st, _ = do(t, "POST", srv.URL+"/api/v1/approvals/"+exportTicket+"/approve", leadToken, map[string]any{}); st == http.StatusOK {
		t.Fatal("运营主管不应能批准明文导出全量审计日志（覆盖全团队操作记录）")
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/approvals/"+exportTicket+"/approve", boss2Token, map[string]any{}); st != http.StatusOK {
		t.Fatalf("导出审批失败: %d", st)
	}
	raw := httpGetRaw(t, srv.URL+"/api/v1/audit/logs/export?ticket="+exportTicket, token)
	if !strings.Contains(raw, "# 本文件为审计日志导出") || !strings.Contains(raw, "审批单="+exportTicket) {
		t.Fatalf("导出内容必须带水印（导出人/审批单/时间），实际: %s", raw[:min(len(raw), 300)])
	}

	// 9) 批量导入：部分成功返回 207
	st, body = do(t, "POST", srv.URL+"/api/v1/shops/batch/import", token, map[string]any{
		"rows": []any{
			map[string]any{"name": "批量店1", "platform": "Amazon", "site": "US"},
			map[string]any{"name": "批量店2", "platform": "Shopee", "site": "MY"},
			map[string]any{"name": "", "platform": "Amazon", "site": "US"}, // 空名称失败
		},
		"url": "/shops",
	})
	if st != http.StatusMultiStatus {
		t.Fatalf("部分成功应返回 207，实际 %d %v", st, body)
	}
	if body["total"] != float64(3) || body["created"] != float64(2) || body["failed"] != float64(1) {
		t.Fatalf("批量导入结果异常: %v", body)
	}

	// 10) 定时任务可手动触发
	st, body = do(t, "POST", srv.URL+"/api/v1/jobs/run", token, nil)
	if st != http.StatusOK {
		t.Fatalf("手动触发任务失败: %d %v", st, body)
	}
	if _, ok := body["results"]; !ok {
		t.Fatalf("任务执行结果缺失: %v", body)
	}

	// 11) 所有写操作落审计（L5）
	if _, err := a.DispatchOnce(ctxBackground()); err != nil {
		t.Fatal(err)
	}
	st, logs := do(t, "GET", srv.URL+"/api/v1/audit/logs?limit=100", token, nil)
	if st != http.StatusOK {
		t.Fatalf("审计查询失败: %d", st)
	}
	items, _ := logs["items"].([]any)
	actions := map[string]bool{}
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if v, ok := m["action"].(string); ok {
				actions[v] = true
			}
		}
	}
	for _, want := range []string{"approval.create", "approval.approve", "shop.credential.reveal", "shop.batch_import"} {
		if !actions[want] {
			t.Fatalf("审计缺少 %s，实际: %v", want, actions)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
