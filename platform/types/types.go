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
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Site     string `json:"site"`
	Status   string `json:"status"` // 注册中|养号期|正常运营|审核中|受限|已弃用
	Group    string `json:"group,omitempty"`
	// GroupID 显式分组外键（0012）。与 Group 双写：Group 是历史文本列，
	// 现有 PUT /api/v1/shops/{id}/group 与既有 e2e 依赖它，读路径以 GroupID 为准。
	GroupID   string     `json:"group_id,omitempty"`
	Code      string     `json:"code,omitempty"` // 运营自定义编号（如 AMZ-001），与系统 id 分离
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
	// 以下 5 列由 0012 引入（启动器内核）：
	ProxyID string `json:"proxy_id,omitempty"` // 手工静态代理；非空即该环境出口走它，否则走 Allocator/direct
	// Country 指纹国家。权威值仍在 Snapshot（"country"）：这里提列只为列表筛选与排序，
	// 改它**不会**重算指纹——指纹由画像库决定。
	Country string `json:"country,omitempty"`
	// StartURL 启动后自动打开的首个页面；为空则 about:blank。
	StartURL string `json:"start_url,omitempty"`
	// LastStartedAt 只在「真正拉起成功（Chrome 起来 + CDP 连通）」时写，
	// 与 UpdatedAt 严格区分：UpdatedAt 每次写都动（含停用、改代理）。
	LastStartedAt *time.Time `json:"last_started_at,omitempty"`
	ThumbnailPath string     `json:"thumbnail_path,omitempty"` // data/ 下的相对路径；本期可空
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ---------- 启动器内核（M1 / 0012） ----------

// Proxy 运营手工录入的静态出口代理。
//
// 与 Device 的边界：Device 是「有采购/续费/健康分」的网络资产，出口由 Allocator
// 从代理商分发；Proxy 只是一段「地址 + 凭据 + 备注 + 启停」，由环境直接引用成出口。
//
// L4：PasswordEnc 与 Device.ProxySecretEnc 同口径——json:"-" 永不入响应，
// API 层只输出 has_password 布尔。明文只存在于 Go 进程内存与本地 TLSProxy 配置。
type Proxy struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Name        string    `json:"name"`
	Protocol    string    `json:"protocol"` // http|https|socks5
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	Username    string    `json:"username,omitempty"`
	PasswordEnc string    `json:"-"` // 信封加密密文，永不入响应
	// HasPassword 派生只读列：`password_enc IS NOT NULL AND <> ''`。
	//
	// 为什么要单独一列：常规读路径（Get/List）的读列表**显式排除** password_enc
	// （密文只经 PasswordEnc() 取），于是「有没有口令」这个纯展示问题也一起
	// 拿不到了。把它做成 SQL 派生列，可以在不把密文带进内存的前提下回答它。
	// 写路径忽略本字段。
	HasPassword bool `json:"-"`
	KeyVersion  int  `json:"key_version"`
	Country     string    `json:"country,omitempty"`
	Note        string    `json:"note,omitempty"`
	// Status 启停（active|disabled）。**不表健康**——健康见 CheckStatus。
	Status string `json:"status"`
	// 以下 8 列由 0013 引入（P3 上游代理接入）：
	//   Type 线路类型（isp|static_residential|residential|datacenter），只用于台账筛选；
	//   CheckStatus 拨测健康（unknown|ok|fail），与 Status 分离；
	//   ExitIP/ExitCountry/LatencyMS/LastCheckedAt/LastError 只在真正拨测过之后才有值。
	Type           string     `json:"type,omitempty"`
	City           string     `json:"city,omitempty"`
	CheckStatus    string     `json:"check_status"`
	ExitIP         string     `json:"exit_ip,omitempty"`
	ExitCountry    string     `json:"exit_country,omitempty"`
	LatencyMS      *int       `json:"latency_ms,omitempty"`
	LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// 代理类型 / 拨测健康的枚举（DB 有 CHECK，mem 驱动没有，Go 侧必须再判一次）。
const (
	ProxyTypeISP               = "isp"
	ProxyTypeStaticResidential = "static_residential"
	ProxyTypeResidential       = "residential"
	ProxyTypeDatacenter        = "datacenter"

	ProxyCheckUnknown = "unknown"
	ProxyCheckOK      = "ok"
	ProxyCheckFail    = "fail"
)

