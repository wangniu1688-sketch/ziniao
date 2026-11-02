package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/network"
	"ziniao/platform/apierr"
	"ziniao/platform/types"
	"ziniao/shop"
)

// EnvControl 启动/停止链路对业务侧的**全部**需求。
//
// 由 *shop.EnvService 实现。抽成接口不是为好看：失败回滚矩阵要求把 8 个阶段
// 逐个打断并断言「没有悬空状态」，没有假实现就只能在真 Chrome 上手点。
type EnvControl interface {
	// Launch 下发启动单（权限 + 数据收口 + 出口分配 + 脚本）。
	Launch(ctx context.Context, in shop.LaunchInput) (*shop.LaunchSpec, error)
	// MarkRunning 提交 running（唯一提交点：Chrome 起来 + CDP 连通之后）。
	MarkRunning(ctx context.Context, in shop.MarkRunningInput) (*types.Environment, error)
	// MarkLaunchFailed 失败复位（绝不覆盖 running）。
	MarkLaunchFailed(ctx context.Context, in shop.MarkLaunchFailedInput) (*types.Environment, error)
	// StopEnv 关闭语义（running/created → idle，幂等）。
	StopEnv(ctx context.Context, in shop.StopInput) (*types.Environment, error)
	// SaveThumbnail 记录停止前的窗口缩略图路径。
	SaveThumbnail(ctx context.Context, in shop.ThumbnailInput) (*types.Environment, error)
	// ResetStaleRunning 对账复位：把「库里 running 但本机没有进程」的环境收回 idle。
	//
	// keep = 本机确认在跑的环境 ID；tenantIDs = 本机注册表里出现过的租户。
	// 跨租户核对而不是只查一个：同一台机器上可能有多个租户的环境在跑，
	// 漏掉任何一个都会留下一个永远关不掉的「运行中」。
	ResetStaleRunning(ctx context.Context, tenantIDs []string, keep map[string]bool) (int, error)
}

// StartInput 启动环境入参。
type StartInput struct {
	EnvID    string
	ShopID   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
	// OpenURL 本次要打开的首个页面；为空时依次回落环境的 start_url、about:blank。
	OpenURL string
}

// StartResult 启动结果。
//
// 红线：**不含上游地址/账号/口令**。启动响应是要进日志与前端的，
// 代理凭据只能活在 Go 进程内存里（详见 shop.UpstreamCredential）。
type StartResult struct {
	EnvID          string    `json:"env_id"`
	ShopID         string    `json:"shop_id"`
	Status         string    `json:"status"`
	PID            int       `json:"pid"`
	CDPPort        int       `json:"cdp_port"`
	LocalProxyPort int       `json:"local_proxy_port"`
	ExitMode       string    `json:"exit_mode"`
	ProfileDir     string    `json:"profile_dir"`
	StartedAt      time.Time `json:"started_at"`
}

