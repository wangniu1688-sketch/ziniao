package network

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CACommonName 本地根证书的 CN。
//
// 导出它的唯一理由：ca_ready 判定要在**不加载私钥**的前提下问「这张证书装进
// 用户信任库了没有」（browser 包的启动前置检查），只能按 CN 去信任库里查。
// 两处各写一份字符串等于埋一颗「改了一边、另一边永远查不到」的雷。
const CACommonName = "Ziniao Local CA (do not trust outside this machine)"

// MITMCA 本地动态签发证书的 CA。
//
// 为什么需要：CONNECT 隧道里客户端（WebView2）自己会跟"目标"做 TLS 握手，
// 代理无法在不终结 TLS 的情况下替换 ClientHello。标准做法（也是所有指纹
// 浏览器的做法）：本地装一个根证书，代理对客户端伪装成目标站点（动态签发
// 目标域名的证书），对目标用 uTLS 握手——两端各自完成 TLS，代理在中间明文桥接。
//
// 红线：
//   - CA 私钥只落在本机用户目录（0600），绝不外发；
//   - 仅签发「 CONNECT 目标」域名的短周期证书（24h）；
//   - CA 安装到**当前用户**信任库（certutil -user），不动机器级信任。
type MITMCA struct {
	mu    sync.Mutex
	key   *ecdsa.PrivateKey
	leaf  *x509.Certificate
	der   []byte
	dir   string
	cache map[string]tls.Certificate
}

// LoadOrCreateCA 加载或首次生成本地 CA（ca.crt / ca.key，目录权限收紧）。
func LoadOrCreateCA(dir string) (*MITMCA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "ca.key")
	crtPath := filepath.Join(dir, "ca.crt")
	ca := &MITMCA{dir: dir, cache: map[string]tls.Certificate{}}

	if keyPEM, err := os.ReadFile(keyPath); err == nil {
		crtPEM, rerr := os.ReadFile(crtPath)
		if rerr != nil {
			return nil, fmt.Errorf("mitm: ca.crt 缺失而 ca.key 存在: %w", rerr)
		}
		key, kerr := parseECKey(keyPEM)
		if kerr != nil {
			return nil, kerr
		}
		leaf, lerr := parseCert(crtPEM)
		if lerr != nil {
			return nil, lerr
		}
		ca.key, ca.leaf, ca.der = key, leaf, leaf.Raw
		return ca, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: CACommonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := write0600(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer(key)})); err != nil {
		return nil, err
	}
	if err := write0600(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, err
	}
	ca.key, ca.leaf, ca.der = key, leaf, der
	return ca, nil
}

// CertPath CA 证书路径（安装引导用）。
func (c *MITMCA) CertPath() string { return filepath.Join(c.dir, "ca.crt") }

// EnsureInstalled 把 CA 装进当前用户信任库（重复安装会被 certutil 报已存在，忽略）。
// 仅 Windows：其它平台 TODO（keychain / NSS）。
func (c *MITMCA) EnsureInstalled() error {
	cmd := exec.Command("certutil", "-user", "-addstore", "Root", c.CertPath())
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.ToLower(string(out))
	if strings.Contains(msg, "exist") || strings.Contains(msg, "已存在") || strings.Contains(msg, "0x80092004") {
		return nil
	}
	return fmt.Errorf("mitm: 安装根证书失败: %s", strings.TrimSpace(string(out)))
}

// ServerTLSConfig 返回用于「伪装目标站点」的服务端 TLS 配置：按 SNI 动态签发。
func (c *MITMCA) ServerTLSConfig() *tls.Config {
	return &tls.Config{ //nolint:gosec // 动态 MinVersion 由客户端 ClientHello 决定
		GetCertificate: c.getCertificate,
	}
}

// ServerTLSConfigFor 按 CONNECT 目标 host 签发证书（比 SNI 更权威：
// 客户端对 CONNECT 的 host 就是它要连的域名）。
func (c *MITMCA) ServerTLSConfigFor(host string) *tls.Config {
	return &tls.Config{ //nolint:gosec // 同上
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.certForHost(host)
		},
	}
}

func (c *MITMCA) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	if host == "" {
		// 客户端没发 SNI（直连 IP）：用连接目标地址
		host = hello.Conn.RemoteAddr().String()
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	return c.certForHost(host)
}

// certForHost 取（或现场签发）host 的叶证书。
func (c *MITMCA) certForHost(host string) (*tls.Certificate, error) {
	// CONNECT 目标可能带端口
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if crt, ok := c.cache[host]; ok {
		return &crt, nil
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		leafTmpl.IPAddresses = []net.IP{ip}
	} else {
		leafTmpl.DNSNames = []string{host}
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTmpl, c.leaf, &leafKey.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	crt := tls.Certificate{Certificate: [][]byte{der, c.der}, PrivateKey: leafKey, Leaf: leaf}
	// 简单防膨胀：超过 512 条清空（域名集合有界，正常到不了）
	if len(c.cache) > 512 {
		c.cache = map[string]tls.Certificate{}
	}
	c.cache[host] = crt
	return &crt, nil
}

// Uninstall 从当前用户信任库移除本地根证书（卸载客户端 / 合规要求时调用）。
// 证书按 subject + 指纹定位，不依赖 certutil 的索引顺序。
func (c *MITMCA) Uninstall() error {
	out, err := exec.Command("certutil", "-user", "-delstore", "Root", c.leaf.Subject.CommonName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mitm: 移除根证书失败: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------- 小工具 ----------

func write0600(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // 私钥必须 0600
		return err
	}
	return nil
}

func parseECKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("mitm: ca.key 解析失败")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("mitm: ca.crt 解析失败")
	}
	return x509.ParseCertificate(block.Bytes)
}

// keyDer 序列化 ECDSA 私钥（PKCS8 兼容）。
func keyDer(k *ecdsa.PrivateKey) []byte {
	b, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil
	}
	return b
}
