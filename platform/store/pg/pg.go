// Package pg PostgreSQL 存储驱动（生产）。
//
// 多租户策略（L12）：
//  1. 读写 SQL 一律显式带 tenant_id 谓词；
//  2. 写路径额外受 RLS WITH CHECK 保护，app.tenant_id 未设置时零行（fail-closed）；
//  3. 非事务调用（如查询）由驱动自动包一层事务并注入 app.tenant_id。
package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// querier 统一 pgxpool.Pool 与 pgx.Tx 的共同子集。
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store PostgreSQL 存储。
type Store struct {
	pool *pgxpool.Pool
}

// New 建立连接池并探活。
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: 连接串解析失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pg: 数据库不可用: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool 暴露连接池（供迁移器使用）。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Close() error { s.pool.Close(); return nil }

// WithTx 开事务并注入 RLS 租户变量；提交/回滚由驱动负责。
func (s *Store) WithTx(ctx context.Context, fn func(store.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if tid, terr := store.TenantFrom(ctx); terr == nil && tid != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tid); err != nil {
			return err
		}
	}
	if err := fn(txStore{s: s, q: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type txStore struct {
	s *Store
	q querier
}

func (t txStore) Tenants() store.TenantRepo             { return tenantRepo{base{q: t.q, inTx: true}} }
func (t txStore) Users() store.UserRepo                 { return userRepo{base{q: t.q, inTx: true}} }
func (t txStore) Credentials() store.CredentialRepo     { return credRepo{base{q: t.q, inTx: true}} }
func (t txStore) Memberships() store.MembershipRepo     { return memberRepo{base{q: t.q, inTx: true}} }
func (t txStore) Roles() store.RoleRepo                 { return roleRepo{base{q: t.q, inTx: true}} }
func (t txStore) UserRoles() store.UserRoleRepo         { return userRoleRepo{base{q: t.q, inTx: true}} }
func (t txStore) DataGrants() store.DataGrantRepo       { return grantRepo{base{q: t.q, inTx: true}} }
func (t txStore) Sessions() store.SessionRepo           { return sessionRepo{base{q: t.q, inTx: true}} }
func (t txStore) Shops() store.ShopRepo                 { return shopRepo{base{q: t.q, inTx: true}} }
func (t txStore) Subscriptions() store.SubscriptionRepo { return subRepo{base{q: t.q, inTx: true}} }
func (t txStore) Usage() store.UsageRepo                { return usageRepo{base{q: t.q, inTx: true}} }
func (t txStore) Outbox() store.OutboxRepo              { return outboxRepo{base{q: t.q, inTx: true}} }
func (t txStore) Audit() store.AuditRepo                { return auditRepo{base{q: t.q, inTx: true}} }
func (t txStore) Notifications() store.NotificationRepo { return notifRepo{base{q: t.q, inTx: true}} }
func (t txStore) Devices() store.DeviceRepo             { return deviceRepo{base{q: t.q, inTx: true}} }
func (t txStore) Bindings() store.BindingRepo           { return bindingRepo{base{q: t.q, inTx: true}} }
func (t txStore) Environments() store.EnvironmentRepo   { return envRepo{base{q: t.q, inTx: true}} }
func (t txStore) Policies() store.AccessPolicyRepo      { return policyRepo{base{q: t.q, inTx: true}} }
func (t txStore) SecurityEvents() store.SecurityEventRepo {
	return secEventRepo{base{q: t.q, inTx: true}}
}
func (t txStore) Vault() store.VaultRepo        { return vaultRepo{base{q: t.q, inTx: true}} }
func (t txStore) Approvals() store.ApprovalRepo { return approvalRepo{base{q: t.q, inTx: true}} }
func (t txStore) Delegations() store.DelegationRepo {
	return delegationRepo{base{q: t.q, inTx: true}}
}
func (t txStore) EgressBindings() store.EgressBindingRepo {
	return egressRepo{base{q: t.q, inTx: true}}
}
func (t txStore) ApprovalRouteRules() store.ApprovalRouteRuleRepo {
	return routeRuleRepo{base{q: t.q, inTx: true}}
}
func (t txStore) Proxies() store.ProxyRepo { return proxyRepo{base{q: t.q, inTx: true}} }
func (t txStore) Groups() store.GroupRepo  { return groupRepo{base{q: t.q, inTx: true}} }

func (s *Store) Tenants() store.TenantRepo               { return tenantRepo{base{st: s, q: s.pool}} }
func (s *Store) Users() store.UserRepo                   { return userRepo{base{st: s, q: s.pool}} }
func (s *Store) Credentials() store.CredentialRepo       { return credRepo{base{st: s, q: s.pool}} }
func (s *Store) Memberships() store.MembershipRepo       { return memberRepo{base{st: s, q: s.pool}} }
func (s *Store) Roles() store.RoleRepo                   { return roleRepo{base{st: s, q: s.pool}} }
func (s *Store) UserRoles() store.UserRoleRepo           { return userRoleRepo{base{st: s, q: s.pool}} }
func (s *Store) DataGrants() store.DataGrantRepo         { return grantRepo{base{st: s, q: s.pool}} }
func (s *Store) Sessions() store.SessionRepo             { return sessionRepo{base{st: s, q: s.pool}} }
func (s *Store) Shops() store.ShopRepo                   { return shopRepo{base{st: s, q: s.pool}} }
func (s *Store) Subscriptions() store.SubscriptionRepo   { return subRepo{base{st: s, q: s.pool}} }
func (s *Store) Usage() store.UsageRepo                  { return usageRepo{base{st: s, q: s.pool}} }
func (s *Store) Outbox() store.OutboxRepo                { return outboxRepo{base{st: s, q: s.pool}} }
func (s *Store) Audit() store.AuditRepo                  { return auditRepo{base{st: s, q: s.pool}} }
func (s *Store) Notifications() store.NotificationRepo   { return notifRepo{base{st: s, q: s.pool}} }
func (s *Store) Devices() store.DeviceRepo               { return deviceRepo{base{st: s, q: s.pool}} }
func (s *Store) Bindings() store.BindingRepo             { return bindingRepo{base{st: s, q: s.pool}} }
func (s *Store) Environments() store.EnvironmentRepo     { return envRepo{base{st: s, q: s.pool}} }
func (s *Store) Policies() store.AccessPolicyRepo        { return policyRepo{base{st: s, q: s.pool}} }
func (s *Store) SecurityEvents() store.SecurityEventRepo { return secEventRepo{base{st: s, q: s.pool}} }
func (s *Store) Vault() store.VaultRepo                  { return vaultRepo{base{st: s, q: s.pool}} }
func (s *Store) Approvals() store.ApprovalRepo           { return approvalRepo{base{st: s, q: s.pool}} }
func (s *Store) Delegations() store.DelegationRepo       { return delegationRepo{base{st: s, q: s.pool}} }
func (s *Store) EgressBindings() store.EgressBindingRepo { return egressRepo{base{st: s, q: s.pool}} }
func (s *Store) ApprovalRouteRules() store.ApprovalRouteRuleRepo {
	return routeRuleRepo{base{st: s, q: s.pool}}
}
func (s *Store) Proxies() store.ProxyRepo { return proxyRepo{base{st: s, q: s.pool}} }
func (s *Store) Groups() store.GroupRepo  { return groupRepo{base{st: s, q: s.pool}} }

// base 仓库公共能力：事务复用 / 非事务自动包事务。
type base struct {
	st   *Store
	q    querier
	inTx bool
}

// run 在正确的执行上下文（事务内）执行 SQL。
func (b base) run(ctx context.Context, fn func(q querier) error) error {
	if b.inTx {
		return fn(b.q)
	}
	return b.st.WithTx(ctx, func(tx store.Tx) error {
		return fn(tx.(txStore).q)
	})
}

func (b base) tenant(ctx context.Context) (string, error) { return store.TenantFrom(ctx) }

// ---------- 租户 ----------

type tenantRepo struct{ base }

func (r tenantRepo) Create(ctx context.Context, t *types.Tenant) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO tenants(id,name,slug,status,plan_code,schema_name,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7)`,
			t.ID, t.Name, t.Slug, string(t.Status), t.PlanCode, nullStr(t.SchemaName), t.CreatedAt)
		return wrapErr(err)
	})
}

func (r tenantRepo) Get(ctx context.Context, id string) (*types.Tenant, error) {
	var out types.Tenant
	var status string
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,name,slug,status,plan_code,COALESCE(schema_name,''),created_at
			FROM tenants WHERE id=$1`, id).
			Scan(&out.ID, &out.Name, &out.Slug, &status, &out.PlanCode, &out.SchemaName, &out.CreatedAt)
	})
	out.Status = types.TenantStatus(status)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &out, nil
}

