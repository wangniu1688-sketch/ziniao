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

// DeviceRetirer 依赖：到期设备的出口归还与状态回收。
type DeviceRetirer interface {
	RetireExpired(ctx context.Context, tenantID string, now time.Time) (int, error)
}

// DeviceRetireJanitor 到期设备回收任务。
//
// 与 EgressJanitor 分开注册的理由：两者扫的是不同生命周期——
// 出口绑定按租约到期（小时级），设备按月租到期（天级）。
// 合在一起只会让「这次到底回收了什么」说不清，频率也没法分别调。
type DeviceRetireJanitor struct {
	retirer DeviceRetirer
}

// NewDeviceRetireJanitor 构造到期设备回收任务。
func NewDeviceRetireJanitor(r DeviceRetirer) *DeviceRetireJanitor {
	return &DeviceRetireJanitor{retirer: r}
}

func (j *DeviceRetireJanitor) Name() string            { return "device.retire_expired" }
func (j *DeviceRetireJanitor) Interval() time.Duration { return 6 * time.Hour }

func (j *DeviceRetireJanitor) Run(ctx context.Context, jc Context) []Result {
	out := []Result{}
	for _, t := range jc.Tenants {
		n, err := j.retirer.RetireExpired(ctx, t.ID, jc.Now)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		out = append(out, Result{Job: j.Name(), TenantID: t.ID, Affected: n,
			Note: "到期设备已停用并归还出口"})
	}
	return out
}

// EgressHistoryPruner 依赖：出口绑定历史的保留期裁剪。
type EgressHistoryPruner interface {
	PruneEvents(ctx context.Context, tenantID string, before time.Time) (int64, error)
}

// EgressHistoryPruneJob 出口绑定历史保留期裁剪任务。
//
// 历史只追加不改写，不裁剪就会无限增长；但裁剪是运营决策（保留多久），
// 不该硬编码在业务里——retentionDays <= 0 时本任务什么都不做（历史是资产，
// 共用 ASN 的证据链怕断不怕长，想永远留着就把天数配成 0）。
type EgressHistoryPruneJob struct {
	pruner        EgressHistoryPruner
	retentionDays int
}

// NewEgressHistoryPruneJob 构造裁剪任务。
func NewEgressHistoryPruneJob(p EgressHistoryPruner, retentionDays int) *EgressHistoryPruneJob {
	return &EgressHistoryPruneJob{pruner: p, retentionDays: retentionDays}
}

func (j *EgressHistoryPruneJob) Name() string            { return "network.egress_history_prune" }
func (j *EgressHistoryPruneJob) Interval() time.Duration { return 24 * time.Hour }

func (j *EgressHistoryPruneJob) Run(ctx context.Context, jc Context) []Result {
	if j.retentionDays <= 0 {
		return []Result{{Job: j.Name(), Note: "保留期未启用（<=0），不裁剪"}}
	}
	cutoff := jc.Now.AddDate(0, 0, -j.retentionDays)
	out := []Result{}
	for _, t := range jc.Tenants {
		n, err := j.pruner.PruneEvents(ctx, t.ID, cutoff)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		out = append(out, Result{Job: j.Name(), TenantID: t.ID, Affected: int(n),
			Note: "已裁剪保留期外的出口绑定历史"})
	}
	return out
}
