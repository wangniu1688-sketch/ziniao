package network

import "fmt"

// PurityInput 一次 IP 纯净度检测的输入（四维）。
type PurityInput struct {
	IP               string
	Blacklisted      bool
	BlacklistHits    int // 命中的黑名单数量
	ASNType          ASNType
	RDNSConsistent   bool // rDNS 是否与 IP 归属一致
	TTLConsistent    bool // TTL 是否与声称的网络类型一致
	WebRTCLeak       bool
	DNSLeak          bool
	IPv6Leak         bool
	SameSubnetEnv    bool // 同网段是否已有其它环境（关联风险）
	SharedASNHistory bool // 该 ASN 是否与其它环境历史共用
}

// PurityWeights 四维权重（配置中心可调，默认 40/30/20/10）。
type PurityWeights struct {
	Blacklist   int `key:"network.purity.blacklist"`
	Type        int `key:"network.purity.type"`
	Leak        int `key:"network.purity.leak"`
	Correlation int `key:"network.purity.correlation"`
}

// DefaultPurityWeights 默认权重。
func DefaultPurityWeights() PurityWeights {
	return PurityWeights{Blacklist: 40, Type: 30, Leak: 20, Correlation: 10}
}

// Dimension 一个维度的得分明细。
type Dimension struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Score  int    `json:"score"`
	Max    int    `json:"max"`
	Detail string `json:"detail"`
}

// PurityResult 纯净度结果。
type PurityResult struct {
	Score      int         `json:"score"`
	Max        int         `json:"max"`
	Grade      string      `json:"grade"` // A|B|C|D
	Dimensions []Dimension `json:"dimensions"`
}

// EvaluatePurity 自建四维检测为主（外部付费 API 仅做抽检交叉校验，见 ADR-0010）。
func EvaluatePurity(in PurityInput, w PurityWeights) PurityResult {
	dims := []Dimension{}

	// 1) 黑名单（0 命中满分，≥5 命中 0 分）
	bl := w.Blacklist
	if in.Blacklisted {
		bl = w.Blacklist - 10*in.BlacklistHits
		if bl < 0 {
			bl = 0
		}
	}
	dims = append(dims, Dimension{Key: "blacklist", Name: "黑名单", Score: bl, Max: w.Blacklist,
		Detail: fmt.Sprintf("命中 %d 个黑名单", in.BlacklistHits)})

	// 2) 类型判定（住宅/运营商 > 移动 > 机房）
	typeMax := w.Type
	typeScore := map[ASNType]int{
		ASNResidential: typeMax,
		ASNISP:         typeMax - 3,
		ASNMobile:      typeMax - 6,
		ASNDatacenter:  typeMax / 5,
		ASNUnknown:     typeMax / 3,
	}[in.ASNType]
	if in.RDNSConsistent && in.TTLConsistent {
		typeScore = minInt(typeMax, typeScore+2)
	}
	dims = append(dims, Dimension{Key: "type", Name: "类型判定(ASN/rDNS/TTL)", Score: typeScore, Max: typeMax,
		Detail: string(in.ASNType)})

	// 3) 泄漏检测（WebRTC / DNS / IPv6）
	leak := w.Leak
	if in.WebRTCLeak {
		leak -= 8
	}
	if in.DNSLeak {
		leak -= 6
	}
	if in.IPv6Leak {
		leak -= 6
	}
	if leak < 0 {
		leak = 0
	}
	dims = append(dims, Dimension{Key: "leak", Name: "泄漏检测", Score: leak, Max: w.Leak,
		Detail: fmt.Sprintf("webrtc=%v dns=%v ipv6=%v", in.WebRTCLeak, in.DNSLeak, in.IPv6Leak)})

	// 4) 关联度（同网段 / 同 ASN 历史共用）
	corr := w.Correlation
	if in.SameSubnetEnv {
		corr -= 4
	}
	if in.SharedASNHistory {
		corr -= 4
	}
	if corr < 0 {
		corr = 0
	}
	dims = append(dims, Dimension{Key: "correlation", Name: "关联度", Score: corr, Max: w.Correlation,
		Detail: fmt.Sprintf("same_subnet=%v shared_asn=%v", in.SameSubnetEnv, in.SharedASNHistory)})

	total, maxTotal := 0, 0
	for _, d := range dims {
		total += d.Score
		maxTotal += d.Max
	}
	pct := 0
	if maxTotal > 0 {
		pct = total * 100 / maxTotal
	}
	// 机房 IP 一票否决：其它维度再好也不能把总分拉高（跨境电商场景下机房 IP = 高危）
	if in.ASNType == ASNDatacenter && pct > 45 {
		pct = 45
	}
	return PurityResult{Score: pct, Max: 100, Grade: grade(pct), Dimensions: dims}
}

func grade(pct int) string {
	switch {
	case pct >= 90:
		return "A"
	case pct >= 75:
		return "B"
	case pct >= 60:
		return "C"
	default:
		return "D"
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
