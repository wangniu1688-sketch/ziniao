package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	// ListByTenant 该租户的全部绑定（含已过期）：启动期靠它回填路由名额。
	ListByTenant(ctx context.Context) ([]types.EgressBinding, error)
}

// HistoryStore 出口绑定历史（可选端口）。
//
// 刻意与 BindingStore 分开，因为两者的失败后果不同：
//   - BindingStore 是正确性的一部分：写不进库就必须拒绝下发出口（否则重启即换 IP）；
//   - 历史是可观测性：写失败只能告警，不能因此让环境起不来——
//     那等于把一次数据库抖动放大成全站停摆。
type HistoryStore interface {
	AppendEvent(ctx context.Context, ev *types.EgressBindingEvent) error
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
	mu    sync.Mutex
	rt    *Router
	lease time.Duration // 厂商未返回到期时间时的兜底租约
	binds BindingStore  // 可空：未落库/单测时为纯内存模式
	hist  HistoryStore  // 可空：未接历史留档时只保留当前绑定
	log   *slog.Logger

	// 三张表全部按 (代理商, 资源ID) 复合键组织，与库侧唯一索引 (provider,resource_id) 同口径。
	//
	// 为什么不能用裸 resourceID 作键：两家代理商都可能签发 "res-1"（这在内网是常态，
	// 各家的订单号互不相干）。用裸 ID 时第二个环境会把第一个的条目覆盖掉，
	// 于是「谁占着这条出口」「归还给哪一家」「名额算谁的」三件事同时错位——
	// 归还时退掉别人家的线路、名额扣到别人头上，而两边的单测都还是绿的。
	resources map[string]*heldResource // ownerKey -> 资源正文
	byEnv     map[string]string        // envID -> ownerKey（保证粘性）
	owner     map[string]string        // ownerKey -> envID（互斥校验的真相源）
	tries     int                      // 单次分配最多向代理商申请几次
	// prevAddr envID -> 上次实际用过的出口。用于识别「这个环境的出口 IP 变了」。
	// 出口变更本身是极强的关联信号（同一账号一天内从不同国家/IP 登录），
	// 哪怕我们做了 correct 的隔离也必须让运营看见它，而不是静默发生。
	prevAddr map[string]string
	// dropWhy envID -> 最近一次「作废绑定」的成因，仅用于写进历史事件：
	// 作废与重新申请发生在两个函数里，不把这层传过去，
	// 历史里就只剩「重新申请出口」四个字，排障时等于没说。
	dropWhy map[string]string
	// holds 本进程从路由**拿到过**名额的资源（key 同 owner）。
	//
	// 为什么需要它：归还时必须区分「这条出口是本进程申请的」与
	// 「重启后按 egress_bindings 取回的」。后者从未经过 Router.Select，
	// 归还时照样 Done 一次就会扣掉**别的环境**的名额，
	// 表现是代理商明明有量、环境却越来越申请不到出口（且看不出原因）。
	holds map[string]bool
}

// heldResource 一条在用的出口资源 + 它的签发方。
// 两者必须绑在一起存：只存资源正文时，「这条资源该还给谁」就只能靠裸 ID 反查，
// 而裸 ID 在多家代理商之间会撞。
type heldResource struct {
	vendor string
	res    *AcquiredProxy
}

// AllocResult 分配结果与出口连续性信息。
type AllocResult struct {
	Proxy    *ProxyConfig
	Rotated  bool   // 本次出口与上次不同（非首次分配才有意义）
	PrevAddr string // 上次出口，空串表示首次分配
}

// NewAllocator 构造分配器（单代理商便捷入口）。lease<=0 时用 24h 兜底。
func NewAllocator(p Provider, lease time.Duration) *Allocator {
	return NewAllocatorWithRouter(NewRouter(Vendor{Provider: p}), lease)
}

// NewAllocatorWithRouter 按路由表构造分配器：出口从哪来由 Router 决定。
func NewAllocatorWithRouter(rt *Router, lease time.Duration) *Allocator {
	if lease <= 0 {
		lease = 24 * time.Hour
	}
	if rt == nil {
		rt = NewRouter()
	}
	return &Allocator{
		rt:        rt,
		lease:     lease,
		resources: map[string]*heldResource{},
		byEnv:     map[string]string{},
		owner:     map[string]string{},
		prevAddr:  map[string]string{},
		dropWhy:   map[string]string{},
		holds:     map[string]bool{},
		tries:     5,
	}
}

