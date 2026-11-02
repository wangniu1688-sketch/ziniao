package fingerprint

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ScriptVersion 注入脚本的版本号。
//
// 用途：环境落库后脚本就固定了，加固（例如 P1 的「Date 时区串 / Intl locale」）
// 不会自动跑到老环境上。启动链路据此判断库里的脚本是不是旧版，是则按快照重建。
// 每改一次注入脚本的覆盖能力就 +1，不要复用旧号。
const ScriptVersion = 2

// Layer 指纹字段的注入层。
type Layer string

const (
	LayerCPP     Layer = "cpp"     // Chromium C++ 渲染层（防 JS 检测）
	LayerJS      Layer = "js"      // 文档创建前 JS 注入（可被检测，但主流站点够用）
	LayerNetwork Layer = "network" // 网络层：TLS/HTTP2 指纹与出口 IP
)

// FieldLayer 「字段 → 注入层 → 可检测性」对照表（主提示词 Q3）。
type FieldLayer struct {
	Field      string `json:"field"`
	Layer      Layer  `json:"layer"`
	Detectable bool   `json:"detectable"` // 是否可能被 JS 侧检出
	Injectable bool   `json:"injectable"` // 当前实现是否已落地
	Note       string `json:"note"`
}

// LayerTable 完整对照表（Phase 1 现状：JS + Network 已落地，C++ 为 TODO）。
func LayerTable() []FieldLayer {
	return []FieldLayer{
		{Field: "navigator.webdriver", Layer: LayerJS, Detectable: true, Injectable: true, Note: "document_start 删除自有属性并把 Navigator.prototype.webdriver 改为返回 undefined；漏改时 Sannysoft 直接判 WebDriver present (failed)"},
		{Field: "navigator.userAgent", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "UA 在 JS 层可覆盖，但 UA-CH/HTTP 头不一致会暴露；正式实现应在 C++ 层与网络层同源"},
		{Field: "navigator.userAgentData", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "brands/platform/platformVersion 必须与 UA 的 Chrome 大版本与画像 OS 同源，禁止硬编码 Windows"},
		{Field: "navigator.platform", Layer: LayerJS, Detectable: false, Injectable: true, Note: "与 UA 的 OS 家族严格对应：Windows=Win32、macOS=MacIntel、Linux=Linux x86_64"},
		{Field: "navigator.hardwareConcurrency", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "navigator.deviceMemory", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "navigator.languages", Layer: LayerJS, Detectable: false, Injectable: true, Note: "按画像国家派生（US=en-US,en；JP=ja-JP,ja,en-US），与 Accept-Language 同源"},
		{Field: "navigator.plugins", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "JS 可伪造 MimeTypeArray；插件列表必须与画像整体一致（反例 5）"},
		{Field: "screen.width/height/dpr", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "JS 覆盖 screen 对象；窗口真实尺寸由 C++ 层保证"},
		{Field: "WebGL vendor/renderer(UNMASKED)", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "必须 C++ 层注入；JS 层 hook getParameter 对多数检测站有效但不是强保证"},
		{Field: "Canvas 噪声", Layer: LayerJS, Detectable: false, Injectable: true, Note: "按 canvas_seed 派生的位置确定性扰动，同环境可复现、跨环境必不同"},
		{Field: "AudioContext 噪声", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "字体列表", Layer: LayerCPP, Detectable: true, Injectable: false, Note: "TODO(P1)：OS 层 fontconfig/DirectWrite 注入；当前仅做画像一致性校验"},
		{Field: "Intl/时区/语言", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "覆盖 Intl 各构造器的 resolvedOptions（locale/timeZone）、Date#getTimezoneOffset/toLocale* 与 Date#toString/toTimeString 的时区串"},
		{Field: "WebRTC IP", Layer: LayerCPP, Detectable: true, Injectable: false, Note: "TODO(P1)：C++ 层 IP 处理策略；网络层用出口代理兜底"},
		{Field: "JA3/JA4/HTTP2 SETTINGS", Layer: LayerNetwork, Detectable: true, Injectable: true, Note: "uTLS 前置代理重建 ClientHello（已实现）"},
		{Field: "IP 归属地/ASN", Layer: LayerNetwork, Detectable: true, Injectable: true, Note: "出口代理 + 纯净度四维检测（已实现）"},
	}
}

// InjectionPlan 注入计划：客户端启动环境时下发的全部内容。
type InjectionPlan struct {
	EnvID        string            `json:"env_id"`
	BaselineHash string            `json:"baseline_hash"` // L8：离线启动必须校验一致
	KernelMajor  int               `json:"kernel_major"`
	TLSSpec      *TLSBaseline      `json:"tls,omitempty"`
	Script       string            `json:"script"` // 文档创建前注入的 JS
	Layers       []FieldLayer      `json:"layers"`
	Profile      *Profile          `json:"profile,omitempty"` // L4：不含任何凭证字段
	Terminal     map[string]string `json:"terminal,omitempty"`
}

// BuildInjectionPlan 生成注入计划（客户端启动环境时调用）。
func BuildInjectionPlan(envID string, p *Profile, tls *TLSBaseline) (*InjectionPlan, error) {
	major, err := p.ChromeMajor()
	if err != nil {
		return nil, err
	}
	return &InjectionPlan{
		EnvID:        envID,
		BaselineHash: p.Hash(),
		KernelMajor:  major,
		TLSSpec:      tls,
		Script:       InjectionScript(p),
		Layers:       LayerTable(),
		Profile:      p,
	}, nil
}

