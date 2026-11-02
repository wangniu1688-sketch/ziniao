package jobs

import (
	"context"
	"time"
)

// ---------- delegation.expire：代理机构委托过期巡检 ----------

// DelegationSweeper 委托过期巡检的窄接口（app 侧适配器实现，避免反向依赖业务包）。
type DelegationSweeper interface {
	ExpireSweep(ctx context.Context, tenantID string, now time.Time) (int, error)
}

// delegationJanitor 委托单过期巡检：
//  1. 申请单超期未决 → expired（通知申请人）；
//  2. 生效中的委托过 valid_until → expired + 授权整批回收（通知双方）。
//
// 到期即刻失效由读路径兜着（DataGrant 带到期时间，过期行即失效），
// 巡检负责清行、改状态与让人知道——与审批单过期 / 临时授权回收同一分工。
type delegationJanitor struct {
	sweeper DelegationSweeper
}

// NewDelegationJanitor 构造委托过期巡检任务。
func NewDelegationJanitor(s DelegationSweeper) Job {
	return &delegationJanitor{sweeper: s}
}

func (j *delegationJanitor) Name() string { return "delegation.expire" }

func (j *delegationJanitor) Interval() time.Duration { return 30 * time.Minute }

func (j *delegationJanitor) Run(ctx context.Context, jc Context) []Result {
	out := []Result{}
	for _, t := range jc.Tenants {
		n, err := j.sweeper.ExpireSweep(ctx, t.ID, jc.Now)
		r := Result{Job: j.Name(), TenantID: t.ID, Scanned: n, Affected: n}
		if err != nil {
			r.Err = err.Error()
		}
		out = append(out, r)
	}
	return out
}
