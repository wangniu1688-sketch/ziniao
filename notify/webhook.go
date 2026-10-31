package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WebhookPayload Webhook 推送体。
//
// 只带「足以让人做决策」的字段，**不带**任何凭据、IP、设备号一类敏感值：
// Webhook 地址通常是企微/飞书机器人或一个接收中转，群里谁都能看到。
type WebhookPayload struct {
	ID        string   `json:"id"`
	TenantID  string   `json:"tenant_id"`
	UserID    string   `json:"user_id"`
	Category  string   `json:"category"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	EventType string   `json:"event_type"`
	Channels  []string `json:"channels"`
	CreatedAt string   `json:"created_at"`
}

// WebhookConfig Webhook 通道配置。
type WebhookConfig struct {
	URL           string // 留空 = 通道关闭
	Secret        string // HMAC-SHA256 签名密钥；留空则不签名
	Timeout       time.Duration
	MaxRetry      int
	AllowLoopback bool // dev/local 自测用，生产必须为 false
}

// WebhookSender Webhook 驱动（企微/飞书/钉钉机器人与自建中转均可对接）。
type WebhookSender struct {
	cfg WebhookConfig
	cl  *http.Client
	log *slog.Logger
	now func() time.Time // 注入以测试时间戳相关逻辑
}

// NewWebhookSender 构造 Webhook 驱动；URL 非法或未配置时返回错误（fail-closed）。
func NewWebhookSender(cfg WebhookConfig, log *slog.Logger) (*WebhookSender, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("notify: Webhook 地址未配置")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("notify: Webhook 地址非法（需 http/https）")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxRetry < 0 {
		cfg.MaxRetry = 0
	}
	if log == nil {
		log = slog.Default()
	}
	return &WebhookSender{
		cfg: cfg,
		cl:  &http.Client{Timeout: cfg.Timeout, Transport: newSafeTransport(cfg.AllowLoopback)},
		log: log,
		now: time.Now,
	}, nil
}

// Channel 通道名。
func (w *WebhookSender) Channel() string { return "webhook" }

// Send 投递一条通知到 Webhook；网络错误与 5xx/429 重试，其它 4xx 直接失败。
//
// 为什么不交给调用方重试：投递失败现在有落库状态（`failed`）兜底由重投任务继续，
// 但单次调用内的抖动（目标发布重启、连接复用失败）在这里就消化掉更划算——
// 少一轮「写回 failed → 等 5 分钟 → 重投」的延迟，high 告警不需要这种延迟。
func (w *WebhookSender) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(WebhookPayload{
		ID: m.ID, TenantID: m.TenantID, UserID: m.UserID, Category: m.Category,
		Title: m.Title, Body: m.Body, EventType: m.EventType, Channels: m.Channels,
		CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("notify: Webhook 载荷序列化失败: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt <= w.cfg.MaxRetry; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt*attempt)*200*time.Millisecond); err != nil {
				return lastErr
			}
		}
		wErr := w.post(ctx, body, m.EventType)
		if wErr == nil {
			return nil
		}
		lastErr = wErr
		if !retryable(wErr) {
			break
		}
		if attempt < w.cfg.MaxRetry && w.log != nil {
			w.log.Warn("Webhook 投递失败，准备重试", "attempt", attempt+1, "err", wErr)
		}
	}
	return lastErr
}

func (w *WebhookSender) post(ctx context.Context, body []byte, eventType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: Webhook 请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ziniao-notify/1")
	if eventType != "" {
		req.Header.Set("X-Ziniao-Event", eventType)
	}
	ts := strconv.FormatInt(w.now().Unix(), 10)
	req.Header.Set("X-Ziniao-Timestamp", ts)
	if sig := w.sign(body, ts); sig != "" {
		req.Header.Set("X-Ziniao-Signature", sig)
	}
	resp, err := w.cl.Do(req)
	if err != nil {
		return fmt.Errorf("notify: Webhook 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.CopyN(io.Discard, resp.Body, 1<<16) // 释活连接以便复用
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(snippet))}
}

// sign HMAC-SHA256(secret, timestamp + "." + body)，返回 "sha256=<hex>"。
//
// 带上时间戳一起签是为了让接收方能拒绝重放：告警重放一次邮件/IP 轮换这种
// 事件足以让运营跑到错误的处置路径上。
func (w *WebhookSender) sign(body []byte, ts string) string {
	if w.cfg.Secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(w.cfg.Secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// httpError 带状态码的投递错误。
type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("notify: Webhook 返回 HTTP %d", e.status)
	}
	return fmt.Sprintf("notify: Webhook 返回 HTTP %d: %s", e.status, truncate(e.body, 200))
}

func (e *httpError) Status() int { return e.status }

// retryable 判定是否值得在同一次投递内再试一次。
// 4xx（除 429）说明对方不认这个请求——URL 配错、机器人被删、签名被拒，
// 再试多少次都一样；5xx/429/连接错误才会随时间好转。
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false // 进程/请求已放弃，重试是浪费
	}
	var he *httpError
	if errors.As(err, &he) {
		return he.status == 429 || he.status >= 500
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// newSafeTransport 限制 Webhook 只能访问公网地址。
//
// 为什么要在 dial 层而不是只校验 URL 字面量：域名可以先解析到公网 IP 通过校验、
// 真正连接时解析到 127.0.0.1（DNS rebinding），或者 Webhook 地址配错时指向
// 云厂商的元数据服务（169.254.169.254）——这两类 SSRF 都不是看 URL 能挡住的。
var safeDialer = &net.Dialer{Timeout: 5 * time.Second}
var safeResolver = &net.Resolver{}

func newSafeTransport(allowLoopback bool) *http.Transport {
	return &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("notify: 目标地址格式非法: %w", err)
			}
			ips, err := safeResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("notify: Webhook 域名解析失败: %w", err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("notify: Webhook 域名无解析结果: %s", host)
			}
			for _, ip := range ips {
				if !isPublicIP(ip.IP, allowLoopback) {
					return nil, fmt.Errorf("notify: Webhook 目标为内网/保留地址，已拦截: %s", ip.IP)
				}
			}
			return safeDialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
}

// IsPublicIPString 该 IP 是否可以作为 Webhook 目标（严格口径，不放行 loopback）。
//
// 导出给运维自检用：Webhook 配到哪才算安全，必须由这一处说了算，
// 不能出现「文档里说内网不行、代码里某个分支又放行」的两套口径。
func IsPublicIPString(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	return isPublicIP(ip, false)
}

// isPublicIP 是否允许作为 Webhook 目标。
func isPublicIP(ip net.IP, allowLoopback bool) bool {
	if allowLoopback && ip.IsLoopback() {
		return true
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip.IsMulticast(), ip.IsUnspecified():
		return false
	}
	// 云元数据地址（AWS/GCP/Azure 均落在链路本地外的保留段）
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1]&0xC0 == 64 { // CGNAT 100.64/10
			return false
		}
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