// StopInput 停止环境入参。
type StopInput struct {
	EnvID    string
	ShopID   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// StopResult 停止结果。
type StopResult struct {
	EnvID       string    `json:"env_id"`
	Status      string    `json:"status"`
	StoppedAt   time.Time `json:"stopped_at"`
	Thumbnail   string    `json:"thumbnail,omitempty"`
	Graceful    bool      `json:"graceful"`     // 是否走完 CDP 优雅关闭
	ForceKilled bool      `json:"force_killed"` // 是否最终强杀
}

// Session 一个活着的浏览器会话（内存态）。
//
// 注册表（磁盘）只记 PID/端口这类可重建的事实；连接与进程句柄是进程内的东西，
// 重启后无论如何都拿不回来——所以它们不进注册表，只在这里。
type Session struct {
	Entry    RuntimeEntry
	proc     ChromeProc
	proxy    ProxyHandle
	conn     *CDPConn
	injector *Injector
	cancel   context.CancelFunc
	actor    iam.Subject
	terminal eventbus.Terminal
	url      string
	shopID   string
}

// ProxyHandle 本地代理句柄。
type ProxyHandle interface {
	Port() int
	Close() error
}

type cdpDialer func(ctx context.Context, url string) (*CDPConn, error)

type proxyStarter func(cfg network.TLSProxyConfig) (ProxyHandle, error)

const (
	// stopForceWait 强杀之后等待进程退出的上限（优雅宽限期另算，见 Manager.gracePeriod）。
	stopForceWait = 5 * time.Second
	// devToolsPortSkew 判定端口文件「是不是本轮写的」时的时间容差。
	//
	// 文件系统时间戳有粒度（NTFS 100ns，FAT/exFAT 可达 2s），写文件与取时间之间
	// 也可能有回拨，留 1s 容差；上一轮的残留通常是分钟级甚至天级，不受影响。
	devToolsPortSkew = time.Second
	// cdpReadyAttempts / cdpReadyInterval / cdpReadyCall：阶段 6 探活的重试策略
	// （详见 waitCDPReady）。总耗时上限 = attempts × (call + interval) ≈ 10s，
	// 仍在 CDP 等待预算（默认 20s）之内，不会把超时拖成「一直转圈」。
	cdpReadyAttempts = 3
	cdpReadyInterval = 300 * time.Millisecond
	cdpReadyCall     = 3 * time.Second
)

// Start 拉起一个环境的浏览器（M3 的核心时序，9 个阶段，见设计文档 §3）。
//
// 阶段边界的意义在于**每个失败点都有确定的回滚动作**，不会出现「库里 running、
// 本机没进程」或「端口占着、进程没了」这类悬空态。回滚动作按注册的逆序执行。
func (m *Manager) Start(ctx context.Context, in StartInput) (*StartResult, error) {
	if in.EnvID == "" {
		return nil, apierr.Validation("环境 ID 为空")
	}
	if m.secondInstance {
		return nil, apierr.Conflict("启动器已在运行（本实例只读），不允许拉起环境")
	}
	if m.envs == nil {
		return nil, apierr.Internal("启动器内核未装配业务服务，无法启动环境")
	}
	var res *StartResult
	err := m.WithEnvLock(in.EnvID, func() error {
		// ---------- 阶段 0：本机冲突与陈旧条目 ----------
		//
		// 幂等重入：运营点了两次「启动」，第二次不该再开一个窗口——
		// 两个 Chrome 抢同一个 profile 目录会互相把对方的锁文件删掉，
		// 表现为「两个窗口都在，但指纹和 Cookie 随机丢失」。
		if e, ok := m.RuntimeOf(in.EnvID); ok {
			if procAlive(e.PID) {
				return apierr.Conflict("该环境已在本机运行（PID " + strconv.Itoa(e.PID) + "）")
			}
			// 进程没了 = 陈旧条目（上次崩溃留下的）。清掉，别让它挡住本次启动。
			_ = m.Unregister(in.EnvID)
		}
		if m.session(in.EnvID) != nil {
			return apierr.Conflict("该环境在本实例内已有会话，请先停止")
		}

		// ---------- 阶段 1：业务启动单（权限/收口/出口/脚本） ----------
		spec, err := m.envs.Launch(ctx, shop.LaunchInput{
			ShopID: in.ShopID, OpenURL: in.OpenURL,
			Actor: in.Actor, Terminal: in.Terminal, URL: in.URL,
		})
		if err != nil {
			// 业务侧拒绝：此刻本机一个资源都还没建（无端口 / 无 Chrome / 无 CDP），
			// 所以不需要逆序回滚，但**状态复位仍要走既有的 MarkLaunchFailed 语义**
			// （P3）：出口拨测失败这类拒绝如果不复位，就会留下「库里说在跑、
			// 本机什么都没有」的悬空 running。
			//
			// 注意这里是「复位 + 原样返回」而不是 failStart：failStart 会把错误
			// 统一包成 Internal，403/409 被改写成 500 之后，前端会把「环境已停用」
			// 显示成「服务故障」——那正是要避免的误报。
			m.resetLaunchFailure(ctx, in, "1", err.Error())
			return err
		}
		if spec.EnvID != in.EnvID || spec.ShopID != in.ShopID {
			return apierr.Conflict("环境与店铺不匹配，拒绝启动")
		}
		rb := &rollback{}

		// ---------- 阶段 2：前置检查（浏览器 + CA + profile 目录） ----------
		profileDir, err := m.Precheck(spec.EnvID)
		if err != nil {
			return m.failStart(ctx, in, "2", err.Error())
		}

		// ---------- 阶段 3：本地 TLSProxy（动态端口） ----------
		//
		// 方案 A：每环境一个本地代理，Chrome 只认 127.0.0.1:<动态端口>。
		// 上游口令只进这里的内存配置，永不进 argv / 日志 / 响应。
		proxy, err := m.startProxy(proxyConfigOf(spec, m.layout.CADir))
		if err != nil {
			return m.failStart(ctx, in, "3", "本地代理启动失败: "+err.Error())
		}
		rb.add(func() { _ = proxy.Close() })

		// ---------- 阶段 4：拉起 Chrome/Edge 独立进程 ----------
		//
		// 命令行里只有「本机代理地址」与 UA 这类非机密项：UA 不是机密，
		// 而代理地址只是 127.0.0.1:<动态端口>——上游账号/口令只在 Go 内存里。
		argv := BuildArgv(ArgvConfig{
			ProfileDir:     profileDir,
			LocalProxyPort: proxy.Port(),
			UserAgent:      spec.UserAgent,
			OpenURL:        firstNonEmpty(in.OpenURL, spec.OpenURL, spec.StartURL, "about:blank"),
			LoadSidepanel:  m.sidepanel,
			SidepanelDir:   m.layout.SidepanelDir,
		})
		// 纵深防御：口令万一进了 argv，宁可启动失败也不能让它出现在进程列表里
		// （任务管理器/安全软件都会采集命令行，那是凭据外泄最常见的一条路）。
		if ArgvHasSecret(argv, secretsOf(spec)...) {
			rb.run()
			return m.failStart(ctx, in, "4", "argv 出现敏感参数，已阻止启动")
		}

		// 4a) 拉起前先清掉上一轮的启动残留（DevToolsActivePort + Singleton*）。
		//
		// 这些文件属于「某一次进程实例」而不是 profile 本身，进程被强杀/崩溃后一定
		// 留在盘上。留着的后果是：本次先读到旧端口，而新 Chrome 又因为看到残留的
		// 单实例锁把启动请求转发给已经不存在的旧实例后自己退出 —— 症状就是
		// 「CDP 连接失败: dial tcp 127.0.0.1:<旧端口>」，且手动杀光 chrome.exe 也没用。
		if leftover := m.clearProfileResidue(profileDir, "启动前"); len(leftover) > 0 {
			m.log.Warn("profile 残留未清干净（该环境可能仍有浏览器在运行）",
				"env", spec.EnvID, "files", strings.Join(leftover, ","))
		}
		// 时间基准：比它更早写出的端口文件一律不算本轮的（见 waitDevToolsPort）。
		launchMark := time.Now().Add(-devToolsPortSkew)

		proc, err := m.startProc(m.browser.Path, argv)
		if err != nil {
			rb.run()
			return m.failStart(ctx, in, "4", "拉起浏览器失败: "+err.Error())
		}
		// 4b) 回滚：收进程 → 等它真退出 → 清残留。
		//
		// 顺序不能反：进程还在时删掉的文件会被正在退出的 Chrome 重新写回，
		// 下一轮启动照样踩同一个坑（这正是失败回滚也必须清理的原因）。
		rb.add(func() {
			_ = proc.Terminate()
			if !waitProcExit(proc, stopForceWait) {
				m.log.Warn("启动回滚：浏览器进程未能在超时内退出",
					"env", spec.EnvID, "pid", proc.PID())
			}
			m.clearProfileResidue(profileDir, "启动失败回滚")
		})

		// ---------- 阶段 5：读 DevToolsActivePort ----------
		//
		// 方案 A：--remote-debugging-port=0 让 OS 分配端口，Chrome 把端口写进
		// profile 下的 DevToolsActivePort。轮询而不是 sleep：冷启动 3-20s 不等，
		// 固定 sleep 要么白等、要么误判。
		//
		// 带 launchMark：只认本轮写出的文件（上一轮的旧文件已在 4a 清掉；万一没清掉，
		// 这里也不会把它当成本轮的端口去连）。
		cdpPort, wsPath, err := waitDevToolsPort(ctx, m.layout.DevToolsActivePort(spec.EnvID),
			m.cdpWait(), proc, launchMark)
		if err != nil {
			rb.run()
			return m.failStart(ctx, in, "5", err.Error())
		}

		// ---------- 阶段 6：CDP 连接 + document_start 注入 ----------
		wsURL := fmt.Sprintf("ws://127.0.0.1:%d%s", cdpPort, wsPath)
		conn, err := m.cdpDial(ctx, wsURL)
		if err != nil {
			rb.run()
			return m.failStart(ctx, in, "6", "CDP 连接失败: "+err.Error())
		}
		rb.add(func() { _ = conn.Close() })
		// 6a) 探活：连上 WS 不等于能发命令。
		//
		// Chrome 冷启动的头一两秒会出现「WS 握手成功但不回命令」。不在这里验，
		// 超时会推迟到阶段 7（导航）甚至阶段 8（状态提交）才爆——那时回滚栈更长，
		// 而日志里只剩一句「阶段 8 失败」，根因完全看不见。
		if rerr := waitCDPReady(conn, m.cdpWait()); rerr != nil {
			rb.run()
			return m.failStart(ctx, in, "6", "CDP 未就绪: "+rerr.Error())
		}
		callCtx, callCancel := context.WithTimeout(context.Background(), m.cdpWait())
		defer callCancel()
		// 新开的标签页也要带指纹：attach 事件里补注入。
		// 重试一次：这是浏览器刚起来时最容易撞上「命令超时」的一条调用。
		if err := retryCDP(callCtx, 2, 300*time.Millisecond, conn.EnableAutoAttach); err != nil {
			m.log.Warn("开启自动 attach 失败（新标签页可能不带指纹）", "env", spec.EnvID, "err", err.Error())
		}
		injector := NewInjector(conn, spec.Script, m.log)
		watchCtx, watchCancel := context.WithCancel(context.Background())
		rb.add(watchCancel)
		// 已有页面补注入：不 reload（批复 §11 第 6 条），打断正在填的表单代价更大。
		if n := injector.InjectExistingTargets(callCtx); n >= 0 {
			m.log.Debug("已有页面补注入完成", "env", spec.EnvID, "pages", n)
		}
		go injector.Watch(watchCtx)

		// ---------- 阶段 7：打开首个页面（失败不阻断） ----------
		//
		// 页面打不开（404/离线/目标站点挂了）不等于环境不可用：
		// 指纹注入已经生效，运营换个地址照常干活。把它当启动失败会让人反复重试。
		if sessions := injector.Sessions(); len(sessions) > 0 {
			if err := conn.Navigate(callCtx, sessions[0],
				firstNonEmpty(in.OpenURL, spec.OpenURL, spec.StartURL, "about:blank")); err != nil {
				m.log.Warn("打开首个页面失败（环境仍可用）", "env", spec.EnvID, "err", err.Error())
			}
		}

		// ---------- 阶段 8：提交 running（唯一提交点） ----------
		//
		// 用 WithoutCancel：走到这里浏览器已经起来了，此时若 HTTP 请求被前端取消，
		// 绝不能连「把状态写对」这件事一起取消——那正是悬空 running 的来源。
		startedAt := time.Now().UTC()
		dbctx, dbcancel := dbContext(ctx)
		defer dbcancel()
		env, err := m.envs.MarkRunning(dbctx, shop.MarkRunningInput{
			EnvID: spec.EnvID, Actor: in.Actor, Terminal: in.Terminal,
			URL: in.URL, StartedAt: startedAt,
		})
		if err != nil {
			rb.run()
			return m.failStart(ctx, in, "8", "状态提交失败: "+err.Error())
		}

		// ---------- 阶段 9：登记运行时 ----------
		entry := RuntimeEntry{
			EnvID: spec.EnvID, ShopID: spec.ShopID, TenantID: in.Actor.TenantID,
			PID: proc.PID(), CDPPort: cdpPort, LocalProxyPort: proxy.Port(),
			ProfileDir: profileDir, ChromePath: m.browser.Path,
			ExitMode: exitModeOf(spec), StartedAt: startedAt, ProcState: ProcRunning,
		}
		sess := &Session{
			Entry: entry, proc: proc, proxy: proxy, conn: conn, injector: injector,
			cancel: watchCancel, actor: in.Actor, terminal: in.Terminal,
			url: in.URL, shopID: spec.ShopID,
		}
		m.track(sess)
		go m.watchProcess(sess)
		regErr := m.Register(entry)
		if regErr != nil {
			// 登记表是缓存，写失败只降级：浏览器确实起来了，不能回滚掉一个可用的窗口。
			m.log.Error("运行时登记失败（degraded：环境已启动但本机缓存缺失）",
				"env", spec.EnvID, "err", regErr.Error())
			return apierr.Internal("环境已启动，但本机运行时登记失败（degraded）：" + regErr.Error())
		}

		res = &StartResult{
			EnvID: spec.EnvID, ShopID: spec.ShopID, Status: env.Status, PID: proc.PID(),
			CDPPort: cdpPort, LocalProxyPort: proxy.Port(), ExitMode: entry.ExitMode,
			ProfileDir: profileDir, StartedAt: startedAt,
		}
		m.log.Info("环境已启动", "env", spec.EnvID, "pid", proc.PID(),
			"cdp_port", cdpPort, "exit_mode", entry.ExitMode)
		return nil
	})
	return res, err
}

// Stop 关闭一个环境（优雅 → 宽限 → 强杀），并把业务状态收回 idle。
func (m *Manager) Stop(ctx context.Context, in StopInput) (*StopResult, error) {
	if in.EnvID == "" {
		return nil, apierr.Validation("环境 ID 为空")
	}
	var res *StopResult
	err := m.WithEnvLock(in.EnvID, func() error {
		sess := m.session(in.EnvID)
		if sess == nil {
			// 本机没有会话（换机器 / 启动器重启过）：业务状态仍要收回，
			// 否则库里会一直挂着 running，而运营根本关不掉它。
			if e, ok := m.RuntimeOf(in.EnvID); ok {
				if procAlive(e.PID) {
					// 进程在本机活着，只是本实例没有会话句柄（启动器重启后重挂的条目）：
					// **绝不能**清它的残留文件——那会把活实例的端口文件删掉。
					return m.stopBusinessOnly(ctx, in, &res)
				}
				_ = m.Unregister(in.EnvID)
			}
			// 本机确实没有活进程：清掉上次崩溃/强杀留下的残留，下次启动才不会读到旧端口。
			m.clearProfileResidue(m.layout.ProfileDir(in.EnvID), "停止（本机无活进程）")
			return m.stopBusinessOnly(ctx, in, &res)
		}

		out := &StopResult{EnvID: in.EnvID, StoppedAt: time.Now().UTC()}
		callCtx, callCancel := context.WithTimeout(context.Background(), m.cdpWait())
		defer callCancel()

		// 先留一张缩略图（关掉窗口之后就再也拿不到了）。
		out.Thumbnail = m.captureThumbnail(callCtx, sess)

		// 1) 逐页优雅关闭（Page.close）；失败不阻断，继续走下面的兜底。
		graceful := true
		if sess.conn != nil {
			for _, s := range sess.injector.Sessions() {
				if err := sess.conn.ClosePage(callCtx, s); err != nil {
					graceful = false
					m.log.Warn("关闭页面失败", "env", in.EnvID, "err", err.Error())
				}
			}
			// 2) Browser.close：让 Chrome 自己走正常退出流程（写回 profile）。
			if err := sess.conn.CloseBrowser(callCtx); err != nil {
				graceful = false
			}
		}
		// 3) 等进程真正退出：优雅关闭 → 宽限期 → 强杀 → 再等一次。
		//
		// 不能只发 kill 就返回：Chrome 退出时要写回 profile 并删掉自己的单实例锁，
		// 强杀之后还要几百毫秒才真的消失；在它消失前删残留，要么删不掉，要么删了
		// 又被写回。所以这里的 Wait 是「清理残留」的前置条件，不是可选项。
		force := false
		exited := waitProcExit(sess.proc, m.gracePeriod())
		if !exited {
			force = true
			if kerr := sess.proc.Terminate(); kerr != nil {
				m.log.Error("强杀浏览器失败", "env", in.EnvID, "pid", sess.Entry.PID, "err", kerr.Error())
			}
			exited = waitProcExit(sess.proc, stopForceWait)
			if !exited {
				m.log.Error("浏览器强杀后仍未退出（进程可能已失控）", "env", in.EnvID, "pid", sess.Entry.PID)
			}
		}
		out.Graceful, out.ForceKilled = graceful, force

		// 3.5) 进程确认退出后才清残留（DevToolsActivePort + Singleton*）。
		if exited {
			m.clearProfileResidue(sess.Entry.ProfileDir, "停止后")
		} else {
			// 没确认退出就不清：清了也会被写回。下次启动的 4a 还会再清一次，
			// 这里只留一条日志说明为什么本次没清。
			m.log.Warn("浏览器进程未确认退出，跳过残留清理（下次启动前会再清）",
				"env", in.EnvID, "pid", sess.Entry.PID)
		}

		// 4) 收资源：顺序固定（监听注入 → 关 CDP → 关本地代理 → 摘注册表）。
		sess.cancel()
		if sess.conn != nil {
			_ = sess.conn.Close()
		}
		if sess.proxy != nil {
			_ = sess.proxy.Close()
		}
		m.untrack(in.EnvID)
		if uerr := m.Unregister(in.EnvID); uerr != nil {
			m.log.Warn("运行时注销失败", "env", in.EnvID, "err", uerr.Error())
		}

		// 5) 业务状态收回 idle（幂等；即使上面某一步失败也要走）。
		dbctx, dbcancel := dbContext(ctx)
		defer dbcancel()
		env, err := m.envs.StopEnv(dbctx, shop.StopInput{
			ShopID: sess.shopID, Actor: in.Actor, Terminal: in.Terminal, URL: in.URL,
		})
		if err != nil {
			return err
		}
		out.Status = env.Status
		res = out
		m.log.Info("环境已停止", "env", in.EnvID, "graceful", graceful, "force", force)
		return nil
	})
	return res, err
}

// stopBusinessOnly 本机没有活会话时的停止：只收回业务状态。
func (m *Manager) stopBusinessOnly(ctx context.Context, in StopInput, out **StopResult) error {
	if m.envs == nil {
		return apierr.Internal("启动器内核未装配业务服务，无法停止环境")
	}
	dbctx, dbcancel := dbContext(ctx)
	defer dbcancel()
	env, err := m.envs.StopEnv(dbctx, shop.StopInput{
		ShopID: in.ShopID, Actor: in.Actor, Terminal: in.Terminal, URL: in.URL,
	})
	if err != nil {
		return err
	}
	*out = &StopResult{EnvID: in.EnvID, Status: env.Status, StoppedAt: time.Now().UTC()}
	return nil
}

// captureThumbnail 停止前截图（M3/T10）。
//
// 截图是**尽力而为**：失败只降级为空缩略图，绝不能因为拿不到图就不关窗口。
func (m *Manager) captureThumbnail(ctx context.Context, sess *Session) string {
	if sess.conn == nil || sess.injector == nil || m.envs == nil {
		return ""
	}
	sessions := sess.injector.Sessions()
	if len(sessions) == 0 {
		return ""
	}
	data, err := sess.conn.CaptureScreenshot(ctx, sessions[0])
	if err != nil || len(data) == 0 {
		m.log.Warn("窗口截图失败（缩略图留空）", "env", sess.Entry.EnvID)
		return ""
	}
	dst := m.layout.ThumbnailPath(sess.Entry.EnvID)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		m.log.Warn("缩略图落盘失败", "env", sess.Entry.EnvID, "err", err.Error())
		return ""
	}
	dbctx, cancel := dbContext(ctx)
	defer cancel()
	if _, err := m.envs.SaveThumbnail(dbctx, shop.ThumbnailInput{
		EnvID: sess.Entry.EnvID, Path: dst, Actor: sess.actor,
	}); err != nil {
		m.log.Warn("缩略图路径入库失败", "env", sess.Entry.EnvID, "err", err.Error())
	}
	return dst
}

