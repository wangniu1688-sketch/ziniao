// Package delegation 代理机构委托审批（approval_flows，docs/TODO-phase2.md 第 5 条）。
//
// 与 approval（审批工单）的区别：审批工单是同一租户内的高危动作二次审批；
// 委托审批是**跨租户**的两方流程——受托方（代理机构，同为系统内租户）申请
// 代管委托方（店铺主团队）的指定店铺，由委托方 owner/ops_manager 裁决。
//
// 铁律（与 approval 同源）：
//  1. 受托方不能批自己的申请，委托方不能申自己的委托（职责分离跨租户成立）；
//  2. 申请单有有效期（复用审批单 TTL），超期未决自动失效；
//  3. 授权随委托生效、随委托终止：批准 = DataGrant（店铺级、带到期）+ 成员身份
//     + agency_ops 角色；撤销 / 到期整批回收，不留悬空授权；
//  4. 申请 / 裁决 / 撤销 / 到期全部落审计（事件前缀 iam.delegation.*，授权类）。
package delegation

import (
	"context"
	"strconv"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// 事件类型：归入「授权类」审计。
const (
	TypeCreated   = "iam.delegation.created"   // 受托方提交申请（定向委托方审批人）
	TypeApproved  = "iam.delegation.approved"  // 委托方批准（定向受托方申请人）
	TypeRejected  = "iam.delegation.rejected"  // 委托方拒绝
	TypeCancelled = "iam.delegation.cancelled" // 受托方撤回
	TypeRevoked   = "iam.delegation.revoked"   // 委托方撤销（授权即时回收）
	TypeExpired   = "iam.delegation.expired"   // 申请超期未决 / 委托到期（授权回收）
)

// agencyRoleCode 批准后授予受托方申请人的角色（内置最小权限角色，见 iam/roles.go）。
const agencyRoleCode = "agency_ops"

// approverRoles 委托审批人角色门槛（与审批工单的 cred/bulk-delete 同级）：
// 把店铺的运营权交给外部团队，影响面不亚于批量删除，必须是管理员级别。
var approverRoles = []string{"owner", "ops_manager"}

// maxShopsPerFlow 单张委托单店铺数上限：范围越大风险越大，审批列表也要可读。
const maxShopsPerFlow = 200

// Service 委托审批服务。
type Service struct {
	st     store.Store
	bus    *eventbus.Bus
	cfg    *config.Config
	engine *iam.Engine
}

// NewService 构造委托审批服务。
func NewService(st store.Store, bus *eventbus.Bus, cfg *config.Config, engine *iam.Engine) *Service {
	return &Service{st: st, bus: bus, cfg: cfg, engine: engine}
}

// SubmitInput 受托方提交委托申请。
type SubmitInput struct {
	GrantorSlug string // 委托方团队 slug（对外标识，不暴露内部 ID）
	ShopIDs     []string
	ValidDays   int    // 委托有效期（天），1..365，默认 30
	Reason      string // 申请理由
	Actor       iam.Subject
	Terminal    eventbus.Terminal
	URL         string
}

// DecideInput 裁决 / 撤回 / 撤销。
type DecideInput struct {
	FlowID   string
	Comment  string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

func itoa(n int) string { return strconv.Itoa(n) }

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func hasAnyRole(s iam.Subject, codes []string) bool {
	for _, r := range s.Roles {
		if contains(codes, r.Code) {
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

// grantScope 委托授权的 scope 标识（回收按它整批删，避免依赖 detail 里的快照）。
func grantScope(flowID string) string { return "delegation:" + flowID }

func validDaysOf(f *types.DelegationFlow) int {
	if f.Detail == nil {
		return 30
	}
	switch v := f.Detail["valid_days"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 30
}

func roleIDOf(f *types.DelegationFlow) string {
	if f.Detail == nil {
		return ""
	}
	v, _ := f.Detail["role_id"].(string)
	return v
}

// grant 批准时的授权落地。**必须在事务内调用**（传 tx 而不是裸 Store）：
// 授权与单据状态同一事务——授权写不进去就整体回滚，不允许出现
// 「单据显示已批准、实际没有任何授权」的假生效。
func (s *Service) grant(tx store.Tx, tctx context.Context, f *types.DelegationFlow,
	in DecideInput, now time.Time) error {
	validUntil := *f.ValidUntil
	scope := grantScope(f.ID)
	// 1) 成员身份：受托方申请人加入委托方团队。没有成员身份，身份层根本
	//    不会把人当成这个租户的主体——授权行写得再多也是死数据。
	ms, err := tx.Memberships().Get(tctx, f.TenantID, f.GranteeID)
	if err != nil && !apierr.Is(err, apierr.CodeNotFound) {
		return err
	}
	if ms == nil {
		if err := tx.Memberships().Create(tctx, &types.Membership{
			ID: ids.New(), TenantID: f.TenantID, UserID: f.GranteeID,
			Status: "active", InvitedBy: in.Actor.UserID, JoinedAt: now,
		}); err != nil {
			return err
		}
	} else if ms.Status == "disabled" {
		return apierr.Conflict("受托方申请人在委托方团队已被停用，请先恢复成员或要求受托方更换申请人")
	}
	// 2) 角色：内置 agency_ops（最小权限：查看 + 环境开关，无审批/删除/凭证/财务）。
	role, err := tx.Roles().GetByCode(tctx, "", agencyRoleCode)
	if err != nil || role == nil {
		return apierr.Internal("内置角色 agency_ops 缺失，请检查启动装配")
	}
	urs, err := tx.UserRoles().ListByUser(tctx, f.TenantID, f.GranteeID)
	if err != nil {
		return err
	}
	hasRole := false
	for _, ur := range urs {
		if ur.RoleID == role.ID {
			hasRole = true
			break
		}
	}
	if !hasRole {
		if err := tx.UserRoles().Assign(tctx, &types.UserRole{
			ID: ids.New(), TenantID: f.TenantID, UserID: f.GranteeID, RoleID: role.ID,
			GrantedBy: in.Actor.UserID, GrantedAt: now,
		}); err != nil {
			return err
		}
	}
	// 3) 店铺级 DataGrant：每店一行、带到期时间、scope 带 flow id 便于整批回收溯源。
	grantIDs := make([]string, 0, len(f.ShopIDs))
	for _, shopID := range f.ShopIDs {
		g := &types.DataGrant{
			ID: ids.New(), TenantID: f.TenantID, UserID: f.GranteeID,
			ResourceType: "shop", ResourceID: shopID, Scope: scope,
			GrantedBy: in.Actor.UserID, ExpiresAt: &validUntil, CreatedAt: now,
		}
		if err := tx.DataGrants().Create(tctx, g); err != nil {
			return err
		}
		grantIDs = append(grantIDs, g.ID)
	}
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail["grants"] = grantIDs
	f.Detail["role_id"] = role.ID
	return nil
}

// revokeAccess 回收一批授权：DataGrant 整批删除 + agency_ops 角色撤销。
// 成员身份保留（空角色成员不构成任何权限面，也不应篡改委托方的成员花名册）。
// **必须在事务内调用**（与 grant 同理，半回收比不回收更难排查）。
func (s *Service) revokeAccess(tx store.Tx, tctx context.Context, f *types.DelegationFlow) error {
	scope := grantScope(f.ID)
	deleted, err := tx.DataGrants().DeleteByScope(tctx, f.TenantID, f.GranteeID, scope)
	if err != nil {
		return err
	}
	roleID := roleIDOf(f)
	if roleID == "" {
		if role, rErr := tx.Roles().GetByCode(tctx, "", agencyRoleCode); rErr == nil && role != nil {
			roleID = role.ID
		}
	}
	if roleID != "" {
		if err := tx.UserRoles().Revoke(tctx, f.TenantID, f.GranteeID, roleID); err != nil &&
			!apierr.Is(err, apierr.CodeNotFound) {
			return err
		}
	}
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail["revoked_grants"] = len(deleted)
	return nil
}

// ResolveApprovers 委托方的合格审批人（owner/ops_manager，active 成员）。
// 用于待办定向推送：委托待办必须落到「真能批的人」手上。
func (s *Service) ResolveApprovers(ctx context.Context, tenantID string) ([]string, error) {
	tctx := store.WithTenant(ctx, tenantID)
	urs, err := s.st.UserRoles().ListByTenant(tctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := []string{}
	seen := map[string]bool{}
	for _, ur := range urs {
		if ur.UserID == "" || seen[ur.UserID] {
			continue
		}
		role, rErr := s.st.Roles().Get(tctx, ur.RoleID)
		if rErr != nil || role == nil {
			continue
		}
		if !contains(approverRoles, role.Code) {
			continue
		}
		// 停用成员不该收到待办（UserRole 无状态，状态挂在 membership 上）
		if ms, mErr := s.st.Memberships().Get(tctx, tenantID, ur.UserID); mErr == nil && ms != nil {
			if ms.Status != "" && ms.Status != "active" {
				continue
			}
		}
		seen[ur.UserID] = true
		out = append(out, ur.UserID)
	}
	return out, nil
}

// Submit 受托方（代理机构）提交委托申请。
//
// 单据落在委托方租户（tenant_id = 委托方），受托方凭双主体 RLS 读自己的申请状态。
func (s *Service) Submit(ctx context.Context, in SubmitInput) (*types.DelegationFlow, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "delegation:apply", Path: in.URL})
	if !dec.Allow {
		return nil, apierr.Forbidden("无权提交委托申请（" + string(dec.Layer) + "）")
	}
	if in.GrantorSlug == "" {
		return nil, apierr.Validation("缺少委托方团队标识（tenant_slug）")
	}
	if len(in.ShopIDs) == 0 {
		return nil, apierr.Validation("委托范围不能为空：至少指定一个店铺")
	}
	if len(in.ShopIDs) > maxShopsPerFlow {
		return nil, apierr.Validation("单张委托单最多包含 " + itoa(maxShopsPerFlow) + " 个店铺")
	}
	validDays := in.ValidDays
	if validDays <= 0 {
		validDays = 30
	}
	if validDays > 365 {
		return nil, apierr.Validation("委托有效期最长 365 天，到期后需重新申请")
	}
	// 委托方租户按 slug 跨租户查询：tenants 表无 RLS（admin_list 策略兜底），
	// 裸上下文即可读；这是本功能唯一的跨租户元数据读取。
	bare := context.Background()
	grantor, err := s.st.Tenants().GetBySlug(bare, in.GrantorSlug)
	if err != nil {
		return nil, apierr.Validation("委托方团队不存在：" + in.GrantorSlug)
	}
	if grantor.Status != types.TenantStatusActive && grantor.Status != types.TenantStatusTrial {
		return nil, apierr.Validation("委托方团队当前不可接受委托（状态：" + string(grantor.Status) + "）")
	}
	if grantor.ID == in.Actor.TenantID {
		return nil, apierr.Validation("不能向自己团队提交委托申请")
	}
	agency, err := s.st.Tenants().Get(bare, in.Actor.TenantID)
	if err != nil {
		return nil, err
	}
	// 店铺归属校验：申请范围里混入不属于委托方的店铺就整体拒绝，而不是悄悄
	// 缩小范围——「你以为托了 5 家、实际只托了 4 家」比直接报错更危险。
	tctxShops := store.WithTenant(ctx, grantor.ID)
	for _, id := range in.ShopIDs {
		if _, err := s.st.Shops().Get(tctxShops, id); err != nil {
			return nil, apierr.Validation("委托范围包含不属于该团队或不存在的店铺：" + id)
		}
	}
	tctxAgency := store.WithTenant(ctx, in.Actor.TenantID)
	dup, err := s.st.Delegations().HasPendingBetween(tctxAgency, grantor.ID, in.Actor.TenantID)
	if err != nil {
		return nil, err
	}
	if dup {
		return nil, apierr.Conflict("已存在待审批的委托申请，请等待对方处理或先撤回")
	}
	ttl := 1800
	if s.cfg != nil && s.cfg.Limits.ApprovalTTLSeconds > 0 {
		ttl = s.cfg.Limits.ApprovalTTLSeconds
	}
	now := time.Now().UTC()
	f := &types.DelegationFlow{
		ID: ids.New(), TenantID: grantor.ID, AgencyTenantID: in.Actor.TenantID, AgencyName: agency.Name,
		GranteeID: in.Actor.UserID, GranteeName: in.Actor.Username,
		ShopIDs: append([]string{}, in.ShopIDs...), Reason: in.Reason,
		Status:    types.DelegationPending,
		Detail:    map[string]any{"valid_days": validDays},
		ExpiresAt: now.Add(time.Duration(ttl) * time.Second), CreatedAt: now,
	}
	// 待办定向推送：委托方 owner/ops_manager（申请人跨租户，无需排除）。
	approvers, _ := s.ResolveApprovers(bare, grantor.ID)
	tctxFlow := store.WithTenant(ctx, grantor.ID)
	if err := s.st.WithTx(tctxFlow, func(tx store.Tx) error {
		if err := tx.Delegations().Create(tctxFlow, f); err != nil {
			return err
		}
		return s.bus.Emit(tctxFlow, tx, eventbus.Event{
			TenantID: grantor.ID, Type: TypeCreated, OccurredAt: now, Risk: eventbus.RiskMedium,
			Actor:    eventbus.Actor{UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor)},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "delegation.submit", Result: "success"},
			Data: map[string]any{
				"flow_id": f.ID, "agency_tenant_id": f.AgencyTenantID, "agency_name": f.AgencyName,
				"shop_count": len(f.ShopIDs), "valid_days": validDays,
				"approver_ids": approvers, "requester_id": f.GranteeID,
			},
		})
	}); err != nil {
		return nil, err
	}
	return f, nil
}

// Approve 委托方批准：授权同事务落地（成员身份 + agency_ops 角色 + 店铺级 DataGrant）。
func (s *Service) Approve(ctx context.Context, in DecideInput) (*types.DelegationFlow, error) {
	return s.decide(ctx, in, types.DelegationActive, TypeApproved, "delegation.approve", "delegation:approve")
}

// Reject 委托方拒绝。
func (s *Service) Reject(ctx context.Context, in DecideInput) (*types.DelegationFlow, error) {
	return s.decide(ctx, in, types.DelegationRejected, TypeRejected, "delegation.reject", "delegation:approve")
}

func (s *Service) decide(ctx context.Context, in DecideInput, status types.DelegationStatus,
	evType, action, perm string) (*types.DelegationFlow, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: perm, Path: in.URL})
	if !dec.Allow {
		return nil, apierr.Forbidden("无权审批委托申请（" + string(dec.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	f, err := s.st.Delegations().Get(tctx, in.FlowID)
	if err != nil {
		return nil, apierr.NotFound("委托单不存在或不属于当前团队")
	}
	// 只有委托方能裁决：受托方 owner 也有 delegation:approve（权限全集），
	// 光靠功能权限拦不住「代理机构自己批自己」——必须按单据内容再拦一次。
	if in.Actor.TenantID != f.TenantID {
		return nil, apierr.Forbidden("委托申请由委托方审批，受托方无权裁决自己的申请")
	}
	if !hasAnyRole(in.Actor, approverRoles) {
		return nil, apierr.Forbidden("委托审批需以下角色批准：owner/ops_manager")
	}
	if f.Status != types.DelegationPending {
		return nil, apierr.Conflict("该委托单已处理（状态：" + string(f.Status) + "）")
	}
	if time.Now().UTC().After(f.ExpiresAt) {
		return nil, apierr.Conflict("委托单已过期，请让受托方重新提交")
	}
	now := time.Now().UTC()
	if in.Comment != "" {
		f.Comment = in.Comment
	}
	f.Status = status
	f.ApproverID = in.Actor.UserID
	f.ApproverName = in.Actor.Username
	f.DecidedAt = &now
	if status == types.DelegationActive {
		validUntil := now.AddDate(0, 0, validDaysOf(f))
		f.ValidUntil = &validUntil
	}
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		// 授权与单据状态同一事务：授权写不进去就整体回滚，
		// 不允许「单据显示已批准、实际没有任何授权」的假生效。
		if status == types.DelegationActive {
			if err := s.grant(tx, tctx, f, in, now); err != nil {
				return err
			}
		}
		if err := tx.Delegations().Update(tctx, f); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: f.TenantID, Type: evType, OccurredAt: now, Risk: eventbus.RiskHigh,
			Actor:    eventbus.Actor{UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor)},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: action, Result: "success"},
			Data: map[string]any{
				"flow_id": f.ID, "agency_tenant_id": f.AgencyTenantID, "status": string(status),
				"shop_count": len(f.ShopIDs), "grantee_id": f.GranteeID,
				// 定向：批准/拒绝的结果通知给受托方申请人
				"approver_ids": []string{f.GranteeID},
			},
		})
	}); err != nil {
		return nil, err
	}
	return f, nil
}

