package pg

import (
	"context"
	"encoding/json"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ---------- 审批工单 ----------

type approvalRepo struct{ base }

func (r approvalRepo) Create(ctx context.Context, t *types.ApprovalTicket) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	detail, err := json.Marshal(t.Detail)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO approval_tickets(id,tenant_id,type,subject_type,subject_id,
			requester_id,requester_name,approver_id,approver_name,reason,status,detail,expires_at,
			created_at,decided_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			t.ID, tid, t.Type, nullStr(t.SubjectType), nullStr(t.SubjectID),
			t.RequesterID, nullStr(t.RequesterName), nullStr(t.ApproverID), nullStr(t.ApproverName),
			nullStr(t.Reason), string(t.Status), detail, t.ExpiresAt, t.CreatedAt, t.DecidedAt)
		return err
	}))
}

func (r approvalRepo) Get(ctx context.Context, id string) (*types.ApprovalTicket, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.ApprovalTicket{}
	var detail []byte
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,type,COALESCE(subject_type,''),COALESCE(subject_id,''),
			requester_id,COALESCE(requester_name,''),COALESCE(approver_id,''),COALESCE(approver_name,''),
			COALESCE(reason,''),status,detail,expires_at,created_at,decided_at
			FROM approval_tickets WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&out.ID, &out.Type, &out.SubjectType, &out.SubjectID, &out.RequesterID,
				&out.RequesterName, &out.ApproverID, &out.ApproverName, &out.Reason,
				&out.Status, &detail, &out.ExpiresAt, &out.CreatedAt, &out.DecidedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	out.TenantID = tid
	_ = json.Unmarshal(detail, &out.Detail)
	return out, nil
}

func (r approvalRepo) Update(ctx context.Context, t *types.ApprovalTicket) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	detail, err := json.Marshal(t.Detail)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE approval_tickets SET type=$3,subject_type=$4,subject_id=$5,
			approver_id=$6,approver_name=$7,reason=$8,status=$9,detail=$10,expires_at=$11,decided_at=$12
			WHERE id=$1 AND tenant_id=$2`,
			t.ID, tid, t.Type, nullStr(t.SubjectType), nullStr(t.SubjectID),
			nullStr(t.ApproverID), nullStr(t.ApproverName), nullStr(t.Reason),
			string(t.Status), detail, t.ExpiresAt, t.DecidedAt)
		return err
	}))
}

func (r approvalRepo) List(ctx context.Context, f store.ApprovalFilter) ([]types.ApprovalTicket, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	where := `WHERE tenant_id=$1`
	args := []any{tid}
	n := 2
	if f.Status != "" {
		where += ` AND status=$` + itoa(n)
		args, n = append(args, string(f.Status)), n+1
	}
	if f.Type != "" {
		where += ` AND type=$` + itoa(n)
		args, n = append(args, f.Type), n+1
	}
	if f.RequesterID != "" {
		where += ` AND requester_id=$` + itoa(n)
		args, n = append(args, f.RequesterID), n+1
	}
	limit, offset := f.Limit, f.Offset
	if limit <= 0 {
		limit = 50
	}
	out := []types.ApprovalTicket{}
	total := 0
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM approval_tickets `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT id,type,COALESCE(subject_type,''),COALESCE(subject_id,''),
			requester_id,COALESCE(requester_name,''),COALESCE(approver_id,''),COALESCE(approver_name,''),
			COALESCE(reason,''),status,detail,expires_at,created_at,decided_at
			FROM approval_tickets `+where+` ORDER BY created_at DESC
			LIMIT `+itoa(n)+` OFFSET `+itoa(n+1), append(args, limit, offset)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t types.ApprovalTicket
			var detail []byte
			if err := rows.Scan(&t.ID, &t.Type, &t.SubjectType, &t.SubjectID, &t.RequesterID,
				&t.RequesterName, &t.ApproverID, &t.ApproverName, &t.Reason, &t.Status,
				&detail, &t.ExpiresAt, &t.CreatedAt, &t.DecidedAt); err != nil {
				return err
			}
			t.TenantID = tid
			_ = json.Unmarshal(detail, &t.Detail)
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

func (r approvalRepo) ExpireBefore(ctx context.Context, at time.Time) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	affected := 0
	err = r.run(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `UPDATE approval_tickets SET status=$3 WHERE tenant_id=$1
			AND status=$2 AND expires_at<=$4`, tid, string(types.ApprovalPending),
			string(types.ApprovalExpired), at)
		if err != nil {
			return err
		}
		affected = int(tag.RowsAffected())
		return nil
	})
	return affected, wrapErr(err)
}

var _ store.ApprovalRepo = approvalRepo{}
