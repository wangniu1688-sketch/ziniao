package pg

import (
	"context"
	"encoding/json"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ---------- 设备 ----------

type deviceRepo struct{ base }

func (r deviceRepo) Create(ctx context.Context, d *types.Device) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO devices(id,tenant_id,name,type,sn,ip,country,region,city,asn,asn_type,
			status,owner_id,protocol,proxy_host,proxy_port,proxy_user,proxy_secret_enc,health_score,purity_score,
			bound_accounts,concurrent_users,max_concurrent,expires_at,auto_renew,created_at,updated_at,deleted_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`,
			d.ID, tid, d.Name, string(d.Type), nullStr(d.SN), nullStr(d.IP), nullStr(d.Country),
			nullStr(d.Region), nullStr(d.City), nullStr(d.ASN), nullStr(d.ASNType), string(d.Status),
			nullStr(d.OwnerID), nullStr(d.Protocol), nullStr(d.ProxyHost), d.ProxyPort, nullStr(d.ProxyUser),
			nullStr(d.ProxySecretEnc), d.HealthScore, d.PurityScore, d.BoundAccounts, d.ConcurrentUsers,
			d.MaxConcurrent, d.ExpiresAt, d.AutoRenew, d.CreatedAt, d.UpdatedAt, d.DeletedAt)
		return err
	}))
}

func (r deviceRepo) Get(ctx context.Context, id string) (*types.Device, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.Device{}
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,name,type,COALESCE(sn,''),COALESCE(ip,''),COALESCE(country,''),
			COALESCE(region,''),COALESCE(city,''),COALESCE(asn,''),COALESCE(asn_type,''),status,COALESCE(owner_id,''),
			COALESCE(protocol,''),COALESCE(proxy_host,''),proxy_port,COALESCE(proxy_user,''),COALESCE(proxy_secret_enc,''),
			health_score,purity_score,bound_accounts,concurrent_users,max_concurrent,expires_at,auto_renew,
			created_at,updated_at,deleted_at FROM devices WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&out.ID, &out.Name, &out.Type, &out.SN, &out.IP, &out.Country, &out.Region, &out.City,
				&out.ASN, &out.ASNType, &out.Status, &out.OwnerID, &out.Protocol, &out.ProxyHost,
				&out.ProxyPort, &out.ProxyUser, &out.ProxySecretEnc, &out.HealthScore, &out.PurityScore,
				&out.BoundAccounts, &out.ConcurrentUsers, &out.MaxConcurrent, &out.ExpiresAt, &out.AutoRenew,
				&out.CreatedAt, &out.UpdatedAt, &out.DeletedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	out.TenantID = tid
	return out, nil
}

func (r deviceRepo) Update(ctx context.Context, d *types.Device) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE devices SET name=$3,type=$4,sn=$5,ip=$6,country=$7,region=$8,city=$9,
			asn=$10,asn_type=$11,status=$12,owner_id=$13,protocol=$14,proxy_host=$15,proxy_port=$16,proxy_user=$17,
			proxy_secret_enc=$18,health_score=$19,purity_score=$20,bound_accounts=$21,concurrent_users=$22,
			max_concurrent=$23,expires_at=$24,auto_renew=$25,updated_at=$26,deleted_at=$27
			WHERE id=$1 AND tenant_id=$2`,
			d.ID, tid, d.Name, string(d.Type), nullStr(d.SN), nullStr(d.IP), nullStr(d.Country),
			nullStr(d.Region), nullStr(d.City), nullStr(d.ASN), nullStr(d.ASNType), string(d.Status),
			nullStr(d.OwnerID), nullStr(d.Protocol), nullStr(d.ProxyHost), d.ProxyPort, nullStr(d.ProxyUser),
			nullStr(d.ProxySecretEnc), d.HealthScore, d.PurityScore, d.BoundAccounts, d.ConcurrentUsers,
			d.MaxConcurrent, d.ExpiresAt, d.AutoRenew, time.Now(), d.DeletedAt)
		return err
	}))
}

