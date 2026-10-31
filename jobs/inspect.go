package jobs

import (
	"context"
	"time"
)

// DeviceLister 设备巡检依赖（由 device.Service 适配，避免 jobs 反向依赖具体模块）。
type DeviceLister interface {
	// Brief 返回待巡检设备的摘要（ID / 到期时间 / 健康分）。
	Brief(ctx context.Context, tenantID string, limit int) ([]DeviceBrief, error)
}

// DeviceBrief 设备摘要（巡检只需这三项，避免搬运整行数据）。
type DeviceBrief struct {
	ID        string
	Name      string
	ExpiresAt time.Time
	Health    int
}

// RaiseSpec 告警规格（定时任务以系统身份告警）。
type RaiseSpec struct {
	TenantID string
	Category string // risk|alert
	Level    string // high|medium|low
	Action   string
	DeviceID string
	ShopID   string
	Detail   map[string]any
	Terminal TerminalInfo
}

// TerminalInfo 来源标记（审计要能看出这条告警是定时任务产生的）。
type TerminalInfo struct {
	Mode      string
	ClientVer string
}

// AlertRaiser 告警出口（由 security.Service 适配）。
type AlertRaiser interface {
	Raise(ctx context.Context, spec RaiseSpec) error
}

// DeviceInspector 设备健康巡检：临期预警 + 健康分恶化告警（M3.2）。
type DeviceInspector struct {
	devices  DeviceLister
	alerts   AlertRaiser
	warnDays int
	minScore int
}

// NewDeviceInspector 构造设备巡检任务（阈值来自配置中心，L10）。
func NewDeviceInspector(devices DeviceLister, alerts AlertRaiser, warnDays, minScore int) *DeviceInspector {
	if warnDays <= 0 {
		warnDays = 7
	}
	if minScore <= 0 {
		minScore = 60
	}
	return &DeviceInspector{devices: devices, alerts: alerts, warnDays: warnDays, minScore: minScore}
}

func (j *DeviceInspector) Name() string            { return "device.inspect" }
func (j *DeviceInspector) Interval() time.Duration { return 6 * time.Hour }

func (j *DeviceInspector) Run(ctx context.Context, jc Context) []Result {
	out := []Result{}
	term := TerminalInfo{Mode: "job", ClientVer: "jobs/1"}
	for _, t := range jc.Tenants {
		list, err := j.devices.Brief(ctx, t.ID, 500)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		res := Result{Job: j.Name(), TenantID: t.ID, Scanned: len(list)}
		for _, d := range list {
			days := int(time.Until(d.ExpiresAt).Hours() / 24)
			if days <= j.warnDays {
				res.Affected++
				if j.alerts != nil {
					_ = j.alerts.Raise(ctx, RaiseSpec{
						TenantID: t.ID, Category: "risk", Level: "high",
						Action: "device.expiring", DeviceID: d.ID, Terminal: term,
						Detail: map[string]any{"device": d.Name, "days_left": days},
					})
				}
			}
			if d.Health < j.minScore {
				res.Affected++
				if j.alerts != nil {
					_ = j.alerts.Raise(ctx, RaiseSpec{
						TenantID: t.ID, Category: "risk", Level: "medium",
						Action: "device.unhealthy", DeviceID: d.ID, Terminal: term,
						Detail: map[string]any{"device": d.Name, "health": d.Health, "threshold": j.minScore},
					})
				}
			}
		}
		if res.Affected > 0 {
			res.Note = "已产生告警"
		}
		out = append(out, res)
	}
	return out
}
