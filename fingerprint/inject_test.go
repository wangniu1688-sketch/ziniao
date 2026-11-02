package fingerprint_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dop251/goja"

	"ziniao/fingerprint"
)

// browserStub 伪造一个最小浏览器环境。
//
// 关键：**默认值刻意填"本机真实值"**（Win32 / zh-CN / 16G / 32 核 / GTX 1060 / UTC），
// 与任何画像都不同。注入脚本跑完之后这些值必须全部被画像覆盖——
// 只要注入没生效（脚本为空、抛异常、时序太晚），读数就会落回这些真实值，测试立刻红。
//
// goja 不带 ECMA-402，所以 Intl 与 Canvas/WebGL 都只实现注入脚本需要的那点行为。
const browserStub = `
function Navigator(){}
Navigator.prototype = {};
var navigator = new Navigator();
Object.defineProperty(Navigator.prototype,"webdriver",{get:function(){return true;},configurable:true});
Object.defineProperty(Navigator.prototype,"userAgent",{get:function(){return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36";},configurable:true});
Object.defineProperty(Navigator.prototype,"appVersion",{get:function(){return "5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36";},configurable:true});
Object.defineProperty(Navigator.prototype,"platform",{get:function(){return "Win32";},configurable:true});
Object.defineProperty(Navigator.prototype,"vendor",{get:function(){return "Google Inc.";},configurable:true});
Object.defineProperty(Navigator.prototype,"language",{get:function(){return "zh-CN";},configurable:true});
Object.defineProperty(Navigator.prototype,"languages",{get:function(){return ["zh-CN","zh"];},configurable:true});
Object.defineProperty(Navigator.prototype,"deviceMemory",{get:function(){return 16;},configurable:true});
Object.defineProperty(Navigator.prototype,"hardwareConcurrency",{get:function(){return 32;},configurable:true});
Object.defineProperty(Navigator.prototype,"maxTouchPoints",{get:function(){return 10;},configurable:true});
Object.defineProperty(Navigator.prototype,"userAgentData",{get:function(){return undefined;},configurable:true});
window.navigator = navigator;
window.screen = {width:2560,height:1440,availWidth:2560,availHeight:1400,colorDepth:24,pixelDepth:24};
window.devicePixelRatio = 1;
window.cdc_adoQpoasnfa76pfcZLmcfl_Symbol = 1;
window.__playwright = {stub:true};

var Intl = {
  DateTimeFormat: function(l,o){
    o = o || {};
    this._tz = o.timeZone || "Etc/UTC";
    this._l = l || "zh-CN";
  }
};
Intl.DateTimeFormat.prototype.resolvedOptions=function(){return {timeZone:this._tz,locale:this._l};};
Intl.DateTimeFormat.supportedLocalesOf=function(l){return [];};
Intl.NumberFormat=function(l,o){this._l=l||"zh-CN";};
Intl.NumberFormat.prototype.resolvedOptions=function(){return {locale:this._l};};
Intl.Collator=function(l,o){this._l=l||"zh-CN";};
Intl.Collator.prototype.resolvedOptions=function(){return {locale:this._l};};

function CanvasRenderingContext2D(canvas){this.canvas=canvas;}
CanvasRenderingContext2D.prototype.getImageData=function(x,y,w,h){
  var d=new Uint8ClampedArray(w*h*4);
  for(var i=0;i<d.length;i++)d[i]=128;
  return {data:d,width:w,height:h};
};
CanvasRenderingContext2D.prototype.putImageData=function(img){this.canvas._data=img.data;};
function HTMLCanvasElement(){this.width=64;this.height=64;this._data=null;}
HTMLCanvasElement.prototype.getContext=function(kind){return kind==="2d"?new CanvasRenderingContext2D(this):null;};
HTMLCanvasElement.prototype.toDataURL=function(){
  return "data:image/png;base64,"+(this._data?Array.prototype.join.call(this._data,","):"EMPTY");
};
HTMLCanvasElement.prototype.toBlob=function(cb){if(cb)cb(null);};
function WebGLRenderingContext(){}
WebGLRenderingContext.prototype.getParameter=function(p){
  if(p===7936)return "WebKit";
  if(p===7938)return "WebKit WebGL";
  if(p===37445)return "Google Inc. (NVIDIA)";
  if(p===37446)return "ANGLE (NVIDIA, NVIDIA GeForce GTX 1060 Direct3D11 vs_5_0 ps_5_0, D3D11)";
  if(p===7937)return "WebGL 1.0 (OpenGL ES 2.0 Chromium)";
  if(p===35724)return "WebGL GLSL ES 1.0 (OpenGL ES GLSL ES 1.0 Chromium)";
  return 0;
};
WebGLRenderingContext.prototype.getExtension=function(n){return null;};
function PluginArray(){}
function Plugin(){}
function MimeTypeArray(){}
function MimeType(){}
var document={createElement:function(tag){return tag==="canvas"?new HTMLCanvasElement():{};}};
`