func (r tenantRepo) GetBySlug(ctx context.Context, slug string) (*types.Tenant, error) {
	var out types.Tenant
	var status string
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,name,slug,status,plan_code,COALESCE(schema_name,''),created_at
			FROM tenants WHERE slug=$1`, slug).
			Scan(&out.ID, &out.Name, &out.Slug, &status, &out.PlanCode, &out.SchemaName, &out.CreatedAt)
	})
	out.Status = types.TenantStatus(status)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &out, nil
}

func (r tenantRepo) List(ctx context.Context, limit, offset int) ([]types.Tenant, int, error) {
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	out := []types.Tenant{}
	total := 0
	err := r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM tenants`).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT id,name,slug,status,plan_code,COALESCE(schema_name,''),created_at
			FROM tenants ORDER BY created_at LIMIT $1 OFFSET $2`, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t types.Tenant
			var status string
			if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &status, &t.PlanCode, &t.SchemaName, &t.CreatedAt); err != nil {
				return err
			}
			t.Status = types.TenantStatus(status)
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

// ---------- 用户 ----------

type userRepo struct{ base }

func (r userRepo) Create(ctx context.Context, u *types.User) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO users(id,username,email,phone,display_name,status,mfa_enabled,totp_secret,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			u.ID, u.Username, nullStr(u.Email), nullStr(u.Phone), nullStr(u.DisplayName),
			u.Status, u.MFAEnabled, nullStr(u.TOTPSecret), u.CreatedAt)
		return wrapErr(err)
	})
}

func (r userRepo) GetByID(ctx context.Context, id string) (*types.User, error) {
	out := &types.User{}
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,username,COALESCE(email,''),COALESCE(phone,''),COALESCE(display_name,''),
			status,mfa_enabled,COALESCE(totp_secret,''),created_at,last_login_at FROM users WHERE id=$1`, id).
			Scan(&out.ID, &out.Username, &out.Email, &out.Phone, &out.DisplayName,
				&out.Status, &out.MFAEnabled, &out.TOTPSecret, &out.CreatedAt, &out.LastLoginAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r userRepo) GetByUsername(ctx context.Context, username string) (*types.User, error) {
	out := &types.User{}
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,username,COALESCE(email,''),COALESCE(phone,''),COALESCE(display_name,''),
			status,mfa_enabled,COALESCE(totp_secret,''),created_at,last_login_at FROM users WHERE username=$1`, username).
			Scan(&out.ID, &out.Username, &out.Email, &out.Phone, &out.DisplayName,
				&out.Status, &out.MFAEnabled, &out.TOTPSecret, &out.CreatedAt, &out.LastLoginAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r userRepo) TouchLogin(ctx context.Context, id string, at time.Time) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE users SET last_login_at=$2 WHERE id=$1`, id, at)
		return wrapErr(err)
	})
}

type credRepo struct{ base }

func (r credRepo) Get(ctx context.Context, userID string) (*types.Credential, error) {
	out := &types.Credential{}
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT user_id,password_enc,algo,key_version,updated_at
			FROM credentials WHERE user_id=$1`, userID).
			Scan(&out.UserID, &out.PasswordEnc, &out.Algo, &out.KeyVersion, &out.UpdatedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r credRepo) Upsert(ctx context.Context, c *types.Credential) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO credentials(user_id,password_enc,algo,key_version,updated_at)
			VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(user_id) DO UPDATE SET password_enc=EXCLUDED.password_enc,
			algo=EXCLUDED.algo,key_version=EXCLUDED.key_version,updated_at=EXCLUDED.updated_at`,
			c.UserID, c.PasswordEnc, c.Algo, c.KeyVersion, c.UpdatedAt)
		return wrapErr(err)
	})
}

// ---------- 成员 ----------

type memberRepo struct{ base }

