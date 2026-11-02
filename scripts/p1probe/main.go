// Command p1probe 用**本机真实 Chrome**实测 P1 加固的三项：
// 时区字符串 / Intl locale / UA 内核版本对齐。
//
// 与单测（goja）的分工：goja 只能证明脚本逻辑对，证明不了真实 V8 上
// Intl / Date 这两个内部对象是否真的被改到了——这里补的是后者。
//
// 用法：go run ./tmp_probe/p1probe
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ziniao/browser"
	"ziniao/fingerprint"
)

// readback 在真实页面里读回 P1 的三个覆盖点。
const readback = `(function(){
var o={
  webdriver: (typeof navigator.webdriver==="undefined")?"undefined":String(navigator.webdriver),
  ua: navigator.userAgent,
  uachFull: "",
  platform: navigator.platform,
  language: navigator.language,
  intlLocale: new Intl.DateTimeFormat().resolvedOptions().locale,
  intlTZ: new Intl.DateTimeFormat().resolvedOptions().timeZone,
  intlNumberLocale: new Intl.NumberFormat().resolvedOptions().locale,
  explicitLocale: new Intl.DateTimeFormat("zh-CN").resolvedOptions().locale,
  dateToString: new Date(0).toString(),
  dateToTimeString: new Date(0).toTimeString(),
  dateToDateString: new Date(0).toDateString(),
  localHours: new Date(0).getHours(),
  utcHours: new Date(0).getUTCHours(),
  offset: new Date(0).getTimezoneOffset(),
  invalid: new Date(NaN).toString(),
  scriptVer: window.__ZINIAO_SCRIPT_VER__||0
};
try{o.uachFull=navigator.userAgentData.getHighEntropyValues(["uaFullVersion"]).then(function(r){return r.uaFullVersion;});}catch(e){}
return JSON.stringify(o);})()`

func probe(ctx context.Context, exe, envID, country, osFamily, script string) error {
	dir := filepath.Join(os.TempDir(), "ziniao-p1probe", envID)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cmd := exec.Command(exe,
		"--user-data-dir="+dir,
		"--remote-debugging-port=0",
		"--no-first-run", "--no-default-browser-check", "--disable-gpu",
		"--headless=new", "--window-size=1920,1080",
		"about:blank")
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(dir)
	}()

	// 等 Chrome 把实际端口写回 DevToolsActivePort
	var wsURL string
	portFile := filepath.Join(dir, "DevToolsActivePort")
	for i := 0; i < 60; i++ {
		b, err := os.ReadFile(portFile)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) >= 2 && lines[0] != "" {
				wsURL = "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1])
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if wsURL == "" {
		return fmt.Errorf("未拿到 CDP 端口（%s 未生成）", portFile)
	}

	conn, err := browser.DialCDP(ctx, wsURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	targets, err := conn.ListTargets(ctx)
	if err != nil {
		return err
	}
	pageID := ""
	for _, t := range targets {
		if t.Type == "page" {
			pageID = t.TargetID
			break
		}
	}
	if pageID == "" {
		return fmt.Errorf("没有 page 类型的 target")
	}
	sess, err := conn.AttachToTarget(ctx, pageID)
	if err != nil {
		return err
	}
	if err := conn.EnablePage(ctx, sess); err != nil {
		return err
	}
	if err := conn.AddScriptToEvaluateOnNewDocument(ctx, sess, script); err != nil {
		return err
	}
	if err := conn.Navigate(ctx, sess, "file:///g:/ziniao/gateway/web/fp-test.html"); err != nil {
		return err
	}
	time.Sleep(1500 * time.Millisecond)

	raw, err := conn.Call(ctx, sess, "Runtime.evaluate", map[string]any{
		"expression":    readback,
		"returnByValue": true,
	})
	if err != nil {
		return err
	}
	var wrap struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(wrap.Result.Value), &out); err != nil {
		return fmt.Errorf("读回结果解析失败: %v (%s)", err, wrap.Result.Value)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Printf("===== %s (%s/%s) =====\n%s\n\n", envID, country, osFamily, string(b))
	return nil
}

func main() {
	info := browser.Locator{}.Detect()
	if !info.Installed {
		fmt.Println("本机未探测到 Chrome/Edge，跳过实测")
		os.Exit(2)
	}
	fmt.Printf("本机浏览器: %s %s (major=%d)\n\n", info.Kind, info.Version, info.Major)

	bl, err := fingerprint.LoadBaselines()
	if err != nil {
		fmt.Println("基线加载失败:", err)
		os.Exit(2)
	}
	lib := fingerprint.NewLibrary(fingerprint.DefaultLibraryConfig(), 7)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	for _, tc := range []struct {
		envID, country, osFamily string
	}{
		{"env-us-win", "US", "Windows"},
		{"env-jp-mac", "JP", "macOS"},
	} {
		p, err := lib.Sample(tc.country, tc.osFamily)
		if err != nil {
			fmt.Println("采样失败:", err)
			continue
		}
		align := fingerprint.AlignKernel(p, bl, info.Major)
		fmt.Printf("[%s] 内核对齐: local=%d → major=%d version=%s hello=%s\n  note: %s\n",
			tc.envID, info.Major, align.Major, align.Version, align.Baseline.HelloID, align.Note)
		if err := probe(ctx, info.Path, tc.envID, tc.country, tc.osFamily, fingerprint.InjectionScript(p)); err != nil {
			fmt.Printf("[%s] 实测失败: %v\n", tc.envID, err)
		}
	}
}
