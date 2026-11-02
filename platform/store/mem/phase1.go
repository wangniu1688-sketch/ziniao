package mem

import (
	"context"
	"sort"
	"strings"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ---------- 设备 ----------

type deviceRepo struct{ s *Store }

func (r deviceRepo) tenant(ctx context.Context) (string, error) { return store.TenantFrom(ctx) }

func (r deviceRepo) Create(ctx context.Context, d *types.Device) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	if _, ok := r.s.devices[d.ID]; ok {
		return conflict("device", d.ID)
	}
	cp := *d
	cp.TenantID = tid
	if cp.MaxConcurrent <= 0 {
		cp.MaxConcurrent = 6 // 配置中心默认值（规格明确值，L9）
	}
	r.s.devices[d.ID] = &cp
	return nil
}

func (r deviceRepo) Get(ctx context.Context, id string) (*types.Device, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	d, ok := r.s.devices[id]
	if !ok || d.TenantID != tid {
		return nil, notFound("device", id)
	}
	cp := *d
	return &cp, nil
}

func (r deviceRepo) Update(ctx context.Context, d *types.Device) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	old, ok := r.s.devices[d.ID]
	if !ok || old.TenantID != tid {
		return notFound("device", d.ID)
	}
	cp := *d
	cp.TenantID = tid
	cp.UpdatedAt = time.Now()
	r.s.devices[d.ID] = &cp
	return nil
}

func (r deviceRepo) List(ctx context.Context, f store.DeviceFilter) ([]types.Device, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.Device{}
	for _, d := range r.s.devices {
		if d.TenantID != tid {
			continue
		}
		if !f.IncludeDeleted && d.DeletedAt != nil {
			continue
		}
		if f.Type != "" && d.Type != f.Type {
			continue
		}
		if f.Status != "" && d.Status != f.Status {
			continue
		}
		if f.Country != "" && d.Country != f.Country {
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	total := len(out)
	if f.Offset > 0 && f.Offset < len(out) {
		out = out[f.Offset:]
	} else if f.Offset >= len(out) {
		out = nil
	}
	if f.Limit > 0 && f.Limit < len(out) {
		out = out[:f.Limit]
	}
	return out, total, nil
}

func (r deviceRepo) Count(ctx context.Context) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range r.s.devices {
		if d.TenantID == tid && d.DeletedAt == nil {
			n++
		}
	}
	return n, nil
}

func (r deviceRepo) IncrBound(ctx context.Context, id string, delta int) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	d, ok := r.s.devices[id]
	if !ok || d.TenantID != tid {
		return notFound("device", id)
	}
	d.BoundAccounts += delta
	if d.BoundAccounts < 0 {
		d.BoundAccounts = 0
	}
	return nil
}

// Delete 软删除（写 deleted_at，与 shops 同口径）：台账出清但行保留，
// 计量回退由服务层在**同一事务**内做，仓储不碰 usage_counters。
func (r deviceRepo) Delete(ctx context.Context, id string) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	d, ok := r.s.devices[id]
	if !ok || d.TenantID != tid {
		return notFound("device", id)
	}
	if d.DeletedAt != nil {
		return nil // 幂等：重复删除不假成功也不报错
	}
	now := time.Now()
	d.DeletedAt = &now
	d.UpdatedAt = now
	return nil
}

// ---------- 绑定关系 ----------

type bindingRepo struct{ s *Store }

func (r bindingRepo) Create(ctx context.Context, b *types.DeviceBinding) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	for _, x := range r.s.bindings {
		if x.DeviceID == b.DeviceID && x.ShopID == b.ShopID && x.UnboundAt == nil {
			return conflict("binding", b.DeviceID+"/"+b.ShopID)
		}
	}
	cp := *b
	cp.TenantID = tid
	r.s.bindings = append(r.s.bindings, &cp)
	return nil
}

func (r bindingRepo) List(ctx context.Context, f store.BindingFilter) ([]types.DeviceBinding, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.DeviceBinding{}
	for _, b := range r.s.bindings {
		if b.TenantID != tid {
			continue
		}
		if f.Active && b.UnboundAt != nil {
			continue
		}
		if f.DeviceID != "" && b.DeviceID != f.DeviceID {
			continue
		}
		if f.ShopID != "" && b.ShopID != f.ShopID {
			continue
		}
		if f.UserID != "" && b.UserID != f.UserID {
			continue
		}
		out = append(out, *b)
	}
	return out, nil
}

func (r bindingRepo) Unbind(ctx context.Context, id string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	for _, b := range r.s.bindings {
		if b.ID == id && b.TenantID == tid {
			now := time.Now()
			b.UnboundAt = &now
			return nil
		}
	}
	return notFound("binding", id)
}

// CountConcurrent 并发人数按 user_id 去重：一人多店只占 1 个槽（L9）。
func (r bindingRepo) CountConcurrent(ctx context.Context, deviceID string) (int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return 0, err
	}
	users := map[string]struct{}{}
	for _, b := range r.s.bindings {
		if b.TenantID != tid || b.DeviceID != deviceID || b.UnboundAt != nil {
			continue
		}
		if b.UserID == "" {
			continue
		}
		users[b.UserID] = struct{}{}
	}
	return len(users), nil
}