func (r memberRepo) Create(ctx context.Context, m *types.Membership) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO memberships(id,tenant_id,user_id,status,invited_by,joined_at)
			VALUES($1,$2,$3,$4,$5,$6)`, m.ID, m.TenantID, m.UserID, m.Status, nullStr(m.InvitedBy), m.JoinedAt)
		return wrapErr(err)
	})
}

func (r memberRepo) Get(ctx context.Context, tenantID, userID string) (*types.Membership, error) {
	out := &types.Membership{}
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,tenant_id,user_id,status,COALESCE(invited_by,''),joined_at
			FROM memberships WHERE tenant_id=$1 AND user_id=$2`, tenantID, userID).
			Scan(&out.ID, &out.TenantID, &out.UserID, &out.Status, &out.InvitedBy, &out.JoinedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r memberRepo) ListByTenant(ctx context.Context, tenantID string) ([]types.Membership, error) {
	out := []types.Membership{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,tenant_id,user_id,status,COALESCE(invited_by,''),joined_at
			FROM memberships WHERE tenant_id=$1 ORDER BY joined_at DESC`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m types.Membership
			if err := rows.Scan(&m.ID, &m.TenantID, &m.UserID, &m.Status, &m.InvitedBy, &m.JoinedAt); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// ListByUser 登录阶段按用户查所属团队：此时租户未知，走 memberships 的
// login_lookup 策略（仅允许 SELECT 且必须带 user_id 谓词）。
func (r memberRepo) ListByUser(ctx context.Context, userID string) ([]types.Membership, error) {
	out := []types.Membership{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,tenant_id,user_id,status,COALESCE(invited_by,''),joined_at
			FROM memberships WHERE user_id=$1 ORDER BY joined_at DESC`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m types.Membership
			if err := rows.Scan(&m.ID, &m.TenantID, &m.UserID, &m.Status, &m.InvitedBy, &m.JoinedAt); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// ---------- 角色 ----------

type roleRepo struct{ base }

func (r roleRepo) Create(ctx context.Context, role *types.Role) error {
	return r.run(ctx, func(q querier) error {
		if _, err := q.Exec(ctx, `INSERT INTO roles(id,tenant_id,code,name,builtin,mask_fields,deny_elements,data_scope,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			role.ID, nullStr(role.TenantID), role.Code, role.Name, role.Builtin,
			strArr(role.MaskFields), strArr(role.DenyElement), role.DataScope, role.CreatedAt); err != nil {
			return wrapErr(err)
		}
		for _, p := range role.Permissions {
			if _, err := q.Exec(ctx, `INSERT INTO role_permissions(role_id,permission_key) VALUES($1,$2)
				ON CONFLICT DO NOTHING`, role.ID, p); err != nil {
				return wrapErr(err)
			}
		}
		return nil
	})
}

func (r roleRepo) Get(ctx context.Context, id string) (*types.Role, error) {
	role := &types.Role{}
	var tenantID *string
	err := r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT id,tenant_id,code,name,builtin,mask_fields,deny_elements,data_scope,created_at
			FROM roles WHERE id=$1`, id).Scan(&role.ID, &tenantID, &role.Code, &role.Name, &role.Builtin,
			&role.MaskFields, &role.DenyElement, &role.DataScope, &role.CreatedAt); err != nil {
			return err
		}
		return loadPerms(ctx, q, role.ID, role)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	if tenantID != nil {
		role.TenantID = *tenantID
	}
	return role, nil
}

func (r roleRepo) GetByCode(ctx context.Context, tenantID, code string) (*types.Role, error) {
	role := &types.Role{}
	var tid *string
	err := r.run(ctx, func(q querier) error {
		// 命中顺序：先租户自定义角色，再系统内置角色（tenant_id IS NULL）。
		//
		// 修正（PG 实跑暴露）：原先写成 COALESCE(tenant_id,'~')=COALESCE($1,'~')，
		// 传进租户 ID 时永远匹配不到系统级角色 ⇒ PG 上**任何授权都会报「角色不存在」**，
		// 而 mem 实现允许回退到系统级角色，所以所有单元测试都是绿的——典型的
		// 「测试替身与真实依赖语义不一致」导致的生产不可用。语义以 List 为准。
		if err := q.QueryRow(ctx, `SELECT id,tenant_id,code,name,builtin,mask_fields,deny_elements,data_scope,created_at
			FROM roles WHERE code=$1 AND (tenant_id IS NULL OR tenant_id=$2)
			ORDER BY CASE WHEN tenant_id IS NULL THEN 1 ELSE 0 END ASC LIMIT 1`, code, nullStr(tenantID)).
			Scan(&role.ID, &tid, &role.Code, &role.Name, &role.Builtin,
				&role.MaskFields, &role.DenyElement, &role.DataScope, &role.CreatedAt); err != nil {
			return err
		}
		return loadPerms(ctx, q, role.ID, role)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	if tid != nil {
		role.TenantID = *tid
	}
	return role, nil
}

func (r roleRepo) List(ctx context.Context, tenantID string) ([]types.Role, error) {
	out := []types.Role{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,tenant_id,code,name,builtin,mask_fields,deny_elements,data_scope,created_at
			FROM roles WHERE tenant_id IS NULL OR tenant_id=$1 ORDER BY code`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var role types.Role
			var tid *string
			if err := rows.Scan(&role.ID, &tid, &role.Code, &role.Name, &role.Builtin,
				&role.MaskFields, &role.DenyElement, &role.DataScope, &role.CreatedAt); err != nil {
				return err
			}
			if tid != nil {
				role.TenantID = *tid
			}
			out = append(out, role)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if err := loadPerms(ctx, q, out[i].ID, &out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return out, wrapErr(err)
}

// AddPermissions 并集补齐：靠 (role_id, permission_key) 主键冲突忽略，
// 已有的行原样不动，返回真正新增的条数（升级同步要靠这个数判断是否真的补了东西）。
func (r roleRepo) AddPermissions(ctx context.Context, id string, perms []string) (int, error) {
	var added int
	err := r.run(ctx, func(q querier) error {
		for _, p := range perms {
			if p == "" {
				continue
			}
			tag, err := q.Exec(ctx, `INSERT INTO role_permissions(role_id,permission_key)
				VALUES($1,$2) ON CONFLICT DO NOTHING`, id, p)
			if err != nil {
				return err
			}
			added += int(tag.RowsAffected())
		}
		return nil
	})
	return added, wrapErr(err)
}

func (r roleRepo) SetPermissions(ctx context.Context, id string, perms []string) error {
	return r.run(ctx, func(q querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM role_permissions WHERE role_id=$1`, id); err != nil {
			return wrapErr(err)
		}
		for _, p := range perms {
			if _, err := q.Exec(ctx, `INSERT INTO role_permissions(role_id,permission_key) VALUES($1,$2)
				ON CONFLICT DO NOTHING`, id, p); err != nil {
				return wrapErr(err)
			}
		}
		return nil
	})
}

func loadPerms(ctx context.Context, q querier, roleID string, role *types.Role) error {
	rows, err := q.Query(ctx, `SELECT permission_key FROM role_permissions WHERE role_id=$1`, roleID)
	if err != nil {
		return err
	}
	defer rows.Close()
	role.Permissions = []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return err
		}
		role.Permissions = append(role.Permissions, p)
	}
	return rows.Err()
}

type userRoleRepo struct{ base }

func (r userRoleRepo) Assign(ctx context.Context, ur *types.UserRole) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO user_roles(id,tenant_id,user_id,role_id,granted_by,granted_at)
			VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(tenant_id,user_id,role_id) DO NOTHING`,
			ur.ID, ur.TenantID, ur.UserID, ur.RoleID, nullStr(ur.GrantedBy), ur.GrantedAt)
		return wrapErr(err)
	})
}

func (r userRoleRepo) Revoke(ctx context.Context, tenantID, userID, roleID string) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM user_roles WHERE tenant_id=$1 AND user_id=$2 AND role_id=$3`,
			tenantID, userID, roleID)
		return wrapErr(err)
	})
}

