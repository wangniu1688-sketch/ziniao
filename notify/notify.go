// Package notify 通知中心：站内 / 邮件 / 短信 / 企业微信 / 飞书 / 钉钉 统一模板与订阅。
// Phase 0 落地：站内 + console 驱动；其余通道为 TODO（P1 接入真实服务商）。
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ziniao/eventbus"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Message 通知消息。
type Message struct {
	ID        string // 关联站内通知行的主键（投递结果回写用）
	TenantID  string
	UserID    string
	Category  string // system|security|billing|task
	Title     string
	Body      string
	Channels  []string
	EventType string
	To        string // 收件邮箱（邮件通道必需；查不到用户邮箱时为空）
	Phone     string // 收件手机号（短信通道必需；查不到或格式非法时按终态处理）
	CreatedAt time.Time
}

// 投递状态取值（migration 0006 的 CHECK 约束与之对应）。
const (
	StatusNone    = "none"    // 无外发通道或通道全被跳过
	StatusPending = "pending" // 待投递 / 投递中
	StatusSent    = "sent"    // 所有需要的通道均已送达
	StatusFailed  = "failed"  // 仍有通道未送达（重投任务的输入）
)

// ChannelInApp 站内通道名。
const ChannelInApp = "inapp"

// Sender 通道驱动。
type Sender interface {
	Channel() string
	Send(ctx context.Context, m Message) error
}

// ConsoleSender 控制台驱动（dev 环境替代邮件/IMAP 等真实通道）。
type ConsoleSender struct{ log *slog.Logger }

// Channel 通道名。
func (c ConsoleSender) Channel() string { return "console" }

// Send 打印到日志（L4：禁止把敏感值写入消息体）。
func (c ConsoleSender) Send(_ context.Context, m Message) error {
	if c.log != nil {
		c.log.Info("[notify] 外发通知", "channel", c.Channel(), "user", m.UserID, "title", m.Title)
	}
	return nil
}

// Sink 事件 → 通知（中高风险事件才打扰人）。
type Sink struct {
	st      store.Store
	senders []Sender
	log     *slog.Logger
	policy  config.Notify // 外发策略（最低风险等级 + 重投上限）
}

// NewSink 构造通知落库与分发器。
//
// 不传 senders 时退回 console 驱动：dev/单测环境与改造前行为完全一致，
// 不会因为没有配邮件就损失任何东西。
func NewSink(st store.Store, log *slog.Logger, senders ...Sender) *Sink {
	if log == nil {
		log = slog.Default()
	}
	if len(senders) == 0 {
		senders = []Sender{ConsoleSender{log: log}}
	}
	return &Sink{st: st, senders: senders, log: log}
}

// UseConfig 应用外部通道策略。
//
// 为什么整段搬过来而不是抄两个字段：重投上限与最低风险等级各自都有默认值，
// 一旦两边各写一份兜底值，改配置就会只有一半生效（申购阈值那次就是这么踩的）。
func (s *Sink) UseConfig(cfg *config.Config) *Sink {
	if cfg != nil {
		s.policy = cfg.Notify
	}
	return s
}

// externalChannels 当前装配的所有外发通道（站内通道不算）。
func (s *Sink) externalChannels() []string {
	out := []string{}
	for _, sd := range s.senders {
		if sd == nil {
			continue
		}
		if ch := sd.Channel(); ch != "" && ch != ChannelInApp {
			out = append(out, ch)
		}
	}
	return out
}

func (s *Sink) minRiskJudge(risk string) bool {
	c := config.Config{Notify: s.policy}
	if s.policy.MinRisk == "" { // 未注入配置时用与 config.Default 同一口径
		c.Notify.MinRisk = config.Default().Notify.MinRisk
	}
	return c.NotifyDeliverable(risk)
}

func (s *Sink) attemptsLimit() int { return s.policy.AttemptsLimit() }

