// Package config 配置中心客户端：所有硬编码数值（人数上限、留存天数、额度、
// 超时、次数限制）必须在此登记，禁止散落在业务代码里（主提示词代码规范 / L10）。
//
// 加载优先级：默认值 < 配置文件 < 环境变量（ZINIAO_*）。
// TODO(P1)：接入真正的配置中心（Nacos/Apollo）+ 租户级覆写（tenant_settings）。
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Limits 所有可调数值的单一真相源。
type Limits struct {
	// 设备调度（L9：配额模型，不是互斥锁）
	DeviceMaxConcurrentUsers int `key:"device.max_concurrent_users"` // 默认 6
	DeviceMaxBoundAccounts   int `key:"device.max_bound_accounts"`   // 默认 20
	DeviceHardLimitUsers     int `key:"device.hard_limit_users"`     // 软阈值 +2，达此值拒绝

	// 店铺（M2）
	ShopNameMaxLen int `key:"shop.name_max_len"` // 60
	ShopMaxTags    int `key:"shop.max_tags"`

	// 审计与合规（M15）
	AuditRetentionDaysDefault int      `key:"audit.retention_days"`          // 180
	AuditPermanentCategories  []string `key:"audit.permanent_categories"`    // 登录/权限变更/高危操作永久留存
	SnapshotRetentionDays     int      `key:"audit.snapshot_retention_days"` // 30

	// 导出（M15.2 / 反例 8）
	ExportMaxRows           int  `key:"export.max_rows"` // 50000
	ExportPlainApproval     bool `key:"export.plain_requires_approval"`
	ExportWatermarkRequired bool `key:"export.watermark_required"`

	// 财务 / 收款（M7 / M13）
	CurrencySnapshotRetentionDays int     `key:"finance.fx_snapshot_retention_days"` // 180
	WithdrawApprovalThresholdCNY  float64 `key:"finance.withdraw_approval_threshold_cny"`
	WithdrawWhitelistCooldownH    int     `key:"finance.withdraw_cooldown_hours"` // 24

	// 设备申购会签阈值（Phase 2 / 双人复核）
	// 不要复用上面的提现阈值：提现和设备申购是两种完全不同的资金动作，
	// 共用一个数值意味着调整财务风控会**静默地**改变双人复核的触发线。
	// 取值一律走 PurchaseOwnerThresholdCNY()，不要在业务代码里读裸字段。
	DevicePurchaseOwnerThresholdCNY float64 `key:"device.purchase_owner_threshold_cny"`

	// 身份与登录保护（M12 / M14）
	PasswordMinLen        int           `key:"auth.password_min_len"`
	MaxFailedLogins       int           `key:"auth.max_failed_logins"`
	LockoutDuration       time.Duration `key:"auth.lockout_duration"`
	AccessTokenTTL        time.Duration `key:"auth.access_token_ttl"`
	SessionIdleTimeout    time.Duration `key:"auth.session_idle_timeout"`
	LoginAllowedStartHour int           `key:"auth.login_allowed_start_hour"` // 0-24，0&24=不限
	LoginAllowedEndHour   int           `key:"auth.login_allowed_end_hour"`

	// 环境回收：running 状态超过这个时长仍无关闭上报，巡检任务把它置回 idle。
	// 这是"客户端崩溃/断电来不及上报"的兜底，不是精确判定——
	// 取得太小会把正在挂机的环境误判成空闲（只影响状态显示，不影响运行中的窗口）。
	EnvReclaimHours int `key:"env.reclaim_hours"` // 12

	// Agent 人工确认闸门（M19.2）
	AgentConfirmTTLSeconds int      `key:"agent.confirm_ttl_seconds"` // 1800
	AgentAlwaysConfirmOps  []string `key:"agent.always_confirm_ops"`  // withdraw|price_change|bulk_delete|account_change

	// 临时授权（M12）
	TempGrantMaxDays int `key:"iam.temp_grant_max_days"` // 30

	// 审批工单（Phase 2 / 反例 8）
	ApprovalTTLSeconds      int      `key:"approval.ttl_seconds"` // 1800
	ApprovalRequiredActions []string `key:"approval.required_actions"`

	// 定时任务（Phase 2）
	ShopTrashRetentionDays int `key:"shop.trash_retention_days"` // 30
	DeviceExpireWarnDays   int `key:"device.expire_warn_days"`   // 7
	DeviceHealthMinScore   int `key:"device.health_min_score"`   // 60

	// 出口绑定历史保留期（Phase 2D）：超过该天数的 egress_binding_history 行由
	// network.egress_history_prune 物理裁剪。<= 0 表示不裁剪（历史是资产，
	// 共用 ASN 的证据链怕断不怕长）；默认 180 与审计留存对齐。
	EgressHistoryRetentionDays int `key:"egress.history_retention_days"` // 180

	// 网关
	APIRatePerMinute  int `key:"api.rate_per_minute"`
	APIDailyQuotaFree int `key:"api.daily_quota_free"`

	// 监控回放（M16）
	RecordingRetentionDaysFree int `key:"replay.retention_days_free"` // 7
	RecordingSegmentSeconds    int `key:"replay.segment_seconds"`     // 30
}

