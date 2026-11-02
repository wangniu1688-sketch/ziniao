package network

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// VendorSpec 一家代理商的完整启动配置：适配器配置 + 路由属性。
type VendorSpec struct {
	VendorConfig
	VendorMeta
}

// LoadVendorSpecs 从环境变量装配代理商清单。
//
// 约定（凭据只走环境变量：口令进 argv 会被任意进程用 wmic 读到，也不落库）：
//
//	ZINIAO_IP_VENDORS=ipidea,922                 名单，逗号分隔；空表示沿用单代理商旧配置
//	ZINIAO_IP_VENDOR_<NAME>_BASE_URL=https://... 必填
//	ZINIAO_IP_VENDOR_<NAME>_TOKEN=...            必填（仅一家时可用 ZINIAO_IP_TOKEN 兜底）
//	ZINIAO_IP_VENDOR_<NAME>_COUNTRIES=US,JP      可选，空=不限国家
//	ZINIAO_IP_VENDOR_<NAME>_WEIGHT=3             可选，默认 1
//	ZINIAO_IP_VENDOR_<NAME>_PRIORITY=0           可选，默认 0，越小越优先
//	ZINIAO_IP_VENDOR_<NAME>_MAX_CONCURRENT=100   可选，0=不限
//	ZINIAO_IP_VENDOR_<NAME>_DISABLED=true        可选，一键摘除而不必改清单
//
// 半配置一律报错而不是跳过：名单里写了却配不全的家会被静默忽略，
// 结果就是「运营以为流量已经切到备用线路，实际一半环境还压在出事的那家上」。
func LoadVendorSpecs(base VendorConfig, legacyToken string) ([]VendorSpec, error) {
	names := splitList(os.Getenv("ZINIAO_IP_VENDORS"))
	if len(names) == 0 {
		cfg := base
		if cfg.Name == "" {
			cfg.Name = "ip-vendor"
		}
		if cfg.Token == "" {
			cfg.Token = legacyToken
		}
		return []VendorSpec{{VendorConfig: cfg, VendorMeta: VendorMeta{Name: cfg.Name, Weight: 1}}}, nil
	}

	seen := map[string]bool{}
	out := make([]VendorSpec, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, fmt.Errorf("network: ZINIAO_IP_VENDORS 里代理商重名: %s", name)
		}
		seen[key] = true

		suffix := envSuffix(name)
		cfg := base
		cfg.Name = name
		cfg.BaseURL = strings.TrimSpace(os.Getenv("ZINIAO_IP_VENDOR_" + suffix + "_BASE_URL"))
		cfg.Token = strings.TrimSpace(os.Getenv("ZINIAO_IP_VENDOR_" + suffix + "_TOKEN"))
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("network: 代理商 %s 缺少 ZINIAO_IP_VENDOR_%s_BASE_URL", name, suffix)
		}
		if cfg.Token == "" {
			// 只有一家时允许沿用老的 ZINIAO_IP_TOKEN，避免单代理商部署被迫改配置；
			// 两家以上必须各配各的——共用一枚 token 说明它们本来就是同一个账号，
			// 那不该当成两家代理商来路由。
			if len(names) == 1 && legacyToken != "" {
				cfg.Token = legacyToken
			} else {
				return nil, fmt.Errorf("network: 代理商 %s 缺少 ZINIAO_IP_VENDOR_%s_TOKEN", name, suffix)
			}
		}

		weight, err := envIntOr("ZINIAO_IP_VENDOR_"+suffix+"_WEIGHT", 1)
		if err != nil {
			return nil, fmt.Errorf("network: 代理商 %s 的权重配置非法：%v", name, err)
		}
		if weight <= 0 {
			return nil, fmt.Errorf("network: 代理商 %s 的权重必须为正，实际 %d", name, weight)
		}
		priority, err := envIntOr("ZINIAO_IP_VENDOR_"+suffix+"_PRIORITY", 0)
		if err != nil {
			return nil, fmt.Errorf("network: 代理商 %s 的优先级配置非法：%v", name, err)
		}
		maxConcurrent, err := envIntOr("ZINIAO_IP_VENDOR_"+suffix+"_MAX_CONCURRENT", 0)
		if err != nil {
			return nil, fmt.Errorf("network: 代理商 %s 的并发上限配置非法：%v", name, err)
		}
		if maxConcurrent < 0 {
			return nil, fmt.Errorf("network: 代理商 %s 的并发上限不能为负，实际 %d", name, maxConcurrent)
		}
		out = append(out, VendorSpec{
			VendorConfig: cfg,
			VendorMeta: VendorMeta{
				Name:     name,
				Priority: priority,
				Weight:   weight,
				// 装载时就归一化大小写：国家匹配是全系统按大写 ISO 比较的，
				// 让配置里的 "us" 也能命中申请时的 "US"。
				Countries:     normCountries(splitList(os.Getenv("ZINIAO_IP_VENDOR_" + suffix + "_COUNTRIES"))),
				MaxConcurrent: maxConcurrent,
				Disabled:      envBoolOK("ZINIAO_IP_VENDOR_"+suffix+"_DISABLED", false),
			},
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("network: ZINIAO_IP_VENDORS 已设置但没解析出有效代理商名")
	}
	return out, nil
}

func envSuffix(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func splitList(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envIntOr 读整数配置。配了却写坏（如 "abc"）必须报错：
// 当成默认值跳过，等于让人以为权重/配额已经生效，而实际走的是另一套路由。
func envIntOr(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数，实际 %q", key, raw)
	}
	return n, nil
}

func envBoolOK(key string, def bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
