package browser

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"ziniao/network"
)

// Options 装配参数（来自 config.Config，见 app 装配处）。
type Options struct {
	DataDir    string // ZINIAO_DATA_DIR（默认 ./data）
	CADir      string // ZINIAO_CA_DIR（留空 → $DataDir/ca）
	ChromePath string // ZINIAO_CHROME_PATH（留空 → 自动探测）
	Sidepanel  bool   // ZINIAO_SIDEPANEL（默认 false）
	Log        *slog.Logger
	// SingleInstance 是否抢占单实例锁（默认 true）。
	// 测试与排障工具传 false：它们要和常驻实例共存，抢占会把正常实例顶成「已在运行」。
	SingleInstance *bool

	// ---------- M3：启动/停止时序参数 ----------
	// CDPTimeout 等 DevToolsActivePort + CDP 握手的上限（0 → 20s）。
	// 这是「慢」和「失败」的分界线：给太短会把冷启动误判成失败并触发回滚。
	CDPTimeout time.Duration
	// StopGrace 优雅关窗口的宽限期，超时才强杀（0 → 5s）。
	StopGrace time.Duration
	// OrphanPolicy 对账遇到孤儿进程的处理：空/"close" → 关掉（默认）；
	// "keep" → 保留并仅提示。生产只认 close（见 design-m1-browser-manager.md §4.3）。
	OrphanPolicy string
}

// Manager 本机浏览器进程编排入口。
//
// 锁边界（与 docs/design-m1-browser-manager.md §9 一致）：
//   - mu：保护 perEnv 这个 map 本身（读多写少，读走 RLock）；
//   - perEnv[envID]：单环境的启动/停止串行化——不同环境完全并发，同一环境排队；
//   - 端口：不加锁（CDP 由 OS 分配；本地代理持 listener，OS 保证唯一）；
//   - 注册表：Registry 自己的 RWMutex + 落盘互斥。
//
// 铁律：**绝不在数据库事务里等浏览器**。CDP 连通要等十几秒，事务里等它等于
// 用一次浏览器卡顿拖垮整库（mem 驱动还是全局串行锁）。因此 M3 的启动时序把
// 「写 running」拆到 CDP 连通之后（EnvService.MarkRunning），本包只负责进程与端口。
type Manager struct {
	mu     sync.RWMutex
	perEnv map[string]*sync.Mutex

	layout   Layout
	ca       CAStatus
	browser  BrowserInfo
	registry *Registry
	lock     *InstanceLock
	log      *slog.Logger
	// sidepanel 是否允许加载侧边栏扩展（默认 false，见 ArgvConfig 注释）。
	sidepanel bool
	// secondInstance 第二个启动器实例：只读，不允许拉起环境（批复 3.4）。
	secondInstance bool
	// reconcileOK 上次对账是否可用（进程快照拿不到时降级为 false）。
	reconcileOK bool

	// ---------- M3：启动/停止链路 ----------
	// envs 业务侧环境服务（启动单 + running 提交 + 失败复位 + 停止语义）。
	// 走接口而不是直接用 *shop.EnvService：启动链路的失败分支要靠假实现逐条断言。
	envs EnvControl
	// sessions 本机活着的会话（进程句柄 + 本地代理 + CDP 连接），内存态。
	// 注册表只是磁盘缓存，重启后靠进程命令行重挂；连接类资源没有持久化的意义。
	sessMu   sync.Mutex
	sessions map[string]*Session
	// 三个可替换的动作：拉起进程 / 起本地代理 / 连 CDP。
	// 抽出来只为可测性——失败回滚矩阵必须能在没有 Chrome 的机器上跑。
	startProc  procStarter
	startProxy proxyStarter
	cdpDial    cdpDialer
	// cdpTimeout 等 DevToolsActivePort 与 CDP 握手的上限（默认 20s）。
	cdpTimeout time.Duration
	// stopGrace 优雅关窗口的宽限期，超时才强杀（默认 5s）。
	stopGrace time.Duration
	// orphanPolicy 对账时遇到孤儿进程：close（关掉，默认）/ keep（留着只提示）。
	orphanPolicy string
}

