package shop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/network"
	"ziniao/platform/apierr"
	"ziniao/platform/crypto"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ProxyService 手工静态代理台账（M2）。
//
// 与 devices.proxy_* 的分工：devices 是「有采购/续费/健康分」的网络资产，
// proxies 是运营手工录入的静态出口，只有地址、凭据与启停。
//
// 红线（全模块生效）：**明文口令只存在于这一次调用的栈里**。
// 它从入参进来 → 立刻 Seal 成密文落库 → 栈帧退出即消失；
// 读路径只回 has_password 布尔，密文只在拨测/启动链路里于进程内存解密。
type ProxyService struct {
	st     store.Store
	bus    *eventbus.Bus
	engine *iam.Engine
	cipher *crypto.EnvelopeCipher
}

// NewProxyService 构造代理台账服务。
func NewProxyService(st store.Store, bus *eventbus.Bus, engine *iam.Engine, cipher *crypto.EnvelopeCipher) *ProxyService {
	return &ProxyService{st: st, bus: bus, engine: engine, cipher: cipher}
}

// ProxyView 代理对外视图。
//
// 刻意**不嵌入** types.Proxy：嵌入等于把「以后往 Proxy 加一个字段」变成
// 「自动出现在所有响应里」，那正是凭据外泄最常见的方式。这里逐个字段声明，
// 口令只以 has_password 布尔出现。
type ProxyView struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	Name          string    `json:"name"`
	Protocol      string    `json:"protocol"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
	Username      string    `json:"username,omitempty"`
	HasPassword   bool      `json:"has_password"`
	// PasswordMask 口令掩码（如 p***23）。**只在详情（Get）里带出**：
	//
	// 掩码要由明文算出来，而列表一次 200 条意味着 200 次信封解密——
	// 为一个「好看」的字段把密钥压到这种频率上不值得。列表只用
	// has_password 布尔告诉前端「要不要显示『已设置』」，这已经够用了。
	PasswordMask   string     `json:"password_mask,omitempty"`
	Country        string     `json:"country,omitempty"`
	City           string     `json:"city,omitempty"`
	Type           string     `json:"type,omitempty"`
	Note           string     `json:"note,omitempty"`
	Status         string     `json:"status"`       // active|disabled：启停
	CheckStatus    string     `json:"check_status"` // unknown|ok|fail：拨测健康
	ExitIP         string     `json:"exit_ip,omitempty"`
	ExitCountry    string     `json:"exit_country,omitempty"`
	LatencyMS      *int       `json:"latency_ms,omitempty"`
	LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	KeyVersion     int        `json:"key_version"`
	BoundEnvCount  int        `json:"bound_env_count"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// maskSecret 口令掩码：首字符 + *** + 末两字符。
//
// 只保留 3 个可打印字符是为了让运营能认出「这是不是我刚改的那条」，
// 同时不足以还原口令。长度不足 4 时一律返回 *** ——短口令反而更值得遮住。
func maskSecret(s string) string {
	r := []rune(s)
	if len(r) < 4 {
		return "***"
	}
	return string(r[:1]) + "***" + string(r[len(r)-2:])
}

// 代理协议白名单（DB 有 CHECK，mem 驱动没有，Go 侧必须再判一次）。
func validProxyProtocol(p string) bool {
	switch p {
	case "http", "https", "socks5":
		return true
	}
	return false
}