// readback 注入之后从页面里读回各个指纹字段。
const readback = `(function(){
var out={
  webdriverType: typeof window.navigator.webdriver,
  webdriver: (typeof window.navigator.webdriver==="undefined")?"undefined":String(window.navigator.webdriver),
  ua: window.navigator.userAgent,
  platform: window.navigator.platform,
  language: window.navigator.language,
  languages: window.navigator.languages,
  deviceMemory: window.navigator.deviceMemory,
  cores: window.navigator.hardwareConcurrency,
  uachPlatform: (window.navigator.userAgentData?window.navigator.userAgentData.platform:""),
  automations: [],
  timezone: "",
  offset: 0,
  intlLocale: "", intlNumberLocale: "", intlCollatorLocale: "", explicitLocale: "",
  dateToString: "", dateToTimeString: "", dateToDateString: "", invalidDate: "",
  localHours: -1, localDay: -1, utcHours: -1, toLocale: "",
  webglVendor: "", webglRenderer: "", unmaskedVendor: "", unmaskedRenderer: "",
  canvas: "", canvasRaw: "",
  pluginsProto: false
}
try{out.pluginsProto=(window.navigator.plugins instanceof PluginArray);}catch(e){};
try{out.timezone=(new Intl.DateTimeFormat()).resolvedOptions().timeZone;}catch(e){}
try{out.offset=(new Date(0)).getTimezoneOffset();}catch(e){}
try{out.intlLocale=(new Intl.DateTimeFormat()).resolvedOptions().locale;}catch(e){}
try{out.intlNumberLocale=(new Intl.NumberFormat()).resolvedOptions().locale;}catch(e){}
try{out.intlCollatorLocale=(new Intl.Collator()).resolvedOptions().locale;}catch(e){}
try{out.explicitLocale=(new Intl.DateTimeFormat("zh-CN")).resolvedOptions().locale;}catch(e){}
try{var d0=new Date(0);
  out.dateToString=d0.toString();
  out.dateToTimeString=d0.toTimeString();
  out.dateToDateString=d0.toDateString();
  out.localHours=d0.getHours();
  out.localDay=d0.getDate();
  out.utcHours=d0.getUTCHours();
  out.toLocale=d0.toLocaleString();
  out.invalidDate=new Date(NaN).toString();}catch(e){}
try{var g=window.WebGLRenderingContext.prototype;
  out.webglVendor=g.getParameter(7936);out.webglRenderer=g.getParameter(7938);
  out.unmaskedVendor=g.getParameter(37445);out.unmaskedRenderer=g.getParameter(37446);}catch(e){}
try{var c=document.createElement("canvas");out.canvas=c.toDataURL();}catch(e){}
try{var ks=Object.keys(window);for(var i=0;i<ks.length;i++){
  if(/^(cdc_|__playwright|__puppeteer|__selenium|__webdriver|__nightmare)/.test(ks[i]))out.automations.push(ks[i]);}}catch(e){}
return JSON.stringify(out);})()`

