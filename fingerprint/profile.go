// Package fingerprint 设备画像模板库与自洽性引擎（产品护城河之一）。
//
// L1 铁律：指纹必须「自洽」而非「随机」。所有字段（UA / platform / GPU / 屏幕 /
// devicePixelRatio / 时区 / 语言 / Accept-Language / CPU 核数 / 字体）必须从
// **同一份设备画像模板**派生，禁止逐字段独立随机。
//
// L8 铁律：离线必须复用缓存的同一份指纹基线哈希，校验一致后才允许启动。
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
)

// OSInfo 操作系统信息。Platform 必须与 navigator.platform 严格对应。
type OSInfo struct {
	Name     string `json:"name"`     // Windows|macOS|Linux
	Version  string `json:"version"`  // 11|14.4|22.04
	Build    string `json:"build"`    // 10.0.22631|23E224
	Platform string `json:"platform"` // Win32|MacIntel|Linux x86_64
	Arch     string `json:"arch"`     // x86_64|arm64
}

// CPUInfo CPU 信息。
type CPUInfo struct {
	Cores        int    `json:"cores"`
	Architecture string `json:"architecture"`
	Vendor       string `json:"vendor"`
	DeviceMemory int    `json:"device_memory"` // navigator.deviceMemory（GB，Chrome 会向上取整到 2 的幂）
	MemApproxGB  int    `json:"mem_approx_gb"`
}

// ScreenInfo 屏幕信息（分辨率与 DPR 必须成对出现，否则自洽性校验失败）。
type ScreenInfo struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	AvailWidth  int     `json:"avail_width"`
	AvailHeight int     `json:"avail_height"`
	DPR         float64 `json:"dpr"`
	ColorDepth  int     `json:"color_depth"`
	TouchPoints int     `json:"touch_points"`
}

// GPUInfo GPU 信息（WebGL UNMASKED 必须能在 C++ 层注入，JS 层改不了）。
type GPUInfo struct {
	Vendor         string   `json:"vendor"`   // Google Inc. (Apple)
	Renderer       string   `json:"renderer"` // ANGLE (Apple, ANGLE Metal Renderer: Apple M2 Pro...)
	WebGLVendor    string   `json:"webgl_vendor"`
	WebGLRenderer  string   `json:"webgl_renderer"`
	DriverVersions []string `json:"driver_versions"` // 微扰白名单：驱动小版本
	DriverVersion  string   `json:"driver_version"`
}

// UACHInfo User-Agent Client Hints（必须与 UA 的 Chrome 大版本一致）。
type UACHInfo struct {
	Brands          []string `json:"brands"` // "Chromium";v="131", "Not_A Brand";v="24"
	FullVersionList []string `json:"full_version_list"`
	Mobile          bool     `json:"mobile"`
	Platform        string   `json:"platform"` // "Windows"
	PlatformVersion string   `json:"platform_version"`
	Model           string   `json:"model"`
}

// WebRTCInfo WebRTC 策略（IP 泄漏是最高危的指纹破绽之一）。
type WebRTCInfo struct {
	Mode     string `json:"mode"` // disabled|replace_with_proxy|allow
	PublicIP string `json:"public_ip,omitempty"`
}

// Plugin 插件声明。L7/L1：插件列表本身是指纹的一部分，必须整体一致
// （要么体现"装了插件"，要么全部不体现，禁止混用）。
type Plugin struct {
	Name        string   `json:"name"`
	Filename    string   `json:"filename"`
	Description string   `json:"description"`
	MimeTypes   []string `json:"mime_types"`
}

// Profile 一份真实设备的完整画像（模板）。
type Profile struct {
	ID          string     `json:"id"`
	Vendor      string     `json:"vendor"`
	Model       string     `json:"model"`
	OS          OSInfo     `json:"os"`
	CPU         CPUInfo    `json:"cpu"`
	MemoryGB    int        `json:"memory_gb"`
	Screen      ScreenInfo `json:"screen"`
	GPU         GPUInfo    `json:"gpu"`
	Timezone    string     `json:"timezone"`
	UTCOffset   int        `json:"utc_offset"`
	Locale      string     `json:"locale"`
	Languages   []string   `json:"languages"`
	Country     string     `json:"country"`
	City        string     `json:"city"`
	Fonts       []string   `json:"fonts"`
	Plugins     []Plugin   `json:"plugins"`
	UA          string     `json:"ua"`
	UACH        UACHInfo   `json:"uach"`
	WebRTC      WebRTCInfo `json:"webrtc"`
	CanvasSeed  string     `json:"canvas_seed"`
	AudioSeed   string     `json:"audio_seed"`
	MarketShare float64    `json:"market_share"` // 加权采样权重（真实市场占比）
	UsageCount  int        `json:"usage_count"`
}

// AcceptLanguage 派生 Accept-Language 头（必须与 Locale/Languages 同源自洽）。
func (p Profile) AcceptLanguage() string {
	if len(p.Languages) == 0 {
		return p.Locale
	}
	out := []string{}
	for i, l := range p.Languages {
		if i == 0 {
			out = append(out, l)
			continue
		}
		out = append(out, l+";q="+qValue(i))
	}
	return strings.Join(out, ", ")
}

func qValue(i int) string {
	switch i {
	case 1:
		return "0.9"
	case 2:
		return "0.8"
	default:
		return "0.7"
	}
}

// ChromeMajor 从 UA 解析 Chrome 大版本（L2：TLS 基线必须与之对应）。
func (p Profile) ChromeMajor() (int, error) {
	return chromeMajorFromUA(p.UA)
}

