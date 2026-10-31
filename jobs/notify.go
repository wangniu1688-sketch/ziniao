package jobs

import (
	"context"
	"strconv"
	"time"
)

// NotifyRetryer 依赖：重投尚未送达的通知。
type NotifyRetryer interface {
	// RetryTenant 返回 (本轮送达条数, 已放弃条数)。
	RetryTenant(ctx context.Context, tenantID string) (int, int, error)
}

// NotifyJanitor 通知投递重投任务。
//
// 为什么单独要一个任务：外发（邮件/Webhook）依赖的是**别人的**服务，
// 而发送失败时站内通知已经入库了——没人管的话它就永远停在 pending，
// 运营看到的「已通知」其实是「写进去了但没人收到」。high 级告警
// （出口 IP 轮换、批量删除、审批通过）恰恰是最不该丢的那几条。
//
// 注意执行节奏：Interval 是自己的节流窗口，实际触发还受 Scheduler tick
// （cfg.JobInterval，默认 10m）限制——即最坏情况多等一个 tick。
type NotifyJanitor struct {
	retryer NotifyRetryer
}

// NewNotifyJanitor 构造通知重投任务。
func NewNotifyJanitor(r NotifyRetryer) *NotifyJanitor { return &NotifyJanitor{retryer: r} }

func (j *NotifyJanitor) Name() string            { return "notify.delivery_retry" }
func (j *NotifyJanitor) Interval() time.Duration { return 5 * time.Minute }

func (j *NotifyJanitor) Run(ctx context.Context, jc Context) []Result {
	if j.retryer == nil {
		return nil
	}
	out := []Result{}
	for _, t := range jc.Tenants {
		sent, failed, err := j.retryer.RetryTenant(ctx, t.ID)
		if err != nil {
			out = append(out, Result{Job: j.Name(), TenantID: t.ID, Err: err.Error()})
			continue
		}
		if sent == 0 && failed == 0 {
			continue
		}
		out = append(out, Result{Job: j.Name(), TenantID: t.ID, Scanned: sent + failed, Affected: sent,
			Note: "未送达通知已重投（仍失败 " + strconv.Itoa(failed) + " 条，超过上限将保留 failed 供排查）"})
	}
	return out
}
