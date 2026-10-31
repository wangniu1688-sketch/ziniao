package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

// ErrNoExclusiveResource 代理商始终返回已被其它环境占用的资源。
// 宁可启动失败也不能共用出口——共用 IP 等于把两个店铺账号在同一证据链上串起来，
// 这在多账号防关联里是最致命的错误，必须显式报错而不是降级。
var ErrNoExclusiveResource = errors.New("network: 代理商未能提供独占出口资源（多次申请均与他人重复）")

// ErrBindingUnavailable 绑定无法落库（缺租户上下文 / 存储故障）。
//
// 这个错误的语义是「宁可这次起不来，也不能发一个重启后必然换 IP 的出口」：
// 没有持久化，每次发布都会让全部店铺换一次登录 IP，那比一次启动失败贵得多。
var ErrBindingUnavailable = errors.New("network: 出口绑定无法落库")

// BindingStore 出口绑定的持久化端口。
//
// 刻意只声明分配器真正用到的四个方法（不直接用 store.EgressBindingRepo）：
// 分配器不需要分页、列表、审计，一个窄接口能让单测用几十行假实现跑起来。
type BindingStore interface {
	// Bind 落库绑定；返回 false 表示该资源已被别的环境独占。
	Bind(ctx context.Context, b *types.EgressBinding) (bool, error)
	Get(ctx context.Context, envID string) (*types.EgressBinding, error)
	Release(ctx context.Context, envID string) error
	DeleteExpired(ctx context.Context, tenantID string, now time.Time) ([]types.EgressBinding, error)
}

// Attacher 代理商的可选能力：按资源 ID 取回既存出口的凭据。
//
// 为什么必须由代理商提供：出口凭据按 L4 红线不落库，重启后唯一合法的来源
// 就是厂商自己（GET /proxies/{id} 这类接口，多数住宅代理商都有）。
// 厂商不支持时分配器只能重新申请，出口会变——那就必须触发轮换告警，而不是静默换。
type Attacher interface {
	Attach(ctx context.Context, id string) (*AcquiredProxy, error)
}

