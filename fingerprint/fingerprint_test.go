package fingerprint_test

import (
	"testing"

	"ziniao/fingerprint"
)

func lib(t *testing.T) *fingerprint.Library {
	t.Helper()
	return fingerprint.NewLibrary(fingerprint.DefaultLibraryConfig(), 42)
}

// TestBuiltinProfilesSelfConsistent 内置画像必须全部通过自洽性体检（L1）。
func TestBuiltinProfilesSelfConsistent(t *testing.T) {
	bl, err := fingerprint.LoadBaselines()
	if err != nil {
		t.Fatalf("基线加载失败: %v", err)
	}
	engine := fingerprint.NewEngine()
	for _, p := range fingerprint.BuiltinProfiles() {
		for _, template := range []fingerprint.TLSBaseline{} {
			_ = template
		}
		major, err := p.ChromeMajor()
		if err != nil {
			t.Fatalf("画像 %s UA 无法解析: %v", p.ID, err)
		}
		tls, _ := bl.For(major)
		rep := engine.Evaluate(&p, fingerprint.CheckContext{IPCountry: p.Country, ProxyCountry: p.Country, KernelMajor: major, TLS: tls})
		if !rep.Passed {
			t.Fatalf("内置画像 %s 自洽性未通过: score=%d issues=%+v", p.ID, rep.Score, rep.Issues)
		}
	}
}

// TestConsistencyDetectsTampering 篡改任一关键字段都必须被检出（表驱动）。
func TestConsistencyDetectsTampering(t *testing.T) {
	base := fingerprint.BuiltinProfiles()[0]
	cases := []struct {
		name   string
		mutate func(p *fingerprint.Profile)
		rule   string
	}{
		{"UA 与 OS 不一致", func(p *fingerprint.Profile) {
			p.UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
		}, "ua-os"},
		{"platform 与 OS 不一致", func(p *fingerprint.Profile) { p.OS.Platform = "MacIntel" }, "platform-os"},
		{"时区与国家不一致", func(p *fingerprint.Profile) { p.Timezone = "Asia/Tokyo" }, "tz-country"},
		{"语言与国家不一致", func(p *fingerprint.Profile) { p.Languages = []string{"ja-JP", "ja"} }, "lang-country"},
		{"GPU 与 OS 不匹配", func(p *fingerprint.Profile) { p.GPU.Renderer = "ANGLE (Apple, ANGLE Metal Renderer: Apple M2 Pro)" }, "gpu-os"},
		{"DPR>=2 但非 macOS", func(p *fingerprint.Profile) { p.Screen.DPR = 2 }, "screen-dpr"},
		{"核数与内存档位不真实", func(p *fingerprint.Profile) { p.CPU.Cores = 4; p.MemoryGB = 64 }, "cpu-memory-tier"},
		{"缺少 OS 标志字体", func(p *fingerprint.Profile) { p.Fonts = []string{"Arial"} }, "fonts-os"},
		{"混用第三方插件", func(p *fingerprint.Profile) {
			p.Plugins = append(p.Plugins, fingerprint.Plugin{Name: "Grammarly", Filename: "external"})
		}, "plugins-consistent"},
	}
	engine := fingerprint.NewEngine()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			rep := engine.Evaluate(&p, fingerprint.CheckContext{IPCountry: p.Country, ProxyCountry: p.Country})
			found := false
			for _, i := range rep.Issues {
				if i.Rule == tc.rule {
					found = true
				}
			}
			if !found {
				t.Fatalf("应检出规则 %s，实际问题: %+v", tc.rule, rep.Issues)
			}
		})
	}
}

// TestSampleWeightedAndScoped 采样：按国家/系统过滤，分布贴近权重且可重复。
func TestSampleWeightedAndScoped(t *testing.T) {
	l := lib(t)
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		p, err := l.Sample("US", "")
		if err != nil {
			t.Fatalf("采样失败: %v", err)
		}
		if p.Country != "US" {
			t.Fatalf("国家过滤失效: %s", p.Country)
		}
		counts[p.Model]++
	}
	if len(counts) < 2 {
		t.Fatalf("US 应有多款机型参与采样，实际 %v", counts)
	}
	if _, err := l.Sample("US", "macOS"); err != nil {
		t.Fatalf("按系统过滤采样失败: %v", err)
	}
	if _, err := l.Sample("NOWHERE", ""); err == nil {
		t.Fatal("无匹配画像应报错")
	}
}

// TestUsageCapDegrades 单模板使用次数超软上限后应降权（防止"批量同款"被识别）。
func TestUsageCapDegrades(t *testing.T) {
	l := fingerprint.NewLibrary(fingerprint.LibraryConfig{UsageCap: 1}, 7)
	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		p, err := l.Sample("US", "")
		if err != nil {
			t.Fatal(err)
		}
		seen[p.Model]++
	}
	if len(seen) < 2 {
		t.Fatalf("usage cap 未生效，仍集中在单一模板: %v", seen)
	}
}