// watchProcess 进程退出（崩溃 / 用户在窗口外关掉）后的自动复位。
//
// 不复位会让前端一直显示「运行中」，而窗口早没了——运营只能去改数据库。
// 复位用 StopEnv（幂等，只把 running 收回 idle），不碰 created/idle。
func (m *Manager) watchProcess(sess *Session) {
	<-sess.proc.Done()
	envID := sess.Entry.EnvID
	m.log.Warn("浏览器进程已退出，自动复位状态", "env", envID, "pid", sess.Entry.PID)
	// 进程已不在：它留下的端口文件与单实例锁现在是垃圾，留着会让下次启动读到旧端口。
	m.clearProfileResidue(sess.Entry.ProfileDir, "浏览器进程退出后")
	sess.cancel()
	if sess.conn != nil {
		_ = sess.conn.Close()
	}
	if sess.proxy != nil {
		_ = sess.proxy.Close()
	}
	m.untrack(envID)
	if uerr := m.Unregister(envID); uerr != nil {
		m.log.Warn("崩溃复位：运行时注销失败", "env", envID, "err", uerr.Error())
	}
	if m.envs == nil {
		return
	}
	ctx, cancel := dbContext(context.Background())
	defer cancel()
	if _, err := m.envs.StopEnv(ctx, shop.StopInput{
		ShopID: sess.shopID, Actor: sess.actor, Terminal: sess.terminal, URL: sess.url,
	}); err != nil {
		m.log.Error("崩溃复位：业务状态收回失败（可能滞留 running）", "env", envID, "err", err.Error())
	}
}

