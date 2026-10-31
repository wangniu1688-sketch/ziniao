// Package store 持久化契约（Repository 接口 + 事务）。
//
// 关键设计：
//  1. 租户上下文由 context 携带，所有租户域仓储强制从 ctx 取 tenant_id，
//     取不到即报错（fail-closed），避免「忘了加 where tenant_id」（L12）。
//  2. 业务写与事件总线投递必须在同一事务内（WithTx），保证「写操作必有事件」
//     （跨模块铁律：写 → eventbus → audit）。
//  3. 双驱动：memory（零依赖本地启动/测试）与 postgres（生产，含 RLS）。
package store

import (
	"context"
	"errors"
	"time"

	"ziniao/platform/types"
)

// ErrNoTenant 上下文中缺少租户时的哨兵错误（fail-closed）。
var ErrNoTenant = errors.New("store: 上下文缺少 tenant_id，拒绝执行租户域查询")

type ctxKeyTenant struct{}

// WithTenant 将租户写入上下文。
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, ctxKeyTenant{}, tenantID)
}

// TenantFrom 读取上下文租户；缺失返回 ErrNoTenant。
func TenantFrom(ctx context.Context) (string, error) {
	v, ok := ctx.Value(ctxKeyTenant{}).(string)
	if !ok || v == "" {
		return "", ErrNoTenant
	}
	return v, nil
}

// ---------- 过滤与分页 ----------

type ShopFilter struct {
	OwnerID        string // 数据范围 own 时按负责人过滤
	Scope          string // own|team|all
	IncludeDeleted bool
	Deleted        bool // true = 只看回收站（软删除）
	Limit          int
	Offset         int
}

type AuditFilter struct {
	Category    types.AuditCategory
	ActorUserID string
	ShopID      string
	Action      string
	From        *time.Time
	To          *time.Time
	Keyword     string
	Limit       int
	Offset      int
}

// ---------- 仓储接口 ----------

type TenantRepo interface {
	Create(ctx context.Context, t *types.Tenant) error
	Get(ctx context.Context, id string) (*types.Tenant, error)
	GetBySlug(ctx context.Context, slug string) (*types.Tenant, error)
	// List 全量团队列表（定时任务与运营后台用；无租户上下文，RLS 由 admin_list 策略放行）。
	List(ctx context.Context, limit, offset int) ([]types.Tenant, int, error)
}

type UserRepo interface {
	Create(ctx context.Context, u *types.User) error
	GetByID(ctx context.Context, id string) (*types.User, error)
	GetByUsername(ctx context.Context, username string) (*types.User, error)
	TouchLogin(ctx context.Context, id string, at time.Time) error
}

type CredentialRepo interface {
	Get(ctx context.Context, userID string) (*types.Credential, error)
	Upsert(ctx context.Context, c *types.Credential) error
}

type MembershipRepo interface {
	Create(ctx context.Context, m *types.Membership) error
	Get(ctx context.Context, tenantID, userID string) (*types.Membership, error)
	ListByTenant(ctx context.Context, tenantID string) ([]types.Membership, error)
	ListByUser(ctx context.Context, userID string) ([]types.Membership, error)
}

type RoleRepo interface {
	Create(ctx context.Context, r *types.Role) error
	Get(ctx context.Context, id string) (*types.Role, error)
	GetByCode(ctx context.Context, tenantID, code string) (*types.Role, error)
	List(ctx context.Context, tenantID string) ([]types.Role, error)
	SetPermissions(ctx context.Context, id string, perms []string) error
}

type UserRoleRepo interface {
	Assign(ctx context.Context, ur *types.UserRole) error
	Revoke(ctx context.Context, tenantID, userID, roleID string) error
	ListByUser(ctx context.Context, tenantID, userID string) ([]types.UserRole, error)
	ListByTenant(ctx context.Context, tenantID string) ([]types.UserRole, error)
}