// Notify 通知外发通道配置。
//
// L4：SMTP 口令与 Webhook 签名密钥只经环境变量注入，不进 argv（wmic 可读命令行）、
// 不进日志、不出现在任何 API 响应里——与 ZINIAO_IP_TOKEN 同一口径。
// 通道未配置即视为关闭：宁可不发，也不发到一个「看起来配过」的地址上。
type Notify struct {
	SMTPHost    string        `key:"notify.smtp_host"` // 留空 = 邮件通道关闭
	SMTPPort    int           `key:"notify.smtp_port"`
	SMTPUser    string        `key:"notify.smtp_user"`
	SMTPPass    string        `key:"notify.smtp_pass"`
	SMTPFrom    string        `key:"notify.smtp_from"`     // 留空则回落到 SMTPUser
	SMTPTLSMode string        `key:"notify.smtp_tls_mode"` // starttls|tls|none
	SMTPTimeout time.Duration `key:"notify.smtp_timeout"`

	WebhookURL           string        `key:"notify.webhook_url"` // 留空 = Webhook 通道关闭
	WebhookSecret        string        `key:"notify.webhook_secret"`
	WebhookTimeout       time.Duration `key:"notify.webhook_timeout"`
	WebhookMaxRetry      int           `key:"notify.webhook_max_retry"`
	WebhookAllowLoopback bool          `key:"notify.webhook_allow_loopback"` // dev/local only

	// 短信（阿里云云市场「阿里短信服务」，AppKey+AppSecret 签名认证）。
	// 与邮件同一口径：AppKey/AppSecret 只经环境变量注入，不进 argv、不进日志、
	// 不出现在任何 API 响应里。
	SMSProvider      string        `key:"notify.sms_provider"` // aliyun_market（默认）
	SMSAppKey        string        `key:"notify.sms_app_key"`  // 留空 = 短信通道关闭
	SMSAppSecret     string        `key:"notify.sms_app_secret"`
	SMSSignName      string        `key:"notify.sms_sign_name"`     // 控制台审核通过的签名
	SMSTemplateCode  string        `key:"notify.sms_template_code"` // 如 SMS_492460356
	SMSTemplateKey   string        `key:"notify.sms_template_key"`  // 模板变量名，默认 content
	SMSTimeout       time.Duration `key:"notify.sms_timeout"`
	SMSMaxRetry      int           `key:"notify.sms_max_retry"`      // 默认 0：宁可漏发也不重复发
	SMSAllowLoopback bool          `key:"notify.sms_allow_loopback"` // dev/local only

	MaxAttempts int    // 含首次在内的单通道投递上限，超过后留给重投任务并最终置 failed
	MinRisk     string // high（默认）| medium
}

// SMSConfigured 是否配置了短信通道的**任一项**。
//
// 用于识别"半配置"：填了 AppKey 却没填模板 CODE 的通道看起来配过、实际一发就 400，
// 运营会以为告警已经发出去了——这比不配更糟，必须在启动装配阶段就炸出来。
func (n Notify) SMSConfigured() bool {
	return n.SMSAppKey != "" || n.SMSAppSecret != "" || n.SMSSignName != "" || n.SMSTemplateCode != ""
}