func (r userRoleRepo) ListByUser(ctx context.Context, tenantID, userID string) ([]types.UserRole, error) {
	out := []types.UserRole{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,tenant_id,user_id,role_id,COALESCE(granted_by,''),granted_at
			FROM user_roles WHERE tenant_id=$1 AND user_id=$2`, tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ur types.UserRole
			if err := rows.Scan(&ur.ID, &ur.TenantID, &ur.UserID, &ur.RoleID, &ur.GrantedBy, &ur.GrantedAt); err != nil {
				return err
			}
			out = append(out, ur)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

func (r userRoleRepo) ListByTenant(ctx context.Context, tenantID string) ([]types.UserRole, error) {
	out := []types.UserRole{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,tenant_id,user_id,role_id,COALESCE(granted_by,''),granted_at
			FROM user_roles WHERE tenant_id=$1`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ur types.UserRole
			if err := rows.Scan(&ur.ID, &ur.TenantID, &ur.UserID, &ur.RoleID, &ur.GrantedBy, &ur.GrantedAt); err != nil {
				return err
			}
			out = append(out, ur)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

type grantRepo struct{ base }

func (r grantRepo) Create(ctx context.Context, g *types.DataGrant) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO data_grants(id,tenant_id,user_id,resource_type,resource_id,scope,granted_by,expires_at,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			g.ID, g.TenantID, g.UserID, g.ResourceType, g.ResourceID, nullStr(g.Scope),
			nullStr(g.GrantedBy), g.ExpiresAt, g.CreatedAt)
		return wrapErr(err)
	})
}

// ListByUser 返回全部授权记录（含已过期），与 mem 实现口径一致。
// 原因见 mem 同名方法：过滤过期行会让身份层的「临时授权到期失效」判定失去依据。
func (r grantRepo) ListByUser(ctx context.Context, tenantID, userID, resourceType string) ([]types.DataGrant, error) {
	out := []types.DataGrant{}
	err := r.run(ctx, func(q querier) error {
		sql := `SELECT id,tenant_id,user_id,resource_type,resource_id,COALESCE(scope,''),COALESCE(granted_by,''),expires_at,created_at
			FROM data_grants WHERE tenant_id=$1 AND user_id=$2`
		args := []any{tenantID, userID}
		if resourceType != "" {
			sql += ` AND resource_type=$3`
			args = append(args, resourceType)
		}
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g types.DataGrant
			if err := rows.Scan(&g.ID, &g.TenantID, &g.UserID, &g.ResourceType, &g.ResourceID,
				&g.Scope, &g.GrantedBy, &g.ExpiresAt, &g.CreatedAt); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// DeleteExpired 用 RETURNING 一次拿回被删条目：
// 先 SELECT 再 DELETE 在并发下会读到已经不存在的行，也会让回收与发通知之间
// DeleteByScope 删除该用户在某 scope 下的全部授权并返回被删条目
// （代理机构委托撤销/到期时整批回收；返回条目供事件 Data 留痕）。
func (r grantRepo) DeleteByScope(ctx context.Context, tenantID, userID, scope string) ([]types.DataGrant, error) {
	out := []types.DataGrant{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `DELETE FROM data_grants
			WHERE tenant_id=$1 AND user_id=$2 AND scope=$3
			RETURNING id,tenant_id,user_id,resource_type,COALESCE(resource_id,''),
				COALESCE(scope,''),COALESCE(granted_by,''),expires_at,created_at`,
			tenantID, userID, scope)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g types.DataGrant
			if err := rows.Scan(&g.ID, &g.TenantID, &g.UserID, &g.ResourceType, &g.ResourceID,
				&g.Scope, &g.GrantedBy, &g.ExpiresAt, &g.CreatedAt); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// 出现窗口（清了行但通知没发出去，等于静默收权）。
func (r grantRepo) DeleteExpired(ctx context.Context, tenantID string, now time.Time) ([]types.DataGrant, error) {
	out := []types.DataGrant{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `DELETE FROM data_grants
			WHERE tenant_id=$1 AND expires_at IS NOT NULL AND expires_at <= $2
			RETURNING id,tenant_id,user_id,resource_type,COALESCE(resource_id,''),
				COALESCE(scope,''),COALESCE(granted_by,''),expires_at,created_at`,
			tenantID, now.UTC())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g types.DataGrant
			if err := rows.Scan(&g.ID, &g.TenantID, &g.UserID, &g.ResourceType, &g.ResourceID,
				&g.Scope, &g.GrantedBy, &g.ExpiresAt, &g.CreatedAt); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// ---------- 会话 ----------

type sessionRepo struct{ base }

func (r sessionRepo) Create(ctx context.Context, s *types.Session) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO sessions(id,user_id,tenant_id,ip,user_agent,terminal_fp,client_ver,
			created_at,last_seen_at,expires_at,revoked_at,login_result,blocked_cause)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			s.ID, s.UserID, s.TenantID, nullStr(s.IP), nullStr(s.UserAgent), nullStr(s.TerminalFP),
			nullStr(s.ClientVer), s.CreatedAt, s.LastSeenAt, s.ExpiresAt, s.RevokedAt,
			s.LoginResult, nullStr(s.BlockedCause))
		return wrapErr(err)
	})
}

func (r sessionRepo) Get(ctx context.Context, id string) (*types.Session, error) {
	out := &types.Session{}
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,user_id,tenant_id,COALESCE(ip,''),COALESCE(user_agent,''),
			COALESCE(terminal_fp,''),COALESCE(client_ver,''),created_at,last_seen_at,expires_at,revoked_at,
			login_result,COALESCE(blocked_cause,'') FROM sessions WHERE id=$1`, id).
			Scan(&out.ID, &out.UserID, &out.TenantID, &out.IP, &out.UserAgent, &out.TerminalFP,
				&out.ClientVer, &out.CreatedAt, &out.LastSeenAt, &out.ExpiresAt, &out.RevokedAt,
				&out.LoginResult, &out.BlockedCause)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r sessionRepo) Revoke(ctx context.Context, id string) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE id=$1`, id)
		return wrapErr(err)
	})
}

