package pg

import (
	"context"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// M1 启动器内核（0012）：手工静态代理台账与店铺分组。
//
// 租户隔离靠两条腿：显式 tenant_id 谓词（读路径）+ RLS WITH CHECK（写路径，0012 已建）。
// 两条都要有——只靠 RLS 的话，一旦某天有人用 superuser 连上来做维护，
// 「忘了带 tenant_id」就从「零行」变成「全表」。
//
// password_enc 只写不读（读出来也要立刻喂给本地 TLSProxy，绝不经 API）：
// 全文件没有任何 SELECT 把它带出来的地方。

// ---------- 手工静态代理 ----------

// proxyColumns 读列清单：**显式排除 password_enc**（密文只经 PasswordEnc() 取）。
//
// P3 新增的 8 列里，可空列一律 COALESCE 成零值再扫，避免把 sql.Null* 的
// 三态处理散到两个驱动里——mem 驱动没有 NULL 这个概念，两边必须同口径。
const proxyColumns = `id,tenant_id,name,protocol,host,port,COALESCE(username,''),
	COALESCE(country,''),COALESCE(note,''),status,key_version,
	(COALESCE(password_enc,'') <> '') AS has_password,
	COALESCE(type,''),COALESCE(city,''),COALESCE(check_status,'unknown'),
	COALESCE(exit_ip,''),COALESCE(exit_country,''),latency_ms,last_checked_at,
	COALESCE(last_error,''),created_at,updated_at`

type proxyRepo struct{ base }

func (r proxyRepo) Create(ctx context.Context, p *types.Proxy) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		now := time.Now()
		if p.CreatedAt.IsZero() {
			p.CreatedAt = now
		}
		if p.UpdatedAt.IsZero() {
			p.UpdatedAt = now
		}
		_, err := q.Exec(ctx, `INSERT INTO proxies(id,tenant_id,name,protocol,host,port,username,password_enc,
			key_version,country,note,status,type,city,check_status,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			p.ID, tid, p.Name, p.Protocol, p.Host, p.Port,
			nullStr(p.Username), nullStr(p.PasswordEnc), p.KeyVersion,
			nullStr(p.Country), nullStr(p.Note), p.Status,
			nullStr(proxyTypeOr(p.Type)), nullStr(p.City), nullStr(proxyCheckOr(p.CheckStatus)),
			p.CreatedAt, p.UpdatedAt)
		return err
	}))
}

func (r proxyRepo) Get(ctx context.Context, id string) (*types.Proxy, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.Proxy{TenantID: tid}
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT `+proxyColumns+` FROM proxies WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&out.ID, &out.TenantID, &out.Name, &out.Protocol, &out.Host, &out.Port,
				&out.Username, &out.Country, &out.Note, &out.Status, &out.KeyVersion,
				&out.HasPassword,
				&out.Type, &out.City, &out.CheckStatus, &out.ExitIP, &out.ExitCountry,
				&out.LatencyMS, &out.LastCheckedAt, &out.LastError,
				&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

func (r proxyRepo) List(ctx context.Context, f store.ProxyFilter) ([]*types.Proxy, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	limit, offset := f.Limit, f.Offset
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	where := `WHERE tenant_id=$1`
	args := []any{tid}
	if f.Status != "" {
		args = append(args, f.Status)
		where += ` AND status=$` + itoa(len(args))
	}
	if f.CheckStatus != "" {
		args = append(args, f.CheckStatus)
		where += ` AND check_status=$` + itoa(len(args))
	}
	if kw := f.Keyword; kw != "" {
		args = append(args, "%"+kw+"%")
		where += ` AND (name ILIKE $` + itoa(len(args)) +
			` OR host ILIKE $` + itoa(len(args)) + ` OR COALESCE(note,'') ILIKE $` + itoa(len(args)) + `)`
	}
	out := []*types.Proxy{}
	total := 0
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM proxies `+where, args...).Scan(&total); err != nil {
			return err
		}
		args := append(args, limit, offset)
		rows, err := q.Query(ctx, `SELECT `+proxyColumns+` FROM proxies `+where+
			` ORDER BY created_at DESC LIMIT $`+itoa(len(args)-1)+` OFFSET $`+itoa(len(args)), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p types.Proxy
			if err := rows.Scan(&p.ID, &p.TenantID, &p.Name, &p.Protocol, &p.Host, &p.Port,
				&p.Username, &p.Country, &p.Note, &p.Status, &p.KeyVersion,
				&p.HasPassword,
				&p.Type, &p.City, &p.CheckStatus, &p.ExitIP, &p.ExitCountry,
				&p.LatencyMS, &p.LastCheckedAt, &p.LastError,
				&p.CreatedAt, &p.UpdatedAt); err != nil {
				return err
			}
			out = append(out, &p)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

func (r proxyRepo) Update(ctx context.Context, p *types.Proxy) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		if p.UpdatedAt.IsZero() {
			p.UpdatedAt = time.Now()
		}
		_, err := q.Exec(ctx, `UPDATE proxies SET name=$3,protocol=$4,host=$5,port=$6,username=$7,
			password_enc=$8,key_version=$9,country=$10,note=$11,status=$12,
			type=$13,city=$14,check_status=$15,updated_at=$16
			WHERE id=$1 AND tenant_id=$2`,
			p.ID, tid, p.Name, p.Protocol, p.Host, p.Port, nullStr(p.Username),
			nullStr(p.PasswordEnc), p.KeyVersion, nullStr(p.Country), nullStr(p.Note),
			p.Status, nullStr(proxyTypeOr(p.Type)), nullStr(p.City),
			nullStr(proxyCheckOr(p.CheckStatus)), p.UpdatedAt)
		return err
	}))
}