// ---------- 会话表 ----------

func (m *Manager) track(s *Session) {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]*Session{}
	}
	m.sessions[s.Entry.EnvID] = s
}

func (m *Manager) untrack(envID string) {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	delete(m.sessions, envID)
}

// session 取活会话（无锁读取快照，调用方需持有 perEnv 锁）。
func (m *Manager) session(envID string) *Session {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	return m.sessions[envID]
}

// ---------- 本地代理 ----------

type localProxy struct {
	port   int
	ln     net.Listener
	cancel context.CancelFunc
}

// startLocalProxy 起一个只监听 127.0.0.1 的本地 TLSProxy（方案 A）。
//
// 端口由 OS 分配（ListenLocal 传 0），不再有 20000-39999 的静态哈希区间——
// 那个区间在并发启动时会撞端口，撞了还只能靠重启解决（M1 批复已废弃）。
func startLocalProxy(cfg network.TLSProxyConfig) (ProxyHandle, error) {
	ln, port, err := ListenLocal()
	if err != nil {
		return nil, err
	}
	p := network.NewTLSProxy(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if serr := p.ServeListener(ctx, ln); serr != nil {
			_ = serr // ServeListener 正常关闭时也会返回，不能当错误打
		}
	}()
	// 等 healthz：代理没起来就让 Chrome 走它，等于让运营面对一个「所有站点都超时」
	// 的窗口，而根因只是一个没监听成功的本地端口。
	if herr := waitHealthz(ctx, port, 5*time.Second); herr != nil {
		cancel()
		_ = ln.Close()
		return nil, fmt.Errorf("本地代理未就绪: %w", herr)
	}
	return &localProxy{port: port, ln: ln, cancel: cancel}, nil
}

