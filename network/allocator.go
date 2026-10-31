package network

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoExclusiveResource 代理商始终返回已被其它环境占用的资源。
// 宁可启动失败也不能共用出口——共用 IP 等于把两个店铺账号在同一证据链上串起来，
// 这在多账号防关联里是最致命的错误，必须显式报错而不是降级。
var ErrNoExclusiveResource = errors.New("network: 代理商未能提供独占出口资源（多次申请均与他人重复）")

// Allocator 出口 IP 资源分配器。
//
// 它守着本系统最硬的一条不变量：**同一个出口资源同一时刻只能绑定一个环境**。
// 两个环境共用出口 IP，等于把两个店铺账号在平台的证据链上直接串起来——
// 画像再像、指纹再散，只要共 IP 就前功尽弃。
type Allocator struct {
	mu       sync.Mutex
	provider Provider
	lease    time.Duration // 厂商未返回到期时间时的兜底租约

	resources map[string]*AcquiredProxy // resourceID -> 资源正文
	byEnv     map[string]string         // envID -> resourceID（保证粘性）
	owner     map[string]string         // resourceID -> envID（互斥校验的真相源）
	tries     int                       // 单次分配最多向厂商申请几次
	// prevAddr envID -> 上次实际用过的出口。用于识别「这个环境的出口 IP 变了」。
	// 出口变更本身是极强的关联信号（同一账号一天内从不同国家/IP 登录），
	// 哪怕我们做了 correct 的隔离也必须让运营看见它，而不是静默发生。
	prevAddr map[string]string
}

// AllocResult 分配结果与出口连续性信息。
type AllocResult struct {
	Proxy    *ProxyConfig
	Rotated  bool   // 本次出口与上次不同（非首次分配才有意义）
	PrevAddr string // 上次出口，空串表示首次分配
}

// NewAllocator 构造分配器。lease<=0 时用 24h 兜底。
func NewAllocator(p Provider, lease time.Duration) *Allocator {
	if lease <= 0 {
		lease = 24 * time.Hour
	}
	return &Allocator{
		provider:  p,
		lease:     lease,
		resources: map[string]*AcquiredProxy{},
		byEnv:     map[string]string{},
		owner:     map[string]string{},
		prevAddr:  map[string]string{},
		tries:     5,
	}
}

// AllocateEx 分配并返回出口连续性信息。
//
// 为什么要区分 Rotated：资源过期被回收、厂商重新指派都会让环境换出口，
// 而对同一账号来说「出口 IP 变了」是平台眼里很强的风险信号，
// 必须让运营看到，而不是悄悄发生。
func (a *Allocator) AllocateEx(ctx context.Context, envID string, r Region) (*AllocResult, error) {
	res, err := a.Allocate(ctx, envID, r)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	addr := res.Addr()
	prev := a.prevAddr[envID]
	rotated := prev != "" && prev != addr
	a.prevAddr[envID] = addr
	a.mu.Unlock()
	return &AllocResult{Proxy: res, Rotated: rotated, PrevAddr: prev}, nil
}

// Provider 当前使用的代理商（排障/展示用）。
func (a *Allocator) Provider() Provider { return a.provider }

// Allocate 为环境分配独占出口资源，并保证幂等：同一环境重复调用返回同一个资源。
//
// 为什么必须幂等：客户端可能重连/重发 launch，若每次都换 IP，
// 平台会看到「同一个账号今天在美国明天在德国」——这也是一种关联信号。
// 反过来，不同环境必须拿到不同资源，哪怕厂商连续返回同一个 IP。
func (a *Allocator) Allocate(ctx context.Context, envID string, r Region) (*ProxyConfig, error) {
	if envID == "" {
		return nil, errors.New("network: 分配出口资源必须带 envID")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if id, ok := a.byEnv[envID]; ok {
		if res, ok := a.resources[id]; ok && !expired(res, time.Now()) {
			out := res.Proxy
			return &out, nil
		}
		// 已过期：解绑后重新申请
		a.unbindLocked(envID)
	}

	var last *AcquiredProxy
	for i := 0; i < a.tries; i++ {
		res, err := a.provider.Acquire(ctx, r)
		if err != nil {
			return nil, err
		}
		if res == nil || res.Proxy.Host == "" || res.Proxy.Port == 0 {
			return nil, fmt.Errorf("network: 代理商 %s 返回了不完整的资源", safeName(a.provider))
		}
		if _, taken := a.owner[res.ID]; taken {
			// 与其它环境撞车：归还这家，换下一个
			last = res
			_ = a.provider.Release(ctx, res.ID)
			continue
		}
		if res.ExpiresAt.IsZero() {
			res.ExpiresAt = time.Now().Add(a.lease)
		}
		a.resources[res.ID] = res
		a.owner[res.ID] = envID
		a.byEnv[envID] = res.ID
		out := res.Proxy
		return &out, nil
	}
	if last != nil {
		return nil, fmt.Errorf("%w（候选冲突 %d 次，最后候选 %s）", ErrNoExclusiveResource, a.tries, last.ID)
	}
	return nil, ErrNoExclusiveResource
}

// Release 解绑并归还给代理商。允许重复调用（第二次是 no-op）。
func (a *Allocator) Release(ctx context.Context, envID string) error {
	a.mu.Lock()
	id, ok := a.byEnv[envID]
	a.unbindLocked(envID)
	a.mu.Unlock()
	if !ok {
		return nil
	}
	// 释放即视为该环境生命周期结束，出口历史不再延续
	a.mu.Lock()
	delete(a.prevAddr, envID)
	a.mu.Unlock()
	return a.provider.Release(ctx, id)
}

// Reap 巡检：回收过期资源并返回回收数量。由后台任务周期性调用。
func (a *Allocator) Reap(ctx context.Context) int {
	a.mu.Lock()
	now := time.Now()
	stale := []string{}
	for id, res := range a.resources {
		if expired(res, now) {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		envID := a.owner[id]
		if envID != "" {
			delete(a.byEnv, envID)
		}
		delete(a.owner, id)
		delete(a.resources, id)
	}
	a.mu.Unlock()
	for _, id := range stale {
		_ = a.provider.Release(ctx, id)
	}
	return len(stale)
}

// Stats 当前占用情况（不含口令：口令不进日志/响应是本项目的 L4 红线）。
type Stats struct {
	Bound   int `json:"bound"`
	Total   int `json:"total"`
	Expired int `json:"expired"`
}

// Stats 返回资源占用统计。
func (a *Allocator) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	exp := 0
	for _, res := range a.resources {
		if expired(res, now) {
			exp++
		}
	}
	return Stats{Bound: len(a.byEnv), Total: len(a.resources), Expired: exp}
}

func (a *Allocator) unbindLocked(envID string) {
	if id, ok := a.byEnv[envID]; ok {
		delete(a.owner, id)
		delete(a.resources, id)
		delete(a.byEnv, envID)
	}
}

func expired(res *AcquiredProxy, now time.Time) bool {
	return !res.ExpiresAt.IsZero() && res.ExpiresAt.Before(now)
}

func safeName(p Provider) string {
	if p == nil {
		return "(nil)"
	}
	return p.Name()
}
