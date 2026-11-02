package network

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNoVendorAvailable 没有任何代理商能满足这次申请。
//
// 返回它而不是「退回默认代理商」：退回去的那家可能正是被摘掉的那家（出过事故、
// 账号被平台标记、或已经买不动了），静默降级等于让运营以为流量已经切走。
var ErrNoVendorAvailable = errors.New("network: 无可用出口代理商（已禁用 / 不支持该地区 / 已达并发上限 / 熔断中）")

// VendorMeta 代理商的路由属性。
//
// 刻意与 Provider 分开：适配器只管「怎么向这家要一个 IP」，
// 路由属性是运营口径（主备、权重、可售国家、名额），两者变化频率完全不同。
type VendorMeta struct {
	// Name 路由表里的唯一标识，同时写进 egress_bindings.provider
	// （归还与重启复用都靠它认人，改名等于让历史绑定失联）。
	Name string
	// Priority 越小越优先：主供应商先承接，溢出或故障才轮到备用。
	Priority int
	// Weight 同一优先级内的分摊权重，越大分到的越多。
	Weight int
	// Countries 可售国家（大写 ISO）；空表示不限。
	Countries []string
	// MaxConcurrent 同时在用的出口上限，0 表示不限。
	// 代理商的并发名额是花钱买的，超了会被对方限流或直接断流，
	// 所以这是「按合同配额分配」，不是性能优化。
	MaxConcurrent int
	// Disabled 手动摘除。用「禁用」而不是「启用」作字段：
	// 零值必须是可用——否则配置里漏写一个 true 就等于全站没出口。
	Disabled bool
}

// Vendor 一个可路由的代理商：适配器 + 路由属性。
type Vendor struct {
	Provider Provider
	Meta     VendorMeta
}

// vendorEntry 路由器内部的一条记录（带运行时状态）。
type vendorEntry struct {
	Provider
	meta      VendorMeta
	inflight  int       // 当前在用出口数（持有即计数，归还才减）
	fails     int       // 连续失败次数
	openUntil time.Time // 熔断到什么时候
	lastErr   string
}

// Name 路由表里的名字优先于适配器自报的名字：
// 同一家代理商可能用同一个适配器类型接入两次（不同账号/不同套餐），
// 那时必须靠 Meta.Name 区分，否则归还时会串到另一条线路上。
func (e *vendorEntry) Name() string {
	if e.meta.Name != "" {
		return e.meta.Name
	}
	return e.Provider.Name()
}

// Router 多代理商路由器。
//
// 它回答一个问题：这次申请该找哪家要出口。判据只有四类——
// 可不可用（禁用/熔断）、卖不卖这个国家、还有没有名额、以及主备与权重。
// 不做「质优者胜」的实时打分：代理商质量是采购决策，应该写进配置，
// 让运行时去猜只会得到一条没人能解释的路由。
type Router struct {
	mu            sync.Mutex
	vendors       []*vendorEntry
	failThreshold int           // 连续失败多少次熔断
	cooldown      time.Duration // 熔断冷却时长
	now           func() time.Time
}

// NewRouter 构造路由器。
func NewRouter(vs ...Vendor) *Router {
	r := &Router{failThreshold: 3, cooldown: time.Minute, now: time.Now}
	for _, v := range vs {
		_ = r.Add(v)
	}
	return r
}

// SetCircuit 调整熔断参数（threshold<=0 表示不熔断）。
func (r *Router) SetCircuit(threshold int, cooldown time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failThreshold = threshold
	if cooldown > 0 {
		r.cooldown = cooldown
	}
}

