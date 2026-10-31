package gateway_test

import (
	"net/http"
	"testing"
)

// inviteAndGrant 邀请成员并授予角色，返回其 token（所有者以外的人 password 统一用 testPassword）。
func inviteAndGrant(t *testing.T, srv, tenantID, token, username, role string) string {
	t.Helper()
	st, body := do(t, "POST", srv+"/api/v1/tenants/"+tenantID+"/members", token, map[string]any{
		"username": username, "password": testPassword, "nickname": username,
	})
	if st != http.StatusCreated {
		t.Fatalf("邀请成员 %s 失败: %d %v", username, st, body)
	}
	memberID, _ := body["id"].(string)
	if st, body = do(t, "POST", srv+"/api/v1/tenants/"+tenantID+"/members/"+memberID+"/roles", token,
		map[string]any{"role": role}); st != http.StatusOK {
		t.Fatalf("授权 %s 失败: %d %v", role, st, body)
	}
	st, body = do(t, "POST", srv+"/api/v1/auth/login", "", map[string]any{
		"username": username, "password": testPassword,
	})
	if st != http.StatusOK {
		t.Fatalf("%s 登录失败: %d %v", username, st, body)
	}
	userToken, _ := body["token"].(string)
	if userToken == "" {
		t.Fatalf("%s 未拿到 token: %v", username, body)
	}
	return userToken
}

// TestBulkDeleteRequiresApproval 批量删除必须持证：
// 没有审批单 → 202 approval_required；他人（运营主管）批准后方可执行；一张单只能用一次。
//
// 这条补的是真实缺口——之前 shop.bulk_delete 只是配置里的一个字符串，
// 没有任何接口去 Verify 它，批量删除可以随手清库。
func TestBulkDeleteRequiresApproval(t *testing.T) {
	_, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "批删团队", "bulk_owner")

	ids := make([]string, 0, 2)
	for _, name := range []string{"批删店A", "批删店B"} {
		st, body := do(t, "POST", srv.URL+"/api/v1/shops", token, map[string]any{
			"name": name, "platform": "Amazon", "site": "US",
		})
		if st != http.StatusCreated {
			t.Fatalf("建店失败: %d %v", st, body)
		}
		id, _ := body["id"].(string)
		ids = append(ids, id)
	}

	// 1) 无证批量删除 → 被服务端硬拦（不是前端灰按钮）
	st, body := do(t, "POST", srv.URL+"/api/v1/shops/batch/delete", token, map[string]any{
		"ids": ids, "url": "/shops",
	})
	if st == http.StatusOK {
		t.Fatalf("无审批单不应删除: %v", body)
	}
	if body["code"] != "approval_required" {
		t.Fatalf("错误码应为 approval_required，实际 %v", body)
	}

	// 2) 提交批量删除审批单
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals", token, map[string]any{
		"type": "shop.bulk_delete", "subject_type": "shop", "reason": "关停美国站店铺",
		"detail": map[string]any{"count": len(ids), "names": []string{"批删店A", "批删店B"}},
		"url":    "/approvals",
	})
	if st != http.StatusCreated {
		t.Fatalf("提交审批单失败: %d %v", st, body)
	}
	ticketID, _ := body["id"].(string)

	// 3) 自己批自己 → 被拒（L3 职责分离）
	if st, body = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", token,
		map[string]any{"comment": "我自己批"}); st == http.StatusOK {
		t.Fatalf("自己审批自己不应通过: %v", body)
	}

	// 4) 运营主管批准（路由规则：批量删除需 owner/ops_manager）
	leadToken := inviteAndGrant(t, srv.URL, tenantID, token, "bulk_lead", "ops_manager")
	if st, body = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", leadToken,
		map[string]any{"comment": "确认关停"}); st != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("运营主管应可批准批量删除: %d %v", st, body)
	}

	// 5) 持证删除
	st, body = do(t, "POST", srv.URL+"/api/v1/shops/batch/delete?ticket="+ticketID, token,
		map[string]any{"ids": ids, "url": "/shops"})
	if st != http.StatusOK {
		t.Fatalf("持证批量删除应成功: %d %v", st, body)
	}
	if got, _ := body["deleted"].(float64); got != 2 {
		t.Fatalf("应删除 2 家，实际 %v（%v）", got, body)
	}

	// 6) 店铺进入回收站（软删，不是物理删除）
	st, body = do(t, "GET", srv.URL+"/api/v1/shops/trash", token, nil)
	if st != http.StatusOK {
		t.Fatalf("查询回收站失败: %d %v", st, body)
	}
	if total, _ := body["total"].(float64); total != 2 {
		t.Fatalf("回收站应有 2 家，实际 %v", total)
	}

	// 7) 同一张单不能再用第二次（一次性核销）
	if st, body = do(t, "POST", srv.URL+"/api/v1/shops/batch/delete?ticket="+ticketID, token,
		map[string]any{"ids": ids, "url": "/shops"}); st == http.StatusOK {
		t.Fatalf("审批单不应重复使用: %v", body)
	}
}

