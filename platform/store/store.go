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

// ShopFilter 店铺查询过滤。
type ShopFilter struct {
	OwnerID        string // 数据范围 own 时按负责人过滤
	Scope          string // own|team|all
	DelegatedFor   string // own 范围下同时放行「该用户持有有效店铺级 DataGrant」的行（代理机构委托）
	IncludeDeleted bool
	Deleted        bool // true = 只看回收站（软删除）
	// GroupID 按显式分组过滤（0012 / M2 分组页）。
	// 用于「分组改名后同步成员店铺的 group_name」这类批量写路径——
	// 不提供它就得先拉全量再在内存里筛，那和缓存列一样会漂移。
	GroupID string
	Limit   int
	Offset  int
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
	// AddPermissions **并集**补齐权限，返回实际新增条数（已有的不动，幂等）。
	//
	// 给「内置角色权限随版本同步」用：老部署的运营主管拿不到新版本新增的权限码，
	// 而全量 SetPermissions 会抹掉部署侧的差异——并集只补，不删。
	AddPermissions(ctx context.Context, id string, perms []string) (int, error)
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
	// DeleteByScope 物理删除指定用户在某 scope 下的全部授权并返回被删条目
	//（代理机构委托撤销/到期时回收整批店铺授权；返回条目供事件 Data 留痕）。
	DeleteByScope(ctx context.Context, tenantID, userID, scope string) ([]types.DataGrant, error)
	// DeleteExpired 物理删除已到期的临时授权，并返回被删除的条目。
	//
	// 返回被删条目不是可选的花架子：回收任务要给当事人发通知、要在审计里写清
	// 楚「谁的什么权限被回收了」。只返回个计数，通知就变成了「你的某个权限没了」。
	// 未到期（ExpiresAt 为 NULL）的正式授权绝不能被这条语句碰到。
	DeleteExpired(ctx context.Context, tenantID string, now time.Time) ([]types.DataGrant, error)
}

// EgressBindingRepo 环境 ↔ 出口资源的绑定（重启不换 IP 的落点）。
//
// 口径与 DataGrantRepo 一致：**仓储只负责给全数据，不替上层做时间判定**。
// 过期行必须能被读到——分配器靠它知道「这个环境原先绑的是哪条资源」，
// 才能归还厂商资源、才能判断重启后出口是否被迫变更。
type EgressBindingRepo interface {
	// Bind 把环境绑定到一条出口资源；返回 false 表示该资源已被别的环境独占
	// （并发下的真相源是 (provider,resource_id) 唯一索引，不是进程内 map）。
	Bind(ctx context.Context, b *types.EgressBinding) (bool, error)
	// Get 取该环境的绑定；没有绑定时返回 (nil, nil) 而不是 NotFound——
	// 「首次分配」是正常路径，用错误码表达会让调用方靠错误字符串分支。
	Get(ctx context.Context, envID string) (*types.EgressBinding, error)
	ListByTenant(ctx context.Context) ([]types.EgressBinding, error)
	// Release 解除绑定（幂等）。资源的归还需由调用方对代理商发起。
	Release(ctx context.Context, envID string) error
	// DeleteExpired 物理删除到期绑定并返回被删条目：回收任务要据此向厂商归还资源。
	DeleteExpired(ctx context.Context, tenantID string, now time.Time) ([]types.EgressBinding, error)

	// AppendEvent 追加一条出口生命周期事件（只追加，不改写）。
	//
	// 幂等按 id：故障重投/重试可能把同一条事件写两次，重复行会让
	// 「这个 ASN 被用过几次」这类统计直接失真。
	AppendEvent(ctx context.Context, ev *types.EgressBindingEvent) error
	// ListEvents 倒序查历史事件，返回当页与总数。
	ListEvents(ctx context.Context, f EgressHistoryFilter) ([]types.EgressBindingEvent, int, error)
	// ASNSharing 找出被 ≥minEnvs 个**不同环境**使用过的 ASN（since 之后）。
	//
	// 这是反关联的线索：同一租户的两个店铺先后落在同一 ASN 上，
	// 平台把它俩串起来的概率会明显上升。判定放在上层（这是线索不是判决）。
	ASNSharing(ctx context.Context, since time.Time, minEnvs int) ([]types.EgressASNShare, error)
	// PruneEvents 按保留期裁剪历史（物理删除 occurred_at < before 的行），返回删除行数。
	//
	// 只做区间删除，不加触发器：历史是只追加的事实表，裁剪是运营决策（保留多久）
	// 而不是数据不变量。before 之后的行、以及跨租户的行一律不碰。
	PruneEvents(ctx context.Context, tenantID string, before time.Time) (int64, error)
}