// Add 登记一家代理商。名字重复直接报错：重名会让「按名字归还资源」变成抽奖。
func (r *Router) Add(v Vendor) error {
	if v.Provider == nil {
		return errors.New("network: 登记代理商必须提供适配器")
	}
	name := strings.TrimSpace(v.Meta.Name)
	if name == "" {
		name = strings.TrimSpace(v.Provider.Name())
	}
	if name == "" {
		return errors.New("network: 登记代理商必须指定名字")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.vendors {
		if strings.EqualFold(e.Name(), name) {
			return fmt.Errorf("network: 代理商重名: %s", name)
		}
	}
	meta := v.Meta
	meta.Name = name
	if meta.Weight <= 0 {
		meta.Weight = 1
	}
	meta.Countries = normCountries(meta.Countries)
	r.vendors = append(r.vendors, &vendorEntry{Provider: v.Provider, meta: meta})
	return nil
}

// Selection 一次路由的结果。
//
// 刻意返回「原始适配器 + 路由名」而不是一个包装过的适配器：
// 包装会丢掉可选能力接口（Attacher 之类不在 Provider 里声明的方法不会被提升），
// 结果就是「重启后按 ID 取回出口」这条路径对所有代理商静默失效。
type Selection struct {
	Provider Provider
	Name     string
}

// Select 按策略挑一家代理商，并预留一个名额（调用方负责 Done/Fail）。
//
// exclude 用于同一轮分配里跳过刚失败过的那家：一次申请最多试 tries 次，
// 若每次都重新挑到同一家，就等于拿用户的启动耗时给这家重试。
func (r *Router) Select(_ context.Context, reg Region, exclude map[string]bool) (Selection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()

	type cand struct {
		e    *vendorEntry
		load float64
	}
	cands := make([]cand, 0, len(r.vendors))
	for _, e := range r.vendors {
		if e.meta.Disabled || exclude[e.Name()] {
			continue
		}
		if !e.openUntil.IsZero() && !now.After(e.openUntil) {
			continue // 熔断中
		}
		if !supportsCountry(e.meta, reg.Country) {
			continue
		}
		if e.meta.MaxConcurrent > 0 && e.inflight >= e.meta.MaxConcurrent {
			continue
		}
		cands = append(cands, cand{e: e, load: float64(e.inflight) / float64(e.meta.Weight)})
	}
	if len(cands) == 0 {
		return Selection{}, ErrNoVendorAvailable
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.e.meta.Priority != b.e.meta.Priority {
			return a.e.meta.Priority < b.e.meta.Priority
		}
		// 同优先级按「权重摊薄后的负载」：权重 3 的那家要扛 3 倍量才轮到下一家。
		if a.load != b.load {
			return a.load < b.load
		}
		// 负载也相同则按名字定序：同样的输入必须得到同样的输出，
		// 否则「这个环境为什么走了备用线路」在排障时无法复现。
		return a.e.Name() < b.e.Name()
	})
	pick := cands[0].e
	pick.inflight++
	return Selection{Provider: pick.Provider, Name: pick.Name()}, nil
}

// Done 归还一个名额。无论这次申请成功失败都要调，否则名额会越用越少，
// 表现是「代理商明明有量，环境却申请不到出口」。
func (r *Router) Done(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.getLocked(name); e != nil && e.inflight > 0 {
		e.inflight--
	}
}

// Fail 记录一次失败；连续失败达阈值即熔断冷却，冷却期内不再参与选择。
//
// 熔断不是惩罚厂家，是止损：一家接口挂了的时候，每次启动都要等满 15s 超时
// 才轮到下一家，一个批量开环境的动作能被拖成几十分钟。
func (r *Router) Fail(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.getLocked(name)
	if e == nil {
		return
	}
	e.fails++
	if err != nil {
		e.lastErr = err.Error()
	}
	if r.failThreshold > 0 && e.fails >= r.failThreshold {
		e.openUntil = r.now().Add(r.cooldown)
		e.fails = 0
	}
}

// Success 一次成功即清零连续失败并解除熔断（冷却结束后第一次成功才算恢复）。
func (r *Router) Success(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.getLocked(name); e != nil {
		e.fails = 0
		e.openUntil = time.Time{}
		e.lastErr = ""
	}
}