// Cancel 受托方申请人撤回自己的待审批申请。
func (s *Service) Cancel(ctx context.Context, in DecideInput) (*types.DelegationFlow, error) {
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	f, err := s.st.Delegations().Get(tctx, in.FlowID)
	if err != nil {
		return nil, apierr.NotFound("委托单不存在或不属于当前团队")
	}
	if in.Actor.TenantID != f.AgencyTenantID || in.Actor.UserID != f.GranteeID {
		return nil, apierr.Forbidden("只能撤回本人以受托方身份提交的委托申请")
	}
	if f.Status != types.DelegationPending {
		return nil, apierr.Conflict("只有待审批的委托单可撤回")
	}
	return s.finish(ctx, f, types.DelegationCancelled, TypeCancelled, "delegation.cancel", in, eventbus.RiskLow)
}

// Revoke 委托方撤销生效中的委托：授权**即时**回收，不等巡检。
func (s *Service) Revoke(ctx context.Context, in DecideInput) (*types.DelegationFlow, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "delegation:revoke", Path: in.URL})
	if !dec.Allow {
		return nil, apierr.Forbidden("无权撤销委托（" + string(dec.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	f, err := s.st.Delegations().Get(tctx, in.FlowID)
	if err != nil {
		return nil, apierr.NotFound("委托单不存在或不属于当前团队")
	}
	if in.Actor.TenantID != f.TenantID {
		return nil, apierr.Forbidden("只有委托方可以撤销委托")
	}
	if !hasAnyRole(in.Actor, approverRoles) {
		return nil, apierr.Forbidden("撤销委托需 owner/ops_manager 角色")
	}
	if f.Status != types.DelegationActive {
		return nil, apierr.Conflict("只有生效中的委托可撤销（当前状态：" + string(f.Status) + "）")
	}
	// 回收与状态收尾同一事务（finish 的展开版）：半回收比不回收更难排查。
	now := time.Now().UTC()
	f.Status = types.DelegationRevoked
	f.DecidedAt = &now
	f.RevokedAt = &now
	if in.Comment != "" {
		f.Comment = in.Comment
	}
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := s.revokeAccess(tx, tctx, f); err != nil {
			return err
		}
		if err := tx.Delegations().Update(tctx, f); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: f.TenantID, Type: TypeRevoked, OccurredAt: now, Risk: eventbus.RiskHigh,
			Actor:    eventbus.Actor{UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor)},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "delegation.revoke", Result: "success"},
			Data: map[string]any{
				"flow_id": f.ID, "agency_tenant_id": f.AgencyTenantID, "status": string(types.DelegationRevoked),
				"grantee_id":   f.GranteeID,
				"approver_ids": []string{f.GranteeID},
			},
		})
	}); err != nil {
		return nil, err
	}
	return f, nil
}