// New 构造 Manager：解析目录 → 抢单实例锁 → 载注册表 → 探测浏览器 → 查 CA → 对账。
//
// 顺序有意义：对账放在对外提供服务之前（此时还没有任何 launch 请求），
// 天然排除「对账把正在启动的进程关掉」这个竞态。
func New(o Options) (*Manager, error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	layout, err := NewLayout(o.DataDir, o.CADir)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		perEnv:       map[string]*sync.Mutex{},
		layout:       layout,
		log:          log,
		sidepanel:    o.Sidepanel,
		ca:           CheckCA(layout.CADir),
		browser:      Locator{Override: o.ChromePath}.Detect(),
		sessions:     map[string]*Session{},
		startProc:    StartChrome,
		cdpDial:      DialCDP,
		cdpTimeout:   o.CDPTimeout,
		stopGrace:    o.StopGrace,
		orphanPolicy: o.OrphanPolicy,
	}
	m.startProxy = func(cfg network.TLSProxyConfig) (ProxyHandle, error) {
		return startLocalProxy(cfg)
	}
	if !m.browser.Installed {
		log.Warn("未探测到本机 Chrome/Edge：环境启动不可用", "data_dir", layout.DataDir)
	}
	if !m.ca.Ready {
		log.Warn("MITM 根证书未就绪：启动流程会被阻断", "ca_dir", layout.CADir, "detail", m.ca.Detail)
	}
	if reg, rerr := LoadRegistry(layout.RegistryPath()); rerr != nil {
		// 注册表损坏不致命：它只是缓存，最坏的结果是「本机看不见已有环境」，
		// 下次启动成功会重新登记。
		log.Warn("运行时注册表不可用，按空注册表启动", "err", rerr.Error())
		m.registry = &Registry{path: layout.RegistryPath(), entries: map[string]*RuntimeEntry{}}
	} else {
		m.registry = reg
	}

	if wantLock(o.SingleInstance) {
		lk, lerr := AcquireInstanceLock(layout.LockPath())
		if lerr == ErrAlreadyRunning {
			// 第二实例：只读打开，不抢锁、不对账（对账会误伤正在跑的环境）。
			m.secondInstance = true
			log.Warn("启动器已在运行：本次以只读方式打开，不允许重复拉起环境")
		} else if lerr != nil {
			return nil, lerr
		} else {
			m.lock = lk
		}
	}
	if !m.secondInstance {
		m.reconcileOK = true
		m.reconcile()
	}
	return m, nil
}

func wantLock(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}

// ---------- 只读视图（M1 的 /api/v1/system/info 用它） ----------

// Layout 数据目录布局。
func (m *Manager) Layout() Layout { return m.layout }

// Browser 浏览器探测结论。
func (m *Manager) Browser() BrowserInfo { return m.browser }

// CA 本地 CA 状态（M1：ca_ready 的唯一来源）。
func (m *Manager) CA() CAStatus { return m.ca }

// CAReady CA 是否就绪（启动前置条件）。
func (m *Manager) CAReady() bool { return m.ca.Ready }

// RefreshCA 重新检查 CA 状态（安装/卸载之后调用，避免整进程重启）。
func (m *Manager) RefreshCA() CAStatus { m.ca = CheckCA(m.layout.CADir); return m.ca }

