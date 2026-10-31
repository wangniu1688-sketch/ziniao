// Package approval 审批工单：高危动作的二次审批（反例 8「只灰按钮不算控制」）。
//
// 铁律：
//  1. 申请人与审批人必须不同人（L3 职责分离，自己批自己等于没审批）；
//  2. 审批单有有效期（默认 30 分钟），过期必须重新申请；
//  3. 审批通过后一次性核销（used），不能拿一张单反复看明文；
//  4. 创建 / 审批 / 拒绝 / 核销全部落审计（L5），事件前缀 iam.* 归入授权类日志。
package approval

import (
	"context"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// 事件类型：归入「授权类」审计（永久留存）。
const (
	TypeCreated  = "iam.approval.created"
	TypeApproved = "iam.approval.approved"
	TypeRejected = "iam.approval.rejected"
	TypeCancel   = "iam.approval.cancelled"
	TypeExpired  = "iam.approval.expired"
	TypeUsed     = "iam.approval.used"
	// TypeSigned 会签中的一次签署（尚未达法定人数，工单仍是 pending）。
	TypeSigned = "iam.approval.signed"
)

// Service 审批服务。
type Service struct {
	st     store.Store
	bus    *eventbus.Bus
	cfg    *config.Config
	engine *iam.Engine
}

// NewService 构造审批服务。
func NewService(st store.Store, bus *eventbus.Bus, cfg *config.Config, engine *iam.Engine) *Service {
	return &Service{st: st, bus: bus, cfg: cfg, engine: engine}
}

// CreateInput 提交审批单。
type CreateInput struct {
	Type        string
	SubjectType string
	SubjectID   string
	Reason      string
	Detail      map[string]any
	Actor       iam.Subject
	Terminal    eventbus.Terminal
	URL         string
}

// knownTypes 允许的审批类型（禁止客户端自定义绕过名单）。
var knownTypes = map[string]bool{
	types.ApprovalTypeAuditExport:    true,
	types.ApprovalTypeCredReveal:     true,
	types.ApprovalTypeDevicePurchase: true,
	types.ApprovalTypeBulkDelete:     true,
}

// Create 提交审批单。
func (s *Service) Create(ctx context.Context, in CreateInput) (*types.ApprovalTicket, error) {
	if !knownTypes[in.Type] {
		return nil, apierr.Validation("未知的审批类型：" + in.Type)
	}
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "approval:create", Path: in.URL})
	if !dec.Allow {
		return nil, apierr.Forbidden("无权提交审批单（" + string(dec.Layer) + "）")
	}
	ttl := s.cfg.Limits.ApprovalTTLSeconds
	if ttl <= 0 {
		ttl = 1800
	}
	now := time.Now().UTC()
	t := &types.ApprovalTicket{
		ID: ids.New(), TenantID: in.Actor.TenantID, Type: in.Type,
		SubjectType: in.SubjectType, SubjectID: in.SubjectID,
		RequesterID: in.Actor.UserID, RequesterName: in.Actor.Username,
		Reason: in.Reason, Status: types.ApprovalPending, Detail: in.Detail,
		ExpiresAt: now.Add(time.Duration(ttl) * time.Second), CreatedAt: now,
	}
	// 路由：算出这张单该由谁批。这里的名单用于待办定向推送——
	// 审批待办必须落到"真能批这批人的人"手上，否则会变成所有人收到通知、
	// 大多数点进去才发现无权处理。
	tctxApprovers := store.WithTenant(ctx, in.Actor.TenantID)
	approvers, _ := s.ResolveApprovers(tctxApprovers, in.Actor.TenantID, t)
	approverIDs := make([]string, 0, len(approvers))
	for _, a := range approvers {
		approverIDs = append(approverIDs, a.UserID)
	}
	req := s.RequiredFor(t)
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Approvals().Create(tctx, t); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: TypeCreated, OccurredAt: now, Risk: eventbus.RiskMedium,
			Actor:    eventbus.Actor{UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor)},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "approval.create", Result: "success"},
			Data: map[string]any{
				"ticket_id": t.ID, "type": t.Type, "subject_id": t.SubjectID,
				"approver_ids": approverIDs, "required_roles": req.Roles,
				"min_approvers": req.MinApprovers, "requirement": req.Reason,
			},
		})
	}); err != nil {
		return nil, err
	}
	return t, nil
}

