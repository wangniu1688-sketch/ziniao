package network

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VendorConfig 通用住宅代理商 API 配置。
//
// 为什么做「配置化通用适配器」而不是给每家厂商各写一个：
// 住宅代理商（ipidea / 922 / Oxylabs / BrightData / 各家白牌）本质上都是同一套语义——
// 申请一个出口 IP、用完释放、可选查询可售区域。差别只在 URL 路径、鉴权方式和字段名。
// 把这些差异收敛成配置 + 宽松字段映射，用户买到任意一家都能直接填，不必改代码。
type VendorConfig struct {
	Name     string // 展示用（如 "ipidea"），也用于多代理商路由
	BaseURL  string // 如 https://api.vendor.example
	Token    string // 密钥；只存在于内存与环境变量，不进日志、不进 argv
	AuthKey  string // query 鉴权时的参数名（AuthStyle=query 用）
	Username string // AuthStyle=basic 时的用户名（部分厂商用 user/pass 而非 token）

	AuthStyle   string // bearer | query | basic
	RegionsPath string
	AcquirePath string
	ReleasePath string

	Protocol Protocol      // 申请时要求的协议，默认 socks5
	Timeout  time.Duration // 单次 API 调用超时
}

// HTTPProvider 通过 REST API 对接住宅代理商。
type HTTPProvider struct {
	cfg VendorConfig
	cl  *http.Client
}

// NewHTTPProvider 构造代理商适配器。
func NewHTTPProvider(cfg VendorConfig) *HTTPProvider {
	if cfg.AuthStyle == "" {
		cfg.AuthStyle = "bearer"
	}
	if cfg.Protocol == "" {
		cfg.Protocol = ProtoSOCKS5
	}
	to := cfg.Timeout
	if to <= 0 {
		to = 15 * time.Second
	}
	return &HTTPProvider{
		cfg: cfg,
		cl:  &http.Client{Timeout: to},
	}
}

func (p *HTTPProvider) Name() string {
	if p.cfg.Name == "" {
		return "http-vendor"
	}
	return p.cfg.Name
}

// ListRegions 查询可售区域。厂商没提供该接口时返回占位 Region，
// 让上层仍能按国家申请（多数代理商以 country/city 参数直接售卖）。
func (p *HTTPProvider) ListRegions(ctx context.Context) ([]Region, error) {
	if p.cfg.RegionsPath == "" {
		return []Region{{Code: "any", Country: "", City: "", ASNType: ASNResidential}}, nil
	}
	var raw map[string]any
	if err := p.call(ctx, http.MethodGet, p.cfg.RegionsPath, nil, &raw); err != nil {
		return nil, err
	}
	rows := collectObjects(raw)
	out := make([]Region, 0, len(rows))
	for _, r := range rows {
		code := stringField(r, "code", "region", "iso", "country_code")
		if code == "" {
			continue
		}
		out = append(out, Region{
			Code:    code,
			Country: stringField(r, "country", "country_code", "iso"),
			City:    stringField(r, "city", "city_name"),
			ASNType: ASNResidential,
		})
	}
	if len(out) == 0 {
		return []Region{{Code: "any", Country: "", City: "", ASNType: ASNResidential}}, nil
	}
	return out, nil
}

// Acquire 申请一个出口资源。返回的资源必须与其它环境互斥，
// 该约束由上层 Allocator 保证（见 allocator.go）。
func (p *HTTPProvider) Acquire(ctx context.Context, r Region) (*AcquiredProxy, error) {
	body := map[string]any{
		"country":  r.Country,
		"city":     r.City,
		"count":    1,
		"protocol": string(p.cfg.Protocol),
		"format":   "json",
	}
	var raw map[string]any
	if err := p.call(ctx, http.MethodPost, p.cfg.AcquirePath, body, &raw); err != nil {
		return nil, err
	}
	nodes := collectObjects(raw)
	for _, n := range nodes {
		host := stringField(n, hostKeys...)
		port := intField(n, portKeys...)
		if host == "" || port == 0 {
			continue
		}
		id := stringField(n, "id", "sid", "neek", "order_id", "proxy_id")
		if id == "" {
			id = fmt.Sprintf("%s-%d-%d", host, port, time.Now().UnixNano())
		}
		out := &AcquiredProxy{
			ID: id,
			Proxy: ProxyConfig{
				ID:       id,
				Protocol: p.cfg.Protocol,
				Host:     host,
				Port:     port,
				Username: stringField(n, "username", "user", "account", "proxy_user"),
				Password: stringField(n, "password", "pass", "pwd", "proxy_pass"),
				Country:  firstNonEmpty(r.Country, stringField(n, "country", "country_code")),
				City:     firstNonEmpty(r.City, stringField(n, "city", "city_name")),
				ASN:      stringField(n, "asn", "asn_number"),
				ASNType:  r.ASNType,
			},
			ExpiresAt: parseVendorTime(n),
		}
		if out.Proxy.ASNType == "" {
			out.Proxy.ASNType = ASNResidential
		}
		return out, nil
	}
	return nil, fmt.Errorf("network: 代理商 %s 返回中未找到可用出口资源", p.Name())
}

// Release 归还资源。多数厂商的释放失败通常是「已过期」，不视为致命错误——
// 上层只要把本地绑定解除，资源到期会自然回收。
func (p *HTTPProvider) Release(ctx context.Context, id string) error {
	if p.cfg.ReleasePath == "" {
		return nil
	}
	body := map[string]any{"id": id}
	var raw map[string]any
	return p.call(ctx, http.MethodPost, p.cfg.ReleasePath, body, &raw)
}

