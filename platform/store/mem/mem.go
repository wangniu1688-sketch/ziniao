// Package mem 内存存储驱动：零依赖，用于本地启动、单元测试与演示
// （本机无 Docker/PostgreSQL 时的默认驱动，生产请用 postgres 驱动）。
//
// 限制：WithTx 提供互斥与串行化，但不支持回滚语义（fn 返回 error 时已写入的
// 内存变更不会撤销）。仅用于 dev/test，禁止用于生产。
package mem

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Store 内存存储。
type Store struct {
	mu sync.Mutex

	tenants     map[string]*types.Tenant
	slugIndex   map[string]string
	users       map[string]*types.User
	userIndex   map[string]string
	creds       map[string]*types.Credential
	memberships map[string]*types.Membership
	roles       map[string]*types.Role
	userRoles   map[string]*types.UserRole
	grants      []*types.DataGrant
	sessions    map[string]*types.Session
	shops       map[string]*types.Shop
	subs        map[string]*types.Subscription
	usage       map[string]int64
	outbox      []*types.OutboxEnvelope
	audit       []*types.AuditEntry
	notifs      []*types.Notification

	// Phase 1：设备 / 网络 / 环境 / 安全策略 / 安全事件 / 凭证托管
	devices   map[string]*types.Device
	bindings  []*types.DeviceBinding
	envs      map[string]*types.Environment
	envByShop map[string]string
	policies  map[string]*types.AccessPolicy
	secEvents []*types.SecurityEvent
	vault     map[string]*types.ShopCredential

	// Phase 2：审批工单
	approvals map[string]*types.ApprovalTicket
}

// New 创建空存储。
func New() *Store {
	return &Store{
		tenants: map[string]*types.Tenant{}, slugIndex: map[string]string{},
		users: map[string]*types.User{}, userIndex: map[string]string{},
		creds: map[string]*types.Credential{}, memberships: map[string]*types.Membership{},
		roles: map[string]*types.Role{}, userRoles: map[string]*types.UserRole{},
		sessions: map[string]*types.Session{}, shops: map[string]*types.Shop{},
		subs: map[string]*types.Subscription{}, usage: map[string]int64{},
		devices: map[string]*types.Device{}, envs: map[string]*types.Environment{},
		envByShop: map[string]string{}, policies: map[string]*types.AccessPolicy{},
		vault: map[string]*types.ShopCredential{}, approvals: map[string]*types.ApprovalTicket{},
	}
}

// Close 空实现（内存驱动无需释放）。
func (s *Store) Close() error { return nil }