// List 双方各自视角的委托单列表（party: "grantor" 默认 | "agency"）。
func (s *Service) List(ctx context.Context, actor iam.Subject, party string,
	status types.DelegationStatus, limit, offset int, path string) ([]types.DelegationFlow, int, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "delegation:read", Path: path})
	if !dec.Allow {
		return nil, 0, apierr.Forbidden("无权查看委托单（" + string(dec.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	return s.st.Delegations().List(tctx, store.DelegationFilter{
		Status: status, Party: party, Limit: limit, Offset: offset,
	})
}

// ExpireSweep 委托单过期巡检（delegation.expire 任务按租户调用）：
//  1. 申请单超期未决 → expired（定向通知申请人，避免「石沉大海」式静默失败）；
//  2. 生效中的委托过 valid_until → expired + 授权整批回收（通知双方）。
//
// 到期即刻失效由读路径兜着（DataGrant.ExpiresAt 过行即失效、identity/iam 均带
// 时间判定），巡检负责的是「清行 + 改状态 + 让人知道」，与临时授权回收同一分工。
func (s *Service) ExpireSweep(ctx context.Context, tenantID string, now time.Time) (int, error) {
	tctx := store.WithTenant(ctx, tenantID)
	affected := 0
	pending, _, err := s.st.Delegations().List(tctx, store.DelegationFilter{Status: types.DelegationPending, Limit: 500})
	if err != nil {
		return 0, err
	}
	for i := range pending {
		if pending[i].ExpiresAt.After(now) {
			continue
		}
		if _, err := s.expirePending(tctx, &pending[i], now); err != nil {
			return affected, err
		}
		affected++
	}
	active, _, err := s.st.Delegations().List(tctx, store.DelegationFilter{Status: types.DelegationActive, Limit: 500})
	if err != nil {
		return affected, err
	}
	for i := range active {
		if active[i].ValidUntil == nil || active[i].ValidUntil.After(now) {
			continue
		}
		if _, err := s.expireActive(tctx, &active[i], now); err != nil {
			return affected, err
		}
		affected++
	}
	return affected, nil
}

func (s *Service) expirePending(tctx context.Context, f *types.DelegationFlow, now time.Time) (*types.DelegationFlow, error) {
	f.Status = types.DelegationExpired
	f.DecidedAt = &now
	term := eventbus.Terminal{Mode: "job", ClientVer: "jobs/1"}
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Delegations().Update(tctx, f); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: f.TenantID, Type: TypeExpired, OccurredAt: now, Risk: eventbus.RiskMedium,
			Terminal: term,
			Context:  eventbus.ContextInfo{Action: "delegation.expire", Result: "blocked"},
			Data: map[string]any{
				"flow_id": f.ID, "agency_tenant_id": f.AgencyTenantID, "status": string(types.DelegationExpired),
				// 申请人等着结果，只有他需要被告知
				"approver_ids": []string{f.GranteeID}, "grantee_id": f.GranteeID,
			},
		})
	}); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Service) expireActive(tctx context.Context, f *types.DelegationFlow, now time.Time) (*types.DelegationFlow, error) {
	f.Status = types.DelegationExpired
	f.DecidedAt = &now
	f.RevokedAt = &now
	// 双方都该知道委托到期了：委托方要知道运营权收回来了，受托方要知道别再上门。
	targets := []string{f.GranteeID}
	if more, err := s.ResolveApprovers(context.Background(), f.TenantID); err == nil {
		targets = append(targets, more...)
	}
	term := eventbus.Terminal{Mode: "job", ClientVer: "jobs/1"}
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := s.revokeAccess(tx, tctx, f); err != nil {
			return err
		}
		if err := tx.Delegations().Update(tctx, f); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: f.TenantID, Type: TypeExpired, OccurredAt: now, Risk: eventbus.RiskMedium,
			Terminal: term,
			Context:  eventbus.ContextInfo{Action: "delegation.expire", Result: "success"},
			Data: map[string]any{
				"flow_id": f.ID, "agency_tenant_id": f.AgencyTenantID, "status": string(types.DelegationExpired),
				"approver_ids": targets, "grantee_id": f.GranteeID,
			},
		})
	}); err != nil {
		return nil, err
	}
	return f, nil
}

// finish 状态收尾 + 事件（Cancel/Revoke 共用）。
func (s *Service) finish(ctx context.Context, f *types.DelegationFlow, status types.DelegationStatus,
	evType, action string, in DecideInput, risk eventbus.RiskLevel) (*types.DelegationFlow, error) {
	now := time.Now().UTC()
	f.Status = status
	f.DecidedAt = &now
	if status == types.DelegationRevoked {
		f.RevokedAt = &now
	}
	if in.Comment != "" {
		f.Comment = in.Comment
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Delegations().Update(tctx, f); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: f.TenantID, Type: evType, OccurredAt: now, Risk: risk,
			Actor:    eventbus.Actor{UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor)},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: action, Result: "success"},
			Data: map[string]any{
				"flow_id": f.ID, "agency_tenant_id": f.AgencyTenantID, "status": string(status),
				"grantee_id":   f.GranteeID,
				"approver_ids": []string{f.GranteeID},
			},
		})
	}); err != nil {
		return nil, err
	}
	return f, nil
}
