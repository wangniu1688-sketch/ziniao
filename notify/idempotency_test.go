package notify_test

import (
	"context"
	"testing"
)

// TestRedeliveryDoesNotDuplicateNotification 重投不能让同一个人收到两份通知。
//
// 场景：Handle 成功到 Ack 之间崩溃（或同事件被重复派发），outbox 会把整条事件重投。
// notify 的消费登记与站内通知落库在**同一事务**内提交，登记提交了通知必然在——
// 重投时逐收件人跳过，谁也不会收到第二封；整体完成标记则让重复投递直接短路。
func TestRedeliveryDoesNotDuplicateNotification(t *testing.T) {
	email := &stubSender{ch: "email"}
	st, sink := newSinkEnv(t, email)
	addUser(t, st, "u-1", "ops@example.com")

	ev := highEvent()
	if err := sink.Handle(context.Background(), ev); err != nil {
		t.Fatalf("首轮 Handle 失败: %v", err)
	}
	if n := len(listOf(t, st, "u-1")); n != 1 {
		t.Fatalf("首轮应入库 1 条通知，实际 %d", n)
	}
	// 同一条事件重投：直接再调一次 Handle（模拟 outbox 重投路径）
	if err := sink.Handle(context.Background(), ev); err != nil {
		t.Fatalf("重投 Handle 不应报错: %v", err)
	}
	if n := len(listOf(t, st, "u-1")); n != 1 {
		t.Fatalf("重投后仍应只有 1 条通知（每人至多一份），实际 %d", n)
	}
	if email.calls != 1 {
		t.Fatalf("外发通道也不该重发，实际 %d 次", email.calls)
	}
}

// TestAuditAndNotifyShareTheIdempotencyLedger 两个 Sink 消费同一条事件互不干扰：
// audit 全量订阅、notify 只订阅中高风险——各自按 (event_id, handler) 独立登记，
// 一方的登记不能把另一方挡掉。
func TestAuditAndNotifyShareTheIdempotencyLedger(t *testing.T) {
	st, sink := newSinkEnv(t)
	addUser(t, st, "u-1", "ops@example.com")
	ev := highEvent()
	if err := sink.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	// notify 的登记不应覆盖 audit 的键位：audit 侧还能登记（返回未消费）
	consumed, err := st.Outbox().Consumed(context.Background(), ev.ID, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if consumed {
		t.Fatal("notify 的消费登记不应被算到 audit 头上")
	}
	// 逐人键位与整体键位是分开的：整体登记了，逐人键位查询仍是 false
	perUser, err := st.Outbox().Consumed(context.Background(), ev.ID, "notify:u-1")
	if err != nil {
		t.Fatal(err)
	}
	if !perUser {
		t.Fatal("u-1 的逐人登记应在通知落库时一并提交")
	}
}
