// Package types 是整个平台的共享内核（shared kernel）：跨模块传递的实体定义。
// 铁律：所有租户域实体必须携带 TenantID（L12 多租户隔离的第一道防线）。
package types

import (
	"encoding/json"
	"time"
)

// ---------- 租户 ----------

type TenantStatus string

const (
	TenantStatusTrial     TenantStatus = "trial"
	TenantStatusActive    TenantStatus = "active"
	TenantStatusSuspended TenantStatus = "suspended"
	TenantStatusClosed    TenantStatus = "closed"
)

// Tenant 团队/租户。SchemaName 为企业版「独立 schema 逃生舱」预留，
// 为空表示使用共享库 + 行级 tenant_id + RLS。
type Tenant struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Slug       string       `json:"slug"`
	Status     TenantStatus `json:"status"`
	PlanCode   string       `json:"plan_code"`
	SchemaName string       `json:"schema_name,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
}

// Membership 用户在某个租户下的成员身份（一个手机号可管理多个团队，团队间完全隔离）。
type Membership struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	UserID    string    `json:"user_id"`
	Status    string    `json:"status"` // invited|active|disabled
	InvitedBy string    `json:"invited_by,omitempty"`
	JoinedAt  time.Time `json:"joined_at"`
}

// ---------- 身份 ----------

type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	Email       string     `json:"email"`
	DisplayName string     `json:"display_name"`
	Phone       string     `json:"phone,omitempty"`
	Status      string     `json:"status"` // active|disabled
	MFAEnabled  bool       `json:"mfa_enabled"`
	TOTPSecret  string     `json:"-"` // L4：不得进入任何 API 响应
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// Credential 凭证密文。L4：仅存密文与算法参数，明文永不落库。
type Credential struct {
	UserID      string    `json:"user_id"`
	PasswordEnc string    `json:"-"`
	Algo        string    `json:"-"`
	KeyVersion  int       `json:"-"`
	UpdatedAt   time.Time `json:"-"`
}

type Session struct {
	ID           string     `json:"id"`
	UserID       string     `json:"user_id"`
	TenantID     string     `json:"tenant_id"`
	IP           string     `json:"ip"`
	UserAgent    string     `json:"user_agent"`
	TerminalFP   string     `json:"terminal_fp"`
	ClientVer    string     `json:"client_ver"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSeenAt   time.Time  `json:"last_seen_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LoginResult  string     `json:"login_result"` // success|failed|blocked
	BlockedCause string     `json:"blocked_cause,omitempty"`
}

// ---------- 权限 ----------

// Role 角色。TenantID 为空表示系统内置角色（对所有租户可见、不可改）。
type Role struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id,omitempty"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Builtin     bool      `json:"builtin"`
	Permissions []string  `json:"permissions"`
	MaskFields  []string  `json:"mask_fields"`   // 字段级遮蔽（M13）
	DenyElement []string  `json:"deny_elements"` // 元素级屏蔽（M14 事前拦截）
	DataScope   string    `json:"data_scope"`    // none|own|team|all
	CreatedAt   time.Time `json:"created_at"`
}

type UserRole struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	UserID    string    `json:"user_id"`
	RoleID    string    `json:"role_id"`
	GrantedBy string    `json:"granted_by,omitempty"`
	GrantedAt time.Time `json:"granted_at"`
}

// DataGrant 资源级授权（第五层）：如「紫鸟盒子购买权」「某店铺数据可见」。
type DataGrant struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"tenant_id"`
	UserID       string     `json:"user_id"`
	ResourceType string     `json:"resource_type"` // shop|device|box_purchase
	ResourceID   string     `json:"resource_id"`   // "*" 表示全部
	Scope        string     `json:"scope"`
	GrantedBy    string     `json:"granted_by,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"` // 临时授权到期自动回收
	CreatedAt    time.Time  `json:"created_at"`
}

// ---------- 计费 ----------