// Delete 软删除（写 deleted_at，与 shops 同口径）。幂等：已删除的行原样返回成功。
// 计量回退由服务层在同一事务内做，仓储不碰 usage_counters。
func (r deviceRepo) Delete(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE devices SET deleted_at=now(), updated_at=now()
			WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL`, id, tid)
		return err
	}))
}

func (r deviceRepo) List(ctx context.Context, f store.DeviceFilter) ([]types.Device, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	where := `WHERE tenant_id=$1 AND deleted_at IS NULL`
	args := []any{tid}
	n := 2
	if f.Type != "" {
		where += ` AND type=$` + itoa(n)
		args, n = append(args, string(f.Type)), n+1
	}
	if f.Status != "" {
		where += ` AND status=$` + itoa(n)
		args, n = append(args, string(f.Status)), n+1
	}
	if f.Country != "" {
		where += ` AND country=$` + itoa(n)
		args, n = append(args, f.Country), n+1
	}
	out := []types.Device{}
	total := 0
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM devices `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT id,name,type,COALESCE(sn,''),COALESCE(ip,''),COALESCE(country,''),
			COALESCE(region,''),COALESCE(city,''),COALESCE(asn,''),COALESCE(asn_type,''),status,COALESCE(owner_id,''),
			COALESCE(protocol,''),COALESCE(proxy_host,''),proxy_port,COALESCE(proxy_user,''),'' ,
			health_score,purity_score,bound_accounts,concurrent_users,max_concurrent,expires_at,auto_renew,
			created_at,updated_at,deleted_at FROM devices `+where+` ORDER BY created_at DESC
			LIMIT `+itoa(maxInt(f.Limit, 50))+` OFFSET `+itoa(f.Offset), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d types.Device
			if err := rows.Scan(&d.ID, &d.Name, &d.Type, &d.SN, &d.IP, &d.Country, &d.Region, &d.City,
				&d.ASN, &d.ASNType, &d.Status, &d.OwnerID, &d.Protocol, &d.ProxyHost, &d.ProxyPort,
				&d.ProxyUser, &d.ProxySecretEnc, &d.HealthScore, &d.PurityScore, &d.BoundAccounts,
				&d.ConcurrentUsers, &d.MaxConcurrent, &d.ExpiresAt, &d.AutoRenew, &d.CreatedAt,
				&d.UpdatedAt, &d.DeletedAt); err != nil {
				return err
			}
			d.TenantID = tid
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

func (r deviceRepo) Count(ctx context.Context) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM devices WHERE tenant_id=$1 AND deleted_at IS NULL`, tid).Scan(&n)
	})
	return n, wrapErr(err)
}

func (r deviceRepo) IncrBound(ctx context.Context, id string, delta int) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE devices SET bound_accounts=GREATEST(0, bound_accounts+$3)
			WHERE id=$1 AND tenant_id=$2`, id, tid, delta)
		return err
	}))
}

// ---------- 绑定关系 ----------

type bindingRepo struct{ base }

func (r bindingRepo) Create(ctx context.Context, b *types.DeviceBinding) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO device_bindings(id,tenant_id,device_id,shop_id,user_id,bound_at,last_used_at,unbound_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, b.ID, tid, b.DeviceID, b.ShopID, nullStr(b.UserID), b.BoundAt, b.LastUsedAt, b.UnboundAt)
		return err
	}))
}

func (r bindingRepo) List(ctx context.Context, f store.BindingFilter) ([]types.DeviceBinding, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	where := `WHERE tenant_id=$1`
	args := []any{tid}
	n := 2
	if f.Active {
		where += ` AND unbound_at IS NULL`
	}
	if f.DeviceID != "" {
		where += ` AND device_id=$` + itoa(n)
		args, n = append(args, f.DeviceID), n+1
	}
	if f.ShopID != "" {
		where += ` AND shop_id=$` + itoa(n)
		args, n = append(args, f.ShopID), n+1
	}
	if f.UserID != "" {
		where += ` AND user_id=$` + itoa(n)
		args, n = append(args, f.UserID), n+1
	}
	out := []types.DeviceBinding{}
	err = r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,device_id,shop_id,COALESCE(user_id,''),bound_at,last_used_at,unbound_at
			FROM device_bindings `+where, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b types.DeviceBinding
			if err := rows.Scan(&b.ID, &b.DeviceID, &b.ShopID, &b.UserID, &b.BoundAt, &b.LastUsedAt, &b.UnboundAt); err != nil {
				return err
			}
			b.TenantID = tid
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