// Config 进程级配置。
type Config struct {
	Env string `key:"env"` // dev|test|prod
	// HTTPAddr 监听地址。默认只绑回环（127.0.0.1:8080）：启动器是本机桌面形态，
	// 绑 0.0.0.0 会把本地 CDP / 本地代理 / 控制台一起暴露到局域网。
	HTTPAddr    string `key:"http.addr"`
	StoreDriver string `key:"store.driver"` // memory|postgres
	PostgresDSN string `key:"store.postgres_dsn"`
	JWTSecret   string `key:"auth.jwt_secret"`
	// KEKKeys 是凭证信封加密的主密钥环，格式由 ZINIAO_KEK_KEYS 注入：
	// "1:<base64-32-byte-key>,2:<base64-32-byte-key>"。旧版本必须保留用于解密。
	KEKKeys map[int][]byte
	KEKCurrentVersion int
	KEKConfigError string

	// DataDir 数据根目录（M1）。profiles / extensions / runtime / logs / ca 都落在它下面。
	// 相对路径在装配阶段统一转绝对路径，避免「进程换了工作目录就换了数据目录」。
	DataDir string `key:"data.dir"`
	// CADir MITM 根证书目录；留空回落到 $DataDir/ca。
	CADir string `key:"ca.dir"`
	// ChromePath 浏览器可执行文件覆盖（自动探测不到时手工指定）。
	ChromePath string `key:"browser.chrome_path"`
	// Sidepanel 是否在启动参数里加载侧边栏扩展。默认 false：未打包扩展会触发
	// Chrome 的「请停用开发者模式扩展」提示条，干扰指纹验收观感。
	Sidepanel bool `key:"browser.sidepanel"`
	// Demo 站点：账号隔离实测用（/demo/*）。生产必须为 false。
	DemoSite bool `key:"demo_site"`
	// ProxyTestEnabled 服务端代理拨测总开关（M2 收尾 / T12；P3 口径见下）。
	//
	// on（默认）= 严格：绑定代理前与启动绑定了代理的环境前都要拨测，
	//   fail 即阻断（拒绝绑定 / 拒绝启动）。
	// off = 跳过健康校验：允许绑定、允许启动（不拨测），只在日志与审计留 WARN。
	//   off 的语义是「这条出网路径被部署侧摘除」，不是「代理不通」——
	//   因此 off **绝不能**被判成 fail，也不能因此禁止绑定或启动。
	ProxyTestEnabled bool `key:"proxy.test_enabled"`
	// ProxyTestTarget 拨测默认目标（部署侧可换成内网可达的纯 IP 回显服务）。
	ProxyTestTarget string `key:"proxy.test_target"`
	// ProxyTestCountryTarget 出口国家实测目标（P3），**默认空 = 不实测**。
	//
	// 空：不额外出网，exit_country 沿用台账录入的 country，一致性软校验也用它。
	// 非空（如 https://ipinfo.io/json）：拨测时再取一次 JSON 里的 country 字段。
	// 实测失败**不致命**：国家拿不到就留空并记 WARN，不得拖垮拨测与启动——
	// 为了一个展示字段把环境挡在启动之外，是典型的「辅助信息绑架主流程」。
	ProxyTestCountryTarget string `key:"proxy.test_country_target"`
	// 事件总线
	DispatcherBatchSize int           `key:"bus.batch_size"`
	DispatcherInterval  time.Duration `key:"bus.interval"`
	DispatcherMaxRetry  int           `key:"bus.max_retry"`
	JobInterval         time.Duration `key:"job.interval"` // 定时巡检间隔
	Limits              Limits
	Notify              Notify // 外发通道（邮件/Webhook），未配即关闭
}