func (r bindingRepo) CountBound(ctx context.Context, deviceID string) (int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range r.s.bindings {
		if b.TenantID == tid && b.DeviceID == deviceID && b.UnboundAt == nil {
			n++
		}
	}
	return n, nil
}

func (r bindingRepo) Touch(ctx context.Context, id, userID string, at time.Time) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	for _, b := range r.s.bindings {
		if b.ID == id && b.TenantID == tid {
			b.UserID = userID
			b.LastUsedAt = at
			return nil
		}
	}
	return notFound("binding", id)
}

// ---------- 环境 ----------

type envRepo struct{ s *Store }

func (r envRepo) Create(ctx context.Context, e *types.Environment) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	if _, ok := r.s.envs[e.ID]; ok {
		return conflict("environment", e.ID)
	}
	cp := *e
	cp.TenantID = tid
	r.s.envs[e.ID] = &cp
	if e.ShopID != "" {
		r.s.envByShop[e.ShopID] = e.ID
	}
	return nil
}

func (r envRepo) Get(ctx context.Context, id string) (*types.Environment, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	e, ok := r.s.envs[id]
	if !ok || e.TenantID != tid {
		return nil, notFound("environment", id)
	}
	cp := *e
	return &cp, nil
}

func (r envRepo) GetByShop(ctx context.Context, shopID string) (*types.Environment, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, ok := r.s.envByShop[shopID]
	if !ok {
		return nil, notFound("environment", shopID)
	}
	e, ok := r.s.envs[id]
	if !ok || e.TenantID != tid {
		return nil, notFound("environment", shopID)
	}
	cp := *e
	return &cp, nil
}

func (r envRepo) Update(ctx context.Context, e *types.Environment) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	old, ok := r.s.envs[e.ID]
	if !ok || old.TenantID != tid {
		return notFound("environment", e.ID)
	}
	cp := *e
	cp.TenantID = tid
	cp.UpdatedAt = time.Now()
	r.s.envs[e.ID] = &cp
	return nil
}

func (r envRepo) List(ctx context.Context, limit, offset int) ([]types.Environment, int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.Environment{}
	for _, e := range r.s.envs {
		if e.TenantID == tid {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	total := len(out)
	if offset > 0 && offset < len(out) {
		out = out[offset:]
	}
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, total, nil
}

// Search 带过滤的聚合列表（M2 店铺网格页）。与 pg 同口径：已软删店铺的环境不返回。
func (r envRepo) Search(ctx context.Context, f store.EnvironmentFilter) ([]types.Environment, int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, 0, err
	}
	allowed := map[string]bool{}
	for _, id := range f.ShopIDs {
		allowed[id] = true
	}
	kw := strings.ToLower(strings.TrimSpace(f.Keyword))
	out := []types.Environment{}
	for _, e := range r.s.envs {
		if e.TenantID != tid {
			continue
		}
		if len(allowed) > 0 && !allowed[e.ShopID] {
			continue
		}
		sh := r.s.shops[e.ShopID]
		if sh != nil && sh.DeletedAt != nil {
			continue
		}
		if f.GroupID != "" && (sh == nil || sh.GroupID != f.GroupID) {
			continue
		}
		if f.Platform != "" && (sh == nil || sh.Platform != f.Platform) {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		if f.Country != "" && e.Country != f.Country {
			continue
		}
		if f.ProxyID != "" && e.ProxyID != f.ProxyID {
			continue
		}
		if kw != "" && !envMatchesKeyword(kw, *e, sh) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	total := len(out)
	if f.Offset > 0 && f.Offset < len(out) {
		out = out[f.Offset:]
	} else if f.Offset >= len(out) {
		out = nil
	}
	if f.Limit > 0 && f.Limit < len(out) {
		out = out[:f.Limit]
	}
	return out, total, nil
}

// envMatchesKeyword 关键词命中：店铺名 / 自定义编号 / 环境 ID（均大小写不敏感）。
func envMatchesKeyword(kw string, e types.Environment, sh *types.Shop) bool {
	if strings.Contains(strings.ToLower(e.ID), kw) {
		return true
	}
	if sh == nil {
		return false
	}
	return strings.Contains(strings.ToLower(sh.Name), kw) ||
		strings.Contains(strings.ToLower(sh.Code), kw)
}

// SetStatusIfIn 只改状态的条件置位（与 pg 同语义：状态不在 from 内则一行不改）。
//
// 只动 status 与 updated_at：**不回写整行**。mem 里整行写回本来不会出错，
// 但语义必须与 pg 完全一致——否则「本地 mem 跑通、线上 pg 崩」这类事故
// 永远发现不了（阶段 8 那条整行 UPDATE 就是这么翻车的）。
func (r envRepo) SetStatusIfIn(ctx context.Context, id string, from []string, to string) (bool, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return false, err
	}
	if to == "" || len(from) == 0 {
		return false, apierr.Validation("状态置位缺少目标状态或前置状态集合")
	}
	e, ok := r.s.envs[id]
	if !ok || e.TenantID != tid {
		return false, notFound("environment", id)
	}
	for _, s := range from {
		if e.Status != s {
			continue
		}
		cp := *e
		cp.Status = to
		cp.UpdatedAt = time.Now()
		r.s.envs[id] = &cp
		return true, nil
	}
	return false, nil
}

// Delete 物理删除环境行（终态动作：出口归还与事件留痕由服务层负责）。
// 索引（envByShop）必须一并清掉，否则「该店铺已有环境」的判定会指向幽灵行。
func (r envRepo) Delete(ctx context.Context, id string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	e, ok := r.s.envs[id]
	if !ok || e.TenantID != tid {
		return notFound("environment", id)
	}
	delete(r.s.envs, id)
	if r.s.envByShop[e.ShopID] == id {
		delete(r.s.envByShop, e.ShopID)
	}
	return nil
}

// ---------- 访问策略 ----------

type policyRepo struct{ s *Store }

func (r policyRepo) Create(ctx context.Context, p *types.AccessPolicy) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	cp := *p
	cp.TenantID = tid
	cp.UpdatedAt = time.Now()
	r.s.policies[p.ID] = &cp
	return nil
}

func (r policyRepo) Update(ctx context.Context, p *types.AccessPolicy) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	old, ok := r.s.policies[p.ID]
	if !ok || old.TenantID != tid {
		return notFound("policy", p.ID)
	}
	cp := *p
	cp.TenantID = tid
	cp.UpdatedAt = time.Now()
	r.s.policies[p.ID] = &cp
	return nil
}