// Allocator 出口 IP 资源分配器。
//
// 它守着本系统最硬的一条不变量：**同一个出口资源同一时刻只能绑定一个环境**。
// 两个环境共用出口 IP，等于把两个店铺账号在平台的证据链上直接串起来——
// 画像再像、指纹再散，只要共 IP 就前功尽弃。
//
// 进程内靠 owner map 保证；跨进程（滚动发布、多副本）靠数据库唯一索引
// egress_bindings(provider,resource_id) 保证。
type Allocator struct {
	mu       sync.Mutex
	provider Provider
	lease    time.Duration // 厂商未返回到期时间时的兜底租约
	binds    BindingStore  // 可空：未落库/单测时为纯内存模式
	log      *slog.Logger

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

// SetBindingStore 接入持久化。必须在开始分配之前调用（构造后、服务启动前）。
//
// 不接也能跑（纯内存），但那等于「重启即换出口」——docs/TODO-phase2 里把
// 落库列为上真账号的硬前置，就是这条。
func (a *Allocator) SetBindingStore(s BindingStore) { a.binds = s }

// SetLogger 注入日志（可选，默认 slog.Default）。
func (a *Allocator) SetLogger(l *slog.Logger) { a.log = l }

// AllocateEx 分配并返回出口连续性信息。
//
// 为什么要区分 Rotated：资源过期被回收、厂商重新指派都会让环境换出口，
// 而对同一账号来说「出口 IP 变了」是平台眼里很强的风险信号，
// 必须让运营看到，而不是悄悄发生。
//
// ctx 必须带租户（store.WithTenant）：绑定是按租户落库的，缺租户时
// 分配会直接失败而不是降级成内存绑定。
func (a *Allocator) AllocateEx(ctx context.Context, envID string, r Region) (*AllocResult, error) {
	res, err := a.acquire(ctx, envID, r)
	if err != nil {
		return nil, err
	}
	addr := res.Proxy.Addr()
	a.mu.Lock()
	prev := a.prevAddr[envID]
	rotated := prev != "" && prev != addr
	a.prevAddr[envID] = addr
	a.mu.Unlock()
	return &AllocResult{Proxy: &res.Proxy, Rotated: rotated, PrevAddr: prev}, nil
}

// Provider 当前使用的代理商（排障/展示用）。
func (a *Allocator) Provider() Provider { return a.provider }

// Allocate 为环境分配独占出口资源，并保证幂等：同一环境重复调用返回同一个资源。
//
// 为什么必须幂等：客户端可能重连/重发 launch，若每次都换 IP，
// 平台会看到「同一个账号今天在美国明天在德国」——这也是一种关联信号。
// 反过来，不同环境必须拿到不同资源，哪怕厂商连续返回同一个 IP。
func (a *Allocator) Allocate(ctx context.Context, envID string, r Region) (*ProxyConfig, error) {
	res, err := a.acquire(ctx, envID, r)
	if err != nil {
		return nil, err
	}
	out := res.Proxy
	return &out, nil
}

// acquire 分配主体。
//
// 注意：整个分配过程（含向代理商申请、读写绑定库）都在同一把锁内完成。
// 这是刻意的——并发申请正是「两个环境抢到同一个资源」的温床，而代理商调用
// 本来就有 15s 超时，把锁拆细换来的是更难验证的竞态。
func (a *Allocator) acquire(ctx context.Context, envID string, r Region) (*AcquiredProxy, error) {
	if envID == "" {
		return nil, errors.New("network: 分配出口资源必须带 envID")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if id, ok := a.byEnv[envID]; ok {
		if res, ok := a.resources[id]; ok && !expired(res, time.Now()) {
			return res, nil
		}
		// 已过期：解绑后重新申请
		a.unbindLocked(envID)
	}

	// 进程重启后内存是空的：先看库里这个环境原先绑的是哪条资源。
	// 没有这一步，每次重启都会换出口——这正是本次落库要解决的唯一问题。
	if a.binds != nil {
		res, err := a.restoreLocked(ctx, envID)
		if err != nil {
			return nil, err
		}
		if res != nil {
			return res, nil
		}
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
		if res.ExpiresAt.IsZero() {
			res.ExpiresAt = time.Now().Add(a.lease)
		}
		if a.takenLocked(res) {
			// 与其它环境撞车：归还这家，换下一个
			last = res
			_ = a.provider.Release(ctx, res.ID)
			continue
		}
		if a.binds != nil {
			ok, perr := a.persistLocked(ctx, envID, r, res)
			if perr != nil {
				_ = a.provider.Release(ctx, res.ID)
				return nil, perr
			}
			if !ok {
				// 库里已被别的环境（或滚动发布时另一个实例）抢走：归还后换下一个
				last = res
				_ = a.provider.Release(ctx, res.ID)
				continue
			}
		}
		a.rememberLocked(envID, res)
		return res, nil
	}
	if last != nil {
		return nil, fmt.Errorf("%w（候选冲突 %d 次，最后候选 %s）", ErrNoExclusiveResource, a.tries, last.ID)
	}
	return nil, ErrNoExclusiveResource
}

// restoreLocked 重启后按库恢复绑定：命中则向代理商取回凭据，出口保持不变。
// 返回 (nil,nil) 表示「没有可复用的绑定」，调用方继续走重新申请。
func (a *Allocator) restoreLocked(ctx context.Context, envID string) (*AcquiredProxy, error) {
	tenantID, terr := store.TenantFrom(ctx)
	if terr != nil || tenantID == "" {
		// 没有租户上下文就读不了绑定库。继续分配等于「发一个重启必换 IP 的出口」，
		// 而这正是要消灭的行为，所以宁可拒绝，也不再静默降级。
		return nil, fmt.Errorf("%w: 缺少租户上下文（%v）", ErrBindingUnavailable, terr)
	}
	b, err := a.binds.Get(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取绑定失败: %v", ErrBindingUnavailable, err)
	}
	if b == nil {
		return nil, nil
	}
	if !b.ExpiresAt.IsZero() && b.ExpiresAt.Before(time.Now()) {
		a.dropBindingLocked(ctx, b, "绑定已到期")
		return nil, nil
	}
	att, ok := a.provider.(Attacher)
	if !ok {
		a.dropBindingLocked(ctx, b, "代理商未配置按 ID 取回凭据的接口")
		return nil, nil
	}
	res, aerr := att.Attach(ctx, b.ResourceID)
	if aerr != nil || res == nil || res.Proxy.Host == "" || res.Proxy.Port == 0 {
		a.warn("按 ID 取回出口凭据失败，将重新申请（出口若变化会触发轮换告警）",
			"env", envID, "resource", b.ResourceID, "err", aerr)
		a.dropBindingLocked(ctx, b, "取回凭据失败")
		return nil, nil
	}
	// 厂商可能少返字段：用库里的事实补齐，而不是下发一份缺字段的启动单。
	if res.ExpiresAt.IsZero() {
		res.ExpiresAt = b.ExpiresAt
	}
	if res.Proxy.Protocol == "" {
		res.Proxy.Protocol = Protocol(b.Protocol)
	}
	if res.Proxy.Country == "" {
		res.Proxy.Country = b.Country
	}
	if res.Proxy.City == "" {
		res.Proxy.City = b.City
	}
	if res.Proxy.ASNType == "" {
		res.Proxy.ASNType = ASNType(b.ASNType)
	}
	// 资源 ID 以库为准：后续归还、冲突判定都用它，不能让厂商换个字段就脱钩。
	res.ID = b.ResourceID
	res.Proxy.ID = b.ResourceID
	a.rememberLocked(envID, res)
	if b.LastExit != "" && a.prevAddr[envID] == "" {
		// 上次出口随库一起回来：否则重启后的 IP 跳变没有对照值，告警发不出来。
		a.prevAddr[envID] = b.LastExit
	}
	return res, nil
}

// dropBindingLocked 作废一条无法复用的绑定：解绑 + 归还厂商资源，然后由调用方重新申请。
func (a *Allocator) dropBindingLocked(ctx context.Context, b *types.EgressBinding, reason string) {
	if err := a.binds.Release(ctx, b.EnvID); err != nil {
		a.warn("解除出口绑定失败（下次启动会重新判定）", "env", b.EnvID, "err", err)
	}
	if err := a.provider.Release(ctx, b.ResourceID); err != nil {
		a.warn("归还代理商资源失败（可能已自然过期）", "resource", b.ResourceID, "err", err)
	}
	// 保留上次出口：接着要重新申请，出口若变了必须让告警说得清「从哪变到哪」。
	if b.LastExit != "" {
		a.prevAddr[b.EnvID] = b.LastExit
	}
	a.warn("既存出口绑定作废，重新申请出口", "env", b.EnvID, "resource", b.ResourceID, "reason", reason)
}

// persistLocked 落库绑定；返回 false 表示资源已被别人独占。
func (a *Allocator) persistLocked(ctx context.Context, envID string, r Region, res *AcquiredProxy) (bool, error) {
	now := time.Now().UTC()
	b := &types.EgressBinding{
		EnvID:      envID,
		Provider:   a.provider.Name(),
		ResourceID: res.ID,
		Protocol:   string(res.Proxy.Protocol),
		Host:       res.Proxy.Host,
		Port:       res.Proxy.Port,
		Country:    res.Proxy.Country,
		Region:     res.Proxy.Region,
		City:       res.Proxy.City,
		ASN:        res.Proxy.ASN,
		ASNType:    string(res.Proxy.ASNType),
		RegionCode: r.Code,
		// 记的是「这条绑定实际在用的出口」：重启后拿它当对照值判断是否换过 IP。
		LastExit:  res.Proxy.Addr(),
		BoundAt:   now,
		ExpiresAt: res.ExpiresAt.UTC(),
		UpdatedAt: now,
	}
	ok, err := a.binds.Bind(ctx, b)
	if err != nil {
		return false, fmt.Errorf("%w: 写入绑定失败: %v", ErrBindingUnavailable, err)
	}
	return ok, nil
}

// takenLocked 该资源是否已被别的环境占用。
//
// 判两条：资源 ID，以及出口地址 host:port。后者才是平台真正看得见的身份——
// 代理商对同一个地址重复下发不同订单号时，只看 ID 会放过一次「两店同 IP」。
// （对应的库侧约束是 egress_bindings 上的两条唯一索引，口径必须一致。）
func (a *Allocator) takenLocked(res *AcquiredProxy) bool {
	if _, ok := a.owner[res.ID]; ok {
		return true
	}
	for _, other := range a.resources {
		if other.Proxy.Host == res.Proxy.Host && other.Proxy.Port == res.Proxy.Port {
			return true
		}
	}
	return false
}

func (a *Allocator) rememberLocked(envID string, res *AcquiredProxy) {
	a.resources[res.ID] = res
	a.owner[res.ID] = envID
	a.byEnv[envID] = res.ID
}

// Release 解绑并归还给代理商。允许重复调用（第二次是 no-op）。
func (a *Allocator) Release(ctx context.Context, envID string) error {
	a.mu.Lock()
	id, ok := a.byEnv[envID]
	a.unbindLocked(envID)
	delete(a.prevAddr, envID)
	a.mu.Unlock()

	// 库里可能还有一条重启前留下的绑定：以库为准，否则「结束会话」会把厂商侧
	// 资源留在外面继续计费，直到它自然过期。
	if a.binds != nil {
		if _, terr := store.TenantFrom(ctx); terr == nil {
			b, berr := a.binds.Get(ctx, envID)
			if berr != nil {
				a.warn("读取出口绑定失败，只能按内存归还", "env", envID, "err", berr)
			}
			if b != nil {
				if err := a.binds.Release(ctx, envID); err != nil {
					a.warn("解除出口绑定失败", "env", envID, "err", err)
				}
				return a.provider.Release(ctx, b.ResourceID)
			}
		}
	}
	if !ok {
		return nil
	}
	return a.provider.Release(ctx, id)
}

// Reap 巡检：回收内存里已过期的资源并返回回收数量。由后台任务周期性调用。
//
// 只处理内存：库里到期的绑定由 SweepTenant 按租户清理（内存巡检是全局的，
// 拿不到租户上下文，也不该在这里猜）。
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

// SweepTenant 清理该租户在库里已到期的绑定，并向代理商归还对应资源。
//
// 为什么需要它：重启后内存表是空的，只有库知道「这台机器还占着哪些资源」。
// 不归还的代价是双份的——继续按天计费，且占着代理商的名额，
// 让新环境申请不到独占 IP（表现为莫名其妙的 ErrNoExclusiveResource）。
func (a *Allocator) SweepTenant(ctx context.Context, tenantID string, now time.Time) (int, error) {
	if a.binds == nil || tenantID == "" {
		return 0, nil
	}
	tctx := store.WithTenant(ctx, tenantID)
	doomed, err := a.binds.DeleteExpired(tctx, tenantID, now)
	if err != nil {
		return 0, err
	}
	for _, b := range doomed {
		a.mu.Lock()
		a.forgetLocked(b.EnvID, b.ResourceID)
		a.mu.Unlock()
		if err := a.provider.Release(ctx, b.ResourceID); err != nil {
			a.warn("归还到期出口资源失败（可能已自然过期）", "resource", b.ResourceID, "env", b.EnvID, "err", err)
		}
	}
	return len(doomed), nil
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

// forgetLocked 按 envID/resourceID 清掉内存里的痕迹（库那边已由调用方删除）。
func (a *Allocator) forgetLocked(envID, resourceID string) {
	if id, ok := a.byEnv[envID]; ok {
		delete(a.resources, id)
		delete(a.owner, id)
		delete(a.byEnv, envID)
	}
	if resourceID != "" {
		delete(a.owner, resourceID)
		delete(a.resources, resourceID)
	}
	delete(a.prevAddr, envID)
}

func (a *Allocator) warn(msg string, args ...any) {
	l := a.log
	if l == nil {
		l = slog.Default()
	}
	l.Warn(msg, args...)
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
