// Package tenant 多租户与团队：租户生命周期、成员、角色授予。
// 团队间数据完全隔离；一个手机号可管理多个独立团队。
package tenant

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ziniao/billing"
	"ziniao/eventbus"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/crypto"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Service 租户服务。
type Service struct {
	st    store.Store
	bus   *eventbus.Bus
	cfg   *config.Config
	meter *billing.Meter
	ent   *billing.Entitlement
}

// NewService 构造租户服务。
func NewService(st store.Store, bus *eventbus.Bus, cfg *config.Config, meter *billing.Meter, ent *billing.Entitlement) *Service {
	return &Service{st: st, bus: bus, cfg: cfg, meter: meter, ent: ent}
}

// CreateInput 创建租户入参。
type CreateInput struct {
	Name     string
	Slug     string
	PlanCode string
	Owner    OwnerInput
	Terminal eventbus.Terminal
}

// OwnerInput 主账号信息。
type OwnerInput struct {
	Username string
	Password string
	Email    string
	Phone    string
	Nickname string
}

// CreateResult 创建结果。
type CreateResult struct {
	Tenant *types.Tenant `json:"tenant"`
	User   *types.User   `json:"user"`
	Role   *types.Role   `json:"role"`
}

// Create 建租户 + 主账号 + 授 owner 角色 + 开订阅（全部在同一事务，含事件投递）。
func (s *Service) Create(ctx context.Context, in CreateInput) (*CreateResult, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, apierr.Validation("团队名称不能为空")
	}
	if len(in.Owner.Password) < s.cfg.Limits.PasswordMinLen {
		return nil, apierr.Validation(fmt.Sprintf("口令长度不足，至少 %d 位", s.cfg.Limits.PasswordMinLen))
	}
	slug := in.Slug
	if slug == "" {
		slug = "t-" + strings.ToLower(ids.New()[:10])
	}
	if _, err := s.st.Tenants().GetBySlug(ctx, slug); err == nil {
		return nil, apierr.Conflict("团队标识已存在")
	}
	if _, err := s.st.Users().GetByUsername(ctx, in.Owner.Username); err == nil {
		return nil, apierr.Conflict("账号已存在")
	}

	now := time.Now().UTC()
	tenant := &types.Tenant{
		ID:        ids.New(),
		Name:      in.Name,
		Slug:      slug,
		Status:    types.TenantStatusTrial,
		PlanCode:  in.PlanCode,
		CreatedAt: now,
	}
	if tenant.PlanCode == "" {
		tenant.PlanCode = "free"
	}
	user := &types.User{
		ID:          ids.New(),
		Username:    in.Owner.Username,
		Email:       in.Owner.Email,
		Phone:       in.Owner.Phone,
		DisplayName: in.Owner.Nickname,
		Status:      "active",
		CreatedAt:   now,
	}
	pwd, err := crypto.HashPassword(in.Owner.Password)
	if err != nil {
		return nil, err
	}
	// 系统内置角色的 ID 需在事务外先解析
	ownerRole, err := s.st.Roles().GetByCode(ctx, "", "owner")
	if err != nil || ownerRole == nil {
		return nil, apierr.Internal("内置 owner 角色缺失，请先执行角色初始化")
	}

	res := &CreateResult{}
	if err := s.st.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.Tenants().Create(ctx, tenant); err != nil {
			return err
		}
		if err := tx.Users().Create(ctx, user); err != nil {
			return err
		}
		if err := tx.Credentials().Upsert(ctx, &types.Credential{
			UserID: user.ID, PasswordEnc: pwd, Algo: "pbkdf2-sha256", KeyVersion: 1, UpdatedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.Memberships().Create(ctx, &types.Membership{
			ID: ids.New(), TenantID: tenant.ID, UserID: user.ID,
			Status: "active", JoinedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.UserRoles().Assign(ctx, &types.UserRole{
			ID: ids.New(), TenantID: tenant.ID, UserID: user.ID, RoleID: ownerRole.ID,
			GrantedBy: user.ID, GrantedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.Subscriptions().Create(ctx, &types.Subscription{
			ID: ids.New(), TenantID: tenant.ID, PlanCode: tenant.PlanCode,
			Status: "active", StartedAt: now, ExpiresAt: now.AddDate(0, 1, 0),
		}); err != nil {
			return err
		}

		// 计量：成员 +1（统一入口，禁止各模块自增）
		if _, err := s.meter.Record(ctx, tx, tenant.ID, billing.MetricMemberCount, 1); err != nil {
			return err
		}

		tctx := store.WithTenant(ctx, tenant.ID)
		actor := eventbus.Actor{UserID: user.ID, Username: user.Username, RoleCode: []string{"owner"}}
		if err := s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: tenant.ID, Type: eventbus.TypeTenantCreated, OccurredAt: now,
			Actor: actor, Terminal: in.Terminal,
			Context: eventbus.ContextInfo{Action: "tenant.create", Result: "success"},
			Data:    map[string]any{"tenant_id": tenant.ID, "plan": tenant.PlanCode},
		}); err != nil {
			return err
		}
		if err := s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: tenant.ID, Type: eventbus.TypeUserRegistered, OccurredAt: now,
			Actor: actor, Terminal: in.Terminal,
			Context: eventbus.ContextInfo{Action: "user.register", Result: "success"},
			Data:    map[string]any{"user_id": user.ID},
		}); err != nil {
			return err
		}
		if err := s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: tenant.ID, Type: eventbus.TypeRoleGranted, OccurredAt: now, Risk: eventbus.RiskMedium,
			Actor: actor, Terminal: in.Terminal,
			Context: eventbus.ContextInfo{Action: "iam.role.grant", Result: "success"},
			Data: map[string]any{
				"user_id":   user.ID,
				"role_id":   ownerRole.ID,
				"role_code": ownerRole.Code,
			},
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	res.Tenant = tenant
	res.User = user
	res.Role = ownerRole
	return res, nil
}

// Get 获取租户。
func (s *Service) Get(ctx context.Context, id string) (*types.Tenant, error) {
	return s.st.Tenants().Get(ctx, id)
}

// ListMembers 列出成员。
func (s *Service) ListMembers(ctx context.Context, tenantID string) ([]types.Membership, error) {
	return s.st.Memberships().ListByTenant(ctx, tenantID)
}

// InviteInput 邀请成员入参。
type InviteInput struct {
	TenantID  string
	Username  string
	Password  string
	Nickname  string
	Email     string
	InvitedBy string
	Terminal  eventbus.Terminal
}

// InviteMember 手动添加成员（M12 三入口之一；邀请链接与批量导入见 TODO）。
func (s *Service) InviteMember(ctx context.Context, in InviteInput) (*types.User, error) {
	if in.TenantID == "" {
		return nil, apierr.Validation("缺少 tenant_id")
	}
	if len(in.Password) < s.cfg.Limits.PasswordMinLen {
		return nil, apierr.Validation(fmt.Sprintf("口令长度不足，至少 %d 位", s.cfg.Limits.PasswordMinLen))
	}
	if err := s.ent.CheckQuota(ctx, in.TenantID, billing.MetricMemberCount, 1); err != nil {
		return nil, err
	}
	existing, err := s.st.Users().GetByUsername(ctx, in.Username)
	now := time.Now().UTC()

	var user *types.User
	if err == nil && existing != nil {
		user = existing
		if _, mErr := s.st.Memberships().Get(ctx, in.TenantID, user.ID); mErr == nil {
			return nil, apierr.Conflict("该账号已是本团队成员")
		}
	} else {
		pwd, hErr := crypto.HashPassword(in.Password)
		if hErr != nil {
			return nil, hErr
		}
		user = &types.User{
			ID: ids.New(), Username: in.Username, Email: in.Email,
			DisplayName: in.Nickname, Status: "active", CreatedAt: now,
		}
		tctx := store.WithTenant(ctx, in.TenantID)
		if err := s.st.WithTx(tctx, func(tx store.Tx) error {
			if err := tx.Users().Create(ctx, user); err != nil {
				return err
			}
			return tx.Credentials().Upsert(ctx, &types.Credential{
				UserID: user.ID, PasswordEnc: pwd, Algo: "pbkdf2-sha256", KeyVersion: 1, UpdatedAt: now,
			})
		}); err != nil {
			return nil, err
		}
	}

	tctx := store.WithTenant(ctx, in.TenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Memberships().Create(ctx, &types.Membership{
			ID: ids.New(), TenantID: in.TenantID, UserID: user.ID,
			Status: "active", InvitedBy: in.InvitedBy, JoinedAt: now,
		}); err != nil {
			return err
		}
		if _, err := s.meter.Record(ctx, tx, in.TenantID, billing.MetricMemberCount, 1); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.TenantID, Type: eventbus.TypeMemberInvited, OccurredAt: now,
			Actor: eventbus.Actor{UserID: in.InvitedBy}, Terminal: in.Terminal,
			Context: eventbus.ContextInfo{Action: "member.invite", Result: "success"},
			Data:    map[string]any{"user_id": user.ID, "username": user.Username},
		})
	}); err != nil {
		return nil, err
	}
	return user, nil
}

