package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"ziniao/device"
	"ziniao/fingerprint"
	"ziniao/iam"
	"ziniao/identity"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/store"
	"ziniao/platform/types"
	"ziniao/security"
	"ziniao/shop"
)

// routesPhase1 Phase 1 路由：设备 / 店铺环境 / 账密托管 / 安全中心 / 指纹基线。
func (s *Server) routesPhase1() {
	m := s.mux
	m.HandleFunc("GET /api/v1/devices", s.authed(s.handleDeviceList))
	m.HandleFunc("POST /api/v1/devices/purchase", s.authed(s.handleDevicePurchase))
	m.HandleFunc("POST /api/v1/devices/import", s.authed(s.handleDeviceImport))
	m.HandleFunc("POST /api/v1/devices/{id}/bind", s.authed(s.handleDeviceBind))
	m.HandleFunc("POST /api/v1/devices/{id}/renew", s.authed(s.handleDeviceRenew))
	m.HandleFunc("POST /api/v1/devices/{id}/acquire", s.authed(s.handleDeviceAcquire))
	m.HandleFunc("POST /api/v1/devices/{id}/release", s.authed(s.handleDeviceRelease))
	m.HandleFunc("GET /api/v1/devices/{id}/health", s.authed(s.handleDeviceHealth))

	m.HandleFunc("GET /api/v1/shops/trash", s.authed(s.handleShopTrash))
	m.HandleFunc("POST /api/v1/shops/{id}/restore", s.authed(s.handleShopRestore))
	m.HandleFunc("POST /api/v1/shops/{id}/status", s.authed(s.handleShopTransition))
	m.HandleFunc("PUT /api/v1/shops/{id}/group", s.authed(s.handleShopGroup))
	m.HandleFunc("POST /api/v1/shops/{id}/environment", s.authed(s.handleEnvCreate))
	m.HandleFunc("GET /api/v1/shops/{id}/environment", s.authed(s.handleEnvGet))
	m.HandleFunc("POST /api/v1/shops/{id}/environment/check", s.authed(s.handleEnvCheck))
	m.HandleFunc("POST /api/v1/shops/{id}/environment/launch", s.authed(s.handleEnvLaunch))

	m.HandleFunc("PUT /api/v1/shops/{id}/credentials", s.authed(s.handleCredPut))
	m.HandleFunc("GET /api/v1/shops/{id}/credentials", s.authed(s.handleCredList))
	m.HandleFunc("GET /api/v1/shops/{id}/credentials/{kind}", s.authed(s.handleCredReveal))

	m.HandleFunc("GET /api/v1/security/policies", s.authed(s.handlePolicyList))
	m.HandleFunc("POST /api/v1/security/policies", s.authed(s.handlePolicyUpsert))
	m.HandleFunc("PUT /api/v1/security/policies/{id}", s.authed(s.handlePolicyUpsert))
	m.HandleFunc("DELETE /api/v1/security/policies/{id}", s.authed(s.handlePolicyDelete))
	m.HandleFunc("GET /api/v1/security/events", s.authed(s.handleSecurityEvents))
	m.HandleFunc("POST /api/v1/security/intercept", s.authed(s.handleIntercept))
	m.HandleFunc("GET /api/v1/security/templates", s.authed(s.handlePolicyTemplates))

	m.HandleFunc("GET /api/v1/fingerprint/tls-baselines", s.authed(s.handleTLSBaselines))
	m.HandleFunc("GET /api/v1/fingerprint/layers", s.authed(s.handleFingerprintLayers))

	s.routesPhase2()
}

// notWired Phase 1 依赖未装配（如仅跑 Phase 0 的最小装配）。
func (s *Server) notWired(w http.ResponseWriter) {
	writeErr(w, apierr.Internal("该能力未装配（Phase 1 依赖缺失）"))
}

// p2subject 取裁决用的身份主体（authorize 已按 token 还原出发起人身份，L3）。
func p2subject(_ *identity.Principal, subj iam.Subject) iam.Subject { return subj }

// ---------- 设备 ----------

func (s *Server) handleDeviceList(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "device:read", "/devices")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Device == nil {
		s.notWired(w)
		return
	}
	f := store.DeviceFilter{
		Type:    types.DeviceType(r.URL.Query().Get("type")),
		Status:  types.DeviceStatus(r.URL.Query().Get("status")),
		Country: r.URL.Query().Get("country"),
		Limit:   queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	}
	list, total, err := s.deps.Device.List(r.Context(), p.TenantID, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": total})
}

