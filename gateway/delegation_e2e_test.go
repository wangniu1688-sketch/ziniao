package gateway_test

import (
	"context"
	"net/http"
	"testing"

	"ziniao/platform/store"
)

// agencyUserID 取受托方账号的用户 ID（store 直查，仅测试用）。
func agencyUserID(t *testing.T, st store.Store, username string) string {
	t.Helper()
	u, err := st.Users().GetByUsername(context.Background(), username)
	if err != nil || u == nil {
		t.Fatalf("查用户失败: %v", err)
	}
	return u.ID
}

// TestDelegationLifecycleE2E 端到端：代理机构申请 → 委托方批准 → 授权生效 → 撤销回收。
//
// 这条链路的意义：委托批准不能只是改个状态——批准后受托方必须**真的**能以
// 委托方团队成员的身份看到被托管的店铺（DataGrant + 成员身份 + agency_ops 角色），
// 撤销后必须**真的**看不到。审批结果与实际权限不一致，比没有这个功能更糟。
func TestDelegationLifecycleE2E(t *testing.T) {
	a, srv := newTestServer(t)
	gToken, gTenant := registerAndLogin(t, srv.URL, "店铺主团队", "g_owner")
	aToken, _ := registerAndLogin(t, srv.URL, "代运营公司", "a_owner")
	const agencyUser = "a_owner"

	// 委托方建两家店
	shopIDs := []string{}
	for _, name := range []string{"亚马逊美国站", "Shopee马来站"} {
		st, body := do(t, "POST", srv.URL+"/api/v1/shops", gToken, map[string]any{"name": name, "url": "/shops"})
		if st != http.StatusCreated {
			t.Fatalf("建店失败: %d %v", st, body)
		}
		id, _ := body["id"].(string)
		shopIDs = append(shopIDs, id)
	}

	// 1) 受托方提交申请
	st, body := do(t, "POST", srv.URL+"/api/v1/delegations", aToken, map[string]any{
		"tenant_slug": "g_owner-shop", "shop_ids": shopIDs, "valid_days": 30, "reason": "代运营合作", "url": "/delegations",
	})
	if st != http.StatusCreated {
		t.Fatalf("提交委托申请失败: %d %v", st, body)
	}
	flowID, _ := body["id"].(string)
	if body["status"] != "pending" {
		t.Fatalf("新申请应为 pending: %v", body)
	}

	// 2) 重复申请被挡（同对（委托方,受托方）同时只允许一张 pending）
	if st, _ = do(t, "POST", srv.URL+"/api/v1/delegations", aToken, map[string]any{
		"tenant_slug": "g_owner-shop", "shop_ids": shopIDs[:1], "url": "/delegations",
	}); st != http.StatusConflict {
		t.Fatalf("重复申请应返回 409，实际 %d", st)
	}

	// 3) 混入不属于委托方的店铺：整体拒绝
	if st, _ = do(t, "POST", srv.URL+"/api/v1/delegations", aToken, map[string]any{
		"tenant_slug": "g_owner-shop", "shop_ids": []string{"s-none"}, "url": "/delegations",
	}); st != http.StatusBadRequest {
		t.Fatalf("范围含不属于委托方的店铺应 400，实际 %d", st)
	}

	// 4) 双方各自视角都能看到这张单
	if st, body = do(t, "GET", srv.URL+"/api/v1/delegations", gToken, nil); st != http.StatusOK {
		t.Fatalf("委托方查列表失败: %d %v", st, body)
	}
	if total, _ := body["total"].(float64); total != 1 {
		t.Fatalf("委托方应看到 1 张申请: %v", body)
	}
	if st, body = do(t, "GET", srv.URL+"/api/v1/delegations?party=agency", aToken, nil); st != http.StatusOK {
		t.Fatalf("受托方查列表失败: %d %v", st, body)
	}
	if total, _ := body["total"].(float64); total != 1 {
		t.Fatalf("受托方应看到自己发出的 1 张申请: %v", body)
	}

	// 5) 受托方不能批自己的申请（它手上也有 delegation:approve）
	if st, _ = do(t, "POST", srv.URL+"/api/v1/delegations/"+flowID+"/approve", aToken, nil); st != http.StatusForbidden {
		t.Fatalf("受托方自行批准应 403，实际 %d", st)
	}

	// 6) 委托方批准：授权落地
	st, body = do(t, "POST", srv.URL+"/api/v1/delegations/"+flowID+"/approve", gToken,
		map[string]any{"comment": "欢迎", "url": "/delegations"})
	if st != http.StatusOK {
		t.Fatalf("批准失败: %d %v", st, body)
	}
	if body["status"] != "active" || body["valid_until"] == nil {
		t.Fatalf("批准后应 active 且带 valid_until: %v", body)
	}

	// 7) 授权三件套落库（成员身份 + 每店一条带到期的 DataGrant）
	uid := agencyUserID(t, a.Store, agencyUser)
	tctx := store.WithTenant(context.Background(), gTenant)
	agrants, err := a.Store.DataGrants().ListByUser(tctx, gTenant, uid, "shop")
	if err != nil || len(agrants) != 2 {
		t.Fatalf("批准后应有 2 条店铺级授权: n=%d err=%v", len(agrants), err)
	}
	if ms, err := a.Store.Memberships().Get(tctx, gTenant, uid); err != nil || ms == nil {
		t.Fatalf("批准后受托方申请人应成为委托方成员: %v %v", ms, err)
	}

	// 8) 受托方申请人以委托方团队身份登录：能看到被托管的店铺（own 范围 + 委托放行）
	st, body = do(t, "POST", srv.URL+"/api/v1/auth/login", "", map[string]any{
		"username": agencyUser, "password": testPassword, "tenant_id": gTenant,
	})
	if st != http.StatusOK {
		t.Fatalf("受托方以委托方团队登录失败: %d %v", st, body)
	}
	gaToken := body["token"].(string)
	st, body = do(t, "GET", srv.URL+"/api/v1/shops", gaToken, nil)
	if st != http.StatusOK {
		t.Fatalf("受托方查店铺失败: %d %v", st, body)
	}
	if total, _ := body["total"].(float64); total != 2 {
		t.Fatalf("受托方应看到 2 家被托管的店铺: %v", body)
	}

	// 8b) 数据级收口（环境入口）：受托方能启动/关闭**被托管**店铺的环境；
	// 未托管店铺虽然同在委托方租户内，也必须被数据级校验挡住——
	// 此前环境入口只做功能层权限，own 范围成员可以操作租户内任何店铺的环境。
	st, body = do(t, "POST", srv.URL+"/api/v1/shops", gToken, map[string]any{"name": "未托管店", "url": "/shops"})
	if st != http.StatusCreated {
		t.Fatalf("建未托管店失败: %d %v", st, body)
	}
	otherShopID := body["id"].(string)
	// agency_ops 没有 env:create：环境由委托方先建好
	st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopIDs[0]+"/environment", gToken,
		map[string]any{"country": "US", "url": "/shops"})
	if st != http.StatusCreated {
		t.Fatalf("委托方建环境失败: %d", st)
	}
	st, launchBody := do(t, "POST", srv.URL+"/api/v1/shops/"+shopIDs[0]+"/environment/launch", gaToken,
		map[string]any{"open_url": ""})
	if st != http.StatusOK {
		t.Fatalf("受托方启动被托管店铺环境应放行: %d %v", st, launchBody)
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+shopIDs[0]+"/environment/stop", gaToken, nil); st != http.StatusOK {
		t.Fatalf("受托方关闭被托管店铺环境应放行: %d", st)
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+otherShopID+"/environment/launch", gaToken, nil); st != http.StatusForbidden {
		t.Fatalf("受托方启动未托管店铺环境应 403: %d", st)
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/shops/"+otherShopID+"/environment/stop", gaToken, nil); st != http.StatusForbidden {
		t.Fatalf("受托方关闭未托管店铺环境应 403: %d", st)
	}

	// 9) 再次批准 → 409（单据已处理）
	if st, _ = do(t, "POST", srv.URL+"/api/v1/delegations/"+flowID+"/approve", gToken, nil); st != http.StatusConflict {
		t.Fatalf("重复批准应 409，实际 %d", st)
	}

	// 10) 撤销：授权即时回收
	st, body = do(t, "POST", srv.URL+"/api/v1/delegations/"+flowID+"/revoke", gToken,
		map[string]any{"comment": "合作终止", "url": "/delegations"})
	if st != http.StatusOK || body["status"] != "revoked" {
		t.Fatalf("撤销失败: %d %v", st, body)
	}
	agrants, _ = a.Store.DataGrants().ListByUser(tctx, gTenant, uid, "shop")
	if len(agrants) != 0 {
		t.Fatalf("撤销后店铺授权应清零: %d", len(agrants))
	}

	// 11) 撤销后受托方（重新登录的会话）失去全部委托权限：查店铺应被功能层拒绝
	//（agency_ops 角色随委托回收，成员身份保留但不再构成任何权限面）
	st, body = do(t, "POST", srv.URL+"/api/v1/auth/login", "", map[string]any{
		"username": agencyUser, "password": testPassword, "tenant_id": gTenant,
	})
	if st != http.StatusOK {
		t.Fatalf("撤销后登录失败: %d %v", st, body)
	}
	st, body = do(t, "GET", srv.URL+"/api/v1/shops", body["token"].(string), nil)
	if st != http.StatusForbidden {
		t.Fatalf("撤销后受托方查店铺应 403（权限已回收），实际 %d %v", st, body)
	}
}