// InjectionScript 生成「文档创建前注入」的 JS（CDP: Page.addScriptToEvaluateOnNewDocument）。
//
// 原则（L1）：所有值来自同一份画像，禁止在脚本内随机生成。
// 原则（时序）：必须在文档创建前注入，否则会被检测出「先有真实值后被改」的时序破绽。
// 原则（体系一致）：UA / platform / UA-CH / WebGL / screen / 语言 / 时区必须同出一源，
// 出现「UA 说 Mac、platform 说 Win32」这类混搭 = 主动告诉检测站指纹被改过。
func InjectionScript(p *Profile) string {
	if p == nil {
		return ""
	}
	return renderInjectScript(injectConfigFromProfile(p))
}

// ScriptFromSnapshot 历史环境补脚本（库里 inject_script 为空的老数据）。
//
// 不能退回「只写个标记」的空壳脚本——那等于不注入，页面里全是本机真实指纹。
// 做法：快照里有的字段一律以快照为准（保证与已落库的基线哈希、漂移检测一致），
// 快照里没有的字段（如 UNMASKED GPU 长串、插件列表）用 profile_id 反查模板补齐。
func ScriptFromSnapshot(profileID string, values map[string]string) string {
	cfg := injectConfigFromSnapshot(values)
	if tpl, ok := LookupTemplate(profileID); ok {
		cfg.fillFromTemplate(tpl)
	}
	return renderInjectScript(cfg)
}

// uachBrand UA-CH 的一个 brand 条目。
type uachBrand struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

// injectScreen 注入用的屏幕参数。
type injectScreen struct {
	Width       int `json:"width"`
	Height      int `json:"height"`
	AvailWidth  int `json:"availWidth"`
	AvailHeight int `json:"availHeight"`
	ColorDepth  int `json:"colorDepth"`
	PixelDepth  int `json:"pixelDepth"`
}

// injectConfig 注入脚本的全部输入（由画像或快照派生，脚本内不做任何随机）。
type injectConfig struct {
	UA                  string       `json:"ua"`
	AppVersion          string       `json:"appVersion"`
	Platform            string       `json:"platform"`
	Vendor              string       `json:"vendor"`
	UACHBrands          []uachBrand  `json:"uachBrands"`
	UACHFullVersionList []uachBrand  `json:"uachFullVersionList"`
	UACHFullVersion     string       `json:"uachFullVersion"`
	UACHPlatform        string       `json:"uachPlatform"`
	UACHPlatformVersion string       `json:"uachPlatformVersion"`
	UACHArch            string       `json:"uachArch"`
	UACHBitness         string       `json:"uachBitness"`
	UACHModel           string       `json:"uachModel"`
	UACHMobile          bool         `json:"uachMobile"`
	HardwareConcurrency int          `json:"hardwareConcurrency"`
	DeviceMemory        int          `json:"deviceMemory"`
	Language            string       `json:"language"`
	Languages           []string     `json:"languages"`
	Timezone            string       `json:"timezone"`
	UTCOffsetMinutes    int          `json:"utcOffsetMinutes"`
	// TZLabel Date#toString 括号里的时区名（"日本標準時"）。由画像时区 × 语种派生，
	// 缺了它就会出现「Intl 说 Asia/Tokyo、字符串却写着中国标准时间」的自曝。
	TZLabel string `json:"tzLabel"`
	WebGLVendor         string       `json:"webglVendor"`
	WebGLRenderer       string       `json:"webglRenderer"`
	UnmaskedVendor      string       `json:"unmaskedVendor"`
	UnmaskedRenderer    string       `json:"unmaskedRenderer"`
	WebGLVersion        string       `json:"webglVersion"`
	WebGL2Version       string       `json:"webgl2Version"`
	GLSLVersion         string       `json:"glslVersion"`
	GLSL2Version        string       `json:"glsl2Version"`
	CanvasSeed          string       `json:"canvasSeed"`
	AudioSeed           string       `json:"audioSeed"`
	Plugins             []Plugin     `json:"plugins"`
	MaxTouchPoints      int          `json:"maxTouchPoints"`
	WebRTCMode          string       `json:"webRTCMode"`
	Screen              injectScreen `json:"screen"`
	DPR                 float64      `json:"dpr"`
}

