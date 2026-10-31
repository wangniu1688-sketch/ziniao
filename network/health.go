package network

import "fmt"

// HealthInput 设备健康评分输入（M3.2 设备健康度评分）。
type HealthInput struct {
	PurityScore     int     // 0-100
	LatencyMS       int     // 到目标站点的 RTT
	UptimePct       float64 // 近 7 天可用率
	FailedLogins    int     // 近期登录失败次数
	BindCount       int     // 已绑定账号数
	ConcurrentUsers int     // 当前同时使用人数
	MaxConcurrent   int     // 配额上限（默认 6）
	DaysToExpire    int     // 距到期天数
}

// HealthResult 健康评分结果。
type HealthResult struct {
	Score   int      `json:"score"`
	Level   string   `json:"level"` // healthy|warning|critical
	Reasons []string `json:"reasons"`
}

// EvaluateHealth 综合评分：纯净度 40% + 延迟 20% + 可用率 20% + 负载 10% + 到期 10%，
// 再按登录失败与并发超限扣分项降权。
func EvaluateHealth(in HealthInput) HealthResult {
	max := in.MaxConcurrent
	if max <= 0 {
		max = 6 // 配置中心默认值（规格明确值）
	}
	latency := 100 - in.LatencyMS/10
	if latency < 0 {
		latency = 0
	}
	if latency > 100 {
		latency = 100
	}
	uptime := int(in.UptimePct)
	if uptime > 100 {
		uptime = 100
	}
	load := 100 - in.ConcurrentUsers*100/max
	if load < 0 {
		load = 0
	}
	expire := 100
	if in.DaysToExpire <= 0 {
		expire = 0
	} else if in.DaysToExpire <= 3 {
		expire = 40
	} else if in.DaysToExpire <= 7 {
		expire = 70
	}
	score := in.PurityScore*40/100 + latency*20/100 + uptime*20/100 + load*10/100 + expire*10/100

	reasons := []string{}
	if in.FailedLogins > 0 {
		penalty := in.FailedLogins * 3
		if penalty > 15 {
			penalty = 15
		}
		score -= penalty
		reasons = append(reasons, fmt.Sprintf("近期登录失败 %d 次（-%d）", in.FailedLogins, penalty))
	}
	if in.ConcurrentUsers > max {
		score -= 10
		reasons = append(reasons, fmt.Sprintf("并发人数 %d 超过配额 %d（-10，L9 允许但告警）", in.ConcurrentUsers, max))
	}
	if in.BindCount > 20 {
		reasons = append(reasons, fmt.Sprintf("绑定账号 %d 超过建议上限 20", in.BindCount))
	}
	if score < 0 {
		score = 0
	}
	level := "healthy"
	if score < 60 {
		level = "critical"
	} else if score < 80 {
		level = "warning"
	}
	return HealthResult{Score: score, Level: level, Reasons: reasons}
}
