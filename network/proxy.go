// Package network 网络与 IP 资源：代理拨号、TLS 指纹（JA3/JA4）重建、
// IP 纯净度四维检测、健康评分与调度（Q4 / Q6 / Q7）。
package network

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Protocol 代理协议。
type Protocol string

const (
	ProtoDirect Protocol = "direct"
	ProtoHTTP   Protocol = "http"
	ProtoHTTPS  Protocol = "https"
	ProtoSOCKS5 Protocol = "socks5"
)

// ASNType 出口 IP 的 ASN 类型（住宅 / 机房 / 移动 / 运营商）。
type ASNType string

const (
	ASNResidential ASNType = "residential"
	ASNISP         ASNType = "isp"
	ASNMobile      ASNType = "mobile"
	ASNDatacenter  ASNType = "datacenter"
	ASNUnknown     ASNType = "unknown"
)

// ProxyConfig 一条网络资源/代理配置。
// L4：Password 不得进日志、不得进 API 响应（序列化时已 omitempty + json:"-"）。
type ProxyConfig struct {
	ID       string   `json:"id"`
	Protocol Protocol `json:"protocol"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"-"`
	Country  string   `json:"country"`
	Region   string   `json:"region,omitempty"`
	City     string   `json:"city,omitempty"`
	ASN      string   `json:"asn,omitempty"`
	ASNType  ASNType  `json:"asn_type"`
}

// Addr 返回 host:port。
func (c ProxyConfig) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// IsDirect 是否直连（无代理）。
func (c ProxyConfig) IsDirect() bool {
	return c.Protocol == ProtoDirect || c.Protocol == "" || c.Host == ""
}

// Dialer 与传输层解耦的拨号接口（便于测试与 mock）。
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewDialer 按协议构造拨号器：直连 / HTTP CONNECT / SOCKS5。
func NewDialer(cfg ProxyConfig) Dialer {
	base := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	if cfg.IsDirect() {
		return base
	}
	switch cfg.Protocol {
	case ProtoSOCKS5:
		return &socks5Dialer{cfg: cfg, base: base}
	default: // http / https
		return &httpConnectDialer{cfg: cfg, base: base}
	}
}

// ---------- HTTP CONNECT ----------

type httpConnectDialer struct {
	cfg  ProxyConfig
	base *net.Dialer
}

func (d *httpConnectDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("network: 不支持的 network=%s", network)
	}
	conn, err := d.base.DialContext(ctx, "tcp", d.cfg.Addr())
	if err != nil {
		return nil, err
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if d.cfg.Username != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(d.cfg.Username + ":" + d.cfg.Password))
		req += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	req += "\r\n"
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	parts := strings.Fields(line)
	if len(parts) < 2 || !(parts[1] == "200" || strings.HasPrefix(parts[1], "2")) {
		_ = conn.Close()
		return nil, fmt.Errorf("network: 代理 CONNECT 失败: %s", strings.TrimSpace(line))
	}
	// 读空剩余响应头
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" || l == "\n" {
			break
		}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// ---------- SOCKS5（自实现，避免额外依赖） ----------

type socks5Dialer struct {
	cfg  ProxyConfig
	base *net.Dialer
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := d.base.DialContext(ctx, "tcp", d.cfg.Addr())
	if err != nil {
		return nil, err
	}
	if err := socks5Handshake(conn, d.cfg); err != nil {
		_ = conn.Close()
		return nil, err
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := socks5Connect(conn, host, port); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func socks5Handshake(conn net.Conn, cfg ProxyConfig) error {
	methods := []byte{0x05, 0x01, 0x00}
	if cfg.Username != "" {
		methods = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := conn.Write(methods); err != nil {
		return err
	}
	buf := make([]byte, 2)
	if _, err := readFull(conn, buf); err != nil {
		return err
	}
	if buf[0] != 0x05 {
		return fmt.Errorf("network: SOCKS5 版本不符: %d", buf[0])
	}
	if buf[1] == 0x02 {
		if cfg.Username == "" {
			return fmt.Errorf("network: 代理要求用户名密码认证")
		}
		ub := []byte(cfg.Username)
		pb := []byte(cfg.Password)
		auth := append([]byte{0x01, byte(len(ub))}, ub...)
		auth = append(auth, byte(len(pb)))
		auth = append(auth, pb...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		rb := make([]byte, 2)
		if _, err := readFull(conn, rb); err != nil {
			return err
		}
		if rb[1] != 0x00 {
			return fmt.Errorf("network: SOCKS5 认证失败")
		}
	} else if buf[1] != 0x00 {
		return fmt.Errorf("network: SOCKS5 无可用认证方式")
	}
	return nil
}

func socks5Connect(conn net.Conn, host string, port int) error {
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	buf := make([]byte, 4)
	if _, err := readFull(conn, buf); err != nil {
		return err
	}
	if buf[1] != 0x00 {
		return fmt.Errorf("network: SOCKS5 连接失败，rep=%d", buf[1])
	}
	var rest []byte
	switch buf[3] {
	case 0x01:
		rest = make([]byte, 4+2)
	case 0x03:
		l := make([]byte, 1)
		if _, err := readFull(conn, l); err != nil {
			return err
		}
		rest = make([]byte, int(l[0])+2)
	case 0x04:
		rest = make([]byte, 16+2)
	default:
		return fmt.Errorf("network: SOCKS5 未知地址类型 %d", buf[3])
	}
	_, err := readFull(conn, rest)
	return err
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
