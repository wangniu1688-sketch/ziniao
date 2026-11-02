package gateway

import (
	"net/http"
	"strings"

	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/shop"
)

// routesProxies M2 手工代理台账。
func (s *Server) routesProxies() {
	m := s.mux
	m.HandleFunc("GET /api/v1/proxies", s.authed(s.handleProxyList))
	m.HandleFunc("GET /api/v1/proxies/{id}", s.authed(s.handleProxyGet))
	m.HandleFunc("POST /api/v1/proxies", s.authed(s.handleProxyCreate))
	m.HandleFunc("PUT /api/v1/proxies/{id}", s.authed(s.handleProxyUpdate))
	m.HandleFunc("DELETE /api/v1/proxies/{id}", s.authed(s.handleProxyDelete))
	m.HandleFunc("POST /api/v1/proxies/{id}/test", s.authed(s.handleProxyTest))
}

// ---------- GET /api/v1/proxies ----------

func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "proxy:read", "/proxies")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Proxies == nil {
		s.notWired(w)
		return
	}
	list, total, err := s.deps.Proxies.List(r.Context(), p2subject(p, subj), store.ProxyFilter{
		Status:      r.URL.Query().Get("status"),
		CheckStatus: r.URL.Query().Get("check_status"),
		Keyword:     r.URL.Query().Get("keyword"),
		Limit:       queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": total})
}

// ---------- GET /api/v1/proxies/{id} ----------

// handleProxyGet 单条代理详情（P3）。
//
// 只有详情才带 password_mask（掩码，不是明文）：列表一次 200 条意味着
// 200 次信封解密，为一个「认得出是不是刚改过」的字段不值得。
func (s *Server) handleProxyGet(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "proxy:read", "/proxies")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Proxies == nil {
		s.notWired(w)
		return
	}
	v, err := s.deps.Proxies.Get(r.Context(), p2subject(p, subj), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---------- POST /api/v1/proxies ----------

// handleProxyCreate 新建代理。
//
// password **只进不出**：入参里的明文本次请求内被信封加密落库，
// 响应只有 has_password 布尔——前端拿不到明文，也就无从把它渲染进页面或 localStorage。
func (s *Server) handleProxyCreate(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "proxy:manage", "/proxies")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Proxies == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Name     string `json:"name"`
		Protocol string `json:"protocol"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		Country  string `json:"country"`
		City     string `json:"city"`
		Type     string `json:"type"`
		Note     string `json:"note"`
		Status   string `json:"status"`
		URL      string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	v, err := s.deps.Proxies.Create(r.Context(), shop.ProxyCreateInput{
		Name: body.Name, Protocol: body.Protocol, Host: body.Host, Port: body.Port,
		Username: body.Username, Password: body.Password, Country: body.Country,
		City: body.City, Type: body.Type,
		Note: body.Note, Status: body.Status,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// ---------- PUT /api/v1/proxies/{id} ----------

func (s *Server) handleProxyUpdate(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "proxy:manage", "/proxies")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Proxies == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Name     string  `json:"name"`
		Protocol string  `json:"protocol"`
		Host     string  `json:"host"`
		Port     int     `json:"port"`
		Username string  `json:"username"`
		Password *string `json:"password"` // nil=不改 / ""=清空 / 非空=新口令
		Country  string  `json:"country"`
		City     string  `json:"city"`
		Type     string  `json:"type"`
		Note     string  `json:"note"`
		Status   string  `json:"status"`
		URL      string  `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	v, err := s.deps.Proxies.Update(r.Context(), shop.ProxyUpdateInput{
		ID:   r.PathValue("id"),
		Name: body.Name, Protocol: body.Protocol, Host: body.Host, Port: body.Port,
		Username: body.Username, Password: body.Password, Country: body.Country,
		City: body.City, Type: body.Type,
		Note: body.Note, Status: body.Status,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---------- DELETE /api/v1/proxies/{id} ----------

func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "proxy:manage", "/proxies")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Proxies == nil {
		s.notWired(w)
		return
	}
	if err := s.deps.Proxies.Delete(r.Context(), p2subject(p, subj), r.PathValue("id"),
		terminalFrom(r), "/proxies"); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "deleted": true})
}

// ---------- POST /api/v1/proxies/{id}/test ----------

// handleProxyTest 服务端连通性拨测。
//
// 拨测失败返回 200 + ok=false：代理不通是**结论**，不是接口错误；
// 用 500 表达会让前端把「代理不通」和「服务挂了」混成同一种提示。
func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "proxy:manage", "/proxies")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Proxies == nil {
		s.notWired(w)
		return
	}
	var body struct {
		TimeoutMS int    `json:"timeout_ms"`
		Target    string `json:"target"`
	}
	_ = decodeJSON(r, &body)
	if body.TimeoutMS < 0 || body.TimeoutMS > 15000 {
		writeErr(w, apierr.Validation("timeout_ms 需在 1-15000 之间"))
		return
	}
	// 目标地址以配置中心为默认值：离线 / 内网部署把 ZINIAO_PROXY_TEST_TARGET
	// 指向内网回显服务即可；请求体仍可逐次覆盖，服务端再做协议白名单校验。
	target := strings.TrimSpace(body.Target)
	if target == "" {
		target = s.deps.Config.ProxyTestTarget
	}
	// 出网总开关（T12 / P3）：off 时不进服务层，密文不解密、代理不连接、目标不请求。
	// 路由仍保留并明确返回「拨测已禁用」，而不是 404 让人以为是接口没部署；
	// 拨测失败一律走 200 + ok=false 的既有口径。
	//
	// P3 关键：off 必须带 skipped=true。off 是「部署侧摘掉了这条出网路径」，
	// 不是「代理不通」——没有这个标记，前端与绑定校验会把「没验证过」
	// 当成「验证过且失败」，内网部署就永远绑不上代理。
	if !s.deps.Config.ProxyTestEnabled {
		writeJSON(w, http.StatusOK, shop.ProxyTestResult{
			Target: target, Skipped: true, Error: "拨测已禁用（ZINIAO_PROXY_TEST=off）",
		})
		return
	}
	res, err := s.deps.Proxies.Test(r.Context(), shop.ProxyTestInput{
		ProxyID: r.PathValue("id"), TimeoutMS: body.TimeoutMS, Target: target,
		CountryTarget: s.deps.Config.ProxyTestCountryTarget,
		Actor:         p2subject(p, subj),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
