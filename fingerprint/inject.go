package fingerprint

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
		{Field: "navigator.userAgent", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "UA 在 JS 层可覆盖，但 UA-CH/HTTP 头不一致会暴露；正式实现应在 C++ 层与网络层同源"},
		{Field: "navigator.userAgentData", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "UA-CH 需 C++ 或启动参数 + JS 补齐，见 M0.3"},
		{Field: "navigator.platform", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "navigator.hardwareConcurrency", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "navigator.deviceMemory", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "navigator.languages", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "navigator.plugins", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "JS 可伪造 MimeTypeArray；插件列表必须与画像整体一致（反例 5）"},
		{Field: "screen.width/height/dpr", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "JS 覆盖 screen 对象；窗口真实尺寸由 C++ 层保证"},
		{Field: "WebGL vendor/renderer(UNMASKED)", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "必须 C++ 层注入；JS 层 hook getParameter 对多数检测站有效但不是强保证"},
		{Field: "Canvas 噪声", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "AudioContext 噪声", Layer: LayerJS, Detectable: false, Injectable: true},
		{Field: "字体列表", Layer: LayerCPP, Detectable: true, Injectable: false, Note: "TODO(P1)：OS 层 fontconfig/DirectWrite 注入；当前仅做画像一致性校验"},
		{Field: "Intl/时区/语言", Layer: LayerCPP, Detectable: true, Injectable: true, Note: "JS 覆盖 Intl.DateTimeFormat 与 Date#getTimezoneOffset"},
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
func InjectionScript(p *Profile) string {
	cfg, err := json.Marshal(map[string]any{
		"ua":                  p.UA,
		"platform":            p.OS.Platform,
		"vendor":              browserVendor(p),
		"hardwareConcurrency": p.CPU.Cores,
		"deviceMemory":        p.CPU.DeviceMemory,
		"languages":           p.Languages,
		"language":            p.Locale,
		"screen":              map[string]any{"width": p.Screen.Width, "height": p.Screen.Height, "availWidth": p.Screen.AvailWidth, "availHeight": p.Screen.AvailHeight, "colorDepth": p.Screen.ColorDepth, "pixelDepth": p.Screen.ColorDepth},
		"dpr":                 p.Screen.DPR,
		"timezone":            p.Timezone,
		"utcOffsetMinutes":    p.UTCOffset * 60,
		"webglVendor":         p.GPU.WebGLVendor,
		"webglRenderer":       p.GPU.WebGLRenderer,
		"unmaskedVendor":      p.GPU.Vendor,
		"unmaskedRenderer":    p.GPU.Renderer,
		"canvasSeed":          p.CanvasSeed,
		"audioSeed":           p.AudioSeed,
		"plugins":             p.Plugins,
		"maxTouchPoints":      p.Screen.TouchPoints,
		"webRTCMode":          p.WebRTC.Mode,
	})
	if err != nil {
		return ""
	}
	return fmt.Sprintf(`(function(){try{
var C = %s;
var def=function(o,k,v){try{Object.defineProperty(o,k,{get:function(){return v;},configurable:true});}catch(e){}};
// ---- navigator ----
var N=navigator;
def(N,'platform',C.platform);def(N,'vendor',C.vendor);
def(N,'hardwareConcurrency',C.hardwareConcurrency);def(N,'deviceMemory',C.deviceMemory);
def(N,'language',C.language);def(N,'languages',C.languages);
def(N,'maxTouchPoints',C.maxTouchPoints);
try{Object.defineProperty(N,'userAgent',{get:function(){return C.ua;},configurable:true});}catch(e){}
if(!N.userAgentData){def(N,'userAgentData',{brands:[{brand:'Chromium',version:C.ua.match(/Chrome\/(\d+)/)?RegExp.$1:'131'}],mobile:false,platform:C.platform});}
// ---- plugins / mimeTypes（必须与画像整体一致，见反例 5）----
function MT(name,type,desc,suffix){this.name=name;this.type=type;this.description=desc;this.suffixes=suffix||'';}
var pl=C.plugins.map(function(x){var o=new MT(x.name,'application/pdf',x.description,'pdf');o.filename=x.filename;o.length=1;return o;});
var arr=[];for(var i=0;i<pl.length;i++){arr[i]=pl[i];}
arr.item=function(i){return pl[i];};arr.namedItem=function(n){for(var j=0;j<pl.length;j++){if(pl[j].name===n)return pl[j];}return null;};arr.refresh=function(){};
def(N,'plugins',arr);
def(N,'mimeTypes',(function(){var m=[];C.plugins.forEach(function(x){(x.mime_types||[]).forEach(function(t){m.push(new MT(x.name,t,x.description,'pdf'));});});
  m.item=function(i){return m[i];};m.namedItem=function(n){return null;};return m;})());
// ---- screen ----
def(window.screen,'width',C.screen.width);def(window.screen,'height',C.screen.height);
def(window.screen,'availWidth',C.screen.availWidth);def(window.screen,'availHeight',C.screen.availHeight);
def(window.screen,'colorDepth',C.screen.colorDepth);def(window.screen,'pixelDepth',C.screen.pixelDepth);
def(window,'devicePixelRatio',C.dpr);
// ---- 时区与语言（Intl + Date）----
// 修正：此前用 _Resolved.constructor（即 Function）当构造器，页面一旦调用
// Intl.DateTimeFormat() 就抛异常并中断页面后续脚本（反例：实测页半屏空白）。
var _DTF=Intl.DateTimeFormat;
function ZDTF(l,o){o=o||{};if(!o.timeZone){o.timeZone=C.timezone;}return new _DTF(l,o);}
ZDTF.prototype=_DTF.prototype;ZDTF.supportedLocalesOf=_DTF.supportedLocalesOf;
Intl.DateTimeFormat=ZDTF;
Date.prototype.getTimezoneOffset=function(){return -C.utcOffsetMinutes;};
// ---- WebGL（UNMASKED 参数）----
function hookGL(proto){if(!proto)return;var gp=proto.getParameter;
  proto.getParameter=function(p){
    if(p===37445)return C.unmaskedVendor;      // UNMASKED_VENDOR_WEBGL
    if(p===37446)return C.unmaskedRenderer;    // UNMASKED_RENDERER_WEBGL
    if(p===7938)return C.webglRenderer;        // RENDERER
    if(p===7936)return C.webglVendor;          // VENDOR
    if(p===7937)return 'WebGL 1.0 (OpenGL ES 2.0 Chromium)';
    if(p===35724)return 'WebGL GLSL ES 1.0 (OpenGL ES GLSL ES 1.0 Chromium)';
    return gp.apply(this,arguments);};
  var gi=proto.getExtension;proto.getExtension=function(n){if(n==='WEBGL_debug_renderer_info'){return {UNMASKED_VENDOR_WEBGL:37445,UNMASKED_RENDERER_WEBGL:37446};}return gi.apply(this,arguments);};}
if(window.WebGLRenderingContext){hookGL(WebGLRenderingContext.prototype);
  if(window.WebGL2RenderingContext)hookGL(WebGL2RenderingContext.prototype);}
// ---- Canvas 噪声（种子化，稳定且与画像绑定）----
var seed=parseInt(C.canvasSeed.slice(0,8),16)||1;
var _gd=CanvasRenderingContext2D.prototype.getImageData;
CanvasRenderingContext2D.prototype.getImageData=function(x,y,w,h){var d=_gd.apply(this,arguments);
  for(var i=0;i<d.data.length;i+=4096){d.data[i]=(d.data[i]+(seed%%7))&255;}return d;};
var _td=HTMLCanvasElement.prototype.toDataURL;
HTMLCanvasElement.prototype.toDataURL=function(){var u=_td.apply(this,arguments);
  return u.slice(0,u.length-1)+((seed%%36).toString(36));};
// ---- Audio 噪声 ----
var aseg=parseInt(C.audioSeed.slice(0,8),16)||1;
if(window.AudioBuffer){var _gc=AudioBuffer.prototype.getChannelData;
  AudioBuffer.prototype.getChannelData=function(ch){var a=_gc.apply(this,arguments);
    for(var i=0;i<Math.min(a.length,1024);i+=97){a[i]=a[i]+((aseg%%13)*1e-7);}return a;};}
// ---- WebRTC 策略（C++ 层 TODO，此处仅做兜底提示）----
if(C.webRTCMode==='disabled'&&window.RTCPeerConnection){try{delete window.RTCPeerConnection;}catch(e){window.RTCPeerConnection=undefined;}}
}catch(e){}})();`, string(cfg))
}

func browserVendor(p *Profile) string {
	switch p.OS.Name {
	case "macOS":
		return "Apple Computer, Inc."
	case "Linux":
		return "Google Inc."
	default:
		return "Google Inc."
	}
}

// ScriptSummary 脚本摘要（用于审计与自检报告，不含完整脚本）。
func ScriptSummary(script string) string {
	if len(script) == 0 {
		return ""
	}
	lines := strings.Count(script, "\n")
	return fmt.Sprintf("js:%d bytes, %d lines", len(script), lines+1)
}
