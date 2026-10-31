package fingerprint

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot 环境基线快照（创建环境时落库，用于后续漂移比对）。
type Snapshot struct {
	EnvID     string            `json:"env_id"`
	ProfileID string            `json:"profile_id"`
	Values    map[string]string `json:"values"`
	Baseline  string            `json:"baseline"` // Profile.Hash()
	TakenAt   time.Time         `json:"taken_at"`
}

// Capture 从画像生成基线快照（所有可比对字段展开为字符串）。
func Capture(p *Profile) Snapshot {
	v := map[string]string{
		"ua":                   p.UA,
		"platform":             p.OS.Platform,
		"os":                   p.OS.Name + " " + p.OS.Version,
		"screen":               strconv.Itoa(p.Screen.Width) + "x" + strconv.Itoa(p.Screen.Height),
		"dpr":                  strconv.FormatFloat(p.Screen.DPR, 'f', 2, 64),
		"color_depth":          strconv.Itoa(p.Screen.ColorDepth),
		"timezone":             p.Timezone,
		"utc_offset":           strconv.Itoa(p.UTCOffset),
		"locale":               p.Locale,
		"languages":            strings.Join(p.Languages, ","),
		"hardware_concurrency": strconv.Itoa(p.CPU.Cores),
		"device_memory":        strconv.Itoa(p.CPU.DeviceMemory),
		"webgl_vendor":         p.GPU.WebGLVendor,
		"webgl_renderer":       p.GPU.WebGLRenderer,
		"fonts_count":          strconv.Itoa(len(p.Fonts)),
		"plugins_count":        strconv.Itoa(len(p.Plugins)),
		"country":              p.Country,
		"canvas_seed":          p.CanvasSeed,
		"audio_seed":           p.AudioSeed,
	}
	return Snapshot{ProfileID: p.ID, Values: v, Baseline: p.Hash(), TakenAt: time.Now().UTC()}
}

// DriftItem 一个漂移项。
type DriftItem struct {
	Field    string   `json:"field"`
	Expected string   `json:"expected"`
	Actual   string   `json:"actual"`
	Severity Severity `json:"severity"`
}

// DriftReport 漂移监控报告（M0 指纹漂移监控）。
type DriftReport struct {
	Score int         `json:"score"` // 100 - 扣分
	Risk  string      `json:"risk"`  // low|medium|high
	Items []DriftItem `json:"items"`
}

// fieldSeverity 字段漂移的危害等级。
var fieldSeverity = map[string]Severity{
	"ua": SeverityCritical, "platform": SeverityCritical, "country": SeverityCritical,
	"ja3": SeverityCritical, "webgl_vendor": SeverityCritical, "webgl_renderer": SeverityCritical,
	"screen": SeverityMajor, "dpr": SeverityMajor, "timezone": SeverityMajor,
	"languages": SeverityMajor, "utc_offset": SeverityMajor, "hardware_concurrency": SeverityMajor,
	"device_memory": SeverityMinor, "fonts_count": SeverityMinor, "plugins_count": SeverityMinor,
	"locale": SeverityMinor, "os": SeverityMinor, "color_depth": SeverityMinor,
	"canvas_seed": SeverityMinor, "audio_seed": SeverityMinor,
}

// DetectDrift 比对基线快照与运行时观测值。
// 观测值由客户端上报（M0.3 自检页）或服务端探测（network 模块）给出。
func DetectDrift(base Snapshot, observed map[string]string) DriftReport {
	items := []DriftItem{}
	score := 100
	fields := make([]string, 0, len(base.Values))
	for k := range base.Values {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	risk := "low"
	for _, k := range fields {
		actual, ok := observed[k]
		if !ok {
			continue // 未观测的字段不参与判定
		}
		if actual == base.Values[k] {
			continue
		}
		sev, ok := fieldSeverity[k]
		if !ok {
			sev = SeverityMinor
		}
		items = append(items, DriftItem{Field: k, Expected: base.Values[k], Actual: actual, Severity: sev})
		score -= penalty[sev]
		if sev == SeverityCritical {
			risk = "high"
		} else if sev == SeverityMajor && risk != "high" {
			risk = "medium"
		}
	}
	if score < 0 {
		score = 0
	}
	return DriftReport{Score: score, Risk: risk, Items: items}
}
