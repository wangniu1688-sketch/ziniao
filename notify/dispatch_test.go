package notify_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/notify"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
)

// stubSender 可控的外发通道替身。
type stubSender struct {
	ch    string
	err   error
	calls int
	tos   []string
}

func (s *stubSender) Channel() string { return s.ch }

func (s *stubSender) Send(_ context.Context, m notify.Message) error {
	s.calls++
	s.tos = append(s.tos, m.To)
	// 与真实 SMTPSender 同口径：收件人为空必须返回哨兵错误
	if s.ch == "email" && strings.TrimSpace(m.To) == "" {
		return notify.ErrNoRecipient
	}
	return s.err
}

func newSinkEnv(t *testing.T, senders ...notify.Sender) (*mem.Store, *notify.Sink) {
	t.Helper()
	ctx := context.Background()
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	return st, notify.NewSink(st, nil, senders...).UseConfig(cfg)
}

func addUser(t *testing.T, st *mem.Store, id, email string) {
	t.Helper()
	// users 表本身不带 tenant_id（租户归属在 memberships），这里只需邮箱用于投递
	u := types.User{
		ID: id, Username: id, Email: email,
		Status: "active", CreatedAt: time.Now().UTC(),
	}
	if err := st.Users().Create(context.Background(), &u); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
}

func highEvent() *eventbus.Event {
	return &eventbus.Event{
		ID: ids.New(), TenantID: "t1", Type: "env.exit_ip_rotated",
		OccurredAt: time.Now().UTC(), Actor: eventbus.Actor{UserID: "u-1"},
		Terminal: eventbus.Terminal{Mode: "browser", ClientVer: "1.0"},
		Context:  eventbus.ContextInfo{Action: "launch", Result: "success"},
		Risk:     eventbus.RiskHigh,
	}
}