func (r sessionRepo) ListByUser(ctx context.Context, userID string) ([]types.Session, error) {
	out := []types.Session{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,user_id,tenant_id,COALESCE(ip,''),COALESCE(user_agent,''),
			COALESCE(terminal_fp,''),COALESCE(client_ver,''),created_at,last_seen_at,expires_at,revoked_at,
			login_result,COALESCE(blocked_cause,'') FROM sessions WHERE user_id=$1`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s types.Session
			if err := rows.Scan(&s.ID, &s.UserID, &s.TenantID, &s.IP, &s.UserAgent, &s.TerminalFP,
				&s.ClientVer, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &s.RevokedAt,
				&s.LoginResult, &s.BlockedCause); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// ---------- 店铺 ----------

type shopRepo struct{ base }

func (r shopRepo) Create(ctx context.Context, s *types.Shop) error {
	return r.run(ctx, func(q querier) error {
		// 0012：code（自定义编号）与 group_id（显式分组）一并落库；group_name 仍双写。
		_, err := q.Exec(ctx, `INSERT INTO shops(id,tenant_id,name,platform,site,status,group_name,group_id,code,tags,device_id,owner_id,profit,cost,balance,created_at,updated_at,deleted_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			s.ID, s.TenantID, s.Name, nullStr(s.Platform), nullStr(s.Site), s.Status,
			nullStr(s.Group), nullStr(s.GroupID), nullStr(s.Code),
			strArr(s.Tags), nullStr(s.DeviceID), s.OwnerID,
			s.Profit, s.Cost, s.Balance, s.CreatedAt, s.UpdatedAt, s.DeletedAt)
		return wrapErr(err)
	})
}

func (r shopRepo) Get(ctx context.Context, id string) (*types.Shop, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.Shop{}
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,tenant_id,name,COALESCE(platform,''),COALESCE(site,''),status,
			COALESCE(group_name,''),COALESCE(group_id,''),COALESCE(code,''),tags,COALESCE(device_id,''),owner_id,
			profit,cost,balance,created_at,updated_at,deleted_at FROM shops WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&out.ID, &out.TenantID, &out.Name, &out.Platform, &out.Site, &out.Status, &out.Group,
				&out.GroupID, &out.Code, &out.Tags, &out.DeviceID, &out.OwnerID,
				&out.Profit, &out.Cost, &out.Balance, &out.CreatedAt, &out.UpdatedAt, &out.DeletedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r shopRepo) List(ctx context.Context, f store.ShopFilter) ([]types.Shop, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.Shop{}
	total := 0
	where := `WHERE tenant_id=$1`
	args := []any{tid}
	if f.Deleted {
		where += ` AND deleted_at IS NOT NULL`
	} else if !f.IncludeDeleted {
		where += ` AND deleted_at IS NULL`
	}
	if f.Scope == "own" {
		args = append(args, f.OwnerID)
		// 委托分支必须与 owner 分支同处一对括号内：AND 优先级高于 OR，
		// 一旦写成 `... AND owner_id=$2 OR (委托...)`，委托行会绕开全部其它谓词
		// （包括下面要加的 group_id），表现为「按分组筛选却筛出了别的组的店铺」。
		own := `owner_id=$` + fmt.Sprint(len(args))
		if f.DelegatedFor != "" && !f.Deleted {
			own += ` OR (deleted_at IS NULL AND id IN
				(SELECT resource_id FROM data_grants
				 WHERE tenant_id=$1 AND user_id=$` + fmt.Sprint(len(args)) + ` AND resource_type='shop'
				   AND (expires_at IS NULL OR expires_at > now())))`
		}
		where += ` AND (` + own + `)`
	}
	if f.GroupID != "" {
		args = append(args, f.GroupID)
		where += ` AND group_id=$` + fmt.Sprint(len(args))
	}
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM shops `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT id,tenant_id,name,COALESCE(platform,''),COALESCE(site,''),status,
			COALESCE(group_name,''),COALESCE(group_id,''),COALESCE(code,''),tags,COALESCE(device_id,''),owner_id,
			profit,cost,balance,created_at,updated_at,deleted_at FROM shops `+where+`
			ORDER BY created_at DESC LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2),
			append(args, f.Limit, f.Offset)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s types.Shop
			if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &s.Platform, &s.Site, &s.Status, &s.Group,
				&s.GroupID, &s.Code, &s.Tags, &s.DeviceID, &s.OwnerID,
				&s.Profit, &s.Cost, &s.Balance, &s.CreatedAt, &s.UpdatedAt, &s.DeletedAt); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

func (r shopRepo) Update(ctx context.Context, s *types.Shop) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE shops SET name=$3,platform=$4,site=$5,status=$6,group_name=$7,group_id=$8,code=$9,tags=$10,
			device_id=$11,owner_id=$12,profit=$13,cost=$14,balance=$15,updated_at=$16,deleted_at=$17
			WHERE id=$1 AND tenant_id=$2`,
			s.ID, tid, s.Name, nullStr(s.Platform), nullStr(s.Site), s.Status, nullStr(s.Group),
			nullStr(s.GroupID), nullStr(s.Code), strArr(s.Tags),
			nullStr(s.DeviceID), s.OwnerID, s.Profit, s.Cost, s.Balance, s.UpdatedAt, s.DeletedAt)
		return err
	}))
}

