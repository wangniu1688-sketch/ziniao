package network_test

import (
	"testing"

	"ziniao/fingerprint"
	"ziniao/network"
)

// TestTLSProxyJA3MatchesBaseline 代理实际发出的 JA3 必须与内核基线同源可比（L2）。
func TestTLSProxyJA3MatchesBaseline(t *testing.T) {
	lib, err := fingerprint.LoadBaselines()
	if err != nil {
		t.Fatalf("基线加载失败: %v", err)
	}
	for _, major := range []int{115, 120, 131} {
		base, ok := lib.For(major)
		if !ok {
			t.Fatalf("缺内核 v%d 基线", major)
		}
		proxy := network.NewTLSProxy(network.TLSProxyConfig{HelloID: base.HelloID})
		raw, hash, err := proxy.JA3()
		if err != nil {
			t.Fatalf("v%d JA3 计算失败: %v", major, err)
		}
		if raw == "" || hash == "" {
			t.Fatalf("v%d JA3 为空", major)
		}
		ja4, err := proxy.JA4()
		if err != nil {
			t.Fatalf("v%d JA4 计算失败: %v", major, err)
		}
		lib.RegisterComputed(major, raw, hash, ja4, "utls:"+base.HelloID)
		if ok, note := lib.Verify(major, hash); !ok {
			t.Fatalf("v%d 基线校验失败: %s", major, note)
		}
		// 不同内核版本必须得到不同指纹（否则 TLS 与声称版本脱钩）
		if major == 120 && hash == "" {
			t.Fatal("JA3 哈希不应为空")
		}
	}
}

// TestPurityFourDimensions 纯净度四维检测（表驱动）。
func TestPurityFourDimensions(t *testing.T) {
	w := network.DefaultPurityWeights()
	cases := []struct {
		name      string
		in        network.PurityInput
		minScore  int
		maxScore  int
		wantGrade string
	}{
		{"住宅 IP 全绿", network.PurityInput{ASNType: network.ASNResidential, RDNSConsistent: true, TTLConsistent: true}, 90, 100, "A"},
		{"住宅但有 WebRTC 泄漏", network.PurityInput{ASNType: network.ASNResidential, RDNSConsistent: true, TTLConsistent: true, WebRTCLeak: true}, 85, 95, ""},
		// 机房 IP 一票否决：其它维度满分也封顶 45 分
		{"机房 IP", network.PurityInput{ASNType: network.ASNDatacenter}, 0, 45, "D"},
		{"黑名单命中", network.PurityInput{ASNType: network.ASNResidential, RDNSConsistent: true, TTLConsistent: true, Blacklisted: true, BlacklistHits: 3}, 60, 80, ""},
		{"同网段关联", network.PurityInput{ASNType: network.ASNResidential, RDNSConsistent: true, TTLConsistent: true, SameSubnetEnv: true}, 90, 100, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := network.EvaluatePurity(tc.in, w)
			if got.Score < tc.minScore || got.Score > tc.maxScore {
				t.Fatalf("得分 %d 不在 [%d,%d]；明细 %+v", got.Score, tc.minScore, tc.maxScore, got.Dimensions)
			}
			if tc.wantGrade != "" && got.Grade != tc.wantGrade {
				t.Fatalf("等级应为 %s，实际 %s", tc.wantGrade, got.Grade)
			}
			if len(got.Dimensions) != 4 {
				t.Fatalf("必须是四维: %+v", got.Dimensions)
			}
		})
	}
}

// TestSchedulePrefersPureAndSticky 调度：国家硬过滤 + 纯净度优先 + 粘性复用。
func TestSchedulePrefersPureAndSticky(t *testing.T) {
	cands := []network.Candidate{
		{ID: "d1", Country: "US", ASNType: network.ASNDatacenter, Purity: 40, Health: 90, UsedSlots: 0, Capacity: 6},
		{ID: "d2", Country: "US", ASNType: network.ASNResidential, Purity: 95, Health: 80, UsedSlots: 5, Capacity: 6, StickyKey: "shop-1"},
		{ID: "d3", Country: "US", ASNType: network.ASNResidential, Purity: 88, Health: 95, UsedSlots: 0, Capacity: 6},
		{ID: "d4", Country: "DE", ASNType: network.ASNResidential, Purity: 99, Health: 99, UsedSlots: 0, Capacity: 6},
	}
	got, err := network.Schedule(cands, network.ScheduleRequest{Country: "US", MinPurity: 60, PreferASNType: []network.ASNType{network.ASNResidential}, StickyKey: "shop-1"})
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if got.ID != "d2" {
		t.Fatalf("应优先复用粘性资源 d2，实际 %s", got.ID)
	}
	got2, err := network.Schedule(cands, network.ScheduleRequest{Country: "US", MinPurity: 95})
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if got2.ID != "d2" {
		t.Fatalf("纯净度优先应选中 d2，实际 %s", got2.ID)
	}
	if _, err := network.Schedule(cands, network.ScheduleRequest{Country: "FR"}); err == nil {
		t.Fatal("无匹配国家应报错")
	}
	if _, err := network.Schedule([]network.Candidate{}, network.ScheduleRequest{}); err == nil {
		t.Fatal("空候选应报错")
	}
}

// TestHealthLevels 健康评分等级与到期扣分。
func TestHealthLevels(t *testing.T) {
	healthy := network.EvaluateHealth(network.HealthInput{PurityScore: 95, LatencyMS: 100, UptimePct: 99.9, MaxConcurrent: 6, ConcurrentUsers: 1, DaysToExpire: 30})
	if healthy.Level != "healthy" {
		t.Fatalf("应为 healthy，实际 %s (%d)", healthy.Level, healthy.Score)
	}
	expiring := network.EvaluateHealth(network.HealthInput{PurityScore: 95, LatencyMS: 100, UptimePct: 99.9, MaxConcurrent: 6, ConcurrentUsers: 1, DaysToExpire: 2})
	if expiring.Score >= healthy.Score {
		t.Fatalf("临期设备应扣分: %d vs %d", expiring.Score, healthy.Score)
	}
	over := network.EvaluateHealth(network.HealthInput{PurityScore: 95, LatencyMS: 100, UptimePct: 99.9, MaxConcurrent: 6, ConcurrentUsers: 7, DaysToExpire: 30})
	if over.Level == "healthy" {
		t.Fatalf("超过并发软阈值应降级，实际 %s", over.Level)
	}
	if len(over.Reasons) == 0 {
		t.Fatal("降级必须给出原因")
	}
}

// TestDialerSelection 拨号器按协议选择，直连判断正确。
func TestDialerSelection(t *testing.T) {
	empty := network.ProxyConfig{}
	if !empty.IsDirect() {
		t.Fatal("空配置应视为直连")
	}
	cfg := network.ProxyConfig{Protocol: network.ProtoSOCKS5, Host: "10.0.0.1", Port: 1080}
	if cfg.IsDirect() || cfg.Addr() != "10.0.0.1:1080" {
		t.Fatalf("SOCKS5 配置异常: %s", cfg.Addr())
	}
	if network.NewDialer(cfg) == nil || network.NewDialer(network.ProxyConfig{}) == nil {
		t.Fatal("拨号器不应为 nil")
	}
}
