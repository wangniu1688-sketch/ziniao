package network

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"

	"ziniao/fingerprint"
)

// helloIDs 内核基线 HelloID → uTLS ClientHelloID。
// 注意：uTLS 的基线更新滞后于 Chrome 正式版（例如 v1.6.7 无 HelloChrome_131），
// 缺失版本回退到最接近的基线并在基线库里标注 unverified，升级 uTLS 后需重新校准。
var helloIDs = map[string]utls.ClientHelloID{
	"HelloChrome_100":         utls.HelloChrome_100,
	"HelloChrome_102":         utls.HelloChrome_102,
	"HelloChrome_106_Shuffle": utls.HelloChrome_106_Shuffle,
	"HelloChrome_115_PQ":      utls.HelloChrome_115_PQ,
	"HelloChrome_120":         utls.HelloChrome_120,
	"HelloChrome_120_PQ":      utls.HelloChrome_120_PQ,
	"HelloChrome_Auto":        utls.HelloChrome_Auto,
	"HelloFirefox_102":        utls.HelloFirefox_102,
	"HelloFirefox_105":        utls.HelloFirefox_105,
	"HelloFirefox_120":        utls.HelloFirefox_120,
	"HelloFirefox_Auto":       utls.HelloFirefox_Auto,
}

// HelloIDForName 取 uTLS ClientHelloID（未知回退 Chrome Auto）。
func HelloIDForName(name string) utls.ClientHelloID {
	if id, ok := helloIDs[name]; ok {
		return id
	}
	return utls.HelloChrome_Auto
}

// AvailableHelloIDs 当前 uTLS 版本支持的基线名（供自检页展示）。
func AvailableHelloIDs() []string {
	out := make([]string, 0, len(helloIDs))
	for k := range helloIDs {
		out = append(out, k)
	}
	return out
}

// TLSProxy 本地前置代理：客户端 → 本机 CONNECT → 上游代理 → **uTLS 重建的 ClientHello** → 目标。
//
// 这是 Phase 1 的 TLS 指纹实现路径（ADR-0006）：不改 BoringSSL，而是在出口代理
// 用 uTLS 按内核基线重建 ClientHello，从而让服务端看到的 JA3/JA4 与声称的
// 浏览器内核版本严格对应（L2）。
type TLSProxy struct {
	upstream Dialer
	helloID  utls.ClientHelloID
	// helloName 构造时指定的基线名（唯一可信来源，见 HelloIDName 注释）
	helloName string
	baseline  *fingerprint.TLSBaseline
	timeout   time.Duration
	insecure  bool

	once     sync.Once
	realSpec fingerprint.ClientHelloSpec
	realErr  error
	ca       *MITMCA
	// rewriteCfg 请求头改写配置（UA 为空表示不改写：sidecar 未被下发该环境的 UA）
	rewriteCfg RewriteHeaders
	// http2 是否与目标协商 HTTP/2（见 TLSProxyConfig.HTTP2）
	http2 bool
}

// TLSProxyConfig 前置代理配置。
type TLSProxyConfig struct {
	Upstream ProxyConfig
	HelloID  string
	Baseline *fingerprint.TLSBaseline
	Timeout  time.Duration
	// CADir 本地 CA 目录（MITM 必需；为空则不加载 CA，CONNECT 全部失败）。
	CADir string
	// HTTP2 是否允许与目标协商 HTTP/2。**默认 false**：见 DialTLS 注释，
	// 开了以后请求头无法改写且实测链路不可用，等 HPACK 层改写实现后再开。
	HTTP2 bool
	// UA 该环境声称的完整 User-Agent（用于改写请求头，必须与 TLS 基线同源）。
	UA string
	// ChromeMajor 该环境声称的 Chrome 大版本（与 UA、TLS 基线同源）。
	ChromeMajor int
	// Platform UA-CH 的 platform（通常是 "Windows"）。
	Platform string
	// Insecure 跳过证书校验。**仅本地自测对着自签探针时用**，生产必须 false
	// （跳过校验 = 中间人可以伪造站点，与「账号安全」目标直接冲突）。
	Insecure bool
}

// NewTLSProxy 构造前置代理。
func NewTLSProxy(cfg TLSProxyConfig) *TLSProxy {
	to := cfg.Timeout
	if to <= 0 {
		to = 20 * time.Second
	}
	p := &TLSProxy{
		upstream:   NewDialer(cfg.Upstream),
		helloID:    HelloIDForName(cfg.HelloID),
		helloName:  strings.TrimSpace(cfg.HelloID),
		baseline:   cfg.Baseline,
		timeout:    to,
		insecure:   cfg.Insecure,
		http2:      cfg.HTTP2,
		rewriteCfg: RewriteHeaders{UA: cfg.UA, ChromeMajor: cfg.ChromeMajor, Platform: cfg.Platform},
	}
	if cfg.CADir != "" {
		if ca, err := LoadOrCreateCA(cfg.CADir); err == nil {
			p.ca = ca
		}
	}
	return p
}