func (r shopRepo) Count(ctx context.Context) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM shops WHERE tenant_id=$1 AND deleted_at IS NULL`, tid).Scan(&n)
	})
	return n, wrapErr(err)
}

func (r shopRepo) Delete(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		// 只允许物理删除已软删的数据（未进回收站的行 deleted_at IS NULL，删除 0 行）
		tag, err := q.Exec(ctx, `DELETE FROM shops WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NOT NULL`, id, tid)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apierr.NotFound("店铺不存在或尚未进入回收站（禁止物理删除在营店铺）")
		}
		return nil
	}))
}

// ---------- 订阅与计量 ----------

type subRepo struct{ base }

func (r subRepo) Create(ctx context.Context, s *types.Subscription) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO subscriptions(id,tenant_id,plan_code,status,started_at,expires_at)
			VALUES($1,$2,$3,$4,$5,$6)`, s.ID, s.TenantID, s.PlanCode, s.Status, s.StartedAt, s.ExpiresAt)
		return wrapErr(err)
	})
}

func (r subRepo) GetActive(ctx context.Context, tenantID string) (*types.Subscription, error) {
	out := &types.Subscription{}
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,tenant_id,plan_code,status,started_at,expires_at
			FROM subscriptions WHERE tenant_id=$1 AND status='active' ORDER BY started_at DESC LIMIT 1`, tenantID).
			Scan(&out.ID, &out.TenantID, &out.PlanCode, &out.Status, &out.StartedAt, &out.ExpiresAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

type usageRepo struct{ base }

func (r usageRepo) Incr(ctx context.Context, tenantID, metric string, delta int64) (int64, error) {
	var v int64
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `INSERT INTO usage_counters(tenant_id,metric,value,updated_at)
			VALUES($1,$2,$3,now()) ON CONFLICT(tenant_id,metric)
			DO UPDATE SET value=usage_counters.value+EXCLUDED.value, updated_at=now() RETURNING value`,
			tenantID, metric, delta).Scan(&v)
	})
	return v, wrapErr(err)
}

func (r usageRepo) Get(ctx context.Context, tenantID, metric string) (int64, error) {
	var v int64
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COALESCE(value,0) FROM usage_counters WHERE tenant_id=$1 AND metric=$2`,
			tenantID, metric).Scan(&v)
	})
	if isNoRows(err) {
		return 0, nil
	}
	return v, wrapErr(err)
}

// ---------- 发件箱 ----------

type outboxRepo struct{ base }

func (r outboxRepo) Insert(ctx context.Context, e *types.OutboxEnvelope) error {
	return r.run(ctx, func(q querier) error {
		payload := e.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		_, err := q.Exec(ctx, `INSERT INTO event_outbox(id,tenant_id,type,payload,status,attempts,available_at,last_error,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			e.ID, e.TenantID, e.Type, payload, e.Status, e.Attempts, e.AvailableAt, nullStr(e.LastError), e.CreatedAt)
		return wrapErr(err)
	})
}

func (r outboxRepo) Claim(ctx context.Context, limit int) ([]types.OutboxEnvelope, error) {
	out := []types.OutboxEnvelope{}
	err := r.run(ctx, func(q querier) error {
		// SKIP LOCKED：多实例并发派发不互相阻塞
		rows, err := q.Query(ctx, `UPDATE event_outbox SET status='claimed'
			WHERE id IN (SELECT id FROM event_outbox WHERE status='pending' AND available_at <= now()
			  ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED)
			RETURNING id,tenant_id,type,payload,status,attempts,available_at,COALESCE(last_error,''),created_at`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e types.OutboxEnvelope
			if err := rows.Scan(&e.ID, &e.TenantID, &e.Type, &e.Payload, &e.Status, &e.Attempts,
				&e.AvailableAt, &e.LastError, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

func (r outboxRepo) Ack(ctx context.Context, id string) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE event_outbox SET status='done' WHERE id=$1`, id)
		return wrapErr(err)
	})
}

func (r outboxRepo) Fail(ctx context.Context, id, errMsg string) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE event_outbox SET status='pending', attempts=attempts+1,
			last_error=$2, available_at=now() + make_interval(secs => attempts::double precision) WHERE id=$1`, id, errMsg)
		return wrapErr(err)
	})
}

// Consumed 接收侧投递幂等（event_consumptions，migration 0011）。
// 显式带 tenant_id 谓词：本表只 ENABLE 不 FORCE RLS，表主连接不受策略约束
// （与 egress_binding_history 同口径）——隔离靠仓储注入租户，RLS 只是最后一道。
func (r outboxRepo) Consumed(ctx context.Context, eventID, handler string) (bool, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM event_consumptions WHERE event_id=$1 AND handler=$2 AND tenant_id=$3)`,
			eventID, handler, tid).Scan(&ok)
	})
	return ok, wrapErr(err)
}

// MarkConsumed 登记消费完成（ON CONFLICT DO NOTHING：重投/重试下幂等）。
// tenant_id 取自 ctx（RLS WITH CHECK 校验），与业务写同事务提交时天然一致。
func (r outboxRepo) MarkConsumed(ctx context.Context, eventID, handler string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO event_consumptions(event_id,handler,tenant_id)
			VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, eventID, handler, tid)
		return err
	}))
}

// ---------- 审计 ----------

type auditRepo struct{ base }

func (r auditRepo) Append(ctx context.Context, e *types.AuditEntry) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	e.TenantID = tid
	var prev string
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COALESCE(hash,'') FROM audit_log WHERE tenant_id=$1
			ORDER BY occurred_at DESC, id DESC LIMIT 1`, tid).Scan(&prev); err != nil && !isNoRows(err) {
			return err
		}
		e.PrevHash = prev
		e.Hash = store.AuditHash(prev, e)
		data := e.Data
		if len(data) == 0 {
			data = json.RawMessage("{}")
		}
		_, err := q.Exec(ctx, `INSERT INTO audit_log(id,tenant_id,category,actor_user_id,actor_name,shop_id,device_id,
			terminal_ip,terminal_fp,action,result,url,occurred_at,data,prev_hash,hash)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			e.ID, e.TenantID, string(e.Category), nullStr(e.ActorUserID), nullStr(e.ActorName),
			nullStr(e.ShopID), nullStr(e.DeviceID), nullStr(e.TerminalIP), nullStr(e.TerminalFP),
			e.Action, e.Result, nullStr(e.URL), e.OccurredAt, data, e.PrevHash, e.Hash)
		return err
	})
	return wrapErr(err)
}