func (p *localProxy) Port() int { return p.port }

func (p *localProxy) Close() error {
	p.cancel()
	return p.ln.Close()
}

// waitHealthz 轮询本地代理的 /healthz。
func waitHealthz(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, derr := http.DefaultClient.Do(req)
			if derr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
				last = fmt.Errorf("健康检查返回 %d", resp.StatusCode)
			} else {
				last = derr
			}
		}
		if time.Now().After(deadline) {
			if last == nil {
				last = fmt.Errorf("超时")
			}
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ---------- DevToolsActivePort ----------

// readDevToolsActivePort 解析 Chrome 写的端口文件：第一行端口，第二行是 WS 路径。
func readDevToolsActivePort(path string) (int, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, "", err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return 0, "", fmt.Errorf("端口文件为空（%s）", path)
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port <= 0 {
		return 0, "", fmt.Errorf("端口文件内容非法（%s）", path)
	}
	ws := ""
	if len(lines) > 1 {
		ws = strings.TrimSpace(lines[1])
	}
	return port, ws, nil
}

// waitDevToolsPort 轮询等待**本轮**写出的端口文件（Chrome 冷启动 3-20s 不等）。
//
// notBefore 是本轮启动的时间基准：比它更早写出的文件一律视为上一轮遗留，绝不拿去连
// （拿去连就是「actively refused」，而错误信息完全指不到根因）。唯一例外是旧文件里的
// 端口现在确实连得上——那说明有一个活着的 Chrome 正在服务这个 profile，连它就是对的。
//
// 同时看进程是否还在：文件迟迟不来且进程已经没了，等下去只是白耗 20s——
// 这时应该立刻失败，让运营看到「启动失败」而不是「一直转圈」。
func waitDevToolsPort(ctx context.Context, path string, timeout time.Duration,
	proc ChromeProc, notBefore time.Time) (int, string, error) {
	deadline := time.Now().Add(timeout)
	start := time.Now()
	var last error
	for {
		port, ws, err := readDevToolsActivePort(path)
		switch {
		case err == nil && ws == "":
			// 文件只写了一半：Chrome 先落端口号，再落 WS 路径。拿半截文件去连，
			// 失败点是阶段 6 的「连接被拒」——根因（文件没写完）完全看不见。
			// 所以这里必须继续等，绝不带着空 WS 路径进入 CDP 阶段。
			last = fmt.Errorf("端口文件尚未写完（只有端口号 %d，缺 WS 路径）", port)
		case err == nil && writtenAfter(path, notBefore):
			return port, ws, nil
		case err == nil && portDialable(port, 300*time.Millisecond):
			// 上一轮遗留，但端口现在连得上 ⇒ 服务这个 profile 的是真实例，接受它。
			return port, ws, nil
		case err == nil:
			last = fmt.Errorf("端口文件是上一轮遗留（端口 %d 已不可连），等待新进程写入", port)
		default:
			last = err
		}
		if procExited(proc) {
			// 进程已经没了却没写出端口：最常见的原因就是残留单实例锁让新 Chrome
			// 把启动请求转发给了不存在的旧实例后自己退出。把这句话写进错误里，
			// 比一句「连接被拒」省掉半小时排查。
			return 0, "", fmt.Errorf("浏览器进程已退出且未写出 CDP 端口（多半是残留单实例锁让新进程把启动请求转发给了不存在的旧实例）: %w", last)
		}
		if time.Now().After(deadline) {
			return 0, "", fmt.Errorf("等待 CDP 端口超时（已等 %.1fs，超时上限 %.0fs，%s，%s）: %w",
				time.Since(start).Seconds(), timeout.Seconds(), path, devToolsFileState(path), last)
		}
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// devToolsFileState 端口文件的当前状态（只进错误信息）。
//
// 「DevToolsActivePort file= 为空」这种日志必须一眼能区分三种情况：
// 压根没写出 / 写出了但是空文件 / 写出的是上一轮的旧内容。
func devToolsFileState(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "文件不存在"
	}
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		return "文件不可读: " + rerr.Error()
	}
	return fmt.Sprintf("文件 %d 字节 mtime=%s", len(b), st.ModTime().Format(time.RFC3339))
}

// waitCDPReady 确认 CDP 真的能发命令（握手之后立刻探活，带有限重试）。
//
// 只试一次是不够的：Chrome 冷启动头一两秒会接受 WS 连接但不回命令，
// 一次失败就回滚等于把正常冷启动误判成失败；试到天荒地老也不行——
// 那会把「CDP 真的连不上」拖到 20s 之后才回滚。所以是「有限次 + 单次短超时」。
func waitCDPReady(conn *CDPConn, budget time.Duration) error {
	if conn == nil {
		return errors.New("CDP 连接为空")
	}
	per := cdpReadyCall
	if budget > 0 && budget < per {
		per = budget
	}
	var last error
	for i := 0; i < cdpReadyAttempts; i++ {
		callCtx, cancel := context.WithTimeout(context.Background(), per)
		_, last = conn.ListTargets(callCtx)
		cancel()
		if last == nil {
			return nil
		}
		if i+1 < cdpReadyAttempts {
			time.Sleep(cdpReadyInterval)
		}
	}
	return fmt.Errorf("探活 %d 次均未响应: %w", cdpReadyAttempts, last)
}

// retryCDP 对单条 CDP 命令做有限重试（返回最后一次错误）。
//
// 只重试「调用超时/连接抖动」这类可自愈的失败：重试次数很小（默认 2），
// 且每次之间留出间隔，避免把一次浏览器卡顿放大成连环超时。
func retryCDP(ctx context.Context, attempts int, wait time.Duration, call func(context.Context) error) error {
	if attempts <= 0 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		last = call(ctx)
		if last == nil {
			return nil
		}
		if i+1 < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return last
}

// writtenAfter 端口文件是否在 notBefore 之后写出（notBefore 为零值表示不校验）。
func writtenAfter(path string, notBefore time.Time) bool {
	if notBefore.IsZero() {
		return true
	}
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !st.ModTime().Before(notBefore)
}

// portDialable 本机端口当前是否能连上（只用于判断「旧端口文件后面有没有活实例」）。
func portDialable(port int, timeout time.Duration) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// ---------- 小工具 ----------

// rollback 逆序执行的回滚栈。
type rollback struct{ steps []func() }

func (r *rollback) add(f func()) { r.steps = append(r.steps, f) }

func (r *rollback) run() {
	for i := len(r.steps) - 1; i >= 0; i-- {
		r.steps[i]()
	}
}

// dbContext 派生一个「不受调用方取消影响」但有上限的上下文。
//
// 为什么必须 WithoutCancel：状态提交（MarkRunning / StopEnv）是启动链路的最后一步，
// 此时浏览器已经起来了。若因前端断开就把提交一起取消，结果就是「窗口开着、
// 库里说没跑」——这正是整个 M3 时序要消灭的悬空态。
func dbContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}