// Default 返回带合理默认值的配置（未标注规格数值的部分按 TODO 处理）。
func Default() *Config {
	return &Config{
		Env:      "dev",
		HTTPAddr: "127.0.0.1:8080",
		DataDir:  "./data",
		// CA 目录留空 → 装配阶段回落到 $DataDir/ca（见 browser 包 Layout）。
		CADir:               "",
		Sidepanel:           false,
		ProxyTestEnabled:    true,
		ProxyTestTarget:     "https://api.ipify.org",
		// 国家实测默认关：多一次外部请求 = 多一个拨测失败点，而它只影响展示。
		ProxyTestCountryTarget: "",
		StoreDriver:         "memory",
		PostgresDSN:         "postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable",
		JWTSecret:           "dev-only-secret-change-me",
		DispatcherBatchSize: 100,
		DispatcherInterval:  200 * time.Millisecond,
		DispatcherMaxRetry:  5,
		JobInterval:         10 * time.Minute,
		Limits: Limits{
			DeviceMaxConcurrentUsers:      6, // 规格明确值
			DeviceMaxBoundAccounts:        20,
			DeviceHardLimitUsers:          8,
			ShopNameMaxLen:                60,
			ShopMaxTags:                   10,
			AuditRetentionDaysDefault:     180, // 规格明确值
			AuditPermanentCategories:      []string{"login", "grant", "risk"},
			SnapshotRetentionDays:         30,
			ExportMaxRows:                 50000,
			ExportPlainApproval:           true,
			ExportWatermarkRequired:       true,
			CurrencySnapshotRetentionDays: 180, // 规格明确值
			WithdrawApprovalThresholdCNY:  50000,
			WithdrawWhitelistCooldownH:    24,
			// 设备申购双人复核阈值：与提现阈值取同一默认值，但必须能各自调整
			DevicePurchaseOwnerThresholdCNY: defaultPurchaseOwnerThresholdCNY,
			PasswordMinLen:                  10,
			MaxFailedLogins:                 5,
			LockoutDuration:                 15 * time.Minute,
			AccessTokenTTL:                  8 * time.Hour,
			SessionIdleTimeout:              30 * time.Minute,
			LoginAllowedStartHour:           0,
			LoginAllowedEndHour:             24,
			AgentConfirmTTLSeconds:          1800,
			AgentAlwaysConfirmOps:           []string{"withdraw", "price_change", "bulk_delete", "account_change"},
			TempGrantMaxDays:                30,
			ApprovalTTLSeconds:              1800,
			ApprovalRequiredActions: []string{
				"audit.export.plain", "shop.credential.reveal", "shop.bulk_delete",
			},
			ShopTrashRetentionDays:     30,
			EnvReclaimHours:            12,
			DeviceExpireWarnDays:       7,
			DeviceHealthMinScore:       60,
			EgressHistoryRetentionDays: 180,
			APIRatePerMinute:           600,
			APIDailyQuotaFree:          1000,
			RecordingRetentionDaysFree: 7,
			RecordingSegmentSeconds:    30,
		},
		// 外发通道默认全关：配了才有 or 没有，不存在"半配置"。
		Notify: Notify{
			SMTPPort:        587, // 587=STARTTLS（Submission），465=隐式 TLS
			SMTPTLSMode:     "starttls",
			SMTPTimeout:     10 * time.Second,
			WebhookURL:      "",
			WebhookTimeout:  5 * time.Second,
			WebhookMaxRetry: 2,
			SMSProvider:     "aliyun_market",
			SMSTimeout:      5 * time.Second,
			// 短信默认**不重试**：超时失败往往意味着对方其实已收下，再发一次就是把
			// 同一条告警发两遍（重复扣费 + 把人烦到静音）。漏发交给重投任务兜底。
			SMSMaxRetry: 0,
			MaxAttempts: 3,
			MinRisk:     "high",
		},
	}
}

