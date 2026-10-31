package jobs_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"ziniao/jobs"
)

// ---------- 假实现 ----------

type fakeTenants struct{ list []jobs.TenantRef }

func (f fakeTenants) List(context.Context, int, int) ([]jobs.TenantRef, int, error) {
	return f.list, len(f.list), nil
}

type fakeDevices struct {
	items []jobs.DeviceBrief
	calls int
}

func (f *fakeDevices) Brief(context.Context, string, int) ([]jobs.DeviceBrief, error) {
	f.calls++
	return f.items, nil
}

type fakeAlerts struct{ raised []jobs.RaiseSpec }

func (f *fakeAlerts) Raise(_ context.Context, spec jobs.RaiseSpec) error {
	f.raised = append(f.raised, spec)
	return nil
}

type fakePurger struct {
	days   int
	cutoff int
	n      int
}

func (f *fakePurger) PurgeExpired(_ context.Context, _ string, days int) (int, error) {
	f.days = days
	return f.n, nil
}

type fakeSweeper struct{ n int }

func (f *fakeSweeper) ExpireSweep(_ context.Context, _ string, _ time.Time) (int, error) {
	return f.n, nil
}

// ---------- 任务行为 ----------

// TestDeviceInspectorWarns 临期与健康分恶化都要告警（阈值来自配置，不硬编码）。
func TestDeviceInspectorWarns(t *testing.T) {
	devices := &fakeDevices{items: []jobs.DeviceBrief{
		{ID: "d1", Name: "正常", ExpiresAt: time.Now().Add(30 * 24 * time.Hour), Health: 95},
		{ID: "d2", Name: "临期", ExpiresAt: time.Now().Add(3 * 24 * time.Hour), Health: 90},
		{ID: "d3", Name: "不健康", ExpiresAt: time.Now().Add(30 * 24 * time.Hour), Health: 40},
	}}
	alerts := &fakeAlerts{}
	job := jobs.NewDeviceInspector(devices, alerts, 7, 60)
	res := job.Run(context.Background(), jobs.Context{Tenants: []jobs.TenantRef{{ID: "t1"}}, Now: time.Now()})
	if len(res) != 1 {
		t.Fatalf("应返回 1 个租户的结果: %+v", res)
	}
	if res[0].Scanned != 3 {
		t.Fatalf("扫描数应为 3，实际 %d", res[0].Scanned)
	}
	if res[0].Affected != 2 {
		t.Fatalf("应命中 2 项（临期 + 不健康），实际 %d", res[0].Affected)
	}
	if len(alerts.raised) != 2 {
		t.Fatalf("应产生 2 条告警，实际 %d", len(alerts.raised))
	}
	if alerts.raised[0].Action != "device.expiring" || alerts.raised[0].Level != "high" {
		t.Fatalf("临期告警内容异常: %+v", alerts.raised[0])
	}
	if alerts.raised[1].Action != "device.unhealthy" {
		t.Fatalf("健康告警内容异常: %+v", alerts.raised[1])
	}
	// 自动化必须留下来源标记（L5：自动化也是可追溯的操作主体）
	if alerts.raised[0].Terminal.Mode != "job" {
		t.Fatalf("告警应标记来源为定时任务: %+v", alerts.raised[0].Terminal)
	}
}

// TestThrottle 调度器按 Interval 节流：连续 RunDue 不应重复执行。
func TestThrottle(t *testing.T) {
	devices := &fakeDevices{}
	sched := jobs.NewScheduler(fakeTenants{list: []jobs.TenantRef{{ID: "t1"}}}, slog.Default())
	sched.Register(jobs.NewDeviceInspector(devices, nil, 7, 60))

	if got := sched.RunDue(context.Background()); len(got) != 1 {
		t.Fatalf("首次应执行，实际 %d", len(got))
	}
	sched.RunDue(context.Background())
	if devices.calls != 1 {
		t.Fatalf("未到期不应重复执行，实际调用 %d 次", devices.calls)
	}
	// RunOnce 忽略节流（手动触发必须真的跑）
	sched.RunOnce(context.Background())
	if devices.calls != 2 {
		t.Fatalf("RunOnce 应强制执行，实际调用 %d 次", devices.calls)
	}
}

// TestAllJobsRun 三个任务都能在调度器里被跑到，且结果带任务名与租户。
func TestAllJobsRun(t *testing.T) {
	sched := jobs.NewScheduler(fakeTenants{list: []jobs.TenantRef{{ID: "t1"}, {ID: "t2"}}}, slog.Default())
	purger := &fakePurger{n: 2}
	sweeper := &fakeSweeper{n: 1}
	sched.Register(
		jobs.NewDeviceInspector(&fakeDevices{}, nil, 7, 60),
		jobs.NewShopPurger(purger, 30),
		jobs.NewApprovalJanitor(sweeper),
	)
	names := sched.Names()
	if len(names) != 3 {
		t.Fatalf("应注册 3 个任务，实际 %v", names)
	}
	results := sched.RunOnce(context.Background())
	if len(results) != 6 { // 3 任务 × 2 租户
		t.Fatalf("应产生 6 条结果，实际 %d", len(results))
	}
	if purger.days != 30 {
		t.Fatalf("留存期应透传，实际 %d", purger.days)
	}
	for _, r := range results {
		if r.Job == "" || r.TenantID == "" {
			t.Fatalf("结果缺少任务名/租户: %+v", r)
		}
	}
}

// TestIntervals 任务周期符合预期（巡检 6h / 清理 24h / 审批过期 10m）。
func TestIntervals(t *testing.T) {
	cases := []struct {
		job  jobs.Job
		want time.Duration
	}{
		{jobs.NewDeviceInspector(&fakeDevices{}, nil, 7, 60), 6 * time.Hour},
		{jobs.NewShopPurger(&fakePurger{}, 30), 24 * time.Hour},
		{jobs.NewApprovalJanitor(&fakeSweeper{}), 10 * time.Minute},
	}
	for _, tc := range cases {
		if got := tc.job.Interval(); got != tc.want {
			t.Fatalf("%s 周期应为 %s，实际 %s", tc.job.Name(), tc.want, got)
		}
	}
}
