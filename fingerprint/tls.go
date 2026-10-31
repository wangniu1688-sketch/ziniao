package fingerprint

import (
	"crypto/md5" //nolint:gosec // JA3 规范指定 MD5，非安全用途
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClientHelloSpec 与传输层实现无关的 ClientHello 描述。
// 目的：fingerprint 包不依赖 uTLS 也能计算 JA3/JA4（网络层负责把 utls 的 spec 转成它）。
type ClientHelloSpec struct {
	Version      uint16   // 0x0301(TLS1.0) ... 0x0304(TLS1.3)；ClientHello 里通常握手版本 0x0303
	CipherSuites []uint16 // 不含 GREASE
	Extensions   []uint16 // 不含 GREASE
	Curves       []uint16 // supported_groups
	PointFormats []uint8  // ec_point_formats
	ALPN         []string
	SigAlgs      []uint16
}

// JA3From 按 JA3 规范计算：
// TLSVersion,Ciphers,Extensions,Curves,PointFormats → MD5。
func JA3From(s ClientHelloSpec) (raw, hash string) {
	ciphers := []string{}
	for _, c := range s.CipherSuites {
		ciphers = append(ciphers, strconv.Itoa(int(c)))
	}
	exts := []string{}
	for _, e := range s.Extensions {
		exts = append(exts, strconv.Itoa(int(e)))
	}
	curves := []string{}
	for _, c := range s.Curves {
		curves = append(curves, strconv.Itoa(int(c)))
	}
	pfs := []string{}
	for _, p := range s.PointFormats {
		pfs = append(pfs, strconv.Itoa(int(p)))
	}
	raw = strings.Join([]string{
		strconv.Itoa(int(s.Version)),
		strings.Join(ciphers, "-"),
		strings.Join(exts, "-"),
		strings.Join(curves, "-"),
		strings.Join(pfs, "-"),
	}, ",")
	sum := md5.Sum([]byte(raw))
	return raw, hex.EncodeToString(sum[:])
}

// JA4From 按 JA4 规范计算 a_b_c：
//
//	a = <proto><tls_version><sni><ciphers_count><exts_count><alpn_first_last>
//	b = sha256(sorted ciphers hex)[:12]
//	c = sha256(sorted extensions hex + sigalgs)[:12]
//
// 说明：sni 位固定为 "d"（发起 SNI），ALPN 取首末字符；这是规范定义的简化表示，
// 与 foxio/ja4 官方实现对齐；未涵盖的罕见场景会在采集器交叉校验中被发现。
func JA4From(s ClientHelloSpec) string {
	proto := "t" // TCP
	version := ja4Version(s.Version)
	ciphers := append([]uint16{}, s.CipherSuites...)
	sort.Slice(ciphers, func(i, j int) bool { return ciphers[i] < ciphers[j] })
	exts := append([]uint16{}, s.Extensions...)
	sort.Slice(exts, func(i, j int) bool { return exts[i] < exts[j] })

	var cipherHex []string
	for _, c := range ciphers {
		cipherHex = append(cipherHex, fmt.Sprintf("%04x", c))
	}
	var extHex []string
	for _, e := range exts {
		extHex = append(extHex, fmt.Sprintf("%04x", e))
	}
	var sigHex []string
	for _, g := range s.SigAlgs {
		sigHex = append(sigHex, fmt.Sprintf("%04x", g))
	}

	b := sha256.Sum256([]byte(strings.Join(cipherHex, ",")))
	c := sha256.Sum256([]byte(strings.Join(extHex, ",") + "_" + strings.Join(sigHex, ",")))

	alpn := "00"
	if len(s.ALPN) > 0 {
		first := s.ALPN[0]
		last := s.ALPN[len(s.ALPN)-1]
		alpn = string(first[0]) + string(last[len(last)-1])
	}
	a := proto + version + "d" + fmt.Sprintf("%02d", len(ciphers)) + fmt.Sprintf("%02d", len(exts)) + alpn
	return a + "_" + hex.EncodeToString(b[:])[:12] + "_" + hex.EncodeToString(c[:])[:12]
}

func ja4Version(v uint16) string {
	switch v {
	case 0x0304:
		return "13"
	case 0x0303:
		return "12"
	case 0x0302:
		return "11"
	case 0x0301:
		return "10"
	default:
		return "00"
	}
}

// TLSBaseline 某个 Chromium 版本对应的 TLS 指纹基线（L2 / L8）。
type TLSBaseline struct {
	ChromiumMajor   int       `json:"chromium_major"`
	ChromiumVersion string    `json:"chromium_version"`
	HelloID         string    `json:"hello_id"` // utls HelloID 名
	JA3             string    `json:"ja3"`
	JA3Hash         string    `json:"ja3_hash"`
	JA4             string    `json:"ja4"`
	HTTP2Settings   string    `json:"http2_settings"` // "1:65536;2:0;3:1000;4:6291456;6:262144"
	HeaderOrder     []string  `json:"header_order"`   // HTTP/2 伪首部与普通首部顺序
	ALPN            []string  `json:"alpn"`
	Source          string    `json:"source"` // 数据来源说明（unverified|utls|collected）
	CollectedAt     time.Time `json:"collected_at,omitempty"`
}

// ---------- 基线库 ----------

//go:embed data/tls_baseline.json
var baselineFS embed.FS

// BaselineLibrary TLS 基线库：内核版本 → 基线（版本强绑定）。
type BaselineLibrary struct {
	mu        sync.RWMutex
	byMajor   map[int]TLSBaseline
	runtimeOK bool // 是否已由网络层用真实 ClientHello 计算过
}

// LoadBaselines 从内置 JSON 加载基线。
func LoadBaselines() (*BaselineLibrary, error) {
	b, err := baselineFS.ReadFile("data/tls_baseline.json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Baselines []TLSBaseline `json:"baselines"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("fingerprint: 基线文件解析失败: %w", err)
	}
	l := &BaselineLibrary{byMajor: map[int]TLSBaseline{}}
	for _, bl := range doc.Baselines {
		l.byMajor[bl.ChromiumMajor] = bl
	}
	return l, nil
}

// For 取某内核大版本的基线。
func (l *BaselineLibrary) For(major int) (*TLSBaseline, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	bl, ok := l.byMajor[major]
	if !ok {
		return nil, false
	}
	return &bl, true
}

// ChromeMajorFromUA 从任意 UA 串解析 Chrome 大版本（导出版，供其它包对齐身份用）。
func ChromeMajorFromUA(ua string) (int, error) {
	return chromeMajorFromUA(ua)
}

// ParseChromeMajorFromHelloID 从 uTLS 的 HelloID 名里解析它**实际代表**的 Chrome 大版本
// （HelloChrome_120_PQ → 120；HelloChrome_Auto → 0，表示不绑定版本）。
//
// 为什么必须有这个函数：uTLS 基线滞后于 Chrome 正式版（v1.6.7 里没有 HelloChrome_131），
// 于是 131 只能复用 HelloChrome_120_PQ——**代理实际发出的是 120 的握手**。
// 如果 UA 仍声称 Chrome/131，平台把 UA 与 JA3/JA4 交叉一比就能看穿：
// 「UA 说 131，TLS 说 120」在多账号防关联里是致命不自洽，比指纹雷同还危险。
func ParseChromeMajorFromHelloID(name string) int {
	const prefix = "HelloChrome_"
	if !strings.HasPrefix(name, prefix) {
		return 0
	}
	rest := strings.TrimPrefix(name, prefix)
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0 // HelloChrome_Auto 之类：不与具体版本绑定，不做对齐
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}

// AlignUAChromeVersion 把 UA 里的 Chrome/x.y.z.w 换成目标版本，让 UA 与
// 「代理实际发出的 ClientHello」同源。version 为空或 UA 里没有 Chrome/ 段则原样返回。
func AlignUAChromeVersion(ua, version string) string {
	const key = "Chrome/"
	i := strings.Index(ua, key)
	if i < 0 || version == "" {
		return ua
	}
	end := i + len(key)
	for end < len(ua) && (ua[end] == '.' || (ua[end] >= '0' && ua[end] <= '9')) {
		end++
	}
	return ua[:i] + key + version + ua[end:]
}

// RegisterComputed 由网络层在启动时用真实 ClientHello（uTLS 构造）计算并回写 JA3/JA4。
// 这样「基线与代理实际发出的指纹」必然同源可比对；与真实 Chrome 是否一致由采集器校准。
func (l *BaselineLibrary) RegisterComputed(major int, ja3, ja3Hash, ja4, source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bl := l.byMajor[major]
	bl.ChromiumMajor = major
	bl.JA3 = ja3
	bl.JA3Hash = ja3Hash
	bl.JA4 = ja4
	bl.Source = source
	l.byMajor[major] = bl
	l.runtimeOK = true
}

// Computed 是否已用真实 ClientHello 计算过。
func (l *BaselineLibrary) Computed() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.runtimeOK
}

// BaselineHash 全部基线的稳定哈希（离线启动校验用，L8）。
func (l *BaselineLibrary) BaselineHash() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	majors := make([]int, 0, len(l.byMajor))
	for m := range l.byMajor {
		majors = append(majors, m)
	}
	sort.Ints(majors)
	parts := []string{}
	for _, m := range majors {
		bl := l.byMajor[m]
		parts = append(parts, fmt.Sprintf("%d:%s:%s:%s:%s", m, bl.JA3Hash, bl.JA4, bl.HTTP2Settings, strings.Join(bl.HeaderOrder, ",")))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// Verify 校验客户端上报的基线哈希是否与服务端一致（离线启动前必检，L8）。
func (l *BaselineLibrary) Verify(major int, clientHash string) (bool, string) {
	bl, ok := l.For(major)
	if !ok {
		return false, fmt.Sprintf("无内核 v%d 的 TLS 基线", major)
	}
	want := bl.JA3Hash
	if want == "" {
		return false, "基线尚未计算（未启动网络层或未采集）"
	}
	return want == clientHash, want
}
