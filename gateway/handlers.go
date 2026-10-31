package gateway

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"ziniao/audit"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
	"ziniao/tenant"
)

func asError(err error, target **apierr.Error) bool { return errors.As(err, target) }

// ---------- 系统 ----------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "phase": "2", "store": s.deps.Config.StoreDriver,
	})
}

// ---------- 身份 ----------

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Slug     string `json:"slug"`
		Plan     string `json:"plan"`
		Username string `json:"username"`
		Password string `json:"password"`
		Email    string `json:"email"`
		Phone    string `json:"phone"`
		Nickname string `json:"nickname"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	res, err := s.deps.Tenant.Create(r.Context(), tenant.CreateInput{
		Name: body.Name, Slug: body.Slug, PlanCode: body.Plan,
		Owner: tenant.OwnerInput{
			Username: body.Username, Password: body.Password,
			Email: body.Email, Phone: body.Phone, Nickname: body.Nickname,
		},
		Terminal: terminalFrom(r),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTP     string `json:"totp"`
		TenantID string `json:"tenant_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	res, err := s.deps.Identity.Login(r.Context(), loginInput(body.Username, body.Password, body.TOTP, body.TenantID, terminalFrom(r)))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		writeErr(w, apierr.Unauthorized("未认证"))
		return
	}
	if err := s.deps.Identity.Logout(r.Context(), p.TenantID, p.SessionID, p.UserID, terminalFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	subj, err := s.deps.Identity.Subject(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	codes := []string{}
	for _, role := range p.Roles {
		codes = append(codes, role.Code)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":    p.UserID,
		"username":   p.Username,
		"tenant_id":  p.TenantID,
		"roles":      codes,
		"data_scope": s.deps.IAM.Check(r.Context(), iam.Request{Subject: subj, Action: "shop:read"}).DataScope,
	})
}

// ---------- 租户与成员 ----------