func (r proxyRepo) Delete(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	// 被引用的代理不允许删：删掉等于让若干环境的出口凭空消失，
	// 下一次启动会静默回落到代理商池（换 IP），那是关联风险而不是「清理」。
	n, err := r.CountBound(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return apierr.Conflict("该代理仍被引用，请先解绑")
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM proxies WHERE id=$1 AND tenant_id=$2`, id, tid)
		return err
	}))
}

// CountBound 该代理被本租户多少个环境引用（实时计数，删除前置校验的唯一判据）。
func (r proxyRepo) CountBound(ctx context.Context, id string) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM environments WHERE tenant_id=$1 AND proxy_id=$2`, tid, id).Scan(&n)
	})
	return n, wrapErr(err)
}

// CountBoundRunning 该代理被多少个**运行中**环境引用（P3 一号一 IP 告警）。
func (r proxyRepo) CountBoundRunning(ctx context.Context, id string) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM environments
			WHERE tenant_id=$1 AND proxy_id=$2 AND status='running'`, tid, id).Scan(&n)
	})
	return n, wrapErr(err)
}

// SaveCheckResult 只写拨测列（P3）。
//
// **绝不触碰 password_enc**：拨测在启动链路上会被反复调用，调用方手里的
// types.Proxy 来自常规读路径（PasswordEnc 恒为空），走整行 UPDATE 等于
// 每次启动都把口令清一次。这里用一条只含 6 列的语句把这件事变成不可能。
func (r proxyRepo) SaveCheckResult(ctx context.Context, id string, c store.ProxyCheckResult) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	status := c.CheckStatus
	if !types.ValidProxyCheckStatus(status) {
		status = types.ProxyCheckUnknown
	}
	checkedAt := c.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE proxies SET check_status=$3,exit_ip=$4,exit_country=$5,
			latency_ms=$6,last_checked_at=$7,last_error=$8,updated_at=$9
			WHERE id=$1 AND tenant_id=$2`,
			id, tid, status, nullStr(c.ExitIP), nullStr(c.ExitCountry),
			c.LatencyMS, checkedAt, nullStr(c.LastError), time.Now().UTC())
		return err
	}))
}

// AppendProbeHistory 只追加拨测事实，不保存目标 URL 或任何代理凭据。
func (r proxyRepo) AppendProbeHistory(ctx context.Context, h *types.ProxyProbeHistory) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	if h == nil || h.ID == "" || h.ProxyID == "" {
		return apierr.Validation("拨测历史缺少必要标识")
	}
	if !types.ValidProxyCheckStatus(h.CheckStatus) || h.CheckStatus == types.ProxyCheckUnknown {
		return apierr.Validation("拨测历史状态无效")
	}
	if h.LatencyMS < 0 || h.StatusCode < 0 || h.StatusCode > 599 {
		return apierr.Validation("拨测历史数值无效")
	}
	if h.CheckedAt.IsZero() {
		h.CheckedAt = time.Now().UTC()
	}
	if len([]rune(h.Error)) > 200 {
		h.Error = string([]rune(h.Error)[:200])
	}
	h.TenantID = tid
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO proxy_probe_history
			(id,tenant_id,proxy_id,check_status,status_code,latency_ms,error_message,checked_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			h.ID, tid, h.ProxyID, h.CheckStatus, h.StatusCode, h.LatencyMS,
			nullStr(h.Error), h.CheckedAt)
		return err
	}))
}