// RetryTenant 重投该租户未送达的通知（`notify.delivery_retry` 任务入口）。
//
// 返回 (本轮成功送达条数, 已耗尽次数仍失败的条数)。
// 只发「所需通道 ∩ 已装配通道 - 已送达通道」的差集：
// 邮件已经发到对方邮箱了，却因为 Webhook 抖动再发一封，是最伤用户信任的做法。
func (s *Sink) RetryTenant(ctx context.Context, tenantID string) (int, int, error) {
	list, err := s.st.Notifications().ListPendingDelivery(store.WithTenant(ctx, tenantID), tenantID, 0)
	if err != nil {
		return 0, 0, err
	}
	ext := s.externalChannels()
	sentNow, givenUp := 0, 0
	for i := range list {
		n := list[i]
		remaining := pendingChannels(n.Channels, n.DeliveredChannels, ext)
		// 再按**当下**的偏好收敛一次：用户在通知排队期间关掉了邮件，
		// 这一条就只该走剩余其它通道（或什么都不发），而不是照着落库时的旧清单继续发。
		remaining = s.channelsFor(store.WithTenant(ctx, tenantID), tenantID, n.UserID, remaining)
		switch {
		case len(remaining) == 0:
			// 通道全部送达但状态还没翻过来（上一次进程在写回前退出了）
			n.DeliveryStatus = StatusSent
			n.DeliveryNote = ""
		case n.Attempts >= s.attemptsLimit():
			n.DeliveryStatus = StatusFailed
			n.DeliveryNote = truncate(fmt.Sprintf("已重试 %d 次仍未送达: %s", n.Attempts, strings.Join(remaining, ",")), 300)
			givenUp++
		default:
			s.dispatch(ctx, &n, remaining)
			if n.DeliveryStatus == StatusSent {
				sentNow++
			} else {
				givenUp++
			}
		}
		if err := s.st.Notifications().UpdateDelivery(store.WithTenant(ctx, tenantID), &n); err != nil {
			// 单条写失败不影响其它行：把它留给下一轮，而不是让整个租户的重投停摆
			s.log.Error("投递状态回写失败", "id", n.ID, "err", err)
		}
	}
	return sentNow, givenUp, nil
}