func (r bindingRepo) Unbind(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE device_bindings SET unbound_at=now() WHERE id=$1 AND tenant_id=$2`, id, tid)
		return err
	}))
}

// CountConcurrent 按 user_id 去重（一人多店只占 1 个槽，L9）。
func (r bindingRepo) CountConcurrent(ctx context.Context, deviceID string) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(DISTINCT user_id) FROM device_bindings
			WHERE tenant_id=$1 AND device_id=$2 AND unbound_at IS NULL AND user_id <> ''`, tid, deviceID).Scan(&n)
	})
	return n, wrapErr(err)
}

func (r bindingRepo) CountBound(ctx context.Context, deviceID string) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM device_bindings
			WHERE tenant_id=$1 AND device_id=$2 AND unbound_at IS NULL`, tid, deviceID).Scan(&n)
	})
	return n, wrapErr(err)
}

func (r bindingRepo) Touch(ctx context.Context, id, userID string, at time.Time) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE device_bindings SET user_id=$3,last_used_at=$4 WHERE id=$1 AND tenant_id=$2`,
			id, tid, nullStr(userID), at)
		return err
	}))
}

// ---------- 环境 ----------

// 写 environments 的三条语句（抽成常量：列名与占位符一一对应要能被一眼核对，
// 也便于测试直接断言「占位符数 == 实参个数」）。
//
// 对照表（改任何一列都要同步改实参列表，execStmt 会在执行前校验）：
//
//	envInsertSQL    19 个占位符 / Create 传 19 个实参
//	envUpdateSQL    18 个占位符 / Update 传 18 个实参（inject_script=$18）
//	envSetStatusSQL  5 个占位符 / SetStatusIfIn 传 5 个实参（$5 = text[] 前置状态）
const (
	envInsertSQL = `INSERT INTO environments(id,tenant_id,shop_id,device_id,profile_id,baseline_hash,
		kernel_major,tls_hello_id,status,snapshot,consistency,created_at,updated_at,
		proxy_id,country,start_url,last_started_at,thumbnail_path,inject_script)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`

	envUpdateSQL = `UPDATE environments SET shop_id=$3,device_id=$4,profile_id=$5,baseline_hash=$6,
		kernel_major=$7,tls_hello_id=$8,status=$9,snapshot=$10,consistency=$11,updated_at=$12,
		proxy_id=$13,country=$14,start_url=$15,last_started_at=$16,thumbnail_path=$17,
		inject_script=$18 WHERE id=$1 AND tenant_id=$2`

	envSetStatusSQL = `UPDATE environments SET status=$3,updated_at=$4
		WHERE id=$1 AND tenant_id=$2 AND status = ANY($5)`

	envDeleteSQL = `DELETE FROM environments WHERE id=$1 AND tenant_id=$2`
)

type envRepo struct{ base }

func (r envRepo) Create(ctx context.Context, e *types.Environment) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		snapshot, err := json.Marshal(e.Snapshot)
		if err != nil {
			return err
		}
		consistency := e.Consistency
		if len(consistency) == 0 {
			consistency = []byte("{}")
		}
		// 0012：proxy_id / country / start_url / last_started_at / thumbnail_path
		// 全部可空；last_started_at 为 NULL 表示「从未成功拉起」。
		//
		// inject_script 必须一并落库：它是环境指纹的**唯一载体**，启动链路不自己
		// 现造脚本（现造 = 每次启动换一套指纹，等于没有画像）。漏写这一列的后果
		// 极隐蔽——窗口照开、CDP 照连，但页面里全是本机真实指纹。
		//
		// 列名与实参必须成对出现：整句 INSERT 有 19 个占位符，就传 19 个实参。
		// execStmt 会在执行前数一遍，少传一个直接报「哪句 SQL 少几个」而不是
		// 让 Postgres 抛一句没有上下文的 mismatched param and argument count。
		_, err = execStmt(ctx, q, envInsertSQL,
			e.ID, tid, nullStr(e.ShopID), nullStr(e.DeviceID), e.ProfileID, e.BaselineHash,
			e.KernelMajor, nullStr(e.TLSHelloID), e.Status, snapshot, consistency, e.CreatedAt, e.UpdatedAt,
			nullStr(e.ProxyID), nullStr(e.Country), nullStr(e.StartURL), nullTime(e.LastStartedAt),
			nullStr(e.ThumbnailPath), nullStr(e.InjectScript))
		return err
	}))
}