func injectConfigFromProfile(p *Profile) injectConfig {
	major, err := p.ChromeMajor()
	if err != nil || major <= 0 {
		major = 120
	}
	full := p.ChromeFullVersion
	if full == "" {
		full = chromeFullVersionFromUA(p.UA, major)
	}
	// Not_A Brand 的小版本随 Chrome 大版本变（131 起是 24，老版本是 8）。
	notA := "8"
	if major >= 125 {
		notA = "24"
	}
	langs := append([]string{}, p.Languages...)
	language := p.Locale
	if language == "" && len(langs) > 0 {
		language = langs[0]
	}
	if len(langs) == 0 && language != "" {
		langs = []string{language}
	}
	plugins := p.Plugins
	if len(plugins) == 0 {
		plugins = defaultPlugins()
	}
	return injectConfig{
		UA:                  p.UA,
		AppVersion:          appVersionFromUA(p.UA),
		Platform:            p.OS.Platform,
		Vendor:              browserVendor(p),
		UACHBrands:          []uachBrand{{Brand: "Not_A Brand", Version: notA}, {Brand: "Chromium", Version: strconv.Itoa(major)}, {Brand: "Google Chrome", Version: strconv.Itoa(major)}},
		UACHFullVersionList: []uachBrand{{Brand: "Not_A Brand", Version: "8.0.0.0"}, {Brand: "Chromium", Version: full}, {Brand: "Google Chrome", Version: full}},
		UACHFullVersion:     full,
		UACHPlatform:        uachPlatformFromPlatform(p.OS.Platform),
		UACHPlatformVersion: uachPlatformVersionFromOS(p.OS.Name, p.OS.Version),
		UACHArch:            uachArch(p),
		UACHBitness:         "64",
		UACHModel:           "",
		UACHMobile:          false,
		HardwareConcurrency: p.CPU.Cores,
		DeviceMemory:        p.CPU.DeviceMemory,
		Language:            language,
		Languages:           langs,
		Timezone:            p.Timezone,
		UTCOffsetMinutes:    p.UTCOffset * 60,
		TZLabel:             tzLabel(p.Timezone, language),
		WebGLVendor:         p.GPU.WebGLVendor,
		WebGLRenderer:       p.GPU.WebGLRenderer,
		UnmaskedVendor:      p.GPU.Vendor,
		UnmaskedRenderer:    p.GPU.Renderer,
		WebGLVersion:        "WebGL 1.0 (OpenGL ES 2.0 Chromium)",
		WebGL2Version:       "WebGL 2.0 (OpenGL ES 3.0 Chromium)",
		GLSLVersion:         "WebGL GLSL ES 1.0 (OpenGL ES GLSL ES 1.0 Chromium)",
		GLSL2Version:        "WebGL GLSL ES 3.0 (OpenGL ES GLSL ES 3.0 Chromium)",
		CanvasSeed:          p.CanvasSeed,
		AudioSeed:           p.AudioSeed,
		Plugins:             pluginsCopy(plugins),
		MaxTouchPoints:      p.Screen.TouchPoints,
		WebRTCMode:          p.WebRTC.Mode,
		Screen: injectScreen{
			Width:       p.Screen.Width,
			Height:      p.Screen.Height,
			AvailWidth:  p.Screen.AvailWidth,
			AvailHeight: p.Screen.AvailHeight,
			ColorDepth:  p.Screen.ColorDepth,
			PixelDepth:  p.Screen.ColorDepth,
		},
		DPR: p.Screen.DPR,
	}
}

// appVersionFromUA navigator.appVersion = UA 去掉 "Mozilla/" 前缀。
func appVersionFromUA(ua string) string {
	return strings.TrimPrefix(ua, "Mozilla/")
}

// chromeFullVersionFromUA 取 UA 里的 Chrome 完整版本号（如 131.0.0.0）。
func chromeFullVersionFromUA(ua string, major int) string {
	i := strings.Index(ua, "Chrome/")
	if i < 0 {
		return strconv.Itoa(major) + ".0.0.0"
	}
	rest := ua[i+len("Chrome/"):]
	end := strings.IndexAny(rest, " ")
	if end < 0 {
		end = len(rest)
	}
	if rest[:end] == "" {
		return strconv.Itoa(major) + ".0.0.0"
	}
	return rest[:end]
}

// uachPlatformFromPlatform navigator.userAgentData.platform（"Windows"/"macOS"/"Linux"）。
//
// 必须从 navigator.platform 派生而不是从 OS.Name：两者一旦分歧，检测站交叉比对
// UA / platform / sec-ch-ua-platform 就能立刻定位到「指纹是拼出来的」。
func uachPlatformFromPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "macintel", "mac68k", "macppc", "macos":
		return "macOS"
	case "linux x86_64", "linux i686", "linux":
		return "Linux"
	default:
		return "Windows"
	}
}

// splitOS 拆 "macOS 14.4" → ("macOS","14.4")；无法拆分时 version 为空。
func splitOS(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " "); i > 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

// uachPlatformVersionFromOS 必须与画像 OS 版本对应（Win11=15.0.0、macOS 14.4=14.4.0）。
func uachPlatformVersionFromOS(name, version string) string {
	switch name {
	case "Windows":
		if strings.HasPrefix(version, "11") {
			return "15.0.0"
		}
		return "10.0.0"
	case "macOS":
		if version != "" {
			return version + ".0"
		}
		return "14.0.0"
	default:
		return "6.5.0"
	}
}

// uachArch sec-ch-ua-arch：x86_64 → x86，arm64 → arm。
func uachArch(p *Profile) string {
	arch := p.CPU.Architecture
	if arch == "" {
		arch = p.OS.Arch
	}
	if strings.Contains(strings.ToLower(arch), "arm") {
		return "arm"
	}
	return "x86"
}

// pluginsCopy 浅拷贝插件集，避免脚本与画像共享底层数组。
func pluginsCopy(in []Plugin) []Plugin {
	out := make([]Plugin, 0, len(in))
	for _, p := range in {
		out = append(out, Plugin{
			Name:        p.Name,
			Filename:    p.Filename,
			Description: p.Description,
			MimeTypes:   append([]string{}, p.MimeTypes...),
		})
	}
	return out
}

