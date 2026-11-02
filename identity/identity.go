// Package identity 身份认证、会话与登录保护（M12/M14 横切能力）。
// L4：口令密文、TOTP 种子不得出现在日志或 API 响应。
package identity

import (
	"context"
	"time"

	"ziniao/billing"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/crypto"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Principal 认证主体（网关解析 token 后的最小身份）。
type Principal struct {
	UserID      string
	TenantID    string
	Username    string
	SessionID   string
	Roles       []types.Role
	Grants      []types.DataGrant
	IP          string
	MFAVerified bool
}

// Service 身份服务。
type Service struct {
	st  store.Store
	bus *eventbus.Bus
	cfg *config.Config
	ent *billing.Entitlement
}

// NewService 构造身份服务。
func NewService(st store.Store, bus *eventbus.Bus, cfg *config.Config, ent *billing.Entitlement) *Service {
	return &Service{st: st, bus: bus, cfg: cfg, ent: ent}
}

// LoginInput 登录入参。
type LoginInput struct {
	Username string
	Password string
	TOTPCode string
	TenantID string // 多团队时指定；为空且只有一个团队时自动选择
	Terminal eventbus.Terminal
}

// LoginResult 登录结果。
type LoginResult struct {
	Token       string      `json:"token"`
	ExpiresAt   time.Time   `json:"expires_at"`
	User        *types.User `json:"user"`
	TenantID    string      `json:"tenant_id"`
	Obligations []string    `json:"obligations,omitempty"`
	NewTerminal bool        `json:"new_terminal"`
}

// Login 认证 + 会话创建 + 登录保护判定 + 登录日志（审计六大日志之一）。
func (s *Service) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	user, err := s.st.Users().GetByUsername(ctx, in.Username)
	if err != nil || user == nil {
		// 用户名不存在也记一次失败日志（不泄露账号是否存在）
		_ = s.emitLogin(ctx, "", "", in.Username, "failure", "user_not_found", in.Terminal, nil)
		return nil, apierr.Unauthorized("账号或口令错误")
	}
	cred, err := s.st.Credentials().Get(ctx, user.ID)
	if err != nil {
		return nil, apierr.Unauthorized("账号或口令错误")
	}
	ok, _, err := crypto.VerifyPassword(in.Password, cred.PasswordEnc)
	if err != nil || !ok {
		_ = s.emitLogin(ctx, "", user.ID, in.Username, "failure", "bad_password", in.Terminal, nil)
		return nil, apierr.Unauthorized("账号或口令错误")
	}
	// 二次验证：启用 MFA 却没有可用种子时必须失败关闭，不能静默降级为密码登录。
	mfaVerified := false
	if user.MFAEnabled {
		if user.TOTPSecret == "" || !crypto.VerifyTOTP(user.TOTPSecret, in.TOTPCode, time.Now(), 1) {
			_ = s.emitLogin(ctx, "", user.ID, in.Username, "blocked", "mfa_failed", in.Terminal, nil)
			return nil, apierr.New(apierr.CodeMFARequired, 401, "二次验证码错误或尚未完成 MFA 配置")
		}
		mfaVerified = true
	}

	// 选择租户
	memberships, err := s.st.Memberships().ListByUser(ctx, user.ID)
	if err != nil || len(memberships) == 0 {
		return nil, apierr.Forbidden("账号未加入任何团队")
	}
	tenantID := in.TenantID
	if tenantID == "" {
		if len(memberships) > 1 {
			return nil, apierr.Validation("该账号属于多个团队，请指定 tenant_id")
		}
		tenantID = memberships[0].TenantID
	}
	var membership *types.Membership
	for i := range memberships {
		if memberships[i].TenantID == tenantID {
			membership = &memberships[i]
			break
		}
	}
	if membership == nil || membership.Status != "active" {
		return nil, apierr.Forbidden("该团队下账号不可用")
	}

	// 登录保护：时段限制（M12）
	now := time.Now()
	if reason := s.checkLoginWindow(now); reason != "" {
		_ = s.emitLogin(ctx, tenantID, user.ID, in.Username, "blocked", reason, in.Terminal, nil)
		return nil, apierr.Forbidden("当前时段不允许登录（登录保护策略）")
	}
	// 登录保护：新终端识别（M12）→ 不阻断，但标记 + 告警
	newTerminal := s.isNewTerminal(store.WithTenant(ctx, tenantID), user.ID, in.Terminal.Fingerprint)

	session := &types.Session{
		ID: ids.New(), UserID: user.ID, TenantID: tenantID,
		IP: in.Terminal.IP, UserAgent: in.Terminal.UserAgent,
		TerminalFP: in.Terminal.Fingerprint, ClientVer: in.Terminal.ClientVer,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.cfg.Limits.AccessTokenTTL),
		LoginResult: "success",
	}
	token, err := crypto.SignJWT(s.cfg.JWTSecret, map[string]any{
		"sub": user.ID, "tid": tenantID, "sid": session.ID, "usr": user.Username,
		"mfa": mfaVerified,
	}, s.cfg.Limits.AccessTokenTTL)
	if err != nil {
		return nil, err
	}

	tctx := store.WithTenant(ctx, tenantID)
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Sessions().Create(ctx, session); err != nil {
			return err
		}
		if err := tx.Users().TouchLogin(ctx, user.ID, now); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: tenantID, Type: eventbus.TypeAuthLogin, OccurredAt: now,
			Risk:     riskOf(newTerminal),
			Actor:    eventbus.Actor{UserID: user.ID, Username: user.Username},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{Action: "auth.login", Result: "success"},
			Data: map[string]any{
				"session_id":   session.ID,
				"ip":           in.Terminal.IP,
				"new_terminal": newTerminal,
			},
		})
	}); err != nil {
		return nil, err
	}

	// API / CLI / Agent 调用同样走这里，因此自动化不会成为权限旁路（L3）
	res := &LoginResult{
		Token: token, ExpiresAt: session.ExpiresAt, User: user,
		TenantID: tenantID, NewTerminal: newTerminal,
	}
	if newTerminal {
		res.Obligations = append(res.Obligations, "require_mfa")
	}
	return res, nil
}