// pendingChannels 尚未送达且当前仍装配了的外发通道。
func pendingChannels(want, delivered, installed []string) []string {
	done := map[string]bool{}
	for _, c := range delivered {
		done[c] = true
	}
	avail := map[string]bool{}
	for _, c := range installed {
		avail[c] = true
	}
	out := []string{}
	for _, c := range want {
		if c == ChannelInApp || done[c] || !avail[c] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// dispatch 按通道逐个投递并回写状态。
//
// Handle 不对投递失败返回 error：通知已经入库了，返回 error 会让 outbox
// 重投**整条事件**、于是再生成一条一模一样的站内通知。投递失败的正确归宿是
// 这一行的 delivery_status=failed，交给重投任务处理。
func (s *Sink) dispatch(ctx context.Context, n *types.Notification, only []string) {
	if len(only) == 0 {
		return
	}
	now := time.Now().UTC()
	n.Attempts++
	n.LastDeliveryAt = &now
	notes := []string{}
	for _, ch := range only {
		var sd Sender
		for _, cand := range s.senders {
			if cand != nil && cand.Channel() == ch {
				sd = cand
				break
			}
		}
		if sd == nil {
			continue // 通道已卸载（配置被撤），不算失败
		}
		m := Message{
			ID: n.ID, TenantID: n.TenantID, UserID: n.UserID, Category: n.Category,
			Title: n.Title, Body: n.Body, Channels: n.Channels, EventType: n.EventType,
			CreatedAt: n.CreatedAt,
		}
		switch ch {
		case "email":
			m.To = recipientOf(ctx, s.st, n.UserID)
		case SMSChannel:
			m.Phone = phoneOf(ctx, s.st, n.UserID)
		}
		err := sd.Send(ctx, m)
		switch {
		case err == nil:
			n.DeliveredChannels = append(n.DeliveredChannels, ch)
		case errors.Is(err, ErrNoRecipient):
			// 收件人为空是**终态**而非失败：用户没填邮箱，再重试一百次也没有。
			// 计入 DeliveredChannels（语义为「该通道已终结」）以免它一直挂在板上。
			s.log.Info("收件人为空，跳过该通道", "channel", ch, "user", n.UserID, "id", n.ID)
			n.DeliveredChannels = append(n.DeliveredChannels, ch)
		default:
			s.log.Error("通知外发失败", "channel", ch, "id", n.ID, "err", err)
			notes = append(notes, fmt.Sprintf("%s: %s", ch, err))
		}
	}
	switch {
	case len(notes) > 0:
		n.DeliveryStatus = StatusFailed
		n.DeliveryNote = truncate(strings.Join(notes, "；"), 300)
	case len(n.DeliveredChannels) > 0:
		n.DeliveryStatus = StatusSent
		n.DeliveryNote = ""
	default:
		n.DeliveryStatus = StatusNone
	}
	// 结果必须回写，否则「发了没有」永远查不到。写失败只记日志：
	// 通知本体已经入库，重投任务下一轮会兜住。
	if err := s.st.Notifications().UpdateDelivery(ctx, n); err != nil {
		s.log.Error("投递状态回写失败", "id", n.ID, "err", err)
	}
}

// recipientOf 取用户邮箱；查不到就返回空（调用方据此跳过邮件通道）。
func recipientOf(ctx context.Context, st store.Store, userID string) string {
	u, err := st.Users().GetByID(ctx, userID)
	if err != nil || u == nil {
		return ""
	}
	return strings.TrimSpace(u.Email)
}

// phoneOf 取用户手机号；查不到就返回空（短信通道据此走 ErrNoRecipient 终态）。
func phoneOf(ctx context.Context, st store.Store, userID string) string {
	u, err := st.Users().GetByID(ctx, userID)
	if err != nil || u == nil {
		return ""
	}
	return strings.TrimSpace(u.Phone)
}

// Name 处理器名。
func (s *Sink) Name() string { return "notify" }

// Types 订阅范围：风险类、部分 IAM 事件，以及**审批待办**（TODO 第 6 条）。
func (s *Sink) Types() []string {
	return []string{"risk.", "auth.login_failed", "shop.deleted", "iam.role.", "iam.temp_grant.", "usage.recorded", "iam.approval."}
}

// Handle 生成站内通知并外发到已启用通道。
//
// 接收侧投递幂等（event_consumptions）：发件箱重投会让这条事件整个重跑一遍，
// 每个收件人的「消费登记 + 站内通知落库」必须在**同一事务**内原子提交——
// 只重跑没登记过的收件人，已通知的人不会收到第二封。
func (s *Sink) Handle(ctx context.Context, ev *eventbus.Event) error {
	if ev.Risk == eventbus.RiskLow {
		return nil
	}
	if consumed, err := s.st.Outbox().Consumed(ctx, ev.ID, s.Name()); err == nil && consumed {
		return nil // 整条事件已处理完（重投或重复调用）
	}
	tpl := TemplateOf(ev.Type)
	// 审批类事件走定向推送：待办必须落到"真能批这批人的人"手上。
	// 否则会出现全团队都收到提示、大多数人点进去才发现无权处理的噪音。
	targets := s.resolveTargets(ctx, ev)
	if ids, ok := approverIDsOf(ev); ok {
		targets = ids
	}
	now := time.Now().UTC()
	// 外发通道按「当前装了什么」决定：没配邮件/Webhook 时清单里只有 console，
	// 与改造前的行为一致（high 事件落 [inapp, console]）。
	ext := s.externalChannels()
	need := []string{}
	if s.minRiskJudge(string(ev.Risk)) {
		need = append(need, ext...)
	}
	for _, userID := range targets {
		// 逐收件人幂等键：中途崩溃重投时，已登记的人跳过，没登记的人补上。
		// 查询失败不吞：当成未处理重跑，宁可依赖登记表兜底，也不静默丢通知。
		mark := s.Name() + ":" + userID
		if consumed, err := s.st.Outbox().Consumed(ctx, ev.ID, mark); err == nil && consumed {
			continue
		}
		// 外发清单再按「这个人在本团队的偏好」收敛一次（语义与边界见 prefs.go）。
		// 收敛必须发生在**落库之前**：Channels 写进表里就是重投任务的输入，
		// 若按全通道落库再靠重投过滤，用户刚关掉的通道会在重投那一轮复活。
		chs := s.channelsFor(store.WithTenant(ctx, ev.TenantID), ev.TenantID, userID, need)
		n := &types.Notification{
			ID: ids.New(), TenantID: ev.TenantID, UserID: userID,
			Category: categoryOf(ev), Title: tpl.Title,
			Body:      fmt.Sprintf("%s（%s，结果=%s，终端IP=%s）", tpl.Body, ev.Context.Action, ev.Context.Result, ev.Terminal.IP),
			EventType: ev.Type, Channels: append([]string{ChannelInApp}, chs...), CreatedAt: now,
			DeliveryStatus: StatusNone,
		}
		if len(chs) > 0 {
			n.DeliveryStatus = StatusPending
		}
		// 通知写入受 RLS 保护，必须在带租户上下文的事务内完成；
		// 消费登记与通知落库同事务：登记提交了通知必然在，重投不会补发第二封。
		if err := s.st.WithTx(store.WithTenant(ctx, ev.TenantID), func(tx store.Tx) error {
			if err := tx.Outbox().MarkConsumed(ctx, ev.ID, mark); err != nil {
				return err
			}
			return tx.Notifications().Create(store.WithTenant(ctx, ev.TenantID), n)
		}); err != nil {
			return err
		}
		// 外发失败不向上抛错：站内通知已入库，抛错会让 outbox 重投整条事件、
		// 于是再生成一条一模一样的站内通知。失败留在 delivery_status 上，
		// 由 notify.delivery_retry 继续。
		s.dispatch(store.WithTenant(ctx, ev.TenantID), n, chs)
	}
	// 全部收件人处理完：登记整条事件的完成标记（bus 层也会登记，双保险幂等）。
	if err := s.st.Outbox().MarkConsumed(ctx, ev.ID, s.Name()); err != nil {
		s.log.Warn("通知事件完成标记写入失败（不影响本轮投递）", "id", ev.ID, "err", err)
	}
	return nil
}

// approverIDsOf 取出事件里定向的审批人名单。
// 事件经 outbox JSON 往返后是 []any{string}，同进程直接投递时可能是 []string，两种都要认。
func approverIDsOf(ev *eventbus.Event) ([]string, bool) {
	if len(ev.Data) == 0 {
		return nil, false
	}
	raw, ok := ev.Data["approver_ids"]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case []string:
		out := []string{}
		for _, id := range v {
			if id != "" {
				out = append(out, id)
			}
		}
		return out, len(out) > 0
	case []any:
		out := []string{}
		for _, item := range v {
			if id, ok := item.(string); ok && id != "" {
				out = append(out, id)
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}

// resolveTargets 通知对象：操作人本人 + 租户内老板/运营主管（安全事件需管理员知晓）。
func (s *Sink) resolveTargets(ctx context.Context, ev *eventbus.Event) []string {
	seen := map[string]bool{}
	out := []string{}
	if ev.Actor.UserID != "" {
		seen[ev.Actor.UserID] = true
		out = append(out, ev.Actor.UserID)
	}
	urs, err := s.st.UserRoles().ListByTenant(ctx, ev.TenantID)
	if err != nil {
		return out
	}
	for _, ur := range urs {
		role, rErr := s.st.Roles().Get(ctx, ur.RoleID)
		if rErr != nil || role == nil {
			continue
		}
		if role.Code != "owner" && role.Code != "ops_manager" {
			continue
		}
		if !seen[ur.UserID] {
			seen[ur.UserID] = true
			out = append(out, ur.UserID)
		}
	}
	return out
}

func categoryOf(ev *eventbus.Event) string {
	switch ev.Category() {
	case "risk", "login":
		return "security"
	case "grant":
		return "system"
	default:
		return "task"
	}
}

// Service 站内消息查询。
type Service struct {
	st store.Store
}

// NewService 构造通知服务。
func NewService(st store.Store) *Service { return &Service{st: st} }

// List 站内消息列表。
func (s *Service) List(ctx context.Context, tenantID, userID string, limit int) ([]types.Notification, error) {
	return s.st.Notifications().List(ctx, tenantID, userID, limit)
}

// UnreadCount 未读条数（角标专用：由 SQL COUNT 得出总量，
// 不是把最近 N 条拉回来数——后者会给出一个会被 limit 截断的假数字）。
func (s *Service) UnreadCount(ctx context.Context, tenantID, userID string) (int, error) {
	return s.st.Notifications().CountUnread(ctx, tenantID, userID)
}

// MarkRead 标记已读。
func (s *Service) MarkRead(ctx context.Context, tenantID, id string) error {
	return s.st.Notifications().MarkRead(ctx, tenantID, id)
}
