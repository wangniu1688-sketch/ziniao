package app

import (
	"context"
	"time"

	"ziniao/eventbus"
	"ziniao/jobs"
	"ziniao/platform/store"
	"ziniao/security"
)

// ---------- 定时任务适配器 ----------
//
// 目的：让 jobs 包只依赖窄接口（不反向依赖 device/shop/approval 的具体类型），
// 便于单测用假实现，也避免定时任务与业务模块互相纠缠。

type tenantLister struct{ st store.Store }

func (l tenantLister) List(ctx context.Context, limit, offset int) ([]jobs.TenantRef, int, error) {
	list, total, err := l.st.Tenants().List(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]jobs.TenantRef, 0, len(list))
	for _, t := range list {
		out = append(out, jobs.TenantRef{ID: t.ID, Name: t.Name})
	}
	return out, total, nil
}

type deviceBriefs struct{ a *App }

func (d deviceBriefs) Brief(ctx context.Context, tenantID string, limit int) ([]jobs.DeviceBrief, error) {
	list, err := d.a.Device.Brief(ctx, tenantID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]jobs.DeviceBrief, 0, len(list))
	for _, it := range list {
		out = append(out, jobs.DeviceBrief{ID: it.ID, Name: it.Name, ExpiresAt: it.ExpiresAt, Health: it.Health})
	}
	return out, nil
}

type alertRaiser struct{ a *App }

// Raise 把定时任务告警接入安全中心（security_events + 事件总线告警）。
func (r alertRaiser) Raise(ctx context.Context, spec jobs.RaiseSpec) error {
	if r.a.Security == nil {
		return nil
	}
	return r.a.Security.Raise(ctx, security.RaiseInput{
		TenantID: spec.TenantID, Category: spec.Category, Level: spec.Level,
		Action: spec.Action, DeviceID: spec.DeviceID, ShopID: spec.ShopID,
		Detail:   spec.Detail,
		Terminal: eventbus.Terminal{Mode: spec.Terminal.Mode, ClientVer: spec.Terminal.ClientVer},
	})
}

type trashPurger struct{ a *App }

func (t trashPurger) PurgeExpired(ctx context.Context, tenantID string, retentionDays int) (int, error) {
	return t.a.Shop.PurgeExpired(ctx, tenantID, retentionDays, eventbus.Terminal{Mode: "job", ClientVer: "jobs/1"})
}

type approvalSweeper struct{ a *App }

func (s approvalSweeper) ExpireSweep(ctx context.Context, tenantID string, now time.Time) (int, error) {
	if s.a.Approvals == nil {
		return 0, nil
	}
	return s.a.Approvals.ExpireSweep(ctx, tenantID, now)
}

type grantReaper struct{ a *App }

func (g grantReaper) RevokeExpired(ctx context.Context, tenantID string, now time.Time) (int, error) {
	if g.a.Tenant == nil {
		return 0, nil
	}
	return g.a.Tenant.RevokeExpiredGrants(ctx, tenantID, now)
}

type delegationSweeper struct{ a *App }

func (d delegationSweeper) ExpireSweep(ctx context.Context, tenantID string, now time.Time) (int, error) {
	if d.a.Delegations == nil {
		return 0, nil
	}
	return d.a.Delegations.ExpireSweep(ctx, tenantID, now)
}

type egressSweeper struct{ a *App }

// SweepTenant 清理该租户到期的出口绑定，并向代理商归还资源。
// 池子未装配时直接返回 0：没配代理商就没有绑定可清。
func (s egressSweeper) SweepTenant(ctx context.Context, tenantID string, now time.Time) (int, error) {
	pool := s.a.Environments.IPPool()
	if pool == nil {
		return 0, nil
	}
	return pool.SweepTenant(ctx, tenantID, now)
}

type deviceRetirer struct{ a *App }

// RetireExpired 回收该租户到期的设备（归还出口 + 标记过期）。
func (d deviceRetirer) RetireExpired(ctx context.Context, tenantID string, now time.Time) (int, error) {
	if d.a.Device == nil {
		return 0, nil
	}
	return d.a.Device.RetireExpired(ctx, tenantID, now)
}

// egressHistoryPruner 出口绑定历史裁剪（仓储层直接就能做，不需要业务服务）。
type egressHistoryPruner struct{ a *App }

func (p egressHistoryPruner) PruneEvents(ctx context.Context, tenantID string, before time.Time) (int64, error) {
	return p.a.Store.EgressBindings().PruneEvents(store.WithTenant(ctx, tenantID), tenantID, before)
}

type notifyRetryer struct{ a *App }

// RetryTenant 补发该租户尚未送达的通知。
// 没装配 Sink（极简测试脚手架）时什么都不做——重投只是尽力而为，
// 不能因为缺的对象让整个 scheduler 报 error。
func (n notifyRetryer) RetryTenant(ctx context.Context, tenantID string) (int, int, error) {
	if n.a.NotifySink == nil {
		return 0, 0, nil
	}
	return n.a.NotifySink.RetryTenant(ctx, tenantID)
}

// wireJobs 注册定时任务（设备巡检 / 回收站清理 / 审批单过期 / 临时授权回收 / 出口绑定回收 / 通知重投 / 环境状态回收）。
func (a *App) wireJobs() {
	cfg := a.Config
	sched := jobs.NewScheduler(tenantLister{st: a.Store}, a.Log)
	sched.Register(
		jobs.NewDeviceInspector(deviceBriefs{a: a}, alertRaiser{a: a},
			cfg.Limits.DeviceExpireWarnDays, cfg.Limits.DeviceHealthMinScore),
		jobs.NewShopPurger(trashPurger{a: a}, cfg.Limits.ShopTrashRetentionDays),
		jobs.NewApprovalJanitor(approvalSweeper{a: a}),
		jobs.NewDelegationJanitor(delegationSweeper{a: a}),
		jobs.NewTempGrantJanitor(grantReaper{a: a}),
		jobs.NewEgressJanitor(egressSweeper{a: a}),
		jobs.NewDeviceRetireJanitor(deviceRetirer{a: a}),
		jobs.NewEgressHistoryPruneJob(egressHistoryPruner{a: a}, cfg.Limits.EgressHistoryRetentionDays),
		jobs.NewNotifyJanitor(notifyRetryer{a: a}),
		jobs.NewEnvJanitor(envReclaimer{a: a}, cfg.Limits.EnvReclaimHours),
	)
	a.Jobs = sched
}

// envReclaimer 环境状态回收（客户端崩溃/断电时没有关闭上报的兜底）。
type envReclaimer struct{ a *App }

func (e envReclaimer) ReclaimIdle(ctx context.Context, tenantID string, now time.Time, maxAge time.Duration) (int, error) {
	if e.a.Environments == nil {
		return 0, nil
	}
	return e.a.Environments.ReclaimIdle(ctx, tenantID, now, maxAge)
}
