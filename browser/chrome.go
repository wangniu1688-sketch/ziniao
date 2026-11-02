package browser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BrowserKind 浏览器种类。
type BrowserKind string

const (
	KindChrome BrowserKind = "chrome"
	KindEdge   BrowserKind = "edge"
)

// Candidate 一个探测到的浏览器安装（含版本）。
type Candidate struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	Major   int    `json:"major,omitempty"`
}

// BrowserInfo 探测结论：首选浏览器 + 全部候选。
type BrowserInfo struct {
	Installed  bool        `json:"installed"`
	Kind       string      `json:"kind,omitempty"`
	Path       string      `json:"path,omitempty"`
	Version    string      `json:"version,omitempty"`
	Major      int         `json:"major,omitempty"`
	Candidates []Candidate `json:"candidates"`
}

// Locator 本机浏览器探测器。
//
// Override 为空时按常见安装路径探测；非空时只认这一个可执行文件
// （ZINIAO_CHROME_PATH：装到奇怪位置 / 想固定用某个渠道版时用）。
type Locator struct {
	Override string
}

// Detect 探测本机 Chrome/Edge。
//
// 顺序固定为「先 Chrome 后 Edge」：Chrome 是指纹基线的采集对象（指纹库里的
// JA3/JA4 都来自真实 Chrome），只有当机器上没有 Chrome 时才回落到 Edge，
// 避免在同一个环境里混用两种内核导致指纹与基线对不上。
func (l Locator) Detect() BrowserInfo {
	if p := strings.TrimSpace(l.Override); p != "" {
		if _, err := os.Stat(p); err != nil {
			return BrowserInfo{Candidates: []Candidate{}}
		}
		ver, major := versionOf(p)
		return BrowserInfo{
			Installed: true, Kind: kindOf(p), Path: p, Version: ver, Major: major,
			Candidates: []Candidate{{Kind: kindOf(p), Path: p, Version: ver, Major: major}},
		}
	}
	cands := []Candidate{}
	seen := map[string]bool{}
	for _, p := range candidatePaths() {
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Stat(p); err != nil {
			continue
		}
		ver, major := versionOf(p)
		cands = append(cands, Candidate{Kind: kindOf(p), Path: p, Version: ver, Major: major})
	}
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i].Kind) < rank(cands[j].Kind) })
	if len(cands) == 0 {
		return BrowserInfo{Candidates: []Candidate{}}
	}
	best := cands[0]
	return BrowserInfo{
		Installed: true, Kind: best.Kind, Path: best.Path,
		Version: best.Version, Major: best.Major, Candidates: cands,
	}
}

func rank(kind string) int {
	if kind == string(KindChrome) {
		return 0
	}
	return 1
}

func kindOf(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if strings.Contains(base, "msedge") {
		return string(KindEdge)
	}
	return string(KindChrome)
}

// candidatePaths 各平台常见安装路径（按优先级排列）。
func candidatePaths() []string {
	var out []string
	switch runtime.GOOS {
	case "windows":
		roots := []string{}
		for _, k := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
			if v := os.Getenv(k); v != "" {
				roots = append(roots, v)
			}
		}
		for _, r := range roots {
			out = append(out,
				filepath.Join(r, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(r, "Microsoft", "Edge", "Application", "msedge.exe"),
			)
		}
	case "darwin":
		out = append(out,
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		)
	default:
		out = append(out,
			"/opt/google/chrome/chrome",
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/microsoft-edge",
			"/snap/bin/chromium",
		)
	}
	return out
}

// versionOf 取浏览器版本。
//
// 先看安装目录下的版本子目录（Chrome/Edge 都按 <dir>/<version>/ 布局，读目录
// 是零成本且不启动进程）；读不到才执行 `<exe> --version`（~100ms，只在冷启动时
// 走一次，不在热路径上）。
func versionOf(exe string) (string, int) {
	if v, major := dirVersion(filepath.Dir(exe)); v != "" {
		return v, major
	}
	return execVersion(exe)
}

// dirVersion 从安装目录里的版本子目录推断（取最高版本）。
func dirVersion(dir string) (string, int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0
	}
	best, bestMajor := "", 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if v, major := parseVersion(name); v != "" && major > bestMajor {
			best, bestMajor = v, major
		}
	}
	return best, bestMajor
}

// execVersion 执行 `<exe> --version` 取版本（带超时，避免卡住启动流程）。
func execVersion(exe string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "--version").Output() //nolint:gosec // 路径来自本机探测或管理员显式配置
	if err != nil {
		return "", 0
	}
	return parseVersion(strings.TrimSpace(string(out)))
}

// parseVersion 从任意字符串里抽出第一个 `数字(.数字)*` 版本号。
func parseVersion(s string) (string, int) {
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return "", 0
	}
	end := start
	for end < len(s) && ((s[end] >= '0' && s[end] <= '9') || s[end] == '.') {
		end++
	}
	raw := strings.TrimRight(s[start:end], ".")
	if raw == "" {
		return "", 0
	}
	major, err := strconv.Atoi(strings.Split(raw, ".")[0])
	if err != nil {
		return raw, 0
	}
	return raw, major
}
