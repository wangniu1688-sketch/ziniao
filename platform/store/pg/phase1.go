package pg

import (
	"context"
	"encoding/json"
	"time"

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
		_, err = q.Exec(ctx, `INSERT INTO environments(id,tenant_id,shop_id,device_id,profile_id,baseline_hash,
			kernel_major,tls_hello_id,status,snapshot,consistency,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			e.ID, tid, nullStr(e.ShopID), nullStr(e.DeviceID), e.ProfileID, e.BaselineHash,
			e.KernelMajor, nullStr(e.TLSHelloID), e.Status, snapshot, consistency, e.CreatedAt, e.UpdatedAt)
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
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,COALESCE(shop_id,''),COALESCE(device_id,''),profile_id,baseline_hash,
			kernel_major,COALESCE(tls_hello_id,''),status,snapshot,consistency,created_at,updated_at
			FROM environments WHERE id=$1 AND tenant_id=$2`, id, tid).
			Scan(&out.ID, &out.ShopID, &out.DeviceID, &out.ProfileID, &out.BaselineHash,
				&out.KernelMajor, &out.TLSHelloID, &out.Status, &snapshot, &consistency,
				&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	_ = json.Unmarshal(snapshot, &out.Snapshot)
	out.Consistency = consistency
	return out, nil
}

func (r envRepo) GetByShop(ctx context.Context, shopID string) (*types.Environment, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	out := &types.Environment{TenantID: tid}
	var snapshot, consistency []byte
	err = r.run(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT id,COALESCE(shop_id,''),COALESCE(device_id,''),profile_id,baseline_hash,
			kernel_major,COALESCE(tls_hello_id,''),status,snapshot,consistency,created_at,updated_at
			FROM environments WHERE shop_id=$1 AND tenant_id=$2 ORDER BY created_at DESC LIMIT 1`, shopID, tid).
			Scan(&out.ID, &out.ShopID, &out.DeviceID, &out.ProfileID, &out.BaselineHash,
				&out.KernelMajor, &out.TLSHelloID, &out.Status, &snapshot, &consistency,
				&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	_ = json.Unmarshal(snapshot, &out.Snapshot)
	out.Consistency = consistency
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
		_, err = q.Exec(ctx, `UPDATE environments SET shop_id=$3,device_id=$4,profile_id=$5,baseline_hash=$6,
			kernel_major=$7,tls_hello_id=$8,status=$9,snapshot=$10,consistency=$11,updated_at=$12 WHERE id=$1 AND tenant_id=$2`,
			e.ID, tid, nullStr(e.ShopID), nullStr(e.DeviceID), e.ProfileID, e.BaselineHash,
			e.KernelMajor, nullStr(e.TLSHelloID), e.Status, snapshot, consistency, time.Now())
		return err
	}))
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
			kernel_major,COALESCE(tls_hello_id,''),status,snapshot,consistency,created_at,updated_at FROM environments
			WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, tid, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e types.Environment
			var snapshot, consistency []byte
			if err := rows.Scan(&e.ID, &e.ShopID, &e.DeviceID, &e.ProfileID, &e.BaselineHash,
				&e.KernelMajor, &e.TLSHelloID, &e.Status, &snapshot, &consistency,
				&e.CreatedAt, &e.UpdatedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(snapshot, &e.Snapshot)
			e.Consistency = consistency
			e.TenantID = tid
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, total, wrapErr(err)
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
