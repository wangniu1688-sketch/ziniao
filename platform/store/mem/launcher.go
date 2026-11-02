package mem

import (
	"context"
	"sort"
	"strings"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

// M1 启动器内核（0012）：手工静态代理台账与店铺分组的内存实现。
//
// 与 pg 驱动同口径是硬要求（ADR-0007）：分页、排序、模糊匹配、租户过滤、
// 错误码（404/409）都必须一致，否则「本地能搜到、线上搜不到」这类缺陷
// 只会在生产里相遇。
//
// 锁的说明见 mem.go 的 grantRepo.ListByUser：并发由 WithTx 负责，仓储层不得加锁
// （WithTx 持有的是同一把不可重入的锁，再锁一次当场自死锁）。

// ---------- 手工静态代理 ----------

type proxyRepo struct{ s *Store }

func (r proxyRepo) Create(ctx context.Context, p *types.Proxy) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	p.TenantID = tid
	for _, it := range r.s.proxies {
		if it.TenantID == tid && it.Name == p.Name {
			return conflict("proxy", p.Name)
		}
	}
	cp := *p
	// 切片字段（无）之外没有引用类型；Tags 类字段不存在，浅拷贝即可。
	r.s.proxies[p.ID] = &cp
	return nil
}

func (r proxyRepo) Get(ctx context.Context, id string) (*types.Proxy, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := r.s.proxies[id]
	if !ok || p.TenantID != tid {
		return nil, notFound("proxy", id)
	}
	cp := *p
	cp.HasPassword = p.PasswordEnc != ""
	return &cp, nil
}

func (r proxyRepo) List(ctx context.Context, f store.ProxyFilter) ([]*types.Proxy, int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, 0, err
	}
	kw := strings.ToLower(strings.TrimSpace(f.Keyword))
	out := []*types.Proxy{}
	for _, p := range r.s.proxies {
		if p.TenantID != tid {
			continue
		}
		if f.Status != "" && p.Status != f.Status {
			continue
		}
		if f.CheckStatus != "" && p.CheckStatus != f.CheckStatus {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(p.Name), kw) &&
			!strings.Contains(strings.ToLower(p.Host), kw) &&
			!strings.Contains(strings.ToLower(p.Note), kw) {
			continue
		}
		cp := *p
		cp.HasPassword = p.PasswordEnc != ""
		out = append(out, &cp)
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

func (r proxyRepo) Update(ctx context.Context, p *types.Proxy) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	cur, ok := r.s.proxies[p.ID]
	if !ok || cur.TenantID != tid {
		return notFound("proxy", p.ID)
	}
	for _, it := range r.s.proxies {
		if it.TenantID == tid && it.Name == p.Name && it.ID != p.ID {
			return conflict("proxy", p.Name)
		}
	}
	cp := *p
	cp.TenantID = tid
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now()
	}
	r.s.proxies[p.ID] = &cp
	return nil
}

func (r proxyRepo) Delete(ctx context.Context, id string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	p, ok := r.s.proxies[id]
	if !ok || p.TenantID != tid {
		return notFound("proxy", id)
	}
	if n, _ := r.CountBound(ctx, id); n > 0 {
		return conflict("proxy", id)
	}
	delete(r.s.proxies, id)
	return nil
}

// CountBound 该代理被本租户多少个环境引用（删除前置校验的唯一判据，实时计数）。
func (r proxyRepo) CountBound(ctx context.Context, id string) (int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range r.s.envs {
		if e.TenantID == tid && e.ProxyID == id {
			n++
		}
	}
	return n, nil
}

// CountBoundRunning 该代理被多少个**运行中**环境引用（P3 一号一 IP 告警）。
func (r proxyRepo) CountBoundRunning(ctx context.Context, id string) (int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range r.s.envs {
		// "running" 用字面量而不是引 shop 包的常量：store 的两个驱动都不该
		// 反向依赖业务层，而环境状态枚举目前只定义在 shop。
		if e.TenantID == tid && e.ProxyID == id && e.Status == "running" {
			n++
		}
	}
	return n, nil
}