func (s *Server) handleTenantGet(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "member:list", "/team")
	if err != nil {
		writeErr(w, err)
		return
	}
	if r.PathValue("id") != p.TenantID {
		writeErr(w, apierr.Forbidden("禁止跨团队访问"))
		return
	}
	t, err := s.deps.Tenant.Get(r.Context(), p.TenantID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleMemberList(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "member:list", "/team")
	if err != nil {
		writeErr(w, err)
		return
	}
	members, err := s.deps.Tenant.ListMembers(r.Context(), p.TenantID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": members, "total": len(members)})
}

func (s *Server) handleMemberInvite(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "member:invite", "/team")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
		Email    string `json:"email"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	user, err := s.deps.Tenant.InviteMember(r.Context(), tenant.InviteInput{
		TenantID: p.TenantID, Username: body.Username, Password: body.Password,
		Nickname: body.Nickname, Email: body.Email, InvitedBy: p.UserID,
		Terminal: terminalFrom(r),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleGrantRole(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "role:grant", "/team")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Role       string `json:"role"`
		ExpiresInH int    `json:"expires_in_hours"` // 临时授权（M12）
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	var expires *time.Time
	if body.ExpiresInH > 0 {
		t := time.Now().Add(time.Duration(body.ExpiresInH) * time.Hour)
		expires = &t
	}
	if err := s.deps.Tenant.GrantRole(r.Context(), tenant.GrantInput{
		TenantID: p.TenantID, UserID: r.PathValue("userID"), RoleCode: body.Role,
		GrantedBy: p.UserID, ExpiresAt: expires, Terminal: terminalFrom(r),
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 权限 ----------

func (s *Server) handleRoleList(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "member:list", "/team")
	if err != nil {
		writeErr(w, err)
		return
	}
	roles, err := s.deps.Store.Roles().List(r.Context(), p.TenantID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": roles})
}

func (s *Server) handlePermissionCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": iam.Catalog})
}

func (s *Server) handlePolicyCheck(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	subj, err := s.deps.Identity.Subject(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Action       string `json:"action"`
		Path         string `json:"path"`
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	d := s.deps.IAM.Check(r.Context(), iam.Request{
		Subject: subj, Action: body.Action, Path: iam.NormalizePath(body.Path),
		ResourceType: body.ResourceType, ResourceID: body.ResourceID,
	})
	writeJSON(w, http.StatusOK, d)
}

// handleDOMPolicy 下发给客户端注入层的策略：字段遮蔽 + 元素屏蔽 + 水印（L6 第三层）。
func (s *Server) handleDOMPolicy(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	subj, err := s.deps.Identity.Subject(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	path := iam.NormalizePath(r.URL.Query().Get("path"))
	if path == "" {
		path = "/shops"
	}
	writeJSON(w, http.StatusOK, s.deps.IAM.DOMPolicy(r.Context(), subj, path))
}

// ---------- 店铺 ----------

func (s *Server) handleShopCreate(w http.ResponseWriter, r *http.Request) {
	_, subj, _, err := s.authorize(r, "shop:create", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
		Site     string `json:"site"`
		Status   string `json:"status"`
		URL      string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	sp, err := s.deps.Shop.Create(r.Context(), shopCreateInput(body.Name, body.Platform, body.Site, body.Status, body.URL, subj, terminalFrom(r)))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sp)
}

func (s *Server) handleShopList(w http.ResponseWriter, r *http.Request) {
	_, subj, _, err := s.authorize(r, "shop:read", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	limit := intQuery(r, "limit", 20)
	offset := intQuery(r, "offset", 0)
	items, total, err := s.deps.Shop.List(r.Context(), subj, "/shops", limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (s *Server) handleShopGet(w http.ResponseWriter, r *http.Request) {
	_, subj, _, err := s.authorize(r, "shop:read", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := s.deps.Shop.Get(r.Context(), subj, r.PathValue("id"), "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleShopDelete(w http.ResponseWriter, r *http.Request) {
	_, subj, _, err := s.authorize(r, "shop:delete", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.deps.Shop.Delete(r.Context(), subj, r.PathValue("id"), "/shops", terminalFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 审计 ----------

func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "audit:read", "/audit")
	if err != nil {
		writeErr(w, err)
		return
	}
	f := store.AuditFilter{
		Category:    types.AuditCategory(r.URL.Query().Get("category")),
		ActorUserID: r.URL.Query().Get("actor_id"),
		ShopID:      r.URL.Query().Get("shop_id"),
		Action:      r.URL.Query().Get("action"),
		Keyword:     r.URL.Query().Get("keyword"),
		Limit:       intQuery(r, "limit", 50),
		Offset:      intQuery(r, "offset", 0),
	}
	items, total, err := s.deps.Audit.List(store.WithTenant(r.Context(), p.TenantID), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	p, subj, d, err := s.authorize(r, "audit:export", "/audit")
	if err != nil {
		writeErr(w, err)
		return
	}
	// 高危导出：必须携带已通过的审批单（反例 8：不能只灰按钮，也不能只传 approved=true）
	ticketID := r.URL.Query().Get("ticket")
	wm := &audit.Watermark{}
	approved := false
	if s.deps.Approvals == nil {
		// 未装配审批模块时退化为义务提示（保持 Phase 0 行为）
		approved = ticketID != "" || r.URL.Query().Get("approved") == "true"
	} else if s.deps.Approvals.Required(types.ApprovalTypeAuditExport) {
		ticket, verr := s.deps.Approvals.Verify(r.Context(), p.TenantID, ticketID,
			types.ApprovalTypeAuditExport, subj.UserID)
		if verr != nil {
			writeErr(w, verr)
			return
		}
		approved = true
		wm = &audit.Watermark{
			TenantID: p.TenantID, ActorID: subj.UserID, ActorName: subj.Username,
			TicketID: ticket.ID, At: time.Now().UTC(),
		}
	} else {
		approved = true
		wm = nil
	}
	if !approved && containsStr(d.Obligations, "require_approval") {
		writeErr(w, apierr.Approval("审计日志明文导出需二级审批，请提交审批工单后携带 ticket 重试"))
		return
	}
	res, err := s.deps.Audit.Export(store.WithTenant(r.Context(), p.TenantID), store.AuditFilter{
		Limit: s.deps.Config.Limits.ExportMaxRows,
	}, s.deps.Config.Limits.ExportMaxRows, approved, wm)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 审批单一次性核销：一张单只能导出一次
	if s.deps.Approvals != nil && ticketID != "" {
		_ = s.deps.Approvals.Consume(r.Context(), p.TenantID, ticketID, subj, terminalFrom(r))
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+res.Filename+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Content)
}

func (s *Server) handleAuditVerify(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "audit:read", "/audit")
	if err != nil {
		writeErr(w, err)
		return
	}
	days := intQuery(r, "days", 3650)
	since := time.Now().AddDate(0, 0, -days)
	res, err := s.deps.Audit.Verify(store.WithTenant(r.Context(), p.TenantID), p.TenantID, since)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 通知 ----------

func (s *Server) handleNotifyList(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	items, err := s.deps.Notify.List(r.Context(), p.TenantID, p.UserID, intQuery(r, "limit", 50))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (s *Server) handleNotifyRead(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := s.deps.Notify.MarkRead(r.Context(), p.TenantID, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 辅助 ----------

func intQuery(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

func containsStr(list []string, v string) bool {
	for _, it := range list {
		if it == v {
			return true
		}
	}
	return false
}