// ValidProxyType 线路类型是否合法。
func ValidProxyType(t string) bool {
	switch t {
	case ProxyTypeISP, ProxyTypeStaticResidential, ProxyTypeResidential, ProxyTypeDatacenter:
		return true
	}
	return false
}

// ValidProxyCheckStatus 拨测健康状态是否合法。
func ValidProxyCheckStatus(s string) bool {
	switch s {
	case ProxyCheckUnknown, ProxyCheckOK, ProxyCheckFail:
		return true
	}
	return false
}

// Group 店铺分组（把 shops.group_name 的文本分组升格为实体）。
//
// 必须携带 tenant_id 并纳入 RLS：分组天然跟随租户隔离，禁止跨租户可见分组。
// EnvironmentCount 是实时统计值（列表接口一次 GROUP BY 带出），**不落库**。
type Group struct {
	ID               string    `json:"id"`
	TenantID         string    `json:"tenant_id"`
	Name             string    `json:"name"`
	Sort             int       `json:"sort"`
	EnvironmentCount int       `json:"environment_count"` // 实时统计，非缓存列
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
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

// 出口绑定生命周期动作。取值直接落进 egress_binding_history.action，
// 并由迁移里的 CHECK 约束兜底——拼错一个字母的事件等于不存在。
const (
	EgressActionBound    = "bound"    // 首次绑定
	EgressActionRebound  = "rebound"  // 换绑（同一环境换了资源/出口）
	EgressActionReleased = "released" // 主动释放（环境停用 / 手工回收）
	EgressActionExpired  = "expired"  // 租约到期回收
	EgressActionDropped  = "dropped"  // 既存绑定作废（代理商下架 / 无法取回 / 已过期）
)

// ValidEgressAction 动作是否合法（与 egress_binding_history 的 CHECK 约束同口径）。
// 放在 Go 侧也判一次：mem 驱动没有 CHECK 兜底，两份驱动必须给出同样的结论。
func ValidEgressAction(a string) bool {
	switch a {
	case EgressActionBound, EgressActionRebound, EgressActionReleased, EgressActionExpired, EgressActionDropped:
		return true
	}
	return false
}

// EgressBindingEvent 出口绑定生命周期事件（只追加，不改写）。
//
// 为什么要有它：「当前绑定」回答不了「这两个店铺历史上有没有共用过同一 ASN」——
// 时间错开的共用，在解绑那一刻就从当前表里消失了。反关联检测的输入只能是历史。
type EgressBindingEvent struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	EnvID    string `json:"env_id"`
	// Action 见 EgressAction* 常量。
	Action     string `json:"action"`
	Provider   string `json:"provider"`
	ResourceID string `json:"resource_id"`
	Protocol   string `json:"protocol,omitempty"`
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	Country    string `json:"country,omitempty"`
	Region     string `json:"region,omitempty"`
	City       string `json:"city,omitempty"`
	ASN        string `json:"asn,omitempty"`
	ASNType    string `json:"asn_type,omitempty"`
	// ExitAddr 事件发生时的出口 host:port（平台真正看得见的身份）。
	ExitAddr string `json:"exit_addr,omitempty"`
	// PrevExit 上一跳出口：rebound 时说明「从哪变到哪」。
	PrevExit string `json:"prev_exit,omitempty"`
	// Reason 人可读的成因（排障第一问，不是给机器看的枚举）。
	Reason     string    `json:"reason,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// EgressASNShare 同一 ASN 被多个环境使用过（时间错开也算）。
//
// 这是反关联的**线索**而不是判决：住宅代理商把同一 ASN 下的不同 IP 分给两个租户
// 是正常商业行为；但如果同一租户的两个店铺先后落在同一 ASN 上，
// 平台侧把两者串起来的概率会明显上升，运营应当至少知道这件事存在。
type EgressASNShare struct {
	ASN       string    `json:"asn"`
	ASNType   string    `json:"asn_type,omitempty"`
	EnvIDs    []string  `json:"env_ids"`
	Providers []string  `json:"providers"`
	Exits     []string  `json:"exits"`
	Events    int       `json:"events"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
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

// ApprovalRouteRule 审批路由规则（租户可配，migration 0010 / approval_route_rules）。
//
// 它取代的是「写在代码里的审批级别」：单据类型 × 金额门槛 → 谁有资格批、要不要会签。
// 审批级别是运营策略而不是代码不变量——改它不该发版。
// 规则命中的顺序由 Priority 升序决定；ticket_type 为空表示兜底规则（适用于所有类型）。
type ApprovalRouteRule struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	TicketType   string    `json:"ticket_type"` // 见 ApprovalType*；空 = 兜底
	MinAmount    float64   `json:"min_amount"`  // CNY 下限；0 = 不卡金额
	Roles        []string  `json:"roles"`       // 合格审批人角色码；空集 = 任意有审批权的人
	MinApprovers int       `json:"min_approvers"`
	Reason       string    `json:"reason"`
	Priority     int       `json:"priority"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

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

// ---------- 代理机构委托审批（approval_flows，跨租户两方流程） ----------

// DelegationStatus 委托单状态。
type DelegationStatus string

const (
	DelegationPending   DelegationStatus = "pending"   // 待委托方审批
	DelegationActive    DelegationStatus = "active"    // 已批准，授权生效中
	DelegationRejected  DelegationStatus = "rejected"  // 委托方拒绝
	DelegationCancelled DelegationStatus = "cancelled" // 受托方撤回
	DelegationRevoked   DelegationStatus = "revoked"   // 委托方提前撤销（授权即时回收）
	DelegationExpired   DelegationStatus = "expired"   // 申请超期未决 / 委托到期
)

// DelegationFlow 代理机构委托单：受托方（代理机构）申请代管委托方的指定店铺，
// 委托方 owner/ops_manager 裁决。批准后落 DataGrant（店铺级、带到期时间）+ 成员身份，
// 撤销/到期由 delegation.expire 巡检回收。
type DelegationFlow struct {
	ID             string           `json:"id"`
	TenantID       string           `json:"tenant_id"`             // 委托方
	AgencyTenantID string           `json:"agency_tenant_id"`      // 受托方
	AgencyName     string           `json:"agency_name,omitempty"` // 受托方名称快照
	GranteeID      string           `json:"grantee_id"`            // 受托方申请人
	GranteeName    string           `json:"grantee_name,omitempty"`
	ShopIDs        []string         `json:"shop_ids"` // 委托店铺范围
	Reason         string           `json:"reason,omitempty"`
	Comment        string           `json:"comment,omitempty"` // 审批意见
	Status         DelegationStatus `json:"status"`
	Detail         map[string]any   `json:"detail,omitempty"` // grants/role_id 溯源
	ApproverID     string           `json:"approver_id,omitempty"`
	ApproverName   string           `json:"approver_name,omitempty"`
	ExpiresAt      time.Time        `json:"expires_at"`            // 申请单有效期
	ValidUntil     *time.Time       `json:"valid_until,omitempty"` // 委托到期时间
	CreatedAt      time.Time        `json:"created_at"`
	DecidedAt      *time.Time       `json:"decided_at,omitempty"`
	RevokedAt      *time.Time       `json:"revoked_at,omitempty"`
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

	// 外发投递留痕（migration 0006）。
	// 站内通知「写进去了」不等于「人看见了」——high 告警的价值全在最后一公里，
	// 所以外发结果必须和通知本体同生共死，未送达的行留给重投任务继续尝试。
	DeliveryStatus    string     `json:"delivery_status"`              // none|pending|sent|failed
	DeliveryNote      string     `json:"delivery_note,omitempty"`      // 失败摘要（L4：不得含密钥/令牌）
	Attempts          int        `json:"attempts"`                     // 累计尝试次数（含首次）
	DeliveredChannels []string   `json:"delivered_channels,omitempty"` // 已成功送达的通道
	LastDeliveryAt    *time.Time `json:"last_delivery_at,omitempty"`
}

// NotificationPrefs 一人在一个团队里的通知偏好（migration 0008）。
//
// 只记录**外发**通道：站内信是通知本体不可关闭（它是可回看的事实记录），
// 关闭全部开关的含义是「只要站内信」，而不是「什么都不要」。
// 行不存在 = 用户没设置过 → 走默认（所有已装配的外发通道），
// 这样既有租户的行为不会因为这次改造而改变。
type NotificationPrefs struct {
	TenantID  string    `json:"tenant_id"`
	UserID    string    `json:"user_id"`
	Channels  []string  `json:"channels"` // 外发通道白名单，空数组 = 仅站内信
	UpdatedAt time.Time `json:"updated_at"`
}
