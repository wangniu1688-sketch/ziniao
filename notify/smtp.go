package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// ErrNoRecipient 收件人为空（用户没填邮箱 / 手机号）。
//
// 这是**可预期的降级条件**而不是故障：Sink 据此跳过该通道并让通知保持 sent，
// 否则「团队里有一个人没填邮箱」会让所有 high 告警永远停在 failed、
// 白占着重投位，最后连真正需要人看的告警也一起淹没。
var ErrNoRecipient = errors.New("notify: 缺少收件人")

// SMTPConfig 邮件通道配置（凭据只经环境变量注入，见 platform/config）。
type SMTPConfig struct {
	Host    string // 留空 = 通道关闭
	Port    int
	User    string
	Pass    string
	From    string // 留空回落到 User
	TLSMode string // starttls|tls|none
	Timeout time.Duration
}

// SMTPSender 邮件驱动。
//
// TLS 口径：STARTTLS 协商失败**直接报错**，绝不降级为明文投递——
// 告警正文里带着租户名、站点域名、操作结果，明文过公网等于替攻击者整理材料；
// 且 Go 标准库的 PlainAuth 本身也拒绝在未加密连接上发口令。
type SMTPSender struct {
	cfg SMTPConfig
	log *slog.Logger
}

// NewSMTPSender 构造邮件驱动并补齐兜底值。
func NewSMTPSender(cfg SMTPConfig, log *slog.Logger) *SMTPSender {
	if cfg.Port <= 0 {
		cfg.Port = 587
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.From == "" {
		cfg.From = cfg.User
	}
	cfg.TLSMode = strings.ToLower(strings.TrimSpace(cfg.TLSMode))
	if cfg.TLSMode == "" {
		cfg.TLSMode = "starttls"
	}
	if log == nil {
		log = slog.Default()
	}
	return &SMTPSender{cfg: cfg, log: log}
}

// Channel 通道名。
func (s *SMTPSender) Channel() string { return "email" }

// Health 真的连一次 SMTP 并走完 EHLO。
//
// 不能因为「配置里有 Host」就返回 OK：上一次交付里 TLSProvider.Health 曾因为
// Path 没配时不打网络而永远通过，这里同一个坑不踩第二次。
func (s *SMTPSender) Health(ctx context.Context) error {
	if s.cfg.Host == "" {
		return errors.New("notify: SMTP 主机未配置")
	}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	c, err := s.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Quit() }()
	if err := c.Hello(heloName); err != nil {
		return fmt.Errorf("notify: SMTP EHLO 失败: %w", err)
	}
	return nil
}

// Send 投递一封纯文本告警邮件。
func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	to := strings.TrimSpace(m.To)
	if to == "" {
		return ErrNoRecipient
	}
	if s.cfg.Host == "" {
		return errors.New("notify: SMTP 主机未配置")
	}
	if s.cfg.From == "" {
		return errors.New("notify: SMTP 发件人为空（配置 SMTPFrom 或 SMTPUser）")
	}
	raw, err := BuildMail(s.cfg.From, to, m.Title, m.Body, time.Now().UTC())
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	c, err := s.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Quit() }()
	if err := c.Hello(heloName); err != nil {
		return fmt.Errorf("notify: SMTP EHLO 失败: %w", err)
	}
	if s.cfg.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.User, s.cfg.Pass, s.cfg.Host)); err != nil {
			// L4：错误里只有用户名，不含口令
			return fmt.Errorf("notify: SMTP 认证失败（user=%s）: %w", s.cfg.User, err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("notify: SMTP MAIL FROM 被拒: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("notify: SMTP RCPT TO 被拒: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("notify: SMTP DATA 被拒: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return fmt.Errorf("notify: 邮件正文写入失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notify: 邮件正文提交失败: %w", err)
	}
	return nil
}

// heloName EHLO 报出的客户端名（与标准库 SendMail 一致）。
const heloName = "localhost"

// dial 按 TLSMode 建立连接并完成必要的加密协商。none 仅供本地自签/内网中继。
//
// 连接 deadline 在这里就设置好：投递超时不能只靠 Dialer.Timeout——那只管建连，
// 而真正会拖住事件派发的是「连上了、对方慢慢吞吞不回 250」的场景。
func (s *SMTPSender) dial(ctx context.Context, addr string) (*smtp.Client, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = s.cfg.Host
	}
	d := &net.Dialer{Timeout: s.cfg.Timeout}
	deadline := func(conn net.Conn) net.Conn {
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		} else {
			_ = conn.SetDeadline(time.Now().Add(s.cfg.Timeout))
		}
		return conn
	}
	newClient := func(conn net.Conn) (*smtp.Client, error) {
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("notify: SMTP 会话建立失败: %w", err)
		}
		return c, nil
	}
	switch s.cfg.TLSMode {
	case "tls":
		conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return nil, fmt.Errorf("notify: SMTP 隐式 TLS 连接失败: %w", err)
		}
		return newClient(deadline(conn))
	case "none":
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("notify: SMTP 连接失败: %w", err)
		}
		if s.cfg.User != "" {
			_ = conn.Close()
			return nil, errors.New("notify: 未加密连接上拒绝发送 SMTP 口令（改用 starttls/tls）")
		}
		if s.log != nil {
			s.log.Warn("SMTP 以明文模式发送通知，仅限内网可信中继", "host", addr)
		}
		return newClient(deadline(conn))
	default: // starttls
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("notify: SMTP 连接失败: %w", err)
		}
		c, err := newClient(deadline(conn))
		if err != nil {
			return nil, err
		}
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("notify: STARTTLS 协商失败（不降级为明文）: %w", err)
		}
		return c, nil
	}
}

// BuildMail 组装一封 UTF-8 纯文本邮件（RFC 5322 + MIME）。
//
// 主题与正文都可能含中文：主题必须做 RFC 2047 编码，正文统一 base64，
// 否则遇到古老的 7bit-only 中继会直接把告警内容打成乱码 —— 乱码的安全告警
// 等于没发。
func BuildMail(from, to, subject, body string, now time.Time) ([]byte, error) {
	if strings.ContainsAny(from, "\r\n") || strings.ContainsAny(to, "\r\n") {
		return nil, errors.New("notify: 收件人/发件人不得含换行（防 SMTP 头注入）")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", encodeRFC2047(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return []byte(b.String()), nil
}

// encodeRFC2047 主题编码（=?UTF-8?B?...?=），按 ≤76 字符折行。
func encodeRFC2047(s string) string {
	const prefix = "=?UTF-8?B?"
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	maxLen := 66 // 76 - len("=?UTF-8?B??=") - 折叠空格
	if len(enc) <= maxLen {
		return prefix + enc + "?="
	}
	parts := []string{}
	for len(enc) > maxLen {
		parts = append(parts, prefix+enc[:maxLen]+"?=")
		enc = enc[maxLen:]
	}
	if len(enc) > 0 {
		parts = append(parts, prefix+enc+"?=")
	}
	return strings.Join(parts, "\r\n ")
}