func (r envRepo) Get(ctx context.Context, id string) (*types.Environment, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.Environment{TenantID: tid}
	var snapshot, consistency []byte
	var last time.Time
	var script string
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,COALESCE(shop_id,''),COALESCE(device_id,''),profile_id,baseline_hash,
			kernel_major,COALESCE(tls_hello_id,''),status,snapshot,consistency,created_at,updated_at,
			COALESCE(proxy_id,''),COALESCE(country,''),COALESCE(start_url,''),
			COALESCE(last_started_at,'0001-01-01'::timestamptz),COALESCE(thumbnail_path,''),
			COALESCE(inject_script,'')
			FROM environments WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&out.ID, &out.ShopID, &out.DeviceID, &out.ProfileID, &out.BaselineHash,
				&out.KernelMajor, &out.TLSHelloID, &out.Status, &snapshot, &consistency,
				&out.CreatedAt, &out.UpdatedAt,
				&out.ProxyID, &out.Country, &out.StartURL, &last, &out.ThumbnailPath, &script)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	_ = json.Unmarshal(snapshot, &out.Snapshot)
	out.Consistency = consistency
	out.LastStartedAt = timePtr(last)
	out.InjectScript = script
	return out, nil
}

func (r envRepo) GetByShop(ctx context.Context, shopID string) (*types.Environment, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.Environment{TenantID: tid}
	var snapshot, consistency []byte
	var last time.Time
	var script string
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,COALESCE(shop_id,''),COALESCE(device_id,''),profile_id,baseline_hash,
			kernel_major,COALESCE(tls_hello_id,''),status,snapshot,consistency,created_at,updated_at,
			COALESCE(proxy_id,''),COALESCE(country,''),COALESCE(start_url,''),
			COALESCE(last_started_at,'0001-01-01'::timestamptz),COALESCE(thumbnail_path,''),
			COALESCE(inject_script,'')
			FROM environments WHERE shop_id=$1 AND tenant_id=$2 ORDER BY created_at DESC LIMIT 1`, shopID, tid).
			Scan(&out.ID, &out.ShopID, &out.DeviceID, &out.ProfileID, &out.BaselineHash,
				&out.KernelMajor, &out.TLSHelloID, &out.Status, &snapshot, &consistency,
				&out.CreatedAt, &out.UpdatedAt,
				&out.ProxyID, &out.Country, &out.StartURL, &last, &out.ThumbnailPath, &script)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	_ = json.Unmarshal(snapshot, &out.Snapshot)
	out.Consistency = consistency
	out.LastStartedAt = timePtr(last)
	out.InjectScript = script
	return out, nil
}

func (r envRepo) Update(ctx context.Context, e *types.Environment) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		snapshot, err := json.Marshal(e.Snapshot)
		if err != nil {
			return err
		}
		consistency := e.Consistency
		if len(consistency) == 0 {
			consistency = []byte("{}")
		}
		// 0012 列随整行一起更新（服务层保证语义：last_started_at 只在拉起成功时写）。
		//
		// inject_script 与 INSERT 同理：列名写了就必须传实参。上一版把列名加上了
		// 却漏了实参（17 个实参对 18 个占位符），结果是 MarkRunning / MarkLaunchFailed
		// 两条路径一起在阶段 8 报 mismatched param and argument count，环境悬在
		// running 上。现在由 execStmt 兜住：再漏就当场报出哪句 SQL 少几个参数。
		_, err = execStmt(ctx, q, envUpdateSQL,
			e.ID, tid, nullStr(e.ShopID), nullStr(e.DeviceID), e.ProfileID, e.BaselineHash,
			e.KernelMajor, nullStr(e.TLSHelloID), e.Status, snapshot, consistency, time.Now(),
			nullStr(e.ProxyID), nullStr(e.Country), nullStr(e.StartURL),
			nullTime(e.LastStartedAt), nullStr(e.ThumbnailPath), nullStr(e.InjectScript))
		return err
	}))
}

// SetStatusIfIn 只改状态的条件置位：仅当当前状态属于 from 时才置为 to。
//
// 为什么单独一条语句而不是复用 Update 整行写回：复位是**止损动作**，它的成功
// 必须只依赖「状态」这一列。整行 UPDATE 涉及 18 列（含 inject_script 这种大字段），
// 任何一列写错都会让「启动失败 → 复位」这条止损路径跟着崩，环境就悬在 running 上。
//
// 条件放在 SQL 里（而不是先读后写）是刻意的：读到的状态可能是几百毫秒前的，
// 「绝不覆盖 running」这条铁律必须由**语句自己**保证。
//
// 返回是否命中：false = 当前状态不在 from 内（多半已经 running），一行未改。
func (r envRepo) SetStatusIfIn(ctx context.Context, id string, from []string, to string) (bool, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return false, err
	}
	if to == "" || len(from) == 0 {
		return false, apierr.Validation("状态置位缺少目标状态或前置状态集合")
	}
	changed := false
	err = r.run(ctx, func(q querier) error {
		tag, err := execStmt(ctx, q, envSetStatusSQL, id, tid, to, time.Now(), from)
		if err != nil {
			return err
		}
		changed = tag.RowsAffected() > 0
		return nil
	})
	return changed, wrapErr(err)
}

func (r envRepo) List(ctx context.Context, limit, offset int) ([]types.Environment, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.Environment{}
	total := 0
	if limit <= 0 {
		limit = 50
	}
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM environments WHERE tenant_id=$1`, tid).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT id,COALESCE(shop_id,''),COALESCE(device_id,''),profile_id,baseline_hash,
			kernel_major,COALESCE(tls_hello_id,''),status,snapshot,consistency,created_at,updated_at,
			COALESCE(proxy_id,''),COALESCE(country,''),COALESCE(start_url,''),
			COALESCE(last_started_at,'0001-01-01'::timestamptz),COALESCE(thumbnail_path,''),
			COALESCE(inject_script,'') FROM environments
			WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, tid, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e types.Environment
			var snapshot, consistency []byte
			var last time.Time
			var script string
			if err := rows.Scan(&e.ID, &e.ShopID, &e.DeviceID, &e.ProfileID, &e.BaselineHash,
				&e.KernelMajor, &e.TLSHelloID, &e.Status, &snapshot, &consistency,
				&e.CreatedAt, &e.UpdatedAt,
				&e.ProxyID, &e.Country, &e.StartURL, &last, &e.ThumbnailPath, &script); err != nil {
				return err
			}
			_ = json.Unmarshal(snapshot, &e.Snapshot)
			e.Consistency = consistency
			e.LastStartedAt = timePtr(last)
			e.TenantID = tid
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

