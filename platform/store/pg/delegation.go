package pg

import (
	"context"
	"encoding/json"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ---------- 代理机构委托审批（approval_flows，migration 0007） ----------
//
// 行归属 tenant_id = 委托方，但 RLS 是双主体策略（OR agency_tenant_id）：
// 受托方必须能看到自己提交的申请状态。仓储谓词与 RLS 口径一致：
// Get/Update 按 (id AND (tenant_id=$x OR agency_tenant_id=$x)) 匹配，
// List/HasPendingBetween 显式带双方租户谓词，不依赖 RLS 单独兜底。

type delegationRepo struct{ base }

func scanFlow(scan func(dest ...any) error) (*types.DelegationFlow, error) {
	var f types.DelegationFlow
	var scope, detail []byte
	if err := scan(&f.ID, &f.TenantID, &f.AgencyTenantID, &f.AgencyName,
		&f.GranteeID, &f.GranteeName, &scope, &f.Reason, &f.Comment,
		&f.Status, &detail, &f.ApproverID, &f.ApproverName,
		&f.ExpiresAt, &f.ValidUntil, &f.CreatedAt, &f.DecidedAt, &f.RevokedAt); err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal(scope, &m)
	if ids, ok := m["shop_ids"].([]any); ok {
		for _, v := range ids {
			if s, ok := v.(string); ok {
				f.ShopIDs = append(f.ShopIDs, s)
			}
		}
	}
	_ = json.Unmarshal(detail, &f.Detail)
	return &f, nil
}

const delegationCols = `id,tenant_id,agency_tenant_id,COALESCE(agency_name,''),
	grantee_id,COALESCE(grantee_name,''),scope,COALESCE(reason,''),COALESCE(comment,''),
	status,detail,COALESCE(approver_id,''),COALESCE(approver_name,''),
	expires_at,valid_until,created_at,decided_at,revoked_at`

func (r delegationRepo) Create(ctx context.Context, f *types.DelegationFlow) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	scope, err := json.Marshal(map[string]any{"shop_ids": f.ShopIDs})
	if err != nil {
		return err
	}
	detail, err := json.Marshal(f.Detail)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO approval_flows(id,tenant_id,agency_tenant_id,agency_name,
			grantee_id,grantee_name,scope,reason,comment,status,detail,approver_id,approver_name,
			expires_at,valid_until,created_at,decided_at,revoked_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			f.ID, tid, f.AgencyTenantID, f.AgencyName,
			f.GranteeID, f.GranteeName, scope, nullStr(f.Reason), nullStr(f.Comment),
			string(f.Status), detail, nullStr(f.ApproverID), nullStr(f.ApproverName),
			f.ExpiresAt, f.ValidUntil, f.CreatedAt, f.DecidedAt, f.RevokedAt)
		return err
	}))
}

// Get 双主体可读：id 命中且 tenant_id 或 agency_tenant_id 等于上下文租户。
func (r delegationRepo) Get(ctx context.Context, id string) (*types.DelegationFlow, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	var out *types.DelegationFlow
	err = r.run(ctx, func(q querier) error {
		row := q.QueryRow(ctx, `SELECT `+delegationCols+`
			FROM approval_flows WHERE id=$1 AND (tenant_id=$2 OR agency_tenant_id=$2)`, id, tid)
		f, scanErr := scanFlow(row.Scan)
		out = f
		return scanErr
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r delegationRepo) Update(ctx context.Context, f *types.DelegationFlow) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	scope, err := json.Marshal(map[string]any{"shop_ids": f.ShopIDs})
	if err != nil {
		return err
	}
	detail, err := json.Marshal(f.Detail)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE approval_flows SET agency_name=$3,
			grantee_id=$4,grantee_name=$5,scope=$6,reason=$7,comment=$8,status=$9,detail=$10,
			approver_id=$11,approver_name=$12,expires_at=$13,valid_until=$14,decided_at=$15,revoked_at=$16
			WHERE id=$1 AND (tenant_id=$2 OR agency_tenant_id=$2)`,
			f.ID, tid, f.AgencyName,
			f.GranteeID, f.GranteeName, scope, nullStr(f.Reason), nullStr(f.Comment),
			string(f.Status), detail, nullStr(f.ApproverID), nullStr(f.ApproverName),
			f.ExpiresAt, f.ValidUntil, f.DecidedAt, f.RevokedAt)
		return err
	}))
}

func (r delegationRepo) List(ctx context.Context, f store.DelegationFilter) ([]types.DelegationFlow, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	// Party 决定视角：grantor（默认）看收到的申请，agency 看自己发出的申请。
	where := `WHERE tenant_id=$1`
	if f.Party == "agency" {
		where = `WHERE agency_tenant_id=$1`
	}
	args := []any{tid}
	n := 2
	if f.Status != "" {
		where += ` AND status=$` + itoa(n)
		args, n = append(args, string(f.Status)), n+1
	}
	limit, offset := f.Limit, f.Offset
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	out := []types.DelegationFlow{}
	total := 0
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM approval_flows `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT `+delegationCols+` FROM approval_flows `+where+`
			ORDER BY created_at DESC LIMIT $`+itoa(n)+` OFFSET $`+itoa(n+1), append(args, limit, offset)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			f, scanErr := scanFlow(rows.Scan)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, *f)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

// HasPendingBetween 同一（委托方, 受托方）是否已有待审批单（重复申请闸门）。
// 显式谓词带双方租户 ID，不取上下文——提交方是受托方，行的 tenant_id 是委托方。
func (r delegationRepo) HasPendingBetween(ctx context.Context, grantorTenantID, agencyTenantID string) (bool, error) {
	ok := false
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_flows
			WHERE tenant_id=$1 AND agency_tenant_id=$2 AND status=$3)`,
			grantorTenantID, agencyTenantID, string(types.DelegationPending)).Scan(&ok)
	})
	return ok, wrapErr(err)
}

var _ store.DelegationRepo = delegationRepo{}