// TestPurchaseTicketAmountIsServerComputed 审批单金额必须由服务端算，不能听客户端报。
//
// 路由规则是拿着单据金额决定要不要两位老板会签的；若采信 detail.amount，
// "申购 198 台（5 万+）把 amount 写成 100" 就能走单人审批，双人复核形同虚设。
func TestPurchaseTicketAmountIsServerComputed(t *testing.T) {
	_, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "谎报团队", "buy_liar")

	// 谎报金额 100 元，但真实台数/月数对应的金额远超阈值
	st, body := do(t, "POST", srv.URL+"/api/v1/approvals", token, map[string]any{
		"type": "device.purchase", "subject_type": "device", "reason": "旺季扩容（谎报金额）",
		"detail": map[string]any{"amount": 100, "count": 198, "months": 4},
		"url":    "/approvals",
	})
	if st != http.StatusCreated {
		t.Fatalf("提交审批单失败: %d %v", st, body)
	}
	detail, _ := body["detail"].(map[string]any)
	amount, _ := detail["amount"].(float64)
	const want = 53856 // 198 台 × 4 月 × 68 元，按套餐单价由服务端重算
	if amount != want {
		t.Fatalf("单据金额应被服务端计算值覆盖，期望 %v 实际 %v（detail=%v）", want, amount, detail)
	}
	ticketID, _ := body["id"].(string)

	// 金额修正后这条单走 owner 双人复核：运营主管不该放行
	leadToken := inviteAndGrant(t, srv.URL, tenantID, token, "liar_lead", "ops_manager")
	if st, _ = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", leadToken,
		map[string]any{"comment": "金额才 100 元，我批了"}); st == http.StatusOK {
		t.Fatal("服务端算出大额后，运营主管不应放行（双人复核须 owner）")
	}
}