// Search 带过滤的聚合列表（M2 店铺网格页）：过滤条件全部下推到 SQL，
// 与 mem 实现同口径（已软删店铺的环境不返回）。
//
// tenant_id 谓词显式带上（不只靠 RLS）：读路径的显式谓词才是防线，
// RLS 是第二道（见 pg/launcher.go 顶部说明）。
func (r envRepo) Search(ctx context.Context, f store.EnvironmentFilter) ([]types.Environment, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	where := `WHERE e.tenant_id=$1 AND s.deleted_at IS NULL`
	args := []any{tid}
	if len(f.ShopIDs) > 0 {
		args = append(args, f.ShopIDs)
		where += ` AND e.shop_id = ANY($` + itoa(len(args)) + `)`
	}
	if f.GroupID != "" {
		args = append(args, f.GroupID)
		where += ` AND s.group_id=$` + itoa(len(args))
	}
	if f.Platform != "" {
		args = append(args, f.Platform)
		where += ` AND s.platform=$` + itoa(len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		where += ` AND e.status=$` + itoa(len(args))
	}
	if f.Country != "" {
		args = append(args, f.Country)
		where += ` AND e.country=$` + itoa(len(args))
	}
	if f.ProxyID != "" {
		args = append(args, f.ProxyID)
		where += ` AND e.proxy_id=$` + itoa(len(args))
	}
	if kw := f.Keyword; kw != "" {
		args = append(args, "%"+kw+"%")
		n := itoa(len(args))
		where += ` AND (s.name ILIKE $` + n + ` OR COALESCE(s.code,'') ILIKE $` + n +
			` OR e.id ILIKE $` + n + `)`
	}
	cols := `e.id,e.tenant_id,COALESCE(e.shop_id,''),COALESCE(e.device_id,''),e.profile_id,e.baseline_hash,
		e.kernel_major,COALESCE(e.tls_hello_id,''),e.status,e.snapshot,e.consistency,e.created_at,e.updated_at,
		COALESCE(e.proxy_id,''),COALESCE(e.country,''),COALESCE(e.start_url,''),
		COALESCE(e.last_started_at,'0001-01-01'::timestamptz),COALESCE(e.thumbnail_path,''),
		COALESCE(e.inject_script,'')`
	from := ` FROM environments e JOIN shops s ON s.id=e.shop_id AND s.tenant_id=e.tenant_id `
	out := []types.Environment{}
	total := 0
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*)`+from+where, args...).Scan(&total); err != nil {
			return err
		}
		paged := append(append([]any{}, args...), limit, offset)
		rows, err := q.Query(ctx, `SELECT `+cols+from+where+
			` ORDER BY e.created_at DESC LIMIT $`+itoa(len(paged)-1)+` OFFSET $`+itoa(len(paged)), paged...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e types.Environment
			var snapshot, consistency []byte
			var last time.Time
			var script string
			if err := rows.Scan(&e.ID, &e.TenantID, &e.ShopID, &e.DeviceID, &e.ProfileID, &e.BaselineHash,
				&e.KernelMajor, &e.TLSHelloID, &e.Status, &snapshot, &consistency,
				&e.CreatedAt, &e.UpdatedAt,
				&e.ProxyID, &e.Country, &e.StartURL, &last, &e.ThumbnailPath, &script); err != nil {
				return err
			}
			_ = json.Unmarshal(snapshot, &e.Snapshot)
			e.Consistency = consistency
			e.LastStartedAt = timePtr(last)
			e.InjectScript = script
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

// Delete 物理删除环境行（终态动作）。出口归还与事件留痕由服务层负责，仓储只删行。
// 幂等：行不存在原样返回成功（重复删除不假成功靠上层事件与审计区分）。
func (r envRepo) Delete(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := execStmt(ctx, q, envDeleteSQL, id, tid)
		return err
	}))
}

// ---------- 访问策略 ----------

type policyRepo struct{ base }

func (r policyRepo) Create(ctx context.Context, p *types.AccessPolicy) error {
	return r.save(ctx, p)
}

func (r policyRepo) Update(ctx context.Context, p *types.AccessPolicy) error {
	return r.save(ctx, p)
}

func (r policyRepo) save(ctx context.Context, p *types.AccessPolicy) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	rules, err := json.Marshal(p.Rules)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO access_policies(id,tenant_id,name,scope,scope_id,template,enabled,rules,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,scope=EXCLUDED.scope,scope_id=EXCLUDED.scope_id,
			template=EXCLUDED.template,enabled=EXCLUDED.enabled,rules=EXCLUDED.rules,updated_at=EXCLUDED.updated_at`,
			p.ID, tid, p.Name, p.Scope, nullStr(p.ScopeID), nullStr(p.Template), p.Enabled, rules, time.Now())
		return err
	}))
}

