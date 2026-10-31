package gateway

import (
	"context"
	"net/http"
	"strings"

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
	m.HandleFunc("GET /api/v1/shops/batch/template", s.authed(s.handleShopBatchTemplate))
	m.HandleFunc("POST /api/v1/shops/batch/delete", s.authed(s.handleShopBatchDelete))
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
	var (
		rows        []shop.BatchImportRow
		failures    []shop.BatchFailure
		sourceTotal int
		url         = "/shops"
	)
	// 双通道：multipart 上传 CSV（Excel 导出的表格），或既有的 JSON rows。
	// 真正的客户数据是表格，只支持 JSON 等于让他们先找个程序员把表格转成 JSON。
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		file, _, err := r.FormFile("file")
		if err != nil {
			writeErr(w, apierr.Validation("缺少上传文件（表单字段名应为 file）"))
			return
		}
		defer func() { _ = file.Close() }()
		maxRows := shop.MaxImportRows
		if s.deps.Config != nil && s.deps.Config.Limits.ExportMaxRows > 0 {
			maxRows = s.deps.Config.Limits.ExportMaxRows
		}
		rows, failures, err = shop.ParseImportCSV(file, maxRows)
		if err != nil {
			writeErr(w, err)
			return
		}
		if v := strings.TrimSpace(r.FormValue("url")); v != "" {
			url = v
		}
	} else {
		var body struct {
			Rows []shop.BatchImportRow `json:"rows"`
			URL  string                `json:"url"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeErr(w, apierr.Validation("请求体格式错误"))
			return
		}
		rows, url = body.Rows, orDefault(body.URL, "/shops")
	}
	sourceTotal = len(rows) + len(failures)

	// dry-run：只做格式与自检，不落任何数据（大批量操作前先看一眼能不能过）
	if queryBool(r, "dry_run") {
		writeJSON(w, http.StatusOK, &shop.BatchImportResult{
			Total: sourceTotal, Created: 0, Failed: len(failures), Failures: failures,
		})
		return
	}

	res, err := s.deps.Shop.BatchImport(r.Context(), shop.BatchImportInput{
		Rows: rows, SourceTotal: sourceTotal, Actor: p2subject(p, subj),
		Terminal: terminalFrom(r), URL: url,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// CSV 解析阶段就发现的行级问题必须合并回来，否则用户会以为那些行被静默丢掉了
	res.Failures = append(res.Failures, failures...)
	res.Failed += len(failures)
	if res.Total < sourceTotal {
		res.Total = sourceTotal
	}
	// 部分成功返回 207（多状态）：不能让调用方误以为全部成功
	status := http.StatusOK
	if res.Failed > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, res)
}

// handleShopBatchTemplate 下发 CSV 模板：让用户按形状填，而不是先读文档猜列名。
func (s *Server) handleShopBatchTemplate(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="shops_template.csv"`)
	// UTF-8 BOM：Windows Excel 没它就默认按本地编码打开，中文列名会变乱码
	_, _ = w.Write([]byte("\ufeff"))
	_, _ = w.Write([]byte(shop.CSVTemplate()))
}

// ---------- 批量删除 ----------

// handleShopBatchDelete 批量删除：必须先有一张已通过的 shop.bulk_delete 审批单。
//
// 之前的真实缺口：路由规则、待办通知都写好了，但没有任何业务接口去 Verify
// 这类单据，审批链路等于只有演示视频、没有接通电源。这里把闸门合上——
// 「不可逆的行为必须有事前授权」才算真的落地。
func (s *Server) handleShopBatchDelete(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "shop:delete", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		IDs []string `json:"ids"`
		URL string   `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	if len(body.IDs) == 0 {
		writeErr(w, apierr.Validation("待删除店铺列表为空"))
		return
	}
	// 不可逆 → 必须持证（反例 8：按钮置灰不是控制）
	ticketID := r.URL.Query().Get("ticket")
	if s.deps.Approvals != nil && s.deps.Approvals.Required(types.ApprovalTypeBulkDelete) {
		ticket, err := s.deps.Approvals.Verify(r.Context(), p.TenantID, ticketID,
			types.ApprovalTypeBulkDelete, subj.UserID)
		if err != nil {
			writeErr(w, err)
			return
		}
		ticketID = ticket.ID
	}
	res, err := s.deps.Shop.BatchDelete(r.Context(), shop.BatchDeleteInput{
		IDs: body.IDs, ApprovalID: ticketID,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: orDefault(body.URL, "/shops"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 一次性核销：一张审批单只能执行一次批量删除，防止持一张单反复清库。
	if s.deps.Approvals != nil && ticketID != "" {
		_ = s.deps.Approvals.Consume(r.Context(), p.TenantID, ticketID, p2subject(p, subj), terminalFrom(r))
	}
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