// Load 从默认值 + 环境变量构造配置。
func Load() *Config {
	c := Default()
	c.Env = envStr("ZINIAO_ENV", c.Env)
	c.HTTPAddr = envStr("ZINIAO_HTTP_ADDR", c.HTTPAddr)
	c.StoreDriver = envStr("ZINIAO_STORE_DRIVER", c.StoreDriver)
	c.PostgresDSN = envStr("ZINIAO_POSTGRES_DSN", c.PostgresDSN)
	c.JWTSecret = envStr("ZINIAO_JWT_SECRET", c.JWTSecret)
	currentKEKVersion := 1
	if rawVersion := strings.TrimSpace(os.Getenv("ZINIAO_KEK_CURRENT_VERSION")); rawVersion != "" {
		parsed, parseErr := strconv.Atoi(rawVersion)
		if parseErr != nil || parsed <= 0 {
			c.KEKConfigError = "ZINIAO_KEK_CURRENT_VERSION 必须是正整数"
		} else {
			currentKEKVersion = parsed
		}
	}
	var keyringErr string
	c.KEKKeys, c.KEKCurrentVersion, keyringErr = parseKEKKeyring(os.Getenv("ZINIAO_KEK_KEYS"), currentKEKVersion)
	if c.KEKConfigError == "" {
		c.KEKConfigError = keyringErr
	}
	c.DemoSite = envBool("ZINIAO_DEMO_SITE", c.DemoSite)
	// 启动器内核（M1）
	c.DataDir = envStr("ZINIAO_DATA_DIR", c.DataDir)
	c.CADir = envStr("ZINIAO_CA_DIR", c.CADir)
	c.ChromePath = envStr("ZINIAO_CHROME_PATH", c.ChromePath)
	c.Sidepanel = envBool("ZINIAO_SIDEPANEL", c.Sidepanel)
	c.DispatcherBatchSize = envInt("ZINIAO_BUS_BATCH", c.DispatcherBatchSize)
	c.DispatcherInterval = envDur("ZINIAO_BUS_INTERVAL", c.DispatcherInterval)
	c.DispatcherMaxRetry = envInt("ZINIAO_BUS_MAX_RETRY", c.DispatcherMaxRetry)
	c.JobInterval = envDur("ZINIAO_JOB_INTERVAL", c.JobInterval)

	l := &c.Limits
	l.DeviceMaxConcurrentUsers = envInt("ZINIAO_LIMIT_DEVICE_CONCURRENT_USERS", l.DeviceMaxConcurrentUsers)
	l.DeviceHardLimitUsers = envInt("ZINIAO_LIMIT_DEVICE_HARD_USERS", l.DeviceHardLimitUsers)
	l.AuditRetentionDaysDefault = envInt("ZINIAO_LIMIT_AUDIT_RETENTION_DAYS", l.AuditRetentionDaysDefault)
	l.ExportMaxRows = envInt("ZINIAO_LIMIT_EXPORT_MAX_ROWS", l.ExportMaxRows)
	l.APIRatePerMinute = envInt("ZINIAO_LIMIT_API_RATE_PER_MINUTE", l.APIRatePerMinute)
	l.DevicePurchaseOwnerThresholdCNY = envFloat("ZINIAO_LIMIT_DEVICE_PURCHASE_THRESHOLD_CNY", l.DevicePurchaseOwnerThresholdCNY)
	l.EnvReclaimHours = envInt("ZINIAO_LIMIT_ENV_RECLAIM_HOURS", l.EnvReclaimHours)
	l.EgressHistoryRetentionDays = envInt("ZINIAO_LIMIT_EGRESS_HISTORY_RETENTION_DAYS", l.EgressHistoryRetentionDays)
	// 外发通道：凭据只走环境变量（详见 Notify 注释）
	n := &c.Notify
	n.SMTPHost = envStr("ZINIAO_SMTP_HOST", n.SMTPHost)
	n.SMTPPort = envInt("ZINIAO_SMTP_PORT", n.SMTPPort)
	n.SMTPUser = envStr("ZINIAO_SMTP_USER", n.SMTPUser)
	n.SMTPPass = envStr("ZINIAO_SMTP_PASS", n.SMTPPass)
	n.SMTPFrom = envStr("ZINIAO_SMTP_FROM", n.SMTPFrom)
	n.SMTPTLSMode = envStr("ZINIAO_SMTP_TLS_MODE", n.SMTPTLSMode)
	n.SMTPTimeout = envDur("ZINIAO_SMTP_TIMEOUT", n.SMTPTimeout)
	n.WebhookURL = envStr("ZINIAO_WEBHOOK_URL", n.WebhookURL)
	n.WebhookSecret = envStr("ZINIAO_WEBHOOK_SECRET", n.WebhookSecret)
	n.WebhookTimeout = envDur("ZINIAO_WEBHOOK_TIMEOUT", n.WebhookTimeout)
	n.WebhookMaxRetry = envInt("ZINIAO_WEBHOOK_MAX_RETRY", n.WebhookMaxRetry)
	n.WebhookAllowLoopback = envBool("ZINIAO_WEBHOOK_ALLOW_LOOPBACK", n.WebhookAllowLoopback)
	n.SMSProvider = envStr("ZINIAO_SMS_PROVIDER", n.SMSProvider)
	n.SMSAppKey = envStr("ZINIAO_SMS_APP_KEY", n.SMSAppKey)
	n.SMSAppSecret = envStr("ZINIAO_SMS_APP_SECRET", n.SMSAppSecret)
	n.SMSSignName = envStr("ZINIAO_SMS_SIGN_NAME", n.SMSSignName)
	n.SMSTemplateCode = envStr("ZINIAO_SMS_TEMPLATE_CODE", n.SMSTemplateCode)
	n.SMSTemplateKey = envStr("ZINIAO_SMS_TEMPLATE_KEY", n.SMSTemplateKey)
	n.SMSTimeout = envDur("ZINIAO_SMS_TIMEOUT", n.SMSTimeout)
	n.SMSMaxRetry = envInt("ZINIAO_SMS_MAX_RETRY", n.SMSMaxRetry)
	n.SMSAllowLoopback = envBool("ZINIAO_SMS_ALLOW_LOOPBACK", n.SMSAllowLoopback)
	n.MaxAttempts = envInt("ZINIAO_NOTIFY_MAX_ATTEMPTS", n.MaxAttempts)
	n.MinRisk = envStr("ZINIAO_NOTIFY_MIN_RISK", n.MinRisk)
	// M2 收尾 / T12：拨测出网开关与目标地址（默认值见 Default()）。
	c.ProxyTestEnabled = envOnOff("ZINIAO_PROXY_TEST", c.ProxyTestEnabled)
	c.ProxyTestTarget = envStr("ZINIAO_PROXY_TEST_TARGET", c.ProxyTestTarget)
	c.ProxyTestCountryTarget = envStr("ZINIAO_PROXY_TEST_COUNTRY", c.ProxyTestCountryTarget)
	return c
}