// DecideInput 审批 / 拒绝 / 取消。
type DecideInput struct {
	TicketID string
	Comment  string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Approve 通过：审批人不能是申请人（L3）。
func (s *Service) Approve(ctx context.Context, in DecideInput) (*types.ApprovalTicket, error) {
	return s.decide(ctx, in, types.ApprovalApproved, TypeApproved, "approval.approve", "approval:approve")
}

// Reject 拒绝。
func (s *Service) Reject(ctx context.Context, in DecideInput) (*types.ApprovalTicket, error) {
	return s.decide(ctx, in, types.ApprovalRejected, TypeRejected, "approval.reject", "approval:approve")
}

// Cancel 申请人撤销自己的申请。
func (s *Service) Cancel(ctx context.Context, in DecideInput) (*types.ApprovalTicket, error) {
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	t, err := s.st.Approvals().Get(tctx, in.TicketID)
	if err != nil {
		return nil, err
	}
	if t.RequesterID != in.Actor.UserID {
		return nil, apierr.Forbidden("只能撤销本人提交的审批单")
	}
	if t.Status != types.ApprovalPending {
		return nil, apierr.Conflict("只有待审批的单据可撤销")
	}
	return s.finish(ctx, t, types.ApprovalCancelled, TypeCancel, "approval.cancel", in, eventbus.RiskLow)
}

func (s *Service) decide(ctx context.Context, in DecideInput, status types.ApprovalStatus,
	evType, action, perm string) (*types.ApprovalTicket, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: perm, Path: in.URL})
	if !dec.Allow {
		return nil, apierr.Forbidden("无权审批（" + string(dec.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	t, err := s.st.Approvals().Get(tctx, in.TicketID)
	if err != nil {
		return nil, err
	}
	if t.Status != types.ApprovalPending {
		return nil, apierr.Conflict("该审批单已处理（状态：" + string(t.Status) + "）")
	}
	if time.Now().UTC().After(t.ExpiresAt) {
		return nil, apierr.Conflict("审批单已过期，请重新提交")
	}
	// L3 职责分离的硬校验：自己不能批自己
	if t.RequesterID == in.Actor.UserID {
		return nil, apierr.Forbidden("申请人与审批人不能是同一人（职责分离）")
	}
	// 路由准入：光有 approval:approve 权限不够，还得是这条单要求的角色。
	// 否则一个普通运营就能批准「导出全量审计日志」「查看店铺主账号口令」。
	if err := s.CanApprove(t, in.Actor); err != nil {
		return nil, err
	}
	// 会签：未达法定人数前只记签署，工单保持 pending。
	if status == types.ApprovalApproved {
		if need := s.RequiredFor(t).MinApprovers; need > 1 {
			sigs, ok := addSignature(t, in.Actor)
			if !ok {
				return nil, apierr.Conflict("你已签署该审批单，请勿重复操作")
			}
			if len(sigs) < need {
				return s.recordSignature(ctx, t, in, len(sigs), need)
			}
		}
	}
	return s.finish(ctx, t, status, evType, action, in, eventbus.RiskHigh)
}

// recordSignature 记录一次会签签署（工单仍 pending）。
func (s *Service) recordSignature(ctx context.Context, t *types.ApprovalTicket,
	in DecideInput, got, need int) (*types.ApprovalTicket, error) {
	now := time.Now().UTC()
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Approvals().Update(tctx, t); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: t.TenantID, Type: TypeSigned, OccurredAt: now, Risk: eventbus.RiskMedium,
			Actor:    eventbus.Actor{UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor)},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "approval.sign", Result: "pending"},
			Data: map[string]any{
				"ticket_id": t.ID, "type": t.Type, "signed": got, "required": need,
				// 剩余待批的人 -> 通知可以继续定向推给他们
				"approver_ids": approverIDsExcept(t, in.Actor.UserID),
			},
		})
	}); err != nil {
		return nil, err
	}
	return t, nil
}

// approverIDsExcept 取已签发这张单的 involved 名单里除某人外的其他人（用于继续推送待办）。
func approverIDsExcept(t *types.ApprovalTicket, except string) []string {
	out := []string{}
	for _, s := range signaturesOf(t) {
		if s.UserID != except {
			out = append(out, s.UserID)
		}
	}
	if len(out) == 0 {
		out = append(out, t.RequesterID) // 无人可推时退回通知申请人进度
	}
	return out
}