// InstallCA 生成（若缺）并安装本地 CA 到当前用户信任库（M3/T7）。
//
// 幂等：已安装时直接返回当前状态。装完立即刷新 m.ca，让随后的
// capabilities.launch 与 system/info 的 ca_ready 马上反映新状态——
// 「点了安装但仍显示未就绪」是最容易被当成 bug 的体验。
func (m *Manager) InstallCA() (CAStatus, error) {
	if _, err := EnsureCA(m.layout.CADir); err != nil {
		m.ca = CheckCA(m.layout.CADir)
		m.log.Error("MITM 根证书安装失败", "ca_dir", m.layout.CADir, "err", err.Error())
		return m.ca, err
	}
	m.ca = CheckCA(m.layout.CADir)
	m.log.Info("MITM 根证书已安装到当前用户信任库", "ca_dir", m.layout.CADir, "ready", m.ca.Ready)
	return m.ca, nil
}

// UninstallCA 从当前用户信任库移除本地 CA（卸载 / 合规要求），并立即刷新状态。
//
// 卸载后 ca_ready=false ⇒ 启动前置检查会阻断，这是**预期**行为：
// 留着一个能起来但每个 HTTPS 站点都报错的环境，比明确告诉运营「CA 没了」更糟。
func (m *Manager) UninstallCA() (CAStatus, error) {
	if err := UninstallCA(m.layout.CADir); err != nil {
		m.log.Error("MITM 根证书卸载失败", "ca_dir", m.layout.CADir, "err", err.Error())
		return m.ca, err
	}
	m.ca = CheckCA(m.layout.CADir)
	m.log.Info("MITM 根证书已从当前用户信任库移除", "ca_dir", m.layout.CADir)
	return m.ca, nil
}

// SecondInstance 本次是否是「已有实例在运行」的只读实例。
func (m *Manager) SecondInstance() bool { return m.secondInstance }

// SetEnvControl 接入业务侧环境服务（M3 启动链路需要它下发启动单与提交状态）。
//
// 为什么是注入而不是在 New 里直接 new：browser 不该知道 store / bus / 画像库怎么装配，
// 而且失败回滚矩阵要靠假实现把 8 个阶段逐个打断点。
func (m *Manager) SetEnvControl(c EnvControl) { m.envs = c }

// CDPTimout CDP 等待上限（只读，供排障展示）。
func (m *Manager) cdpWait() time.Duration {
	if m.cdpTimeout <= 0 {
		return 20 * time.Second
	}
	return m.cdpTimeout
}

// gracePeriod 优雅关闭宽限期（只读）。
func (m *Manager) gracePeriod() time.Duration {
	if m.stopGrace <= 0 {
		return 5 * time.Second
	}
	return m.stopGrace
}

// Capabilities 能力位：前端据此决定引导卡文案与按钮可用性。
//
// launch 的条件里包含 CA 就绪：本地代理是 MITM 架构，CA 不在信任库里就是
// 「能起来但每个 HTTPS 站点都报证书错误」——那不是可用状态，不能算 true。
func (m *Manager) Capabilities() map[string]bool {
	return map[string]bool{
		"launch":      m.browser.Installed && m.ca.Ready && !m.secondInstance,
		"cdp":         m.browser.Installed,
		"local_proxy": true, // network.TLSProxy.ServeListener 常驻可用
		"mitm":        m.ca.Present,
		"sidepanel":   sidepanelEnabled(m),
		"reconcile":   m.reconcileOK && !m.secondInstance,
	}
}

func sidepanelEnabled(m *Manager) bool {
	if m.secondInstance {
		return false
	}
	// 目录建好但开关关着 = 「可选能力」，不算当前能力。
	return m.sidepanel && dirExists(m.layout.SidepanelDir)
}

// ---------- 运行时注册表 ----------

// Runtime 本机运行时快照（供 /api/v1/environments/runtime 与排障用）。
//
// 快照里**不含**任何上游凭据与注入脚本正文——排障页面会被截图发到群里，
// 那是凭据最常见的外泄路径。
func (m *Manager) Runtime() []RuntimeEntry {
	if m.registry == nil {
		return []RuntimeEntry{}
	}
	return m.registry.Snapshot()
}

