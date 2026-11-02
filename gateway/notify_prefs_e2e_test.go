package gateway_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// userIDOf 按用户名取 user id（读通知要落在具体的人身上）。
func userIDOf(t *testing.T, st store.Store, username string) string {
	t.Helper()
	u, err := st.Users().GetByUsername(context.Background(), username)
	if err != nil || u == nil {
		t.Fatalf("查用户 %s 失败: %v", username, err)
	}
	return u.ID
}

// TestNotificationPrefsAndUnread 通知偏好读写 + 未读角标真相。
//
// 覆盖两件事：
//  1. 偏好是**自助数据**：每人改自己的，未知通道名直接 400（静默生效会让用户
//     以为自己关掉了某个通道，实际还在发）；
//  2. 未读数是 SQL COUNT 的真值，不是"把最近 N 条拉回来数"。
func TestNotificationPrefsAndUnread(t *testing.T) {
	a, srv := newTestServer(t)
	token, tenantID := registerAndLogin(t, srv.URL, "通知测试团队", "notif_owner")
	uid := userIDOf(t, a.Store, "notif_owner")

	// 1) 默认：没设过偏好的人，UI 上看到的是「已装配的个人可选通道」全勾。
	//    测试环境没配 SMTP/短信，于是可选清单为空——这本身就是一条契约：
	//    没装通道时设置页不该摆出一个点了也没用的开关。
	st, body := do(t, "GET", srv.URL+"/api/v1/notifications/preferences", token, nil)
	if st != http.StatusOK {
		t.Fatalf("读取偏好失败: %d %v", st, body)
	}
	if avail, _ := body["available"].([]any); len(avail) != 0 {
		t.Fatalf("未装配通道时可选清单应为空，实际 %v", avail)
	}
	if chs, _ := body["channels"].([]any); len(chs) != 0 {
		t.Fatalf("未设置偏好时不应有启用通道，实际 %v", chs)
	}

	// 2) 未知通道名直接 400，而不是静默忽略
	st, body = do(t, "PUT", srv.URL+"/api/v1/notifications/preferences", token,
		map[string]any{"channels": []string{"wecom"}})
	if st != http.StatusBadRequest {
		t.Fatalf("未知通道应返回 400，实际 %d %v", st, body)
	}

	// 3) 合法写入（未装配也能存：偏好是「想要」，不是「现在有没有」）
	st, body = do(t, "PUT", srv.URL+"/api/v1/notifications/preferences", token,
		map[string]any{"channels": []string{"email"}})
	if st != http.StatusOK {
		t.Fatalf("写入偏好失败: %d %v", st, body)
	}
	prefs, err := a.Store.Notifications().GetPrefs(store.WithTenant(context.Background(), tenantID), tenantID, uid)
	if err != nil || prefs == nil {
		t.Fatalf("偏好应已落库: %v %v", prefs, err)
	}
	if len(prefs.Channels) != 1 || prefs.Channels[0] != "email" {
		t.Fatalf("落库通道应为 [email]，实际 %v", prefs.Channels)
	}

	// 4) 未登录不能读写偏好
	if st, _ = do(t, "GET", srv.URL+"/api/v1/notifications/preferences", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("未认证读取偏好应 401，实际 %d", st)
	}
	if st, _ = do(t, "PUT", srv.URL+"/api/v1/notifications/preferences", "",
		map[string]any{"channels": []string{"email"}}); st != http.StatusUnauthorized {
		t.Fatalf("未认证写入偏好应 401，实际 %d", st)
	}

	// 5) 未读数取真值：造 3 条通知，标记 1 条已读后必须是 2
	tctx := store.WithTenant(context.Background(), tenantID)
	idsOf := []string{}
	for i := 0; i < 3; i++ {
		n := &types.Notification{
			ID: ids.New(), TenantID: tenantID, UserID: uid, Category: "security",
			Title: "出口 IP 轮换告警", Body: "env-x 出口发生轮换",
			EventType: "env.exit_ip_rotated", Channels: []string{"inapp"},
			CreatedAt:      time.Now().UTC().Add(time.Duration(i) * time.Second),
			DeliveryStatus: "none",
		}
		if err := a.Store.Notifications().Create(tctx, n); err != nil {
			t.Fatal(err)
		}
		idsOf = append(idsOf, n.ID)
	}
	st, body = do(t, "GET", srv.URL+"/api/v1/notifications", token, nil)
	if st != http.StatusOK {
		t.Fatalf("读通知失败: %d %v", st, body)
	}
	if unread, _ := body["unread"].(float64); unread != 3 {
		t.Fatalf("未读数应为 3，实际 %v", unread)
	}
	if st, _ = do(t, "POST", srv.URL+"/api/v1/notifications/"+idsOf[0]+"/read", token, nil); st != http.StatusOK {
		t.Fatalf("标记已读失败: %d", st)
	}
	st, body = do(t, "GET", srv.URL+"/api/v1/notifications?limit=1", token, nil)
	if st != http.StatusOK {
		t.Fatalf("读通知失败: %d %v", st, body)
	}
	// limit=1 只拉回 1 条，但未读仍是 2 → 证明 unread 不是"拉回来数"的结果
	if unread, _ := body["unread"].(float64); unread != 2 {
		t.Fatalf("标记 1 条已读后未读数应为 2（且不受 limit 影响），实际 %v", unread)
	}

	// 6) 别人的通知看不见：换一个团队，查同一条通知 id 必须 404 而不是 200
	otherToken, _ := registerAndLogin(t, srv.URL, "不相干团队", "other_owner")
	if st, _ = do(t, "GET", srv.URL+"/api/v1/notifications", otherToken, nil); st != http.StatusOK {
		t.Fatalf("读自己的通知应成功: %d", st)
	}
	if total, _ := body["total"].(float64); total == 3 {
		t.Fatal("新团队不应看到别人的通知")
	}
	st, _ = do(t, "POST", srv.URL+"/api/v1/notifications/"+idsOf[1]+"/read", otherToken, nil)
	if st == http.StatusOK {
		t.Fatal("不应能把别人的通知标记为已读")
	}
}
