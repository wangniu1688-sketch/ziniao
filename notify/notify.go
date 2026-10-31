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

// Types 只订阅风险相关与任务类事件。
func (s *Sink) Types() []string {
	return []string{"risk.", "auth.login_failed", "shop.deleted", "iam.role.", "usage.recorded"}
}

// Handle 生成站内通知并外发到已启用通道。
func (s *Sink) Handle(ctx context.Context, ev *eventbus.Event) error {
	if ev.Risk == eventbus.RiskLow {
		return nil
	}
	tpl := TemplateOf(ev.Type)
	targets := s.resolveTargets(ctx, ev)
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
