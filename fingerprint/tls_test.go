package fingerprint

import "testing"

// TestParseChromeMajorFromHelloID uTLS 基线名 → 实际代表的 Chrome 版本。
// 这是「UA 与 TLS 自洽」的前提：131 复用 HelloChrome_120_PQ 时，
// 必须认出发出来的是 120，否则 UA 说 131、握手是 120 会被平台交叉比对识破。
func TestParseChromeMajorFromHelloID(t *testing.T) {
	cases := map[string]int{
		"HelloChrome_120_PQ": 120,
		"HelloChrome_120":    120,
		"HelloChrome_115_PQ": 115,
		"HelloChrome_100":    100,
		"HelloChrome_Auto":   0, // 不绑定版本，不做对齐
		"HelloFirefox_120":   0, // 非 Chrome
		"":                   0,
	}
	for name, want := range cases {
		if got := ParseChromeMajorFromHelloID(name); got != want {
			t.Errorf("%q: got %d, want %d", name, got, want)
		}
	}
}

func TestAlignUAChromeVersion(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.6778.86 Safari/537.36"
	got := AlignUAChromeVersion(ua, "120.0.6099.109")
	if want := "Chrome/120.0.6099.109"; !contains(got, want) {
		t.Fatalf("对齐失败: %s", got)
	}
	if contains(got, "131.0") {
		t.Fatalf("旧版本号残留: %s", got)
	}
	// Safari 尾巴不能被吃掉
	if !contains(got, "Safari/537.36") {
		t.Fatalf("UA 尾部被破坏: %s", got)
	}
	// 空版本 / 无 Chrome 段：原样返回，不许改坏
	if AlignUAChromeVersion(ua, "") != ua {
		t.Fatal("空版本号应原样返回")
	}
	if AlignUAChromeVersion("Mozilla/5.0 (iPhone)", "120.0.0.0") != "Mozilla/5.0 (iPhone)" {
		t.Fatal("无 Chrome 段应原样返回")
	}
}

// TestBaselineFallbackHasRealEntry 数据不变量（防回归）：
// 基线库里凡是「hello_id 版本 ≠ chromium_major」的回落条目（例如 131→120_PQ），
// 必须存在它实际代表的那个版本的条目——否则 shop.CreateEnv 无法对齐 UA，
// 环境会带着「UA 131 / TLS 120」的致命不自洽被下发。
func TestBaselineFallbackHasRealEntry(t *testing.T) {
	lib, err := LoadBaselines()
	if err != nil {
		t.Fatalf("加载基线库失败: %v", err)
	}
	checked := 0
	for _, major := range []int{96, 100, 102, 106, 115, 120, 124, 131} {
		bl, ok := lib.For(major)
		if !ok {
			continue
		}
		checked++
		eff := ParseChromeMajorFromHelloID(bl.HelloID)
		if eff == 0 {
			t.Errorf("major=%d hello_id=%q 无法解析版本（UA 将无法对齐）", major, bl.HelloID)
			continue
		}
		if eff != major {
			if _, ok := lib.For(eff); !ok {
				t.Errorf("回落条目 major=%d hello_id=%q 实际代表 %d，但基线库缺少 %d 的条目，UA 无法对齐", major, bl.HelloID, eff, eff)
			}
		}
	}
	if checked == 0 {
		t.Fatal("基线库里一条都没匹配到，检查测试用的版本清单")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
