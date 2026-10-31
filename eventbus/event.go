// Package eventbus 统一事件总线：所有模块的写操作都必须经过它，
// 审计 / 告警 / 看板 / BI / 计量 全部从这一条流派生（跨模块铁律）。
package eventbus

import (
	"time"

	"ziniao/platform/types"
)

// Actor 操作人（L5：一条操作要能追到人）。
type Actor struct {
	UserID   string   `json:"user_id"`
	Username string   `json:"username"`
	RoleCode []string `json:"role_codes,omitempty"`
}

// Subject 操作对象（店铺 / 设备 / 账号）。
type Subject struct {
	ShopID    string `json:"shop_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

// Terminal 终端信息。
type Terminal struct {
	IP          string `json:"ip,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	ClientVer   string `json:"client_ver,omitempty"`
	Mode        string `json:"mode,omitempty"` // browser|remote_desktop|api|cli|agent
	TraceID     string `json:"trace_id,omitempty"`
}

// ContextInfo 动作上下文（URL / 动作 / 结果）。
type ContextInfo struct {
	URL    string `json:"url,omitempty"`
	Action string `json:"action"`
	Result string `json:"result"` // success|failure|blocked|partial
}

// RiskLevel 事件风险等级，决定告警通道与命中策略。
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// Event 总线事件。
type Event struct {
	ID         string         `json:"id"`
	TenantID   string         `json:"tenant_id"`
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurred_at"`
	Actor      Actor          `json:"actor"`
	Subject    Subject        `json:"subject"`
	Terminal   Terminal       `json:"terminal"`
	Context    ContextInfo    `json:"context"`
	Risk       RiskLevel      `json:"risk"`
	Data       map[string]any `json:"data,omitempty"`
}

// Category 事件类型 → 六大审计日志类别（M15.1）。
func (e Event) Category() types.AuditCategory {
	switch {
	case e.Type == "auth.login" || e.Type == "auth.logout" || e.Type == "auth.login_failed":
		return types.AuditCategoryLogin
	case e.Type == "risk.*":
		return types.AuditCategoryRisk
	default:
		prefix := e.Type
		if i := indexOf(prefix, '.'); i > 0 {
			prefix = prefix[:i]
		}
		switch prefix {
		case "auth", "session":
			return types.AuditCategoryLogin
		case "iam", "role", "member", "tenant":
			return types.AuditCategoryGrant
		case "risk", "security":
			return types.AuditCategoryRisk
		case "file":
			return types.AuditCategoryUpload
		default:
			return types.AuditCategoryOperation
		}
	}
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// 事件类型常量（各模块引用，避免拼写漂移）。
const (
	TypeTenantCreated  = "tenant.created"
	TypeUserRegistered = "user.registered"
	TypeMemberInvited  = "member.invited"
	TypeRoleGranted    = "iam.role.granted"
	TypeRoleRevoked    = "iam.role.revoked"
	TypeAuthLogin      = "auth.login"
	TypeAuthLogout     = "auth.logout"
	TypeAuthFailed     = "auth.login_failed"
	TypeShopCreated    = "shop.created"
	TypeShopDeleted    = "shop.deleted"
	TypeUsageRecorded  = "usage.recorded"
	TypeRiskAlert      = "risk.alert"
	// TypeTempGrantExpired 临时授权到期被回收（定时任务发出，定向通知当事人）。
	TypeTempGrantExpired = "iam.temp_grant.expired"
)
