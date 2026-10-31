package pg

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/ids"
	"ziniao/platform/migrate"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// TestNotificationDeliveryPG 通知投递留痕的真库验证（默认跳过）：
//
//	$env:ZINIAO_PG_DSN="postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable"
//	go test ./platform/store/pg -run TestNotificationDeliveryPG -v -count=1
//
// 为什么非要跑真库：这次加的东西有三处只有 PG 才管得住——
//
//	① text[] NOT NULL 遇到 Go 的 nil 切片会变 SQL NULL（23 系列报错，mem 无所谓）；
//	② delivery_status 的 CHECK 约束（mem 允许任意字符串）；
//	③ ListPendingDelivery 的过滤与偏序结果是纯 SQL 语义。
func TestNotificationDeliveryPG(t *testing.T) {
	dsn := os.Getenv("ZINIAO_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 ZINIAO_PG_DSN，跳过 PG 实跑用例")
	}
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := migrate.Up(ctx, st.Pool()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	tid := "t-notif-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	tctx := store.WithTenant(ctx, tid)
	defer func() {
		if _, err := st.Pool().Exec(context.Background(),
			`DELETE FROM notifications WHERE tenant_id=$1`, tid); err != nil {
			t.Logf("清理测试数据失败: %v", err)
		}
	}()
	repo := st.Notifications()
	now := time.Now().UTC().Truncate(time.Second)

	mk := func(status string, at time.Time, delivered []string) *types.Notification {
		return &types.Notification{
			ID: ids.New(), TenantID: tid, UserID: "u-1", Category: "security",
			Title: "出口 IP 轮换告警", Body: "env-1 出口发生轮换", EventType: "env.exit_ip_rotated",
			Channels: []string{"inapp", "email", "webhook"}, CreatedAt: at,
			DeliveryStatus: status, DeliveredChannels: delivered,
		}
	}

	// 1) 待投递行：delivered 为空切片（最容易触发 nil/NULL 的那种写法）
	pending := mk("pending", now, []string{})
	if err := repo.Create(tctx, pending); err != nil {
		t.Fatalf("写入待投递通知失败（delivered_channels 空切片被当成 NULL？）: %v", err)
	}
	// 2) nil 切片同样不能报错（strArr 兜底）
	nilSlice := mk("pending", now.Add(time.Second), nil)
	if err := repo.Create(tctx, nilSlice); err != nil {
		t.Fatalf("delivered_channels 为 nil 时写入失败: %v", err)
	}
	// 3) 已完成的和纯站内的不应出现在待重投清单
	if err := repo.Create(tctx, mk("sent", now.Add(2*time.Second), []string{"email"})); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(tctx, mk("none", now.Add(3*time.Second), nil)); err != nil {
		t.Fatal(err)
	}

	got, err := repo.List(tctx, tid, "u-1", 20)
	if err != nil || len(got) != 4 {
		t.Fatalf("列表读取失败或条数不符: n=%d err=%v", len(got), err)
	}
	var read types.Notification
	for _, n := range got {
		if n.ID == pending.ID {
			read = n
		}
	}
	if read.ID == "" {
		t.Fatalf("未读到目标行: %+v", got)
	}
	if read.DeliveryStatus != "pending" || read.Attempts != 0 || read.LastDeliveryAt != nil {
		t.Fatalf("投递字段读回不符: %+v", read)
	}
	if read.DeliveredChannels == nil || len(read.DeliveredChannels) != 0 {
		t.Fatalf("text[] 列应读回空切片而非 nil: %#v", read.DeliveredChannels)
	}

	// 4) 待重投清单：只含 pending（不含 sent/none），且按 created_at 升序
	pendingList, err := repo.ListPendingDelivery(tctx, tid, 10)
	if err != nil {
		t.Fatalf("ListPendingDelivery 失败: %v", err)
	}
	if len(pendingList) != 2 {
		t.Fatalf("待重投应只有 2 条 pending，实际 %d: %+v", len(pendingList), pendingList)
	}
	if pendingList[0].ID != pending.ID || pendingList[1].ID != nilSlice.ID {
		t.Fatalf("顺序应为 created_at 升序: %s, %s", pendingList[0].ID, pendingList[1].ID)
	}

	// 5) 回写投递结果：状态/note/attempts/已送达通道/时间都要能持久化
	cut := time.Now().UTC().Truncate(time.Second)
	pending.DeliveryStatus = "sent"
	pending.DeliveryNote = ""
	pending.Attempts = 1
	pending.DeliveredChannels = []string{"email", "webhook"}
	pending.LastDeliveryAt = &cut
	if err := repo.UpdateDelivery(tctx, pending); err != nil {
		t.Fatalf("UpdateDelivery 失败: %v", err)
	}
	again, err := repo.List(tctx, tid, "u-1", 20)
	if err != nil {
		t.Fatal(err)
	}
	var after types.Notification
	for _, n := range again {
		if n.ID == pending.ID {
			after = n
		}
	}
	if after.DeliveryStatus != "sent" || after.Attempts != 1 || after.LastDeliveryAt == nil {
		t.Fatalf("投递结果未落库: %+v", after)
	}
	if len(after.DeliveredChannels) != 2 || after.DeliveredChannels[0] != "email" {
		t.Fatalf("已送达通道未落库: %#v", after.DeliveredChannels)
	}
	// 回写不得动标题正文
	if after.Title != "出口 IP 轮换告警" {
		t.Fatalf("UpdateDelivery 不应改动通知正文: %q", after.Title)
	}

	// 6) 行不属于该租户时，必须明确「没找到」而不是静默成功。
	//
	// 注意：本 PG 环境连库账号是表的 owner，而 RLS 对表 owner 默认不生效
	// （要 FORCE ROW LEVEL SECURITY 才生效），所以真正拦住这次越权的只有
	// SQL 里那条显式的 tenant_id 谓词——这也是为什么写路径必须带租户参数，
	// 不能把「RLS 会兜住」当成唯一防线。
	ghost := *pending
	ghost.TenantID = "t-someone-else"
	if err := repo.UpdateDelivery(tctx, &ghost); err == nil {
		t.Fatal("定位不到的行应返回未找到")
	} else if ae, ok := err.(*apierr.Error); !ok || ae.Code != apierr.CodeNotFound {
		t.Fatalf("错误码应为 404，实际: %v", err)
	}

	// 7) CHECK 约束：非法状态直接被数据库拒掉
	bad := mk("unknown", now.Add(4*time.Second), nil)
	if err := repo.Create(store.WithTenant(ctx, tid), bad); err == nil {
		t.Fatal("非法 delivery_status 应被 CHECK 拒绝")
	}
	// 非法状态的行一定没落库，否则重投任务会把它当成待办反复扫描
	if rows, err := repo.ListPendingDelivery(tctx, tid, 10); err != nil || len(rows) != 1 {
		t.Fatalf("非法行不得入库；当前待重投 %d 条 err=%v", len(rows), err)
	}
}
