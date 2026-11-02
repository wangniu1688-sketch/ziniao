package mem

import (
	"context"
	"sort"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ---------- 代理机构委托审批（approval_flows） ----------

type delegationRepo struct{ s *Store }

func (r delegationRepo) tenant(ctx context.Context) (string, error) { return store.TenantFrom(ctx) }

func (r delegationRepo) Create(ctx context.Context, f *types.DelegationFlow) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	if _, ok := r.s.delegations[f.ID]; ok {
		return conflict("delegation", f.ID)
	}
	cp := *f
	cp.TenantID = tid
	r.s.delegations[f.ID] = &cp
	return nil
}

// Get 双主体可读：tenant_id 或 agency_tenant_id 命中上下文租户即可，
// 与 pg 侧 RLS 双主体策略（0007）口径一致。
func (r delegationRepo) Get(ctx context.Context, id string) (*types.DelegationFlow, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, err
	}
	f, ok := r.s.delegations[id]
	if !ok || (f.TenantID != tid && f.AgencyTenantID != tid) {
		return nil, notFound("delegation", id)
	}
	cp := *f
	return &cp, nil
}

func (r delegationRepo) Update(ctx context.Context, f *types.DelegationFlow) error {
	tid, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	old, ok := r.s.delegations[f.ID]
	if !ok || (old.TenantID != tid && old.AgencyTenantID != tid) {
		return notFound("delegation", f.ID)
	}
	cp := *f
	r.s.delegations[f.ID] = &cp
	return nil
}

func (r delegationRepo) List(ctx context.Context, fl store.DelegationFilter) ([]types.DelegationFlow, int, error) {
	tid, err := r.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []types.DelegationFlow{}
	for _, f := range r.s.delegations {
		if fl.Party == "agency" {
			if f.AgencyTenantID != tid {
				continue
			}
		} else if f.TenantID != tid {
			continue
		}
		if fl.Status != "" && f.Status != fl.Status {
			continue
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	total := len(out)
	limit, offset := fl.Limit, fl.Offset
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

func (r delegationRepo) HasPendingBetween(_ context.Context, grantorTenantID, agencyTenantID string) (bool, error) {
	for _, f := range r.s.delegations {
		if f.TenantID == grantorTenantID && f.AgencyTenantID == agencyTenantID &&
			f.Status == types.DelegationPending {
			return true, nil
		}
	}
	return false, nil
}

var _ store.DelegationRepo = delegationRepo{}