func (r auditRepo) Last(ctx context.Context, tenantID string) (*types.AuditEntry, error) {
	out := &types.AuditEntry{}
	var category string
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,tenant_id,category,COALESCE(actor_user_id,''),COALESCE(actor_name,''),
			COALESCE(shop_id,''),COALESCE(device_id,''),COALESCE(terminal_ip,''),COALESCE(terminal_fp,''),
			action,result,COALESCE(url,''),occurred_at,COALESCE(data,'{}'::jsonb),prev_hash,hash
			FROM audit_log WHERE tenant_id=$1 ORDER BY occurred_at DESC, id DESC LIMIT 1`, tenantID).
			Scan(&out.ID, &out.TenantID, &category, &out.ActorUserID, &out.ActorName, &out.ShopID,
				&out.DeviceID, &out.TerminalIP, &out.TerminalFP, &out.Action, &out.Result, &out.URL,
				&out.OccurredAt, &out.Data, &out.PrevHash, &out.Hash)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	out.Category = types.AuditCategory(category)
	return out, nil
}

func (r auditRepo) List(ctx context.Context, f store.AuditFilter) ([]types.AuditEntry, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	where := `WHERE tenant_id=$1`
	args := []any{tid}
	idx := 2
	if f.Category != "" {
		where += fmt.Sprintf(` AND category=$%d`, idx)
		args, idx = append(args, string(f.Category)), idx+1
	}
	if f.ActorUserID != "" {
		where += fmt.Sprintf(` AND actor_user_id=$%d`, idx)
		args, idx = append(args, f.ActorUserID), idx+1
	}
	if f.ShopID != "" {
		where += fmt.Sprintf(` AND shop_id=$%d`, idx)
		args, idx = append(args, f.ShopID), idx+1
	}
	if f.Action != "" {
		where += fmt.Sprintf(` AND action=$%d`, idx)
		args, idx = append(args, f.Action), idx+1
	}
	if f.From != nil {
		where += fmt.Sprintf(` AND occurred_at >= $%d`, idx)
		args, idx = append(args, *f.From), idx+1
	}
	if f.To != nil {
		where += fmt.Sprintf(` AND occurred_at <= $%d`, idx)
		args, idx = append(args, *f.To), idx+1
	}
	if f.Keyword != "" {
		where += fmt.Sprintf(` AND (action ILIKE $%d OR actor_name ILIKE $%d OR data::text ILIKE $%d)`, idx, idx, idx)
		args, idx = append(args, "%"+f.Keyword+"%"), idx+1
	}
	out := []types.AuditEntry{}
	total := 0
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT id,tenant_id,category,COALESCE(actor_user_id,''),COALESCE(actor_name,''),
			COALESCE(shop_id,''),COALESCE(device_id,''),COALESCE(terminal_ip,''),COALESCE(terminal_fp,''),
			action,result,COALESCE(url,''),occurred_at,COALESCE(data,'{}'::jsonb),prev_hash,hash
			FROM audit_log `+where+` ORDER BY occurred_at DESC
			LIMIT `+fmt.Sprint(limit)+` OFFSET `+fmt.Sprint(f.Offset), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e types.AuditEntry
			var category string
			if err := rows.Scan(&e.ID, &e.TenantID, &category, &e.ActorUserID, &e.ActorName, &e.ShopID,
				&e.DeviceID, &e.TerminalIP, &e.TerminalFP, &e.Action, &e.Result, &e.URL,
				&e.OccurredAt, &e.Data, &e.PrevHash, &e.Hash); err != nil {
				return err
			}
			e.Category = types.AuditCategory(category)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

func (r auditRepo) Since(ctx context.Context, tenantID string, from time.Time) ([]types.AuditEntry, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.AuditEntry{}
	err = r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,tenant_id,category,COALESCE(actor_user_id,''),COALESCE(actor_name,''),
			COALESCE(shop_id,''),COALESCE(device_id,''),COALESCE(terminal_ip,''),COALESCE(terminal_fp,''),
			action,result,COALESCE(url,''),occurred_at,COALESCE(data,'{}'::jsonb),prev_hash,hash
			FROM audit_log WHERE tenant_id=$1 AND occurred_at >= $2 ORDER BY occurred_at ASC, id ASC`, tid, from)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e types.AuditEntry
			var category string
			if err := rows.Scan(&e.ID, &e.TenantID, &category, &e.ActorUserID, &e.ActorName, &e.ShopID,
				&e.DeviceID, &e.TerminalIP, &e.TerminalFP, &e.Action, &e.Result, &e.URL,
				&e.OccurredAt, &e.Data, &e.PrevHash, &e.Hash); err != nil {
				return err
			}
			e.Category = types.AuditCategory(category)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// ---------- 通知 ----------

type notifRepo struct{ base }

func (r notifRepo) Create(ctx context.Context, n *types.Notification) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO notifications(id,tenant_id,user_id,category,title,body,event_type,channels,created_at,read_at,
			delivery_status,delivery_note,attempts,delivered_channels,last_delivery_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			n.ID, n.TenantID, n.UserID, n.Category, n.Title, n.Body, nullStr(n.EventType), strArr(n.Channels), n.CreatedAt, n.ReadAt,
			nullStatus(n.DeliveryStatus), nullStr(n.DeliveryNote), n.Attempts, strArr(n.DeliveredChannels), n.LastDeliveryAt)
		return wrapErr(err)
	})
}

// scanNotif 通知行公共扫描（List 与 ListPendingDelivery 共用同一套列顺序）。
func scanNotif(rows pgx.Rows) ([]types.Notification, error) {
	out := []types.Notification{}
	for rows.Next() {
		var n types.Notification
		if err := rows.Scan(&n.ID, &n.TenantID, &n.UserID, &n.Category, &n.Title, &n.Body,
			&n.EventType, &n.Channels, &n.CreatedAt, &n.ReadAt,
			&n.DeliveryStatus, &n.DeliveryNote, &n.Attempts, &n.DeliveredChannels, &n.LastDeliveryAt); err != nil {
			return out, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

const notifColumns = `id,tenant_id,user_id,category,title,body,COALESCE(event_type,''),
	channels,created_at,read_at,COALESCE(delivery_status,'none'),COALESCE(delivery_note,''),
	attempts,delivered_channels,last_delivery_at`

func (r notifRepo) List(ctx context.Context, tenantID, userID string, limit int) ([]types.Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	out := []types.Notification{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+notifColumns+` FROM notifications WHERE tenant_id=$1 AND user_id=$2
			ORDER BY created_at DESC LIMIT $3`, tenantID, userID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		var serr error
		out, serr = scanNotif(rows)
		return serr
	})
	return out, wrapErr(err)
}

func (r notifRepo) MarkRead(ctx context.Context, tenantID, id string) error {
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE notifications SET read_at=now() WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		return wrapErr(err)
	})
}

// UpdateDelivery 回写投递结果。
//
// RowsAffected=0 必须当成「没找到」来处理，而不是静默成功：调用方（重投任务）
// 据此判断这条通知是否已被删除/换租户，否则会对着空气反复投递。
func (r notifRepo) UpdateDelivery(ctx context.Context, n *types.Notification) error {
	return r.run(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `UPDATE notifications SET delivery_status=$1,delivery_note=$2,attempts=$3,
			delivered_channels=$4,last_delivery_at=$5 WHERE tenant_id=$6 AND id=$7`,
			nullStatus(n.DeliveryStatus), nullStr(n.DeliveryNote), n.Attempts,
			strArr(n.DeliveredChannels), n.LastDeliveryAt, n.TenantID, n.ID)
		if err != nil {
			return wrapErr(err)
		}
		if tag.RowsAffected() == 0 {
			return apierr.NotFound("记录不存在")
		}
		return nil
	})
}

// CountUnread 未读条数：read_at IS NULL。
// 用 SQL COUNT 而不是把列表拉回来数——后者只能数到 limit 条，
// 角标会显示假的未读数（"3+"这种含糊写法正是为了避免这个 bug，但真相更省事）。
func (r notifRepo) CountUnread(ctx context.Context, tenantID, userID string) (int, error) {
	n := 0
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM notifications
			WHERE tenant_id=$1 AND user_id=$2 AND read_at IS NULL`, tenantID, userID).Scan(&n)
	})
	return n, wrapErr(err)
}