// reading 注入后从页面读回的指纹读数。
type reading struct {
	WebdriverType     string   `json:"webdriverType"`
	Webdriver         string   `json:"webdriver"`
	UA                string   `json:"ua"`
	Platform          string   `json:"platform"`
	Language          string   `json:"language"`
	Languages         []string `json:"languages"`
	DeviceMemory      int      `json:"deviceMemory"`
	Cores             int      `json:"cores"`
	UACHPlatform      string   `json:"uachPlatform"`
	Automations       []string `json:"automations"`
	Timezone          string   `json:"timezone"`
	Offset            int      `json:"offset"`
	IntlLocale        string   `json:"intlLocale"`
	IntlNumberLocale  string   `json:"intlNumberLocale"`
	IntlCollatorLocale string  `json:"intlCollatorLocale"`
	ExplicitLocale    string   `json:"explicitLocale"`
	DateToString      string   `json:"dateToString"`
	DateToTimeString  string   `json:"dateToTimeString"`
	DateToDateString  string   `json:"dateToDateString"`
	InvalidDate       string   `json:"invalidDate"`
	LocalHours        int      `json:"localHours"`
	LocalDay          int      `json:"localDay"`
	UTCHours          int      `json:"utcHours"`
	ToLocale          string   `json:"toLocale"`
	WebGLVendor       string   `json:"webglVendor"`
	WebGLRenderer     string   `json:"webglRenderer"`
	UnmaskedVendor    string   `json:"unmaskedVendor"`
	UnmaskedRenderer  string   `json:"unmaskedRenderer"`
	Canvas            string   `json:"canvas"`
	PluginsProto      bool     `json:"pluginsProto"`
}

// runInjected 在 goja 里跑一遍「浏览器桩 → 注入脚本 → 读数」，返回读数。
func runInjected(t *testing.T, script string) reading {
	t.Helper()
	vm := goja.New()
	vm.Set("window", vm.GlobalObject())
	if _, err := vm.RunString(browserStub); err != nil {
		t.Fatalf("构建浏览器桩失败: %v", err)
	}
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("注入脚本执行失败: %v", err)
	}
	v, err := vm.RunString(readback)
	if err != nil {
		t.Fatalf("读取指纹失败: %v", err)
	}
	var out reading
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("解析指纹读数失败: %v (%s)", err, v.String())
	}
	return out
}

// runProfile 按画像生成脚本并执行，返回读数。
func runProfile(t *testing.T, p fingerprint.Profile) reading {
	t.Helper()
	return runInjected(t, fingerprint.InjectionScript(&p))
}