func toProxyView(p *types.Proxy, bound int) ProxyView {
	return ProxyView{
		ID: p.ID, TenantID: p.TenantID, Name: p.Name, Protocol: p.Protocol,
		Host: p.Host, Port: p.Port, Username: p.Username,
		// has_password 取仓储的派生列而不是 p.PasswordEnc != ""：
		// 常规读路径（Get/List）显式排除 password_enc，那个字段恒为空，
		// 用它判断会让「明明设了口令」的代理在列表里显示成「未设置」。
		HasPassword: p.HasPassword, Country: p.Country, City: p.City, Type: p.Type,
		Note: p.Note, Status: p.Status, CheckStatus: checkStatusOr(p.CheckStatus),
		ExitIP: p.ExitIP, ExitCountry: p.ExitCountry, LatencyMS: p.LatencyMS,
		LastCheckedAt: p.LastCheckedAt, LastError: p.LastError,
		KeyVersion: p.KeyVersion, BoundEnvCount: bound,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// checkStatusOr 视图层的枚举兜底：历史行（0013 之前）与 mem 零值都可能带空串，
// 前端要拿它当 class 用，空串会渲染出一个没有文案的徽章。
func checkStatusOr(s string) string {
	if s == "" {
		return types.ProxyCheckUnknown
	}
	return s
}

// ---------- 创建 ----------

// ProxyCreateInput 新建代理入参。
type ProxyCreateInput struct {
	Name     string
	Protocol string
	Host     string
	Port     int
	Username string
	// Password 明文口令：**只进不出**。落库前 Seal，任何响应都不再出现。
	Password string
	Country  string
	// City 城市（P3）。可空，只用于台账识别与备注展示，不参与任何判据。
	City string
	// Type 线路类型（P3）：isp|static_residential|residential|datacenter，默认 isp。
	Type     string
	Note     string
	Status   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Create 新建代理：权限 → 校验 → 加密 → 落库 + 事件（同一事务）。
func (s *ProxyService) Create(ctx context.Context, in ProxyCreateInput) (*ProxyView, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "proxy:manage", Path: "/proxies"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权维护代理台账（" + string(d.Layer) + "）")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, apierr.Validation("代理名称需为 1-64 个字符")
	}
	proto := strings.ToLower(strings.TrimSpace(in.Protocol))
	if proto == "" {
		proto = "http"
	}
	if !validProxyProtocol(proto) {
		return nil, apierr.Validation("代理协议只能是 http / https / socks5")
	}
	if strings.TrimSpace(in.Host) == "" {
		return nil, apierr.Validation("代理主机不能为空")
	}
	if in.Port < 1 || in.Port > 65535 {
		return nil, apierr.Validation("代理端口需在 1-65535 之间")
	}
	// 类型与拨测健康都在服务层再判一次白名单：DB 有 CHECK，mem 驱动没有，
	// 「本地能存、线上报错」的错配只能靠这里消灭。
	ptype := strings.ToLower(strings.TrimSpace(in.Type))
	if ptype == "" {
		ptype = types.ProxyTypeISP
	}
	if !types.ValidProxyType(ptype) {
		return nil, apierr.Validation("代理类型只能是 isp / static_residential / residential / datacenter")
	}
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "disabled" {
		return nil, apierr.Validation("代理状态只能是 active / disabled")
	}
	var enc string
	keyVersion := 1
	if in.Password != "" {
		if s.cipher == nil {
			return nil, apierr.Internal("信封加密未装配，无法保存代理口令")
		}
		sealed, err := s.cipher.Seal(in.Password)
		if err != nil {
			return nil, apierr.Internal("代理口令加密失败")
		}
		enc = sealed
		keyVersion = s.cipher.CurrentVersion()
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	now := time.Now().UTC()
	p := &types.Proxy{
		ID: ids.New(), TenantID: in.Actor.TenantID, Name: name, Protocol: proto,
		Host: strings.TrimSpace(in.Host), Port: in.Port, Username: strings.TrimSpace(in.Username),
		PasswordEnc: enc, KeyVersion: keyVersion, HasPassword: enc != "",
		Country: strings.ToUpper(strings.TrimSpace(in.Country)),
		City:    strings.TrimSpace(in.City),
		Type:    ptype,
		// 新建即 unknown：没拨测过就是没拨测过。绑不绑得上看 BindProxy 的校验，
		// 这里替运营断言「它是通的」会让整条健康语义失效。
		CheckStatus: types.ProxyCheckUnknown,
		Note:        strings.TrimSpace(in.Note),
		Status:      status, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Proxies().Create(tctx, p); err != nil {
			return err
		}
		// L4：审计只记「有没有口令」，绝不记用户名与口令本身。
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "proxy.created", OccurredAt: now,
			Actor: actorOf(in.Actor), Terminal: in.Terminal,
			Context: eventbus.ContextInfo{URL: in.URL, Action: "proxy.create", Result: "success"},
			Data:    map[string]any{"proxy_id": p.ID, "has_password": enc != ""},
		})
	}); err != nil {
		return nil, err
	}
	v := toProxyView(p, 0)
	return &v, nil
}

