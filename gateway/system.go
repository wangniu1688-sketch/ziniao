package gateway

import (
	"context"
	"net/http"
	"time"

	"ziniao/browser"
	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// routesSystem M1 启动器自描述接口。
//
// 权限：`-`（登录即可）。理由：这两个接口只回答「本机启动器是什么状态」，
// 不含任何业务数据；真正需要权限的是后续的环境/代理/分组操作接口。
// 租户隔离仍由 authed() 注入的 store.WithTenant 保证（stats 的计数全部带 RLS）。
//
// CA 装/删是唯一例外：它改的是**本机当前用户的信任库**，属高危管理动作，
// 走 ca:manage（只有 owner / ops_manager 持有）。
func (s *Server) routesSystem() {
	m := s.mux
	m.HandleFunc("GET /api/v1/system/info", s.authed(s.handleSystemInfo))
	m.HandleFunc("GET /api/v1/system/stats", s.authed(s.handleSystemStats))
	m.HandleFunc("GET /api/v1/system/ca", s.authed(s.handleCAGet))
	m.HandleFunc("POST /api/v1/system/ca/install", s.authed(s.handleCAInstall))
	m.HandleFunc("POST /api/v1/system/ca/uninstall", s.authed(s.handleCAUninstall))
}

// ---------- 本机 MITM 根证书（M3/T7） ----------

// handleCAGet CA 状态（只读，登录即可看：启动按钮能不能用取决于它）。
func (s *Server) handleCAGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.caStatusOrUnwired())
}

