package gateway

import (
	"net/http"
	"time"

	"ziniao/browser"
	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/shop"
)

// routesEnvironments M2 环境聚合视图 + M3 的 start/stop 生命周期闭环。
//
// 「/environments/runtimes」与「/environments/{id}」同时注册是安全的：
// net/http 的路由按字面量优先，字面段多于通配段，不会互相抢。
func (s *Server) routesEnvironments() {
	m := s.mux
	m.HandleFunc("GET /api/v1/environments", s.authed(s.handleEnvList))
	m.HandleFunc("POST /api/v1/environments", s.authed(s.handleEnvCreateShop))
	m.HandleFunc("GET /api/v1/environments/runtimes", s.authed(s.handleEnvRuntimes))
	// M3：本机进程态（PID / CDP 端口 / 运行时长），与上面那份「数据库侧事实」互补。
	m.HandleFunc("GET /api/v1/environments/local-runtime", s.authed(s.handleEnvLocalRuntime))
	m.HandleFunc("GET /api/v1/environments/{id}", s.authed(s.handleEnvView))
	m.HandleFunc("PUT /api/v1/environments/{id}/proxy", s.authed(s.handleEnvBindProxy))
	// M3：真实拉起/关闭浏览器窗口。
	m.HandleFunc("POST /api/v1/environments/{id}/start", s.authed(s.handleEnvStart))
	m.HandleFunc("POST /api/v1/environments/{id}/stop", s.authed(s.handleEnvStop))
}

// ---------- GET /api/v1/environments ----------

// handleEnvList 环境聚合列表（跨店铺）。
//
// 过滤参数与 docs/design-m1-api.md §4.6 对齐，并补了 M2 批复里的 platform / keyword：
//
//	?group_id=&platform=&status=&country=&keyword=&limit=&offset=
func (s *Server) handleEnvList(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:read", "/environments")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	limit := queryInt(r, "limit", 50)
	if limit <= 0 || limit > 200 {
		writeErr(w, apierr.Validation("limit 需在 1-200 之间"))
		return
	}
	list, total, err := s.deps.Environments.ListViews(r.Context(), p2subject(p, subj), shop.EnvListFilter{
		GroupID:  r.URL.Query().Get("group_id"),
		Platform: r.URL.Query().Get("platform"),
		Status:   r.URL.Query().Get("status"),
		Country:  r.URL.Query().Get("country"),
		Keyword:  r.URL.Query().Get("keyword"),
		Limit:    limit, Offset: queryInt(r, "offset", 0),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": list, "total": total, "limit": limit, "offset": queryInt(r, "offset", 0),
		"next_offset": nextOffset(queryInt(r, "offset", 0), limit, total),
	})
}

// ---------- POST /api/v1/environments ----------