// envOnOff 解析 on/off 型开关（兼容 1/0、true/false、yes/no、enabled/disabled）。
// 不用 envBool：strconv.ParseBool 不认 "on"/"off"，运维写了 off 会被当成解析失败
// 悄悄回落默认 on——出网开关绝不能这样静默失效。
func envOnOff(k string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "on", "1", "true", "yes", "enabled":
		return true
	case "off", "0", "false", "no", "disabled":
		return false
	}
	return def
}

// NotifyChannels 当前配置下**真正可用**的外发通道（顺序固定，便于断言与展示）。
//
// 返回值用于决定通知行要不要带某个通道标记；未配置时返回空切片，
// notify 包据此退回 console 驱动（dev 行为与改动前完全一致）。
func (c Config) NotifyChannels() []string {
	out := []string{}
	if n := c.Notify; n.SMTPHost != "" {
		out = append(out, "email")
	}
	if n := c.Notify; n.WebhookURL != "" {
		out = append(out, "webhook")
	}
	// 短信要 AppKey 与模板 CODE 都在才算装配：只有其中一个时 BuildSenders 会直接
	// 报错（半配置必须在启动阶段暴露），这里因此不把它算作可用通道。
	if n := c.Notify; n.SMSAppKey != "" && n.SMSTemplateCode != "" {
		out = append(out, "sms")
	}
	return out
}

// NotifyDeliverable 该风险等级的事件是否值得打扰人。
//
// 默认只放行 high：medium 事件（如 `usage.recorded`）量大且不需要立刻处理，
// 全部推到邮箱/IM 只会让人把通知当作噪音静音——那等同于连 high 告警一起失去。
func (c Config) NotifyDeliverable(risk string) bool {
	switch strings.ToLower(strings.TrimSpace(c.Notify.MinRisk)) {
	case "medium":
		return risk == "medium" || risk == "high"
	case "critical":
		return false // 当前事件模型里没有比 high 更高的等级，等于只留 inapp
	default:
		return risk == "high"
	}
}

// AttemptsLimit 单通道投递次数上限的取值口径（含首次）。
//
// 与 PurchaseOwnerThresholdCNY 同理：不要让业务代码各自兜底 3，
// 否则"改了配置只有一半生效"。
func (n Notify) AttemptsLimit() int {
	if n.MaxAttempts > 0 {
		return n.MaxAttempts
	}
	return 3
}

// defaultPurchaseOwnerThresholdCNY 设备申购双人复核阈值的内置兜底值（元）。
const defaultPurchaseOwnerThresholdCNY = 50000

// PurchaseOwnerThresholdCNY 设备申购达到多大金额就要求两位 owner 会签（元）。
//
// 这是**唯一**的取值口径。此前 approval 的路由规则与网关的申购闸门各自读一遍
// 配置、各自兜底 50000，属于典型的「两边口径不一致」——改了配置只有一半生效，
// 而单测两边都过（又是一个 mem 与 pg 那种只在生产中相遇的缺陷）。
func (l Limits) PurchaseOwnerThresholdCNY() float64 {
	if l.DevicePurchaseOwnerThresholdCNY > 0 {
		return l.DevicePurchaseOwnerThresholdCNY
	}
	return defaultPurchaseOwnerThresholdCNY
}