// Health 健康检查：能连上代理商且非 5xx 即视为健康。
//
// 注意必须真的发一次请求。早期实现里 Health 走 ListRegions，而 RegionsPath 未配置时
// ListRegions 直接返回占位值、**根本不打网络**——健康检查永远"通过"，
// 代理商地址写错也不会被发现，直到某个店铺分配出口时才报错。
func (p *HTTPProvider) Health(ctx context.Context) error {
	if strings.TrimSpace(p.cfg.RegionsPath) != "" {
		_, err := p.ListRegions(ctx)
		return err
	}
	u := strings.TrimRight(p.cfg.BaseURL, "/")
	if u == "" {
		return fmt.Errorf("network: 代理商 %s 未配置 BaseURL", p.Name())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	switch strings.ToLower(p.cfg.AuthStyle) {
	case "query":
		q := req.URL.Query()
		key := p.cfg.AuthKey
		if key == "" {
			key = "token"
		}
		q.Set(key, p.cfg.Token)
		req.URL.RawQuery = q.Encode()
	case "basic":
		req.SetBasicAuth(p.cfg.Username, p.cfg.Token)
	default:
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	resp, err := p.cl.Do(req)
	if err != nil {
		return fmt.Errorf("network: 代理商 %s 不可达: %w", p.Name(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("network: 代理商 %s 服务异常 HTTP %d", p.Name(), resp.StatusCode)
	}
	return nil
}

// call 发起一次带鉴权的 JSON 调用。
// 注意：错误信息里不能带 Token（口令泄漏是本项目 L4 红线）。
func (p *HTTPProvider) call(ctx context.Context, method, path string, body map[string]any, out *map[string]any) error {
	if p.cfg.BaseURL == "" {
		return fmt.Errorf("network: 代理商 %s 未配置 BaseURL", p.Name())
	}
	u, err := url.JoinPath(strings.TrimRight(p.cfg.BaseURL, "/"), strings.TrimLeft(path, "/"))
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch strings.ToLower(p.cfg.AuthStyle) {
	case "query":
		q := req.URL.Query()
		key := p.cfg.AuthKey
		if key == "" {
			key = "token"
		}
		q.Set(key, p.cfg.Token)
		req.URL.RawQuery = q.Encode()
	case "basic":
		req.SetBasicAuth(p.cfg.Username, p.cfg.Token)
	default:
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	resp, err := p.cl.Do(req)
	if err != nil {
		return fmt.Errorf("network: 调用代理商 %s 失败: %w", p.Name(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("network: 代理商 %s 返回 HTTP %d: %s", p.Name(), resp.StatusCode, truncate(string(raw), 200))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("network: 代理商 %s 返回非 JSON: %s", p.Name(), truncate(string(raw), 120))
	}
	return nil
}

// collectObjects 递归找出响应里所有含 host/ip 字段的对象。
// 厂商的返回形态差异极大（{data:[...]}、{data:{list:[...]}}、{result:{proxy:{...}}}…），
// 与其为每个厂商写分支，不如宽容地找真正的代理节点。
func collectObjects(v any) []map[string]any {
	out := []map[string]any{}
	switch t := v.(type) {
	case map[string]any:
		if looksLikeProxy(t) {
			out = append(out, t)
		}
		for _, val := range t {
			out = append(out, collectObjects(val)...)
		}
	case []any:
		for _, item := range t {
			out = append(out, collectObjects(item)...)
		}
	}
	return out
}

// hostKeys / portKeys 厂商字段别名表。
// 实测教训：只认 `port` 会让第 2、3 个环境直接分配失败——厂商返回的是
// http_port / socks_port 这类别名（尤其是同时提供多协议的代理商），
// 单元测试的假数据往往太规整，必须在解析层而不是测试层兜住这种差异。
var hostKeys = []string{"host", "ip", "hostname", "server", "proxy_host", "ip_address"}
var portKeys = []string{"port", "http_port", "https_port", "socks_port", "socks5_port", "proxy_port", "port_num"}

func looksLikeProxy(m map[string]any) bool {
	hasHost, hasPort := false, false
	for _, k := range hostKeys {
		if _, ok := m[k]; ok {
			hasHost = true
			break
		}
	}
	for _, k := range portKeys {
		if _, ok := m[k]; ok {
			hasPort = true
			break
		}
	}
	return hasHost && hasPort
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				if strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			case float64:
				return strconvItoa(int(s))
			}
		}
	}
	return ""
}

func intField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return int(n)
			case string:
				v2 := 0
				for _, c := range n {
					if c < '0' || c > '9' {
						return 0
					}
					v2 = v2*10 + int(c-'0')
				}
				return v2
			}
		}
	}
	return 0
}

// parseVendorTime 解析到期时间；厂商格式五花八门，认不出来时不报错，
// 由上层按默认租约兜底。
func parseVendorTime(m map[string]any) time.Time {
	for _, k := range []string{"expire", "expire_time", "expires_at", "expired_at", "deadline"} {
		v := stringField(m, k)
		if v == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00", "2006/01/02 15:04:05"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t
			}
		}
	}
	if secs := intField(m, "ttl", "duration", "expire_seconds"); secs > 0 {
		return time.Now().Add(time.Duration(secs) * time.Second)
	}
	return time.Time{}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	buf := [24]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