// Logout 注销会话并写登出日志。
func (s *Service) Logout(ctx context.Context, tenantID, sessionID, userID string, term eventbus.Terminal) error {
	tctx := store.WithTenant(ctx, tenantID)
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Sessions().Revoke(ctx, sessionID); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: tenantID, Type: eventbus.TypeAuthLogout, OccurredAt: time.Now(),
			Actor: eventbus.Actor{UserID: userID}, Terminal: term,
			Context: eventbus.ContextInfo{Action: "auth.logout", Result: "success"},
			Data:    map[string]any{"session_id": sessionID},
		})
	})
}

// Authenticate 解析并校验访问令牌，返回认证主体。
func (s *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	claims, err := crypto.ParseJWT(s.cfg.JWTSecret, token)
	if err != nil {
		return nil, apierr.Unauthorized("令牌无效或已过期")
	}
	userID, _ := claims["sub"].(string)
	tenantID, _ := claims["tid"].(string)
	sessionID, _ := claims["sid"].(string)
	username, _ := claims["usr"].(string)
	if userID == "" || tenantID == "" {
		return nil, apierr.Unauthorized("令牌缺少必要声明")
	}
	// 会话查询按 token 声明的租户隔离（此时上下文尚未注入租户）
	sess, err := s.st.Sessions().Get(store.WithTenant(ctx, tenantID), sessionID)
	if err != nil || sess.UserID != userID || sess.TenantID != tenantID {
		return nil, apierr.Unauthorized("会话不存在")
	}
	if sess.RevokedAt != nil {
		return nil, apierr.Unauthorized("会话已注销")
	}
	if time.Now().After(sess.ExpiresAt) {
		return nil, apierr.Unauthorized("会话已过期")
	}
	if time.Since(sess.LastSeenAt) > s.cfg.Limits.SessionIdleTimeout {
		return nil, apierr.Unauthorized("无操作超时，请重新登录")
	}

	_, err = s.st.Users().GetByID(ctx, userID)
	if err != nil {
		return nil, apierr.Unauthorized("账号不存在")
	}
	urs, err := s.st.UserRoles().ListByUser(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	roles := make([]types.Role, 0, len(urs))
	for _, ur := range urs {
		role, rErr := s.st.Roles().Get(ctx, ur.RoleID)
		if rErr != nil || role == nil {
			continue
		}
		// 临时授权：到期自动失效（M12）
		// 判定依据是 data_grants 里带 expires_at 的记录——所以底层仓储必须把
		// 过期记录一并返回（含过期），不能提前过滤。这里的 now 判定解决
		// 「到期即可失效」，物理清理与通知交给 tenant.RevokeExpiredGrants 任务。
		if ur.TenantID != "" {
			grants, gErr := s.st.DataGrants().ListByUser(ctx, tenantID, userID, "role")
			if gErr == nil {
				expired := false
				for _, g := range grants {
					if g.ResourceID == role.ID && g.ExpiresAt != nil && g.ExpiresAt.Before(time.Now()) {
						expired = true
					}
				}
				if expired {
					continue
				}
			}
		}
		roles = append(roles, *role)
	}
	grants, _ := s.st.DataGrants().ListByUser(ctx, tenantID, userID, "")
	// MFA 状态必须来自登录时签名的会话声明，不能仅凭用户启用了 MFA 就
	// 把所有会话标记为已验证（否则未完成二次验证的旧令牌也能通过 MFA 权限门）。
	mfaVerified, _ := claims["mfa"].(bool)
	return &Principal{
		UserID: userID, TenantID: tenantID, Username: username, SessionID: sessionID,
		Roles: roles, Grants: grants, IP: sess.IP, MFAVerified: mfaVerified,
	}, nil
}

// Subject 把认证主体转换为权限裁决主体（L3：以发起人身份裁决）。
func (s *Service) Subject(ctx context.Context, p *Principal) (iam.Subject, error) {
	plan, err := s.ent.PlanOf(ctx, p.TenantID)
	if err != nil {
		plan = config.PlanCatalog(s.cfg.Limits)["free"]
	}
	return iam.Subject{
		UserID:      p.UserID,
		TenantID:    p.TenantID,
		Username:    p.Username,
		Roles:       p.Roles,
		Grants:      p.Grants,
		Plan:        &plan,
		IP:          p.IP,
		MFAVerified: p.MFAVerified,
		Now:         time.Now(),
	}, nil
}

// ---------- 内部辅助 ----------

func (s *Service) emitLogin(ctx context.Context, tenantID, userID, username, result, cause string,
	term eventbus.Terminal, extra map[string]any) error {
	data := map[string]any{"cause": cause}
	for k, v := range extra {
		data[k] = v
	}
	if userID != "" {
		data["user_id"] = userID
	}
	ev := eventbus.Event{
		TenantID: tenantID, Type: eventbus.TypeAuthFailed, OccurredAt: time.Now(),
		Risk:     eventbus.RiskMedium,
		Actor:    eventbus.Actor{UserID: userID, Username: username},
		Terminal: term,
		Context:  eventbus.ContextInfo{Action: "auth.login", Result: result},
		Data:     data,
	}
	if tenantID == "" {
		// 未确定租户（账号不存在/口令错误）时，仍要在无租户上下文下留痕：
		// 写入系统级审计流，避免攻击者探测行为完全无痕。
		ctx = store.WithTenant(ctx, "-")
		ev.TenantID = "-"
	}
	return s.st.WithTx(ctx, func(tx store.Tx) error {
		return s.bus.Emit(ctx, tx, ev)
	})
}

// checkLoginWindow 非工作时间禁止登录（可配置，默认 0-24 即不限制）。
func (s *Service) checkLoginWindow(now time.Time) string {
	start, end := s.cfg.Limits.LoginAllowedStartHour, s.cfg.Limits.LoginAllowedEndHour
	if start == 0 && end == 24 {
		return ""
	}
	h := now.Hour()
	if h < start || h >= end {
		return "outside_login_window"
	}
	return ""
}

func (s *Service) isNewTerminal(ctx context.Context, userID, fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	sessions, err := s.st.Sessions().ListByUser(ctx, userID)
	if err != nil {
		return false
	}
	for _, sess := range sessions {
		if sess.TerminalFP == fingerprint {
			return false
		}
	}
	return true
}

func riskOf(newTerminal bool) eventbus.RiskLevel {
	if newTerminal {
		return eventbus.RiskMedium
	}
	return eventbus.RiskLow
}