// ---------- 修改 ----------

// ProxyUpdateInput 修改代理入参。
type ProxyUpdateInput struct {
	ID       string
	Name     string
	Protocol string
	Host     string
	Port     int
	Username string
	// Password 三态语义（与常见的「缺省即清空」刻意区分）：
	//   nil   = 不改口令；
	//   ""    = 清空口令；
	//   非空  = 设置新口令。
	// 用指针而不是字符串是因为「不改」和「清空」在 UI 上是两个不同的动作，
	// 混成一个零值会让运营在只改备注时把口令抹掉。
	Password *string
	Country  string
	City     string
	Type     string
	Note     string
	Status   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Update 修改代理（含改口令 / 清口令）。
func (s *ProxyService) Update(ctx context.Context, in ProxyUpdateInput) (*ProxyView, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "proxy:manage", Path: "/proxies"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权维护代理台账（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	cur, err := s.st.Proxies().Get(tctx, in.ID)
	if err != nil {
		return nil, err
	}
	if name := strings.TrimSpace(in.Name); name != "" {
		if len([]rune(name)) > 64 {
			return nil, apierr.Validation("代理名称需为 1-64 个字符")
		}
		cur.Name = name
	}
	if proto := strings.ToLower(strings.TrimSpace(in.Protocol)); proto != "" {
		if !validProxyProtocol(proto) {
			return nil, apierr.Validation("代理协议只能是 http / https / socks5")
		}
		cur.Protocol = proto
	}
	if host := strings.TrimSpace(in.Host); host != "" {
		cur.Host = host
	}
	if in.Port != 0 {
		if in.Port < 1 || in.Port > 65535 {
			return nil, apierr.Validation("代理端口需在 1-65535 之间")
		}
		cur.Port = in.Port
	}
	if in.Username != "" {
		cur.Username = strings.TrimSpace(in.Username)
	}
	if in.Password != nil {
		if *in.Password == "" {
			cur.PasswordEnc = ""
		} else {
			if s.cipher == nil {
				return nil, apierr.Internal("信封加密未装配，无法保存代理口令")
			}
			sealed, serr := s.cipher.Seal(*in.Password)
			if serr != nil {
				return nil, apierr.Internal("代理口令加密失败")
			}
			cur.PasswordEnc = sealed
			cur.KeyVersion = s.cipher.CurrentVersion()
		}
	}
	// 未改口令时必须把库里的密文带回去再整行写回。
	//
	// cur 来自 Get，而 Get 的读列表**显式排除** password_enc（密文只经
	// PasswordEnc() 取），所以这里的 cur.PasswordEnc 恒为空——直接写回
	// 就是「改一次备注，代理口令被清空」，而且运营看不出来，直到下次
	// 启动时上游返回 407 才发现。这是需求 §4「编辑时未改口令则不覆盖」
	// 的落点。
	if in.Password == nil {
		if enc, eerr := s.st.Proxies().PasswordEnc(tctx, in.ID); eerr == nil {
			cur.PasswordEnc = enc
		}
	}
	cur.HasPassword = cur.PasswordEnc != ""
	if in.Country != "" {
		cur.Country = strings.ToUpper(strings.TrimSpace(in.Country))
	}
	if in.City != "" {
		cur.City = strings.TrimSpace(in.City)
	}
	if pt := strings.ToLower(strings.TrimSpace(in.Type)); pt != "" {
		if !types.ValidProxyType(pt) {
			return nil, apierr.Validation("代理类型只能是 isp / static_residential / residential / datacenter")
		}
		cur.Type = pt
	}
	if in.Note != "" {
		cur.Note = strings.TrimSpace(in.Note)
	}
	if st := strings.TrimSpace(in.Status); st != "" {
		if st != "active" && st != "disabled" {
			return nil, apierr.Validation("代理状态只能是 active / disabled")
		}
		cur.Status = st
	}
	now := time.Now().UTC()
	cur.UpdatedAt = now
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Proxies().Update(tctx, cur); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "proxy.updated", OccurredAt: now,
			Actor: actorOf(in.Actor), Terminal: in.Terminal,
			Context: eventbus.ContextInfo{URL: in.URL, Action: "proxy.update", Result: "success"},
			Data: map[string]any{
				"proxy_id": cur.ID, "has_password": cur.PasswordEnc != "",
				"password_changed": in.Password != nil,
			},
		})
	}); err != nil {
		return nil, err
	}
	bound, _ := s.st.Proxies().CountBound(tctx, cur.ID)
	v := toProxyView(cur, bound)
	return &v, nil
}

