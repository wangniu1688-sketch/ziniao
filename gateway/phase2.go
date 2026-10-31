package gateway

import (
	"context"
	"net/http"

	"ziniao/approval"
	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
	"ziniao/shop"
)

// routesPhase2 Phase 2 路由：审批工单 / 定时任务 / 批量导入。
func (s *Server) routesPhase2() {
	m := s.mux
	m.HandleFunc("GET /api/v1/approvals", s.authed(s.handleApprovalList))
	m.HandleFunc("POST /api/v1/approvals", s.authed(s.handleApprovalCreate))
	m.HandleFunc("POST /api/v1/approvals/{id}/approve", s.authed(s.handleApprovalApprove))
	m.HandleFunc("POST /api/v1/approvals/{id}/reject", s.authed(s.handleApprovalReject))
	m.HandleFunc("POST /api/v1/approvals/{id}/cancel", s.authed(s.handleApprovalCancel))

	m.HandleFunc("GET /api/v1/jobs", s.authed(s.handleJobList))
	m.HandleFunc("POST /api/v1/jobs/run", s.authed(s.handleJobRun))

	m.HandleFunc("POST /api/v1/shops/batch/import", s.authed(s.handleShopBatchImport))
}

// ---------- 审批工单 ----------

func (s *Server) handleApprovalList(w http.ResponseWriter, r *http.Request) {
	_, subj, _, err := s.authorize(r, "approval:read", "/approvals")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Approvals == nil {
		s.notWired(w)
		return
	}
	f := store.ApprovalFilter{
		Status:      types.ApprovalStatus(r.URL.Query().Get("status")),
		Type:        r.URL.Query().Get("type"),
		RequesterID: r.URL.Query().Get("requester_id"),
		Limit:       queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	}
	list, total, err := s.deps.Approvals.List(r.Context(), subj, f, "/approvals")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": total})
}

func (s *Server) handleApprovalCreate(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "approval:create", "/approvals")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Approvals == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Type        string         `json:"type"`
		SubjectType string         `json:"subject_type"`
		SubjectID   string         `json:"subject_id"`
		Reason      string         `json:"reason"`
		Detail      map[string]any `json:"detail"`
		URL         string         `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	t, err := s.deps.Approvals.Create(r.Context(), approval.CreateInput{
		Type: body.Type, SubjectType: body.SubjectType, SubjectID: body.SubjectID,
		Reason: body.Reason, Detail: body.Detail,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: orDefault(body.URL, "/approvals"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleApprovalApprove(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, s.deps.Approvals.Approve)
}

func (s *Server) handleApprovalReject(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, s.deps.Approvals.Reject)
}

func (s *Server) handleApprovalCancel(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, s.deps.Approvals.Cancel)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request,
	fn func(ctx context.Context, in approval.DecideInput) (*types.ApprovalTicket, error)) {
	p, subj, _, err := s.authorize(r, "approval:read", "/approvals")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Approvals == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Comment string `json:"comment"`
		URL     string `json:"url"`
	}
	_ = decodeJSON(r, &body)
	t, err := fn(r.Context(), approval.DecideInput{
		TicketID: r.PathValue("id"), Comment: body.Comment,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: orDefault(body.URL, "/approvals"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// ---------- 定时任务 ----------

func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "job:read", "/approvals"); err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Jobs == nil {
		s.notWired(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.deps.Jobs.Names()})
}

func (s *Server) handleJobRun(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "job:run", "/approvals"); err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Jobs == nil {
		s.notWired(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": s.deps.Jobs.RunOnce(r.Context())})
}

// ---------- 批量导入 ----------

func (s *Server) handleShopBatchImport(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "shop:create", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Rows []shop.BatchImportRow `json:"rows"`
		URL  string                `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	res, err := s.deps.Shop.BatchImport(r.Context(), shop.BatchImportInput{
		Rows: body.Rows, Actor: p2subject(p, subj),
		Terminal: terminalFrom(r), URL: orDefault(body.URL, "/shops"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 部分成功返回 207（多状态）：不能让调用方误以为全部成功
	status := http.StatusOK
	if res.Failed > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, res)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
