package shop

import (
	"context"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/config"
	"ziniao/platform/store"
)

// ---------- 批量导入（M2） ----------

// BatchImportRow 一行待导入店铺。
type BatchImportRow struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Site     string `json:"site"`
	Group    string `json:"group,omitempty"`
}

// BatchImportInput 批量导入入参。
type BatchImportInput struct {
	Rows     []BatchImportRow
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
	// SourceTotal CSV 里的数据行总数。解析时过滤掉的行（例如缺名称）不会进入 Rows，
	// 但统计里必须记得它们，否则用户看到「共 3 行、失败 0 行」会以为另外两行被导入了。
	SourceTotal int
}

// BatchFailure 单行失败原因（不中断整批，反例 11：配额超限要给出可读原因）。
type BatchFailure struct {
	Row    int    `json:"row"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

// BatchImportResult 批量导入结果。
type BatchImportResult struct {
	Total    int            `json:"total"`
	Created  int            `json:"created"`
	Failed   int            `json:"failed"`
	Failures []BatchFailure `json:"failures"`
}

// BatchImport 批量导入：逐行走权限 + 配额 + 审计，失败行不影响其它行。
func (s *Service) BatchImport(ctx context.Context, in BatchImportInput) (*BatchImportResult, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "shop:create", Path: in.URL})
	if !dec.Allow {
		return nil, denyErr(dec)
	}
	if len(in.Rows) == 0 {
		return nil, apierr.Validation("导入内容为空")
	}
	total := in.SourceTotal
	if total <= 0 {
		total = len(in.Rows)
	}
	res := &BatchImportResult{Total: total, Failures: []BatchFailure{}}
	for i, row := range in.Rows {
		sp, err := s.Create(ctx, CreateInput{
			Name: row.Name, Platform: row.Platform, Site: row.Site,
			Actor: in.Actor, Terminal: in.Terminal, URL: in.URL,
		})
		if err != nil {
			res.Failed++
			res.Failures = append(res.Failures, BatchFailure{Row: i + 1, Name: row.Name, Reason: errMsg(err)})
			continue
		}
		res.Created++
		if row.Group != "" {
			if _, err := s.SetGroup(ctx, SetGroupInput{
				ShopID: sp.ID, Group: row.Group,
				Actor: in.Actor, Terminal: in.Terminal, URL: in.URL,
			}); err != nil {
				res.Failures = append(res.Failures, BatchFailure{Row: i + 1, Name: row.Name, Reason: "分组设置失败：" + errMsg(err)})
			}
		}
	}
	// 汇总事件（逐行事件已在 Create 内产生，这里只补一条批量维度记录）
	now := time.Now().UTC()
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	_ = s.st.WithTx(tctx, func(tx store.Tx) error {
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "shop.batch_import", OccurredAt: now,
			Actor: eventbus.Actor{
				UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor),
			},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.batch_import", Result: "success"},
			Data: map[string]any{
				"total": res.Total, "created": res.Created, "failed": res.Failed,
			},
		})
	})
	return res, nil
}

// ---------- 批量删除（M2，需 shop.bulk_delete 审批单） ----------

// BatchDeleteInput 批量删除入参。
type BatchDeleteInput struct {
	IDs      []string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
	// ApprovalID 本次删除所依据的审批单（网关校验通过后传入，留痕因果）。
	ApprovalID string
}

// BatchDeleteResult 批量删除结果（部分成功语义同导入：单行失败不牵连其它行）。
type BatchDeleteResult struct {
	Total    int            `json:"total"`
	Deleted  int            `json:"deleted"`
	Failed   int            `json:"failed"`
	Failures []BatchFailure `json:"failures"`
}

// BatchDelete 批量删除：影响不可逆，故必须持有一张已通过的 shop.bulk_delete 审批单。
//
// 与逐条删除的区别在于「规模」：一次勾删几十个店铺时，人工逐个确认已经失去意义，
// 真正能拦住误操作与恶意操作的是事前的一张审批单（反例 8：只把按钮置灰不算控制）。
func (s *Service) BatchDelete(ctx context.Context, in BatchDeleteInput) (*BatchDeleteResult, error) {
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "shop:delete", Path: in.URL})
	if !dec.Allow {
		return nil, denyErr(dec)
	}
	if len(in.IDs) == 0 {
		return nil, apierr.Validation("待删除店铺列表为空")
	}
	res := &BatchDeleteResult{Total: len(in.IDs), Failures: []BatchFailure{}}
	for i, id := range in.IDs {
		if err := s.Delete(ctx, in.Actor, id, in.URL, in.Terminal); err != nil {
			res.Failed++
			res.Failures = append(res.Failures, BatchFailure{Row: i + 1, Reason: errMsg(err)})
			continue
		}
		res.Deleted++
	}
	if res.Deleted == 0 {
		return res, nil
	}
	// 汇总事件：逐条删除已在 Delete 内落审计，这里补一条批量维度记录，
	// 让事后追溯能看出「这是一次批量操作」而不是 N 次互不相关的删除。
	now := time.Now().UTC()
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	_ = s.st.WithTx(tctx, func(tx store.Tx) error {
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "shop.bulk_delete", OccurredAt: now,
			Risk: eventbus.RiskHigh,
			Actor: eventbus.Actor{
				UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor),
			},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.bulk_delete", Result: "success"},
			Data: map[string]any{
				"total": res.Total, "deleted": res.Deleted, "failed": res.Failed,
				"approval_id": in.ApprovalID,
			},
		})
	})
	return res, nil
}

// ---------- 回收站清理（M2.1） ----------

// PurgeInput 彻底删除入参。
type PurgeInput struct {
	ShopID   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Purge 彻底删除（回收站内的店铺）：高风险，落审计。
func (s *Service) Purge(ctx context.Context, in PurgeInput) error {
	dec := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "shop:delete", Path: in.URL})
	if !dec.Allow {
		return denyErr(dec)
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	sp, err := s.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return err
	}
	if sp.DeletedAt == nil {
		return apierr.Conflict("只能彻底删除回收站内的店铺")
	}
	now := time.Now().UTC()
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Shops().Delete(tctx, in.ShopID); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "shop.purged", OccurredAt: now, Risk: eventbus.RiskHigh,
			Actor: eventbus.Actor{
				UserID: in.Actor.UserID, Username: in.Actor.Username, RoleCode: roleCodes(in.Actor),
			},
			Subject:  eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.purge", Result: "success"},
			Data:     map[string]any{"shop_id": in.ShopID, "name": sp.Name},
		})
	})
}

// PurgeExpired 定时清理：软删超过留存期的店铺彻底删除（由 jobs 调用，系统身份）。
// 不走权限引擎——定时任务代表系统执行，但必须留下审计（L5）。
func (s *Service) PurgeExpired(ctx context.Context, tenantID string, retentionDays int, term eventbus.Terminal) (int, error) {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	tctx := store.WithTenant(ctx, tenantID)
	items, _, err := s.st.Shops().List(tctx, store.ShopFilter{
		Deleted: true, IncludeDeleted: true, Limit: 500,
	})
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	n := 0
	for _, sp := range items {
		if sp.DeletedAt == nil || sp.DeletedAt.After(cutoff) {
			continue
		}
		now := time.Now().UTC()
		id := sp.ID
		if err := s.st.WithTx(tctx, func(tx store.Tx) error {
			if err := tx.Shops().Delete(tctx, id); err != nil {
				return err
			}
			return s.bus.Emit(tctx, tx, eventbus.Event{
				TenantID: tenantID, Type: "shop.purged", OccurredAt: now, Risk: eventbus.RiskMedium,
				Actor:    eventbus.Actor{UserID: "system", Username: "system"},
				Subject:  eventbus.Subject{ShopID: id},
				Terminal: term,
				Context:  eventbus.ContextInfo{Action: "shop.purge_expired", Result: "success"},
				Data: map[string]any{
					"shop_id": id, "retention_days": retentionDays,
					"deleted_at": sp.DeletedAt.Format(time.RFC3339),
				},
			})
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// SystemActor 定时任务使用的系统身份（审计可追溯为 system）。
func SystemActor(tenantID string) iam.Subject {
	return iam.Subject{UserID: "system", Username: "system", TenantID: tenantID}
}

// ---------- 辅助 ----------

func errMsg(err error) string {
	if e, ok := err.(*apierr.Error); ok {
		return e.Message
	}
	return err.Error()
}

// RetentionDays 回收站留存期（配置中心单一真相源，L10）。
func RetentionDays(cfg *config.Config) int {
	if cfg == nil || cfg.Limits.ShopTrashRetentionDays <= 0 {
		return 30
	}
	return cfg.Limits.ShopTrashRetentionDays
}