// injectConfigFromSnapshot 从基线快照重建配置（老环境兜底）。
//
// 快照里没有 UNMASKED GPU 长串与插件集，这两项留给 fillFromTemplate 补齐；
// 其余字段一律以快照为准，保证与已落库的基线快照一致（不引入无谓漂移）。
func injectConfigFromSnapshot(v map[string]string) injectConfig {
	if v == nil {
		v = map[string]string{}
	}
	cfg := injectConfig{
		UA:                  v["ua"],
		AppVersion:          appVersionFromUA(v["ua"]),
		Platform:            v["platform"],
		Vendor:              "Google Inc.",
		UACHBitness:         "64",
		UACHModel:           "",
		UACHMobile:          false,
		HardwareConcurrency: atoiOr(v["hardware_concurrency"], 8),
		DeviceMemory:        atoiOr(v["device_memory"], 8),
		Language:            v["locale"],
		Timezone:            v["timezone"],
		UTCOffsetMinutes:    atoiOr(v["utc_offset"], 0) * 60,
		WebGLVendor:         v["webgl_vendor"],
		WebGLRenderer:       v["webgl_renderer"],
		UnmaskedVendor:      "",
		UnmaskedRenderer:    "",
		WebGLVersion:        "WebGL 1.0 (OpenGL ES 2.0 Chromium)",
		WebGL2Version:       "WebGL 2.0 (OpenGL ES 3.0 Chromium)",
		GLSLVersion:         "WebGL GLSL ES 1.0 (OpenGL ES GLSL ES 1.0 Chromium)",
		GLSL2Version:        "WebGL GLSL ES 3.0 (OpenGL ES GLSL ES 3.0 Chromium)",
		CanvasSeed:          v["canvas_seed"],
		AudioSeed:           v["audio_seed"],
		Plugins:             nil,
		MaxTouchPoints:      0,
		WebRTCMode:          "",
	}
	if langs := strings.TrimSpace(v["languages"]); langs != "" {
		for _, l := range strings.Split(langs, ",") {
			if l = strings.TrimSpace(l); l != "" {
				cfg.Languages = append(cfg.Languages, l)
			}
		}
	}
	if cfg.Language == "" && len(cfg.Languages) > 0 {
		cfg.Language = cfg.Languages[0]
	}
	if len(cfg.Languages) == 0 && cfg.Language != "" {
		cfg.Languages = []string{cfg.Language}
	}
	cfg.TZLabel = tzLabel(cfg.Timezone, cfg.Language)
	cfg.Screen = parseSnapshotScreen(v)
	cfg.DPR = parseFloatOr(v["dpr"], 1)
	if cd := atoiOr(v["color_depth"], 0); cd > 0 {
		cfg.Screen.ColorDepth = cd
		cfg.Screen.PixelDepth = cd
	}
	// UA-CH 从**快照自己**派生：快照里的 UA 与 platform 才是这个环境已落库的权威值，
	// 拿模板里未被 TLS 闸门对齐过的原始 UA 反而会自相矛盾。
	name, ver := splitOS(v["os"])
	major := 120
	if m, err := chromeMajorFromUA(cfg.UA); err == nil && m > 0 {
		major = m
	}
	full := chromeFullVersionFromUA(cfg.UA, major)
	notA := "8"
	if major >= 125 {
		notA = "24"
	}
	cfg.UACHBrands = []uachBrand{{Brand: "Not_A Brand", Version: notA}, {Brand: "Chromium", Version: strconv.Itoa(major)}, {Brand: "Google Chrome", Version: strconv.Itoa(major)}}
	cfg.UACHFullVersionList = []uachBrand{{Brand: "Not_A Brand", Version: "8.0.0.0"}, {Brand: "Chromium", Version: full}, {Brand: "Google Chrome", Version: full}}
	cfg.UACHFullVersion = full
	cfg.UACHPlatform = uachPlatformFromPlatform(cfg.Platform)
	cfg.UACHPlatformVersion = uachPlatformVersionFromOS(name, ver)
	if cfg.UACHArch == "" {
		cfg.UACHArch = "x86"
	}
	return cfg
}

// parseSnapshotScreen 快照里 screen 存的是 "1920x1080"，可用高度没有单独落库，
// 按任务栏/菜单栏高度回推（口径与画像模板一致）。
func parseSnapshotScreen(v map[string]string) injectScreen {
	w, h := 1920, 1080
	if s := v["screen"]; s != "" {
		parts := strings.Split(strings.ToLower(s), "x")
		if len(parts) == 2 {
			if n := atoiOr(strings.TrimSpace(parts[0]), 0); n > 0 {
				w = n
			}
			if n := atoiOr(strings.TrimSpace(parts[1]), 0); n > 0 {
				h = n
			}
		}
	}
	availH := h
	switch {
	case h >= 1080:
		availH = h - 40
	case h >= 900:
		availH = h - 30
	default:
		availH = h - 28
	}
	return injectScreen{Width: w, Height: h, AvailWidth: w, AvailHeight: availH, ColorDepth: 24, PixelDepth: 24}
}

