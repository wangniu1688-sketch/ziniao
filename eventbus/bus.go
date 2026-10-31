package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Handler 事件处理器。Types 支持前缀通配（"shop." 或 "shop.*"）；为空表示订阅全部。
type Handler interface {
	Name() string
	Types() []string
	Handle(ctx context.Context, ev *Event) error
}

// Bus 事件总线：写路径只做「同事务入发件箱」，异步派发给所有 Handler。
type Bus struct {
	st       store.Store
	cfg      *config.Config
	log      *slog.Logger
	handlers []Handler
}

// New 构造总线。
func New(st store.Store, cfg *config.Config, log *slog.Logger) *Bus {
	if log == nil {
		log = slog.Default()
	}
	return &Bus{st: st, cfg: cfg, log: log}
}

// Register 注册处理器。
func (b *Bus) Register(h Handler) { b.handlers = append(b.handlers, h) }

// Emit 在业务事务内投递事件（必须与业务写同事务，否则会出现「写了但没审计」）。
func (b *Bus) Emit(ctx context.Context, tx store.Tx, ev Event) error {
	if ev.ID == "" {
		ev.ID = ids.New()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	if ev.Context.Result == "" {
		ev.Context.Result = "success"
	}
	if ev.Risk == "" {
		ev.Risk = RiskLow
	}
	if ev.TenantID == "" {
		tid, err := store.TenantFrom(ctx)
		if err != nil {
			return err
		}
		ev.TenantID = tid
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("eventbus: 事件序列化失败: %w", err)
	}
	return tx.Outbox().Insert(ctx, &types.OutboxEnvelope{
		ID:          ev.ID,
		TenantID:    ev.TenantID,
		Type:        ev.Type,
		Payload:     payload,
		Status:      "pending",
		AvailableAt: ev.OccurredAt,
		CreatedAt:   ev.OccurredAt,
	})
}

// Start 启动派发循环，直到 ctx 取消。
func (b *Bus) Start(ctx context.Context) {
	ticker := time.NewTicker(b.cfg.DispatcherInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := b.DispatchOnce(ctx); err != nil {
				b.log.Error("事件派发失败", "err", err)
			}
		}
	}
}

// DispatchOnce 拉取一批待处理事件并派发，返回成功处理条数。
func (b *Bus) DispatchOnce(ctx context.Context) (int, error) {
	batch, err := b.st.Outbox().Claim(ctx, b.cfg.DispatcherBatchSize)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, env := range batch {
		var ev Event
		if err := json.Unmarshal(env.Payload, &ev); err != nil {
			_ = b.st.Outbox().Fail(ctx, env.ID, err.Error())
			continue
		}
		ectx := store.WithTenant(ctx, env.TenantID)
		handled := true
		for _, h := range b.handlers {
			if !matchType(h.Types(), ev.Type) {
				continue
			}
			if err := h.Handle(ectx, &ev); err != nil {
				handled = false
				b.log.Error("事件处理器失败", "handler", h.Name(), "type", ev.Type, "err", err)
				break
			}
		}
		if !handled {
			if env.Attempts+1 >= b.cfg.DispatcherMaxRetry {
				_ = b.st.Outbox().Fail(ctx, env.ID, "超过最大重试次数")
			} else {
				_ = b.st.Outbox().Fail(ctx, env.ID, "处理器失败，等待重试")
			}
			continue
		}
		if err := b.st.Outbox().Ack(ctx, env.ID); err != nil {
			b.log.Error("事件确认失败", "id", env.ID, "err", err)
			continue
		}
		done++
	}
	return done, nil
}

// matchType 支持 "shop.*" / "shop." 前缀通配与精确匹配。
func matchType(patterns []string, typ string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		p = strings.TrimSuffix(p, "*")
		if p == "" || typ == p || strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}
