package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"ziniao/jobs"
)

// delegationSweeperFake 记录每个租户被巡检的次数，供断言。
// （命名避开 jobs_test.go 里既有的 fakeSweeper。）
type delegationSweeperFake struct {
	calls map[string]int
	err   error
}

func (f *delegationSweeperFake) ExpireSweep(_ context.Context, tenantID string, _ time.Time) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.calls[tenantID]++
	return f.calls[tenantID], nil
}

// TestDelegationJanitorRunsPerTenant 委托过期巡检必须按租户逐个执行（与审批单巡检同一分工）。
func TestDelegationJanitorRunsPerTenant(t *testing.T) {
	fake := &delegationSweeperFake{calls: map[string]int{}}
	job := jobs.NewDelegationJanitor(fake)
	if job.Name() != "delegation.expire" {
		t.Fatalf("任务名应为 delegation.expire，实际 %s", job.Name())
	}
	if job.Interval() != 30*time.Minute {
		t.Fatalf("巡检周期应为 30 分钟，实际 %v", job.Interval())
	}
	jc := jobs.Context{
		Tenants: []jobs.TenantRef{{ID: "t1"}, {ID: "t2"}},
		Now:     time.Now().UTC(),
	}
	results := job.Run(context.Background(), jc)
	if len(results) != 2 {
		t.Fatalf("应每个租户一条结果，实际 %d", len(results))
	}
	if fake.calls["t1"] != 1 || fake.calls["t2"] != 1 {
		t.Fatalf("每个租户应恰好巡检一次: %v", fake.calls)
	}

	// 失败要进 Result.Err，不能静默吞掉（否则过期委托永远没人知道没扫成）
	failing := &delegationSweeperFake{calls: map[string]int{}, err: errors.New("db down")}
	res := jobs.NewDelegationJanitor(failing).Run(context.Background(), jc)
	for _, r := range res {
		if r.Err == "" {
			t.Fatal("巡检失败必须体现在 Result.Err")
		}
	}
}