func (r policyRepo) Get(ctx context.Context, id string) (*types.AccessPolicy, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := r.s.policies[id]
	if !ok || p.TenantID != tid {
		return nil, notFound("policy", id)
	}
	cp := *p
	return &cp, nil
}

func (r policyRepo) List(ctx context.Context, scope, scopeID string) ([]types.AccessPolicy, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.AccessPolicy{}
	for _, p := range r.s.policies {
		if p.TenantID != tid {
			continue
		}
		if scope != "" && p.Scope != scope {
			continue
		}
		if scopeID != "" && p.ScopeID != scopeID {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (r policyRepo) Delete(ctx context.Context, id string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	p, ok := r.s.policies[id]
	if !ok || p.TenantID != tid {
		return notFound("policy", id)
	}
	delete(r.s.policies, id)
	return nil
}

// ---------- 安全事件 ----------

type secEventRepo struct{ s *Store }

func (r secEventRepo) Create(ctx context.Context, e *types.SecurityEvent) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	cp := *e
	cp.TenantID = tid
	r.s.secEvents = append(r.s.secEvents, &cp)
	return nil
}

func (r secEventRepo) List(ctx context.Context, f store.SecurityEventFilter) ([]types.SecurityEvent, int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.SecurityEvent{}
	for _, e := range r.s.secEvents {
		if e.TenantID != tid {
			continue
		}
		if f.Category != "" && e.Category != f.Category {
			continue
		}
		if f.Level != "" && e.Level != f.Level {
			continue
		}
		if f.ShopID != "" && e.ShopID != f.ShopID {
			continue
		}
		if f.From != nil && e.CreatedAt.Before(*f.From) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	total := len(out)
	if f.Offset > 0 && f.Offset < len(out) {
		out = out[f.Offset:]
	}
	if f.Limit > 0 && f.Limit < len(out) {
		out = out[:f.Limit]
	}
	return out, total, nil
}

// ---------- 凭证托管 ----------

type vaultRepo struct{ s *Store }

func (r vaultRepo) Upsert(ctx context.Context, c *types.ShopCredential) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	cp := *c
	cp.TenantID = tid
	cp.UpdatedAt = time.Now()
	key := strings.Join([]string{tid, cp.ShopID, string(cp.Kind)}, "|")
	r.s.vault[key] = &cp
	return nil
}

func (r vaultRepo) Get(ctx context.Context, shopID string, kind types.CredentialKind) (*types.ShopCredential, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, ok := r.s.vault[strings.Join([]string{tid, shopID, string(kind)}, "|")]
	if !ok {
		return nil, notFound("credential", shopID+"/"+string(kind))
	}
	cp := *c
	return &cp, nil
}

func (r vaultRepo) List(ctx context.Context, shopID string) ([]types.ShopCredential, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.ShopCredential{}
	for k, c := range r.s.vault {
		if !strings.HasPrefix(k, tid+"|"+shopID+"|") {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (r vaultRepo) Delete(ctx context.Context, id string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	for k, c := range r.s.vault {
		if c.ID == id && strings.HasPrefix(k, tid+"|") {
			delete(r.s.vault, k)
			return nil
		}
	}
	return notFound("credential", id)
}