type Subscription struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	PlanCode  string    `json:"plan_code"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Plan 套餐。所有数值来自配置中心，不在代码里散落硬编码（L10）。
type Plan struct {
	Code                   string `json:"code"`
	Name                   string `json:"name"`
	MaxShops               int    `json:"max_shops"`
	MaxDevices             int    `json:"max_devices"`
	MaxMembers             int    `json:"max_members"`
	RecordingRetentionDays int    `json:"recording_retention_days"`
	APICallsPerDay         int    `json:"api_calls_per_day"`
	HasAgent               bool   `json:"has_agent"`
	HasForensicExport      bool   `json:"has_forensic_export"`
	HasCustomPlugin        bool   `json:"has_custom_plugin"`
	PricePerDevicePerMonth int    `json:"price_per_device_per_month"` // 分
}

// ---------- 店铺（Phase 0 最小实体，M2 正式实现） ----------

type Shop struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	Name      string     `json:"name"`
	Platform  string     `json:"platform"`
	Site      string     `json:"site"`
	Status    string     `json:"status"` // 注册中|养号期|正常运营|审核中|受限|已弃用
	Group     string     `json:"group,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
	DeviceID  string     `json:"device_id,omitempty"`
	OwnerID   string     `json:"owner_id"`
	Profit    float64    `json:"profit,omitempty"`  // 受字段级遮蔽
	Cost      float64    `json:"cost,omitempty"`    // 受字段级遮蔽
	Balance   float64    `json:"balance,omitempty"` // 受字段级遮蔽
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// ---------- 设备与网络（M3 / Phase 1） ----------

// DeviceType 设备类型（云网络设备 / 自有设备 / 紫鸟盒子 / 云手机 / 云号）。
type DeviceType string

const (
	DeviceCloudNetwork DeviceType = "cloud_network"
	DeviceSelfProxy    DeviceType = "self_proxy"
	DeviceVPS          DeviceType = "vps"
	DeviceBroadband    DeviceType = "broadband"
	DeviceBox          DeviceType = "box"
	DeviceCloudPhone   DeviceType = "cloud_phone"
	DeviceCloudNumber  DeviceType = "cloud_number"
)

// DeviceStatus 设备状态机：purchasing → allocating → active → expired（可续费回到 active）。
type DeviceStatus string

const (
	DeviceStatusPurchasing DeviceStatus = "purchasing"
	DeviceStatusAllocating DeviceStatus = "allocating"
	DeviceStatusActive     DeviceStatus = "active"
	DeviceStatusExpired    DeviceStatus = "expired"
	DeviceStatusRecycled   DeviceStatus = "recycled"
)

