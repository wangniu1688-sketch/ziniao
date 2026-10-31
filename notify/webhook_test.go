package notify_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ziniao/notify"
)

// receiver 记录 Webhook 服务端收到的一切。
type receiver struct {
	mu     sync.Mutex
	count  int
	bodies []string
	sigs   []string
	ts     []string
	events []string
	hook   func(w http.ResponseWriter, attempt int)
}

func newReceiver(hook func(http.ResponseWriter, int)) (*receiver, *httptest.Server) {
	r := &receiver{hook: hook}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		r.mu.Lock()
		r.count++
		attempt := r.count
		r.bodies = append(r.bodies, string(body))
		r.sigs = append(r.sigs, req.Header.Get("X-Ziniao-Signature"))
		r.ts = append(r.ts, req.Header.Get("X-Ziniao-Timestamp"))
		r.events = append(r.events, req.Header.Get("X-Ziniao-Event"))
		r.mu.Unlock()
		if r.hook != nil {
			r.hook(w, attempt)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return r, srv
}

func (r *receiver) snapshot() (count int, bodies, sigs, ts, events []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count, append([]string{}, r.bodies...), append([]string{}, r.sigs...),
		append([]string{}, r.ts...), append([]string{}, r.events...)
}

const hookSecret = "s3cr3t-for-testing"

func webhookCfg(url string, allowLoopback bool) notify.WebhookConfig {
	return notify.WebhookConfig{
		URL: url, Secret: hookSecret, Timeout: 2 * time.Second,
		MaxRetry: 2, AllowLoopback: allowLoopback,
	}
}

func mustHook(t *testing.T, url string, allowLoopback bool) *notify.WebhookSender {
	t.Helper()
	s, err := notify.NewWebhookSender(webhookCfg(url, allowLoopback), nil)
	if err != nil {
		t.Fatalf("构造 Webhook 驱动失败: %v", err)
	}
	return s
}

func sampleMessage() notify.Message {
	return notify.Message{
		ID: "n-1", TenantID: "t1", UserID: "u-1", Category: "security",
		Title: "出口 IP 轮换告警", Body: "env-1 出口 1.2.3.4:39101 → 1.2.3.4:39102",
		EventType: "env.exit_ip_rotated", Channels: []string{"inapp", "webhook"},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
}

// TestWebhookSignsPayload 签名必须能被接收方独立验算（HMAC-SHA256 over ts+"."+body）。
//
// 不带时间戳只签 body 时，任何截获过一次告警的人都能无限重放同一条
// 「出口 IP 已轮换」——足以把运营引到完全错误的处置路径上。
func TestWebhookSignsPayload(t *testing.T) {
	rec, srv := newReceiver(nil)
	defer srv.Close()
	s := mustHook(t, srv.URL, true)

	if err := s.Send(context.Background(), sampleMessage()); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	count, bodies, sigs, ts, events := rec.snapshot()
	if count != 1 {
		t.Fatalf("应投递 1 次，实际 %d", count)
	}
	want := "sha256=" + hex.EncodeToString(hmacOf(ts[0], bodies[0]))
	if !hmac.Equal([]byte(sigs[0]), []byte(want)) {
		t.Fatalf("签名不符：got=%s want=%s", sigs[0], want)
	}
	if events[0] != "env.exit_ip_rotated" {
		t.Fatalf("事件头缺失: %q", events[0])
	}
	var p notify.WebhookPayload
	if err := json.Unmarshal([]byte(bodies[0]), &p); err != nil {
		t.Fatalf("载荷非法 JSON: %v", err)
	}
	if p.Title != "出口 IP 轮换告警" || p.EventType != "env.exit_ip_rotated" || p.TenantID != "t1" {
		t.Fatalf("载荷字段不符: %+v", p)
	}
	if strings.Contains(bodies[0], "s3cr3t") {
		t.Fatalf("载荷里不该出现密钥: %s", bodies[0])
	}
	if _, err := strconv.ParseInt(ts[0], 10, 64); err != nil {
		t.Fatalf("时间戳头非法: %q", ts[0])
	}
}

func hmacOf(ts, body string) []byte {
	mac := hmac.New(sha256.New, []byte(hookSecret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

// TestWebhookRetriesTransientFailures 5xx 要重试：对方发布重启不该丢告警。
func TestWebhookRetriesTransientFailures(t *testing.T) {
	rec, srv := newReceiver(func(w http.ResponseWriter, attempt int) {
		if attempt < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()
	s := mustHook(t, srv.URL, true)

	if err := s.Send(context.Background(), sampleMessage()); err != nil {
		t.Fatalf("第三次应成功，实际失败: %v", err)
	}
	if count, _, _, _, _ := rec.snapshot(); count != 3 {
		t.Fatalf("期望 1 次首投 + 2 次重试，实际 %d", count)
	}
}

// TestWebhookNoRetryOn4xx 4xx 不重试：URL 配错或被删的机器人，再试也一样。
func TestWebhookNoRetryOn4xx(t *testing.T) {
	rec, srv := newReceiver(func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusNotFound) })
	defer srv.Close()
	s := mustHook(t, srv.URL, true)

	err := s.Send(context.Background(), sampleMessage())
	if err == nil {
		t.Fatal("404 应导致投递失败")
	}
	if count, _, _, _, _ := rec.snapshot(); count != 1 {
		t.Fatalf("4xx 不应重试，实际请求 %d 次", count)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("错误信息应带状态码便于排查: %v", err)
	}
}

// TestWebhookRetry429 限流要重试但不立刻猛攻（退避存在且可控）。
func TestWebhookRetry429(t *testing.T) {
	rec, srv := newReceiver(func(w http.ResponseWriter, attempt int) {
		if attempt < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()
	cfg := webhookCfg(srv.URL, true)
	cfg.MaxRetry = 1
	s, err := notify.NewWebhookSender(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Send(context.Background(), sampleMessage()); err != nil {
		t.Fatalf("限流后应重试成功: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("重试未退避（仅 %v），会把刚限流的对方打死", elapsed)
	}
	if count, _, _, _, _ := rec.snapshot(); count != 2 {
		t.Fatalf("期望 2 次请求，实际 %d", count)
	}
}

// TestWebhookBlocksLoopbackByDefault Webhook 目标默认不允许内网地址（SSRF）。
func TestWebhookBlocksLoopbackByDefault(t *testing.T) {
	_, srv := newReceiver(nil)
	defer srv.Close()
	if _, err := notify.NewWebhookSender(webhookCfg(srv.URL, false), nil); err != nil {
		t.Fatal(err)
	}
	// allowLoopback=false 也能构造成功（地址本身合法），真正拦在出网这一跳
	s := mustHook(t, srv.URL, false)
	err := s.Send(context.Background(), sampleMessage())
	if err == nil {
		t.Fatal("指向 127.0.0.1 的 Webhook 必须被拦截")
	}
	if !strings.Contains(err.Error(), "内网") {
		t.Fatalf("错误信息应说明拦截原因: %v", err)
	}
}

// TestWebhookRejectsMetadataTarget 云元数据地址同样要拦（169.254.169.254）。
func TestWebhookRejectsMetadataTarget(t *testing.T) {
	if allowed := notify.IsPublicIPString("169.254.169.254"); allowed {
		t.Fatal("链路本地地址不得作为 Webhook 目标")
	}
	if allowed := notify.IsPublicIPString("10.0.0.5"); allowed {
		t.Fatal("私有地址不得作为 Webhook 目标")
	}
	if allowed := notify.IsPublicIPString("8.8.8.8"); !allowed {
		t.Fatal("公网地址应放行")
	}
}

// TestWebhookNoSecretOmitsSignature 没配密钥就不签名，但**照发**：
// 内网可信中转的场景里签名是多余的，别因为缺配置就把告警吞掉。
func TestWebhookNoSecretOmitsSignature(t *testing.T) {
	rec, srv := newReceiver(nil)
	defer srv.Close()
	cfg := webhookCfg(srv.URL, true)
	cfg.Secret = ""
	s, err := notify.NewWebhookSender(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), sampleMessage()); err != nil {
		t.Fatalf("无密钥时应照常投递: %v", err)
	}
	_, _, sigs, _, _ := rec.snapshot()
	if len(sigs) != 1 || sigs[0] != "" {
		t.Fatalf("无密钥时不应带签名头: %v", sigs)
	}
}

// TestWebhookTimeoutBounded 超时必须封顶：目标挂住时不能拖垮整条事件派发。
func TestWebhookTimeoutBounded(t *testing.T) {
	_, srv := newReceiver(func(_ http.ResponseWriter, _ int) { time.Sleep(400 * time.Millisecond) })
	defer srv.Close()
	cfg := webhookCfg(srv.URL, true)
	cfg.Timeout = 80 * time.Millisecond
	cfg.MaxRetry = 0
	s, err := notify.NewWebhookSender(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Send(context.Background(), sampleMessage()); err == nil {
		t.Fatal("目标超时时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("投递未受超时约束：%v", elapsed)
	}
}

// TestWebhookRejectsIllegalURL 非法地址在构造阶段就被拒，不留半配置的通道。
func TestWebhookRejectsIllegalURL(t *testing.T) {
	if _, err := notify.NewWebhookSender(notify.WebhookConfig{URL: "not-a-url"}, nil); err == nil {
		t.Fatal("非法 URL 必须被拒绝")
	}
	if _, err := notify.NewWebhookSender(notify.WebhookConfig{URL: ""}, nil); err == nil {
		t.Fatal("空 URL 必须被拒绝")
	}
}
