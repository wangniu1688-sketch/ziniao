package notify_test

import (
	"context"
	"strings"
	"testing"

	"ziniao/notify"
	"ziniao/platform/store"
)

// TestPrefsDefaultKeepsOldBehavior 没设过偏好的人行为不变：全通道照发。
//
// 这条是所有偏好逻辑的地基——老租户升级上来不能突然收不到邮件，
// 「我没动过设置」必须等价于「一切照旧」。
func TestPrefsDefaultKeepsOldBehavior(t *testing.T) {
	email := &stubSender{ch: "email"}
	hook := &stubSender{ch: "webhook"}
	st, sink := newSinkEnv(t, email, hook)
	addUser(t, st, "u-1", "ops@example.com")
	ctx := context.Background()

	if err := sink.Handle(ctx, highEvent()); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	n := listOf(t, st, "u-1")[0]
	if strings.Join(n.Channels, ",") != "inapp,email,webhook" {
		t.Fatalf("未设置偏好应按全通道外发，实际 %v", n.Channels)
	}
	if email.calls != 1 {
		t.Fatalf("邮件应投递 1 次，实际 %d", email.calls)
	}
	// 设置页默认全勾：否则 UI 一打开就像"我什么都没选"
	if got := sink.EffectiveChannels(ctx, "t1", "u-1"); len(got) != 1 || got[0] != "email" {
		t.Fatalf("默认展示应包含已装配的个人可选通道，实际 %v", got)
	}
}

// TestPrefsTurnOffAllEmail 关掉全部外发 = 只要站内信：
// 通知本体仍然入库（可回看），但一封邮件一条短信都不许发。
func TestPrefsTurnOffAllEmail(t *testing.T) {
	email := &stubSender{ch: "email"}
	sms := &stubSender{ch: notify.SMSChannel}
	st, sink := newSinkEnv(t, email, sms)
	addUser(t, st, "u-1", "ops@example.com")
	ctx := context.Background()
	tctx := store.WithTenant(ctx, "t1")

	if err := sink.SetChannels(tctx, "t1", "u-1", []string{}); err != nil {
		t.Fatalf("设置偏好失败: %v", err)
	}
	if err := sink.Handle(ctx, highEvent()); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	list := listOf(t, st, "u-1")
	if len(list) != 1 {
		t.Fatalf("站内通知必须仍然入库（关的是外发不是通知本身），实际 %d", len(list))
	}
	if strings.Join(list[0].Channels, ",") != "inapp" {
		t.Fatalf("关闭全部外发后通道应只剩 inapp，实际 %v", list[0].Channels)
	}
	if list[0].DeliveryStatus != notify.StatusNone {
		t.Fatalf("无外发通道时状态应为 none，实际 %s", list[0].DeliveryStatus)
	}
	if email.calls != 0 || sms.calls != 0 {
		t.Fatalf("关闭后不应有任何外发投递: email=%d sms=%d", email.calls, sms.calls)
	}
}

// TestPrefsKeepsOnlyEmail 只留邮件：其余通道不能偷偷发出去。
func TestPrefsKeepsOnlyEmail(t *testing.T) {
	email := &stubSender{ch: "email"}
	sms := &stubSender{ch: notify.SMSChannel}
	st, sink := newSinkEnv(t, email, sms)
	addUser(t, st, "u-1", "ops@example.com")
	ctx := context.Background()

	if err := sink.SetChannels(store.WithTenant(ctx, "t1"), "t1", "u-1", []string{"email", "inapp"}); err != nil {
		t.Fatalf("设置偏好失败（inapp 应被静默忽略而不是报错）: %v", err)
	}
	if err := sink.Handle(ctx, highEvent()); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	if strings.Join(listOf(t, st, "u-1")[0].Channels, ",") != "inapp,email" {
		t.Fatalf("通道应为 inapp,email，实际 %v", listOf(t, st, "u-1")[0].Channels)
	}
	if email.calls != 1 || sms.calls != 0 {
		t.Fatalf("只应发邮件: email=%d sms=%d", email.calls, sms.calls)
	}
}

