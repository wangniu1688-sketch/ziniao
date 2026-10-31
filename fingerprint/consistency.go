package fingerprint

import (
	"strconv"
	"strings"
)

// Severity 自洽性问题等级。
type Severity string

const (
	SeverityCritical Severity = "critical" // 必然被平台识别为异常
	SeverityMajor    Severity = "major"    // 大概率被风控标记
	SeverityMinor    Severity = "minor"    // 轻微不合理
)

// penalty 扣分权重（总分 100，扣完为 0）。
var penalty = map[Severity]int{SeverityCritical: 25, SeverityMajor: 10, SeverityMinor: 4}

// Issue 一条自洽性问题。
type Issue struct {
	Rule     string   `json:"rule"`
	Field    string   `json:"field"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// Report 自洽性体检报告（M0.3 环境自检页的数据源）。
type Report struct {
	Score      int      `json:"score"` // 0-100
	Passed     bool     `json:"passed"`
	Threshold  int      `json:"threshold"`
	Issues     []Issue  `json:"issues"`
	Unverified []string `json:"unverified,omitempty"` // 因缺少外部观测值而无法判定的规则
}

// CheckContext 校验上下文：运行时真实观测值（IP 归属地、TLS 指纹、内核版本）。
type CheckContext struct {
	IPCountry    string
	ObservedUA   string
	ObservedJA3  string
	ObservedJA4  string
	KernelMajor  int
	TLS          *TLSBaseline
	ProxyCountry string // 代理出口国（应与时区/语言一致）
}

// Rule 一条自洽性规则。
type Rule struct {
	ID       string
	Field    string
	Severity Severity
	Desc     string
	Check    func(p *Profile, c CheckContext) (bool, string, bool)
}

// Engine 自洽性引擎。
type Engine struct {
	rules     []Rule
	threshold int
}

// NewEngine 构造引擎（默认规则集）。
func NewEngine() *Engine { return &Engine{rules: DefaultRules(), threshold: 85} }

// WithThreshold 设置通过阈值（配置中心可调）。
func (e *Engine) WithThreshold(t int) *Engine {
	if t > 0 && t <= 100 {
		e.threshold = t
	}
	return e
}

// Rules 暴露规则集（供自检页展示）。
func (e *Engine) Rules() []Rule { return e.rules }

// Evaluate 对一份画像做体检。
func (e *Engine) Evaluate(p *Profile, c CheckContext) Report {
	if p == nil {
		return Report{Threshold: e.threshold}
	}
	score := 100
	issues := []Issue{}
	unverified := []string{}
	for _, r := range e.rules {
		ok, msg, checked := r.Check(p, c)
		if !checked {
			unverified = append(unverified, r.ID)
			continue
		}
		if !ok {
			issues = append(issues, Issue{Rule: r.ID, Field: r.Field, Severity: r.Severity, Message: msg})
			score -= penalty[r.Severity]
		}
	}
	if score < 0 {
		score = 0
	}
	return Report{Score: score, Passed: score >= e.threshold, Threshold: e.threshold, Issues: issues, Unverified: unverified}
}

// DefaultRules 自洽性校验规则集（对应规格「自洽性校验规则集」）。
func DefaultRules() []Rule {
	return []Rule{
		{
			ID: "ua-os", Field: "ua", Severity: SeverityCritical, Desc: "UA 中的操作系统必须与画像 OS 一致",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				ua := strings.ToLower(p.UA)
				switch p.OS.Name {
				case "Windows":
					if !strings.Contains(ua, "windows nt 10.0") {
						return false, "UA 未声明 Windows NT 10.0", true
					}
				case "macOS":
					if !strings.Contains(ua, "macintosh") || !strings.Contains(ua, "mac os x") {
						return false, "UA 未声明 Macintosh / Mac OS X", true
					}
				case "Linux":
					if !strings.Contains(ua, "x11; linux") {
						return false, "UA 未声明 X11; Linux", true
					}
				}
				return true, "", true
			},
		},
		{
			ID: "platform-os", Field: "os.platform", Severity: SeverityCritical, Desc: "navigator.platform 必须与 OS 对应",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				want := map[string]string{"Windows": "Win32", "macOS": "MacIntel", "Linux": "Linux x86_64"}[p.OS.Name]
				if p.OS.Platform != want {
					return false, "platform=" + p.OS.Platform + " 与 OS=" + p.OS.Name + " 不匹配", true
				}
				return true, "", true
			},
		},
		{
			ID: "uach-ua", Field: "uach.brands", Severity: SeverityMajor, Desc: "UA-CH 的 Chromium 大版本必须与 UA 一致",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				major, err := p.ChromeMajor()
				if err != nil {
					return false, "UA 无法解析 Chrome 版本", true
				}
				joined := strings.Join(p.UACH.Brands, " ")
				if !strings.Contains(joined, `Chromium";v="`+strconv.Itoa(major)) {
					return false, "UA-CH 未声明 Chromium v" + strconv.Itoa(major), true
				}
				return true, "", true
			},
		},
		{
			ID: "tz-country", Field: "timezone", Severity: SeverityCritical, Desc: "时区所属国家必须与 IP/代理出口国一致",
			Check: func(p *Profile, c CheckContext) (bool, string, bool) {
				want := p.Country
				if c.ProxyCountry != "" {
					want = c.ProxyCountry
				} else if c.IPCountry != "" {
					want = c.IPCountry
				}
				got, ok := tzCountry[p.Timezone]
				if !ok {
					return false, "未知时区: " + p.Timezone, true
				}
				if got != want {
					return false, "时区 " + p.Timezone + " 属 " + got + "，但出口国为 " + want, true
				}
				return true, "", true
			},
		},
		{
			ID: "tz-offset", Field: "utc_offset", Severity: SeverityMajor, Desc: "UTC 偏移必须与时区一致",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				want, ok := tzOffset[p.Timezone]
				if !ok {
					return false, "无该时区的偏移基线", true
				}
				if p.UTCOffset != want {
					return false, "偏移 " + strconv.Itoa(p.UTCOffset) + " 与基线 " + strconv.Itoa(want) + " 不符", true
				}
				return true, "", true
			},
		},
		{
			ID: "lang-country", Field: "languages", Severity: SeverityCritical, Desc: "主语言地区变体必须与国家一致",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				if len(p.Languages) == 0 {
					return false, "未设置语言", true
				}
				region := ""
				if parts := strings.Split(p.Languages[0], "-"); len(parts) == 2 {
					region = parts[1]
				}
				if region != p.Country {
					return false, "主语言 " + p.Languages[0] + " 与国家 " + p.Country + " 不一致", true
				}
				if p.Locale != p.Languages[0] {
					return false, "Locale 与主语言不一致", true
				}
				return true, "", true
			},
		},
		{
			ID: "accept-language", Field: "languages", Severity: SeverityMinor, Desc: "Accept-Language 首项必须是主语言",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				if !strings.HasPrefix(p.AcceptLanguage(), p.Languages[0]) {
					return false, "Accept-Language 首项异常", true
				}
				return true, "", true
			},
		},
		{
			ID: "screen-dpr", Field: "screen", Severity: SeverityMajor, Desc: "分辨率、DPR 与可用高度必须自洽",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				s := p.Screen
				if s.DPR < 1 || s.DPR > 3 {
					return false, "DPR 超出合理范围", true
				}
				if s.Width <= 0 || s.Height <= 0 {
					return false, "分辨率非法", true
				}
				if s.AvailHeight >= s.Height {
					return false, "可用高度应小于屏幕高度（任务栏/菜单栏）", true
				}
				// 高 DPR 常见于 macOS 视网膜屏
				if s.DPR >= 2 && p.OS.Name != "macOS" {
					return false, "非 macOS 却使用 DPR>=2，容易被识别", true
				}
				return true, "", true
			},
		},
		{
			ID: "gpu-os", Field: "gpu", Severity: SeverityCritical, Desc: "GPU 厂商必须与 OS 匹配（Apple GPU 只出现在 macOS）",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				r := strings.ToLower(p.GPU.Renderer)
				switch p.OS.Name {
				case "macOS":
					if !strings.Contains(r, "apple") && !strings.Contains(r, "metal") {
						return false, "macOS 却使用非 Apple GPU", true
					}
				case "Windows":
					if !strings.Contains(r, "direct3d11") && !strings.Contains(r, "direct3d12") {
						return false, "Windows 的 ANGLE 渲染器应带 Direct3D", true
					}
				case "Linux":
					if !strings.Contains(r, "mesa") && !strings.Contains(r, "opengl") {
						return false, "Linux 的渲染器应带 Mesa/OpenGL", true
					}
				}
				return true, "", true
			},
		},
		{
			ID: "cpu-memory-tier", Field: "cpu", Severity: SeverityMajor, Desc: "CPU 核数与内存档位必须匹配真实机型档位",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				tiers := map[int][]int{
					2:  {4, 8},
					4:  {4, 8, 16},
					6:  {8, 16, 32},
					8:  {8, 16, 32, 64},
					10: {16, 32, 64},
					12: {16, 32, 64},
					16: {32, 64},
				}
				allowed, ok := tiers[p.CPU.Cores]
				if !ok {
					return false, "CPU 核数不在已知档位", true
				}
				for _, m := range allowed {
					if m == p.MemoryGB {
						if p.CPU.DeviceMemory < 1 || p.CPU.DeviceMemory > p.MemoryGB {
							return false, "deviceMemory 与物理内存不符", true
						}
						return true, "", true
					}
				}
				return false, "核数 " + strconv.Itoa(p.CPU.Cores) + " 与内存 " + strconv.Itoa(p.MemoryGB) + "GB 组合不真实", true
			},
		},
		{
			ID: "fonts-os", Field: "fonts", Severity: SeverityMajor, Desc: "字体集必须包含该 OS 的标志性字体",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				marker := map[string]string{"Windows": "Segoe UI", "macOS": "Helvetica Neue", "Linux": "DejaVu Sans"}[p.OS.Name]
				for _, f := range p.Fonts {
					if f == marker {
						return true, "", true
					}
				}
				return false, "缺少 " + p.OS.Name + " 标志字体 " + marker, true
			},
		},
		{
			ID: "webrtc-leak", Field: "webrtc", Severity: SeverityCritical, Desc: "WebRTC 公网 IP 必须与出口国一致，或明确禁用/替换",
			Check: func(p *Profile, c CheckContext) (bool, string, bool) {
				switch p.WebRTC.Mode {
				case "disabled", "replace_with_proxy":
					return true, "", true
				case "allow":
					if p.WebRTC.PublicIP == "" {
						return false, "允许 WebRTC 但未指定出口 IP", true
					}
					// 出口 IP 归属国需与画像国家一致（此处仅做非空与模式判定，
					// 真实归属由 network 模块的 IP 纯净度检测给出）
					return true, "", true
				}
				return false, "未知 WebRTC 模式: " + p.WebRTC.Mode, true
			},
		},
		{
			ID: "tls-kernel", Field: "tls", Severity: SeverityCritical, Desc: "TLS 基线对应的内核大版本必须与 UA 声明一致（L2）",
			Check: func(p *Profile, c CheckContext) (bool, string, bool) {
				if c.TLS == nil {
					return false, "", false // 无基线数据：无法判定
				}
				major, err := p.ChromeMajor()
				if err != nil {
					return false, "UA 无法解析 Chrome 版本", true
				}
				if c.TLS.ChromiumMajor != major {
					return false, "内核基线 v" + strconv.Itoa(c.TLS.ChromiumMajor) + " 与 UA Chrome v" + strconv.Itoa(major) + " 不对应", true
				}
				return true, "", true
			},
		},
		{
			ID: "ja3-match", Field: "tls.ja3", Severity: SeverityCritical, Desc: "实测 JA3 必须与基线一致（L2）",
			Check: func(p *Profile, c CheckContext) (bool, string, bool) {
				if c.ObservedJA3 == "" || c.TLS == nil || c.TLS.JA3 == "" {
					return false, "", false
				}
				if c.ObservedJA3 != c.TLS.JA3 {
					return false, "实测 JA3 与基线不符", true
				}
				return true, "", true
			},
		},
		{
			ID: "plugins-consistent", Field: "plugins", Severity: SeverityMajor, Desc: "插件列表必须整体一致（禁止混用第三方插件）",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				if len(p.Plugins) == 0 {
					return false, "插件列表为空（真实 Chrome 至少有内置 PDF 插件）", true
				}
				for _, pl := range p.Plugins {
					if pl.Filename != "internal-pdf-viewer" {
						return false, "存在非内置插件 " + pl.Name + "，需在所有环境统一体现或不体现", true
					}
				}
				return true, "", true
			},
		},
		{
			ID: "color-depth", Field: "screen.color_depth", Severity: SeverityMinor, Desc: "色深必须与 OS 常见值一致",
			Check: func(p *Profile, _ CheckContext) (bool, string, bool) {
				want := 24
				if p.OS.Name == "macOS" {
					want = 30
				}
				if p.Screen.ColorDepth != want {
					return false, "色深 " + strconv.Itoa(p.Screen.ColorDepth) + " 与 " + p.OS.Name + " 常见值 " + strconv.Itoa(want) + " 不符", true
				}
				return true, "", true
			},
		},
	}
}

// chromeMajorFromUA 解析 UA 中的 Chrome 大版本。
func chromeMajorFromUA(ua string) (int, error) {
	i := strings.Index(ua, "Chrome/")
	if i < 0 {
		return 0, errNoChrome
	}
	rest := ua[i+len("Chrome/"):]
	end := strings.IndexAny(rest, " .")
	if end < 0 {
		end = len(rest)
	}
	return strconv.Atoi(rest[:end])
}

var errNoChrome = &uaErr{}

type uaErr struct{}

func (e *uaErr) Error() string { return "UA 中未找到 Chrome 版本" }

// tzCountry 时区 → 国家（内置基线，覆盖主要站点国家）。
var tzCountry = map[string]string{
	"America/New_York":    "US",
	"America/Los_Angeles": "US",
	"America/Chicago":     "US",
	"America/Denver":      "US",
	"America/Sao_Paulo":   "BR",
	"Europe/London":       "GB",
	"Europe/Berlin":       "DE",
	"Europe/Paris":        "FR",
	"Europe/Madrid":       "ES",
	"Europe/Rome":         "IT",
	"Asia/Tokyo":          "JP",
	"Asia/Seoul":          "KR",
	"Asia/Shanghai":       "CN",
	"Asia/Singapore":      "SG",
	"Asia/Bangkok":        "TH",
	"Asia/Jakarta":        "ID",
	"Asia/Dubai":          "AE",
	"Australia/Sydney":    "AU",
}

// tzOffset 时区 → 标准 UTC 偏移（小时，不含夏令时）。
var tzOffset = map[string]int{
	"America/New_York": -5, "America/Los_Angeles": -8, "America/Chicago": -6, "America/Denver": -7,
	"America/Sao_Paulo": -3, "Europe/London": 0, "Europe/Berlin": 1, "Europe/Paris": 1,
	"Europe/Madrid": 1, "Europe/Rome": 1, "Asia/Tokyo": 9, "Asia/Seoul": 9, "Asia/Shanghai": 8,
	"Asia/Singapore": 8, "Asia/Bangkok": 7, "Asia/Jakarta": 7, "Asia/Dubai": 4, "Australia/Sydney": 10,
}