// Get 按名字取代理商。用于按 egress_bindings.provider 归还/复用资源——
// 找不到返回 nil，由调用方决定降级，绝不猜一个默认家：
// 拿 A 家的订单号去退 B 家的资源，轻则报错，重则退掉别人正在用的出口。
func (r *Router) Get(name string) Provider {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.getLocked(name)
	// 两处必须小心：
	//  1) 显式判空后再装箱——直接 return e 会把「nil *vendorEntry」装成非空接口值，
	//     调用方的 p == nil 判定失效，然后在空指针上调 Release 直接崩；
	//  2) 返回 e.Provider 而不是 e——包装体只提升 Provider 声明过的方法，
	//     可选能力（Attacher）会丢，表现为「重启后复用出口」对所有代理商静默失效。
	if e == nil {
		return nil
	}
	return e.Provider
}

// Primary 路由表里的第一家（按主备与名字排序），仅用于日志与展示。
func (r *Router) Primary() Provider {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 空表返回真 nil（不能返回装箱后的空指针，否则调用方的 nil 判定会失效）
	if len(r.vendors) == 0 {
		return nil
	}
	best := r.vendors[0]
	for _, e := range r.vendors[1:] {
		if e.meta.Priority < best.meta.Priority ||
			(e.meta.Priority == best.meta.Priority && e.Name() < best.Name()) {
			best = e
		}
	}
	return best.Provider
}

// Seed 启动期按库里仍存在的绑定回填一格在用量。名字不在路由表里返回 false。
//
// 为什么需要它：inflight 是纯内存计数，进程一重启就归零，
// 而库里的绑定（设备按月租、环境跨重启复用）并没有消失。
// 不回填的后果是「重启后的一段时间内名额看起来无限」——
// 代理商那边的真实并发已经满了，我们却继续派单，
// 对方轻则限流重则断流，这边的监控还一切正常。
func (r *Router) Seed(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.getLocked(name)
	if e == nil {
		return false
	}
	e.inflight++
	return true
}

// VendorStatus 代理商的运行时状态（不含任何凭据：L4 红线）。
type VendorStatus struct {
	Name          string   `json:"name"`
	Priority      int      `json:"priority"`
	Weight        int      `json:"weight"`
	Countries     []string `json:"countries"`
	MaxConcurrent int      `json:"max_concurrent"`
	Disabled      bool     `json:"disabled"`
	InUse         int      `json:"in_use"`
	CircuitOpen   bool     `json:"circuit_open"`
	LastError     string   `json:"last_error,omitempty"`
}

// Snapshot 导出路由表状态（排障与控制台展示用）。
func (r *Router) Snapshot() []VendorStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]VendorStatus, 0, len(r.vendors))
	for _, e := range r.vendors {
		cs := e.meta.Countries
		if cs == nil {
			cs = []string{}
		}
		out = append(out, VendorStatus{
			Name:          e.Name(),
			Priority:      e.meta.Priority,
			Weight:        e.meta.Weight,
			Countries:     cs,
			MaxConcurrent: e.meta.MaxConcurrent,
			Disabled:      e.meta.Disabled,
			InUse:         e.inflight,
			CircuitOpen:   !e.openUntil.IsZero() && !now.After(e.openUntil),
			LastError:     e.lastErr,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (r *Router) getLocked(name string) *vendorEntry {
	if name == "" {
		return nil
	}
	for _, e := range r.vendors {
		if strings.EqualFold(e.Name(), name) {
			return e
		}
	}
	return nil
}

func supportsCountry(m VendorMeta, country string) bool {
	if len(m.Countries) == 0 {
		return true
	}
	c := strings.ToUpper(strings.TrimSpace(country))
	if c == "" {
		return true // 不指定国家时不按国家筛：指定与否是两种申请方式，不能互相误伤
	}
	for _, x := range m.Countries {
		if strings.EqualFold(x, c) {
			return true
		}
	}
	return false
}

func normCountries(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