// Spec 导出该代理实际会发出的 ClientHello（与传输层无关的指纹描述）。
//
// 取源规则：**真实字节优先**——让 uTLS 真发一次 ClientHello 并解析（RealClientHello）；
// 只有在自采样失败时才退回静态 spec 转换（并标记为 spec 来源）。
// 这样做的原因：uTLS 会把 Chrome 的扩展顺序随机化 GREASE，静态转换不易精确还原，
// 而电商平台看到的是线上字节，不是 spec。
func (p *TLSProxy) Spec() (fingerprint.ClientHelloSpec, error) {
	p.once.Do(func() {
		p.realSpec, p.realErr = RealClientHello(p.helloID)
	})
	if p.realErr == nil {
		return p.realSpec, nil
	}
	spec, err := utls.UTLSIdToSpec(p.helloID)
	if err != nil {
		return fingerprint.ClientHelloSpec{}, err
	}
	return toFingerprintSpec(spec), nil
}

// SpecSource 指纹来源：real=真实握手字节，spec=静态转换（降级）。
func (p *TLSProxy) SpecSource() string {
	_, err := p.Spec()
	if err != nil {
		return "unknown"
	}
	if p.realErr != nil {
		return "spec"
	}
	return "real"
}

// JA3 计算该代理实际发出的 JA3（raw, hash）。
func (p *TLSProxy) JA3() (string, string, error) {
	spec, err := p.Spec()
	if err != nil {
		return "", "", err
	}
	raw, hash := fingerprint.JA3From(spec)
	return raw, hash, nil
}

// JA4 计算该代理实际发出的 JA4。
func (p *TLSProxy) JA4() (string, error) {
	spec, err := p.Spec()
	if err != nil {
		return "", err
	}
	return fingerprint.JA4From(spec), nil
}

// MatchesBaseline 校验实际指纹是否与内核基线一致（启动前自检，L2/L8）。
func (p *TLSProxy) MatchesBaseline() (bool, string) {
	if p.baseline == nil {
		return false, "未指定 TLS 基线"
	}
	_, hash, err := p.JA3()
	if err != nil {
		return false, err.Error()
	}
	if p.baseline.JA3Hash == "" {
		return false, "基线 JA3 尚未计算"
	}
	return p.baseline.JA3Hash == hash, p.baseline.JA3Hash + " vs " + hash
}

// uClient 构造 uTLS 客户端连接。
//
// 默认（HTTP2=false）会把 ClientHello 里的 ALPN 限制为 http/1.1：
// 代理一旦与目标协商 h2，头部就变成 HPACK 编码的二进制帧——既无法改写身份头
// （sec-ch-ua 会暴露真实内核版本），实测连透传都会失败（浏览器侧是 h1 字节，
// 不能被塞进 h2 连接）。与其做一个对多数站点不可用的代理，不如先用 h1 保证
// 可用性与请求头一致性；真正的 h2 支持需要 HPACK 层改写或 fork Chromium。
//
// 注意：必须改 spec 里的 ALPNExtension——uTLS 用 HelloID 对应的完整 spec 装箱，
// 光设置 cfg.NextProtos 是无效的（实测仍协商出 h2）。
func (p *TLSProxy) uClient(conn net.Conn, cfg *utls.Config) (*utls.UConn, error) {
	if p.http2 {
		return utls.UClient(conn, cfg, p.helloID), nil
	}
	spec, err := utls.UTLSIdToSpec(p.helloID)
	if err != nil {
		return utls.UClient(conn, cfg, p.helloID), nil
	}
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			clone := *alpn
			clone.AlpnProtocols = []string{"http/1.1"}
			spec.Extensions[i] = &clone
		}
	}
	uconn := utls.UClient(conn, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		return nil, err
	}
	return uconn, nil
}