// resetLaunchFailure 只做失败复位，**不改写错误**（P3）。
//
// 与 failStart 的分工：failStart 用于「本机已经有副作用」的阶段（回滚 + 包错），
// 这一条用于阶段 1 这类「还没碰本机资源」的拒绝——复位要做，但调用方（网关）
// 必须还能看到原始的业务错误码（403 无权 / 409 停用 / 409 出口拨测未通过）。
func (m *Manager) resetLaunchFailure(ctx context.Context, in StartInput, stage, reason string) {
	if m.envs == nil {
		return
	}
	dbctx, cancel := dbContext(ctx)
	defer cancel()
	if _, err := m.envs.MarkLaunchFailed(dbctx, shop.MarkLaunchFailedInput{
		EnvID: in.EnvID, Actor: in.Actor, Terminal: in.Terminal, URL: in.URL,
		Stage: stage, Reason: sanitizeReason(reason, 200),
	}); err != nil {
		m.log.Error("启动失败复位未生效（状态可能滞留）",
			"env", in.EnvID, "stage", stage, "err", err.Error())
	}
}

// failStart 标记启动失败（复位状态 + 返回可读错误）。
//
// reason 会进审计事件，所以**只带阶段与原因**，不带地址/端口/账号/口令：
// 排障需要的细节在服务端日志里，审计事件是要给所有人看的。
func (m *Manager) failStart(ctx context.Context, in StartInput, stage, reason string) error {
	if m.envs != nil {
		dbctx, cancel := dbContext(ctx)
		defer cancel()
		if _, err := m.envs.MarkLaunchFailed(dbctx, shop.MarkLaunchFailedInput{
			EnvID: in.EnvID, Actor: in.Actor, Terminal: in.Terminal, URL: in.URL,
			Stage: stage, Reason: sanitizeReason(reason, 200),
		}); err != nil {
			m.log.Error("启动失败复位未生效（状态可能滞留）",
				"env", in.EnvID, "stage", stage, "err", err.Error())
		}
	}
	return apierr.Internal("环境启动失败（阶段 " + stage + "）：" + sanitizeReason(reason, 200))
}

