package network

import (
	"strings"
	"testing"
)

func baseCfg() VendorConfig {
	return VendorConfig{
		AuthStyle:   "bearer",
		AcquirePath: "/api/v1/proxies/acquire",
		ReleasePath: "/api/v1/proxies/release",
		Protocol:    ProtoSOCKS5,
	}
}

// TestLoadVendorSpecsLegacySingle 没配 ZINIAO_IP_VENDORS 时行为必须与改造前完全一致：
// 单代理商部署不该因为这次改造被迫改配置。
func TestLoadVendorSpecsLegacySingle(t *testing.T) {
	t.Setenv("ZINIAO_IP_VENDORS", "")
	cfg := baseCfg()
	cfg.BaseURL = "https://vendor.example"
	specs, err := LoadVendorSpecs(cfg, "legacy-token")
	if err != nil {
		t.Fatalf("单代理商旧配置不该报错: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("应只有一家，实际 %d", len(specs))
	}
	if specs[0].VendorConfig.Name != "ip-vendor" || specs[0].VendorConfig.Token != "legacy-token" {
		t.Fatalf("旧配置未正确继承: %+v", specs[0])
	}
	if specs[0].VendorMeta.Weight != 1 || specs[0].VendorMeta.Disabled {
		t.Fatalf("默认应是权重 1 且启用: %+v", specs[0].VendorMeta)
	}
}

// TestLoadVendorSpecsMulti 两家代理商：名字、权重、国家、配额都要解析出来。
func TestLoadVendorSpecsMulti(t *testing.T) {
	t.Setenv("ZINIAO_IP_VENDORS", "ipidea, 922")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_BASE_URL", "https://ipidea.example")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_TOKEN", "t1")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_COUNTRIES", "us,jp")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_WEIGHT", "3")
	t.Setenv("ZINIAO_IP_VENDOR_922_BASE_URL", "https://922.example")
	t.Setenv("ZINIAO_IP_VENDOR_922_TOKEN", "t2")
	t.Setenv("ZINIAO_IP_VENDOR_922_PRIORITY", "10")
	t.Setenv("ZINIAO_IP_VENDOR_922_MAX_CONCURRENT", "50")

	specs, err := LoadVendorSpecs(baseCfg(), "legacy-token")
	if err != nil {
		t.Fatalf("多代理商配置解析失败: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("应有两家，实际 %d", len(specs))
	}
	a, b := specs[0], specs[1]
	if a.VendorMeta.Name != "ipidea" || a.VendorConfig.BaseURL != "https://ipidea.example" || a.VendorConfig.Token != "t1" {
		t.Fatalf("第一家解析错误: %+v", a)
	}
	if a.VendorMeta.Weight != 3 || len(a.VendorMeta.Countries) != 2 || a.VendorMeta.Countries[0] != "JP" {
		t.Fatalf("第一家的权重/国家解析错误: %+v", a.VendorMeta)
	}
	if b.VendorMeta.Name != "922" || b.VendorMeta.Priority != 10 || b.VendorMeta.MaxConcurrent != 50 {
		t.Fatalf("第二家解析错误: %+v", b.VendorMeta)
	}
}

// TestLoadVendorSpecsRejectsHalfConfig 半配置一律报错：
// 悄悄跳过一家，等于让运营以为流量已经切走，实际一半环境还压在出事的那家上。
func TestLoadVendorSpecsRejectsHalfConfig(t *testing.T) {
	t.Setenv("ZINIAO_IP_VENDORS", "ipidea,922")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_BASE_URL", "https://ipidea.example")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_TOKEN", "t1")
	t.Setenv("ZINIAO_IP_VENDOR_922_TOKEN", "t2")
	// 922 缺 BASE_URL
	if _, err := LoadVendorSpecs(baseCfg(), ""); err == nil {
		t.Fatalf("缺少 base url 应报错")
	}

	t.Setenv("ZINIAO_IP_VENDOR_922_BASE_URL", "https://922.example")
	t.Setenv("ZINIAO_IP_VENDOR_922_TOKEN", "")
	// 两家都不能共用一枚 token（共享 token 说明本来就是同一个账号）
	if _, err := LoadVendorSpecs(baseCfg(), "shared"); err == nil {
		t.Fatalf("多家代理商缺 token 时应报错")
	}
}

// TestLoadVendorSpecsRejectsBadNumbers 数字配错必须炸出来：
// 静默按 0 处理会让权重/配额看起来生效、实际走的是另一套路由。
func TestLoadVendorSpecsRejectsBadNumbers(t *testing.T) {
	for _, bad := range []string{"0", "-1", "abc"} {
		t.Setenv("ZINIAO_IP_VENDORS", "ipidea")
		t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_BASE_URL", "https://ipidea.example")
		t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_TOKEN", "t1")
		t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_WEIGHT", bad)
		if _, err := LoadVendorSpecs(baseCfg(), ""); err == nil {
			t.Fatalf("权重 %q 应被拒绝", bad)
		}
	}
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_WEIGHT", "2")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_MAX_CONCURRENT", "x")
	if _, err := LoadVendorSpecs(baseCfg(), ""); err == nil {
		t.Fatalf("并发上限写成非数字应报错")
	}
}

// TestLoadVendorSpecsRejectsDuplicate 重名必须报错（否则归还资源会变成抽奖）。
func TestLoadVendorSpecsRejectsDuplicate(t *testing.T) {
	t.Setenv("ZINIAO_IP_VENDORS", "ipidea,IPIDEA")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_BASE_URL", "https://ipidea.example")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_TOKEN", "t1")
	if _, err := LoadVendorSpecs(baseCfg(), ""); err == nil {
		t.Fatalf("重名应报错")
	}
}

// TestLoadVendorSpecsDisabledFlag 摘除标记要能被读出来（一键下线而不必删清单）。
func TestLoadVendorSpecsDisabledFlag(t *testing.T) {
	t.Setenv("ZINIAO_IP_VENDORS", "ipidea")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_BASE_URL", "https://ipidea.example")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_TOKEN", "t1")
	t.Setenv("ZINIAO_IP_VENDOR_IPIDEA_DISABLED", "true")
	specs, err := LoadVendorSpecs(baseCfg(), "")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !specs[0].VendorMeta.Disabled {
		t.Fatalf("DISABLED 未生效: %+v", specs[0].VendorMeta)
	}
	if !strings.EqualFold(envSuffix("vendor-a"), "VENDOR_A") {
		t.Fatalf("环境变量名规范化错误: %s", envSuffix("vendor-a"))
	}
}