// RuntimeOf 单个环境的本机运行时。
func (m *Manager) RuntimeOf(envID string) (RuntimeEntry, bool) {
	if m.registry == nil {
		return RuntimeEntry{}, false
	}
	return m.registry.Get(envID)
}

// Register 登记运行时（M3 启动成功后调用）。第二实例拒绝登记：它不负责进程，
// 登记进去只会让真实例的下次对账把它当成孤儿。
func (m *Manager) Register(e RuntimeEntry) error {
	if m.secondInstance {
		return fmt.Errorf("browser: 只读实例不允许登记运行时")
	}
	if e.EnvID == "" {
		return fmt.Errorf("browser: 环境 ID 为空，拒绝登记")
	}
	m.registry.Upsert(e)
	return m.registry.Flush()
}

// Unregister 移除运行时（M3 停止完成后调用）。
func (m *Manager) Unregister(envID string) error {
	if m.secondInstance {
		return fmt.Errorf("browser: 只读实例不允许修改运行时")
	}
	m.registry.Remove(envID)
	return m.registry.Flush()
}

// ---------- 单环境串行化 ----------

// envMutex 取（或建）某环境的互斥锁。
func (m *Manager) envMutex(envID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.perEnv == nil {
		m.perEnv = map[string]*sync.Mutex{}
	}
	lk, ok := m.perEnv[envID]
	if !ok {
		lk = &sync.Mutex{}
		m.perEnv[envID] = lk
	}
	return lk
}

// WithEnvLock 在同一环境上串行执行 fn。
//
// 为什么是「按环境」而不是一把大锁：多环境并发启动是日常操作，一把大锁会让
// 第 20 个环境的启动排在第 1 个环境的 CDP 超时（20s）后面——那不是串行化，
// 是把并发度锁死成 1。
func (m *Manager) WithEnvLock(envID string, fn func() error) error {
	lk := m.envMutex(envID)
	lk.Lock()
	defer lk.Unlock()
	return fn()
}

// ---------- 收尾 ----------

// Close 释放实例锁（registry 已在每次变更时落盘，无需额外 flush）。
func (m *Manager) Close() error {
	if m.lock == nil {
		return nil
	}
	err := m.lock.Release()
	m.lock = nil
	return err
}

// ErrBrowserMissing 本机没有可用浏览器（启动前置检查失败）。
var ErrBrowserMissing = errBrowserMissing()

func errBrowserMissing() error {
	return fmt.Errorf("browser: 未检测到 Chrome/Edge（设置 ZINIAO_CHROME_PATH 指定路径）")
}

// Precheck 启动前置检查（M3 的 Stage 2 复用，M1 先落地断言口径）。
//
// 检查项：浏览器存在、CA 就绪、profile 目录可建、非只读实例。
// 任一不满足都返回明确错误——「浏览器启动失败」这种笼统文案会让人反复重试，
// 而重试对「没装 Chrome」这类问题没有任何意义。
func (m *Manager) Precheck(envID string) (string, error) {
	if m.secondInstance {
		return "", fmt.Errorf("browser: 启动器已在运行，本实例为只读，不允许拉起环境")
	}
	if !m.browser.Installed {
		return "", ErrBrowserMissing
	}
	if !m.ca.Ready {
		return "", ErrCANotReady
	}
	dir, err := m.layout.EnsureProfileDir(envID)
	if err != nil {
		return "", err
	}
	return dir, nil
}

// UpstreamKind 上游出口来源（仅用于日志与事件，不含任何地址/端口/凭据）。
//
// 单独定义它而不是直接打 network.ProxyConfig：日志里一旦顺手打了整个结构体，
// 上游用户名与口令就跟着进了日志文件——这是「顺手」最常见的代价。
func UpstreamKind(p *network.ProxyConfig) string {
	if p == nil || p.Protocol == network.ProtoDirect {
		return ExitDirect
	}
	return ExitPool
}