// DialTLS 经上游代理拨号并用 uTLS 完成 TLS 握手。
func (p *TLSProxy) DialTLS(ctx context.Context, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
		addr = net.JoinHostPort(addr, "443")
	}
	conn, err := p.upstream.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	cfg := &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: p.insecure}
	uconn, err := p.uClient(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	uconn.SetSNI(host)
	if dl, ok := ctx.Deadline(); ok {
		_ = uconn.SetDeadline(dl)
	} else {
		_ = uconn.SetDeadline(time.Now().Add(p.timeout))
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = uconn.SetDeadline(time.Time{})
	return uconn, nil
}

// Serve 启动本地前置代理：CONNECT 走 uTLS 隧道，/healthz 供客户端探活与核对指纹。
func (p *TLSProxy) Serve(ctx context.Context, listen string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	// 注意：不能用 http.ServeMux——它会把 CONNECT 请求当成「需要补齐路径的
	// 普通请求」并返回 301 Location:/（host:port 被当作 path），隧道永远建不起来。
	// 必须自己在单一 handler 里按方法分流。
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				p.handleConnect(w, r)
				return
			}
			p.handleHealth(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.Serve(ln)
}

// handleHealth 探活 + 指纹自检：客户端据此确认代理已就绪、并核对实际 JA3/JA4。
// 不返回任何凭证，仅指纹描述（L4）。
func (p *TLSProxy) handleHealth(w http.ResponseWriter, _ *http.Request) {
	raw, hash, err := p.JA3()
	if err != nil {
		http.Error(w, "ja3 计算失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ja4, err := p.JA4()
	if err != nil {
		http.Error(w, "ja4 计算失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type resp struct {
		Status  string `json:"status"`
		HelloID string `json:"hello_id"`
		JA3Raw  string `json:"ja3_raw"`
		JA3Hash string `json:"ja3_hash"`
		JA4     string `json:"ja4"`
		Source  string `json:"source"`
		Note    string `json:"note,omitempty"`
	}
	w.Header().Set("Content-Type", "application/json")
	note := "JA3 因 Chrome 扩展顺序随机化会逐连接变化，稳定的一致性依据以 JA4 为准"
	_ = json.NewEncoder(w).Encode(resp{
		Status: "ok", HelloID: p.HelloIDName(), JA3Raw: raw, JA3Hash: hash, JA4: ja4,
		Source: p.SpecSource(), Note: note,
	})
}

// HelloIDName 当前生效的 HelloID 名（排障/审计用）。
//
// 必须返回**构造时传入的名字**，不能靠反查 helloIDs map：uTLS 里
// `HelloChrome_Auto = HelloChrome_120`（同一个 ClientHelloID 值互为别名），
// 反查时命中哪个取决于 Go map 的随机迭代顺序——实测同样 `--kernel 120`
// 的进程会随机报 Auto 或 120，审计口径直接失效。
func (p *TLSProxy) HelloIDName() string {
	if p.helloName != "" {
		return p.helloName
	}
	for name, id := range helloIDs {
		if id == p.helloID {
			return name
		}
	}
	return "HelloChrome_Auto"
}

// CA 本地 MITM 根证书（未配置时为 nil）。
func (p *TLSProxy) CA() *MITMCA { return p.ca }

// handleConnect 处理 CONNECT（MITM 架构，指纹浏览器的标准做法）：
//
//	客户端 ──TLS(本地CA动态签发的目标证书)──> sidecar ──TLS(uTLS重建ClientHello)──> 目标
//
// 为什么必须终结 TLS：隧道里客户端自己会跟目标握手，代理无法在不解密的情况下
// 替换 ClientHello——之前的"透传+已建立连接"写法会把客户端的 ClientHello 再加密
// 一次塞进 uTLS 连接（双重 TLS），目标端协议错乱（ERR_SSL_PROTOCOL_ERROR）。
// 两端各自完成 TLS、代理在中间明文桥接，是唯一正确形态；这也是所有指纹
// 浏览器都要装根证书的原因。
func (p *TLSProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "仅支持 CONNECT", http.StatusMethodNotAllowed)
		return
	}
	if p.ca == nil {
		http.Error(w, "本地 CA 未初始化", http.StatusInternalServerError)
		return
	}
	target := r.URL.Host
	if !strings.Contains(target, ":") {
		target = net.JoinHostPort(target, "443")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持 Hijack", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		http.Error(w, "Hijack 失败", http.StatusInternalServerError)
		return
	}
	// 1) 先答应隧道（客户端此时才开始 TLS 握手）
	if _, err := buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		_ = buf.Flush()
	}
	// 2) 先与目标握手：必须知道目标协商出的 ALPN，才能在下一步给客户端同样的选择。
	// 顺序不能反——两侧 ALPN 不一致时，h1 的请求字节会被送进 h2 连接（或反之），
	// 而字节搬运无法跨协议，页面必然打不开（实测 code=000）。
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	upstream, err := p.DialTLS(ctx, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mitm: 与目标握手失败 target=%s err=%v\n", target, err)
		_ = client.Close()
		return
	}
	defer func() { _ = upstream.Close() }()
	alpn := ""
	if uc, ok := upstream.(*utls.UConn); ok {
		alpn = uc.ConnectionState().NegotiatedProtocol
	}
	// 3) 对客户端伪装成目标站点：本地 CA 动态签发目标证书；
	// ALPN 必须与目标协商结果一致（目标支持 h2 就给 h2，否则给 h1）。
	srvTLS := p.ca.ServerTLSConfigFor(r.URL.Host)
	if alpn != "" {
		srvTLS.NextProtos = []string{alpn}
	}
	tlsConn := tls.Server(client, srvTLS)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "mitm: 与客户端握手失败 target=%s err=%v\n", target, err)
		_ = client.Close()
		return
	}
	defer func() { _ = tlsConn.Close() }()
	// 4) 明文桥接（两侧各自 TLS）
	// 客户端→目标这段是明文 HTTP：在这里改写身份头（sec-ch-ua 是浏览器自动加的，
	// JS 改不掉，不改写就会出现「UA 说 120、请求头说 156」的自相矛盾）
	done := make(chan struct{}, 2)
	go func() {
		if cfg := p.rewriteCfg; cfg.UA != "" {
			_ = copyRewritingRequests(upstream, tlsConn, cfg)
		} else {
			_, _ = io.Copy(upstream, tlsConn)
		}
		done <- struct{}{}
	}()
	go func() { _, _ = io.Copy(tlsConn, upstream); done <- struct{}{} }()
	<-done
	_ = client.Close()
}