// PlanCatalog 套餐矩阵（M20 运营后台可配置，此处为内置默认值）。
func PlanCatalog(l Limits) map[string]PlanSpec {
	return map[string]PlanSpec{
		"free": {
			Code: "free", Name: "免费版",
			MaxShops: 3, MaxDevices: 1, MaxMembers: 3,
			RecordingRetentionDays: l.RecordingRetentionDaysFree,
			APICallsPerDay:         l.APIDailyQuotaFree,
		},
		"pro": {
			Code: "pro", Name: "专业版",
			MaxShops: 100, MaxDevices: 200, MaxMembers: 50,
			RecordingRetentionDays: 30, APICallsPerDay: 100000,
			HasAgent: true, HasForensicExport: true,
			PricePerDevicePerMonth: 6800, // 68 元/设备/月（规格锚定值）
		},
		"enterprise": {
			Code: "enterprise", Name: "企业版",
			MaxShops: -1, MaxDevices: -1, MaxMembers: -1, // -1 = 不限
			RecordingRetentionDays: 180, APICallsPerDay: -1,
			HasAgent: true, HasForensicExport: true, HasCustomPlugin: true,
			PricePerDevicePerMonth: 6800,
		},
	}
}

// PlanSpec 套餐规格。
type PlanSpec struct {
	Code                   string
	Name                   string
	MaxShops               int
	MaxDevices             int
	MaxMembers             int
	RecordingRetentionDays int
	APICallsPerDay         int
	HasAgent               bool
	HasForensicExport      bool
	HasCustomPlugin        bool
	PricePerDevicePerMonth int
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// ParseCSVEnv 解析逗号分隔的环境变量（如永久留存类别）。
func ParseCSVEnv(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}


// Validate 检查进程启动前必须满足的安全配置。
// 开发环境保留便捷默认值；生产环境对危险默认值采取 fail-closed。
func (c Config) Validate() error {
	if strings.EqualFold(strings.TrimSpace(c.Env), "prod") {
		if c.JWTSecret == "" || c.JWTSecret == "dev-only-secret-change-me" || len([]byte(c.JWTSecret)) < 32 {
			return fmt.Errorf("config: 生产环境必须设置至少 32 字节的独立 ZINIAO_JWT_SECRET")
		}
		if c.StoreDriver != "postgres" {
			return fmt.Errorf("config: 生产环境必须使用 postgres 存储")
		}
		if c.DemoSite {
			return fmt.Errorf("config: 生产环境禁止启用 demo_site")
		}
		if c.KEKConfigError != "" {
			return fmt.Errorf("config: KEK 配置非法: %s", c.KEKConfigError)
		}
		if len(c.KEKKeys) == 0 || c.KEKCurrentVersion <= 0 || len(c.KEKKeys[c.KEKCurrentVersion]) != 32 {
			return fmt.Errorf("config: 生产环境必须通过 ZINIAO_KEK_KEYS 配置持久化的 32 字节 KEK 密钥环，并设置有效的 ZINIAO_KEK_CURRENT_VERSION")
		}
	}
	return nil
}

// parseKEKKeyring 解析 "版本:base64密钥,版本:base64密钥" 格式。
// 接受标准或 Raw Base64；每个版本必须唯一且密钥必须恰好 32 字节。
func parseKEKKeyring(raw string, current int) (map[int][]byte, int, string) {
	if strings.TrimSpace(raw) == "" {
		return nil, current, ""
	}
	keys := make(map[int][]byte)
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 2)
		if len(parts) != 2 {
			return nil, current, "每项必须采用 version:base64 格式"
		}
		version, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || version <= 0 {
			return nil, current, "密钥版本必须是正整数"
		}
		if _, exists := keys[version]; exists {
			return nil, current, "密钥版本重复"
		}
		encoded := strings.TrimSpace(parts[1])
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			key, err = base64.RawStdEncoding.DecodeString(encoded)
		}
		if err != nil || len(key) != 32 {
			return nil, current, fmt.Sprintf("密钥版本 %d 必须是有效的 32 字节 Base64 密钥", version)
		}
		keys[version] = key
	}
	if _, ok := keys[current]; !ok {
		return nil, current, "当前 KEK 版本不在密钥环中"
	}
	return keys, current, ""
}