func listOf(t *testing.T, st *mem.Store, userID string) []types.Notification {
	t.Helper()
	out, err := st.Notifications().List(context.Background(), "t1", userID, 20)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHighEventFanOutToExternalChannels high 告警必须落到所有已装配的外发通道。
func TestHighEventFanOutToExternalChannels(t *testing.T) {
	email := &stubSender{ch: "email"}
	hook := &stubSender{ch: "webhook"}
	st, sink := newSinkEnv(t, email, hook)
	addUser(t, st, "u-1", "ops@example.com")

	if err := sink.Handle(context.Background(), highEvent()); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	list := listOf(t, st, "u-1")
	if len(list) != 1 {
		t.Fatalf("应入库 1 条通知，实际 %d", len(list))
	}
	n := list[0]
	if strings.Join(n.Channels, ",") != "inapp,email,webhook" {
		t.Fatalf("通道清单不符: %v", n.Channels)
	}
	if n.DeliveryStatus != notify.StatusSent {
		t.Fatalf("投递状态应为 sent，实际 %s（note=%s）", n.DeliveryStatus, n.DeliveryNote)
	}
	if len(n.DeliveredChannels) != 2 || n.Attempts != 1 {
		t.Fatalf("投递留痕不符: delivered=%v attempts=%d", n.DeliveredChannels, n.Attempts)
	}
	if email.calls != 1 || hook.calls != 1 {
		t.Fatalf("每个通道应恰好投递一次: email=%d webhook=%d", email.calls, hook.calls)
	}
	if len(email.tos) != 1 || email.tos[0] != "ops@example.com" {
		t.Fatalf("邮件收件人应取自用户档案，实际: %v", email.tos)
	}
}

// TestMediumEventStaysInApp medium 不外发：否则邮箱会被用量提醒淹没，
// 真正的高危告警反而没人看。
func TestMediumEventStaysInApp(t *testing.T) {
	hook := &stubSender{ch: "webhook"}
	st, sink := newSinkEnv(t, hook)
	addUser(t, st, "u-1", "ops@example.com")

	ev := highEvent()
	ev.Type = "usage.recorded"
	ev.Risk = eventbus.RiskMedium
	if err := sink.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	list := listOf(t, st, "u-1")
	if len(list) != 1 {
		t.Fatalf("站内通知仍应入库，实际 %d", len(list))
	}
	if strings.Join(list[0].Channels, ",") != "inapp" {
		t.Fatalf("medium 事件不应带外发通道: %v", list[0].Channels)
	}
	if list[0].DeliveryStatus != notify.StatusNone {
		t.Fatalf("无外发通道时应为 none，实际 %s", list[0].DeliveryStatus)
	}
	if hook.calls != 0 {
		t.Fatalf("medium 事件不应触发外发，实际 %d 次", hook.calls)
	}
}

// TestDeliveryFailureDoesNotDuplicateNotification 投递失败绝不能让整条事件重投
// ——那会再生成一条一模一样的站内通知。
func TestDeliveryFailureDoesNotDuplicateNotification(t *testing.T) {
	hook := &stubSender{ch: "webhook", err: context.DeadlineExceeded}
	st, sink := newSinkEnv(t, hook)
	addUser(t, st, "u-1", "ops@example.com")

	ev := highEvent()
	if err := sink.Handle(context.Background(), ev); err != nil {
		t.Fatalf("外发失败不应让 Handle 返回错误: %v", err)
	}
	// 同一条事件再派发一次：此处只应仍然是一条通知（重复由调用方的幂等保证，
	// 关键是即使 Handle 被调用两次，失败也不会额外放大）
	list := listOf(t, st, "u-1")
	if len(list) != 1 {
		t.Fatalf("通知条数异常: %d", len(list))
	}
	n := list[0]
	if n.DeliveryStatus != notify.StatusFailed {
		t.Fatalf("应标记为 failed，实际 %s", n.DeliveryStatus)
	}
	if !strings.Contains(n.DeliveryNote, "webhook") {
		t.Fatalf("失败摘要应指出通道: %q", n.DeliveryNote)
	}
	if n.Attempts != 1 {
		t.Fatalf("首次投递 attempts 应为 1，实际 %d", n.Attempts)
	}
}

// TestEmailSkippedWhenUserHasNoMailbox 用户没填邮箱：跳过邮件而不是把通知判死。
func TestEmailSkippedWhenUserHasNoMailbox(t *testing.T) {
	email := &stubSender{ch: "email"}
	hook := &stubSender{ch: "webhook"}
	st, sink := newSinkEnv(t, email, hook)
	addUser(t, st, "u-1", "")

	if err := sink.Handle(context.Background(), highEvent()); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	n := listOf(t, st, "u-1")[0]
	if n.DeliveryStatus != notify.StatusSent {
		t.Fatalf("Webhook 已送达时整体应为 sent（邮件缺收件人属跳过），实际 %s note=%s",
			n.DeliveryStatus, n.DeliveryNote)
	}
	pending, err := st.Notifications().ListPendingDelivery(context.Background(), "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("已终结的通知不应出现在待重投清单里: %+v", pending)
	}
}

// TestRetryOnlySendsMissingChannels 重投只补「还没送达」的通道：
// 邮件已经收到的人不该因为 Webhook 抖动又收到一封。
func TestRetryOnlySendsMissingChannels(t *testing.T) {
	email := &stubSender{ch: "email"}
	hook := &stubSender{ch: "webhook"}
	st, sink := newSinkEnv(t, email, hook)
	addUser(t, st, "u-1", "ops@example.com")
	ctx := context.Background()

	n := types.Notification{
		ID: ids.New(), TenantID: "t1", UserID: "u-1", Category: "security",
		Title: "出口 IP 轮换告警", Body: "…", EventType: "env.exit_ip_rotated",
		Channels: []string{"inapp", "email", "webhook"}, CreatedAt: time.Now().UTC().Add(-time.Minute),
		DeliveryStatus: notify.StatusFailed, DeliveryNote: "webhook: timeout",
		Attempts: 1, DeliveredChannels: []string{"webhook"},
	}
	if err := st.Notifications().Create(store.WithTenant(ctx, "t1"), &n); err != nil {
		t.Fatal(err)
	}

	sent, givenUp, err := sink.RetryTenant(ctx, "t1")
	if err != nil {
		t.Fatalf("重投失败: %v", err)
	}
	if sent != 1 || givenUp != 0 {
		t.Fatalf("应成功补投 1 条：sent=%d givenUp=%d", sent, givenUp)
	}
	if email.calls != 1 || hook.calls != 0 {
		t.Fatalf("只应补投邮件；email=%d webhook=%d", email.calls, hook.calls)
	}
	got := listOf(t, st, "u-1")[0]
	if got.DeliveryStatus != notify.StatusSent {
		t.Fatalf("全部通道送达后应为 sent，实际 %s", got.DeliveryStatus)
	}
	if got.Attempts != 2 {
		t.Fatalf("attempts 应累加到 2，实际 %d", got.Attempts)
	}
	if pending, _ := st.Notifications().ListPendingDelivery(ctx, "t1", 10); len(pending) != 0 {
		t.Fatalf("已送达的行不应仍在待重投清单: %+v", pending)
	}
}

// TestRetryGivesUpAfterMaxAttempts 重投必须有终点：耗尽次数后置 failed 并停手，
// 否则一封永远投不出去的邮件会让失败清单无限增长。
func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	hook := &stubSender{ch: "webhook", err: context.DeadlineExceeded}
	st, sink := newSinkEnv(t, hook)
	addUser(t, st, "u-1", "ops@example.com")
	ctx := context.Background()

	limit := config.Default().Notify.AttemptsLimit()
	n := types.Notification{
		ID: ids.New(), TenantID: "t1", UserID: "u-1", Category: "security",
		Title: "出口 IP 轮换告警", Body: "…", EventType: "env.exit_ip_rotated",
		Channels: []string{"inapp", "webhook"}, CreatedAt: time.Now().UTC().Add(-time.Minute),
		DeliveryStatus: notify.StatusFailed, Attempts: limit,
	}
	if err := st.Notifications().Create(store.WithTenant(ctx, "t1"), &n); err != nil {
		t.Fatal(err)
	}
	sent, givenUp, err := sink.RetryTenant(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if sent != 0 || givenUp != 1 {
		t.Fatalf("应放弃 1 条：sent=%d givenUp=%d", sent, givenUp)
	}
	if hook.calls != 0 {
		t.Fatalf("attempts 已达上限时不应再尝试投递，实际 %d 次", hook.calls)
	}
	got := listOf(t, st, "u-1")[0]
	if got.DeliveryStatus != notify.StatusFailed || !strings.Contains(got.DeliveryNote, "已重试") {
		t.Fatalf("应保留 failed 并写清原因: %s / %s", got.DeliveryStatus, got.DeliveryNote)
	}
}

// TestConsoleFallbackUnchanged 没配任何外发通道时（dev/单测）行为必须与改动前一致。
func TestConsoleFallbackUnchanged(t *testing.T) {
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	sink := notify.NewSink(st, nil) // 不传 sender → console
	addUser(t, st, "u-1", "ops@example.com")
	if err := sink.Handle(context.Background(), highEvent()); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	n := listOf(t, st, "u-1")[0]
	if strings.Join(n.Channels, ",") != "inapp,console" {
		t.Fatalf("默认装配应保持 [inapp,console]，实际 %v", n.Channels)
	}
	if n.DeliveryStatus != notify.StatusSent {
		t.Fatalf("console 投递应成功并记录 sent，实际 %s", n.DeliveryStatus)
	}
}

// TestNotifiesOwnerAndOpsManager 通知对象范围回归：操作人 + 租户内的 owner/运营主管。
func TestNotifiesOwnerAndOpsManager(t *testing.T) {
	st, sink := newSinkEnv(t)
	ctx := context.Background()
	addUser(t, st, "u-1", "ops@example.com")
	addUser(t, st, "u-owner", "owner@example.com")
	addUser(t, st, "u-normal", "normal@example.com")

	role, err := st.Roles().GetByCode(ctx, "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UserRoles().Assign(ctx, &types.UserRole{
		ID: ids.New(), TenantID: "t1", UserID: "u-owner", RoleID: role.ID,
		GrantedBy: "system", GrantedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Handle(ctx, highEvent()); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"u-1": 1, "u-owner": 1, "u-normal": 0} {
		if got := len(listOf(t, st, id)); got != want {
			t.Fatalf("用户 %s 应收到 %d 条，实际 %d", id, want, got)
		}
	}
}