type DataGrantRepo interface {
	Create(ctx context.Context, g *types.DataGrant) error
	ListByUser(ctx context.Context, tenantID, userID, resourceType string) ([]types.DataGrant, error)
	// DeleteExpired 物理删除已到期的临时授权，并返回被删除的条目。
	//
	// 返回被删条目不是可选的花架子：回收任务要给当事人发通知、要在审计里写清
	// 楚「谁的什么权限被回收了」。只返回个计数，通知就变成了「你的某个权限没了」。
	// 未到期（ExpiresAt 为 NULL）的正式授权绝不能被这条语句碰到。
	DeleteExpired(ctx context.Context, tenantID string, now time.Time) ([]types.DataGrant, error)
}

type SessionRepo interface {
	Create(ctx context.Context, s *types.Session) error
	Get(ctx context.Context, id string) (*types.Session, error)
	Revoke(ctx context.Context, id string) error
	ListByUser(ctx context.Context, userID string) ([]types.Session, error)
}

type ShopRepo interface {
	Create(ctx context.Context, s *types.Shop) error
	Get(ctx context.Context, id string) (*types.Shop, error)
	List(ctx context.Context, f ShopFilter) ([]types.Shop, int, error)
	Update(ctx context.Context, s *types.Shop) error
	Count(ctx context.Context) (int, error)
	// Delete 物理删除（仅回收站清理 / 合规删除用；业务删除一律走软删）。
	Delete(ctx context.Context, id string) error
}

type SubscriptionRepo interface {
	Create(ctx context.Context, s *types.Subscription) error
	GetActive(ctx context.Context, tenantID string) (*types.Subscription, error)
}

// UsageRepo 计量计数器。写路径只经 billing.Meter，禁止各模块自增。
type UsageRepo interface {
	Incr(ctx context.Context, tenantID, metric string, delta int64) (int64, error)
	Get(ctx context.Context, tenantID, metric string) (int64, error)
}

type OutboxRepo interface {
	Insert(ctx context.Context, e *types.OutboxEnvelope) error
	Claim(ctx context.Context, limit int) ([]types.OutboxEnvelope, error)
	Ack(ctx context.Context, id string) error
	Fail(ctx context.Context, id string, errMsg string) error
}

type AuditRepo interface {
	// Append 必须在事务内调用：读取上一条哈希 → 计算本条哈希 → 写入（哈希链，防篡改）。
	Append(ctx context.Context, e *types.AuditEntry) error
	Last(ctx context.Context, tenantID string) (*types.AuditEntry, error)
	List(ctx context.Context, f AuditFilter) ([]types.AuditEntry, int, error)
	Since(ctx context.Context, tenantID string, from time.Time) ([]types.AuditEntry, error)
}

type NotificationRepo interface {
	Create(ctx context.Context, n *types.Notification) error
	List(ctx context.Context, tenantID, userID string, limit int) ([]types.Notification, error)
	MarkRead(ctx context.Context, tenantID, id string) error
}

// ---------- Phase 1：设备 / 网络 / 环境 / 安全 / 凭证托管 ----------

// DeviceFilter 设备查询过滤。
type DeviceFilter struct {
	Type           types.DeviceType
	Status         types.DeviceStatus
	Country        string
	IncludeDeleted bool
	Limit          int
	Offset         int
}

// BindingFilter 绑定关系过滤。
type BindingFilter struct {
	DeviceID string
	ShopID   string
	UserID   string
	Active   bool // 仅未解绑
}

type DeviceRepo interface {
	Create(ctx context.Context, d *types.Device) error
	Get(ctx context.Context, id string) (*types.Device, error)
	Update(ctx context.Context, d *types.Device) error
	List(ctx context.Context, f DeviceFilter) ([]types.Device, int, error)
	Count(ctx context.Context) (int, error)
	IncrBound(ctx context.Context, id string, delta int) error
}