// ListProbeHistory 返回最近记录；SQL 中显式 tenant_id + proxy_id 双重过滤。
func (r proxyRepo) ListProbeHistory(ctx context.Context, proxyID string, limit int) ([]types.ProxyProbeHistory, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	out := []types.ProxyProbeHistory{}
	err = r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,tenant_id,proxy_id,check_status,status_code,
			latency_ms,COALESCE(error_message,''),checked_at
			FROM proxy_probe_history
			WHERE tenant_id=$1 AND proxy_id=$2
			ORDER BY checked_at DESC,id DESC LIMIT $3`, tid, proxyID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h types.ProxyProbeHistory
			if err := rows.Scan(&h.ID,&h.TenantID,&h.ProxyID,&h.CheckStatus,&h.StatusCode,
				&h.LatencyMS,&h.Error,&h.CheckedAt); err != nil {
				return err
			}
			out = append(out,h)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

// proxyTypeOr / proxyCheckOr 枚举兜底（DB 有 CHECK，mem 没有，Go 侧必须再判一次）。
//
// 空串走到这里只有一种来源：历史行（0013 之前建的）或 mem 驱动里手工塞的零值。
// 给默认值而不是报错，是为了让「读历史数据」这种只读路径不至于因为一次
// 迁移没跑就整个不可用——写路径的白名单校验在服务层，不在这里。
func proxyTypeOr(t string) string {
	if t == "" {
		return types.ProxyTypeISP
	}
	return t
}

func proxyCheckOr(s string) string {
	if s == "" {
		return types.ProxyCheckUnknown
	}
	return s
}

// PasswordEnc 取口令密文（唯一读密文的入口，见接口注释）。
func (r proxyRepo) PasswordEnc(ctx context.Context, id string) (string, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return "", err
	}
	var enc string
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COALESCE(password_enc,'') FROM proxies WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&enc)
	})
	return enc, wrapErr(err)
}

// ---------- 店铺分组 ----------

type groupRepo struct{ base }

func (r groupRepo) Create(ctx context.Context, g *types.Group) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		now := time.Now()
		if g.CreatedAt.IsZero() {
			g.CreatedAt = now
		}
		if g.UpdatedAt.IsZero() {
			g.UpdatedAt = now
		}
		_, err := q.Exec(ctx, `INSERT INTO groups(id,tenant_id,name,sort,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6)`, g.ID, tid, g.Name, g.Sort, g.CreatedAt, g.UpdatedAt)
		return err
	}))
}

func (r groupRepo) Get(ctx context.Context, id string) (*types.Group, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.Group{TenantID: tid}
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,tenant_id,name,sort,created_at,updated_at,
			(SELECT COUNT(*) FROM shops s WHERE s.tenant_id=$2 AND s.group_id=g.id AND s.deleted_at IS NULL) AS environment_count
			FROM groups g WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&out.ID, &out.TenantID, &out.Name, &out.Sort, &out.CreatedAt, &out.UpdatedAt, &out.EnvironmentCount)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

// List 一次 GROUP BY 带出 environment_count（不建缓存列，见 0012 注释）。
func (r groupRepo) List(ctx context.Context) ([]*types.Group, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []*types.Group{}
	err = r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT g.id,g.tenant_id,g.name,g.sort,g.created_at,g.updated_at,
			COUNT(s.id) FILTER (WHERE s.deleted_at IS NULL) AS environment_count
			FROM groups g
			LEFT JOIN shops s ON s.group_id = g.id AND s.tenant_id = g.tenant_id
			WHERE g.tenant_id = $1
			GROUP BY g.id,g.tenant_id,g.name,g.sort,g.created_at,g.updated_at
			ORDER BY g.sort ASC, g.created_at ASC`, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g types.Group
			if err := rows.Scan(&g.ID, &g.TenantID, &g.Name, &g.Sort, &g.CreatedAt, &g.UpdatedAt, &g.EnvironmentCount); err != nil {
				return err
			}
			out = append(out, &g)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

func (r groupRepo) Update(ctx context.Context, g *types.Group) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		if g.UpdatedAt.IsZero() {
			g.UpdatedAt = time.Now()
		}
		_, err := q.Exec(ctx, `UPDATE groups SET name=$3,sort=$4,updated_at=$5 WHERE id=$1 AND tenant_id=$2`,
			g.ID, tid, g.Name, g.Sort, g.UpdatedAt)
		return err
	}))
}

func (r groupRepo) Delete(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	n, err := r.CountMembers(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return apierr.Conflict("分组下仍有店铺，请先移出或改组")
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM groups WHERE id=$1 AND tenant_id=$2`, id, tid)
		return err
	}))
}

// CountMembers 「空分组才能删除」的唯一判据：该分组下未软删的店铺数（一店铺一环境）。
func (r groupRepo) CountMembers(ctx context.Context, id string) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM shops WHERE tenant_id=$1 AND group_id=$2 AND deleted_at IS NULL`, tid, id).Scan(&n)
	})
	return n, wrapErr(err)
}