// TestDriftDetection 漂移监控：关键字段变化 → high 风险；无变化 → 零漂移。
func TestDriftDetection(t *testing.T) {
	p := fingerprint.BuiltinProfiles()[0]
	snap := fingerprint.Capture(&p)
	if rep := fingerprint.DetectDrift(snap, snap.Values); len(rep.Items) != 0 || rep.Risk != "low" {
		t.Fatalf("与基线一致时不应有漂移: %+v", rep)
	}
	observed := map[string]string{}
	for k, v := range snap.Values {
		observed[k] = v
	}
	observed["ua"] = "Mozilla/5.0 (X11; Linux x86_64) Chrome/120"
	observed["dpr"] = "2"
	rep := fingerprint.DetectDrift(snap, observed)
	if rep.Risk != "high" {
		t.Fatalf("UA 漂移必须判定为 high，实际 %s", rep.Risk)
	}
	if len(rep.Items) != 2 {
		t.Fatalf("应有 2 个漂移项，实际 %+v", rep.Items)
	}
}

// TestJA3AndJA4 JA3/JA4 计算：确定性、与输入严格对应。
func TestJA3AndJA4(t *testing.T) {
	spec := fingerprint.ClientHelloSpec{
		Version:      0x0303,
		CipherSuites: []uint16{0x1301, 0x1302, 0x1303},
		Extensions:   []uint16{0, 23, 65281},
		Curves:       []uint16{29, 23, 24},
		PointFormats: []uint8{0},
		ALPN:         []string{"h2", "http/1.1"},
	}
	raw1, hash1 := fingerprint.JA3From(spec)
	raw2, hash2 := fingerprint.JA3From(spec)
	if raw1 != raw2 || hash1 != hash2 {
		t.Fatal("JA3 计算必须稳定")
	}
	if raw1 != "771,4865-4866-4867,0-23-65281,29-23-24,0" {
		t.Fatalf("JA3 原始串不符: %s", raw1)
	}
	// JA4 的版本位取 ClientHello 的 legacy_version（Chrome 恒为 TLS1.2，TLS1.3 由扩展声明）
	ja4 := fingerprint.JA4From(spec)
	if len(ja4) == 0 || ja4[:3] != "t12" {
		t.Fatalf("JA4 前缀应为 t12（TCP + legacy_version=TLS1.2）: %s", ja4)
	}
	// 修改套件必须改变指纹
	spec.CipherSuites = []uint16{0x1301}
	if _, h := fingerprint.JA3From(spec); h == hash1 {
		t.Fatal("修改密码套件后 JA3 必须变化")
	}
}

// TestBaselineLibrary 基线库：加载、回写计算值、离线校验（L8）。
func TestBaselineLibrary(t *testing.T) {
	bl, err := fingerprint.LoadBaselines()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, ok := bl.For(131); !ok {
		t.Fatal("应包含 131 内核基线")
	}
	if ok, _ := bl.Verify(131, "anything"); ok {
		t.Fatal("基线未计算时校验必须失败（fail-closed）")
	}
	bl.RegisterComputed(131, "raw", "deadbeef", "t13d0302_h1_h2", "utls:test")
	if !bl.Computed() {
		t.Fatal("回写后应标记为已计算")
	}
	if ok, _ := bl.Verify(131, "deadbeef"); !ok {
		t.Fatal("一致哈希应通过")
	}
	if ok, _ := bl.Verify(131, "wrong"); ok {
		t.Fatal("不一致哈希应失败")
	}
	if bl.BaselineHash() == "" {
		t.Fatal("基线库哈希不应为空")
	}
}

// TestInjectionScriptIsDeterministic 注入脚本：同画像 → 同基线哈希；脚本含关键覆盖点。
func TestInjectionScriptIsDeterministic(t *testing.T) {
	p := fingerprint.BuiltinProfiles()[1]
	s1 := fingerprint.InjectionScript(&p)
	s2 := fingerprint.InjectionScript(&p)
	if s1 != s2 {
		t.Fatal("注入脚本必须与画像严格对应（禁止随机）")
	}
	for _, key := range []string{"userAgent", "platform", "hardwareConcurrency", "getParameter", "getTimezoneOffset"} {
		if !contains(s1, key) {
			t.Fatalf("注入脚本缺少覆盖点: %s", key)
		}
	}
	plan, err := fingerprint.BuildInjectionPlan("env-1", &p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.BaselineHash != p.Hash() {
		t.Fatal("计划基线哈希必须与画像一致")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