// Device 一台设备/网络资源。
type Device struct {
	ID              string       `json:"id"`
	TenantID        string       `json:"tenant_id"`
	Name            string       `json:"name"`
	Type            DeviceType   `json:"type"`
	SN              string       `json:"sn,omitempty"` // 紫鸟盒子硬件序列号
	IP              string       `json:"ip,omitempty"`
	Country         string       `json:"country,omitempty"`
	Region          string       `json:"region,omitempty"`
	City            string       `json:"city,omitempty"`
	ASN             string       `json:"asn,omitempty"`
	ASNType         string       `json:"asn_type,omitempty"` // residential|isp|mobile|datacenter
	Status          DeviceStatus `json:"status"`
	OwnerID         string       `json:"owner_id,omitempty"`
	Protocol        string       `json:"protocol,omitempty"` // http|https|socks5
	ProxyHost       string       `json:"proxy_host,omitempty"`
	ProxyPort       int          `json:"proxy_port,omitempty"`
	ProxyUser       string       `json:"proxy_user,omitempty"`
	ProxySecretEnc  string       `json:"-"` // L4：代理口令密文，永不入响应
	HealthScore     int          `json:"health_score"`
	PurityScore     int          `json:"purity_score"`
	BoundAccounts   int          `json:"bound_accounts"`
	ConcurrentUsers int          `json:"concurrent_users"`
	MaxConcurrent   int          `json:"max_concurrent"` // 默认 6（L9：配额模型而非互斥锁）
	ExpiresAt       time.Time    `json:"expires_at"`
	AutoRenew       bool         `json:"auto_renew"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	DeletedAt       *time.Time   `json:"deleted_at,omitempty"`
}

// DeviceBinding 设备与店铺账号的绑定关系（一设备可绑多账号，L9）。
type DeviceBinding struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	DeviceID   string     `json:"device_id"`
	ShopID     string     `json:"shop_id"`
	UserID     string     `json:"user_id,omitempty"` // 当前占用者（并发计数按 user 去重）
	BoundAt    time.Time  `json:"bound_at"`
	LastUsedAt time.Time  `json:"last_used_at,omitempty"`
	UnboundAt  *time.Time `json:"unbound_at,omitempty"`
}

// Environment 店铺浏览器环境（画像 + 内核 + TLS 基线绑定）。
type Environment struct {
	ID           string            `json:"id"`
	TenantID     string            `json:"tenant_id"`
	ShopID       string            `json:"shop_id"`
	DeviceID     string            `json:"device_id,omitempty"`
	ProfileID    string            `json:"profile_id"`
	BaselineHash string            `json:"baseline_hash"` // L8：离线启动校验用
	KernelMajor  int               `json:"kernel_major"`
	TLSHelloID   string            `json:"tls_hello_id"`
	Status       string            `json:"status"`                  // created|running|stopped|drift
	Snapshot     map[string]string `json:"snapshot"`                // 基线快照（漂移检测用）
	Consistency  json.RawMessage   `json:"consistency"`             // 创建时的自洽性报告
	InjectScript string            `json:"inject_script,omitempty"` // 服务端下发的注入脚本（客户端不得自行随机化）
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// ---------- 出口绑定（网络） ----------

// EgressBinding 环境 ↔ 代理商出口资源的绑定，落库以保证重启/发布后出口不变。
//
// 刻意没有 username/password 字段（L4）：出口凭据只随启动单下发，重启后按
// ResourceID 向代理商取回（network.Attacher）。库里有凭据就意味着多一份泄漏面，
// 而凭据本身对「绑定关系」这件事毫无信息价值。
type EgressBinding struct {
	EnvID      string `json:"env_id"`
	TenantID   string `json:"tenant_id"`
	Provider   string `json:"provider"`
	ResourceID string `json:"resource_id"`
	Protocol   string `json:"protocol"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Country    string `json:"country,omitempty"`
	Region     string `json:"region,omitempty"`
	City       string `json:"city,omitempty"`
	ASN        string `json:"asn,omitempty"`
	ASNType    string `json:"asn_type,omitempty"`
	RegionCode string `json:"region_code,omitempty"`
	// LastExit 上次实际使用的出口 host:port。存在的唯一理由：进程重启后
	// 内存里的「上次出口」没了，而出口变更告警恰恰依赖它——没有它，
	// 重启导致的 IP 跳变会变成静默发生。
	LastExit string    `json:"last_exit,omitempty"`
	BoundAt  time.Time `json:"bound_at"`
	// ExpiresAt 到期时间：到期后必须归还厂商资源，否则既持续计费，
	// 又会占着代理商的名额让新环境申请不到。
	ExpiresAt time.Time `json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ---------- 安全中心（M14） ----------

// PolicyRuleType 访问策略规则类型：页面级 / 元素级 / 高危操作。
type PolicyRuleType string

const (
	PolicyRulePage    PolicyRuleType = "page"
	PolicyRuleElement PolicyRuleType = "element"
	PolicyRuleAction  PolicyRuleType = "action"
)

// PolicyAction 命中规则后的动作。
type PolicyAction string

const (
	PolicyActionAllow PolicyAction = "allow"
	PolicyActionDeny  PolicyAction = "deny"
	PolicyActionMask  PolicyAction = "mask"
	PolicyActionAlert PolicyAction = "alert"
)

// AccessPolicy 访问策略（事前拦截）。
type AccessPolicy struct {
	ID        string       `json:"id"`
	TenantID  string       `json:"tenant_id"`
	Name      string       `json:"name"`
	Scope     string       `json:"scope"` // shop|member|team
	ScopeID   string       `json:"scope_id,omitempty"`
	Template  string       `json:"template,omitempty"`
	Enabled   bool         `json:"enabled"`
	Rules     []PolicyRule `json:"rules"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// PolicyRule 一条策略规则。
type PolicyRule struct {
	ID       string         `json:"id"`
	Type     PolicyRuleType `json:"type"`
	Pattern  string         `json:"pattern"` // URL 前缀/通配 或 DOM 选择器
	Action   PolicyAction   `json:"action"`
	Severity string         `json:"severity"` // high|medium|low
	Note     string         `json:"note,omitempty"`
}

// SecurityEvent 安全事件（事中告警 / 事后追溯的数据源）。
type SecurityEvent struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Category    string          `json:"category"` // intercept|alert|risk|drift
	Level       string          `json:"level"`    // high|medium|low
	ShopID      string          `json:"shop_id,omitempty"`
	DeviceID    string          `json:"device_id,omitempty"`
	ActorUserID string          `json:"actor_user_id,omitempty"`
	Action      string          `json:"action"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// ---------- 账密托管（M2.2 / Q10） ----------

// CredentialKind 托管凭证类型。
type CredentialKind string

const (
	CredKindPassword   CredentialKind = "password"
	CredKindTOTP       CredentialKind = "totp"
	CredKindPasskey    CredentialKind = "passkey"
	CredKindPayAccount CredentialKind = "pay_account"
	CredKindEmail      CredentialKind = "email"
)

// ShopCredential 店铺托管凭证（信封加密密文 + 密钥版本）。
type ShopCredential struct {
	ID         string         `json:"id"`
	TenantID   string         `json:"tenant_id"`
	ShopID     string         `json:"shop_id"`
	Kind       CredentialKind `json:"kind"`
	Username   string         `json:"username,omitempty"`
	SecretEnc  string         `json:"-"` // L4：密文，永不入响应
	Algo       string         `json:"-"`
	KeyVersion int            `json:"key_version"`
	UpdatedAt  time.Time      `json:"updated_at"`
	UpdatedBy  string         `json:"updated_by,omitempty"`
}

// ---------- 审批工单（Phase 2 / 反例 8：不能只灰按钮） ----------

// ApprovalStatus 审批单状态。
type ApprovalStatus string

const (
	ApprovalPending   ApprovalStatus = "pending"
	ApprovalApproved  ApprovalStatus = "approved"
	ApprovalRejected  ApprovalStatus = "rejected"
	ApprovalExpired   ApprovalStatus = "expired"
	ApprovalCancelled ApprovalStatus = "cancelled"
	ApprovalUsed      ApprovalStatus = "used" // 一次性核销（审批通过后只允许用一次）
)

// 需要二次审批的高危动作。
const (
	ApprovalTypeAuditExport    = "audit.export.plain"
	ApprovalTypeCredReveal     = "shop.credential.reveal"
	ApprovalTypeDevicePurchase = "device.purchase"
	ApprovalTypeBulkDelete     = "shop.bulk_delete"
)

// ApprovalTicket 审批工单：申请与审批必须分离（L3 职责分离）。
type ApprovalTicket struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	Type          string         `json:"type"`
	SubjectType   string         `json:"subject_type,omitempty"` // shop|device|tenant|audit
	SubjectID     string         `json:"subject_id,omitempty"`
	RequesterID   string         `json:"requester_id"`
	RequesterName string         `json:"requester_name,omitempty"`
	ApproverID    string         `json:"approver_id,omitempty"`
	ApproverName  string         `json:"approver_name,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Status        ApprovalStatus `json:"status"`
	Detail        map[string]any `json:"detail,omitempty"`
	ExpiresAt     time.Time      `json:"expires_at"`
	CreatedAt     time.Time      `json:"created_at"`
	DecidedAt     *time.Time     `json:"decided_at,omitempty"`
}