// fillFromTemplate 用模板补齐快照缺失的字段（只填空，不动快照已有的值）。
func (c *injectConfig) fillFromTemplate(tpl Profile) {
	if c.UA == "" {
		c.UA = tpl.UA
		c.AppVersion = appVersionFromUA(tpl.UA)
	}
	if c.Platform == "" {
		c.Platform = tpl.OS.Platform
	}
	// 快照里存不出 UNMASKED 长串与插件集，只能由模板补齐。
	if c.UnmaskedVendor == "" {
		c.UnmaskedVendor = tpl.GPU.Vendor
	}
	if c.UnmaskedRenderer == "" {
		c.UnmaskedRenderer = tpl.GPU.Renderer
	}
	if c.WebGLVendor == "" {
		c.WebGLVendor = tpl.GPU.WebGLVendor
	}
	if c.WebGLRenderer == "" {
		c.WebGLRenderer = tpl.GPU.WebGLRenderer
	}
	if len(c.Plugins) == 0 {
		c.Plugins = pluginsCopy(tpl.Plugins)
	}
	if c.Timezone == "" {
		c.Timezone = tpl.Timezone
		c.UTCOffsetMinutes = tpl.UTCOffset * 60
	}
	if c.Language == "" {
		c.Language = tpl.Locale
	}
	if len(c.Languages) == 0 {
		c.Languages = append([]string{}, tpl.Languages...)
	}
	if c.WebRTCMode == "" {
		c.WebRTCMode = tpl.WebRTC.Mode
	}
	if c.TZLabel == "" {
		c.TZLabel = tzLabel(c.Timezone, c.Language)
	}
	// 快照里存不下 CPU 架构，只能由模板补齐（x86_64 → x86、arm64 → arm）。
	if c.UACHArch == "" {
		c.UACHArch = uachArch(&tpl)
	}
}