func (r policyRepo) Get(ctx context.Context, id string) (*types.AccessPolicy, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.AccessPolicy{TenantID: tid}
	var raw []byte
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,name,scope,COALESCE(scope_id,''),COALESCE(template,''),enabled,rules,updated_at
			FROM access_policies WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&out.ID, &out.Name, &out.Scope, &out.ScopeID, &out.Template, &out.Enabled, &raw, &out.UpdatedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	_ = json.Unmarshal(raw, &out.Rules)
	return out, nil
}

func (r policyRepo) List(ctx context.Context, scope, scopeID string) ([]types.AccessPolicy, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.AccessPolicy{}
	err = r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,name,scope,COALESCE(scope_id,''),COALESCE(template,''),enabled,rules,updated_at
			FROM access_policies WHERE tenant_id=$1
			AND ($2 = '' OR scope=$2) AND ($3 = '' OR scope_id=$3) ORDER BY updated_at DESC`, tid, scope, scopeID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p types.AccessPolicy
			var raw []byte
			if err := rows.Scan(&p.ID, &p.Name, &p.Scope, &p.ScopeID, &p.Template, &p.Enabled, &raw, &p.UpdatedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(raw, &p.Rules)
			p.TenantID = tid
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

func (r policyRepo) Delete(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM access_policies WHERE id=$1 AND tenant_id=$2`, id, tid)
		return err
	}))
}

// ---------- 安全事件 ----------

type secEventRepo struct{ base }

