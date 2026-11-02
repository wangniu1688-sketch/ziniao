// Package gateway API 网关：鉴权、租户隔离、限流、路由（L6 第一层权限校验）。
package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"ziniao/approval"
	"ziniao/audit"
	"ziniao/billing"
	"ziniao/browser"
	"ziniao/delegation"
	"ziniao/device"
	"ziniao/eventbus"
	"ziniao/fingerprint"
	"ziniao/iam"
	"ziniao/identity"
	"ziniao/jobs"
	"ziniao/notify"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/ids"
	"ziniao/platform/openapi"
	"ziniao/platform/store"
	"ziniao/security"
	"ziniao/shop"
	"ziniao/tenant"
)

// Deps 网关依赖。
type Deps struct {
	Store       store.Store
	Bus         *eventbus.Bus
	Config      *config.Config
	IAM         *iam.Engine
	Identity    *identity.Service
	Tenant      *tenant.Service
	Shop        *shop.Service
	Audit       *audit.Service
	Notify      *notify.Service
	Entitlement *billing.Entitlement
	Log         *slog.Logger

	// Phase 1：设备 / 环境 / 凭证托管 / 安全中心 / 指纹
	Device       *device.Service
	Security     *security.Service
	Environments *shop.EnvService
	Vault        *shop.Vault
	Fingerprints *fingerprint.Library
	Baselines    *fingerprint.BaselineLibrary

	// Phase 2：审批工单与定时任务
	Approvals *approval.Service
	Jobs      *jobs.Scheduler

	// Phase 2：代理机构委托审批（approval_flows）
	Delegations *delegation.Service

	// Phase 2C：通知 Sink（外发偏好需要它：偏好的读取与校验都在这里，
	// 网关只负责搬运自助数据，不重复实现一遍通道规则）
	NotifySink *notify.Sink

	// M1：启动器内核（本机浏览器进程编排 + 运行时注册表 + CA 就绪状态）。
	// 为 nil 时 /api/v1/system/info 的 browser 段如实给出 installed=false。
	Browser *browser.Manager

	// M2 启动器内核（业务侧）：手工代理台账与店铺分组。
	// 放在 Deps 里而不是由网关自己拼仓储：代理口令的加解密只在 ProxyService 里发生，
	// 网关层永远拿不到明文。
	Proxies *shop.ProxyService
	Groups  *shop.GroupService

	// DemoSite 是否挂载账号隔离实测站点 /demo/*（cfg.DemoSite 控制，生产必须为 false）
	DemoSite bool
}

// Server HTTP 网关。
type Server struct {
	deps  Deps
	mux   *http.ServeMux
	log   *slog.Logger
	lim   *rateLimiter
	demoS *DemoStore
	// startedAt 进程启动时刻（uptime_sec 用；它是「本机启动器跑了多久」，
	// 不是业务指标，因此不需要落库）。
	startedAt time.Time
}

// New 构造网关并注册全部路由。
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{
		deps: d, mux: http.NewServeMux(), log: d.Log,
		lim: newRateLimiter(d.Config.Limits.APIRatePerMinute), demoS: newDemoStore(),
		startedAt: time.Now(),
	}
	if d.DemoSite {
		d.Log.Warn("已挂载 demo 隔离实测站点 /demo/*（禁止用于生产环境）")
		demoRoutes(s.mux, s.demoS)
	}
	s.routes()
	s.docs()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ---------- 路由 ----------

func (s *Server) routes() {
	m := s.mux
	s.webRoutes() // Web 控制台（/ 与 /static/）
	// 公开
	m.HandleFunc("GET /healthz", s.handleHealth)
	m.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	m.HandleFunc("POST /api/v1/auth/login", s.handleLogin)

	// 需认证
	m.HandleFunc("POST /api/v1/auth/logout", s.authed(s.handleLogout))
	m.HandleFunc("GET /api/v1/me", s.authed(s.handleMe))

	m.HandleFunc("GET /api/v1/tenants/{id}", s.authed(s.handleTenantGet))
	m.HandleFunc("GET /api/v1/tenants/{id}/members", s.authed(s.handleMemberList))
	m.HandleFunc("POST /api/v1/tenants/{id}/members", s.authed(s.handleMemberInvite))
	m.HandleFunc("POST /api/v1/tenants/{id}/members/{userID}/roles", s.authed(s.handleGrantRole))

	m.HandleFunc("GET /api/v1/iam/roles", s.authed(s.handleRoleList))
	m.HandleFunc("GET /api/v1/iam/permissions", s.authed(s.handlePermissionCatalog))
	m.HandleFunc("POST /api/v1/iam/check", s.authed(s.handlePolicyCheck))
	m.HandleFunc("GET /api/v1/iam/dom-policy", s.authed(s.handleDOMPolicy))

	m.HandleFunc("POST /api/v1/shops", s.authed(s.handleShopCreate))
	m.HandleFunc("GET /api/v1/shops", s.authed(s.handleShopList))
	m.HandleFunc("GET /api/v1/shops/{id}", s.authed(s.handleShopGet))
	m.HandleFunc("DELETE /api/v1/shops/{id}", s.authed(s.handleShopDelete))

	m.HandleFunc("GET /api/v1/audit/logs", s.authed(s.handleAuditList))
	m.HandleFunc("GET /api/v1/audit/logs/export", s.authed(s.handleAuditExport))
	m.HandleFunc("GET /api/v1/audit/verify", s.authed(s.handleAuditVerify))

	m.HandleFunc("GET /api/v1/notifications", s.authed(s.handleNotifyList))
	m.HandleFunc("POST /api/v1/notifications/{id}/read", s.authed(s.handleNotifyRead))
	m.HandleFunc("GET /api/v1/notifications/preferences", s.authed(s.handleNotifyPrefsGet))
	m.HandleFunc("PUT /api/v1/notifications/preferences", s.authed(s.handleNotifyPrefsPut))

	s.routesPhase1()
	s.routesDelegation()
	s.routesEgress()
	s.routesSystem()
	// M2：代理台账 / 分组 / 环境聚合视图 / 聚合日志
	s.routesProxies()
	s.routesGroups()
	s.routesEnvironments()
	s.routesLogs()
}