// handleCAInstall 安装 CA 到当前用户信任库（ca:manage）。
//
// 为什么必须显式人工触发：改信任库是有副作用的动作，装完本机所有浏览器都会信任
// 这张证书。启动器自己只在 system/info 里如实报告 ca_ready，绝不自动装。
func (s *Server) handleCAInstall(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "ca:manage", "/settings"); err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Browser == nil {
		s.notWired(w)
		return
	}
	st, err := s.deps.Browser.InstallCA()
	if err != nil {
		writeErr(w, apierr.Internal("MITM 根证书安装失败："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ca": st})
}

// handleCAUninstall 从当前用户信任库移除 CA（ca:manage）。
//
// 卸载后 ca_ready=false，启动会被阻断——响应里明确回这句话，避免运营以为
// 「卸载把环境弄坏了」。
func (s *Server) handleCAUninstall(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "ca:manage", "/settings"); err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Browser == nil {
		s.notWired(w)
		return
	}
	st, err := s.deps.Browser.UninstallCA()
	if err != nil {
		writeErr(w, apierr.Internal("MITM 根证书卸载失败："+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ca": st})
}

// caStatusOrUnwired 启动器内核未装配时给一个「不可用」的 CA 状态而不是 500：
// 前端要能画出「未装配」这张卡，而不是弹一个看不懂的错误。
func (s *Server) caStatusOrUnwired() browser.CAStatus {
	if s.deps.Browser == nil {
		return browser.CAStatus{Detail: "启动器内核未装配"}
	}
	return s.deps.Browser.CA()
}

// ---------- GET /api/v1/system/info ----------

// LauncherVersion 启动器产品版本号。
//
// 单独立于 git/build 信息：它要出现在「系统设置」页面上，运营报障时第一句就是
// 「我用的是哪个版本」。随里程碑推进手动递增（M1=0.1.0，M2=0.2.0）。
const LauncherVersion = "0.2.0"

// systemBrowser 浏览器探测结论的自描述形状。
//
// name/path/version 是 M2 批复里点名要求的字段（前端「系统设置」页直接读），
// installed/major/candidates 是 M1 引导卡依赖的字段——两组并存，不做二选一：
// 引导卡要回答「能不能启动」，设置页要回答「用的是哪个浏览器」。
type systemBrowser struct {
	// name/path/version 不带 omitempty：没装浏览器时也要给出空串而不是缺字段。
	// 缺字段会让前端渲染成 undefined，那是「页面坏了」，与「没装浏览器」是两件事。
	Installed  bool                `json:"installed"`
	Name       string              `json:"name"`
	Path       string              `json:"path"`
	Version    string              `json:"version"`
	Major      int                 `json:"major,omitempty"`
	Candidates []browser.Candidate `json:"candidates"`
}

// systemInfo 启动器自描述。
//
// 红线：**绝不**返回 DSN、JWT secret、任何上游代理账号口令、ZINIAO_IP_TOKEN。
// 这个接口的设计目的是给前端画「环境自检卡」，不是给排障人员 dump 配置——
// 排障要看配置有日志与进程列表，不需要一个 HTTP 接口把凭据送出去。
type systemInfo struct {
	// 以下 7 个扁平字段是 M2 补查点名要求的自描述项（系统设置页逐项展示）：
	// product_version / data_dir / server_addr / browser{name,path,version} /
	// profile_root / extension_root / ca_ready。
	ProductVersion string        `json:"product_version"`
	DataDir        string        `json:"data_dir"`
	ProfileRoot    string        `json:"profile_root"`
	ExtensionRoot  string        `json:"extension_root"`
	ServerAddr     string        `json:"server_addr"`
	CAReady        bool          `json:"ca_ready"`
	Browser        systemBrowser `json:"browser"`
	Service        struct {
		Name      string `json:"name"`
		Env       string `json:"env"`
		UptimeSec int64  `json:"uptime_sec"`
	} `json:"service"`
	Store struct {
		Driver string `json:"driver"`
	} `json:"store"`
	Data struct {
		Dir      string `json:"dir"`
		Profiles string `json:"profiles"`
		Runtime  string `json:"runtime"`
		CADir    string `json:"ca_dir"`
		CAReady  bool   `json:"ca_ready"`
		CADetail string `json:"ca_detail,omitempty"`
	} `json:"data"`
	Capabilities map[string]bool `json:"capabilities"`
	Listen       struct {
		HTTP string `json:"http"`
	} `json:"listen"`
	Limits struct {
		EnvReclaimHours  int `json:"env_reclaim_hours"`
		APIRatePerMinute int `json:"api_rate_per_minute"`
	} `json:"limits"`
	Launcher struct {
		// SecondInstance 本次进程是「已有启动器在运行」的只读实例（本期不支持多开）。
		SecondInstance bool `json:"second_instance"`
		RuntimeEntries int  `json:"runtime_entries"`
	} `json:"launcher"`
	GeneratedAt time.Time `json:"generated_at"`
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	var out systemInfo
	out.Service.Name = "ziniao-gateway"
	out.Service.Env = s.deps.Config.Env
	out.Service.UptimeSec = int64(time.Since(s.startedAt).Seconds())
	out.Store.Driver = s.deps.Config.StoreDriver
	out.Listen.HTTP = s.deps.Config.HTTPAddr
	out.Limits.EnvReclaimHours = s.deps.Config.Limits.EnvReclaimHours
	out.Limits.APIRatePerMinute = s.deps.Config.Limits.APIRatePerMinute
	out.GeneratedAt = time.Now().UTC()
	// 扁平自描述项：无论启动器内核有没有装配都要给（设置页靠它们画只读卡片，
	// 缺字段会显示成 undefined，那是「页面坏了」而不是「能力没装」）。
	out.ProductVersion = LauncherVersion
	out.DataDir = s.deps.Config.DataDir
	out.ServerAddr = s.deps.Config.HTTPAddr

	// 浏览器能力未装配（Browser 为 nil）时，browser 段如实给「未安装」而不是
	// 省略字段：前端靠 installed=false 画引导卡，缺字段会让它以为「还没加载完」。
	if s.deps.Browser != nil {
		b := s.deps.Browser
		l := b.Layout()
		ca := b.CA()
		out.Data.Dir = l.DataDir
		out.Data.Profiles = l.ProfilesDir
		out.Data.Runtime = l.RuntimeDir
		out.Data.CADir = l.CADir
		out.Data.CAReady = ca.Ready
		out.Data.CADetail = ca.Detail
		out.ProfileRoot = l.ProfilesDir
		out.ExtensionRoot = l.ExtensionsDir
		out.CAReady = ca.Ready
		info := b.Browser()
		out.Browser = systemBrowser{
			Installed: info.Installed, Name: info.Kind, Path: info.Path,
			Version: info.Version, Major: info.Major, Candidates: info.Candidates,
		}
		out.Capabilities = b.Capabilities()
		out.Launcher.SecondInstance = b.SecondInstance()
		out.Launcher.RuntimeEntries = len(b.Runtime())
	} else {
		out.Data.Dir = s.deps.Config.DataDir
		out.Browser = systemBrowser{Installed: false}
		out.Capabilities = map[string]bool{
			"launch": false, "cdp": false, "local_proxy": false,
			"mitm": false, "sidepanel": false, "reconcile": false,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- GET /api/v1/system/stats ----------

// systemStats 首页静态快照（M1 不轮询）。
//
// running_envs 取 environments.status='running' 的计数：权威在库，不采样本机进程。
// 「本机进程还在」不等于「环境在跑」，反过来也一样——把进程数当权威会让
// 换机器登录后看到一堆幽灵运行态。
type systemStats struct {
	// 以下 4 项是 M2 补查点名要求的环境口径（工作台「最近环境」卡片直接读）：
	// environment_total / running / offline / recent[]。
	// 与 shops.* 的历史字段并存，不做替换——既有前端与 e2e 还在读 shops.total。
	EnvironmentTotal int             `json:"environment_total"`
	Running          int             `json:"running"`
	Offline          int             `json:"offline"`
	Recent           []envRecentItem `json:"recent"`
	Shops            struct {
		Total       int `json:"total"`
		RunningEnvs int `json:"running_envs"`
	} `json:"shops"`
	Devices struct {
		Total int `json:"total"`
	} `json:"devices"`
	Proxies struct {
		Total  int `json:"total"`
		Active int `json:"active"`
	} `json:"proxies"`
	Groups struct {
		Total int `json:"total"`
	} `json:"groups"`
	Approvals struct {
		Pending int `json:"pending"`
	} `json:"approvals"`
	GeneratedAt time.Time `json:"generated_at"`
}

// envRecentItem 工作台「最近环境」条目。
//
// 只给列表展示所需的最小集：不含注入脚本正文、不含代理口令、不含本机进程态
// （PID / CDP 端口属 M3，现在没有真值）。统计口径全部来自 PostgreSQL，
// 不读本机运行时注册表——进程在不在，不等于环境在不在跑。
type envRecentItem struct {
	ID            string     `json:"id"`
	ShopID        string     `json:"shop_id"`
	ShopName      string     `json:"shop_name,omitempty"`
	Status        string     `json:"status"`
	Country       string     `json:"country,omitempty"`
	KernelMajor   int        `json:"kernel_major"`
	StartURL      string     `json:"start_url,omitempty"`
	ExitMode      string     `json:"exit_mode"`
	LastStartedAt *time.Time `json:"last_started_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func (s *Server) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var out systemStats

	// 店铺总数与运行中环境数
	if n, err := s.deps.Store.Shops().Count(ctx); err == nil {
		out.Shops.Total = n
	} else {
		s.statsFail(w, "店铺数量统计失败", err)
		return
	}
	envs, envTotal, err := s.deps.Store.Environments().List(ctx, 200, 0)
	if err != nil {
		s.statsFail(w, "环境列表统计失败", err)
		return
	}
	for _, e := range envs {
		if e.Status == "running" {
			out.Shops.RunningEnvs++
		}
	}
	// 环境口径：总数 / 运行中 / 未运行（idle|created|stopped 统称 offline）。
	// offline 用「总数 - 运行中」而不是逐状态枚举：状态机以后加态时，
	// 逐枚举会漏掉新态，让「离线数 + 运行数 ≠ 总数」这种自相矛盾出现在首页。
	out.EnvironmentTotal = envTotal
	out.Running = out.Shops.RunningEnvs
	out.Offline = envTotal - out.Running
	if out.Offline < 0 {
		out.Offline = 0
	}
	out.Recent = s.recentEnvs(ctx, envs, 5)

	if n, err := s.deps.Store.Devices().Count(ctx); err == nil {
		out.Devices.Total = n
	} else {
		s.statsFail(w, "设备数量统计失败", err)
		return
	}

	_, pt, err := s.deps.Store.Proxies().List(ctx, store.ProxyFilter{Limit: 1})
	if err != nil {
		s.statsFail(w, "代理台账统计失败", err)
		return
	}
	_, pa, err := s.deps.Store.Proxies().List(ctx, store.ProxyFilter{Status: "active", Limit: 1})
	if err != nil {
		s.statsFail(w, "代理台账统计失败", err)
		return
	}
	out.Proxies.Total, out.Proxies.Active = pt, pa

	groups, err := s.deps.Store.Groups().List(ctx)
	if err != nil {
		s.statsFail(w, "分组统计失败", err)
		return
	}
	out.Groups.Total = len(groups)

	_, at, err := s.deps.Store.Approvals().List(ctx, store.ApprovalFilter{Status: types.ApprovalPending, Limit: 1})
	if err != nil {
		s.statsFail(w, "审批统计失败", err)
		return
	}
	out.Approvals.Pending = at
	out.GeneratedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, out)
}

// recentEnvs 取最近变动的 N 个环境（按 last_started_at → updated_at 倒序）。
//
// 排序键优先用 last_started_at：运营眼中的「最近」是「最近打开过哪个店铺」，
// 而不是「最近改过哪条配置」——改配置（绑定代理、改起始 URL）会顶掉真正的
// 最近使用记录，那这个卡片就没用了。
func (s *Server) recentEnvs(ctx context.Context, envs []types.Environment, n int) []envRecentItem {
	if n <= 0 || len(envs) == 0 {
		return []envRecentItem{}
	}
	sorted := make([]types.Environment, len(envs))
	copy(sorted, envs)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0; j-- {
			if recentKey(sorted[j]).After(recentKey(sorted[j-1])) {
				sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
				continue
			}
			break
		}
	}
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	out := make([]envRecentItem, 0, len(sorted))
	for i := range sorted {
		e := sorted[i]
		item := envRecentItem{
			ID: e.ID, ShopID: e.ShopID, Status: e.Status, Country: e.Country,
			KernelMajor: e.KernelMajor, StartURL: e.StartURL,
			ExitMode: exitModeOf(e), LastStartedAt: e.LastStartedAt, UpdatedAt: e.UpdatedAt,
		}
		if sp, err := s.deps.Store.Shops().Get(ctx, e.ShopID); err == nil && sp != nil {
			item.ShopName = sp.Name
		}
		out = append(out, item)
	}
	return out
}

func recentKey(e types.Environment) time.Time {
	if e.LastStartedAt != nil {
		return *e.LastStartedAt
	}
	return e.UpdatedAt
}

// exitModeOf 出口模式（与 shop.EnvService 同口径）：手工代理 > 代理商池 > 直连。
//
// 这里只按「有没有绑手工代理」判断：代理商池是否接入由 IPPool 决定，
// 而统计接口不持有它——宁可显示 direct，也不要在这里猜一个「应该有」的池子。
func exitModeOf(e types.Environment) string {
	if e.ProxyID != "" {
		return "manual"
	}
	return "direct"
}

// statsFail 统计类失败统一 500。
//
// 不静默降级为 0：「统计失败」显示成「有 0 个店铺」比报错更糟——运营会以为
// 数据真的空了，然后去重建一遍台账。详细原因写服务端日志，响应只给可读文案。
func (s *Server) statsFail(w http.ResponseWriter, what string, err error) {
	if s.log != nil {
		s.log.Error(what, "err", err.Error())
	}
	writeErr(w, apierr.Internal(what))
}
