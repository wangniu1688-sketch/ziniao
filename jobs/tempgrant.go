package jobs

import (
	"context"
	"time"
)

// TempGrantReaper 依赖：临时授权到期回收。
type TempGrantReaper interface {
	RevokeExpired(ctx context.Context, tenantID string, now time.Time) (int, error)
}

// TempGrantJanitor 临时授权到期回收任务（M12）。
//
// 这里扫的不是「有没有超过期限」，而是把已经确认到期的授权从库里清掉。
// 到期即刻失效不由本任务负责（读路径判定，见 store.DataGrantRepo.ListByUser
// 的注释）——否则会留下最长一个执行周期的权限裸奔窗口。
type TempGrantJanitor struct {
	reaper TempGrantReaper
}

// NewTempGrantJanitor 构造临时授权回收任务。
func NewTempGrantJanitor(reaper TempGrantReaper) *TempGrantJanitor {
	return &TempGrantJanitor{reaper: reaper}
}

func (j *TempGrantJanitor) Name() string            { return "iam.temp_grant_revoke" }
func (j *TempGrantJanitor) Interval() time.Duration { return 30 * time.Minute }

func (j *TempGrantJanitor) Run(ctx context.Context, jc Context) []Result {
	out := []Result{}
	for _, t := range jc.Tenants {
		n, err := j.reaper.RevokeExpired(ctx, t.ID, jc.Now)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		out = append(out, Result{Job: j.Name(), TenantID: t.ID, Affected: n,
			Note: "到期的临时授权已回收并通知当事人"})
	}
	return out
}