// ---------- 查询 ----------

// Get 单条代理（不含口令）。
func (s *ProxyService) Get(ctx context.Context, actor iam.Subject, id string) (*ProxyView, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "proxy:read", Path: "/proxies"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权查看代理台账（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	p, err := s.st.Proxies().Get(tctx, id)
	if err != nil {
		return nil, err
	}
	bound, err := s.st.Proxies().CountBound(tctx, id)
	if err != nil {
		return nil, err
	}
	v := toProxyView(p, bound)
	// 详情才解密算掩码：这是**唯一**一次为「展示」而非「使用」解密口令的地方。
	// 掩码不含可用信息，且绝不进日志与审计。
	if v.HasPassword && s.cipher != nil {
		if enc, eerr := s.st.Proxies().PasswordEnc(tctx, id); eerr == nil && enc != "" {
			if plain, oerr := s.cipher.Open(enc); oerr == nil {
				v.PasswordMask = maskSecret(plain)
				plain = ""
			}
		}
	}
	return &v, nil
}

// List 代理台账（分页 + 状态/关键词过滤），每项带 bound_env_count。
// History 查询单条代理最近 100 次拨测记录。
func (s *ProxyService) History(ctx context.Context, actor iam.Subject, proxyID string, limit int) ([]types.ProxyProbeHistory, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "proxy:read", Path: "/proxies"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权查看代理拨测历史（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	// 先确认代理归属，避免跨租户 ID 被当成空历史成功返回。
	if _, err := s.st.Proxies().Get(tctx, proxyID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return s.st.Proxies().ListProbeHistory(tctx, proxyID, limit)
}

// List 代理台账（分页 + 状态/关键词过滤），每项带 bound_env_count。
func (s *ProxyService) List(ctx context.Context, actor iam.Subject, f store.ProxyFilter) ([]ProxyView, int, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "proxy:read", Path: "/proxies"})
	if !d.Allow {
		return nil, 0, apierr.Forbidden("无权查看代理台账（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	list, total, err := s.st.Proxies().List(tctx, f)
	if err != nil {
		return nil, 0, err
	}
	out := make([]ProxyView, 0, len(list))
	for _, p := range list {
		bound, berr := s.st.Proxies().CountBound(tctx, p.ID)
		if berr != nil {
			return nil, 0, berr
		}
		out = append(out, toProxyView(p, bound))
	}
	return out, total, nil
}

// Delete 删除代理：被环境引用则 409（仓储层是唯一判据，这里补一条可读文案）。
func (s *ProxyService) Delete(ctx context.Context, actor iam.Subject, id string, term eventbus.Terminal, url string) error {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "proxy:manage", Path: "/proxies"})
	if !d.Allow {
		return apierr.Forbidden("无权维护代理台账（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	p, err := s.st.Proxies().Get(tctx, id)
	if err != nil {
		return err
	}
	n, err := s.st.Proxies().CountBound(tctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return apierr.Conflict("该代理仍被 " + itoa(n) + " 个环境引用，请先解绑")
	}
	now := time.Now().UTC()
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Proxies().Delete(tctx, id); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: "proxy.deleted", OccurredAt: now, Risk: eventbus.RiskMedium,
			Actor: actorOf(actor), Terminal: term,
			Context: eventbus.ContextInfo{URL: url, Action: "proxy.delete", Result: "success"},
			Data:    map[string]any{"proxy_id": id, "name": p.Name},
		})
	})
}

