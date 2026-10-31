package jobs

import (
	"context"
	"time"
)

// EgressSweeper 依赖：到期出口绑定的清理与厂商资源归还。
type EgressSweeper interface {
	SweepTenant(ctx context.Context, tenantID string, now time.Time) (int, error)
}

// EgressJanitor 出口绑定到期回收任务。
//
// 为什么必须扫库而不只看内存：重启后内存表是空的，只有库知道这台机器还占着
// 代理商哪些资源。不归还的代价是双份的——
//  1. 继续按天计费（钱白花）；
//  2. 占着代理商的名额，让新环境申请不到独占出口，表现为莫名其妙的
//     ErrNoExclusiveResource（排查方向会全跑偏）。
//
// 与 jobs 里其它任务一致，按租户逐家扫：绑定行本身是租户域数据（RLS），
// 系统任务也不该拿一个「全库视角」去绕隔离。
type EgressJanitor struct {
	sweeper EgressSweeper
}

// NewEgressJanitor 构造出口绑定回收任务。
func NewEgressJanitor(sweeper EgressSweeper) *EgressJanitor {
	return &EgressJanitor{sweeper: sweeper}
}

func (j *EgressJanitor) Name() string            { return "network.egress_sweep" }
func (j *EgressJanitor) Interval() time.Duration { return 15 * time.Minute }

func (j *EgressJanitor) Run(ctx context.Context, jc Context) []Result {
	out := []Result{}
	for _, t := range jc.Tenants {
		n, err := j.sweeper.SweepTenant(ctx, t.ID, jc.Now)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		out = append(out, Result{Job: j.Name(), TenantID: t.ID, Affected: n,
			Note: "到期的出口绑定已清理并向代理商归还资源"})
	}
	return out
}
