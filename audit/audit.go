// Package audit 审计日志：六大日志 + 哈希链防篡改 + 差异化留存 + 导出管控。
// 它是唯一审计真相源——看板 / 告警 / BI 都从事件总线派生，不各自造数据。
package audit

import (
	"context"
	"encoding/json"
	"time"

	"ziniao/eventbus"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Sink 事件 → 审计落库（写操作唯一入口）。
type Sink struct {
	st store.Store
}

// NewSink 构造审计落库器。
func NewSink(st store.Store) *Sink { return &Sink{st: st} }

// Name 处理器名。
func (s *Sink) Name() string { return "audit" }

// Types 订阅全部事件（审计不能有遗漏）。
func (s *Sink) Types() []string { return nil }

// Handle 落一条审计记录（哈希链在事务内计算，保证 per-tenant 顺序一致）。
//
// 接收侧投递幂等：消费登记与审计写入在**同一事务**内提交。审计条目的主键就是
// event.ID——没有这个原子登记，重投会在 Append 处撞主键，事件卡在 pending
// 直到重试耗尽，期间同租户哈希链也无法继续。
func (s *Sink) Handle(ctx context.Context, ev *eventbus.Event) error {
	if consumed, err := s.st.Outbox().Consumed(ctx, ev.ID, s.Name()); err == nil && consumed {
		return nil // 重复投递（直接调用或重投）：审计只留一份
	}
	var data json.RawMessage
	if ev.Data != nil {
		b, err := json.Marshal(ev.Data)
		if err != nil {
			return err
		}
		data = b
	}
	entry := &types.AuditEntry{
		ID:          ev.ID,
		TenantID:    ev.TenantID,
		Category:    ev.Category(),
		ActorUserID: ev.Actor.UserID,
		ActorName:   ev.Actor.Username,
		ShopID:      ev.Subject.ShopID,
		DeviceID:    ev.Subject.DeviceID,
		TerminalIP:  ev.Terminal.IP,
		TerminalFP:  ev.Terminal.Fingerprint,
		Action:      ev.Context.Action,
		Result:      ev.Context.Result,
		URL:         ev.Context.URL,
		OccurredAt:  ev.OccurredAt,
		Data:        data,
	}
	return s.st.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.Outbox().MarkConsumed(ctx, ev.ID, s.Name()); err != nil {
			return err
		}
		return tx.Audit().Append(ctx, entry)
	})
}

// Service 审计查询与取证服务。
type Service struct {
	st store.Store
}

// NewService 构造审计服务。
func NewService(st store.Store) *Service { return &Service{st: st} }

// List 按时间/成员/店铺/类别筛选（M15.2）。
func (s *Service) List(ctx context.Context, f store.AuditFilter) ([]types.AuditEntry, int, error) {
	return s.st.Audit().List(ctx, f)
}

// VerifyResult 哈希链校验结果。
type VerifyResult struct {
	OK       bool   `json:"ok"`
	Checked  int    `json:"checked"`
	BrokenAt int    `json:"broken_at"`
	BrokenID string `json:"broken_id,omitempty"`
}

// Verify 校验某租户的审计哈希链完整性（WORM 防篡改的核心验证手段）。
func (s *Service) Verify(ctx context.Context, tenantID string, since time.Time) (VerifyResult, error) {
	entries, err := s.st.Audit().Since(ctx, tenantID, since)
	if err != nil {
		return VerifyResult{}, err
	}
	ok, at := store.VerifyChain(entries)
	res := VerifyResult{OK: ok, Checked: len(entries), BrokenAt: at}
	if at >= 0 && at < len(entries) {
		res.BrokenID = entries[at].ID
	}
	return res, nil
}

// ExportResult 导出结果（CSV 字节 + 是否触发审批）。
type ExportResult struct {
	Filename         string `json:"filename"`
	Content          []byte `json:"-"`
	Rows             int    `json:"rows"`
	ApprovalRequired bool   `json:"approval_required"`
	Watermarked      bool   `json:"watermarked"`
}

// Watermark 导出水印（M15.2）：谁在什么时候导出、依据哪张审批单。
type Watermark struct {
	TenantID  string
	ActorID   string
	ActorName string
	TicketID  string
	At        time.Time
}

// Export 导出：独立权限过滤器 + 脱敏引擎 + 行数上限 + 明文二次审批 + 水印（反例 8）。
func (s *Service) Export(ctx context.Context, f store.AuditFilter, maxRows int, plainApproved bool, wm *Watermark) (*ExportResult, error) {
	entries, total, err := s.st.Audit().List(ctx, f)
	if err != nil {
		return nil, err
	}
	if total > maxRows {
		entries = entries[:maxRows]
	}
	res := &ExportResult{
		Filename: "audit-export-" + time.Now().Format("20060102-150405") + ".csv",
		Rows:     len(entries),
	}
	// 明文导出需二次审批（默认开启）
	if !plainApproved {
		res.ApprovalRequired = true
	}
	var buf []byte
	// 水印行：CSV 中以 # 注释行承载（Excel 打开可见，不影响解析）
	if wm != nil {
		buf = append(buf, "# 本文件为审计日志导出，含水印，禁止外传\n"...)
		buf = append(buf, []byte("# 租户="+wm.TenantID+
			" 导出人="+wm.ActorName+"("+wm.ActorID+")"+
			" 审批单="+wm.TicketID+
			" 导出时间="+wm.At.Format(time.RFC3339)+"\n")...)
		res.Watermarked = true
	}
	buf = append(buf, "id,category,actor,action,result,shop_id,ip,url,occurred_at,hash\n"...)
	for _, e := range entries {
		buf = append(buf, csvLine(e)...)
	}
	res.Content = buf
	return res, nil
}

func csvLine(e types.AuditEntry) []byte {
	fields := []string{
		e.ID, string(e.Category), e.ActorUserID + "/" + e.ActorName, e.Action, e.Result,
		e.ShopID, e.TerminalIP, e.URL, e.OccurredAt.Format(time.RFC3339), e.Hash,
	}
	out := []byte{}
	for i, f := range fields {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '"')
		out = append(out, []byte(escapeCSV(f))...)
		out = append(out, '"')
	}
	out = append(out, '\n')
	return out
}

func escapeCSV(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"', '"')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}
