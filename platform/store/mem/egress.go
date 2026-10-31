package mem

import (
	"context"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

// egressRepo 出口绑定（内存）。
//
// 锁的说明同 grantRepo：并发由 WithTx 负责，仓储方法内不得再加 s.mu.Lock()，
// mem 的 WithTx 持有的是一把不可重入的 sync.Mutex，再锁一次当场自死锁。
type egressRepo struct{ s *Store }

// Bind 替换该环境的绑定；返回 false 表示资源已被别的环境独占。
//
// 冲突判定必须**排除该环境自身**：同一环境重复绑定同一资源（重启后 Attach 成功、
// 或幂等重发）是正常路径，不能因为「自己占着自己」而判成冲突。
func (r egressRepo) Bind(ctx context.Context, b *types.EgressBinding) (bool, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return false, err
	}
	for envID, cur := range r.s.egress {
		if envID == b.EnvID {
			continue
		}
		if cur.Provider != b.Provider {
			continue
		}
		// 两条互斥口径必须与 PG 的唯一索引一一对应（资源 ID / 出口地址），
		// 否则同一份代码在两种驱动下会给出不同结论。
		if cur.ResourceID == b.ResourceID {
			return false, nil
		}
		if cur.Host == b.Host && cur.Port == b.Port {
			return false, nil
		}
	}
	cp := *b
	cp.TenantID = tid
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now().UTC()
	}
	r.s.egress[b.EnvID] = &cp
	return true, nil
}

// Get 返回该环境的绑定（含已过期）；没有则 (nil,nil)。
func (r egressRepo) Get(ctx context.Context, envID string) (*types.EgressBinding, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	b, ok := r.s.egress[envID]
	if !ok || b.TenantID != tid {
		return nil, nil
	}
	cp := *b
	return &cp, nil
}

func (r egressRepo) ListByTenant(ctx context.Context) ([]types.EgressBinding, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	out := []types.EgressBinding{}
	for _, b := range r.s.egress {
		if b.TenantID == tid {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (r egressRepo) Release(ctx context.Context, envID string) error {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	if b, ok := r.s.egress[envID]; ok && b.TenantID == tid {
		delete(r.s.egress, envID)
	}
	return nil
}

// DeleteExpired 删除到期绑定并返回副本（切片/映射里存的是指针，直接返回会泄漏内部状态）。
func (r egressRepo) DeleteExpired(_ context.Context, tenantID string, now time.Time) ([]types.EgressBinding, error) {
	out := []types.EgressBinding{}
	for envID, b := range r.s.egress {
		if b.TenantID != tenantID || b.ExpiresAt.After(now) {
			continue
		}
		out = append(out, *b)
		delete(r.s.egress, envID)
	}
	return out, nil
}
