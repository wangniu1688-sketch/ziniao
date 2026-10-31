package approval

import (
	"context"
	"fmt"
	"sort"
	"time"

	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Requirement 一张审批单对审批人的要求。
//
// 之前的实现只有一句话：有 approval:approve 权限且不是申请人就能批。
// 这意味着一个刚入职的运营可以批准「导出全量审计日志」「查看店铺主账号口令」——
// 权限系统没做错（他确实有 approve 权限），错的是**没有按单据内容分级**。
type Requirement struct {
	// Roles 合格审批人须具备其中之一的角色码；空集表示任意有审批权的人。
	Roles []string `json:"roles"`
	// MinApprovers 会签：至少几人签署才算通过。1 表示单人即可。
	MinApprovers int `json:"min_approvers"`
	// Reason 为什么这条单需要这个级别（展示给申请人，避免“为什么我不能批”的黑箱）。
	Reason string `json:"reason,omitempty"`
}

// rule 路由规则。刻意做成数据表而非散落在 if 里：
// 未来从 approval_flows 表加载时，只需要替换 rulesOf 的数据来源，判定逻辑不动。
type rule struct {
	ticketType   string  // 空表示适用于所有类型（兜底规则）
	minAmount    float64 // 金额（CNY）下限；0 表示不卡金额
	roles        []string
	minApprovers int
	reason       string
}

// rulesOf 内置路由规则。顺序敏感：取**第一条**命中的规则。
//
// 关于 MinApprovers：大额设备申购为 2（双人复核），其余为 1。
// 会签判定逻辑见 decide 里的 quorum 分支，生产入口是
// 网关 `POST /api/v1/devices/purchase`（金额超阈值时必须持证）——
// 也就是说，这条规则现在是真的会被执行到的，改动前请先看那条链路上的测试。
// 规则数据要能由运营后台配置，还需 approval_flows 表（见 TODO-phase2 第 5 条）。
func rulesOf(amountOwnerThreshold float64) []rule {
	return []rule{
		{
			// 明文导出审计日志 = 把**全团队**的操作记录一次性带出库外，
			// 影响面比单个店铺口令大得多，所以级别反而最高
			ticketType:   types.ApprovalTypeAuditExport,
			roles:        []string{"owner"},
			minApprovers: 1,
			reason:       "明文导出全量审计日志影响全团队，仅老板可批准",
		},
		{
			// 揭示单个店铺主账号口令不可逆：泄露即账号被接管，没有回滚余地。
			// 这里放 owner/ops_manager 而不是仅 owner——若只允许老板批，
			// 老板自己申请时整个团队没人能批，流程直接死锁。
			ticketType:   types.ApprovalTypeCredReveal,
			roles:        []string{"owner", "ops_manager"},
			minApprovers: 1,
			reason:       "揭示主账号口令不可逆，需管理员级别审批",
		},
		{
			ticketType:   types.ApprovalTypeBulkDelete,
			roles:        []string{"owner", "ops_manager"},
			minApprovers: 1,
			reason:       "批量删除影响不可逆，需管理员级别审批",
		},
		{
			// 大额设备申购：**双人复核**。金额大到一定程度，
			// 一个老板签字不足以制衡（也是最容易内鬼套利的地方）。
			ticketType:   types.ApprovalTypeDevicePurchase,
			minAmount:    amountOwnerThreshold,
			roles:        []string{"owner"},
			minApprovers: 2,
			reason:       "申购金额超过阈值，需两位老板签署（双人复核）",
		},
		{
			ticketType:   types.ApprovalTypeDevicePurchase,
			roles:        []string{"owner", "ops_manager"},
			minApprovers: 1,
			reason:       "设备申购需管理员审批",
		},
	}
}

// amountOfTicket 从单据详情里取金额（Go 的 JSON 数字会变成 float64）。
func amountOfTicket(t *types.ApprovalTicket) float64 {
	if t == nil || t.Detail == nil {
		return 0
	}
	switch v := t.Detail["amount"].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// RequiredFor 计算某张单的审批要求。
//
// 这是纯函数（不碰存储），因此可以被单元测试完整覆盖——
// 路由规则最容易出的问题是"金额刚好卡在阈值上"，必须能直接断言。
func (s *Service) RequiredFor(t *types.ApprovalTicket) Requirement {
	if t == nil {
		return Requirement{MinApprovers: 1}
	}
	// 阈值必须走同一个取值口径：网关判断「要不要持证」用的也是 PurchaseOwnerThresholdCNY。
	// 若两边口径不一致，会出现「网关放行但单据要求双人签署」或反之——这种错现场看起来
	// 像随机抽风，实际是配置被读了两次。
	threshold := config.Default().Limits.PurchaseOwnerThresholdCNY()
	if s.cfg != nil {
		threshold = s.cfg.Limits.PurchaseOwnerThresholdCNY()
	}
	amount := amountOfTicket(t)
	for _, r := range rulesOf(threshold) {
		if r.ticketType != "" && r.ticketType != t.Type {
			continue
		}
		if r.minAmount > 0 && amount < r.minAmount {
			continue
		}
		roles := append([]string{}, r.roles...)
		n := r.minApprovers
		if n <= 0 {
			n = 1
		}
		return Requirement{Roles: roles, MinApprovers: n, Reason: r.reason}
	}
	// 兜底：任何有审批权的人（但要排除申请人，这条硬约束在 CanApprove 里）
	return Requirement{MinApprovers: 1}
}

// ApproverCandidate 合格审批人。
type ApproverCandidate struct {
	UserID   string `json:"user_id"`
	RoleCode string `json:"role_code"`
}

// ResolveApprovers 解析租户内**合格**审批人（不含申请人）。
//
// 排除申请人不是可选项：职责分离的底线，否则审批流程形同虚设。
// 结果用于待办推送——审批待办必须落到"真能批这批人的人"手上，
// 否则会变成"所有人收到通知但大多数点进去才发现无权处理"。
func (s *Service) ResolveApprovers(ctx context.Context, tenantID string, t *types.ApprovalTicket) ([]ApproverCandidate, error) {
	if tenantID == "" || t == nil {
		return nil, nil
	}
	req := s.RequiredFor(t)
	tctx := store.WithTenant(ctx, tenantID)
	urs, err := s.st.UserRoles().ListByTenant(tctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]ApproverCandidate, 0, len(urs))
	seen := map[string]bool{}
	for _, ur := range urs {
		if ur.UserID == "" || ur.UserID == t.RequesterID || seen[ur.UserID] {
			continue
		}
		role, rErr := s.st.Roles().Get(tctx, ur.RoleID)
		if rErr != nil || role == nil {
			continue
		}
		if !roleAllowed(req, role.Code) {
			continue
		}
		// 停用成员不该收到待办（UserRole 本身没有状态，状态挂在 membership 上）
		if ms, mErr := s.st.Memberships().Get(tctx, tenantID, ur.UserID); mErr == nil && ms != nil {
			if ms.Status != "" && ms.Status != "active" {
				continue
			}
		}
		seen[ur.UserID] = true
		out = append(out, ApproverCandidate{UserID: ur.UserID, RoleCode: role.Code})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

// CanApprove 判断 actor 是否具备审批该单的资格。
// 返回 nil 表示通过；否则返回可直接抛给 API 层的错误。
func (s *Service) CanApprove(t *types.ApprovalTicket, actor iam.Subject) error {
	if t == nil {
		return apierr.Validation("审批单不存在")
	}
	if t.RequesterID == actor.UserID {
		return apierr.Forbidden("申请人与审批人不能是同一人（职责分离）")
	}
	req := s.RequiredFor(t)
	if roleAllowed(req, roleCodes(actor)...) {
		return nil
	}
	return apierr.Forbidden("该审批单需以下角色批准：" + req.describe() + "（" + req.Reason + "）")
}

func (req Requirement) describe() string {
	if len(req.Roles) == 0 {
		return "任意审批人"
	}
	out := ""
	for i, r := range req.Roles {
		if i > 0 {
			out += "/"
		}
		out += r
	}
	return out
}

// roleAllowed 候选角色是否命中要求。
func roleAllowed(req Requirement, codes ...string) bool {
	if len(req.Roles) == 0 {
		return true
	}
	for _, need := range req.Roles {
		for _, c := range codes {
			if c == need {
				return true
			}
		}
	}
	return false
}

// ---- 会签签名记录 ----

// Signature 一次签署。会签（MinApprovers>1）时需要累计到法定人数才生效。
type Signature struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	RoleCode string `json:"role_code,omitempty"`
	At       string `json:"at"`
}

// signaturesOf 读取已有签名。
func signaturesOf(t *types.ApprovalTicket) []Signature {
	if t.Detail == nil {
		return nil
	}
	raw, ok := t.Detail["signatures"].([]any)
	if !ok {
		return nil
	}
	out := make([]Signature, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := Signature{}
		if v, ok := m["user_id"].(string); ok {
			s.UserID = v
		}
		if v, ok := m["username"].(string); ok {
			s.Username = v
		}
		if v, ok := m["role_code"].(string); ok {
			s.RoleCode = v
		}
		if v, ok := m["at"].(string); ok {
			s.At = v
		}
		out = append(out, s)
	}
	return out
}

// addSignature 追加签名（幂等：同人重复签署不增加计数）。
func addSignature(t *types.ApprovalTicket, actor iam.Subject) ([]Signature, bool) {
	sigs := signaturesOf(t)
	for _, s := range sigs {
		if s.UserID == actor.UserID {
			return sigs, false // 已签过
		}
	}
	sigs = append(sigs, Signature{
		UserID:   actor.UserID,
		Username: actor.Username,
		RoleCode: firstRoleCode(actor),
		At:       time.Now().UTC().Format(time.RFC3339),
	})
	if t.Detail == nil {
		t.Detail = map[string]any{}
	}
	list := make([]any, 0, len(sigs))
	for _, s := range sigs {
		list = append(list, map[string]any{
			"user_id": s.UserID, "username": s.Username,
			"role_code": s.RoleCode, "at": s.At,
		})
	}
	t.Detail["signatures"] = list
	return sigs, true
}

func firstRoleCode(s iam.Subject) string {
	if len(s.Roles) == 0 {
		return ""
	}
	return s.Roles[0].Code
}

// describeRequirement 供日志/通知文案使用。
func describeRequirement(req Requirement) string {
	if req.MinApprovers > 1 {
		return fmt.Sprintf("%s（需 %d 人会签）", req.describe(), req.MinApprovers)
	}
	return req.describe()
}
