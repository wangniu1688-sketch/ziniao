package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// egressRepo 出口绑定（PostgreSQL）。
//
// 租户一律取自 ctx（与 envRepo 同口径），RLS 的 WITH CHECK 会再挡一层；
// 只有 DeleteExpired 额外带显式 tenantID，因为它是系统回收任务调用的，
// 需要把「给哪个租户回收」写在调用点而不是隐式依赖上下文。
type egressRepo struct{ base }

const egressCols = `env_id,provider,resource_id,protocol,host,port,
	COALESCE(country,''),COALESCE(region,''),COALESCE(city,''),COALESCE(asn,''),
	COALESCE(asn_type,''),COALESCE(region_code,''),COALESCE(last_exit,''),
	bound_at,expires_at,updated_at`

func scanEgress(tid string, scan func(...any) error) (*types.EgressBinding, error) {
	out := &types.EgressBinding{TenantID: tid}
	if err := scan(&out.EnvID, &out.Provider, &out.ResourceID, &out.Protocol, &out.Host, &out.Port,
		&out.Country, &out.Region, &out.City, &out.ASN, &out.ASNType, &out.RegionCode, &out.LastExit,
		&out.BoundAt, &out.ExpiresAt, &out.UpdatedAt); err != nil {
		return nil, err
	}
	return out, nil
}

// Bind 落库绑定。互斥交给唯一索引（(provider,resource_id) 与 (provider,host,port)
// 两条），不做「先查后写」：滚动发布时新旧进程会同时向代理商申请，先查后写必然
// 双成功——两个环境共用一个出口 IP，等于把两个店铺账号在平台证据链上串起来。
//
// 这里用不带推断目标的 ON CONFLICT DO NOTHING：任何一个唯一索引被撞到都算
// 「资源已被占用」，交给上层换下一个候选，而不是让 23505 冒成 500。
func (r egressRepo) Bind(ctx context.Context, b *types.EgressBinding) (bool, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return false, err
	}
	ok := false
	err = r.run(ctx, func(q querier) error {
		// env_id 是主键：先清掉这个环境的旧绑定，才能保证「一个环境一条绑定」。
		// 旧行对应的厂商资源由调用方负责归还（仓储不对外发请求）。
		if _, err := q.Exec(ctx, `DELETE FROM egress_bindings WHERE env_id=$1 AND tenant_id=$2`, b.EnvID, tid); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `INSERT INTO egress_bindings(env_id,tenant_id,provider,resource_id,protocol,host,port,
			country,region,city,asn,asn_type,region_code,last_exit,bound_at,expires_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			ON CONFLICT DO NOTHING`,
			b.EnvID, tid, b.Provider, b.ResourceID, b.Protocol, b.Host, b.Port,
			nullStr(b.Country), nullStr(b.Region), nullStr(b.City), nullStr(b.ASN),
			nullStr(b.ASNType), nullStr(b.RegionCode), nullStr(b.LastExit),
			b.BoundAt, b.ExpiresAt, b.UpdatedAt)
		if err != nil {
			return err
		}
		ok = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return false, wrapErr(err)
	}
	return ok, nil
}

// Get 取绑定，含已过期（判定在分配器）。没有绑定返回 (nil,nil)。
func (r egressRepo) Get(ctx context.Context, envID string) (*types.EgressBinding, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	var out *types.EgressBinding
	err = r.run(ctx, func(q querier) error {
		row := q.QueryRow(ctx, `SELECT `+egressCols+` FROM egress_bindings WHERE env_id=$1 AND tenant_id=$2`, envID, tid)
		got, err := scanEgress(tid, row.Scan)
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r egressRepo) ListByTenant(ctx context.Context) ([]types.EgressBinding, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.EgressBinding{}
	err = r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+egressCols+` FROM egress_bindings WHERE tenant_id=$1
			ORDER BY bound_at DESC`, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			got, err := scanEgress(tid, rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, *got)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

func (r egressRepo) Release(ctx context.Context, envID string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM egress_bindings WHERE env_id=$1 AND tenant_id=$2`, envID, tid)
		return err
	}))
}

// DeleteExpired 一次语句删掉到期绑定并返回被删行：回收任务要按 resource_id
// 向代理商归还资源，只拿计数就没法归还（资源会一直被计费到厂商侧自然过期）。
func (r egressRepo) DeleteExpired(ctx context.Context, tenantID string, now time.Time) ([]types.EgressBinding, error) {
	out := []types.EgressBinding{}
	err := r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `DELETE FROM egress_bindings
			WHERE tenant_id=$1 AND expires_at <= $2
			RETURNING `+egressCols, tenantID, now.UTC())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			got, err := scanEgress(tenantID, rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, *got)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// ---------- 出口绑定历史（只追加） ----------

const egressEventCols = `id,env_id,action,provider,resource_id,COALESCE(protocol,''),
	COALESCE(host,''),COALESCE(port,0),COALESCE(country,''),COALESCE(region,''),COALESCE(city,''),
	COALESCE(asn,''),COALESCE(asn_type,''),COALESCE(exit_addr,''),COALESCE(prev_exit,''),
	COALESCE(reason,''),occurred_at`

func scanEgressEvent(tid string, scan func(...any) error) (*types.EgressBindingEvent, error) {
	out := &types.EgressBindingEvent{TenantID: tid}
	if err := scan(&out.ID, &out.EnvID, &out.Action, &out.Provider, &out.ResourceID, &out.Protocol,
		&out.Host, &out.Port, &out.Country, &out.Region, &out.City, &out.ASN, &out.ASNType,
		&out.ExitAddr, &out.PrevExit, &out.Reason, &out.OccurredAt); err != nil {
		return nil, err
	}
	return out, nil
}

// AppendEvent 追加一条生命周期事件。
//
// 幂等按 id（ON CONFLICT DO NOTHING）：回收任务重投、启动重试都可能把同一条
// 事件写两遍，而「这个 ASN 被用过几次」这类统计经不起重复行。
func (r egressRepo) AppendEvent(ctx context.Context, ev *types.EgressBindingEvent) error {
	if ev == nil {
		return errors.New("pg: 出口历史事件不能为空")
	}
	if !types.ValidEgressAction(ev.Action) {
		// 先于 SQL 拦：让 mem/pg 两种驱动的错误点一致，也省一次往返。
		return fmt.Errorf("pg: 未知的出口历史动作 %q", ev.Action)
	}
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	if ev.ID == "" {
		ev.ID = ids.New()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO egress_binding_history(id,tenant_id,env_id,action,provider,resource_id,
			protocol,host,port,country,region,city,asn,asn_type,exit_addr,prev_exit,reason,occurred_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
			ON CONFLICT (id) DO NOTHING`,
			ev.ID, tid, ev.EnvID, ev.Action, ev.Provider, ev.ResourceID,
			nullStr(ev.Protocol), nullStr(ev.Host), ev.Port, nullStr(ev.Country), nullStr(ev.Region),
			nullStr(ev.City), nullStr(ev.ASN), nullStr(ev.ASNType), nullStr(ev.ExitAddr),
			nullStr(ev.PrevExit), nullStr(ev.Reason), ev.OccurredAt)
		return err
	}))
}