// GetPrefs 取通知偏好。无行返回 (nil, nil)：语义是「用户没设置过」，
// 由上层按默认（全部已装配通道）处理。
func (r notifRepo) GetPrefs(ctx context.Context, tenantID, userID string) (*types.NotificationPrefs, error) {
	var out *types.NotificationPrefs
	err := r.run(ctx, func(q querier) error {
		var chs []string
		var updated time.Time
		err := q.QueryRow(ctx, `SELECT channels,updated_at FROM notification_prefs
			WHERE tenant_id=$1 AND user_id=$2`, tenantID, userID).Scan(&chs, &updated)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // 未设置
			}
			return err
		}
		out = &types.NotificationPrefs{TenantID: tenantID, UserID: userID, Channels: chs, UpdatedAt: updated}
		return nil
	})
	return out, wrapErr(err)
}

// SetPrefs 写通知偏好（UPSERT）。注意 tenant_id 来自**传入的结构**而非上下文：
// 表 owner 不受 RLS 限制，写路径的显式谓词才是防线（既有约定）。
func (r notifRepo) SetPrefs(ctx context.Context, p *types.NotificationPrefs) error {
	if p.Channels == nil {
		p.Channels = []string{}
	}
	return r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO notification_prefs(tenant_id,user_id,channels,updated_at)
			VALUES($1,$2,$3,now())
			ON CONFLICT (tenant_id,user_id) DO UPDATE SET channels=EXCLUDED.channels,updated_at=now()`,
			p.TenantID, p.UserID, jsonOf(p.Channels))
		return err
	})
}

// ListPendingDelivery 待重投通知（与 mem 同语义：pending/failed，created_at 升序）。
func (r notifRepo) ListPendingDelivery(ctx context.Context, tenantID string, limit int) ([]types.Notification, error) {
	if limit <= 0 {
		limit = 100
	}
	out := []types.Notification{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+notifColumns+` FROM notifications
			WHERE tenant_id=$1 AND delivery_status IN ('pending','failed')
			ORDER BY created_at ASC LIMIT $2`, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		var serr error
		out, serr = scanNotif(rows)
		return serr
	})
	return out, wrapErr(err)
}

// ---------- 辅助 ----------

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullTime 写可空 timestamptz 列：nil 指针 → SQL NULL。
//
// 直接把 *time.Time 交给 pgx 在 v5 下未必走「指针解引用」语义，
// 显式转换一次比赌驱动行为便宜——空值落错（写进零值时间）会让
// 「这个环境上次什么时候开的」变成一个 0001 年的假答案。
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// timePtr 读可空 timestamptz 列：零值时间视为 NULL。
// 与 nullTime 成对：读路径用 COALESCE(last_started_at, '0001-01-01') 兜底，
// 因此「零值」只可能是「列里本来就是 NULL」。
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	cp := t
	return &cp
}

// strArr 归一化写入 text[] NOT NULL 列的 Go 切片。
//
// 必要性（PG 实跑 0001/0002/0003 时暴露，mem 实现永远发现不了）：
// nullStatus 投递状态兜底：表里另有 CHECK 约束，空字符串会被 PG 直接拒掉（23514），
// 而 mem 允许任何字符串——这是两边最容易分叉的一处，写库前在这里统一。
func nullStatus(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Go 的 nil []string 经 pgx 编码为 SQL NULL，而列的 DEFAULT '{}' 只在 INSERT
// **省略该列**时生效，显式写入 NULL 会直接触发 23502 not-null violation。
// 典型触发者是 EnsureBuiltinRoles——除 finance 外所有内置角色的
// MaskFields/DenyElement 都是 nil。
func strArr(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// jsonOf 归一化写入 jsonb NOT NULL 列的通道列表。
// 与 strArr 同理：nil 切片必须先落成 `[]`，否则写进去的是 JSON null，
// 读回来 channels 字段为 null，上层解析分支又要多一种。
func jsonOf(s []string) []byte {
	if s == nil {
		s = []string{}
	}
	b, _ := json.Marshal(s)
	return b
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// wrapErr 把 pgx 错误翻译成平台错误码；L4：不得回显原始 SQL 细节给客户端。
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	if isNoRows(err) {
		return apierr.NotFound("记录不存在")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return apierr.Conflict("记录已存在")
		case "42501":
			return apierr.Forbidden("行级安全策略拒绝了该操作（租户隔离）")
		}
	}
	return err
}