// GrantInput 授予角色入参。
type GrantInput struct {
	TenantID  string
	UserID    string
	RoleCode  string
	GrantedBy string
	ExpiresAt *time.Time // 临时授权到期时间（M12）
	Terminal  eventbus.Terminal
}

// GrantRole 授予角色（管理变动类 → 权限日志）。
func (s *Service) GrantRole(ctx context.Context, in GrantInput) error {
	role, err := s.st.Roles().GetByCode(ctx, in.TenantID, in.RoleCode)
	if err != nil || role == nil {
		return apierr.NotFound("角色不存在: " + in.RoleCode)
	}
	tctx := store.WithTenant(ctx, in.TenantID)
	now := time.Now().UTC()
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.UserRoles().Assign(ctx, &types.UserRole{
			ID: ids.New(), TenantID: in.TenantID, UserID: in.UserID, RoleID: role.ID,
			GrantedBy: in.GrantedBy, GrantedAt: now,
		}); err != nil {
			return err
		}
		if in.ExpiresAt != nil {
			if err := tx.DataGrants().Create(ctx, &types.DataGrant{
				ID: ids.New(), TenantID: in.TenantID, UserID: in.UserID,
				ResourceType: "role", ResourceID: role.ID, Scope: "temporary",
				GrantedBy: in.GrantedBy, ExpiresAt: in.ExpiresAt, CreatedAt: now,
			}); err != nil {
				return err
			}
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.TenantID, Type: eventbus.TypeRoleGranted, OccurredAt: now,
			Risk:  eventbus.RiskMedium,
			Actor: eventbus.Actor{UserID: in.GrantedBy}, Terminal: in.Terminal,
			Context: eventbus.ContextInfo{Action: "iam.role.grant", Result: "success"},
			Data: map[string]any{
				"user_id": in.UserID, "role_id": role.ID, "role_code": role.Code,
				"temporary": in.ExpiresAt != nil,
			},
		})
	})
}

// RevokeRole 回收角色。
func (s *Service) RevokeRole(ctx context.Context, tenantID, userID, roleCode, operatorID string, term eventbus.Terminal) error {
	role, err := s.st.Roles().GetByCode(ctx, tenantID, roleCode)
	if err != nil || role == nil {
		return apierr.NotFound("角色不存在: " + roleCode)
	}
	tctx := store.WithTenant(ctx, tenantID)
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.UserRoles().Revoke(ctx, tenantID, userID, role.ID); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: tenantID, Type: eventbus.TypeRoleRevoked, OccurredAt: time.Now().UTC(),
			Risk:  eventbus.RiskMedium,
			Actor: eventbus.Actor{UserID: operatorID}, Terminal: term,
			Context: eventbus.ContextInfo{Action: "iam.role.revoke", Result: "success"},
			Data:    map[string]any{"user_id": userID, "role_code": roleCode},
		})
	})
}
