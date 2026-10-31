// Package config 配置中心客户端：所有硬编码数值（人数上限、留存天数、额度、
// 超时、次数限制）必须在此登记，禁止散落在业务代码里（主提示词代码规范 / L10）。
//
// 加载优先级：默认值 < 配置文件 < 环境变量（ZINIAO_*）。
// TODO(P1)：接入真正的配置中心（Nacos/Apollo）+ 租户级覆写（tenant_settings）。
package config

import (
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

	// 身份与登录保护（M12 / M14）
	PasswordMinLen        int           `key:"auth.password_min_len"`
	MaxFailedLogins       int           `key:"auth.max_failed_logins"`
	LockoutDuration       time.Duration `key:"auth.lockout_duration"`
	AccessTokenTTL        time.Duration `key:"auth.access_token_ttl"`
	SessionIdleTimeout    time.Duration `key:"auth.session_idle_timeout"`
	LoginAllowedStartHour int           `key:"auth.login_allowed_start_hour"` // 0-24，0&24=不限
	LoginAllowedEndHour   int           `key:"auth.login_allowed_end_hour"`

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

	// 网关
	APIRatePerMinute  int `key:"api.rate_per_minute"`
	APIDailyQuotaFree int `key:"api.daily_quota_free"`

	// 监控回放（M16）
	RecordingRetentionDaysFree int `key:"replay.retention_days_free"` // 7
	RecordingSegmentSeconds    int `key:"replay.segment_seconds"`     // 30
}

// Config 进程级配置。
type Config struct {
	Env         string `key:"env"` // dev|test|prod
	HTTPAddr    string `key:"http.addr"`
	StoreDriver string `key:"store.driver"` // memory|postgres
	PostgresDSN string `key:"store.postgres_dsn"`
	JWTSecret   string `key:"auth.jwt_secret"`
	// Demo 站点：账号隔离实测用（/demo/*）。生产必须为 false。
	DemoSite bool `key:"demo_site"`
	// 事件总线
	DispatcherBatchSize int           `key:"bus.batch_size"`
	DispatcherInterval  time.Duration `key:"bus.interval"`
	DispatcherMaxRetry  int           `key:"bus.max_retry"`
	JobInterval         time.Duration `key:"job.interval"` // 定时巡检间隔
	Limits              Limits
}

// Default 返回带合理默认值的配置（未标注规格数值的部分按 TODO 处理）。
func Default() *Config {
	return &Config{
		Env:                 "dev",
		HTTPAddr:            ":8080",
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
			PasswordMinLen:                10,
			MaxFailedLogins:               5,
			LockoutDuration:               15 * time.Minute,
			AccessTokenTTL:                8 * time.Hour,
			SessionIdleTimeout:            30 * time.Minute,
			LoginAllowedStartHour:         0,
			LoginAllowedEndHour:           24,
			AgentConfirmTTLSeconds:        1800,
			AgentAlwaysConfirmOps:         []string{"withdraw", "price_change", "bulk_delete", "account_change"},
			TempGrantMaxDays:              30,
			ApprovalTTLSeconds:            1800,
			ApprovalRequiredActions: []string{
				"audit.export.plain", "shop.credential.reveal", "shop.bulk_delete",
			},
			ShopTrashRetentionDays:     30,
			DeviceExpireWarnDays:       7,
			DeviceHealthMinScore:       60,
			APIRatePerMinute:           600,
			APIDailyQuotaFree:          1000,
			RecordingRetentionDaysFree: 7,
			RecordingSegmentSeconds:    30,
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
	c.DemoSite = envBool("ZINIAO_DEMO_SITE", c.DemoSite)
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
	return c
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