// Hash 环境基线哈希：离线启动时校验一致才允许启动（L8）。
func (p Profile) Hash() string {
	b, err := json.Marshal(struct {
		OS        OSInfo     `json:"os"`
		CPU       CPUInfo    `json:"cpu"`
		Screen    ScreenInfo `json:"screen"`
		GPU       GPUInfo    `json:"gpu"`
		Timezone  string     `json:"timezone"`
		Locale    string     `json:"locale"`
		Languages []string   `json:"languages"`
		Country   string     `json:"country"`
		Fonts     []string   `json:"fonts"`
		Plugins   []Plugin   `json:"plugins"`
		UA        string     `json:"ua"`
		WebRTC    WebRTCInfo `json:"webrtc"`
	}{p.OS, p.CPU, p.Screen, p.GPU, p.Timezone, p.Locale, p.Languages, p.Country, p.Fonts, p.Plugins, p.UA, p.WebRTC})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---------- 模板库 ----------

// Library 设备画像模板库。
type Library struct {
	mu        sync.Mutex
	templates []Profile
	usage     map[string]int
	rng       *rand.Rand
	usageCap  int // 单模板被使用次数上限，超限降权（防止"批量生成的同款"被识别）
}

// LibraryConfig 采样配置（数值来自配置中心，禁止散落硬编码，L10）。
type LibraryConfig struct {
	UsageCap          int // 默认 50：单模板使用次数软上限
	PerturbFonts      bool
	PerturbGPUDriver  bool
	MinPoolPerCountry int // 默认 50：每国家模板池最小规模（不足则告警）
}

// DefaultLibraryConfig 默认配置。
func DefaultLibraryConfig() LibraryConfig {
	return LibraryConfig{UsageCap: 50, PerturbFonts: true, PerturbGPUDriver: true, MinPoolPerCountry: 50}
}

// NewLibrary 构造模板库（内置真实设备画像）。
func NewLibrary(cfg LibraryConfig, seed int64) *Library {
	if cfg.UsageCap <= 0 {
		cfg.UsageCap = 50
	}
	return &Library{templates: BuiltinProfiles(), usage: map[string]int{}, rng: rand.New(rand.NewSource(seed)), usageCap: cfg.UsageCap}
}

// Add 追加模板（供运营后台录入与采集器入库）。
func (l *Library) Add(p Profile) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.templates = append(l.templates, p)
}

// PoolSize 返回指定国家的模板池规模（用于「模板池是否够大」的自检）。
func (l *Library) PoolSize(country string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, p := range l.templates {
		if country == "" || p.Country == country {
			n++
		}
	}
	return n
}

// Sample 按「平台 × 站点」采样一份画像：
//  1. 过滤 OS 家族与国家；
//  2. 按真实市场占比加权；
//  3. 单模板使用次数超软上限则降权（防撞库识别）；
//  4. 仅对白名单字段做熵预算内微扰（字体顺序 / GPU 驱动小版本 / Canvas+Audio 噪声种子）。
func (l *Library) Sample(country, osFamily string) (*Profile, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cands := []Profile{}
	for _, p := range l.templates {
		if country != "" && p.Country != country {
			continue
		}
		if osFamily != "" && !strings.EqualFold(p.OS.Name, osFamily) {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("fingerprint: 无匹配画像（country=%s os=%s）", country, osFamily)
	}
	weights := make([]float64, len(cands))
	total := 0.0
	for i, p := range cands {
		w := p.MarketShare
		if w <= 0 {
			w = 1
		}
		// 使用次数降权：usage/cap 越大权重越低（软上限，不是硬拒绝）
		w = w / (1 + float64(l.usage[p.ID])/float64(l.usageCap))
		weights[i] = w
		total += w
	}
	pick := l.rng.Float64() * total
	idx := len(cands) - 1
	for i, w := range weights {
		pick -= w
		if pick <= 0 {
			idx = i
			break
		}
	}
	out := cands[idx]
	l.usage[out.ID]++
	out.UsageCount = l.usage[out.ID]
	out.ID = fmt.Sprintf("%s-%s", out.ID, idsShort(l.rng))
	perturb(&out, l.rng)
	return &out, nil
}

// perturb 熵预算内微扰：只允许改「不影响自洽性」的字段。
func perturb(p *Profile, r *rand.Rand) {
	p.CanvasSeed = randomHex(r, 16)
	p.AudioSeed = randomHex(r, 16)
	if len(p.GPU.DriverVersions) > 0 {
		p.GPU.DriverVersion = p.GPU.DriverVersions[r.Intn(len(p.GPU.DriverVersions))]
	}
	if len(p.Fonts) > 4 {
		// 只 shuffle 尾部字体（前 4 个是 OS 标志性字体，必须保持稳定）
		tail := append([]string{}, p.Fonts[4:]...)
		r.Shuffle(len(tail), func(i, j int) { tail[i], tail[j] = tail[j], tail[i] })
		head := append([]string{}, p.Fonts[:4]...)
		p.Fonts = append(head, tail...)
	}
}

func randomHex(r *rand.Rand, n int) string {
	const chars = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = chars[r.Intn(len(chars))]
	}
	return string(out)
}

func idsShort(r *rand.Rand) string {
	const chars = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	out := make([]byte, 8)
	for i := range out {
		out[i] = chars[r.Intn(len(chars))]
	}
	return string(out)
}

// SortFonts 稳定排序（导出用于测试断言字体集合一致性）。
func SortFonts(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