// SaveCheckResult 只改拨测字段（P3）。
//
// 与 pg 同口径：只动这 6 个字段，绝不整行覆盖——整行覆盖会把调用方手里那份
// 空 PasswordEnc 写回内存库，表现是「改一次备注，代理口令就没了」。
func (r proxyRepo) SaveCheckResult(ctx context.Context, id string, c store.ProxyCheckResult) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	p, ok := r.s.proxies[id]
	if !ok || p.TenantID != tid {
		return notFound("proxy", id)
	}
	status := c.CheckStatus
	if !types.ValidProxyCheckStatus(status) {
		status = types.ProxyCheckUnknown
	}
	checkedAt := c.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	lat := c.LatencyMS
	p.CheckStatus = status
	p.ExitIP = c.ExitIP
	p.ExitCountry = c.ExitCountry
	p.LatencyMS = &lat
	p.LastCheckedAt = &checkedAt
	p.LastError = c.LastError
	p.UpdatedAt = time.Now().UTC()
	return nil
}

// PasswordEnc 取口令密文（唯一读密文的入口，见接口注释）。
func (r proxyRepo) PasswordEnc(ctx context.Context, id string) (string, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return "", err
	}
	p, ok := r.s.proxies[id]
	if !ok || p.TenantID != tid {
		return "", notFound("proxy", id)
	}
	return p.PasswordEnc, nil
}

// ---------- 店铺分组 ----------

type groupRepo struct{ s *Store }

func (r groupRepo) Create(ctx context.Context, g *types.Group) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	g.TenantID = tid
	for _, it := range r.s.groups {
		if it.TenantID == tid && it.Name == g.Name {
			return conflict("group", g.Name)
		}
	}
	cp := *g
	r.s.groups[g.ID] = &cp
	return nil
}

func (r groupRepo) Get(ctx context.Context, id string) (*types.Group, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	g, ok := r.s.groups[id]
	if !ok || g.TenantID != tid {
		return nil, notFound("group", id)
	}
	cp := *g
	cp.EnvironmentCount = r.memberCount(tid, id)
	return &cp, nil
}

// List 带 environment_count（一次遍历算完，避免 N+1）。
func (r groupRepo) List(ctx context.Context) ([]*types.Group, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	out := []*types.Group{}
	for _, g := range r.s.groups {
		if g.TenantID != tid {
			continue
		}
		cp := *g
		cp.EnvironmentCount = r.memberCount(tid, g.ID)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r groupRepo) Update(ctx context.Context, g *types.Group) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	cur, ok := r.s.groups[g.ID]
	if !ok || cur.TenantID != tid {
		return notFound("group", g.ID)
	}
	for _, it := range r.s.groups {
		if it.TenantID == tid && it.Name == g.Name && it.ID != g.ID {
			return conflict("group", g.Name)
		}
	}
	cp := *g
	cp.TenantID = tid
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now()
	}
	r.s.groups[g.ID] = &cp
	return nil
}

func (r groupRepo) Delete(ctx context.Context, id string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	g, ok := r.s.groups[id]
	if !ok || g.TenantID != tid {
		return notFound("group", id)
	}
	if r.memberCount(tid, id) > 0 {
		return conflict("group", id)
	}
	delete(r.s.groups, id)
	return nil
}

// CountMembers 「空分组才能删除」的唯一判据：该分组下未软删的店铺数（一店铺一环境）。
func (r groupRepo) CountMembers(ctx context.Context, id string) (int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return 0, err
	}
	return r.memberCount(tid, id), nil
}

func (r groupRepo) memberCount(tenantID, groupID string) int {
	n := 0
	for _, s := range r.s.shops {
		if s.TenantID == tenantID && s.GroupID == groupID && s.DeletedAt == nil {
			n++
		}
	}
	return n
}
