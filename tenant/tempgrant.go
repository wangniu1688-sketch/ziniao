package tenant

import (
	"context"
	"time"

	"ziniao/eventbus"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// RevokeExpiredGrants 回收到期的临时授权：物理删除授权记录 + 定向通知当事人。
//
// 为什么不能只做「读时过滤」（此前就是这么干的）：
//  1. 到期立即失效由 identity 读路径负责（拿着 expires_at 现场判定），这条不等
//     定时任务；但只做读判定，过期记录会永远堆在库里，"当前有效名单"越来越不可信；
//  2. 更现实的一条：没有通知的话，当事人只会发现「某个操作忽然 403 了」，
//     而不知道是自己的临时授权到期——他会去找管理员骂流程，而不是来找你续期。
//
// 因此本方法做的是写侧的：**清理 + 让人知道**。
// 只有临时授权（ExpiresAt 非空）会被动到，正式授权一行都不会少。
func (s *Service) RevokeExpiredGrants(ctx context.Context, tenantID string, now time.Time) (int, error) {
	if tenantID == "" {
		return 0, nil
	}
	tctx := store.WithTenant(ctx, tenantID)
	n := 0
	err := s.st.WithTx(tctx, func(tx store.Tx) error {
		// 一次语句拿回被删条目：先查后删在并发下会读到已经不存在的行，
		// 也会留下「行清了但通知没发」的窗口——那等于静默收权。
		doomed, err := tx.DataGrants().DeleteExpired(tctx, tenantID, now)
		if err != nil {
			return err
		}
		if len(doomed) == 0 {
			return nil
		}
		// 系统而非某个人的动作：Terminal 标注来源，事后追溯看得出不是人点的。
		term := eventbus.Terminal{Mode: "job", ClientVer: "jobs/1"}
		for _, g := range doomed {
			if g.ResourceType == "role" {
				if err := revokeTempRole(tctx, tx, g); err != nil {
					return err
				}
			}
			data := map[string]any{
				"grant_id":      g.ID,
				"user_id":       g.UserID,
				"resource_type": g.ResourceType,
				"resource_id":   g.ResourceID,
				"granted_by":    g.GrantedBy,
				// 定向推送：只有当事人需要知道自己被收了权，不该打扰全团队。
				"approver_ids": []string{g.UserID},
			}
			if g.ExpiresAt != nil {
				data["expired_at"] = g.ExpiresAt.UTC().Format(time.RFC3339)
			}
			if err := s.bus.Emit(tctx, tx, eventbus.Event{
				TenantID:   tenantID,
				Type:       eventbus.TypeTempGrantExpired,
				OccurredAt: now,
				// 必须是 medium 以上：notify.Sink 第 76 行对 low 直接 return nil，
				// 用 low 的话事件发了、审计也进了，但谁都收不到通知。
				Risk:     eventbus.RiskMedium,
				Actor:    eventbus.Actor{UserID: g.GrantedBy},
				Terminal: term,
				Context:  eventbus.ContextInfo{Action: "iam.temp_grant.revoke", Result: "revoked"},
				Data:     data,
			}); err != nil {
				return err
			}
		}
		n = len(doomed)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// revokeTempRole 连带撤销临时角色对应的 user_roles 行。
//
// 关键在于「只撤跟着这笔临时授权一起写进去的那一行」：user_roles 上有
// (tenant_id,user_id,role_id) 唯一约束，先正式授予过、之后又追加一笔临时授权时，
// 两种授权共用同一行——不加区分地删，临时到期会把人家原有的正式角色一起捎走。
// 判据：该行的 granted_at 不早于这笔授权的 created_at（同一事务写入，时刻相同）。
//
// 已知限制（诚实登记）：靠时间戳推断来源，终究是间接证据。根治要等 user_roles
// 增加 source 列（permanent/temporary），届时改为显式字段判断。
func revokeTempRole(ctx context.Context, tx store.Tx, g types.DataGrant) error {
	urs, err := tx.UserRoles().ListByUser(ctx, g.TenantID, g.UserID)
	if err != nil {
		return err
	}
	for _, ur := range urs {
		if ur.RoleID != g.ResourceID {
			continue
		}
		if ur.GrantedAt.Before(g.CreatedAt.Add(-time.Second)) {
			continue // 早于这笔临时授权 ⇒ 是原先就有的正式授权，不能动
		}
		return tx.UserRoles().Revoke(ctx, g.TenantID, g.UserID, g.ResourceID)
	}
	return nil
}