func atoiOr(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func parseFloatOr(s string, def float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}

func browserVendor(p *Profile) string {
	// Chrome 在所有平台上 navigator.vendor 都是 "Google Inc."；
	// 写实 Apple 会造出「Chrome + Apple vendor」这种不存在的组合，一眼被识破。
	return "Google Inc."
}

// ScriptSummary 脚本摘要（用于审计与自检报告，不含完整脚本）。
func ScriptSummary(script string) string {
	if len(script) == 0 {
		return ""
	}
	return "js:" + strconv.Itoa(len(script)) + " bytes, " +
		strconv.Itoa(strings.Count(script, "\n")+1) + " lines"
}

// renderInjectScript 把画像配置填进模板。
//
// 用 Replace 而不是 Sprintf：脚本正文里有 % 取模运算，走格式化就必须到处写 %%，
// 这种"改一处忘一处"的坑正是注入脚本静默失效的经典来源。
func renderInjectScript(cfg injectConfig) string {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	out := strings.Replace(injectTemplate, "__ZINIAO_CONFIG__", string(raw), 1)
	return strings.Replace(out, "__ZINIAO_VER__", strconv.Itoa(ScriptVersion), 1)
}

// ScriptHasVersion 判断一段已落库的脚本是否达到指定版本（用于老环境补脚本）。
func ScriptHasVersion(script string, version int) bool {
	return version > 0 && strings.Contains(script, "__ZINIAO_SCRIPT_VER__="+strconv.Itoa(version))
}

// injectTemplate 文档创建前注入的脚本模板。
//
// 执行时机：Page.addScriptToEvaluateOnNewDocument（document_start）。
// 晚于这个时点再改，页面脚本已经把真实值读走了。
//
// 覆盖清单（缺一项都会留一行红）：
//  1. navigator.webdriver      → undefined（并清掉 cdc_/$cdc_ 等自动化残留）
//  2. UA / appVersion / UA-CH  → 与画像同源，UA-CH platform 随 OS 走
//  3. platform / vendor / 核数 / 内存 / 语言 / 触屏
//  4. plugins / mimeTypes      → 统一内置集
//  5. screen / DPR
//  6. 语言与时区（Intl 的 locale + Date#toString/toTimeString 的时区串）
//  7. WebGL（含 UNMASKED_DEBUG_RENDERER_INFO）
//  8. Canvas 噪声（按 canvas_seed 派生，可复现）
//  9. Audio 噪声
//  10. WebRTC 策略
//
// 每个小节独立 try/catch：一处抛异常不能拖垮后面所有小节。
// 注意：整段是 Go 的 raw string，里面任何位置都不能出现反引号。
const injectTemplate = `(function(){
var C=__ZINIAO_CONFIG__;
var def=function(o,k,v){try{Object.defineProperty(o,k,{get:function(){return v;},configurable:true});}catch(e){}};
var hasOwn=function(o,k){return Object.prototype.hasOwnProperty.call(o,k);};
var clone=function(o){var r={};if(!o)return r;for(var k in o){if(hasOwn(o,k))r[k]=o[k];}return r;};
if(typeof window==="undefined"||!window.navigator)return;
var N=window.navigator;

/* 1) 自动化痕迹：navigator.webdriver 必须读不到值 */
try{
  delete N.webdriver;
  Object.defineProperty(Navigator.prototype,"webdriver",{get:function(){return undefined;},configurable:true});
}catch(e){}
try{
  var ks=Object.keys(window);
  for(var ki=0;ki<ks.length;ki++){
    var kk=ks[ki];
    if(/^(cdc_|__playwright|__puppeteer|__selenium|__webdriver|callPhantom|_phantom|__nightmare|domAutomation)/.test(kk)){
      try{delete window[kk];}catch(e1){try{window[kk]=undefined;}catch(e2){}}
    }
  }
}catch(e){}
try{delete document["$cdc_asdjflasutopfhvcZLmcfl_"];}catch(e){}
try{delete document["$chrome_asyncScriptInfo"];}catch(e){}

/* 2) navigator 基础字段（与 UA 同出一源） */
try{
  def(N,"platform",C.platform);
  def(N,"vendor",C.vendor);
  def(N,"hardwareConcurrency",C.hardwareConcurrency);
  def(N,"deviceMemory",C.deviceMemory);
  def(N,"language",C.language);
  def(N,"languages",C.languages);
  def(N,"maxTouchPoints",C.maxTouchPoints);
  try{Object.defineProperty(N,"userAgent",{get:function(){return C.ua;},configurable:true});}catch(e1){}
  try{Object.defineProperty(N,"appVersion",{get:function(){return C.appVersion;},configurable:true});}catch(e2){}
}catch(e){}

/* 3) UA-CH：大版本与 UA 同源，platform 与画像 OS 同源（禁止硬编码 Windows） */
try{
  var _uad={
    brands:C.uachBrands,
    mobile:!!C.uachMobile,
    platform:C.uachPlatform,
    getHighEntropyValues:function(hints){
      return Promise.resolve({
        architecture:C.uachArch,
        bitness:C.uachBitness,
        model:C.uachModel,
        platformVersion:C.uachPlatformVersion,
        uaFullVersion:C.uachFullVersion,
        fullVersionList:C.uachFullVersionList
      });
    },
    toJSON:function(){return {brands:this.brands,mobile:this.mobile,platform:this.platform};}
  };
  try{Object.defineProperty(Navigator.prototype,"userAgentData",{get:function(){return _uad;},configurable:true});}catch(e1){}
  def(N,"userAgentData",_uad);
}catch(e){}

/* 4) plugins / mimeTypes：所有环境共用同一套内置插件 */
try{
  /* 关键：伪造对象必须挂在 PluginArray/Plugin 的原型链上，
     否则 navigator.plugins instanceof PluginArray 直接红（Sannysoft 会查） */
  function makePlugin(name,desc,filename){
    var p=Object.create(window.Plugin?Plugin.prototype:Object.prototype);
    p.name=name;p.type="application/pdf";p.description=desc;
    p.filename=filename||"internal-pdf-viewer";p.suffixes="pdf";p.length=1;
    return p;
  }
  function makeArray(proto,list){
    var a=Object.create(proto);
    for(var i=0;i<list.length;i++){a[i]=list[i];a[list[i].name]=list[i];}
    a.length=list.length;
    a.item=function(i){return list[i];};
    a.namedItem=function(n){return a[n]||null;};
    a.refresh=function(){};
    return a;
  }
  var _pl=(C.plugins||[]).map(function(x){return makePlugin(x.name,x.description,x.filename);});
  def(N,"plugins",makeArray(window.PluginArray?PluginArray.prototype:Object.prototype,_pl));
  var _mts=[];
  (C.plugins||[]).forEach(function(x){(x.mime_types||[]).forEach(function(t){_mts.push(makePlugin(x.name,x.description,x.filename));});});
  def(N,"mimeTypes",makeArray(window.MimeTypeArray?MimeTypeArray.prototype:Object.prototype,_mts));
}catch(e){}

/* 5) screen / DPR */
try{
  def(window.screen,"width",C.screen.width);
  def(window.screen,"height",C.screen.height);
  def(window.screen,"availWidth",C.screen.availWidth);
  def(window.screen,"availHeight",C.screen.availHeight);
  def(window.screen,"colorDepth",C.screen.colorDepth);
  def(window.screen,"pixelDepth",C.screen.pixelDepth);
  def(window,"devicePixelRatio",C.dpr);
}catch(e){}

/* 6) 语言与时区（Intl + Date，必须与出口国一致）
   两个必须一起改的地方：
     a) Intl 的 resolvedOptions().locale —— 它取的是浏览器 UI 语言，不是 navigator.language，
        只改 navigator.language 会出现「语言说 ja-JP、Intl 说 zh-CN」的拼接痕迹；
     b) Date#toString / toTimeString 尾部那段 "GMT+0800 (中国标准时间)" —— 它由内核按
        系统时区生成，改不到就会出现「Intl 说 Asia/Tokyo、字符串说中国标准时间」。 */
try{
  var _TZ=C.timezone,_LANG=C.language,_LABEL=C.tzLabel;
  var _DAYS=["Sun","Mon","Tue","Wed","Thu","Fri","Sat"];
  var _MONTHS=["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
  function _p2(n){var s=String(n);return s.length<2?"0"+s:s;}
  function _p4(n){var s=String(n);while(s.length<4)s="0"+s;return s;}
  /* C.utcOffsetMinutes 是「本地 = UTC + 偏移」，与 getTimezoneOffset 反号 */
  function _offs(min){
    var a=Math.abs(min);
    return "GMT"+(min<0?"-":"+")+_p2(Math.floor(a/60))+_p2(a%60);
  }
  function _tzparen(){return _LABEL?" ("+_LABEL+")":"";}
  /* 墙上时间：把 UTC 时刻搬到画像时区后的年月日时分秒（周日=0） */
  function _wall(t){
    var d=new Date(t.getTime()+C.utcOffsetMinutes*60000);
    return {y:d.getUTCFullYear(),mo:d.getUTCMonth(),d:d.getUTCDate(),w:d.getUTCDay(),
            h:d.getUTCHours(),mi:d.getUTCMinutes(),s:d.getUTCSeconds(),ms:d.getUTCMilliseconds()};
  }
  /* 调用方没给 locale 时，按画像语言补（否则退回系统语言 = 自曝） */
  function _defloc(l){
    return (l===undefined||l===null||l===""||(Array.isArray(l)&&l.length===0))?_LANG:l;
  }

  /* 6.1 Intl 构造器：未显式指定 locale 的实例，resolvedOptions().locale 必须是画像语言 */
  var _IC=["DateTimeFormat","NumberFormat","Collator","PluralRules","RelativeTimeFormat","ListFormat","DisplayNames","Segmenter"];
  for(var _i6=0;_i6<_IC.length;_i6++){
    (function(_n){
      var _O=(typeof Intl!=="undefined")?Intl[_n]:null;
      if(typeof _O!=="function"||!_O.prototype||typeof _O.prototype.resolvedOptions!=="function")return;
      var _RO=_O.prototype.resolvedOptions;
      function _W(l,o){
        var _ftz=false;
        if(_n==="DateTimeFormat"){
          o=clone(o);
          if(o.timeZone===undefined||o.timeZone===null||o.timeZone===""){o.timeZone=_TZ;_ftz=true;}
        }
        var _inst=new _O(l,o);
        try{
          if(l===undefined||l===null||l===""||(Array.isArray(l)&&l.length===0)){
            Object.defineProperty(_inst,"__zl",{value:1});
          }
          if(_ftz)Object.defineProperty(_inst,"__zt",{value:1});
        }catch(e1){}
        return _inst;
      }
      _W.prototype=_O.prototype;
      _W.supportedLocalesOf=_O.supportedLocalesOf;
      _O.prototype.resolvedOptions=function(){
        var o=_RO.call(this);
        try{
          if(o){
            if(this.__zl)o.locale=_LANG;
            if(this.__zt)o.timeZone=_TZ;
          }
        }catch(e2){}
        return o;
      };
      try{Intl[_n]=_W;}catch(e3){}
    })(_IC[_i6]);
  }

  /* 6.2 Date 原型：本地时间分量与字符串必须与画像时区一致 */
  Date.prototype.getTimezoneOffset=function(){return -C.utcOffsetMinutes;};
  Date.prototype.getFullYear=function(){return _wall(this).y;};
  Date.prototype.getYear=function(){return _wall(this).y-1900;};
  Date.prototype.getMonth=function(){return _wall(this).mo;};
  Date.prototype.getDate=function(){return _wall(this).d;};
  Date.prototype.getDay=function(){return _wall(this).w;};
  Date.prototype.getHours=function(){return _wall(this).h;};
  Date.prototype.getMinutes=function(){return _wall(this).mi;};
  Date.prototype.getSeconds=function(){return _wall(this).s;};
  Date.prototype.getMilliseconds=function(){return _wall(this).ms;};
  Date.prototype.toString=function(){
    if(isNaN(this.getTime()))return "Invalid Date";
    var w=_wall(this);
    return _DAYS[w.w]+" "+_MONTHS[w.mo]+" "+_p2(w.d)+" "+_p4(w.y)+" "+
           _p2(w.h)+":"+_p2(w.mi)+":"+_p2(w.s)+" "+_offs(C.utcOffsetMinutes)+_tzparen();
  };
  Date.prototype.toDateString=function(){
    if(isNaN(this.getTime()))return "Invalid Date";
    var w=_wall(this);
    return _DAYS[w.w]+" "+_MONTHS[w.mo]+" "+_p2(w.d)+" "+_p4(w.y);
  };
  Date.prototype.toTimeString=function(){
    if(isNaN(this.getTime()))return "Invalid Date";
    var w=_wall(this);
    return _p2(w.h)+":"+_p2(w.mi)+":"+_p2(w.s)+" "+_offs(C.utcOffsetMinutes)+_tzparen();
  };
  var _DLS=Date.prototype.toLocaleString,_DLD=Date.prototype.toLocaleDateString,_DLT=Date.prototype.toLocaleTimeString;
  Date.prototype.toLocaleString=function(l,o){o=clone(o);if(!o.timeZone)o.timeZone=_TZ;return _DLS.call(this,_defloc(l),o);};
  Date.prototype.toLocaleDateString=function(l,o){o=clone(o);if(!o.timeZone)o.timeZone=_TZ;return _DLD.call(this,_defloc(l),o);};
  Date.prototype.toLocaleTimeString=function(l,o){o=clone(o);if(!o.timeZone)o.timeZone=_TZ;return _DLT.call(this,_defloc(l),o);};
  if(window.Number&&Number.prototype.toLocaleString){
    var _NLS=Number.prototype.toLocaleString;
    Number.prototype.toLocaleString=function(l,o){return _NLS.call(this,_defloc(l),o);};
  }
}catch(e){}

/* 7) WebGL：VENDOR/RENDERER + UNMASKED 必须与画像 GPU 一致 */
try{
  function hookGL(proto,ver,glsl){
    if(!proto)return;
    var gp=proto.getParameter;
    if(gp){
      proto.getParameter=function(p){
        if(p===37445)return C.unmaskedVendor;
        if(p===37446)return C.unmaskedRenderer;
        if(p===7936)return C.webglVendor;
        if(p===7938)return C.webglRenderer;
        if(p===7937)return ver;
        if(p===35724)return glsl;
        return gp.apply(this,arguments);
      };
    }
    var gi=proto.getExtension;
    if(gi){
      proto.getExtension=function(n){
        if(n==="WEBGL_debug_renderer_info"){return {UNMASKED_VENDOR_WEBGL:37445,UNMASKED_RENDERER_WEBGL:37446};}
        return gi.apply(this,arguments);
      };
    }
  }
  hookGL(window.WebGLRenderingContext?window.WebGLRenderingContext.prototype:null,C.webglVersion,C.glslVersion);
  if(window.WebGL2RenderingContext)hookGL(window.WebGL2RenderingContext.prototype,C.webgl2Version,C.glsl2Version);
}catch(e){}

/* 8) Canvas 噪声：由 canvas_seed 派生、按像素位置确定，同环境可复现、跨环境必不同 */
function h32(x){x=x>>>0;x^=x>>>16;x=Math.imul(x,0x7feb352d);x^=x>>>15;x=Math.imul(x,0x846ca68b);x^=x>>>16;return x>>>0;}
var _gd=window.CanvasRenderingContext2D?CanvasRenderingContext2D.prototype.getImageData:null;
var _CMAX=4194304;
var _cseed=h32(parseInt(String(C.canvasSeed||"").slice(0,8),16)||0)||1;
var _camp=1+(h32(_cseed^0x5f356495)%4);
function _cnoise(data){
  var n=data.length;
  for(var i=0;i<n;i+=4){
    var d1=(h32(i^_cseed)%(2*_camp+1))-_camp;
    var d2=(h32((i+1)^_cseed)%(2*_camp+1))-_camp;
    var d3=(h32((i+2)^_cseed)%(2*_camp+1))-_camp;
    if(d1!==0)data[i]=(data[i]+d1+256)&255;
    if(d2!==0)data[i+1]=(data[i+1]+d2+256)&255;
    if(d3!==0)data[i+2]=(data[i+2]+d3+256)&255;
  }
  return data;
}
try{
  if(_gd){
    CanvasRenderingContext2D.prototype.getImageData=function(x,y,w,h){
      var d=_gd.apply(this,arguments);
      try{if(d&&d.data&&d.data.length<=_CMAX)_cnoise(d.data);}catch(e1){}
      return d;
    };
  }
}catch(e){}
try{
  if(window.HTMLCanvasElement&&HTMLCanvasElement.prototype.toDataURL){
    var _td=HTMLCanvasElement.prototype.toDataURL;
    HTMLCanvasElement.prototype.toDataURL=function(){
      try{
        var w=this.width|0,h=this.height|0;
        if(_gd&&w>0&&h>0&&w*h*4<=_CMAX){
          var ctx=this.getContext("2d");
          if(ctx){
            var img=_gd.call(ctx,0,0,w,h);
            if(img&&img.data){
              _cnoise(img.data);
              var tmp=document.createElement("canvas");
              tmp.width=w;tmp.height=h;
              var tctx=tmp.getContext("2d");
              if(tctx){tctx.putImageData(img,0,0);return _td.apply(tmp,arguments);}
            }
          }
        }
      }catch(e1){}
      return _td.apply(this,arguments);
    };
  }
}catch(e){}
try{
  if(window.HTMLCanvasElement&&HTMLCanvasElement.prototype.toBlob){
    var _tb=HTMLCanvasElement.prototype.toBlob;
    HTMLCanvasElement.prototype.toBlob=function(cb){
      try{
        var w=this.width|0,h=this.height|0;
        if(_gd&&w>0&&h>0&&w*h*4<=_CMAX){
          var ctx=this.getContext("2d");
          if(ctx){
            var img=_gd.call(ctx,0,0,w,h);
            if(img&&img.data){
              _cnoise(img.data);
              var tmp=document.createElement("canvas");
              tmp.width=w;tmp.height=h;
              var tctx=tmp.getContext("2d");
              if(tctx){tctx.putImageData(img,0,0);return _tb.apply(tmp,arguments);}
            }
          }
        }
      }catch(e1){}
      return _tb.apply(this,arguments);
    };
  }
}catch(e){}

/* 9) Audio 噪声 */
try{
  var aseg=parseInt(String(C.audioSeed||"").slice(0,8),16)||1;
  if(window.AudioBuffer&&AudioBuffer.prototype.getChannelData){
    var _gc=AudioBuffer.prototype.getChannelData;
    AudioBuffer.prototype.getChannelData=function(ch){
      var a=_gc.apply(this,arguments);
      for(var i=0;i<Math.min(a.length,1024);i+=97){a[i]=a[i]+((aseg%13)*1e-7);}
      return a;
    };
  }
}catch(e){}

/* 10) WebRTC 策略 */
try{
  if(C.webRTCMode==="disabled"&&window.RTCPeerConnection){
    try{delete window.RTCPeerConnection;}catch(e1){window.RTCPeerConnection=undefined;}
  }
}catch(e){}

try{window.__ZINIAO_INJECTED__=true;window.__ZINIAO_SCRIPT_VER__=__ZINIAO_VER__;}catch(e){}
})();`