// ListEvents 倒序查历史事件。
func (r egressRepo) ListEvents(ctx context.Context, f store.EgressHistoryFilter) ([]types.EgressBindingEvent, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	where := `WHERE tenant_id=$1`
	args := []any{tid}
	idx := 2
	// 每个条件都显式带 tenant_id：RLS 是最后一道，不是唯一一道（L12）。
	if f.EnvID != "" {
		where += fmt.Sprintf(` AND env_id=$%d`, idx)
		args, idx = append(args, f.EnvID), idx+1
	}
	if f.Provider != "" {
		where += fmt.Sprintf(` AND provider=$%d`, idx)
		args, idx = append(args, f.Provider), idx+1
	}
	if f.ASN != "" {
		where += fmt.Sprintf(` AND asn=$%d`, idx)
		args, idx = append(args, f.ASN), idx+1
	}
	if f.Host != "" {
		where += fmt.Sprintf(` AND host=$%d`, idx)
		args, idx = append(args, f.Host), idx+1
	}
	if f.Action != "" {
		where += fmt.Sprintf(` AND action=$%d`, idx)
		args, idx = append(args, f.Action), idx+1
	}
	if f.From != nil {
		where += fmt.Sprintf(` AND occurred_at >= $%d`, idx)
		args, idx = append(args, *f.From), idx+1
	}
	if f.To != nil {
		where += fmt.Sprintf(` AND occurred_at <= $%d`, idx)
		args, idx = append(args, *f.To), idx+1
	}
	out := []types.EgressBindingEvent{}
	total := 0
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM egress_binding_history `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT `+egressEventCols+` FROM egress_binding_history `+where+
			` ORDER BY occurred_at DESC, id DESC LIMIT `+fmt.Sprint(limit)+` OFFSET `+fmt.Sprint(f.Offset), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			got, err := scanEgressEvent(tid, rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, *got)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, wrapErr(err)
	}
	return out, total, nil
}

// PruneEvents 按保留期裁剪历史。一次语句删除并计数（CTE），严格早于 before：
// 裁剪是运营决策（保留多久），宁少勿多——多删一条边界上的事件，共用 ASN 的证据链就断一截。
func (r egressRepo) PruneEvents(ctx context.Context, tenantID string, before time.Time) (int64, error) {
	var n int64
	err := r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `WITH del AS (
			DELETE FROM egress_binding_history WHERE tenant_id=$1 AND occurred_at < $2 RETURNING 1
		) SELECT COUNT(*) FROM del`, tenantID, before.UTC()).Scan(&n)
	})
	return n, wrapErr(err)
}

// ASNSharing 找出被至少 minEnvs 个不同环境使用过的 ASN。
//
// GROUP BY + HAVING COUNT(DISTINCT env_id)：同一个环境反复换绑到同一 ASN 不算
// 「共用」（那是一个店在漂移），必须是**不同环境**落在同一 ASN 上才是关联线索。
// 上限 100 组：这是给人看的告警列表，不是导出口径。
func (r egressRepo) ASNSharing(ctx context.Context, since time.Time, minEnvs int) ([]types.EgressASNShare, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	if minEnvs < 2 {
		minEnvs = 2
	}
	out := []types.EgressASNShare{}
	err = r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT asn, COALESCE(MAX(asn_type),''),
			array_agg(DISTINCT env_id), array_agg(DISTINCT provider),
			COALESCE(array_agg(DISTINCT exit_addr) FILTER (WHERE exit_addr IS NOT NULL AND exit_addr <> ''), '{}'),
			COUNT(*), MIN(occurred_at), MAX(occurred_at)
			FROM egress_binding_history
			WHERE tenant_id=$1 AND occurred_at >= $2 AND asn IS NOT NULL AND asn <> ''
			GROUP BY asn
			HAVING COUNT(DISTINCT env_id) >= $3
			ORDER BY COUNT(DISTINCT env_id) DESC, asn
			LIMIT 100`, tid, since.UTC(), minEnvs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s types.EgressASNShare
			if err := rows.Scan(&s.ASN, &s.ASNType, &s.EnvIDs, &s.Providers, &s.Exits,
				&s.Events, &s.FirstSeen, &s.LastSeen); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}