// ---------- utls spec → fingerprint spec ----------

func toFingerprintSpec(spec utls.ClientHelloSpec) fingerprint.ClientHelloSpec {
	out := fingerprint.ClientHelloSpec{
		// Chrome 的 ClientHello 里 legacy_version 恒为 TLS1.2(0x0303)，
		// TLS1.3 由 supported_versions 扩展声明，这是 JA3 规范的取值口径。
		Version: 0x0303,
	}
	// 必须过滤 GREASE：uTLS spec 里含 0x0a0a 等占位套件，而线上字节里的 GREASE 每次随机、
	// JA3/JA4 规范也要求排除。不过滤会得到 16 个套件（真实只有 15 个），
	// 算出的指纹与电商平台实际看到的永远对不上。
	for _, c := range spec.CipherSuites {
		if isGREASE(c) {
			continue
		}
		out.CipherSuites = append(out.CipherSuites, c)
	}
	for _, ext := range spec.Extensions {
		id, ok := extType(ext)
		if !ok {
			continue
		}
		out.Extensions = append(out.Extensions, id)
		switch e := ext.(type) {
		case *utls.SupportedCurvesExtension:
			for _, c := range e.Curves {
				if isGREASE(uint16(c)) {
					continue
				}
				out.Curves = append(out.Curves, uint16(c))
			}
		case *utls.SupportedPointsExtension:
			out.PointFormats = append(out.PointFormats, e.SupportedPoints...)
		case *utls.ALPNExtension:
			out.ALPN = append(out.ALPN, e.AlpnProtocols...)
		case *utls.SignatureAlgorithmsExtension:
			for _, s := range e.SupportedSignatureAlgorithms {
				out.SigAlgs = append(out.SigAlgs, uint16(s))
			}
		}
	}
	return out
}

// extType 把 uTLS 扩展映射到 TLS 扩展数值；GREASE 随机扩展必须排除（否则 JA3 每次都变）。
func extType(ext utls.TLSExtension) (uint16, bool) {
	switch e := ext.(type) {
	case *utls.SNIExtension:
		return 0, true
	case *utls.SupportedCurvesExtension:
		return 10, true
	case *utls.SupportedPointsExtension:
		return 11, true
	case *utls.SignatureAlgorithmsExtension:
		return 13, true
	case *utls.ALPNExtension:
		return 16, true
	case *utls.SCTExtension:
		return 18, true
	case *utls.UtlsPaddingExtension:
		return 21, true
	case *utls.ExtendedMasterSecretExtension:
		return 23, true
	case *utls.UtlsCompressCertExtension:
		return 27, true
	case *utls.FakeRecordSizeLimitExtension:
		return 28, true
	case *utls.StatusRequestExtension:
		return 5, true
	case *utls.CookieExtension:
		return 44, true
	case *utls.PSKKeyExchangeModesExtension:
		return 45, true
	case *utls.SupportedVersionsExtension:
		return 43, true
	case *utls.KeyShareExtension:
		return 51, true
	case *utls.QUICTransportParametersExtension:
		return 57, true
	case *utls.NPNExtension:
		return 13172, true
	case *utls.ApplicationSettingsExtension:
		return 17513, true
	case *utls.RenegotiationInfoExtension:
		return 65281, true
	case *utls.GenericExtension:
		return e.Id, true
	case *utls.UtlsGREASEExtension:
		// GREASE 值每次随机：JA3/JA4 必须排除，否则指纹不可复现
		return 0, false
	}
	return 0, false
}