// purchaseAmountCNY 本次申购金额（元）＝套餐单价 × 月数 × 台数。
// 由服务端计算，避免客户端谎报金额绕过阈值。取不到套餐时返回 0（不卡申购）。
func (s *Server) purchaseAmountCNY(ctx context.Context, tenantID string, months, count int) float64 {
	if s.deps.Entitlement == nil {
		return 0
	}
	return device.PurchaseAmountCNY(ctx, s.deps.Entitlement, tenantID, months, count)
}

// purchaseNeedsApproval 本次申购是否需要持证：金额达到审批阈值即需要。
//
// 这里刻意区分金额：小额零星申购不该走审批，否则日常操作被流程拖垮，
// 最后的结果一定是大家想方设法绕过流程。阈值本身是可配置项。
func (s *Server) purchaseNeedsApproval(ctx context.Context, tenantID string, months, count int) bool {
	threshold := config.Default().Limits.PurchaseOwnerThresholdCNY()
	if s.deps.Config != nil {
		threshold = s.deps.Config.Limits.PurchaseOwnerThresholdCNY()
	}
	return s.purchaseAmountCNY(ctx, tenantID, months, count) >= threshold
}

// ticketAmountOf 取审批单上的金额（JSON 数字经反序列化为 float64）。
func ticketAmountOf(t *types.ApprovalTicket) float64 {
	if t == nil || t.Detail == nil {
		return 0
	}
	switch v := t.Detail["amount"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func (s *Server) handleDevicePurchase(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "device:purchase", "/devices")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Device == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Country string `json:"country"`
		Region  string `json:"region"`
		City    string `json:"city"`
		Months  int    `json:"months"`
		Count   int    `json:"count"`
		URL     string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	if body.Count < 0 {
		writeErr(w, apierr.Validation("申购台数不能为负"))
		return
	}
	if body.Count == 0 {
		body.Count = 1
	}
	months := body.Months
	if months <= 0 {
		months = 1
	}
	// 大额申购闸门：金额由服务端按套餐单价重算，达到阈值就必须持有一张已经双人
	// 签署过的 device.purchase 审批单。这是会签引擎唯一现实的入口——之前路由规则
	// 写好了却没有任何地方 Verify 它，等于没接电。
	ticketID := r.URL.Query().Get("ticket")
	// 两条都必须走校验：金额达阈值必须持证；此外只要客户端**带了**单据，
	// 这张单就必须是有效可用的——否则拿着一张已核销/已拒绝的单据也能混过去，
	// 让审计里留下一个根本没生效的「审批依据」。
	needsTicket := s.purchaseNeedsApproval(r.Context(), p.TenantID, months, body.Count)
	if s.deps.Approvals != nil && (needsTicket || ticketID != "") {
		ticket, err := s.deps.Approvals.Verify(r.Context(), p.TenantID, ticketID,
			types.ApprovalTypeDevicePurchase, subj.UserID)
		if err != nil {
			writeErr(w, err)
			return
		}
		// 防「拿小额单据买大额」：单据金额必须覆盖本次申购，否则重新开单。
		if needsTicket && ticketAmountOf(ticket) < s.purchaseAmountCNY(r.Context(), p.TenantID, months, body.Count) {
			writeErr(w, apierr.Approval("审批单金额不足以覆盖本次申购额度，请按实际金额重新提交审批单"))
			return
		}
		ticketID = ticket.ID
	}
	d, err := s.deps.Device.Purchase(r.Context(), device.PurchaseInput{
		Name: body.Name, Type: types.DeviceType(body.Type), Country: body.Country,
		Region: body.Region, City: body.City, PeriodMonths: months, Count: body.Count,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Approvals != nil && ticketID != "" {
		_ = s.deps.Approvals.Consume(r.Context(), p.TenantID, ticketID, p2subject(p, subj), terminalFrom(r))
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) handleDeviceImport(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "device:import", "/devices")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Device == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Protocol string `json:"protocol"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		Country  string `json:"country"`
		City     string `json:"city"`
		SN       string `json:"sn"`
		BindCode string `json:"bind_code"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	act := p2subject(p, subj)
	term := terminalFrom(r)
	if body.SN != "" { // 紫鸟盒子
		d, err := s.deps.Device.ImportBox(r.Context(), device.BoxInput{
			SN: body.SN, BindCode: body.BindCode, Name: body.Name,
			Region: body.Country, Actor: act, Terminal: term,
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, d)
		return
	}
	d, err := s.deps.Device.ImportOwn(r.Context(), device.ImportInput{
		Name: body.Name, Type: types.DeviceType(body.Type), Protocol: body.Protocol,
		Host: body.Host, Port: body.Port, Username: body.Username, Password: body.Password,
		Country: body.Country, City: body.City, Actor: act, Terminal: term,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) handleDeviceBind(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "device:bind", "/devices")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Device == nil {
		s.notWired(w)
		return
	}
	var body struct {
		ShopID string `json:"shop_id"`
		URL    string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	b, err := s.deps.Device.Bind(r.Context(), device.BindInput{
		DeviceID: r.PathValue("id"), ShopID: body.ShopID,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleDeviceRenew(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "device:renew", "/devices")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Device == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Months int `json:"months"`
	}
	_ = decodeJSON(r, &body)
	if err := s.deps.Device.Renew(r.Context(), r.PathValue("id"), body.Months, p2subject(p, subj), terminalFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeviceAcquire(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "device:read", "/devices")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Device == nil {
		s.notWired(w)
		return
	}
	var body struct {
		ShopID string `json:"shop_id"`
	}
	_ = decodeJSON(r, &body)
	res, err := s.deps.Device.Acquire(r.Context(), r.PathValue("id"), body.ShopID, p2subject(p, subj), terminalFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeviceRelease(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "device:read", "/devices")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Device == nil {
		s.notWired(w)
		return
	}
	var body struct {
		BindingID string `json:"binding_id"`
	}
	_ = decodeJSON(r, &body)
	if body.BindingID == "" {
		body.BindingID = r.PathValue("id")
	}
	if err := s.deps.Device.Release(r.Context(), body.BindingID, p2subject(p, subj), terminalFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeviceHealth(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "device:read", "/devices")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Device == nil {
		s.notWired(w)
		return
	}
	h, err := s.deps.Device.Health(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// ---------- 店铺生命周期与环境 ----------

func (s *Server) handleShopTransition(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "shop:transition", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		To     string `json:"to"`
		Reason string `json:"reason"`
		URL    string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	sp, err := s.deps.Shop.Transition(r.Context(), shop.TransitionInput{
		ShopID: r.PathValue("id"), To: body.To, Reason: body.Reason,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (s *Server) handleShopGroup(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "shop:update", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Group string   `json:"group"`
		Tags  []string `json:"tags"`
		URL   string   `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	sp, err := s.deps.Shop.SetGroup(r.Context(), shop.SetGroupInput{
		ShopID: r.PathValue("id"), Group: body.Group, Tags: body.Tags,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (s *Server) handleShopTrash(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "shop:read", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	list, total, err := s.deps.Shop.Trash(r.Context(), p.TenantID, queryInt(r, "limit", 50), queryInt(r, "offset", 0))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": total})
}

func (s *Server) handleShopRestore(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "shop:update", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	sp, err := s.deps.Shop.Restore(r.Context(), r.PathValue("id"), p2subject(p, subj), terminalFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (s *Server) handleEnvCreate(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:create", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Country  string `json:"country"`
		OSFamily string `json:"os"`
		DeviceID string `json:"device_id"`
		URL      string `json:"url"`
	}
	_ = decodeJSON(r, &body)
	env, plan, err := s.deps.Environments.CreateEnv(r.Context(), shop.CreateEnvInput{
		ShopID: r.PathValue("id"), Country: body.Country, OSFamily: body.OSFamily,
		DeviceID: body.DeviceID, Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"environment": env,
		// L4：注入计划不含任何凭证；script 供客户端在文档创建前注入
		"plan": map[string]any{
			"env_id": plan.EnvID, "baseline_hash": plan.BaselineHash,
			"kernel_major": plan.KernelMajor, "tls": plan.TLSSpec,
			"script_bytes": len(plan.Script),
		},
	})
}

func (s *Server) handleEnvGet(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "env:check", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	env, err := s.deps.Environments.Get(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// handleEnvLaunch 下发环境启动单：客户端据此开「隔离窗口」（UA + 注入脚本 + 代理端口 + 内核基线）。
// 客户端不得自行随机化任何字段（L2 致命项）；响应不含任何凭证（L4）。
func (s *Server) handleEnvLaunch(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:launch", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	var body struct {
		OpenURL string `json:"open_url"`
	}
	_ = decodeJSON(r, &body)
	spec, err := s.deps.Environments.Launch(r.Context(), shop.LaunchInput{
		ShopID: r.PathValue("id"), OpenURL: body.OpenURL,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: "/shops",
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

func (s *Server) handleEnvCheck(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "env:check", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	var body map[string]string
	_ = decodeJSON(r, &body)
	report, err := s.deps.Environments.Check(r.Context(), p.TenantID, r.PathValue("id"), body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// ---------- 账密托管 ----------

func (s *Server) handleCredPut(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "shop:credential.manage", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Vault == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Kind     string `json:"kind"`
		Username string `json:"username"`
		Secret   string `json:"secret"`
		URL      string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	if err := s.deps.Vault.Put(r.Context(), shop.PutInput{
		ShopID: r.PathValue("id"), Kind: types.CredentialKind(body.Kind),
		Username: body.Username, Secret: body.Secret,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCredList(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "shop:credential.manage", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Vault == nil {
		s.notWired(w)
		return
	}
	kinds, err := s.deps.Vault.ListKinds(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": kinds})
}

// handleCredReveal 查看凭证明文：需 shop:credential.reveal（Approval 义务 + 高危告警）。
func (s *Server) handleCredReveal(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "shop:credential.reveal", "/shops")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Vault == nil {
		s.notWired(w)
		return
	}
	// 凭证明文属于高危动作：必须携带已通过的审批单（反例 8 + L4）
	ticketID := r.URL.Query().Get("ticket")
	if s.deps.Approvals != nil && s.deps.Approvals.Required(types.ApprovalTypeCredReveal) {
		ticket, err := s.deps.Approvals.Verify(r.Context(), p.TenantID, ticketID,
			types.ApprovalTypeCredReveal, subj.UserID)
		if err != nil {
			writeErr(w, err)
			return
		}
		ticketID = ticket.ID
	}
	plain, err := s.deps.Vault.Reveal(r.Context(), shop.RevealInput{
		ShopID: r.PathValue("id"), Kind: types.CredentialKind(r.PathValue("kind")),
		Reason: r.URL.Query().Get("reason"), ApprovalID: ticketID,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: "/shops",
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 一次性核销：一张审批单只能看一次明文
	if s.deps.Approvals != nil && ticketID != "" {
		_ = s.deps.Approvals.Consume(r.Context(), p.TenantID, ticketID, p2subject(p, subj), terminalFrom(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"secret": plain})
}

// ---------- 安全中心 ----------

func (s *Server) handlePolicyList(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "security:policy.read", "/security")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Security == nil {
		s.notWired(w)
		return
	}
	list, err := s.deps.Security.List(r.Context(), p.TenantID, r.URL.Query().Get("scope"), r.URL.Query().Get("scope_id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handlePolicyTemplates(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "security:policy.read", "/security"); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": security.Templates()})
}

func (s *Server) handlePolicyUpsert(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "security:policy.manage", "/security")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Security == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Name     string             `json:"name"`
		Scope    string             `json:"scope"`
		ScopeID  string             `json:"scope_id"`
		Template string             `json:"template"`
		Enabled  *bool              `json:"enabled"`
		Rules    []types.PolicyRule `json:"rules"`
		URL      string             `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	pol, err := s.deps.Security.Upsert(r.Context(), security.UpsertInput{
		ID: r.PathValue("id"), Name: body.Name, Scope: body.Scope, ScopeID: body.ScopeID,
		Template: body.Template, Enabled: enabled, Rules: body.Rules,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (s *Server) handlePolicyDelete(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "security:policy.manage", "/security")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Security == nil {
		s.notWired(w)
		return
	}
	if err := s.deps.Security.Delete(r.Context(), p.TenantID, r.PathValue("id"), p2subject(p, subj), terminalFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSecurityEvents(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "security:event.read", "/security")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Security == nil {
		s.notWired(w)
		return
	}
	f := store.SecurityEventFilter{
		Category: r.URL.Query().Get("category"), Level: r.URL.Query().Get("level"),
		ShopID: r.URL.Query().Get("shop_id"),
		Limit:  queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	}
	list, total, err := s.deps.Security.ListEvents(r.Context(), p.TenantID, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": total})
}

// handleIntercept 事前拦截判定（客户端在访问页面/点击元素/执行动作前调用）。
func (s *Server) handleIntercept(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "shop:read", "/security")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Security == nil {
		s.notWired(w)
		return
	}
	var body struct {
		ShopID  string `json:"shop_id"`
		URL     string `json:"url"`
		Action  string `json:"action"`
		Element string `json:"element"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	res, err := s.deps.Security.Intercept(r.Context(), security.InterceptInput{
		TenantID: p.TenantID, ShopID: body.ShopID, URL: body.URL,
		Action: body.Action, Element: body.Element,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 指纹基线 ----------

func (s *Server) handleTLSBaselines(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "env:check", "/shops"); err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Baselines == nil {
		s.notWired(w)
		return
	}
	items := []any{}
	for _, major := range []int{115, 120, 131} {
		if bl, ok := s.deps.Baselines.For(major); ok {
			items = append(items, bl)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "baseline_hash": s.deps.Baselines.BaselineHash(),
		"computed": s.deps.Baselines.Computed(),
	})
}

func (s *Server) handleFingerprintLayers(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "env:check", "/shops"); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": fingerprint.LayerTable()})
}

// queryBool 解析布尔查询参数：?dry_run=1 / true / yes 均为真。
func queryBool(r *http.Request, key string) bool {
	switch strings.ToLower(r.URL.Query().Get(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