func (r secEventRepo) Create(ctx context.Context, e *types.SecurityEvent) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	detail := e.Detail
	if len(detail) == 0 {
		detail = []byte("{}")
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO security_events(id,tenant_id,category,level,shop_id,device_id,actor_user_id,action,detail,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			e.ID, tid, e.Category, e.Level, nullStr(e.ShopID), nullStr(e.DeviceID),
			nullStr(e.ActorUserID), e.Action, detail, e.CreatedAt)
		return err
	}))
}

func (r secEventRepo) List(ctx context.Context, f store.SecurityEventFilter) ([]types.SecurityEvent, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	where := `WHERE tenant_id=$1`
	args := []any{tid}
	n := 2
	if f.Category != "" {
		where += ` AND category=$` + itoa(n)
		args, n = append(args, f.Category), n+1
	}
	if f.Level != "" {
		where += ` AND level=$` + itoa(n)
		args, n = append(args, f.Level), n+1
	}
	if f.ShopID != "" {
		where += ` AND shop_id=$` + itoa(n)
		args, n = append(args, f.ShopID), n+1
	}
	if f.From != nil {
		where += ` AND created_at >= $` + itoa(n)
		args, n = append(args, *f.From), n+1
	}
	out := []types.SecurityEvent{}
	total := 0
	err = r.run(ctx, func(q querier) error {
		if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM security_events `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT id,category,level,COALESCE(shop_id,''),COALESCE(device_id,''),
			COALESCE(actor_user_id,''),action,detail,created_at FROM security_events `+where+`
			ORDER BY created_at DESC LIMIT `+itoa(maxInt(f.Limit, 50))+` OFFSET `+itoa(f.Offset), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e types.SecurityEvent
			if err := rows.Scan(&e.ID, &e.Category, &e.Level, &e.ShopID, &e.DeviceID,
				&e.ActorUserID, &e.Action, &e.Detail, &e.CreatedAt); err != nil {
				return err
			}
			e.TenantID = tid
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
}

// ---------- 凭证托管 ----------

type vaultRepo struct{ base }

func (r vaultRepo) Upsert(ctx context.Context, c *types.ShopCredential) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO shop_credentials(id,tenant_id,shop_id,kind,username,secret_enc,algo,key_version,updated_at,updated_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT(tenant_id,shop_id,kind) DO UPDATE SET username=EXCLUDED.username,
			secret_enc=EXCLUDED.secret_enc,algo=EXCLUDED.algo,key_version=EXCLUDED.key_version,
			updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by`,
			c.ID, tid, c.ShopID, string(c.Kind), nullStr(c.Username), c.SecretEnc, c.Algo,
			c.KeyVersion, time.Now(), nullStr(c.UpdatedBy))
		return err
	}))
}

func (r vaultRepo) Get(ctx context.Context, shopID string, kind types.CredentialKind) (*types.ShopCredential, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.ShopCredential{TenantID: tid, ShopID: shopID, Kind: kind}
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,COALESCE(username,''),secret_enc,algo,key_version,updated_at,COALESCE(updated_by,'')
			FROM shop_credentials WHERE tenant_id=$1 AND shop_id=$2 AND kind=$3`, tid, shopID, string(kind)).
			Scan(&out.ID, &out.Username, &out.SecretEnc, &out.Algo, &out.KeyVersion, &out.UpdatedAt, &out.UpdatedBy)
	})
	return out, wrapErr(err)
}

func (r vaultRepo) List(ctx context.Context, shopID string) ([]types.ShopCredential, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.ShopCredential{}
	err = r.run(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT id,kind,COALESCE(username,''),secret_enc,algo,key_version,updated_at,COALESCE(updated_by,'')
			FROM shop_credentials WHERE tenant_id=$1 AND shop_id=$2`, tid, shopID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c types.ShopCredential
			if err := rows.Scan(&c.ID, &c.Kind, &c.Username, &c.SecretEnc, &c.Algo, &c.KeyVersion, &c.UpdatedAt, &c.UpdatedBy); err != nil {
				return err
			}
			c.TenantID, c.ShopID = tid, shopID
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, wrapErr(err)
}

func (r vaultRepo) Delete(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM shop_credentials WHERE id=$1 AND tenant_id=$2`, id, tid)
		return err
	}))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	if i < 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	return out
}

func maxInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
