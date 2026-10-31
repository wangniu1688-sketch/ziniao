package network

import (
	"fmt"
	"sort"
)

// Candidate 可调度网络资源候选（来自设备池/代理池）。
type Candidate struct {
	ID        string
	Country   string
	Region    string
	ASNType   ASNType
	Purity    int // 0-100
	Health    int // 0-100
	UsedSlots int
	Capacity  int
	StickyKey string // 上次分配给同一店铺/用户的标识（优先稳定复用）
}

// ScheduleRequest 调度请求（M3.2 设备池调度）。
type ScheduleRequest struct {
	Country       string
	MinPurity     int
	PreferASNType []ASNType
	StickyKey     string // 优先复用上次资源（一店多人：优先稳定同设备/同 ASN）
}

// Schedule 选择最优资源：国家与纯净度硬过滤，再按
// 纯净度 40% + 健康 30% + 余量 20% + 粘性 10% 排序。
func Schedule(cands []Candidate, req ScheduleRequest) (*Candidate, error) {
	if len(cands) == 0 {
		return nil, fmt.Errorf("network: 无可用网络资源")
	}
	ok := []Candidate{}
	for _, c := range cands {
		if req.Country != "" && c.Country != req.Country {
			continue
		}
		if c.Purity < req.MinPurity {
			continue
		}
		if c.Capacity > 0 && c.UsedSlots >= c.Capacity {
			continue
		}
		ok = append(ok, c)
	}
	if len(ok) == 0 {
		return nil, fmt.Errorf("network: 无满足条件的资源（country=%s min_purity=%d）", req.Country, req.MinPurity)
	}
	rank := func(c Candidate) float64 {
		free := 100.0
		if c.Capacity > 0 {
			free = float64(c.Capacity-c.UsedSlots) * 100 / float64(c.Capacity)
		}
		prefer := 0.0
		for _, t := range req.PreferASNType {
			if t == c.ASNType {
				prefer = 100
				break
			}
		}
		sticky := 0.0
		if req.StickyKey != "" && c.StickyKey == req.StickyKey {
			sticky = 100
		}
		// 权重：纯净度 35% + 健康 25% + 余量 15% + 类型偏好 5% + 粘性 20%
		// 粘性权重高是为了「一店多人优先稳定同设备/同 ASN」（M3.2）。
		return float64(c.Purity)*0.35 + float64(c.Health)*0.25 + free*0.15 + prefer*0.05 + sticky*0.20
	}
	sort.SliceStable(ok, func(i, j int) bool { return rank(ok[i]) > rank(ok[j]) })
	return &ok[0], nil
}