// TestPrefsPerTenantAndPerUser 偏好按「人 × 团队」隔离：同一个人换个团队偏好不跟着跑，
// 别人在同一个团队也不受影响。
func TestPrefsPerTenantAndPerUser(t *testing.T) {
	email := &stubSender{ch: "email"}
	st, sink := newSinkEnv(t, email)
	addUser(t, st, "u-1", "ops@example.com")
	addUser(t, st, "u-2", "two@example.com")
	ctx := context.Background()

	if err := sink.SetChannels(store.WithTenant(ctx, "t1"), "t1", "u-1", []string{}); err != nil {
		t.Fatalf("设置偏好失败: %v", err)
	}
	// u-2 没设过 → 默认全发
	if err := sink.Handle(ctx, highEvent()); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	ev2 := highEvent()
	ev2.Actor.UserID = "u-2"
	if err := sink.Handle(ctx, ev2); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	if listOf(t, st, "u-1")[0].Channels[0] != "inapp" || len(listOf(t, st, "u-1")[0].Channels) != 1 {
		t.Fatalf("u-1 应仅站内信，实际 %v", listOf(t, st, "u-1")[0].Channels)
	}
	if len(listOf(t, st, "u-2")[0].Channels) != 2 {
		t.Fatalf("u-2 未设置应按默认外发，实际 %v", listOf(t, st, "u-2")[0].Channels)
	}
	// 同一个人换个团队：偏好不继承
	if got := sink.EffectiveChannels(store.WithTenant(ctx, "t9"), "t9", "u-1"); len(got) != 1 {
		t.Fatalf("换团队后应回到默认，实际 %v", got)
	}
}

// TestPrefsRejectsUnknownChannel 未知通道名必须报错：
// 拼错通道名却静默生效，用户会以为自己关掉了某个通道。
func TestPrefsRejectsUnknownChannel(t *testing.T) {
	_, sink := newSinkEnv(t)
	ctx := context.Background()
	if err := sink.SetChannels(store.WithTenant(ctx, "t1"), "t1", "u-1", []string{"wecom"}); err == nil {
		t.Fatal("未知通道应被拒绝")
	}
	if _, err := notify.NormalizeChannels([]string{"sms", "sms", "email"}); err != nil {
		t.Fatalf("重复值不应报错，应去重: %v", err)
	}
	out, err := notify.NormalizeChannels([]string{"inapp", "email"})
	if err != nil {
		t.Fatalf("inapp 应被静默忽略: %v", err)
	}
	if len(out) != 1 || out[0] != "email" {
		t.Fatalf("inapp 不该出现在偏好里: %v", out)
	}
}

// TestPrefsAffectsRetry 重投也要遵守**当下**的偏好：
// 排队期间用户关掉了邮件，这一轮就不能再把它发出去。
func TestPrefsAffectsRetry(t *testing.T) {
	email := &stubSender{ch: "email"}
	hook := &stubSender{ch: "webhook", err: context.DeadlineExceeded}
	st, sink := newSinkEnv(t, email, hook)
	addUser(t, st, "u-1", "ops@example.com")
	ctx := context.Background()
	tctx := store.WithTenant(ctx, "t1")

	if err := sink.Handle(ctx, highEvent()); err != nil {
		t.Fatalf("Handle 失败: %v", err)
	}
	// 此时邮件已发出（webhook 抖动失败），用户在两轮之间反悔了
	if err := sink.SetChannels(tctx, "t1", "u-1", []string{}); err != nil {
		t.Fatalf("设置偏好失败: %v", err)
	}
	before := email.calls
	if _, _, err := sink.RetryTenant(ctx, "t1"); err != nil {
		t.Fatalf("重投失败: %v", err)
	}
	if email.calls != before {
		t.Fatalf("偏好变更后重投不应再发邮件: before=%d after=%d", before, email.calls)
	}
	if hook.calls < 1 {
		t.Fatal("重投仍应尝试未送达的 webhook 通道")
	}
}

// TestAvailableChannelsExcludesWebhook webhook 是服务端集成通道，
// 不该出现在个人开关里——否则"这条告警有没有进 SIEM"取决于某个人的勾勾选选。
func TestAvailableChannelsExcludesWebhook(t *testing.T) {
	_, sink := newSinkEnv(t, &stubSender{ch: "webhook"}, &stubSender{ch: "email"})
	avail := sink.AvailableChannels()
	for _, ch := range avail {
		if ch == "webhook" {
			t.Fatalf("webhook 不可进个人偏好: %v", avail)
		}
	}
	if len(avail) != 1 || avail[0] != "email" {
		t.Fatalf("可选清单应为 [email]，实际 %v", avail)
	}
}
