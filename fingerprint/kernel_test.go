package fingerprint

import (
	"strconv"
	"strings"
	"testing"
)

func baselineLib(t *testing.T) *BaselineLibrary {
	t.Helper()
	lib, err := LoadBaselines()
	if err != nil {
		t.Fatalf("基线加载失败: %v", err)
	}
	return lib
}

func usProfile() Profile { return BuiltinProfiles()[0] } // UA: Chrome/131.0.0.0

// TestAlignKernelCapsAtLocalKernel 内核对齐：不得声称高于本机内核，
// 也不得与代理实际发出的握手矛盾（131 的 hello 是 120_PQ，真正发出的是 120）。
func TestAlignKernelCapsAtLocalKernel(t *testing.T) {
	lib := baselineLib(t)

	// 本机 156（远高于基线库上限）：取上限条目 131 → 其握手实为 120 → UA 对齐到 120
	p := usProfile()
	a := AlignKernel(&p, lib, 156)
	if a.Major != 120 {
		t.Fatalf("生效内核应为 120，实际 %d（note=%s）", a.Major, a.Note)
	}
	if !strings.Contains(p.UA, "Chrome/120.0.0.0") {
		t.Fatalf("UA 未对齐到 120（桌面 UA 只出大版本）: %s", p.UA)
	}
	if p.ChromeFullVersion != "120.0.6099.109" {
		t.Fatalf("UA-CH 完整版本应为 120.0.6099.109，实际 %q", p.ChromeFullVersion)
	}
	if a.Note == "" || !strings.Contains(a.Note, "156") {
		t.Fatalf("未说明为什么不是本机内核版本: %q", a.Note)
	}
	if a.Baseline == nil || a.Baseline.HelloID != "HelloChrome_120" {
		t.Fatalf("生效基线应为 120 条目，实际 %+v", a.Baseline)
	}

	// 本机 115：上限 115 → 基线 115（hello 115_PQ → 115）→ UA 115，且无需回落说明
	p2 := usProfile()
	a2 := AlignKernel(&p2, lib, 115)
	if a2.Major != 115 || !strings.Contains(p2.UA, "Chrome/115.0.0.0") {
		t.Fatalf("本机 115 应对齐到 115：major=%d ua=%s", a2.Major, p2.UA)
	}
	if a2.Note != "" {
		t.Fatalf("已对齐到本机内核时不该有说明: %q", a2.Note)
	}

	// 本机 100（低于基线库最低条目）：只能取最低条目 115，并把原因写清楚
	p3 := usProfile()
	a3 := AlignKernel(&p3, lib, 100)
	if a3.Major != 115 {
		t.Fatalf("本机 100 应回退到最低条目 115，实际 %d", a3.Major)
	}
	if !strings.Contains(a3.Note, "100") || !strings.Contains(a3.Note, "115") {
		t.Fatalf("未说明回退原因: %q", a3.Note)
	}

	// 本机未知（0）：不做上限约束，等价于「按画像版本 + 握手回落」
	p4 := usProfile()
	a4 := AlignKernel(&p4, lib, 0)
	if a4.Major != 120 || !strings.Contains(p4.UA, "Chrome/120.0.0.0") {
		t.Fatalf("未探测到本机内核时应沿用画像版本并做握手回落：major=%d ua=%s", a4.Major, p4.UA)
	}
}

// TestAlignKernelKeepsUACHInSync 只改 UA 不改 UA-CH 是自洽性事故：
// 自洽性规则 uach-ua 查的就是这两个来源是否同源。
func TestAlignKernelKeepsUACHInSync(t *testing.T) {
	lib := baselineLib(t)
	p := usProfile()
	a := AlignKernel(&p, lib, 156)

	major, err := p.ChromeMajor()
	if err != nil || major != a.Major {
		t.Fatalf("UA 解析出的内核版本与对齐结论不符: %d vs %d (%v)", major, a.Major, err)
	}
	joined := strings.Join(p.UACH.Brands, " ")
	if !strings.Contains(joined, `Chromium";v="`+strconv.Itoa(a.Major)) {
		t.Fatalf("UA-CH brands 未随 UA 对齐: %v", p.UACH.Brands)
	}
	if strings.Contains(joined, `Chromium";v="131`) {
		t.Fatalf("UA-CH 仍残留对齐前的 131: %v", p.UACH.Brands)
	}
	// 完整版本只在 UA-CH 的 fullVersionList 里出现，不能写成冻结的 X.0.0.0
	if !strings.Contains(strings.Join(p.UACH.FullVersionList, " "), a.Version) {
		t.Fatalf("UA-CH fullVersionList 缺少完整版本 %q: %v", a.Version, p.UACH.FullVersionList)
	}

	// 对齐后的画像必须仍能通过自洽性体检（uach-ua 是 major 级规则）
	rep := NewEngine().Evaluate(&p, CheckContext{
		IPCountry: p.Country, ProxyCountry: p.Country, KernelMajor: a.Major, TLS: a.Baseline,
	})
	for _, i := range rep.Issues {
		if i.Rule == "uach-ua" {
			t.Fatalf("UA 与 UA-CH 仍不自洽: %s", i.Message)
		}
	}
	if !rep.Passed {
		t.Fatalf("对齐后画像自洽性未通过: score=%d issues=%+v", rep.Score, rep.Issues)
	}
}

// TestAlignKernelIsDefensive 对齐永远只能是增强：nil / 无法解析 / 空基线库都不得改坏画像。
func TestAlignKernelIsDefensive(t *testing.T) {
	lib := baselineLib(t)

	if got := AlignKernel(nil, lib, 156); got.Major != 0 || got.Note == "" {
		t.Fatalf("nil 画像应原样返回并给出说明: %+v", got)
	}

	broken := Profile{UA: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0)"}
	before := broken.UA
	if got := AlignKernel(&broken, lib, 156); broken.UA != before || got.Note == "" {
		t.Fatalf("UA 无 Chrome 段时不得改动: %q", broken.UA)
	}

	empty := &BaselineLibrary{byMajor: map[int]TLSBaseline{}}
	p := usProfile()
	uaBefore := p.UA
	if got := AlignKernel(&p, empty, 156); p.UA != uaBefore || got.Major != 131 {
		t.Fatalf("空基线库不得改动画像: ua=%s major=%d", p.UA, got.Major)
	}

	// nil 基线库同样不能 panic
	p2 := usProfile()
	if got := AlignKernel(&p2, nil, 156); got.Major != 131 {
		t.Fatalf("nil 基线库应沿用画像版本，实际 %d", got.Major)
	}
}

// TestUAFrozenVersion 桌面 Chrome（M110 起）的 UA 只出大版本，完整版本只在 UA-CH 里。
func TestUAFrozenVersion(t *testing.T) {
	if got := UAFrozenVersion(120); got != "120.0.0.0" {
		t.Fatalf("UA 冻结版本应为 120.0.0.0，实际 %q", got)
	}
	if got := UAFrozenVersion(0); got != "" {
		t.Fatalf("非法大版本应返回空串，实际 %q", got)
	}
}
