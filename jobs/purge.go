package jobs

import (
	"context"
	"time"
)

// TrashPurger 依赖：回收站超期店铺清理。
type TrashPurger interface {
	PurgeExpired(ctx context.Context, tenantID string, retentionDays int) (int, error)
}

// ShopPurger 回收站清理任务（M2.1）：软删超过留存期的店铺彻底删除。
type ShopPurger struct {
	purger TrashPurger
	days   int
}

// NewShopPurger 构造清理任务。
func NewShopPurger(purger TrashPurger, retentionDays int) *ShopPurger {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	return &ShopPurger{purger: purger, days: retentionDays}
}

func (j *ShopPurger) Name() string            { return "shop.trash_purge" }
func (j *ShopPurger) Interval() time.Duration { return 24 * time.Hour }

func (j *ShopPurger) Run(ctx context.Context, jc Context) []Result {
	out := []Result{}
	for _, t := range jc.Tenants {
		n, err := j.purger.PurgeExpired(ctx, t.ID, j.days)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		out = append(out, Result{Job: j.Name(), TenantID: t.ID, Affected: n,
			Note: "清理超过留存期的店铺（留存期 " + itoa(j.days) + " 天）"})
	}
	return out
}

// ApprovalSweeper 依赖：审批单过期扫描。
type ApprovalSweeper interface {
	ExpireSweep(ctx context.Context, tenantID string, now time.Time) (int, error)
}

// ApprovalJanitor 审批单过期扫描：超时未决自动置为 expired。
type ApprovalJanitor struct {
	sweeper ApprovalSweeper
}

// NewApprovalJanitor 构造审批单清理任务。
func NewApprovalJanitor(sweeper ApprovalSweeper) *ApprovalJanitor {
	return &ApprovalJanitor{sweeper: sweeper}
}

func (j *ApprovalJanitor) Name() string            { return "approval.expire" }
func (j *ApprovalJanitor) Interval() time.Duration { return 10 * time.Minute }

func (j *ApprovalJanitor) Run(ctx context.Context, jc Context) []Result {
	out := []Result{}
	for _, t := range jc.Tenants {
		n, err := j.sweeper.ExpireSweep(ctx, t.ID, jc.Now)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		out = append(out, Result{Job: j.Name(), TenantID: t.ID, Affected: n, Note: "超时未决审批单已置为过期"})
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