// SetBindingStore 接入持久化。必须在开始分配之前调用（构造后、服务启动前）。
//
// 不接也能跑（纯内存），但那等于「重启即换出口」——docs/TODO-phase2 里把
// 落库列为上真账号的硬前置，就是这条。
func (a *Allocator) SetBindingStore(s BindingStore) { a.binds = s }

// SetHistoryStore 接入出口绑定历史留档（可选）。
//
// 不接也能跑，但「这两个店铺历史上共用过同一 ASN」就永远查不出来：
// 当前绑定表在解绑那一刻就把证据抹掉了。生产必须接（cmd/server 已接）。
func (a *Allocator) SetHistoryStore(h HistoryStore) { a.hist = h }

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

// Router 当前的代理商路由表（排障/展示/按名字归还用）。
func (a *Allocator) Router() *Router { return a.rt }

// Provider 路由表里的主代理商（排障/展示用，多代理商时只代表默认那一家）。
func (a *Allocator) Provider() Provider { return a.rt.Primary() }

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

// Acquire 取一条出口资源本体（含资源 ID 与到期时间）。
//
// 与 Allocate 的区别只在于返回什么：Allocate 给的是连接用的配置
// （环境侧只要 host/port/凭据），而设备这类**长期持有者**还需要
// resource_id 与 expires_at——前者是归还的凭据，后者决定续费时
// 要不要顺带换出口。口令同样在里面，L4：不进日志、不进响应。
func (a *Allocator) Acquire(ctx context.Context, envID string, r Region) (*AcquiredProxy, error) {
	return a.acquire(ctx, envID, r)
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

	if key, ok := a.byEnv[envID]; ok {
		if hr, ok := a.resources[key]; ok && !expired(hr.res, time.Now()) {
			return hr.res, nil
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
	lastErr := error(nil)
	// 本轮已经失败过的代理商：一次分配里不重复试同一家。
	// 不排掉的话，主供应商接口一挂，tries 次会全部耗在它身上（每次等满超时），
	// 一家挂了 = 全站起不来，多代理商就白接了。
	skip := map[string]bool{}
	for i := 0; i < a.tries; i++ {
		sel, serr := a.rt.Select(ctx, r, skip)
		if serr != nil {
			// 可用代理商被这一轮试完了（或压根没有）：撞车不算失败，
			// 但要让运营看得到「撞了几次」，而不是一个干巴巴的 ErrNoVendorAvailable。
			if last != nil {
				return nil, fmt.Errorf("%w（候选冲突 %d 次，最后候选 %s）", ErrNoExclusiveResource, a.tries, last.ID)
			}
			// 有真实的失败原因时优先报原因：单代理商部署下「无可用代理商」是句误导，
			// 那家明明在表里，只是这次没申请下来。原始错误用 %w 包住，调用方能继续判。
			if lastErr != nil {
				return nil, fmt.Errorf("network: 出口申请失败（已试代理商 %v）：%w", keysOf(skip), lastErr)
			}
			return nil, serr
		}
		name := sel.Name
		res, err := sel.Provider.Acquire(ctx, r)
		if err != nil {
			lastErr = err
			skip[name] = true
			a.rt.Fail(name, err)
			a.rt.Done(name)
			a.warn("代理商申请出口失败，切换到其它代理商", "vendor", name, "err", err)
			continue
		}
		if res == nil || res.Proxy.Host == "" || res.Proxy.Port == 0 {
			lastErr = fmt.Errorf("network: 代理商 %s 返回了不完整的资源", name)
			skip[name] = true
			a.rt.Fail(name, lastErr)
			a.rt.Done(name)
			continue
		}
		a.rt.Success(name)
		// Select 已经预留了名额：从这一刻起这条资源在路由表里占一个坑，
		// 无论后面是落库成功、撞车还是写库失败，都必须原路还回去。
		a.holds[ownerKey(name, res.ID)] = true
		if res.ExpiresAt.IsZero() {
			res.ExpiresAt = time.Now().Add(a.lease)
		}
		if a.takenLocked(res, name) {
			// 与其它环境撞车：归还这家，换下一个（撞车不是这家的错，不进 skip）
			last = res
			a.dropSlotLocked(name, res.ID)
			a.releaseTo(ctx, name, res.ID)
			continue
		}
		if a.binds != nil {
			ok, perr := a.persistLocked(ctx, envID, r, res, name)
			if perr != nil {
				a.dropSlotLocked(name, res.ID)
				a.releaseTo(ctx, name, res.ID)
				return nil, perr
			}
			if !ok {
				// 库里已被别的环境（或滚动发布时另一个实例）抢走：归还后换下一个
				last = res
				a.dropSlotLocked(name, res.ID)
				a.releaseTo(ctx, name, res.ID)
				continue
			}
		}
		a.rememberLocked(envID, res, name)
		return res, nil
	}
	if last != nil {
		return nil, fmt.Errorf("%w（候选冲突 %d 次，最后候选 %s）", ErrNoExclusiveResource, a.tries, last.ID)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("network: 全部候选代理商均申请失败（%v）：%w", keysOf(skip), lastErr)
	}
	return nil, ErrNoExclusiveResource
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// releaseTo 把资源归还给它真正来自的那家代理商（**不负责名额记账**）。
//
// 为什么必须按名字分派：资源 ID 只在签发它的那家内部有意义。
// 找错了家，轻则报「资源不存在」，重则把同名 ID 指到的别人家的线路退掉——
// 后者会让另一个店铺的出口当场断掉，而且很难归因。
//
// 名额单独归还（dropSlotLocked / dropSlot）：重启后按库取回的绑定从未 Select 过，
// 在这里无条件 Done 一次，扣掉的是**别的环境**的名额。
func (a *Allocator) releaseTo(ctx context.Context, vendor, resourceID string) error {
	p := a.rt.Get(vendor)
	if p == nil {
		// 代理商已下架/改名：宁可让厂商侧自然过期，也不拿这个 ID 去别家碰运气。
		a.warn("出口资源所属代理商已不在路由表，跳过归还",
			"vendor", vendor, "resource", resourceID)
		return nil
	}
	err := p.Release(ctx, resourceID)
	if err != nil {
		a.warn("归还代理商资源失败（可能已自然过期）", "vendor", vendor, "resource", resourceID, "err", err)
	}
	return err
}

// dropSlotLocked 归还本进程占用的并发名额（调用方持 a.mu）。
// 未持有（重启后按库取回的资源、或已归还过）则什么也不做——名额账不能凭感觉扣。
func (a *Allocator) dropSlotLocked(vendor, resourceID string) {
	key := ownerKey(vendor, resourceID)
	if !a.holds[key] {
		return
	}
	delete(a.holds, key)
	a.rt.Done(vendor)
}

// dropSlot 归还名额（自行加锁；持锁路径必须用 dropSlotLocked）。
func (a *Allocator) dropSlot(vendor, resourceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropSlotLocked(vendor, resourceID)
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
		a.dropBindingLocked(ctx, b, types.EgressActionExpired, "绑定已到期")
		return nil, nil
	}
	// 按绑定记的代理商分派：重启后要找的是「当初签发这个资源的那家」。
	// 走默认家去取回，等于拿 A 家的订单号到 B 家查单——查不到还算好，
	// 查到同名订单就会把别人的出口塞给这个环境。
	p := a.rt.Get(b.Provider)
	if p == nil {
		a.dropBindingLocked(ctx, b, types.EgressActionDropped, "绑定记录的代理商已不在路由表")
		return nil, nil
	}
	att, ok := p.(Attacher)
	if !ok {
		a.dropBindingLocked(ctx, b, types.EgressActionDropped, "代理商未配置按 ID 取回凭据的接口")
		return nil, nil
	}
	res, aerr := att.Attach(ctx, b.ResourceID)
	if aerr != nil || res == nil || res.Proxy.Host == "" || res.Proxy.Port == 0 {
		a.warn("按 ID 取回出口凭据失败，将重新申请（出口若变化会触发轮换告警）",
			"env", envID, "resource", b.ResourceID, "err", aerr)
		a.dropBindingLocked(ctx, b, types.EgressActionDropped, "取回凭据失败")
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
	a.rememberLocked(envID, res, b.Provider)
	if b.LastExit != "" && a.prevAddr[envID] == "" {
		// 上次出口随库一起回来：否则重启后的 IP 跳变没有对照值，告警发不出来。
		a.prevAddr[envID] = b.LastExit
	}
	return res, nil
}

// dropBindingLocked 作废一条无法复用的绑定：解绑 + 归还厂商资源 + 记历史，
// 然后由调用方重新申请。
func (a *Allocator) dropBindingLocked(ctx context.Context, b *types.EgressBinding, action, reason string) {
	if err := a.binds.Release(ctx, b.EnvID); err != nil {
		a.warn("解除出口绑定失败（下次启动会重新判定）", "env", b.EnvID, "err", err)
	}
	// 名额记账：这条绑定若来自启动回填（库里既有），它的坑就是在这里还回去的。
	a.dropSlotLocked(b.Provider, b.ResourceID)
	_ = a.releaseTo(ctx, b.Provider, b.ResourceID)
	a.record(ctx, action, reason, b, "")
	// 保留上次出口：接着要重新申请，出口若变了必须让告警说得清「从哪变到哪」。
	if b.LastExit != "" {
		a.prevAddr[b.EnvID] = b.LastExit
	}
	// 把成因带到下一次绑定事件上：否则历史里只看到「重新申请」，看不出为什么。
	a.dropWhy[b.EnvID] = reason
	a.warn("既存出口绑定作废，重新申请出口", "env", b.EnvID, "resource", b.ResourceID, "reason", reason)
}

// persistLocked 落库绑定；返回 false 表示资源已被别人独占。
func (a *Allocator) persistLocked(ctx context.Context, envID string, r Region, res *AcquiredProxy, vendor string) (bool, error) {
	now := time.Now().UTC()
	b := &types.EgressBinding{
		EnvID: envID,
		// 记下签发方：重启复用与归还都靠它认人，多代理商下这是唯一凭据。
		Provider:   vendor,
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
	// 先看这个环境原先绑的是哪条（同一事务外的单行读，主键命中，成本可忽略）。
	// 只有要写历史时才读：历史是线索，不值得为它在每次分配上多一次往返。
	var prev *types.EgressBinding
	if a.hist != nil {
		if p, perr := a.binds.Get(ctx, envID); perr == nil {
			prev = p
		}
	}
	ok, err := a.binds.Bind(ctx, b)
	if err != nil {
		return false, fmt.Errorf("%w: 写入绑定失败: %v", ErrBindingUnavailable, err)
	}
	if !ok {
		return false, nil
	}
	action, prevExit := types.EgressActionBound, ""
	if prev != nil {
		if prev.ResourceID == b.ResourceID && prev.Host == b.Host && prev.Port == b.Port {
			// 同一环境重新写入同一资源（幂等重发）：什么都不算变化，别污染历史。
			delete(a.dropWhy, envID)
			return true, nil
		}
		action, prevExit = types.EgressActionRebound, prev.LastExit
	} else if a.prevAddr[envID] != "" {
		// 库里没有旧行但内存记得上一跳：原绑定在本轮里刚被作废（到期/取不回凭据）。
		action, prevExit = types.EgressActionRebound, a.prevAddr[envID]
	}
	reason := "首次分配出口"
	if action == types.EgressActionRebound {
		reason = "重新申请出口"
		if why := a.dropWhy[envID]; why != "" {
			reason = "原绑定作废（" + why + "），重新申请出口"
		}
	}
	delete(a.dropWhy, envID)
	a.record(ctx, action, reason, b, prevExit)
	return true, nil
}

// record 追加一条出口历史事件。
//
// 失败只告警：历史写不进去不该让环境起不来（那是把可观测性问题放大成可用性问题），
// 但也不能静默——所以每次失败都会在日志里留下 env 与动作。
func (a *Allocator) record(ctx context.Context, action, reason string, b *types.EgressBinding, prevExit string) {
	if a.hist == nil || b == nil || !types.ValidEgressAction(action) {
		return
	}
	ev := &types.EgressBindingEvent{
		EnvID: b.EnvID, Action: action,
		Provider: b.Provider, ResourceID: b.ResourceID,
		Protocol: b.Protocol, Host: b.Host, Port: b.Port,
		Country: b.Country, Region: b.Region, City: b.City,
		ASN: b.ASN, ASNType: b.ASNType,
		ExitAddr: b.LastExit, PrevExit: prevExit, Reason: reason,
	}
	if ev.ExitAddr == "" && b.Host != "" {
		ev.ExitAddr = fmt.Sprintf("%s:%d", b.Host, b.Port)
	}
	if err := a.hist.AppendEvent(ctx, ev); err != nil {
		a.warn("写入出口绑定历史失败（不影响本次分配，但历史会缺一段）",
			"env", b.EnvID, "action", action, "err", err)
	}
}

// takenLocked 该资源是否已被别的环境占用。
//
// 判两条：资源 ID，以及出口地址 host:port。后者才是平台真正看得见的身份——
// 代理商对同一个地址重复下发不同订单号时，只看 ID 会放过一次「两店同 IP」。
// （对应的库侧约束是 egress_bindings 上的两条唯一索引，口径必须一致。）
// vendor 参数：地址判重按代理商分域，与库侧唯一索引 (provider, host, port)
// 以及 mem 驱动的冲突判定保持同一口径。跨代理商同址在现实里不存在
// （不同代理商的 IP 段归属不同），真出现了也是代理商准入该管的事，
// 不该让分配器把两家都判成「已占用」而白白报错。
func (a *Allocator) takenLocked(res *AcquiredProxy, vendor string) bool {
	if _, ok := a.owner[ownerKey(vendor, res.ID)]; ok {
		return true
	}
	for _, other := range a.resources {
		if other.vendor != vendor {
			continue
		}
		if other.res.Proxy.Host == res.Proxy.Host && other.res.Proxy.Port == res.Proxy.Port {
			return true
		}
	}
	return false
}

func (a *Allocator) rememberLocked(envID string, res *AcquiredProxy, vendor string) {
	key := ownerKey(vendor, res.ID)
	a.resources[key] = &heldResource{vendor: vendor, res: res}
	a.owner[key] = envID
	a.byEnv[envID] = key
}

// ownerKey 互斥表的键：(代理商, 资源ID)。
func ownerKey(vendor, resourceID string) string { return vendor + "|" + resourceID }

// Release 解绑并归还给代理商。允许重复调用（第二次是 no-op）。
func (a *Allocator) Release(ctx context.Context, envID string) error {
	a.mu.Lock()
	key, ok := a.byEnv[envID]
	// 先记下归属再解绑：unbindLocked 会把这条记录整个删掉，
	// 之后再读就只剩空串，归还时会找不到家，历史也拼不出这一行。
	var res *AcquiredProxy
	vendor := ""
	if hr, hit := a.resources[key]; hit {
		res, vendor = hr.res, hr.vendor
	}
	a.unbindLocked(envID)
	delete(a.prevAddr, envID)
	delete(a.dropWhy, envID)
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
				a.record(ctx, types.EgressActionReleased, "主动释放（环境停用 / 手工回收）", b, "")
				// 这条绑定可能是启动期回填进名额账的（上一个进程留下的）：
				// 现在资源真的还回去了，坑也必须让出来。
				a.dropSlot(b.Provider, b.ResourceID)
				return a.releaseTo(ctx, b.Provider, b.ResourceID)
			}
		}
	}
	if !ok {
		return nil
	}
	// 纯内存模式（未落库）下也留一条历史：出口用过就要能查得到。
	if res == nil {
		return nil
	}
	a.record(ctx, types.EgressActionReleased, "主动释放（环境停用 / 手工回收）",
		bindingOf(envID, vendor, res), "")
	return a.releaseTo(ctx, vendor, res.ID)
}

// bindingOf 把内存里的资源投影成一条绑定（只用于写历史，不落库）。
func bindingOf(envID, vendor string, res *AcquiredProxy) *types.EgressBinding {
	return &types.EgressBinding{
		EnvID: envID, Provider: vendor, ResourceID: res.ID,
		Protocol: string(res.Proxy.Protocol), Host: res.Proxy.Host, Port: res.Proxy.Port,
		Country: res.Proxy.Country, Region: res.Proxy.Region, City: res.Proxy.City,
		ASN: res.Proxy.ASN, ASNType: string(res.Proxy.ASNType),
		LastExit: res.Proxy.Addr(),
	}
}

// Reap 巡检：回收内存里已过期的资源并返回回收数量。由后台任务周期性调用。
//
// 只处理内存：库里到期的绑定由 SweepTenant 按租户清理（内存巡检是全局的，
// 拿不到租户上下文，也不该在这里猜）。
func (a *Allocator) Reap(ctx context.Context) int {
	a.mu.Lock()
	now := time.Now()
	type doomed struct {
		vendor string
		resID  string
	}
	stale := []doomed{}
	for key, hr := range a.resources {
		if !expired(hr.res, now) {
			continue
		}
		stale = append(stale, doomed{vendor: hr.vendor, resID: hr.res.ID})
		if envID := a.owner[key]; envID != "" {
			delete(a.byEnv, envID)
		}
		a.dropSlotLocked(hr.vendor, hr.res.ID)
		delete(a.owner, key)
		delete(a.resources, key)
	}
	a.mu.Unlock()
	for _, d := range stale {
		_ = a.releaseTo(ctx, d.vendor, d.resID)
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
		a.forgetLocked(b.EnvID, b.Provider, b.ResourceID)
		a.mu.Unlock()
		// 历史必须在删除之前留档：DeleteExpired 已经把行删了，
		// 不在这里记一笔，「这个环境曾用过哪个 ASN」就永久消失。
		a.record(tctx, types.EgressActionExpired, "租约到期回收（未续期的绑定）", &b, "")
		// 到期绑定同样按库里记的代理商归还：这里的资源多半来自上一个进程，
		// 内存里没有归属记录，只有 egress_bindings.provider 说得清它属于谁。
		_ = a.releaseTo(ctx, b.Provider, b.ResourceID)
	}
	return len(doomed), nil
}

// RestoreInflight 启动期按库里仍存在的绑定回填路由的名额计数。
//
// 为什么必须做：Router.inflight 是内存计数，重启归零；而「库里那 N 条绑定」
// 对应的资源**还压在代理商身上**（设备按月租、环境跨重启复用）。
// 不回填就等于重启后的一段时间里名额无限，代理商那边已经满额了我们还继续派单。
//
// 回填的不只是计数：每条绑定同时登记进 holds，这样后续归还（主动释放、
// 到期 sweep、绑定作废）才有据可依地把坑让出来——
// 只加计数不还，会把一次重启变成永久性的配额泄漏。
//
// 逐个租户读而不做跨租户全表扫描：仓储只暴露带租户上下文的读口，
// 也避免给「绕过 RLS 的全局读」开先例。
func (a *Allocator) RestoreInflight(ctx context.Context, tenants []string) (int, error) {
	if a.binds == nil {
		return 0, nil
	}
	n := 0
	for _, tid := range tenants {
		if tid == "" {
			continue
		}
		list, err := a.binds.ListByTenant(store.WithTenant(ctx, tid))
		if err != nil {
			// 读不到就必须让启动失败：半回填比不回填更危险——
			// 它看起来「回填过了」，实际只有一部分代理商的配额是准的。
			return n, fmt.Errorf("network: 回填出口名额失败（租户 %s）: %w", tid, err)
		}
		for i := range list {
			b := list[i]
			if b.Provider == "" || b.ResourceID == "" {
				continue
			}
			a.mu.Lock()
			if a.rt.Seed(b.Provider) {
				a.holds[ownerKey(b.Provider, b.ResourceID)] = true
				n++
			} else {
				// 代理商已下架/改名：这条绑定的资源已经还不回去了，
				// 但它仍然占着对方名额（直到租约到期），所以只告警不计数。
				a.warn("库里的出口绑定指向已不在路由表的代理商，名额未回填",
					"tenant", tid, "env", b.EnvID, "vendor", b.Provider, "resource", b.ResourceID)
			}
			a.mu.Unlock()
		}
	}
	return n, nil
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
	for _, hr := range a.resources {
		if expired(hr.res, now) {
			exp++
		}
	}
	return Stats{Bound: len(a.byEnv), Total: len(a.resources), Expired: exp}
}

func (a *Allocator) unbindLocked(envID string) {
	key, ok := a.byEnv[envID]
	if !ok {
		return
	}
	hr := a.resources[key]
	// 名额随绑定一起归还：忘了这一步，in_use 只增不减，
	// 到 MaxConcurrent 之后所有新环境都会被判「无名额」。
	if hr != nil {
		a.dropSlotLocked(hr.vendor, hr.res.ID)
	}
	delete(a.owner, key)
	delete(a.resources, key)
	delete(a.byEnv, envID)
}

// forgetLocked 按 (envID, 代理商, resourceID) 清掉内存里的痕迹（库那边已由调用方删除）。
//
// 必须带代理商：重启后内存是空的，只能靠库里那一行的 provider+resource_id
// 拼出复合键，拿裸 resource ID 去猜会清掉别人家的同名条目。
func (a *Allocator) forgetLocked(envID, vendor, resourceID string) {
	if key, ok := a.byEnv[envID]; ok {
		if hr := a.resources[key]; hr != nil {
			a.dropSlotLocked(hr.vendor, hr.res.ID)
		}
		delete(a.resources, key)
		delete(a.owner, key)
		delete(a.byEnv, envID)
	}
	if resourceID != "" {
		key := ownerKey(vendor, resourceID)
		a.dropSlotLocked(vendor, resourceID)
		delete(a.owner, key)
		delete(a.resources, key)
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
