package gateway_test

import (
	"net/http"
	"testing"
)

// TestApprovalRouteRulesConfigurable 审批路由规则可配置（迁移 0010）：
// 运营改「谁有资格批这张单」不必发版，且改完立刻生效在真实审批链路上。
//
// 这条同时锁住权限面：规则本身是控制变更，普通运营既看不了也改不了——
// 否则他可以把「批量删除需管理员」调成「谁都能批」，审批就成了可绕过的门。
func TestApprovalRouteRulesConfigurable(t *testing.T) {
	_, srv := newTestServer(t)
	owner, tenantID := registerAndLogin(t, srv.URL, "规则团队", "rule_owner")
	lead := inviteAndGrant(t, srv.URL, tenantID, owner, "rule_lead", "ops_manager")
	staff := inviteAndGrant(t, srv.URL, tenantID, owner, "rule_staff", "operator")

	// 1) 初始状态：无租户规则，接口要同时给出内置规则（否则运营不知道删掉会变成什么）
	st, body := do(t, "GET", srv.URL+"/api/v1/approvals/rules", owner, nil)
	if st != http.StatusOK {
		t.Fatalf("老板读规则失败: %d %v", st, body)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("新租户不应有自定义规则: %v", items)
	}
	if builtin, _ := body["builtin"].([]any); len(builtin) == 0 {
		t.Fatal("必须同时返回内置规则：运营需要知道「没配置时走什么」")
	}

	// 2) 普通运营既不能看也不能改
	if st, _ = do(t, "GET", srv.URL+"/api/v1/approvals/rules", staff, nil); st != http.StatusForbidden {
		t.Fatalf("普通运营读规则应 403，实际 %d", st)
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/approvals/rules", staff, map[string]any{
		"ticket_type": "shop.bulk_delete", "roles": []string{"operator"}, "min_approvers": 1,
	}); st != http.StatusForbidden {
		t.Fatalf("普通运营改规则应 403，实际 %d", st)
	}

	// 3) 运营主管收紧规则：批量删除从「owner/ops_manager」改为「仅老板」
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals/rules", lead, map[string]any{
		"ticket_type": "shop.bulk_delete", "roles": []string{"owner"}, "min_approvers": 1,
		"reason": "本团队要求：批量删除一律老板批", "priority": 10, "url": "/approvals",
	})
	if st != http.StatusOK {
		t.Fatalf("运营主管建规则失败: %d %v", st, body)
	}
	ruleID, _ := body["id"].(string)
	if ruleID == "" {
		t.Fatalf("应返回规则 ID: %v", body)
	}
	if st, body = do(t, "GET", srv.URL+"/api/v1/approvals/rules", owner, nil); st != http.StatusOK {
		t.Fatalf("读规则失败: %d", st)
	}
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("应有 1 条自定义规则，实际 %v", items)
	}

	// 4) 配置必须真的生效在审批链路上：运营主管自己不能再批批量删除
	//（单据由运营主管提交——申请人与审批人不能是同一人，老板才来批）
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals", lead, map[string]any{
		"type": "shop.bulk_delete", "subject_type": "shop", "reason": "关停店铺",
		"detail": map[string]any{"count": 2}, "url": "/approvals",
	})
	if st != http.StatusCreated {
		t.Fatalf("提交审批单失败: %d %v", st, body)
	}
	ticketID, _ := body["id"].(string)
	if st, _ = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", lead,
		map[string]any{"comment": "同意"}); st == http.StatusOK {
		t.Fatal("收紧后运营主管不应能批批量删除（内置规则下他是可以批的）")
	}
	// 老板可以
	if st, _ = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", owner,
		map[string]any{"comment": "同意"}); st != http.StatusOK {
		t.Fatalf("老板应能批: %d", st)
	}

	// 5) 删除规则 = 回落内置，而不是「不用审批」
	if st, _ = do(t, "DELETE", srv.URL+"/api/v1/approvals/rules/"+ruleID, owner, nil); st != http.StatusOK {
		t.Fatalf("删除规则失败: %d", st)
	}
	if st, body = do(t, "GET", srv.URL+"/api/v1/approvals/rules", owner, nil); st != http.StatusOK {
		t.Fatalf("读规则失败: %d", st)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("删除后应无自定义规则: %v", items)
	}
	if st, _ = do(t, "DELETE", srv.URL+"/api/v1/approvals/rules/"+ruleID, owner, nil); st == http.StatusOK {
		t.Fatal("重复删除不应返回 200（否则前端以为删掉了其实没删）")
	}
}