// WithTx 串行执行事务闭包。
func (s *Store) WithTx(ctx context.Context, fn func(store.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(s)
}

// 仓储访问器。
func (s *Store) Tenants() store.TenantRepo               { return tenantRepo{s} }
func (s *Store) Users() store.UserRepo                   { return userRepo{s} }
func (s *Store) Credentials() store.CredentialRepo       { return credRepo{s} }
func (s *Store) Memberships() store.MembershipRepo       { return memberRepo{s} }
func (s *Store) Roles() store.RoleRepo                   { return roleRepo{s} }
func (s *Store) UserRoles() store.UserRoleRepo           { return userRoleRepo{s} }
func (s *Store) DataGrants() store.DataGrantRepo         { return grantRepo{s} }
func (s *Store) Sessions() store.SessionRepo             { return sessionRepo{s} }
func (s *Store) Shops() store.ShopRepo                   { return shopRepo{s} }
func (s *Store) Subscriptions() store.SubscriptionRepo   { return subRepo{s} }
func (s *Store) Usage() store.UsageRepo                  { return usageRepo{s} }
func (s *Store) Outbox() store.OutboxRepo                { return outboxRepo{s} }
func (s *Store) Audit() store.AuditRepo                  { return auditRepo{s} }
func (s *Store) Notifications() store.NotificationRepo   { return notifRepo{s} }
func (s *Store) Devices() store.DeviceRepo               { return deviceRepo{s} }
func (s *Store) Bindings() store.BindingRepo             { return bindingRepo{s} }
func (s *Store) Environments() store.EnvironmentRepo     { return envRepo{s} }
func (s *Store) Policies() store.AccessPolicyRepo        { return policyRepo{s} }
func (s *Store) SecurityEvents() store.SecurityEventRepo { return secEventRepo{s} }
func (s *Store) Vault() store.VaultRepo                  { return vaultRepo{s} }
func (s *Store) Approvals() store.ApprovalRepo           { return approvalRepo{s} }

// ---------- 租户 ----------

type tenantRepo struct{ s *Store }

func (r tenantRepo) Create(_ context.Context, t *types.Tenant) error {
	if _, ok := r.s.tenants[t.ID]; ok {
		return conflict("tenant", t.ID)
	}
	cp := *t
	r.s.tenants[t.ID] = &cp
	r.s.slugIndex[t.Slug] = t.ID
	return nil
}

func (r tenantRepo) Get(_ context.Context, id string) (*types.Tenant, error) {
	t, ok := r.s.tenants[id]
	if !ok {
		return nil, notFound("tenant", id)
	}
	cp := *t
	return &cp, nil
}

func (r tenantRepo) GetBySlug(_ context.Context, slug string) (*types.Tenant, error) {
	id, ok := r.s.slugIndex[slug]
	if !ok {
		return nil, notFound("tenant", slug)
	}
	cp := *r.s.tenants[id]
	return &cp, nil
}

func (r tenantRepo) List(_ context.Context, limit, offset int) ([]types.Tenant, int, error) {
	out := []types.Tenant{}
	for _, t := range r.s.tenants {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	total := len(out)
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return out[offset:end], total, nil
}

// ---------- 用户 ----------

type userRepo struct{ s *Store }

func (r userRepo) Create(_ context.Context, u *types.User) error {
	if _, ok := r.s.users[u.ID]; ok {
		return conflict("user", u.ID)
	}
	cp := *u
	r.s.users[u.ID] = &cp
	r.s.userIndex[u.Username] = u.ID
	return nil
}

func (r userRepo) GetByID(_ context.Context, id string) (*types.User, error) {
	u, ok := r.s.users[id]
	if !ok {
		return nil, notFound("user", id)
	}
	cp := *u
	return &cp, nil
}

func (r userRepo) GetByUsername(_ context.Context, username string) (*types.User, error) {
	id, ok := r.s.userIndex[username]
	if !ok {
		return nil, notFound("user", username)
	}
	cp := *r.s.users[id]
	return &cp, nil
}

func (r userRepo) TouchLogin(_ context.Context, id string, at time.Time) error {
	u, ok := r.s.users[id]
	if !ok {
		return notFound("user", id)
	}
	u.LastLoginAt = &at
	return nil
}

type credRepo struct{ s *Store }

func (r credRepo) Get(_ context.Context, userID string) (*types.Credential, error) {
	c, ok := r.s.creds[userID]
	if !ok {
		return nil, notFound("credential", userID)
	}
	cp := *c
	return &cp, nil
}

func (r credRepo) Upsert(_ context.Context, c *types.Credential) error {
	cp := *c
	r.s.creds[c.UserID] = &cp
	return nil
}

// ---------- 成员 ----------

type memberRepo struct{ s *Store }

func memberKey(tenantID, userID string) string { return tenantID + "|" + userID }

func (r memberRepo) Create(_ context.Context, m *types.Membership) error {
	k := memberKey(m.TenantID, m.UserID)
	if _, ok := r.s.memberships[k]; ok {
		return conflict("membership", k)
	}
	cp := *m
	r.s.memberships[k] = &cp
	return nil
}

func (r memberRepo) Get(_ context.Context, tenantID, userID string) (*types.Membership, error) {
	m, ok := r.s.memberships[memberKey(tenantID, userID)]
	if !ok {
		return nil, notFound("membership", userID)
	}
	cp := *m
	return &cp, nil
}

func (r memberRepo) ListByTenant(_ context.Context, tenantID string) ([]types.Membership, error) {
	out := []types.Membership{}
	for _, m := range r.s.memberships {
		if m.TenantID == tenantID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JoinedAt.After(out[j].JoinedAt) })
	return out, nil
}

func (r memberRepo) ListByUser(_ context.Context, userID string) ([]types.Membership, error) {
	out := []types.Membership{}
	for _, m := range r.s.memberships {
		if m.UserID == userID {
			out = append(out, *m)
		}
	}
	return out, nil
}

// ---------- 角色 ----------

type roleRepo struct{ s *Store }

func (r roleRepo) Create(_ context.Context, role *types.Role) error {
	if _, ok := r.s.roles[role.ID]; ok {
		return conflict("role", role.ID)
	}
	cp := *role
	r.s.roles[role.ID] = &cp
	return nil
}

func (r roleRepo) Get(_ context.Context, id string) (*types.Role, error) {
	role, ok := r.s.roles[id]
	if !ok {
		return nil, notFound("role", id)
	}
	cp := *role
	cp.Permissions = append([]string(nil), role.Permissions...)
	return &cp, nil
}

func (r roleRepo) GetByCode(_ context.Context, tenantID, code string) (*types.Role, error) {
	for _, role := range r.s.roles {
		if role.Code == code && (role.TenantID == tenantID || role.TenantID == "") {
			cp := *role
			cp.Permissions = append([]string(nil), role.Permissions...)
			return &cp, nil
		}
	}
	return nil, notFound("role", code)
}

func (r roleRepo) List(_ context.Context, tenantID string) ([]types.Role, error) {
	out := []types.Role{}
	for _, role := range r.s.roles {
		if role.TenantID == tenantID || role.TenantID == "" {
			cp := *role
			cp.Permissions = append([]string(nil), role.Permissions...)
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, nil
}

func (r roleRepo) SetPermissions(_ context.Context, id string, perms []string) error {
	role, ok := r.s.roles[id]
	if !ok {
		return notFound("role", id)
	}
	role.Permissions = append([]string(nil), perms...)
	return nil
}

type userRoleRepo struct{ s *Store }

func urKey(tenantID, userID, roleID string) string { return tenantID + "|" + userID + "|" + roleID }

func (r userRoleRepo) Assign(_ context.Context, ur *types.UserRole) error {
	cp := *ur
	r.s.userRoles[urKey(ur.TenantID, ur.UserID, ur.RoleID)] = &cp
	return nil
}

func (r userRoleRepo) Revoke(_ context.Context, tenantID, userID, roleID string) error {
	delete(r.s.userRoles, urKey(tenantID, userID, roleID))
	return nil
}

func (r userRoleRepo) ListByUser(_ context.Context, tenantID, userID string) ([]types.UserRole, error) {
	out := []types.UserRole{}
	for _, ur := range r.s.userRoles {
		if ur.TenantID == tenantID && ur.UserID == userID {
			out = append(out, *ur)
		}
	}
	return out, nil
}

func (r userRoleRepo) ListByTenant(_ context.Context, tenantID string) ([]types.UserRole, error) {
	out := []types.UserRole{}
	for _, ur := range r.s.userRoles {
		if ur.TenantID == tenantID {
			out = append(out, *ur)
		}
	}
	return out, nil
}

type grantRepo struct{ s *Store }

func (r grantRepo) Create(_ context.Context, g *types.DataGrant) error {
	cp := *g
	r.s.grants = append(r.s.grants, &cp)
	return nil
}

// ListByUser 返回该用户的全部授权记录（**含已过期的**）。
//
// 刻意不在仓储层吞掉过期行：这里曾经有 `expires_at < now 则跳过` 的过滤，
// 而 identity 判定「角色是否因临时授权到期而失效」恰恰是拿这些过期记录做依据的
// ——仓储先把它过滤掉，上层就永远看不到，结果是**临时授权到期后权限照旧**。
// 过期与否由策略层（identity / iam）拿着 now 判定，仓储只负责把数据给全。
//
// 注意：这里**不能**加 s.mu.Lock()。mem 的 WithTx 已经持有同一把不可重入的锁，
// 仓储方法再锁一次会当场自死锁（在自己的委员会帮助下重现过：测试卡 10 分钟超时）。
// 仓储层不负责并发控制，事务边界在 WithTx。
func (r grantRepo) ListByUser(_ context.Context, tenantID, userID, resourceType string) ([]types.DataGrant, error) {
	out := []types.DataGrant{}
	for _, g := range r.s.grants {
		if g.TenantID != tenantID || g.UserID != userID {
			continue
		}
		if resourceType != "" && g.ResourceType != resourceType {
			continue
		}
		out = append(out, *g)
	}
	return out, nil
}

// DeleteExpired 删除到期行并返回副本（mem 的切片里存的是指针，直接返回 *g
// 会把内部状态泄漏出去，调用方改一下就等于改了库）。
// 锁的说明见 ListByUser：并发由 WithTx 负责，此处不得加锁。
func (r grantRepo) DeleteExpired(_ context.Context, tenantID string, now time.Time) ([]types.DataGrant, error) {
	out := []types.DataGrant{}
	kept := make([]*types.DataGrant, 0, len(r.s.grants))
	for _, g := range r.s.grants {
		if g.TenantID == tenantID && g.ExpiresAt != nil && !g.ExpiresAt.After(now) {
			out = append(out, *g)
			continue
		}
		kept = append(kept, g)
	}
	if len(out) > 0 {
		r.s.grants = kept
	}
	return out, nil
}

// ---------- 会话 ----------

type sessionRepo struct{ s *Store }

func (r sessionRepo) Create(_ context.Context, sess *types.Session) error {
	cp := *sess
	r.s.sessions[sess.ID] = &cp
	return nil
}

func (r sessionRepo) Get(_ context.Context, id string) (*types.Session, error) {
	sess, ok := r.s.sessions[id]
	if !ok {
		return nil, notFound("session", id)
	}
	cp := *sess
	return &cp, nil
}

func (r sessionRepo) Revoke(_ context.Context, id string) error {
	sess, ok := r.s.sessions[id]
	if !ok {
		return notFound("session", id)
	}
	now := time.Now()
	sess.RevokedAt = &now
	return nil
}

func (r sessionRepo) ListByUser(_ context.Context, userID string) ([]types.Session, error) {
	out := []types.Session{}
	for _, sess := range r.s.sessions {
		if sess.UserID == userID {
			out = append(out, *sess)
		}
	}
	return out, nil
}

// ---------- 店铺 ----------

type shopRepo struct{ s *Store }

func (r shopRepo) Create(ctx context.Context, shop *types.Shop) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	shop.TenantID = tid
	cp := *shop
	r.s.shops[shop.ID] = &cp
	return nil
}

func (r shopRepo) Get(ctx context.Context, id string) (*types.Shop, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	shop, ok := r.s.shops[id]
	if !ok || shop.TenantID != tid {
		return nil, notFound("shop", id)
	}
	cp := *shop
	return &cp, nil
}

func (r shopRepo) List(ctx context.Context, f store.ShopFilter) ([]types.Shop, int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.Shop{}
	for _, shop := range r.s.shops {
		if shop.TenantID != tid {
			continue
		}
		if f.Deleted {
			if shop.DeletedAt == nil {
				continue
			}
		} else if !f.IncludeDeleted && shop.DeletedAt != nil {
			continue
		}
		if f.Scope == "own" && shop.OwnerID != f.OwnerID {
			continue
		}
		out = append(out, *shop)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	total := len(out)
	if f.Offset > 0 && f.Offset < len(out) {
		out = out[f.Offset:]
	} else if f.Offset >= len(out) {
		out = nil
	}
	if f.Limit > 0 && f.Limit < len(out) {
		out = out[:f.Limit]
	}
	return out, total, nil
}

func (r shopRepo) Update(ctx context.Context, shop *types.Shop) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	cur, ok := r.s.shops[shop.ID]
	if !ok || cur.TenantID != tid {
		return notFound("shop", shop.ID)
	}
	cp := *shop
	cp.TenantID = tid
	r.s.shops[shop.ID] = &cp
	return nil
}

func (r shopRepo) Count(ctx context.Context) (int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, shop := range r.s.shops {
		if shop.TenantID == tid && shop.DeletedAt == nil {
			n++
		}
	}
	return n, nil
}

func (r shopRepo) Delete(ctx context.Context, id string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	sp, ok := r.s.shops[id]
	if !ok || sp.TenantID != tid {
		return notFound("shop", id)
	}
	if sp.DeletedAt == nil {
		return conflict("shop", id) // 未进回收站不允许物理删除（防止误删在营店铺）
	}
	delete(r.s.shops, id)
	return nil
}

// ---------- 订阅 / 计量 ----------

type subRepo struct{ s *Store }

func (r subRepo) Create(_ context.Context, sub *types.Subscription) error {
	cp := *sub
	r.s.subs[sub.TenantID] = &cp
	return nil
}

func (r subRepo) GetActive(_ context.Context, tenantID string) (*types.Subscription, error) {
	sub, ok := r.s.subs[tenantID]
	if !ok {
		return nil, notFound("subscription", tenantID)
	}
	cp := *sub
	return &cp, nil
}

type usageRepo struct{ s *Store }

func (r usageRepo) Incr(_ context.Context, tenantID, metric string, delta int64) (int64, error) {
	k := tenantID + "|" + metric
	r.s.usage[k] += delta
	return r.s.usage[k], nil
}

func (r usageRepo) Get(_ context.Context, tenantID, metric string) (int64, error) {
	return r.s.usage[tenantID+"|"+metric], nil
}

// ---------- 事件发件箱 ----------

type outboxRepo struct{ s *Store }

func (r outboxRepo) Insert(_ context.Context, e *types.OutboxEnvelope) error {
	cp := *e
	r.s.outbox = append(r.s.outbox, &cp)
	return nil
}

func (r outboxRepo) Claim(_ context.Context, limit int) ([]types.OutboxEnvelope, error) {
	out := []types.OutboxEnvelope{}
	now := time.Now()
	for _, e := range r.s.outbox {
		if e.Status != "pending" || e.AvailableAt.After(now) {
			continue
		}
		e.Status = "claimed"
		out = append(out, *e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r outboxRepo) Ack(_ context.Context, id string) error {
	for _, e := range r.s.outbox {
		if e.ID == id {
			e.Status = "done"
			return nil
		}
	}
	return notFound("outbox", id)
}

func (r outboxRepo) Fail(_ context.Context, id, errMsg string) error {
	for _, e := range r.s.outbox {
		if e.ID == id {
			e.Attempts++
			e.LastError = errMsg
			e.Status = "pending"
			e.AvailableAt = time.Now().Add(time.Duration(e.Attempts) * time.Second)
			return nil
		}
	}
	return notFound("outbox", id)
}

// ---------- 审计 ----------

type auditRepo struct{ s *Store }

func (r auditRepo) Append(ctx context.Context, e *types.AuditEntry) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	e.TenantID = tid
	prev := ""
	for i := len(r.s.audit) - 1; i >= 0; i-- {
		if r.s.audit[i].TenantID == tid {
			prev = r.s.audit[i].Hash
			break
		}
	}
	e.PrevHash = prev
	e.Hash = store.AuditHash(prev, e)
	cp := *e
	r.s.audit = append(r.s.audit, &cp)
	return nil
}

func (r auditRepo) Last(ctx context.Context, tenantID string) (*types.AuditEntry, error) {
	for i := len(r.s.audit) - 1; i >= 0; i-- {
		if r.s.audit[i].TenantID == tenantID {
			cp := *r.s.audit[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (r auditRepo) List(ctx context.Context, f store.AuditFilter) ([]types.AuditEntry, int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.AuditEntry{}
	for _, e := range r.s.audit {
		if e.TenantID != tid {
			continue
		}
		if f.Category != "" && e.Category != f.Category {
			continue
		}
		if f.ActorUserID != "" && e.ActorUserID != f.ActorUserID {
			continue
		}
		if f.ShopID != "" && e.ShopID != f.ShopID {
			continue
		}
		if f.Action != "" && e.Action != f.Action {
			continue
		}
		if f.From != nil && e.OccurredAt.Before(*f.From) {
			continue
		}
		if f.To != nil && e.OccurredAt.After(*f.To) {
			continue
		}
		if f.Keyword != "" && !strings.Contains(string(e.Data), f.Keyword) &&
			!strings.Contains(e.ActorName, f.Keyword) && !strings.Contains(e.Action, f.Keyword) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.After(out[j].OccurredAt) })
	total := len(out)
	if f.Offset > 0 && f.Offset < len(out) {
		out = out[f.Offset:]
	} else if f.Offset >= len(out) {
		out = nil
	}
	if f.Limit > 0 && f.Limit < len(out) {
		out = out[:f.Limit]
	}
	return out, total, nil
}

func (r auditRepo) Since(ctx context.Context, tenantID string, from time.Time) ([]types.AuditEntry, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.AuditEntry{}
	for _, e := range r.s.audit {
		if e.TenantID == tid && !e.OccurredAt.Before(from) {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, nil
}

// ---------- 通知 ----------

type notifRepo struct{ s *Store }

func (r notifRepo) Create(_ context.Context, n *types.Notification) error {
	cp := *n
	r.s.notifs = append(r.s.notifs, &cp)
	return nil
}

func (r notifRepo) List(_ context.Context, tenantID, userID string, limit int) ([]types.Notification, error) {
	out := []types.Notification{}
	for _, n := range r.s.notifs {
		if n.TenantID == tenantID && n.UserID == userID {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

func (r notifRepo) MarkRead(_ context.Context, tenantID, id string) error {
	now := time.Now()
	for _, n := range r.s.notifs {
		if n.ID == id && n.TenantID == tenantID {
			n.ReadAt = &now
			return nil
		}
	}
	return notFound("notification", id)
}
