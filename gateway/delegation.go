package gateway

import (
	"context"
	"net/http"

	"ziniao/delegation"
	"ziniao/platform/apierr"
	"ziniao/platform/types"
)

// routesDelegation 代理机构委托审批路由（跨租户两方流程，approval_flows）。
func (s *Server) routesDelegation() {
	m := s.mux
	m.HandleFunc("GET /api/v1/delegations", s.authed(s.handleDelegationList))
	m.HandleFunc("POST /api/v1/delegations", s.authed(s.handleDelegationSubmit))
	m.HandleFunc("POST /api/v1/delegations/{id}/approve", s.authed(s.handleDelegationApprove))
	m.HandleFunc("POST /api/v1/delegations/{id}/reject", s.authed(s.handleDelegationReject))
	m.HandleFunc("POST /api/v1/delegations/{id}/cancel", s.authed(s.handleDelegationCancel))
	m.HandleFunc("POST /api/v1/delegations/{id}/revoke", s.authed(s.handleDelegationRevoke))
}

func (s *Server) delegationSvc(w http.ResponseWriter) *delegation.Service {
	if s.deps.Delegations == nil {
		s.notWired(w)
		return nil
	}
	return s.deps.Delegations
}

func (s *Server) handleDelegationList(w http.ResponseWriter, r *http.Request) {
	_, subj, _, err := s.authorize(r, "delegation:read", "/delegations")
	if err != nil {
		writeErr(w, err)
		return
	}
	svc := s.delegationSvc(w)
	if svc == nil {
		return
	}
	party := r.URL.Query().Get("party")
	if party != "agency" {
		party = "grantor"
	}
	list, total, err := svc.List(r.Context(), subj, party,
		types.DelegationStatus(r.URL.Query().Get("status")),
		queryInt(r, "limit", 50), queryInt(r, "offset", 0), "/delegations")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": total})
}

func (s *Server) handleDelegationSubmit(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "delegation:apply", "/delegations")
	if err != nil {
		writeErr(w, err)
		return
	}
	svc := s.delegationSvc(w)
	if svc == nil {
		return
	}
	var body struct {
		TenantSlug string   `json:"tenant_slug"`
		ShopIDs    []string `json:"shop_ids"`
		ValidDays  int      `json:"valid_days"`
		Reason     string   `json:"reason"`
		URL        string   `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	f, err := svc.Submit(r.Context(), delegation.SubmitInput{
		GrantorSlug: body.TenantSlug, ShopIDs: body.ShopIDs, ValidDays: body.ValidDays,
		Reason: body.Reason,
		Actor:  p2subject(p, subj), Terminal: terminalFrom(r), URL: orDefault(body.URL, "/delegations"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// decideDelegation approve/reject 共用（委托方裁决）。
func (s *Server) decideDelegation(w http.ResponseWriter, r *http.Request,
	fn func(ctx context.Context, in delegation.DecideInput) (*types.DelegationFlow, error)) {
	p, subj, _, err := s.authorize(r, "delegation:approve", "/delegations")
	if err != nil {
		writeErr(w, err)
		return
	}
	svc := s.delegationSvc(w)
	if svc == nil {
		return
	}
	var body struct {
		Comment string `json:"comment"`
		URL     string `json:"url"`
	}
	_ = decodeJSON(r, &body)
	f, err := fn(r.Context(), delegation.DecideInput{
		FlowID: r.PathValue("id"), Comment: body.Comment,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: orDefault(body.URL, "/delegations"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleDelegationApprove(w http.ResponseWriter, r *http.Request) {
	s.decideDelegation(w, r, s.deps.Delegations.Approve)
}

func (s *Server) handleDelegationReject(w http.ResponseWriter, r *http.Request) {
	s.decideDelegation(w, r, s.deps.Delegations.Reject)
}

// decideDelegationSelf cancel/revoke 共用（各自一方按单据内容收尾）。
func (s *Server) decideDelegationSelf(w http.ResponseWriter, r *http.Request, perm string,
	fn func(ctx context.Context, in delegation.DecideInput) (*types.DelegationFlow, error)) {
	p, subj, _, err := s.authorize(r, perm, "/delegations")
	if err != nil {
		writeErr(w, err)
		return
	}
	svc := s.delegationSvc(w)
	if svc == nil {
		return
	}
	var body struct {
		Comment string `json:"comment"`
		URL     string `json:"url"`
	}
	_ = decodeJSON(r, &body)
	f, err := fn(r.Context(), delegation.DecideInput{
		FlowID: r.PathValue("id"), Comment: body.Comment,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: orDefault(body.URL, "/delegations"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleDelegationCancel(w http.ResponseWriter, r *http.Request) {
	s.decideDelegationSelf(w, r, "delegation:apply", s.deps.Delegations.Cancel)
}

func (s *Server) handleDelegationRevoke(w http.ResponseWriter, r *http.Request) {
	s.decideDelegationSelf(w, r, "delegation:revoke", s.deps.Delegations.Revoke)
}