// handleEnvCreateShop 原子创建店铺 + 环境 + profile 元数据。
//
// 本期**不拉起 Chrome**（M2 边界）：只写库与初始化 profile 元数据，
// 真实启动是 M3 的 POST /environments/{id}/start。
//
// 权限按 docs/design-m1-api.md §9.1：一次性建店铺 + 环境，故 shop:create 与 env:create 缺一不可
// （此前只挂 env:create，与设计记录不一致，M2 终审第 3.4 项复核时修正）。
func (s *Server) handleEnvCreateShop(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorizeAll(r, "/environments", "shop:create", "env:create")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Name            string `json:"name"`
		Platform        string `json:"platform"`
		Site            string `json:"site"`
		Code            string `json:"code"`
		GroupID         string `json:"group_id"`
		Country         string `json:"country"`
		OS              string `json:"os"`
		StartURL        string `json:"start_url"`
		ProxyID         string `json:"proxy_id"`
		DeviceID        string `json:"device_id"`
		Status          string `json:"status"`
		FingerprintMode string `json:"fingerprint_mode"`
		URL             string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	res, err := s.deps.Environments.CreateShopEnv(r.Context(), shop.CreateShopEnvInput{
		Name: body.Name, Platform: body.Platform, Site: body.Site, Code: body.Code,
		GroupID: body.GroupID, Country: body.Country, OSFamily: body.OS,
		StartURL: body.StartURL, ProxyID: body.ProxyID, DeviceID: body.DeviceID,
		Status: body.Status, FingerprintMode: body.FingerprintMode,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// ---------- GET /api/v1/environments/{id} ----------

func (s *Server) handleEnvView(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:read", "/environments")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	v, err := s.deps.Environments.GetView(r.Context(), p2subject(p, subj), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---------- PUT /api/v1/environments/{id}/proxy ----------

// handleEnvBindProxy 绑定 / 解绑手工代理（proxy_id 为空 = 解绑，回落到代理商池）。
//
// 响应只有 proxy_id 与 exit_mode：**不含任何凭据**（凭据不进响应是 L4 的硬要求）。
func (s *Server) handleEnvBindProxy(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:update", "/environments")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	var body struct {
		ProxyID string `json:"proxy_id"`
		URL     string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	res, err := s.deps.Environments.BindProxy(r.Context(), shop.BindProxyInput{
		EnvID: r.PathValue("id"), ProxyID: body.ProxyID,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- POST /api/v1/environments/{id}/start（M3） ----------

// handleEnvStart 拉起该环境的浏览器窗口。
//
// 权限 env:launch（与旧 launch 接口同口径）。启动成功后状态才变 running：
// 失败会复位成 idle，绝不留下「库里 running、本机没窗口」的悬空态。
//
// 响应**不含**任何代理地址/账号/口令：那些只活在 Go 进程内存里（L4）。
func (s *Server) handleEnvStart(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:launch", "/environments")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Browser == nil {
		s.notWired(w)
		return
	}
	var body struct {
		OpenURL string `json:"open_url"`
		URL     string `json:"url"`
	}
	_ = decodeJSON(r, &body)
	envID := r.PathValue("id")
	shopID, err := s.shopIDOfEnv(r, p.TenantID, envID)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.deps.Browser.Start(r.Context(), browser.StartInput{
		EnvID: envID, ShopID: shopID, OpenURL: body.OpenURL,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- POST /api/v1/environments/{id}/stop（M3） ----------

// handleEnvStop 关闭该环境的浏览器窗口并把状态收回 idle。
//
// 幂等：本机没有会话（换机器 / 启动器重启过）时仍会收回业务状态——
// 否则运营永远关不掉一个「库里说在跑、本机没有」的环境。
func (s *Server) handleEnvStop(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:stop", "/environments")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Browser == nil {
		s.notWired(w)
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	_ = decodeJSON(r, &body)
	envID := r.PathValue("id")
	shopID, err := s.shopIDOfEnv(r, p.TenantID, envID)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.deps.Browser.Stop(r.Context(), browser.StopInput{
		EnvID: envID, ShopID: shopID,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// shopIDOfEnv 环境 ID → 店铺 ID（start/stop 需要它做业务侧的数据级收口）。
func (s *Server) shopIDOfEnv(r *http.Request, tenantID, envID string) (string, error) {
	if envID == "" {
		return "", apierr.Validation("环境 ID 为空")
	}
	env, err := s.deps.Store.Environments().Get(store.WithTenant(r.Context(), tenantID), envID)
	if err != nil {
		return "", err
	}
	if env == nil {
		return "", apierr.NotFound("环境不存在")
	}
	return env.ShopID, nil
}

// ---------- GET /api/v1/environments/runtimes ----------

// handleEnvRuntimes 设备与网络只读台账（M2）。
//
// 只给**数据库侧**事实：环境、代理绑定、内核与基线。
// 不展示本机 PID / CDP 端口 / 运行时长——M2 不拉起浏览器，这些字段现在没有真值，
// 用占位显示等于让运营把假数据当成运行态。
func (s *Server) handleEnvRuntimes(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:read", "/environments")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	limit := queryInt(r, "limit", 50)
	if limit <= 0 || limit > 200 {
		writeErr(w, apierr.Validation("limit 需在 1-200 之间"))
		return
	}
	offset := queryInt(r, "offset", 0)
	list, total, err := s.deps.Environments.RuntimeLedger(r.Context(), p2subject(p, subj), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": list, "total": total, "limit": limit, "offset": offset,
		"next_offset": nextOffset(offset, limit, total),
		// 明确告诉前端「这一份没有进程态」，避免后面有人拿空 pid 当「没在跑」。
		"process_state": "unavailable",
	})
}

// ---------- GET /api/v1/environments/local-runtime（M3） ----------

// handleEnvLocalRuntime 本机运行时（PID / CDP 端口 / 本地代理端口 / 运行时长）。
//
// 与 /runtimes 的区别：那份是**数据库侧事实**（可跨机器、可审计），这份是
// **本机这一刻的进程态**（换机器即失效）。前端必须按 env_id 合并两份，
// 且以接口返回为准——不做乐观更新，否则「点了启动」和「真的起来了」会被混为一谈。
//
// 同样不含任何凭据：这份页面会被截图发到群里排障。
func (s *Server) handleEnvLocalRuntime(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "env:read", "/environments"); err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Browser == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "process_state": "unavailable",
		})
		return
	}
	items := make([]map[string]any, 0)
	for _, e := range s.deps.Browser.Runtime() {
		uptime := int64(0)
		if !e.StartedAt.IsZero() {
			uptime = int64(time.Since(e.StartedAt).Seconds())
		}
		items = append(items, map[string]any{
			"env_id": e.EnvID, "shop_id": e.ShopID, "pid": e.PID,
			"cdp_port": e.CDPPort, "local_proxy_port": e.LocalProxyPort,
			"exit_mode": e.ExitMode, "proc_state": e.ProcState,
			"started_at": e.StartedAt, "uptime_seconds": uptime,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "process_state": "available",
	})
}

// nextOffset 下一页偏移；已到末页返回 0（前端据此停用「下一页」）。
func nextOffset(offset, limit, total int) int {
	if offset+limit >= total {
		return 0
	}
	return offset + limit
}