// sanitizeReason 截断失败原因。
//
// 只截断长度：上游口令走的是 Go 内存 → 本地代理配置这条路径，本就不进错误串；
// 这里再截一刀是为了防止第三方库把整段 URL（常带 user:pass@host）塞进 error。
func sanitizeReason(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// proxyConfigOf 启动单 → 本地代理配置。
func proxyConfigOf(spec *shop.LaunchSpec, caDir string) network.TLSProxyConfig {
	cfg := network.TLSProxyConfig{
		HelloID:     spec.TLSHelloID,
		UA:          spec.UserAgent,
		ChromeMajor: spec.KernelMajor,
		Platform:    platformOf(spec.Snapshot),
		CADir:       caDir,
		Upstream:    network.ProxyConfig{Protocol: network.ProtoDirect},
	}
	if u := spec.Upstream; u != nil && u.Host != "" {
		cfg.Upstream = network.ProxyConfig{
			Protocol: network.Protocol(u.Protocol), Host: u.Host, Port: u.Port,
			Username: u.Username, Password: u.Password, Country: u.Country,
		}
	}
	return cfg
}

// exitModeOf 出口模式（只用于展示与日志，不含任何地址/凭据）。
func exitModeOf(spec *shop.LaunchSpec) string {
	switch spec.ExitMode {
	case ExitManual, ExitPool:
		return spec.ExitMode
	}
	if spec.Upstream != nil {
		return ExitPool
	}
	return ExitDirect
}

// secretsOf 启动单里所有**不得出现在 argv** 的机密串。
//
// 只取上游凭据：脚本正文与快照不进 argv 是 BuildArgv 的结构保证（压根没有这些字段），
// 而凭据是从业务侧传进来的外部输入，必须靠这道扫描拦住。
func secretsOf(spec *shop.LaunchSpec) []string {
	if spec.Upstream == nil {
		return nil
	}
	return []string{spec.Upstream.Password, spec.Upstream.Username}
}

// platformOf 快照的 navigator.platform → UA-CH 的 platform 口径（"Windows"/"macOS"/"Linux"）。
func platformOf(snapshot map[string]string) string {
	switch strings.ToLower(snapshot["platform"]) {
	case "win32", "windows", "win64":
		return "Windows"
	case "macintel", "macos", "mac68k", "macppc":
		return "macOS"
	case "linux x86_64", "linux", "linux i686":
		return "Linux"
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
