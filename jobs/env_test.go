package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"ziniao/jobs"
)

type fakeEnvReclaimer struct {
	calls int
	n     int
	err   error
	age   time.Duration
}

func (f *fakeEnvReclaimer) ReclaimIdle(_ context.Context, _ string, _ time.Time, maxAge time.Duration) (int, error) {
	f.calls++
	f.age = maxAge
	if f.err != nil {
		return 0, f.err
	}
	return f.n, nil
}

// TestEnvJanitorReclaimsStuckRunning 客户端崩溃时的兜底：卡住的 running 要能被收回。
func TestEnvJanitorReclaimsStuckRunning(t *testing.T) {
	reaper := &fakeEnvReclaimer{n: 2}
	j := jobs.NewEnvJanitor(reaper, 12)
	if j.Name() != "env.reclaim" {
		t.Fatalf("任务名不符: %s", j.Name())
	}
	now := time.Now().UTC()
	out := j.Run(context.Background(), jobs.Context{
		Tenants: []jobs.TenantRef{{ID: "t1"}, {ID: "t2"}}, Now: now,
	})
	if reaper.calls != 2 {
		t.Fatalf("每个租户都应执行一次，实际 %d", reaper.calls)
	}
	if reaper.age != 12*time.Hour {
		t.Fatalf("超期阈值应为 12h，实际 %v", reaper.age)
	}
	affected := 0
	for _, r := range out {
		if r.Err != "" {
			t.Fatalf("不应报错: %s", r.Err)
		}
		affected += r.Affected
	}
	if affected != 4 {
		t.Fatalf("应累计 4 条，实际 %d", affected)
	}
}

// TestEnvJanitorDisabled 阈值 ≤ 0 时不能退化成"把所有环境都回收掉"。
func TestEnvJanitorDisabled(t *testing.T) {
	reaper := &fakeEnvReclaimer{}
	j := jobs.NewEnvJanitor(reaper, 0)
	out := j.Run(context.Background(), jobs.Context{Tenants: []jobs.TenantRef{{ID: "t1"}}})
	if reaper.calls != 0 {
		t.Fatal("未启用时不应调用回收")
	}
	if len(out) != 1 || out[0].Err != "" {
		t.Fatalf("应给出一条未启用的说明而不是报错: %+v", out)
	}
}

// TestEnvJanitorPropagatesError 单租户失败不能吞掉，否则巡检看起来一直很健康。
func TestEnvJanitorPropagatesError(t *testing.T) {
	reaper := &fakeEnvReclaimer{err: errors.New("boom")}
	j := jobs.NewEnvJanitor(reaper, 12)
	out := j.Run(context.Background(), jobs.Context{Tenants: []jobs.TenantRef{{ID: "t1"}}})
	if len(out) != 1 || out[0].Err == "" {
		t.Fatalf("失败应体现在结果里: %+v", out)
	}
}
