package mem

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"ziniao/platform/ids"
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

// ---------- 出口绑定历史（只追加） ----------

// AppendEvent 追加一条生命周期事件。
//
// 幂等按 id，与 PG 侧的 ON CONFLICT DO NOTHING 同口径：重复事件会让
// 「这个 ASN 被几个环境用过」的统计虚高，而这正是这条链路的唯一产出。
func (r egressRepo) AppendEvent(ctx context.Context, ev *types.EgressBindingEvent) error {
	if ev == nil {
		return errors.New("mem: 出口历史事件不能为空")
	}
	if !types.ValidEgressAction(ev.Action) {
		return fmt.Errorf("mem: 未知的出口历史动作 %q", ev.Action)
	}
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return err
	}
	for _, old := range r.s.egressHist {
		if ev.ID != "" && old.ID == ev.ID {
			return nil // 重投：同一条事件只留一份
		}
	}
	cp := *ev
	cp.TenantID = tid
	if cp.ID == "" {
		cp.ID = ids.New()
	}
	if cp.OccurredAt.IsZero() {
		cp.OccurredAt = time.Now().UTC()
	}
	ev.ID, ev.OccurredAt = cp.ID, cp.OccurredAt
	r.s.egressHist = append(r.s.egressHist, &cp)
	return nil
}

// ListEvents 倒序查历史事件（时间相同按 id 倒序，保证分页稳定）。
func (r egressRepo) ListEvents(ctx context.Context, f store.EgressHistoryFilter) ([]types.EgressBindingEvent, int, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, 0, err
	}
	matched := []types.EgressBindingEvent{}
	for _, ev := range r.s.egressHist {
		if ev.TenantID != tid {
			continue
		}
		if f.EnvID != "" && ev.EnvID != f.EnvID {
			continue
		}
		if f.Provider != "" && ev.Provider != f.Provider {
			continue
		}
		if f.ASN != "" && ev.ASN != f.ASN {
			continue
		}
		if f.Host != "" && ev.Host != f.Host {
			continue
		}
		if f.Action != "" && ev.Action != f.Action {
			continue
		}
		if f.From != nil && ev.OccurredAt.Before(*f.From) {
			continue
		}
		if f.To != nil && ev.OccurredAt.After(*f.To) {
			continue
		}
		matched = append(matched, *ev)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].OccurredAt.Equal(matched[j].OccurredAt) {
			return matched[i].OccurredAt.After(matched[j].OccurredAt)
		}
		return matched[i].ID > matched[j].ID
	})
	total := len(matched)
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []types.EgressBindingEvent{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

// ASNSharing 找出被至少 minEnvs 个不同环境使用过的 ASN（口径与 PG 侧一致）。
func (r egressRepo) ASNSharing(ctx context.Context, since time.Time, minEnvs int) ([]types.EgressASNShare, error) {
	tid, err := store.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if minEnvs < 2 {
		minEnvs = 2
	}
	type agg struct {
		share *types.EgressASNShare
		envs  map[string]bool
		provi map[string]bool
		exits map[string]bool
	}
	byASN := map[string]*agg{}
	for _, ev := range r.s.egressHist {
		if ev.TenantID != tid || ev.ASN == "" || ev.OccurredAt.Before(since) {
			continue
		}
		a, ok := byASN[ev.ASN]
		if !ok {
			a = &agg{
				share: &types.EgressASNShare{ASN: ev.ASN, ASNType: ev.ASNType,
					FirstSeen: ev.OccurredAt, LastSeen: ev.OccurredAt},
				envs: map[string]bool{}, provi: map[string]bool{}, exits: map[string]bool{},
			}
			byASN[ev.ASN] = a
		}
		// 同一环境的 ASNType 取最后一条（代理商偶尔改字段，旧值不代表现状）
		if ev.ASNType != "" && ev.OccurredAt.After(a.share.LastSeen) {
			a.share.ASNType = ev.ASNType
		}
		a.envs[ev.EnvID] = true
		a.provi[ev.Provider] = true
		if ev.ExitAddr != "" {
			a.exits[ev.ExitAddr] = true
		}
		if ev.OccurredAt.Before(a.share.FirstSeen) {
			a.share.FirstSeen = ev.OccurredAt
		}
		if ev.OccurredAt.After(a.share.LastSeen) {
			a.share.LastSeen = ev.OccurredAt
		}
		a.share.Events++
	}
	out := []types.EgressASNShare{}
	for _, a := range byASN {
		if len(a.envs) < minEnvs {
			continue
		}
		a.share.EnvIDs = keysSorted(a.envs)
		a.share.Providers = keysSorted(a.provi)
		a.share.Exits = keysSorted(a.exits)
		out = append(out, *a.share)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].EnvIDs) != len(out[j].EnvIDs) {
			return len(out[i].EnvIDs) > len(out[j].EnvIDs)
		}
		return out[i].ASN < out[j].ASN
	})
	if len(out) > 100 {
		out = out[:100]
	}
	return out, nil
}

// PruneEvents 按保留期裁剪历史（物理删除 occurred_at < before 的行）。
//
// 只删本租户、且严格早于 before 的行：裁剪是运营决策，宁少勿多——
// 多删一条「正好在边界上」的事件，共用 ASN 的证据链就断一截。
func (r egressRepo) PruneEvents(_ context.Context, tenantID string, before time.Time) (int64, error) {
	kept := r.s.egressHist[:0]
	var pruned int64
	for _, ev := range r.s.egressHist {
		if ev.TenantID == tenantID && ev.OccurredAt.Before(before) {
			pruned++
			continue
		}
		kept = append(kept, ev)
	}
	r.s.egressHist = kept
	return pruned, nil
}

func keysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
