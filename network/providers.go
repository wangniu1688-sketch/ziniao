package network

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Region 代理商可售区域。
type Region struct {
	Code    string  `json:"code"`
	Country string  `json:"country"`
	City    string  `json:"city"`
	ASNType ASNType `json:"asn_type"`
}

// AcquiredProxy 申请到的代理资源。
type AcquiredProxy struct {
	ID        string      `json:"id"`
	Proxy     ProxyConfig `json:"proxy"`
	ExpiresAt time.Time   `json:"expires_at"`
}

// Provider 住宅代理商适配接口（Q6）：
// 首批内置 mock provider；接入真实代理商只需实现本接口，不改调度与检测逻辑。
type Provider interface {
	Name() string
	ListRegions(ctx context.Context) ([]Region, error)
	Acquire(ctx context.Context, r Region) (*AcquiredProxy, error)
	Release(ctx context.Context, id string) error
	Health(ctx context.Context) error
}

// MockProvider 内存 mock 代理商（本地演示与单测用）。
type MockProvider struct {
	mu      sync.Mutex
	seq     int
	Country string
	Type    ASNType
	City    string
	// VendorName 路由表里的名字。多代理商测试要靠它区分两家：
	// 名字相同的两家在归还时会互相串线（egress_bindings.provider 认名字）。
	VendorName string
	// PortOffset 端口偏移：让两家 mock 发出不一样的出口地址，
	// 否则分配器按代理商分域判重，两个不同代理商的出口会一模一样，测试看不出问题。
	PortOffset int
	issued     map[string]*AcquiredProxy
}

// NewMockProvider 构造 mock 代理商。
func NewMockProvider(country string, t ASNType, city string) *MockProvider {
	return &MockProvider{Country: country, Type: t, City: city, issued: map[string]*AcquiredProxy{}}
}

func (p *MockProvider) Name() string {
	if p.VendorName != "" {
		return p.VendorName
	}
	return "mock"
}

func (p *MockProvider) ListRegions(_ context.Context) ([]Region, error) {
	return []Region{{Code: p.Country + "-" + p.City, Country: p.Country, City: p.City, ASNType: p.Type}}, nil
}

func (p *MockProvider) Acquire(_ context.Context, r Region) (*AcquiredProxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 字面量构造（&MockProvider{...}）时 issued 是 nil，直接写会 panic。
	// 兜底初始化而不是要求调用方必须走构造函数：零值可用的结构体才不容易被用错。
	if p.issued == nil {
		p.issued = map[string]*AcquiredProxy{}
	}
	p.seq++
	id := fmt.Sprintf("mock-%d", p.seq)
	out := &AcquiredProxy{
		ID: id,
		Proxy: ProxyConfig{
			ID: id, Protocol: ProtoSOCKS5, Host: "127.0.0.1", Port: 10800 + p.PortOffset + p.seq,
			Country: r.Country, City: r.City, ASNType: r.ASNType,
		},
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}
	p.issued[id] = out
	return out, nil
}

// Attach 按 ID 取回尚未释放的资源，模拟「重启后向代理商取回凭据」。
// 有它，内存模式/本地演示也能验证「重启不换出口」这条路径。
func (p *MockProvider) Attach(_ context.Context, id string) (*AcquiredProxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.issued[id]
	if !ok {
		return nil, fmt.Errorf("network: 资源不存在: %s", id)
	}
	cp := *res
	return &cp, nil
}

func (p *MockProvider) Release(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.issued[id]; !ok {
		return fmt.Errorf("network: 资源不存在: %s", id)
	}
	delete(p.issued, id)
	return nil
}

func (p *MockProvider) Health(_ context.Context) error { return nil }