func (s *Service) finish(ctx context.Context, t *types.ApprovalTicket, status types.ApprovalStatus,
	evType, action string, in DecideInput, risk eventbus.RiskLevel) (*types.ApprovalTicket, error) {
	now := time.Now().UTC()
	t.Status = status
	t.DecidedAt = &now
	if in.Actor.UserID != t.RequesterID {
		t.ApproverID = in.Actor.UserID
		t.ApproverName = in.Actor.Username
	}
	if in.Comment != "" {
		if t.Detail == nil {
			t.Detail = map[string]any{}
		}
		t.Detail["comment"] = in.Comment
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Approvals().Update(tctx, t); err != nil {
			return err
		}
		result := "success"
		if status == types.ApprovalRejected {
			result = "blocked"
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: t.TenantID, Type: evType, OccurredAt: now, Risk: risk,
			Actor:    eventbus.Actor{UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor)},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: action, Result: result},
			Data: map[string]any{
				"ticket_id": t.ID, "type": t.Type, "status": string(status),
				"requester_id": t.RequesterID, "approver_id": in.Actor.UserID,
			},
		})
	}); err != nil {
		return nil, err
	}
	return t, nil
}

// List 审批单列表。
func (s *Service) List(ctx context.Context, actor iam.Subject, f store.ApprovalFilter, path string) ([]types.ApprovalTicket, int, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "approval:read", Path: path})
	if !dec.Allow {
		return nil, 0, apierr.Forbidden("无权查看审批单（" + string(dec.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	return s.st.Approvals().List(tctx, f)
}

// Verify 校验审批单是否可用于某高危动作（导出 / 查看明文 / 批量删除）。
// 校验项：存在 + 同租户 + 类型匹配 + 申请人即操作人 + 已通过 + 未过期。
func (s *Service) Verify(ctx context.Context, tenantID, ticketID, typ, requesterID string) (*types.ApprovalTicket, error) {
	if ticketID == "" {
		return nil, apierr.Approval("该操作需二次审批：请先提交审批单并携带 ticket 重试")
	}
	t, err := s.st.Approvals().Get(store.WithTenant(ctx, tenantID), ticketID)
	if err != nil {
		return nil, apierr.Approval("审批单不存在或不属于当前团队")
	}
	if t.Type != typ {
		return nil, apierr.Approval("审批单类型不匹配")
	}
	if t.RequesterID != requesterID {
		return nil, apierr.Approval("审批单申请人必须是操作人本人")
	}
	switch t.Status {
	case types.ApprovalApproved:
	case types.ApprovalPending:
		return nil, apierr.Approval("审批单尚未通过")
	case types.ApprovalRejected:
		return nil, apierr.Approval("审批单已被拒绝")
	case types.ApprovalExpired:
		return nil, apierr.Approval("审批单已过期")
	case types.ApprovalUsed:
		return nil, apierr.Approval("审批单已核销（一次性使用）")
	default:
		return nil, apierr.Approval("审批单状态不可用")
	}
	if time.Now().UTC().After(t.ExpiresAt) {
		return nil, apierr.Approval("审批单已过期，请重新提交")
	}
	return t, nil
}

// Consume 核销：审批通过后只允许使用一次（防止一张单反复看明文）。
func (s *Service) Consume(ctx context.Context, tenantID, ticketID string, actor iam.Subject, term eventbus.Terminal) error {
	tctx := store.WithTenant(ctx, tenantID)
	t, err := s.st.Approvals().Get(tctx, ticketID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	t.Status = types.ApprovalUsed
	t.DecidedAt = &now
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Approvals().Update(tctx, t); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: tenantID, Type: TypeUsed, OccurredAt: now, Risk: eventbus.RiskHigh,
			Actor:    eventbus.Actor{UserID: actor.UserID, Username: actor.Username, RoleCode: roleCodes(actor)},
			Terminal: term,
			Context:  eventbus.ContextInfo{Action: "approval.consume", Result: "success"},
			Data:     map[string]any{"ticket_id": t.ID, "type": t.Type},
		})
	})
}

// ExpireSweep 把超时未决的审批单置为过期（定时任务调用）。
func (s *Service) ExpireSweep(ctx context.Context, tenantID string, now time.Time) (int, error) {
	return s.st.Approvals().ExpireBefore(store.WithTenant(ctx, tenantID), now)
}

// Required 判断某个动作是否需要二次审批（配置中心驱动，不硬编码）。
func (s *Service) Required(action string) bool {
	for _, a := range s.cfg.Limits.ApprovalRequiredActions {
		if a == action {
			return true
		}
	}
	return false
}

func roleCodes(s iam.Subject) []string {
	out := make([]string, 0, len(s.Roles))
	for _, r := range s.Roles {
		out = append(out, r.Code)
	}
	return out
}
