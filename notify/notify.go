// Package notify 通知中心：站内 / 邮件 / 短信 / 企业微信 / 飞书 / 钉钉 统一模板与订阅。
// Phase 0 落地：站内 + console 驱动；其余通道为 TODO（P1 接入真实服务商）。
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"ziniao/eventbus"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Message 通知消息。
type Message struct {
	TenantID  string
	UserID    string
	Category  string // system|security|billing|task
	Title     string
	Body      string
	Channels  []string
	EventType string
}

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
}

// NewSink 构造通知落库与分发器。
func NewSink(st store.Store, log *slog.Logger, senders ...Sender) *Sink {
	if log == nil {
		log = slog.Default()
	}
	if len(senders) == 0 {
		senders = []Sender{ConsoleSender{log: log}}
	}
	return &Sink{st: st, senders: senders, log: log}
}

// Name 处理器名。
func (s *Sink) Name() string { return "notify" }

// Types 订阅范围：风险类、部分 IAM 事件，以及**审批待办**（TODO 第 6 条）。
func (s *Sink) Types() []string {
	return []string{"risk.", "auth.login_failed", "shop.deleted", "iam.role.", "iam.temp_grant.", "usage.recorded", "iam.approval."}
}

// Handle 生成站内通知并外发到已启用通道。
func (s *Sink) Handle(ctx context.Context, ev *eventbus.Event) error {
	if ev.Risk == eventbus.RiskLow {
		return nil
	}
	tpl := TemplateOf(ev.Type)
	// 审批类事件走定向推送：待办必须落到"真能批这批人的人"手上。
	// 否则会出现全团队都收到提示、大多数人点进去才发现无权处理的噪音。
	targets := s.resolveTargets(ctx, ev)
	if ids, ok := approverIDsOf(ev); ok {
		targets = ids
	}
	now := time.Now().UTC()
	for _, userID := range targets {
		n := &types.Notification{
			ID: ids.New(), TenantID: ev.TenantID, UserID: userID,
			Category: categoryOf(ev), Title: tpl.Title,
			Body:      fmt.Sprintf("%s（%s，结果=%s，终端IP=%s）", tpl.Body, ev.Context.Action, ev.Context.Result, ev.Terminal.IP),
			EventType: ev.Type, Channels: []string{"inapp"}, CreatedAt: now,
		}
		if ev.Risk == eventbus.RiskHigh {
			n.Channels = append(n.Channels, "console")
		}
		// 通知写入受 RLS 保护，必须在带租户上下文的事务内完成
		if err := s.st.Notifications().Create(store.WithTenant(ctx, ev.TenantID), n); err != nil {
			return err
		}
		for _, sender := range s.senders {
			for _, ch := range n.Channels {
				if sender.Channel() != ch {
					continue
				}
				if err := sender.Send(ctx, Message{
					TenantID: ev.TenantID, UserID: userID, Category: n.Category,
					Title: n.Title, Body: n.Body, Channels: n.Channels, EventType: ev.Type,
				}); err != nil {
					s.log.Error("通知外发失败", "channel", ch, "err", err)
				}
			}
		}
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

// MarkRead 标记已读。
func (s *Service) MarkRead(ctx context.Context, tenantID, id string) error {
	return s.st.Notifications().MarkRead(ctx, tenantID, id)
}
