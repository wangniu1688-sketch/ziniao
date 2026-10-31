package pg

import (
	"context"
	"time"

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