// TestDelegationCrossTenantDenied 第三方团队既不能批也不能撤别人的委托。
func TestDelegationCrossTenantDenied(t *testing.T) {
	_, srv := newTestServer(t)
	gToken, _ := registerAndLogin(t, srv.URL, "店铺主团队B", "gb_owner")
	aToken, _ := registerAndLogin(t, srv.URL, "代运营公司B", "ab_owner")
	third, _ := registerAndLogin(t, srv.URL, "无关团队", "third_owner")

	st, body := do(t, "POST", srv.URL+"/api/v1/shops", gToken, map[string]any{"name": "店B", "url": "/shops"})
	if st != http.StatusCreated {
		t.Fatalf("建店失败: %d %v", st, body)
	}
	shopID, _ := body["id"].(string)

	st, body = do(t, "POST", srv.URL+"/api/v1/delegations", aToken, map[string]any{
		"tenant_slug": "gb_owner-shop", "shop_ids": []string{shopID}, "url": "/delegations",
	})
	if st != http.StatusCreated {
		t.Fatalf("提交失败: %d %v", st, body)
	}
	flowID := body["id"].(string)

	for _, op := range []string{"approve", "reject", "revoke"} {
		if st, _ = do(t, "POST", srv.URL+"/api/v1/delegations/"+flowID+"/"+op, third, nil); st == http.StatusOK {
			t.Fatalf("第三方不应能 %s 别人的委托", op)
		}
	}
	// 撤回只能由受托方申请人本人发起
	if st, _ = do(t, "POST", srv.URL+"/api/v1/delegations/"+flowID+"/cancel", third, nil); st == http.StatusOK {
		t.Fatal("第三方不应能撤回")
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/delegations/"+flowID+"/cancel", aToken, nil); st != http.StatusOK {
		t.Fatalf("受托方申请人撤回应成功: %d", st)
	}
}