func builtinByID(t *testing.T, id string) fingerprint.Profile {
	t.Helper()
	for _, p := range fingerprint.BuiltinProfiles() {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("内置画像不存在: %s", id)
	return fingerprint.Profile{}
}

// TestInjectedScriptErasesWebDriver navigator.webdriver 必须读不到值：
// 它一旦为真，Sannysoft 的 WebDriver 行就是红的（failed）。
func TestInjectedScriptErasesWebDriver(t *testing.T) {
	cases := []string{"us-win-dell-xps-8930", "jp-mac-mba-m1", "us-linux-ubuntu-thinkcentre"}
	for _, id := range cases {
		r := runProfile(t, builtinByID(t, id))
		if r.WebdriverType != "undefined" || r.Webdriver != "undefined" {
			t.Fatalf("画像 %s：注入后 navigator.webdriver 仍为 %s (type=%s)", id, r.Webdriver, r.WebdriverType)
		}
	}
}

// TestInjectedScriptLanguagesFollowCountry 语言必须按国家派生，
// 否则「美国店说中文」这种组合一眼就是假的。
func TestInjectedScriptLanguagesFollowCountry(t *testing.T) {
	cases := []struct {
		id       string
		language string
		first    string
	}{
		{"us-win-dell-xps-8930", "en-US", "en-US"},
		{"jp-mac-mba-m1", "ja-JP", "ja-JP"},
		{"de-win-acer-aspire-5", "de-DE", "de-DE"},
		{"uk-win-lenovo-thinkpad-t14", "en-GB", "en-GB"},
	}
	for _, tc := range cases {
		p := builtinByID(t, tc.id)
		r := runProfile(t, p)
		if r.Language != tc.language {
			t.Fatalf("画像 %s：navigator.language=%s，应为 %s", tc.id, r.Language, tc.language)
		}
		if len(r.Languages) == 0 || r.Languages[0] != tc.first {
			t.Fatalf("画像 %s：navigator.languages=%v，首项应为 %s", tc.id, r.Languages, tc.first)
		}
		if len(r.Languages) != len(p.Languages) {
			t.Fatalf("画像 %s：languages 长度 %d，应为 %d", tc.id, len(r.Languages), len(p.Languages))
		}
	}
}

// TestInjectedScriptTimezoneFollowsCountry 时区必须与出口国一致（Intl 与 Date 两处都要改）。
func TestInjectedScriptTimezoneFollowsCountry(t *testing.T) {
	// getTimezoneOffset 的口径是「本地时间减 UTC」，所以 UTC-5 返回 +300、UTC+9 返回 -540。
	cases := []struct {
		id        string
		timezone  string
		offsetMin int
	}{
		{"us-win-dell-xps-8930", "America/New_York", 300},
		{"jp-mac-mba-m1", "Asia/Tokyo", -540},
		{"de-win-acer-aspire-5", "Europe/Berlin", -60},
	}
	for _, tc := range cases {
		r := runProfile(t, builtinByID(t, tc.id))
		if r.Timezone != tc.timezone {
			t.Fatalf("画像 %s：timezone=%s，应为 %s", tc.id, r.Timezone, tc.timezone)
		}
		if r.Offset != tc.offsetMin {
			t.Fatalf("画像 %s：getTimezoneOffset=%d，应为 %d", tc.id, r.Offset, tc.offsetMin)
		}
	}
}

// TestStubReadbackDetectsMissingInjection 反向验证：不注入时读数必须落回「本机真实值」。
//
// 没有这条，桩里如果把默认值写成画像值，上面所有断言都会变成假绿——
// 注入脚本哪天静默失效（生成空串 / 抛异常 / 时序晚了）也测不出来。
func TestStubReadbackDetectsMissingInjection(t *testing.T) {
	r := runInjected(t, "")
	if r.WebdriverType != "boolean" {
		t.Fatalf("未注入时 webdriver 应为桩里的 true（type=boolean），实际 %s", r.WebdriverType)
	}
	if r.IntlLocale != "zh-CN" {
		t.Fatalf("未注入时 Intl locale 应为桩里的本机值 zh-CN，实际 %q", r.IntlLocale)
	}
	if r.Language != "zh-CN" {
		t.Fatalf("未注入时 navigator.language 应为桩里的本机值 zh-CN，实际 %q", r.Language)
	}
}

// TestInjectedScriptDateStringFollowsTimezone Date#toString / toTimeString 尾部那段
// "GMT+0800 (中国标准时间)" 由内核按系统时区生成，只改 Intl 会留下
// 「Intl 说 Asia/Tokyo、字符串说中国标准时间」这种一眼假的组合（browserleaks System Time 行）。
//
// 基准时刻取 epoch 0（1970-01-01T00:00:00Z，周四），偏移后跨日的那侧最能暴露错误：
// UTC-5 → 1969-12-31 19:00（周三），只改小时不改日期就会在这里露馅。
func TestInjectedScriptDateStringFollowsTimezone(t *testing.T) {
	cases := []struct {
		id       string
		toString string
		timeStr  string
		dateStr  string
		hours    int
		day      int
	}{
		{"us-win-dell-xps-8930", "Wed Dec 31 1969 19:00:00 GMT-0500 (Eastern Standard Time)", "19:00:00 GMT-0500 (Eastern Standard Time)", "Wed Dec 31 1969", 19, 31},
		{"jp-mac-mba-m1", "Thu Jan 01 1970 09:00:00 GMT+0900 (日本標準時)", "09:00:00 GMT+0900 (日本標準時)", "Thu Jan 01 1970", 9, 1},
		{"de-win-acer-aspire-5", "Thu Jan 01 1970 01:00:00 GMT+0100 (Mitteleuropäische Zeit)", "01:00:00 GMT+0100 (Mitteleuropäische Zeit)", "Thu Jan 01 1970", 1, 1},
		{"uk-win-lenovo-thinkpad-t14", "Thu Jan 01 1970 00:00:00 GMT+0000 (Greenwich Mean Time)", "00:00:00 GMT+0000 (Greenwich Mean Time)", "Thu Jan 01 1970", 0, 1},
	}
	for _, tc := range cases {
		r := runProfile(t, builtinByID(t, tc.id))
		if r.DateToString != tc.toString {
			t.Fatalf("画像 %s：Date#toString=%q，应为 %q", tc.id, r.DateToString, tc.toString)
		}
		if r.DateToTimeString != tc.timeStr {
			t.Fatalf("画像 %s：Date#toTimeString=%q，应为 %q", tc.id, r.DateToTimeString, tc.timeStr)
		}
		if r.DateToDateString != tc.dateStr {
			t.Fatalf("画像 %s：Date#toDateString=%q，应为 %q", tc.id, r.DateToDateString, tc.dateStr)
		}
		// 本地时间分量必须与字符串同源：字符串说 19 点而 getHours 说本机小时 = 自曝
		if r.LocalHours != tc.hours || r.LocalDay != tc.day {
			t.Fatalf("画像 %s：getHours/getDate=%d/%d，应为 %d/%d", tc.id, r.LocalHours, r.LocalDay, tc.hours, tc.day)
		}
		if r.UTCHours != 0 {
			t.Fatalf("画像 %s：getUTCHours=%d，UTC 分量不得被改动", tc.id, r.UTCHours)
		}
		// 本机值残留（GMT+8 / 中国标准时间）是最典型的回归信号
		if strings.Contains(r.DateToString, "GMT+0800") || strings.Contains(r.DateToString, "中国标准时间") {
			t.Fatalf("画像 %s：Date 串里仍是本机时区: %q", tc.id, r.DateToString)
		}
		if r.InvalidDate != "Invalid Date" {
			t.Fatalf("画像 %s：无效日期应输出 Invalid Date，实际 %q", tc.id, r.InvalidDate)
		}
	}
}

// TestInjectedScriptIntlLocaleFollowsProfile Intl 的 resolvedOptions().locale 取的是
// 浏览器 UI 语言，只改 navigator.language 改不到它——两者不一致就是拼接痕迹。
func TestInjectedScriptIntlLocaleFollowsProfile(t *testing.T) {
	cases := []struct {
		id     string
		locale string
	}{
		{"us-win-dell-xps-8930", "en-US"},
		{"jp-mac-mba-m1", "ja-JP"},
		{"de-win-acer-aspire-5", "de-DE"},
		{"uk-win-lenovo-thinkpad-t14", "en-GB"},
	}
	for _, tc := range cases {
		r := runProfile(t, builtinByID(t, tc.id))
		if r.IntlLocale != tc.locale {
			t.Fatalf("画像 %s：Intl.DateTimeFormat locale=%q，应为 %q", tc.id, r.IntlLocale, tc.locale)
		}
		if r.IntlNumberLocale != tc.locale {
			t.Fatalf("画像 %s：Intl.NumberFormat locale=%q，应为 %q", tc.id, r.IntlNumberLocale, tc.locale)
		}
		if r.IntlCollatorLocale != tc.locale {
			t.Fatalf("画像 %s：Intl.Collator locale=%q，应为 %q", tc.id, r.IntlCollatorLocale, tc.locale)
		}
		// 调用方显式指定的 locale 必须原样保留（覆盖不等于强行改写）
		if r.ExplicitLocale != "zh-CN" {
			t.Fatalf("画像 %s：显式传入的 locale 被改写为 %q", tc.id, r.ExplicitLocale)
		}
	}
}

// TestInjectedScriptUAAndPlatformAgree UA 说 Mac 就必须 platform=MacIntel，
// 说 Windows 就必须 Win32，UA-CH platform 也要跟着走——混搭即自杀。
func TestInjectedScriptUAAndPlatformAgree(t *testing.T) {
	want := map[string]string{"Macintosh": "MacIntel", "Windows NT": "Win32", "X11; Linux": "Linux x86_64"}
	uach := map[string]string{"Macintosh": "macOS", "Windows NT": "Windows", "X11; Linux": "Linux"}
	for _, p := range fingerprint.BuiltinProfiles() {
		r := runProfile(t, p)
		for frag, platform := range want {
			if !strings.Contains(p.UA, frag) {
				continue
			}
			if r.Platform != platform {
				t.Fatalf("画像 %s：UA 含 %q，platform=%s，应为 %s", p.ID, frag, r.Platform, platform)
			}
			if r.UACHPlatform != uach[frag] {
				t.Fatalf("画像 %s：UA 含 %q，UA-CH platform=%s，应为 %s", p.ID, frag, r.UACHPlatform, uach[frag])
			}
		}
		if r.UA != p.UA {
			t.Fatalf("画像 %s：navigator.userAgent 未覆盖", p.ID)
		}
	}
}

// TestInjectedScriptCanvasNoiseDiffersPerEnv 两个环境的 Canvas 读数必须不同，
// 且同一环境重跑必须一致（可复现）。
func TestInjectedScriptCanvasNoiseDiffersPerEnv(t *testing.T) {
	lib := fingerprint.NewLibrary(fingerprint.DefaultLibraryConfig(), 7)
	us, err := lib.Sample("US", "")
	if err != nil {
		t.Fatal(err)
	}
	jp, err2 := lib.Sample("JP", "")
	if err2 != nil {
		t.Fatal(err2)
	}
	a := runProfile(t, *us)
	b := runProfile(t, *jp)
	if a.Canvas == "" || a.Canvas == "data:image/png;base64,EMPTY" {
		t.Fatal("Canvas 钩子未生效（读数为空）")
	}
	if a.Canvas == b.Canvas {
		t.Fatalf("两个环境的 Canvas 读数相同（%s），Canvas 噪声未随画像变化", a.Canvas)
	}
	a2 := runProfile(t, *us)
	if a.Canvas != a2.Canvas {
		t.Fatal("同一环境两次渲染的 Canvas 读数不一致（噪声不可复现）")
	}
}

// TestInjectedScriptDeviceMemoryAndCores 内存与核数必须按画像覆盖，而不是本机真实值。
func TestInjectedScriptDeviceMemoryAndCores(t *testing.T) {
	p := builtinByID(t, "jp-mac-mba-m1") // DeviceMemory=8 Cores=8
	r := runProfile(t, p)
	if r.DeviceMemory != p.CPU.DeviceMemory {
		t.Fatalf("deviceMemory=%d，应为 %d", r.DeviceMemory, p.CPU.DeviceMemory)
	}
	if r.Cores != p.CPU.Cores {
		t.Fatalf("hardwareConcurrency=%d，应为 %d", r.Cores, p.CPU.Cores)
	}
}

// TestInjectedScriptWebGLUsesProfileGPU WebGL vendor/renderer 必须来自画像，
// 不能两环境都报本机那块 GTX 1060。
func TestInjectedScriptWebGLUsesProfileGPU(t *testing.T) {
	lib := fingerprint.NewLibrary(fingerprint.DefaultLibraryConfig(), 11)
	us, err := lib.Sample("US", "")
	if err != nil {
		t.Fatal(err)
	}
	jp, err2 := lib.Sample("JP", "")
	if err2 != nil {
		t.Fatal(err2)
	}
	a := runProfile(t, *us)
	b := runProfile(t, *jp)
	if a.WebGLRenderer == b.WebGLRenderer {
		t.Fatalf("两个环境 WebGL renderer 相同（%s），未随画像变化", a.WebGLRenderer)
	}
	if a.WebGLRenderer != us.GPU.WebGLRenderer || a.UnmaskedRenderer != us.GPU.Renderer {
		t.Fatalf("WebGL 读数与画像不符: %+v", a)
	}
}

// TestInjectedScriptCleansAutomationLeftovers cdc_ / __playwright 这类自动化残留必须清掉。
func TestInjectedScriptCleansAutomationLeftovers(t *testing.T) {
	r := runProfile(t, builtinByID(t, "us-win-dell-xps-8930"))
	if len(r.Automations) != 0 {
		t.Fatalf("自动化残留未清理: %v", r.Automations)
	}
}

// TestInjectedScriptPluginsLookNative Sannysoft 会查
// navigator.plugins instanceof PluginArray，纯数组伪造会留一行红。
func TestInjectedScriptPluginsLookNative(t *testing.T) {
	r := runProfile(t, builtinByID(t, "us-win-dell-xps-8930"))
	if !r.PluginsProto {
		t.Fatal("navigator.plugins 不是 PluginArray 实例（instanceof 检查会失败）")
	}
}

// TestScriptFromSnapshotRebuildsUsableScript 老环境（inject_script 为空）必须能补出
// **真正生效**的注入脚本，而不是只落一个标记对象。
func TestScriptFromSnapshotRebuildsUsableScript(t *testing.T) {
	lib := fingerprint.NewLibrary(fingerprint.DefaultLibraryConfig(), 3)
	p, err := lib.Sample("JP", "")
	if err != nil {
		t.Fatal(err)
	}
	script := fingerprint.ScriptFromSnapshot(p.ID, fingerprint.Capture(p).Values)
	r := runInjected(t, script)
	if r.WebdriverType != "undefined" {
		t.Fatalf("快照重建脚本未隐藏 webdriver: %s", r.Webdriver)
	}
	if r.Language != p.Locale || len(r.Languages) != len(p.Languages) {
		t.Fatalf("快照重建脚本语言不符: %+v (期望 %s %v)", r, p.Locale, p.Languages)
	}
	if r.Timezone != p.Timezone {
		t.Fatalf("快照重建脚本时区不符: %s（期望 %s）", r.Timezone, p.Timezone)
	}
	if r.Platform != p.OS.Platform {
		t.Fatalf("快照重建脚本 platform 不符: %s（期望 %s）", r.Platform, p.OS.Platform)
	}
	// 时区与语言的两处新覆盖点（Date 串 / Intl locale）也必须一起补齐
	if r.IntlLocale != p.Locale {
		t.Fatalf("快照重建脚本 Intl locale 不符: %q（期望 %q）", r.IntlLocale, p.Locale)
	}
	if !strings.Contains(r.DateToString, "GMT+0900") {
		t.Fatalf("快照重建脚本 Date 串时区不符: %q（期望含 GMT+0900）", r.DateToString)
	}
	if r.WebGLRenderer != p.GPU.WebGLRenderer {
		t.Fatalf("快照重建脚本 WebGL 不符: %s（期望 %s）", r.WebGLRenderer, p.GPU.WebGLRenderer)
	}
}