// ---------- 连通性拨测 ----------

// ProxyTestInput 拨测入参。
type ProxyTestInput struct {
	ProxyID   string
	TimeoutMS int
	// Target 探测目标 URL。为空时用默认值（一个只回显出口 IP 的公网服务）。
	Target string
	// CountryTarget 出口国家实测目标（P3，可空）。
	// 空 = 不额外出网，ExitCountry 沿用台账录入的 country。
	CountryTarget string
	Actor         iam.Subject
}

// ProxyTestResult 拨测结果。
//
// ok=false 不是接口错误：拨测失败正是要告诉运营的结论，用 500 表达会让前端
// 把「代理不通」和「服务挂了」混成一种提示。只有参数非法才 400。
// exit_ip 只在目标服务回显了裸 IP 时才带出；拿不到就留空——**猜出来的出口 IP
// 比没有更危险**，运营会拿它去判断是否换过 IP。
type ProxyTestResult struct {
	OK          bool   `json:"ok"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	StatusCode  int    `json:"status_code,omitempty"`
	ExitIP      string `json:"exit_ip,omitempty"`
	ExitCountry string `json:"exit_country,omitempty"`
	// CountryMeasured ExitCountry 是不是真测出来的。
	//
	// false 表示它只是台账里录入的 country——把「录入值」和「实测值」
	// 混在一个字段里不标注来源，运营会拿它去判断「这个代理真的在美国」，
	// 而它其实只是某人当初填了 US。
	CountryMeasured bool   `json:"country_measured"`
	Error           string `json:"error,omitempty"`
	Target          string `json:"target"`
	// Skipped 拨测**根本没跑**（ZINIAO_PROXY_TEST=off）。
	//
	// 必须与 OK=false 分开表达：off 是部署侧摘掉了这条服务端出网路径，
	// 不是「代理不通」。合成一个 false 会让绑定校验把「没验证过」当成
	// 「验证过且失败」，内网部署就永远绑不上代理。
	Skipped bool `json:"skipped"`
}

// defaultProxyTestTarget 只是配置缺失时的最后兜底；权威默认值在
// platform/config.Default().ProxyTestTarget（ZINIAO_PROXY_TEST_TARGET 可覆写）。
const defaultProxyTestTarget = "https://api.ipify.org"

var bareIPPattern = regexp.MustCompile(`^\s*(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\s*$`)

// Test 服务端拨测：经该代理访问目标 URL，测连通性与耗时（不回传任何凭据）。
//
// P3：拨测结论**落库**（check_status / exit_ip / exit_country / latency_ms /
// last_error）。原因是「能不能绑」这个判据必须来自库里的事实，而不是某次
// 前端调用的返回值——否则换个浏览器点一下就能绕过校验。
func (s *ProxyService) Test(ctx context.Context, in ProxyTestInput) (*ProxyTestResult, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "proxy:manage", Path: "/proxies"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权拨测代理（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	p, err := s.st.Proxies().Get(tctx, in.ProxyID)
	if err != nil {
		return nil, err
	}
	// 必须带租户上下文：PasswordEnc 与 SaveCheckResult 都是租户域读写入口。
	out := probeProxy(tctx, s.st, s.cipher, p, proxyProbeInput{
		Target:        in.Target,
		CountryTarget: in.CountryTarget,
		TimeoutMS:     in.TimeoutMS,
	})
	if out == nil {
		// probeProxy 只在「读不到凭据」这种仓储级故障下返回 nil：
		// 那不是「代理不通」，不能伪装成 ok=false 写进台账。
		return nil, apierr.Internal("代理凭据读取失败，无法拨测")
	}
	persistProxyCheck(tctx, s.st, nil, p.ID, out)
	return out, nil
}

// ---------- 出口拨测（台账拨测与启动前拨测共用） ----------

const (
	defaultProxyProbeTimeout = 5 * time.Second
	maxProxyProbeTimeout     = 15 * time.Second
)

// proxyProbeInput 拨测参数（内部共用）。
type proxyProbeInput struct {
	Target        string
	CountryTarget string
	TimeoutMS     int
}

// probeProxy 经一条代理台账记录做一次出口拨测（P3）。
//
// 台账拨测与启动前拨测**共用这一份实现**：两条路径对「这台代理现在通不通」
// 必须是同一个判断。各写一份的代价不是重复代码，而是出现
// 「拨测说 ok、启动说不通」这种两边都没错的矛盾，且无从排查。
//
// 明文口令在本函数栈内解密、只喂给 network.NewDialer，之后不再有任何引用：
// 不进日志、不进 ProxyTestResult、不进响应。
func probeProxy(ctx context.Context, st store.Store, cipher *crypto.EnvelopeCipher,
	p *types.Proxy, in proxyProbeInput) *ProxyTestResult {
	timeout := time.Duration(in.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultProxyProbeTimeout
	}
	if timeout > maxProxyProbeTimeout {
		timeout = maxProxyProbeTimeout
	}
	target := strings.TrimSpace(in.Target)
	if target == "" {
		target = defaultProxyTestTarget
	}
	out := &ProxyTestResult{Target: target}
	// 目标只接受 http/https：请求体与配置都能指定目标，协议白名单在这里收口，
	// 避免 file:// / gopher:// 之类被当成服务端出网入口。
	if u, uerr := url.Parse(target); uerr != nil || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		out.Error = "目标地址非法"
		return out
	}

	// 密文必须走独立的 PasswordEnc 读入口：PG 的 Get/List 显式排除 password_enc
	// 列，用 Get 拿到的 p.PasswordEnc 恒为空，带鉴权的上游代理会拨测失败。
	enc, err := st.Proxies().PasswordEnc(ctx, p.ID)
	if err != nil {
		return nil // 仓储错误：交给调用方按错误处理，不伪装成「拨测失败」
	}
	password := ""
	if enc != "" {
		if cipher == nil {
			out.Error = "代理口令信封未装配，无法解密出口凭据"
			return out
		}
		plain, oerr := cipher.Open(enc)
		if oerr != nil {
			// 给明确原因，但不回显密文/密钥状态细节（排障看服务端日志）。
			out.Error = "代理口令解密失败（请检查信封密钥）"
			return out
		}
		password = plain
	}
	cfg := network.ProxyConfig{
		Protocol: network.Protocol(p.Protocol), Host: p.Host, Port: p.Port,
		Username: p.Username, Password: password, Country: p.Country,
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: network.NewDialer(cfg).DialContext},
		Timeout:   timeout,
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		out.Error = "目标地址非法"
		return out
	}
	resp, err := client.Do(req)
	out.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		out.Error = trimErr(err)
		return out
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	// 4xx 不是连通成功：403/407 恰恰说明代理鉴权或目标拒绝，
	// 旧口径 StatusCode < 500 会把它们显示成「拨测成功」。
	out.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out.Error = "目标返回 HTTP " + itoa(resp.StatusCode)
		return out
	}
	out.OK = true
	// 目标回显裸 IP 时才认领出口 IP（如 ipify）；否则留空。
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 256))
	if rerr == nil {
		if m := bareIPPattern.FindSubmatch(body); m != nil && net.ParseIP(string(m[1])) != nil {
			out.ExitIP = string(m[1])
		}
	}
	// 国家实测是**可选**的：默认不额外出网，沿用台账录入值并标注来源。
	// 实测失败不致命——它只影响展示与软校验，不该让一次成功的连通性拨测
	// 变成失败，更不该阻断启动。
	if ct := strings.TrimSpace(in.CountryTarget); ct != "" {
		if c, cerr := probeCountry(reqCtx, client, ct); cerr == nil && c != "" {
			out.ExitCountry, out.CountryMeasured = c, true
		}
	}
	if out.ExitCountry == "" {
		out.ExitCountry = p.Country
	}
	return out
}

// probeCountry 用同一条上游通道再取一次出口国家（如 https://ipinfo.io/json）。
//
// 只认 JSON 里的 country / country_code 字段，解析不到就当没测——
// 宁可留空，也不要把一个猜出来的国家写进台账。
func probeCountry(ctx context.Context, client *http.Client, target string) (string, error) {
	if u, err := url.Parse(target); err != nil || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("国家探测目标非法")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("国家探测目标返回 HTTP " + itoa(resp.StatusCode))
	}
	var m map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&m); err != nil {
		return "", err
	}
	for _, k := range []string{"country", "country_code", "countryCode"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.ToUpper(strings.TrimSpace(v)), nil
		}
	}
	return "", errors.New("国家探测响应里没有 country 字段")
}

// persistProxyCheck 把一次拨测结论落回台账（拨测接口与启动前拨测共用）。
//
// 失败只告警：结论本身已经在 out 里返回给调用方了，落库失败影响的是
// 「下次能不能绑」，而不是「这次通不通」。为了写库失败把一次成功的启动
// 挡下来，是把辅助动作放到了主流程前面。
func persistProxyCheck(ctx context.Context, st store.Store, log *slog.Logger, id string, res *ProxyTestResult) {
	if res == nil || res.Skipped {
		return // 没跑过就不写：写了等于把「未知」记成「失败」
	}
	status := types.ProxyCheckFail
	if res.OK {
		status = types.ProxyCheckOK
	}
	checkedAt := time.Now().UTC()
	err := st.Proxies().SaveCheckResult(ctx, id, store.ProxyCheckResult{
		CheckStatus: status,
		StatusCode:  res.StatusCode,
		ExitIP:      res.ExitIP,
		ExitCountry: res.ExitCountry,
		LatencyMS:   int(res.ElapsedMS),
		CheckedAt:   checkedAt,
		LastError:   res.Error,
	})
	if err != nil && log != nil {
		log.Warn("拨测结论落库失败（不影响本次结论）", "proxy", id, "err", err.Error())
	}
	// 历史记录只保存结果摘要，不保存目标 URL、代理地址凭据或密文。
	history := &types.ProxyProbeHistory{
		ID: ids.New(), ProxyID: id, CheckStatus: status, StatusCode: res.StatusCode,
		LatencyMS: int(res.ElapsedMS), Error: res.Error, CheckedAt: checkedAt,
	}
	if err := st.Proxies().AppendProbeHistory(ctx, history); err != nil && log != nil {
		log.Warn("拨测历史写入失败（不影响本次结论）", "proxy", id, "err", err.Error())
	}
}

// trimErr 拨测错误信息裁剪：只保留类型与地址，**绝不带凭据**。
// 网络层的错误串来自 Dialer，理论上不含口令，但拼接前统一过一道是必要的
// ——口令一旦进过字符串，就可能被日志或前端渲染出来。
func trimErr(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "password"); i >= 0 {
		msg = msg[:i]
	}
	if len([]rune(msg)) > 200 {
		msg = string([]rune(msg)[:200]) + "…"
	}
	return msg
}