// TestPurchaseQuorumRequiresTwoOwners 大额设备申购必须双人复核（会签）：
// 阈值以下的小额申购照常放行；超过阈值则需两位 owner 分别签署。
//
// 会签引擎早就写好了，但此前没有任何业务入口调用它——这是把它接上电的那条链路。
func TestPurchaseQuorumRequiresTwoOwners(t *testing.T) {
	_, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "申购团队", "buy_owner")

	// 套餐单价 68 元/设备/月：1 台 1 个月 = 68 元，远低于阈值 → 不该打扰任何人
	st, body := do(t, "POST", srv.URL+"/api/v1/devices/purchase", token, map[string]any{
		"name": "小额申购", "type": "cloud_network", "country": "US", "city": "New York",
		"months": 1, "count": 1, "url": "/devices",
	})
	if st != http.StatusCreated {
		t.Fatalf("小额申购不应需要审批: %d %v", st, body)
	}

	// 大额申购：专业版配额 200 台，198 台 × 4 个月 × 68 元 = 53856 元 > 阈值 50000
	bigCount := 198
	st, body = do(t, "POST", srv.URL+"/api/v1/devices/purchase", token, map[string]any{
		"name": "大额申购", "type": "cloud_network", "country": "US", "city": "New York",
		"months": 4, "count": bigCount, "url": "/devices",
	})
	if st == http.StatusCreated {
		t.Fatalf("大额申购未拦截: %v", body)
	}
	if body["code"] != "approval_required" {
		t.Fatalf("大额申购应要求审批单，实际 %v", body)
	}

	// 提交审批单：金额写进 detail，路由规则据此判定双人复核
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals", token, map[string]any{
		"type": "device.purchase", "subject_type": "device", "reason": "旺季扩容",
		"detail": map[string]any{"amount": 53856, "count": bigCount, "months": 4},
		"url":    "/approvals",
	})
	if st != http.StatusCreated {
		t.Fatalf("提交审批单失败: %d %v", st, body)
	}
	ticketID, _ := body["id"].(string)

	// 运营主管不是这条单要求的角色（须 owner），应被拒且单据保持 pending
	leadToken := inviteAndGrant(t, srv.URL, tenantID, token, "buy_lead", "ops_manager")
	if st, _ = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", leadToken,
		map[string]any{"comment": "我批了"}); st == http.StatusOK {
		t.Fatal("大额申购需 owner 签署，运营主管不应放行")
	}
	if st, body = do(t, "GET", srv.URL+"/api/v1/approvals?limit=10", token, nil); st != http.StatusOK {
		t.Fatalf("查询审批单失败: %d %v", st, body)
	}
	if items, _ := body["items"].([]any); len(items) == 1 {
		tk, _ := items[0].(map[string]any)
		if tk["status"] != "pending" {
			t.Fatalf("越权签署后单据必须仍是 pending，实际 %v", tk["status"])
		}
	}

	// 第一位老板签署 → 仍未达法定人数，应保持 pending
	boss2 := inviteAndGrant(t, srv.URL, tenantID, token, "buy_boss2", "owner")
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", boss2,
		map[string]any{"comment": "同意，第一位"})
	if st != http.StatusOK {
		t.Fatalf("老板签署失败: %d %v", st, body)
	}
	if body["status"] != "pending" {
		t.Fatalf("双人复核：第一人签署后应仍为 pending，实际 %v", body["status"])
	}
	// 同一人再签一次不累计票数（否则一人就能刷够票）
	if st, _ = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", boss2,
		map[string]any{"comment": "再签一次"}); st == http.StatusOK {
		t.Fatal("同一人重复签署不应改变票数")
	}

	// 第二位老板签署 → 达到法定人数
	boss3 := inviteAndGrant(t, srv.URL, tenantID, token, "buy_boss3", "owner")
	st, body = do(t, "POST", srv.URL+"/api/v1/approvals/"+ticketID+"/approve", boss3,
		map[string]any{"comment": "同意，第二位"})
	if st != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("双人签署后应 approved，实际 %d %v", st, body)
	}

	// 持证完成大额申购（此前已被拦截的同一请求）
	st, body = do(t, "POST", srv.URL+"/api/v1/devices/purchase?ticket="+ticketID, token, map[string]any{
		"name": "大额申购", "type": "cloud_network", "country": "US", "city": "New York",
		"months": 4, "count": bigCount, "url": "/devices",
	})
	if st != http.StatusCreated {
		t.Fatalf("持证后的大额申购应成功: %d %v", st, body)
	}

	// 单据被核销后不能再买第二笔
	if st, _ = do(t, "POST", srv.URL+"/api/v1/devices/purchase?ticket="+ticketID, token, map[string]any{
		"name": "再买一批", "type": "cloud_network", "country": "US", "city": "New York",
		"months": 4, "count": 1, "url": "/devices",
	}); st == http.StatusCreated {
		t.Fatal("审批单一次性使用，不应再次放行")
	}
}
