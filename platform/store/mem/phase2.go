package mem

import (
	"context"
	"sort"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ---------- 审批工单 ----------

type approvalRepo struct{ s *Store }

func (r approvalRepo) tenant(ctx context.Context) (string, error) { return store.TenantFrom(ctx) }

func (r approvalRepo) Create(ctx context.Context, t *types.ApprovalTicket) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	if _, ok := r.s.approvals[t.ID]; ok {
		return conflict("approval", t.ID)
	}
	cp := *t
	cp.TenantID = tid
	r.s.approvals[t.ID] = &cp
	return nil
}

func (r approvalRepo) Get(ctx context.Context, id string) (*types.ApprovalTicket, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	t, ok := r.s.approvals[id]
	if !ok || t.TenantID != tid {
		return nil, notFound("approval", id)
	}
	cp := *t
	return &cp, nil
}

func (r approvalRepo) Update(ctx context.Context, t *types.ApprovalTicket) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	old, ok := r.s.approvals[t.ID]
	if !ok || old.TenantID != tid {
		return notFound("approval", t.ID)
	}
	cp := *t
	cp.TenantID = tid
	r.s.approvals[t.ID] = &cp
	return nil
}

func (r approvalRepo) List(ctx context.Context, f store.ApprovalFilter) ([]types.ApprovalTicket, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.ApprovalTicket{}
	for _, t := range r.s.approvals {
		if t.TenantID != tid {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		if f.Type != "" && t.Type != f.Type {
			continue
		}
		if f.RequesterID != "" && t.RequesterID != f.RequesterID {
			continue
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	total := len(out)
	limit, offset := f.Limit, f.Offset
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return out[offset:end], total, nil
}

func (r approvalRepo) ExpireBefore(ctx context.Context, at time.Time) (int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range r.s.approvals {
		if t.TenantID != tid || t.Status != types.ApprovalPending {
			continue
		}
		if t.ExpiresAt.Before(at) || t.ExpiresAt.Equal(at) {
			cp := *t
			cp.Status = types.ApprovalExpired
			r.s.approvals[t.ID] = &cp
			n++
		}
	}
	return n, nil
}

var _ store.ApprovalRepo = approvalRepo{}

// ---------- 审批路由规则（租户可配） ----------

type routeRuleRepo struct{ s *Store }

func (r routeRuleRepo) Create(ctx context.Context, rule *types.ApprovalRouteRule) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	if _, ok := r.s.routeRules[rule.ID]; ok {
		return conflict("approval_route_rule", rule.ID)
	}
	cp := *rule
	cp.TenantID = tid
	cp.Roles = append([]string{}, rule.Roles...)
	r.s.routeRules[rule.ID] = &cp
	return nil
}

func (r routeRuleRepo) Get(ctx context.Context, id string) (*types.ApprovalRouteRule, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	rule, ok := r.s.routeRules[id]
	if !ok || rule.TenantID != tid {
		return nil, notFound("approval_route_rule", id)
	}
	cp := *rule
	return &cp, nil
}

func (r routeRuleRepo) Update(ctx context.Context, rule *types.ApprovalRouteRule) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	old, ok := r.s.routeRules[rule.ID]
	if !ok || old.TenantID != tid {
		return notFound("approval_route_rule", rule.ID)
	}
	cp := *rule
	cp.TenantID = tid
	cp.Roles = append([]string{}, rule.Roles...)
	r.s.routeRules[rule.ID] = &cp
	return nil
}

// ListByTenant 按优先级升序返回（含停用行——停用与否由服务层决定，
// 与 DataGrant 同一分工：仓储给全数据，时间/状态判定在上层）。
func (r routeRuleRepo) ListByTenant(ctx context.Context, tenantID string) ([]types.ApprovalRouteRule, error) {
	out := []types.ApprovalRouteRule{}
	for _, r := range r.s.routeRules {
		if r.TenantID != tenantID {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r routeRuleRepo) Delete(ctx context.Context, id string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	old, ok := r.s.routeRules[id]
	if !ok || old.TenantID != tid {
		return notFound("approval_route_rule", id)
	}
	delete(r.s.routeRules, id)
	return nil
}

var _ store.ApprovalRouteRuleRepo = routeRuleRepo{}