type BindingRepo interface {
	Create(ctx context.Context, b *types.DeviceBinding) error
	List(ctx context.Context, f BindingFilter) ([]types.DeviceBinding, error)
	Unbind(ctx context.Context, id string) error
	// CountConcurrent 统计设备当前并发使用人数（按 user_id 去重，L9 一人多店占 1 个槽）。
	CountConcurrent(ctx context.Context, deviceID string) (int, error)
	// CountBound 统计设备已绑定账号数。
	CountBound(ctx context.Context, deviceID string) (int, error)
	// Touch 更新占用者与最近使用时间。
	Touch(ctx context.Context, id, userID string, at time.Time) error
}

type EnvironmentRepo interface {
	Create(ctx context.Context, e *types.Environment) error
	Get(ctx context.Context, id string) (*types.Environment, error)
	GetByShop(ctx context.Context, shopID string) (*types.Environment, error)
	Update(ctx context.Context, e *types.Environment) error
	List(ctx context.Context, limit, offset int) ([]types.Environment, int, error)
}

type AccessPolicyRepo interface {
	Create(ctx context.Context, p *types.AccessPolicy) error
	Update(ctx context.Context, p *types.AccessPolicy) error
	Get(ctx context.Context, id string) (*types.AccessPolicy, error)
	List(ctx context.Context, scope, scopeID string) ([]types.AccessPolicy, error)
	Delete(ctx context.Context, id string) error
}

// SecurityEventFilter 安全事件过滤。
type SecurityEventFilter struct {
	Category string
	Level    string
	ShopID   string
	From     *time.Time
	Limit    int
	Offset   int
}

type SecurityEventRepo interface {
	Create(ctx context.Context, e *types.SecurityEvent) error
	List(ctx context.Context, f SecurityEventFilter) ([]types.SecurityEvent, int, error)
}

// ApprovalFilter 审批单查询过滤。
type ApprovalFilter struct {
	Status      types.ApprovalStatus
	Type        string
	RequesterID string
	Limit       int
	Offset      int
}

type ApprovalRepo interface {
	Create(ctx context.Context, t *types.ApprovalTicket) error
	Get(ctx context.Context, id string) (*types.ApprovalTicket, error)
	Update(ctx context.Context, t *types.ApprovalTicket) error
	List(ctx context.Context, f ApprovalFilter) ([]types.ApprovalTicket, int, error)
	// ExpireBefore 把过期且未决的审批单置为 expired，返回受影响条数。
	ExpireBefore(ctx context.Context, at time.Time) (int, error)
}

type VaultRepo interface {
	Upsert(ctx context.Context, c *types.ShopCredential) error
	Get(ctx context.Context, shopID string, kind types.CredentialKind) (*types.ShopCredential, error)
	List(ctx context.Context, shopID string) ([]types.ShopCredential, error)
	Delete(ctx context.Context, id string) error
}

// Repositories 全部仓储集合。
type Repositories interface {
	Tenants() TenantRepo
	Users() UserRepo
	Credentials() CredentialRepo
	Memberships() MembershipRepo
	Roles() RoleRepo
	UserRoles() UserRoleRepo
	DataGrants() DataGrantRepo
	Sessions() SessionRepo
	Shops() ShopRepo
	Subscriptions() SubscriptionRepo
	Usage() UsageRepo
	Outbox() OutboxRepo
	Audit() AuditRepo
	Notifications() NotificationRepo
	Devices() DeviceRepo
	Bindings() BindingRepo
	Environments() EnvironmentRepo
	Policies() AccessPolicyRepo
	SecurityEvents() SecurityEventRepo
	Vault() VaultRepo
	Approvals() ApprovalRepo
}

// Tx 事务句柄：与业务写在同一事务内投递事件。
type Tx interface {
	Repositories
}

// Store 存储入口。
type Store interface {
	Repositories
	// WithTx 执行事务；fn 返回 error 则回滚。
	WithTx(ctx context.Context, fn func(Tx) error) error
	Close() error
}