// ---------- 中间件 ----------

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			writeErr(w, apierr.Unauthorized("缺少访问令牌"))
			return
		}
		principal, err := s.deps.Identity.Authenticate(ctx, token)
		if err != nil {
			writeErr(w, err)
			return
		}
		if !s.lim.allow(principal.UserID) {
			writeErr(w, apierr.New(apierr.CodeRateLimited, http.StatusTooManyRequests, "请求过于频繁"))
			return
		}
		// 租户上下文：全链路携带，仓储层强制校验（L12）
		ctx = store.WithTenant(ctx, principal.TenantID)
		ctx = withPrincipal(ctx, principal)
		next(w, r.WithContext(ctx))
	}
}

type ctxKeyPrincipal struct{}

func withPrincipal(ctx context.Context, p *identity.Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal{}, p)
}

// principalFrom 取认证主体。
func principalFrom(ctx context.Context) (*identity.Principal, bool) {
	p, ok := ctx.Value(ctxKeyPrincipal{}).(*identity.Principal)
	return p, ok
}

// authorize 统一鉴权：网关（第一层）+ 服务层 PolicyEngine（第二层）在此衔接。
func (s *Server) authorize(r *http.Request, action, path string) (*identity.Principal, iam.Subject, iam.Decision, error) {
	ctx := r.Context()
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, iam.Subject{}, iam.Decision{}, apierr.Unauthorized("未认证")
	}
	subj, err := s.deps.Identity.Subject(ctx, p)
	if err != nil {
		return nil, subj, iam.Decision{}, err
	}
	d := s.deps.IAM.Check(ctx, iam.Request{Subject: subj, Action: action, Path: iam.NormalizePath(path)})
	if !d.Allow {
		return p, subj, d, apierr.Forbidden("无权执行该操作（" + string(d.Layer) + "）")
	}
	return p, subj, d, nil
}

// authorizeAll 多权限码联合裁决：全部通过才放行（如「建环境」既要能建店也要能建环境）。
// 任一码缺失即 403，拒绝层级沿用第一个被拒的码，便于审计定位。
func (s *Server) authorizeAll(r *http.Request, path string, actions ...string) (*identity.Principal, iam.Subject, iam.Decision, error) {
	var last iam.Decision
	for _, a := range actions {
		p, subj, d, err := s.authorize(r, a, path)
		if err != nil {
			return p, subj, d, err
		}
		last = d
	}
	p, ok := principalFrom(r.Context())
	if !ok {
		return nil, iam.Subject{}, iam.Decision{}, apierr.Unauthorized("未认证")
	}
	subj, err := s.deps.Identity.Subject(r.Context(), p)
	if err != nil {
		return p, subj, iam.Decision{}, err
	}
	return p, subj, last, nil
}

// terminalFrom 从请求提取终端信息（L5：一条操作要能追到终端与 IP）。
func terminalFrom(r *http.Request) eventbus.Terminal {
	ip := r.Header.Get("X-Forwarded-For")
	if ip == "" {
		ip = r.RemoteAddr
	}
	if i := strings.LastIndex(ip, ":"); i > 0 && !strings.Contains(ip, "]") {
		ip = ip[:i]
	}
	mode := r.Header.Get("X-Client-Mode")
	if mode == "" {
		mode = "api"
	}
	return eventbus.Terminal{
		IP:          ip,
		UserAgent:   r.UserAgent(),
		Fingerprint: r.Header.Get("X-Terminal-FP"),
		ClientVer:   r.Header.Get("X-Client-Ver"),
		Mode:        mode,
		TraceID:     r.Header.Get("X-Trace-ID"),
	}
}

// ---------- 响应辅助 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	openapi.WriteJSON(w, status, v)
}

func writeErr(w http.ResponseWriter, err error) {
	var e *apierr.Error
	if !asError(err, &e) {
		e = apierr.Internal(err.Error())
	}
	// L4：错误信息不得包含敏感数据
	openapi.WriteJSON(w, e.HTTP, map[string]any{
		"code": e.Code, "message": e.Message,
		"request_id": ids.New()[:10],
	})
}

// rateLimiter 简单令牌桶（P1 换成 Redis 分布式限流）。
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	buckets map[string]*bucket
}

type bucket struct {
	tokens int
	last   time.Time
}

func newRateLimiter(limit int) *rateLimiter {
	if limit <= 0 {
		limit = 600
	}
	return &rateLimiter{limit: limit, buckets: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &bucket{tokens: l.limit - 1, last: now}
		return true
	}
	refill := int(now.Sub(b.last).Minutes() * float64(l.limit))
	if refill > 0 {
		b.tokens = min(l.limit, b.tokens+refill)
		b.last = now
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
