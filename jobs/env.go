package jobs

import (
	"context"
	"time"
)

// EnvReclaimer 依赖：回收卡在 running 的环境。
type EnvReclaimer interface {
	ReclaimIdle(ctx context.Context, tenantID string, now time.Time, maxAge time.Duration) (int, error)
}

// EnvJanitor 环境状态回收任务。
//
// 为什么需要它：关闭环境的正常路径是客户端关窗口时上报一次（env.stopped）。
// 但客户端会崩溃、机器会断电、网络会断——这些情况下那条上报永远不会来，
// 环境就永远停在 running。运营看到的一屏"正在运行"，其实可能一个窗口都没开。
//
// 判据只有"多久没人动过它"，因此这是启发式兜底：误判只把状态显示成空闲，
// 不删 profile、不放出口绑定，正在开着的窗口不受影响（下次启动会重新置 running）。
type EnvJanitor struct {
	reclaimer EnvReclaimer
	maxAge    time.Duration
}

// NewEnvJanitor 构造环境回收任务（maxAge ≤ 0 表示不启用）。
func NewEnvJanitor(reclaimer EnvReclaimer, hours int) *EnvJanitor {
	maxAge := time.Duration(hours) * time.Hour
	if hours <= 0 {
		maxAge = 0
	}
	return &EnvJanitor{reclaimer: reclaimer, maxAge: maxAge}
}

func (j *EnvJanitor) Name() string            { return "env.reclaim" }
func (j *EnvJanitor) Interval() time.Duration { return 30 * time.Minute }

func (j *EnvJanitor) Run(ctx context.Context, jc Context) []Result {
	if j.reclaimer == nil || j.maxAge <= 0 {
		return []Result{{Job: j.Name(), Note: "未启用（EnvReclaimHours ≤ 0）"}}
	}
	out := []Result{}
	for _, t := range jc.Tenants {
		n, err := j.reclaimer.ReclaimIdle(ctx, t.ID, jc.Now, j.maxAge)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		out = append(out, Result{Job: j.Name(), TenantID: t.ID, Affected: n,
			Note: "长时间无关闭上报的环境已置回空闲"})
	}
	return out
}