// EgressHistoryFilter 出口绑定历史查询过滤。
type EgressHistoryFilter struct {
	EnvID    string
	Provider string
	ASN      string
	Host     string
	Action   string
	From     *time.Time
	To       *time.Time
	Limit    int
	Offset   int
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

	// Consumed 判断 (event, handler) 是否已消费（接收侧投递幂等，event_consumptions 表）。
	//
	// 发件箱只能保证 at-least-once：Handle 成功到 Ack 之间崩溃，整条事件会重投。
	// 没有这张表，重投会让 notify 再生成一整份站内通知、audit 撞审计主键
	// （entry.ID = event.ID），事件卡在 pending 直到重试耗尽。
	Consumed(ctx context.Context, eventID, handler string) (bool, error)
	// MarkConsumed 登记消费完成；重复登记幂等（ON CONFLICT DO NOTHING）。
	// 必须与 Handler 的业务写在**同一事务**内调用（见 audit/notify 两个 Sink），
	// 否则「业务写已提交、登记没提交」的窗口会原样复现重投重复。
	MarkConsumed(ctx context.Context, eventID, handler string) error
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
	// UpdateDelivery 回写投递结果（id + tenant_id 双重定位）。
	// 只更新 COLUMN delivery_* / attempts / delivered_channels / last_delivery_at，
	// 不碰标题正文——重投时手里拿的可能是一份旧快照。
	UpdateDelivery(ctx context.Context, n *types.Notification) error
	// ListPendingDelivery 待重投通知：delivery_status ∈ (pending, failed)，按创建时间升序。
	// limit<=0 时取 100。注意调用方必须带租户上下文（RLS），本接口不做跨租户兜底。
	ListPendingDelivery(ctx context.Context, tenantID string, limit int) ([]types.Notification, error)

	// CountUnread 未读条数（通知角标用）。tenantID+userID 双条件，
	// 调用方仍需带租户上下文（RLS 兜底）。
	CountUnread(ctx context.Context, tenantID, userID string) (int, error)
	// GetPrefs 取通知偏好。**无行返回 (nil, nil)**，语义是「未设置」，
	// 由上层按默认处理；不要把它当成 error。
	GetPrefs(ctx context.Context, tenantID, userID string) (*types.NotificationPrefs, error)
	// SetPrefs 写通知偏好（UPSERT，按 (tenant_id, user_id) 覆盖）。
	SetPrefs(ctx context.Context, p *types.NotificationPrefs) error
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
	// Delete 软删除（写 deleted_at，与 shops 同口径）：台账出清但审计与出口历史保留。
	Delete(ctx context.Context, id string) error
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

// EnvironmentFilter 环境聚合列表过滤（M2 店铺网格页）。
//
// 过滤必须下推到仓储，不能在网关里「拉全表再筛」：那会让 total 与分页在第 N 页
// 之后算错，且 mem 与 pg 两条路径的口径必须一致（ADR-0007）。
type EnvironmentFilter struct {
	// ShopIDs 数据级收口后的可见店铺集合（own 范围 + 委托放行后的结果）。
	// nil = 不过滤（all/team 范围）；空切片与 nil 同义。
	ShopIDs  []string
	GroupID  string // shops.group_id
	Platform string // shops.platform
	Status   string // environments.status
	Country  string // environments.country（列表筛选用，指纹权威仍在 snapshot）
	Keyword  string // 店铺名 / 编号 / 环境 ID 模糊匹配
	ProxyID  string // environments.proxy_id
	Limit    int
	Offset   int
}

type EnvironmentRepo interface {
	Create(ctx context.Context, e *types.Environment) error
	Get(ctx context.Context, id string) (*types.Environment, error)
	GetByShop(ctx context.Context, shopID string) (*types.Environment, error)
	Update(ctx context.Context, e *types.Environment) error
	List(ctx context.Context, limit, offset int) ([]types.Environment, int, error)
	// Search 带过滤的聚合列表（M2 店铺网格页）。
	//
	// 只返回未软删店铺下的环境：已删店铺的环境在界面上是幽灵行，
	// 运营点进去才发现「店铺没了、环境还在」，那是回收站语义没对齐。
	Search(ctx context.Context, f EnvironmentFilter) ([]types.Environment, int, error)
	// Delete 物理删除环境行（终态动作：出口归还、事件留痕由服务层负责，仓储只删行）。
	Delete(ctx context.Context, id string) error
	// SetStatusIfIn 只改状态的条件置位：当前状态属于 from 才置为 to，返回是否命中。
	//
	// 用途是**止损**：启动失败复位、对账收回残留 running。这些路径必须只依赖
	// 「状态」这一列——走整行 Update 的话，任一列写错就会让复位跟着崩，
	// 环境悬在 running 上没人收。
	//
	// 条件写在 SQL 里（而不是「先读后写」）：读到的状态可能是几百毫秒前的，
	// 「绝不覆盖 running」只能由语句自己保证。
	SetStatusIfIn(ctx context.Context, id string, from []string, to string) (bool, error)
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

// ---------- 启动器内核（M1 / 0012）：手工代理与分组 ----------

// ProxyFilter 手工代理查询过滤。
type ProxyFilter struct {
	Status string // active|disabled，空 = 不过滤
	// CheckStatus unknown|ok|fail，空 = 不过滤（P3：绑定下拉只列 ok 的代理）。
	CheckStatus string
	// Keyword 名称 / 主机 / 备注模糊匹配（mem 与 pg 必须同口径，避免「本地能搜到、线上搜不到」）。
	Keyword string
	Limit   int
	Offset  int
}

// ProxyCheckResult 一次拨测的结论（P3）。
//
// 刻意只装「拨测列」而不装整行 types.Proxy：整行 UPDATE 会把调用方手里那个
// PasswordEnc（常规读路径恒为空）写回库里，**把口令抹掉**。拨测结果落库是
// 启动链路上的高频动作，绝不能让它有机会碰到凭据列。
type ProxyCheckResult struct {
	CheckStatus string    // ok|fail|unknown
	ExitIP      string    // 实测出口 IP；拿不到留空（不猜）
	ExitCountry string    // 实测国家；未配置国家探测目标时沿用台账 country
	LatencyMS   int       // 拨测耗时
	CheckedAt   time.Time // 拨测时刻（与 LatencyMS 成对出现，见 0013 的 CHECK）
	LastError   string    // 失败原因，**已裁剪、不含任何凭据**
}

// ProxyRepo 手工静态代理台账。
//
// 契约与其余租户域仓储一致：tenant_id 一律从 ctx 取（RLS fail-closed），
// 明文口令永不落在这里——PasswordEnc 是信封加密密文，解密只发生在启动链路的
// 进程内存里。
type ProxyRepo interface {
	Create(ctx context.Context, p *types.Proxy) error
	Get(ctx context.Context, id string) (*types.Proxy, error)
	List(ctx context.Context, f ProxyFilter) ([]*types.Proxy, int, error)
	Update(ctx context.Context, p *types.Proxy) error
	Delete(ctx context.Context, id string) error
	// CountBound 该代理被多少个环境引用（删除前置校验的唯一判据）。
	CountBound(ctx context.Context, id string) (int, error)
	// CountBoundRunning 该代理被多少个**运行中**环境引用（P3：一号一 IP 告警）。
	//
	// 与 CountBound 分开：被 3 个空闲环境绑定只是「以后会共用」，被 2 个运行中
	// 环境绑定是「此刻正在共用」——后者才是平台能直接观察到的关联信号，
	// 告警措辞与紧急程度都不同。
	CountBoundRunning(ctx context.Context, id string) (int, error)
	// SaveCheckResult 落一次拨测结论（P3）。
	//
	// 只 UPDATE 拨测相关的 6 列，**永不触碰 password_enc**：拨测在启动链路上
	// 会被反复调用，一旦它走整行写回，手里那份空密文就会把真实口令清掉。
	SaveCheckResult(ctx context.Context, id string, r ProxyCheckResult) error
	// PasswordEnc 取该代理的口令密文。
	//
	// 刻意独立成方法、且**不在** List/Get 的列里（proxyColumns 显式排除 password_enc）：
	// 「读密文」这个动作必须在代码里看得见，才能被 review——一旦它混进常规读路径，
	// 迟早会跟着某个整行序列化进响应或日志。调用方拿到后只能在进程内存里解密，
	// 用于本地 TLSProxy 上游配置或拨测，绝不回传。
	PasswordEnc(ctx context.Context, id string) (string, error)
}

// GroupRepo 店铺分组。
//
// EnvironmentCount 由 List 一次 GROUP BY 带出，不建缓存列；
// CountMembers 是「空分组才能删除」的唯一判据，两者都必须实时。
type GroupRepo interface {
	Create(ctx context.Context, g *types.Group) error
	Get(ctx context.Context, id string) (*types.Group, error)
	List(ctx context.Context) ([]*types.Group, error) // 带 environment_count，按 sort, created_at
	Update(ctx context.Context, g *types.Group) error
	Delete(ctx context.Context, id string) error
	CountMembers(ctx context.Context, id string) (int, error)
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

// ---------- 代理机构委托审批（approval_flows） ----------

// DelegationFilter 委托单查询过滤。
//
// Party 决定以哪一方视角查询：grantor（委托方，默认）按 tenant_id 匹配；
// agency（受托方）按 agency_tenant_id 匹配。两列上都显式带谓词——
// 不依赖 RLS 单独兜底（写路径显式 tenant_id 谓词才是防线）。
type DelegationFilter struct {
	Status types.DelegationStatus
	Party  string // "grantor"（默认）| "agency"
	Limit  int
	Offset int
}

type DelegationRepo interface {
	Create(ctx context.Context, f *types.DelegationFlow) error
	// Get 双主体可读：id 命中且 tenant_id 或 agency_tenant_id 等于上下文租户。
	Get(ctx context.Context, id string) (*types.DelegationFlow, error)
	Update(ctx context.Context, f *types.DelegationFlow) error
	List(ctx context.Context, f DelegationFilter) ([]types.DelegationFlow, int, error)
	// HasPendingBetween 同一（委托方, 受托方）是否已有待审批单——重复申请闸门。
	// 显式传双方租户 ID，不取上下文（提交方是受托方，行的 tenant_id 是委托方）。
	HasPendingBetween(ctx context.Context, grantorTenantID, agencyTenantID string) (bool, error)
}

// ApprovalRouteRuleRepo 审批路由规则（租户可配）。
type ApprovalRouteRuleRepo interface {
	Create(ctx context.Context, r *types.ApprovalRouteRule) error
	Get(ctx context.Context, id string) (*types.ApprovalRouteRule, error)
	Update(ctx context.Context, r *types.ApprovalRouteRule) error
	// ListByTenant 按优先级升序返回（含停用行，由上层决定是否跳过）。
	ListByTenant(ctx context.Context, tenantID string) ([]types.ApprovalRouteRule, error)
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
	Delegations() DelegationRepo
	EgressBindings() EgressBindingRepo
	ApprovalRouteRules() ApprovalRouteRuleRepo
	// M1 启动器内核
	Proxies() ProxyRepo
	Groups() GroupRepo
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