// ---------- 事件总线 ----------

type OutboxEnvelope struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"` // pending|done|failed
	Attempts    int             `json:"attempts"`
	AvailableAt time.Time       `json:"available_at"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// ---------- 审计 ----------

// AuditCategory 六大日志（M15.1）。
type AuditCategory string

const (
	AuditCategoryLogin     AuditCategory = "login"
	AuditCategoryOperation AuditCategory = "operation"
	AuditCategoryAdmin     AuditCategory = "admin"  // 审计日志：管理变动类
	AuditCategoryRisk      AuditCategory = "risk"   // 风控日志
	AuditCategoryUpload    AuditCategory = "upload" // 上传日志
	AuditCategoryGrant     AuditCategory = "grant"  // 权限日志
)

// AuditEntry 一条审计记录。字段覆盖「人→店铺→设备→终端→IP→时间→URL→动作→结果」（L5）。
type AuditEntry struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Category    AuditCategory   `json:"category"`
	ActorUserID string          `json:"actor_user_id"`
	ActorName   string          `json:"actor_name"`
	ShopID      string          `json:"shop_id,omitempty"`
	DeviceID    string          `json:"device_id,omitempty"`
	TerminalIP  string          `json:"terminal_ip,omitempty"`
	TerminalFP  string          `json:"terminal_fp,omitempty"`
	Action      string          `json:"action"`
	Result      string          `json:"result"` // success|failure|blocked|partial
	URL         string          `json:"url,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Data        json.RawMessage `json:"data,omitempty"`
	PrevHash    string          `json:"prev_hash"`
	Hash        string          `json:"hash"`
}

// ---------- 通知 ----------

type Notification struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	UserID    string     `json:"user_id"`
	Category  string     `json:"category"` // system|security|billing|task
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	EventType string     `json:"event_type"`
	Channels  []string   `json:"channels"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}
