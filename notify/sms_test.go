package notify_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"ziniao/notify"
	"ziniao/platform/config"
)

// ---------- 假短信网关 ----------
//
// 自起 httptest 而不是打桩 Send：这条通道唯一真正会错的地方就是**签名字节**
// 和**查询串顺序**——打桩后「反正 Send 被调用了」永远为真，上了真网关才是 403。

type fakeGateway struct {
	srv       *httptest.Server
	mu        sync.Mutex
	calls     int
	reqs      []fakeGatewayReq
	status    int
	body      string
	hitChan   chan struct{}
	failFirst int // 前 N 次返回失败（模拟抖动后恢复）
}

type fakeGatewayReq struct {
	URL     string
	Query   map[string]string
	Headers map[string]string
	Body    string
}

func newFakeGateway(status int, body string) *fakeGateway {
	g := &fakeGateway{status: status, body: body, hitChan: make(chan struct{}, 16)}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.calls++
		idx := g.calls
		req := fakeGatewayReq{
			URL:     r.URL.String(),
			Query:   map[string]string{},
			Headers: map[string]string{},
		}
		for k, v := range r.URL.Query() {
			req.Query[k] = v[0]
		}
		for k, v := range r.Header {
			req.Headers[strings.ToLower(k)] = v[0]
		}
		g.reqs = append(g.reqs, req)
		st, bd := g.status, g.body
		if idx <= g.failFirst {
			st, bd = 503, `{"error":"服务暂时不可用"}`
		}
		g.mu.Unlock()
		select {
		case g.hitChan <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = w.Write([]byte(bd))
	}))
	return g
}

func (g *fakeGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *fakeGateway) last() fakeGatewayReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.reqs) == 0 {
		return fakeGatewayReq{}
	}
	return g.reqs[len(g.reqs)-1]
}

func (g *fakeGateway) all() []fakeGatewayReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]fakeGatewayReq, len(g.reqs))
	copy(out, g.reqs)
	return out
}

const (
	testAppKey    = "ck_4cec17c1549147f3f7e1456f0f729c59dc1d1521"
	testAppSecret = "cs_test_secret_for_unit_tests"
	testSign      = "佳圆"
	testTpl       = "SMS_492460356"
)

func smsTo(g *fakeGateway) notify.SMSConfig {
	return notify.SMSConfig{
		Endpoint:      g.srv.URL,
		AppKey:        testAppKey,
		AppSecret:     testAppSecret,
		SignName:      testSign,
		TemplateCode:  testTpl,
		AllowLoopback: true,
		Timeout:       2 * time.Second,
	}
}

// expectedSignature 用**独立实现**重算一遍签名。
//
// 关键点：不复用 notify 包内部的 sign，而是照网关文档从请求原文（方法/Accept/Date/
// 三个 X-Ca 头/path+query）重新拼一遍。两边对得上才说明实现符合网关口径；
// 若直接断言"签名等于 sign() 的返回值"，实现写错时测试会跟着一起错。
func expectedSignature(secret, date, url string, headers map[string]string) string {
	keys := []string{"x-ca-key", "x-ca-nonce", "x-ca-timestamp"}
	sort.Strings(keys)
	var hb strings.Builder
	for _, k := range keys {
		hb.WriteString(k + ":" + headers[k] + "\n")
	}
	s := strings.Join([]string{
		"GET",
		"application/json",
		"", // Content-MD5（无正文）
		"", // Content-Type（无正文）
		date,
		hb.String(),
		url,
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(s))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// TestSMSSignatureMatchesIndependentRecompute 签名必须能被网关那套算法复算出来。
func TestSMSSignatureMatchesIndependentRecompute(t *testing.T) {
	g := newFakeGateway(200, `{"success":true,"message":"Ok"}`)
	defer g.srv.Close()
	s, err := notify.NewSMSSender(smsTo(g), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), notify.Message{
		Title: "出口 IP 已轮换", Body: "店铺 A 的出口 IP 发生变化", Phone: "138 0013 8000",
	}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	req := g.last()
	if got := req.Headers["x-ca-key"]; got != testAppKey {
		t.Fatalf("X-Ca-Key 应为 AppKey，实际 %q", got)
	}
	if req.Headers["x-ca-timestamp"] == "" || req.Headers["x-ca-nonce"] == "" {
		t.Fatal("缺少 X-Ca-Timestamp / X-Ca-Nonce（防重放要素）")
	}
	if got := req.Headers["x-ca-signature-headers"]; got != "x-ca-key,x-ca-nonce,x-ca-timestamp" {
		t.Fatalf("参与签名的头列表不符: %q", got)
	}
	canonicalURL := strings.TrimPrefix(req.URL, "")
	want := expectedSignature(testAppSecret, req.Headers["date"], canonicalURL, req.Headers)
	if req.Headers["x-ca-signature"] != want {
		t.Fatalf("签名不符\n实现: %s\n重算: %s\nURL: %s", req.Headers["x-ca-signature"], want, canonicalURL)
	}
}

// TestSMSQueryParams 查询参数与模板变量必须符合网关约定。
func TestSMSQueryParams(t *testing.T) {
	g := newFakeGateway(200, `{"success":true}`)
	defer g.srv.Close()
	s, _ := notify.NewSMSSender(smsTo(g), nil)
	if err := s.Send(context.Background(), notify.Message{
		Title: "出口 IP 已轮换", Body: "店铺 A：1.2.3.4 → 5.6.7.8", Phone: "+86 138-0013-8000",
	}); err != nil {
		t.Fatal(err)
	}
	q := g.last().Query
	if q["RecNum"] != "13800138000" {
		t.Fatalf("RecNum 应规范化为 11 位国内号，实际 %q", q["RecNum"])
	}
	if q["SignName"] != testSign || q["TemplateCode"] != testTpl {
		t.Fatalf("签名/模板不符: %q / %q", q["SignName"], q["TemplateCode"])
	}
	var param map[string]string
	if err := json.Unmarshal([]byte(q["ParamString"]), &param); err != nil {
		t.Fatalf("ParamString 不是合法 JSON: %v", err)
	}
	if !strings.Contains(param["content"], "出口 IP 已轮换") {
		t.Fatalf("模板变量内容缺标题: %q", param)
	}
	// 查询串必须字典序：签名与请求是同一份字符串，顺序不同即 403
	if !strings.HasPrefix(g.last().URL, "/singleSendSms?ParamString=") {
		t.Fatalf("查询串未按字典序（ParamString 应在最前）: %s", g.last().URL)
	}
}

// TestSMSRetrySemantics 5xx 重试、4xx 不重试、业务失败不重试——与 Webhook 同口径。
func TestSMSRetrySemantics(t *testing.T) {
	// 4xx：请求只发一次
	g4 := newFakeGateway(400, `{"error":"模板不存在"}`)
	defer g4.srv.Close()
	s4, _ := notify.NewSMSSender(notify.SMSConfig{
		Endpoint: g4.srv.URL, AppKey: testAppKey, AppSecret: testAppSecret,
		SignName: testSign, TemplateCode: testTpl, AllowLoopback: true, MaxRetry: 2,
	}, nil)
	if err := s4.Send(context.Background(), notify.Message{Title: "t", Phone: "13800138000"}); err == nil {
		t.Fatal("4xx 应失败")
	}
	if g4.count() != 1 {
		t.Fatalf("4xx 不应重试，实际请求 %d 次", g4.count())
	}

	// 5xx：按 MaxRetry 重试到成功
	g5 := newFakeGateway(200, `{"success":true}`)
	g5.failFirst = 2
	defer g5.srv.Close()
	s5, _ := notify.NewSMSSender(notify.SMSConfig{
		Endpoint: g5.srv.URL, AppKey: testAppKey, AppSecret: testAppSecret,
		SignName: testSign, TemplateCode: testTpl, AllowLoopback: true, MaxRetry: 2,
	}, nil)
	if err := s5.Send(context.Background(), notify.Message{Title: "t", Phone: "13800138000"}); err != nil {
		t.Fatalf("第 3 次应成功: %v", err)
	}
	if g5.count() != 3 {
		t.Fatalf("5xx 应重试 2 次（共 3 次请求），实际 %d", g5.count())
	}
	// 每次 nonce 必须不同：网关会拒重放
	seen := map[string]bool{}
	for _, r := range g5.all() {
		if seen[r.Headers["x-ca-nonce"]] {
			t.Fatal("nonce 重复，网关会判重放")
		}
		seen[r.Headers["x-ca-nonce"]] = true
	}
}

// TestSMSDefaultDoesNotRetry 默认不重试：短信重复发送比漏发更贵。
func TestSMSDefaultDoesNotRetry(t *testing.T) {
	g := newFakeGateway(200, `{"success":true}`)
	g.failFirst = 99
	defer g.srv.Close()
	cfg := smsTo(g) // MaxRetry 未显式设置
	s, _ := notify.NewSMSSender(cfg, nil)
	if err := s.Send(context.Background(), notify.Message{Title: "t", Phone: "13800138000"}); err == nil {
		t.Fatal("持续 503 时应失败")
	}
	if g.count() != 1 {
		t.Fatalf("短信默认不应重试（宁可漏发也不重复发），实际请求 %d 次", g.count())
	}
}

// TestSMSBusinessFailureIsNotSuccess 网关 200 + success:false 必须算失败。
//
// 只看状态码的话，"模板不存在/余额不足"全都会被记成已送达——控制台一片"已外发"，
// 而用户一条都没收到。这是短信通道最容易骗人的一处。
func TestSMSBusinessFailureIsNotSuccess(t *testing.T) {
	g := newFakeGateway(200, `{"success":false,"message":"模板不存在"}`)
	defer g.srv.Close()
	s, _ := notify.NewSMSSender(smsTo(g), nil)
	err := s.Send(context.Background(), notify.Message{Title: "t", Phone: "13800138000"})
	if err == nil {
		t.Fatal("success:false 应视为投递失败")
	}
	if !strings.Contains(err.Error(), "模板不存在") {
		t.Fatalf("错误信息应带上网关原因，实际: %v", err)
	}
	if g.count() != 1 {
		t.Fatalf("模板/签名类业务错误重试无意义，实际请求 %d 次", g.count())
	}
}

// TestSMSThrottledRetries 只有限流值得再试一次（会随时间好转）。
func TestSMSThrottledRetries(t *testing.T) {
	g := newFakeGateway(200, `{"success":false,"message":"触发限流"}`)
	defer g.srv.Close()
	s, _ := notify.NewSMSSender(notify.SMSConfig{
		Endpoint: g.srv.URL, AppKey: testAppKey, AppSecret: testAppSecret,
		SignName: testSign, TemplateCode: testTpl, AllowLoopback: true, MaxRetry: 1,
	}, nil)
	if err := s.Send(context.Background(), notify.Message{Title: "t", Phone: "13800138000"}); err == nil {
		t.Fatal("限流最终仍失败时应返回错误")
	}
	if g.count() != 2 {
		t.Fatalf("限流应重试 1 次（共 2 次请求），实际 %d", g.count())
	}
}

// TestSMSRecipientErrors 没手机号 / 号码格式错都是终态，不是故障。
func TestSMSRecipientErrors(t *testing.T) {
	g := newFakeGateway(200, `{"success":true}`)
	defer g.srv.Close()
	s, _ := notify.NewSMSSender(smsTo(g), nil)
	for _, phone := range []string{"", "  ", "123", "1380013800a", "8613800138000000"} {
		err := s.Send(context.Background(), notify.Message{Title: "t", Phone: phone})
		if !errors.Is(err, notify.ErrNoRecipient) {
			t.Fatalf("号码 %q 应返回 ErrNoRecipient，实际: %v", phone, err)
		}
	}
	if g.count() != 0 {
		t.Fatalf("收件人无效时不应发出请求，实际 %d 次", g.count())
	}
	// 号码不进错误原文（日志同样会打这条信息）
	err := s.Send(context.Background(), notify.Message{Title: "t", Phone: "123"})
	if strings.Contains(err.Error(), "123") {
		t.Fatalf("错误信息不应回显完整号码: %v", err)
	}
}

// TestSMSRejectsPartialConfig 半配置必须在启动装配阶段就炸。
func TestSMSRejectsPartialConfig(t *testing.T) {
	cases := map[string]notify.SMSConfig{
		"缺 AppSecret":    {AppKey: "ck_1", SignName: "s", TemplateCode: "SMS_1"},
		"缺签名":            {AppKey: "ck_1", AppSecret: "cs_1", TemplateCode: "SMS_1"},
		"缺模板 CODE":       {AppKey: "ck_1", AppSecret: "cs_1", SignName: "s"},
		"端点非 http/https": {AppKey: "ck_1", AppSecret: "cs_1", SignName: "s", TemplateCode: "SMS_1", Endpoint: "ftp://x"},
	}
	for name, cfg := range cases {
		if _, err := notify.NewSMSSender(cfg, nil); err == nil {
			t.Fatalf("%s 应报错", name)
		}
	}
}

// TestSMSSecretNeverLeaks 密钥不得出现在错误、请求或日志里。
func TestSMSSecretNeverLeaks(t *testing.T) {
	g := newFakeGateway(500, "boom")
	defer g.srv.Close()
	s, _ := notify.NewSMSSender(smsTo(g), nil)
	err := s.Send(context.Background(), notify.Message{Title: "t", Phone: "13800138000"})
	if err == nil {
		t.Fatal("500 应失败")
	}
	if strings.Contains(err.Error(), testAppSecret) {
		t.Fatalf("错误信息泄露了 AppSecret: %v", err)
	}
	for _, r := range g.all() {
		if strings.Contains(r.URL, testAppSecret) {
			t.Fatal("查询串泄露了 AppSecret")
		}
		for k, v := range r.Headers {
			if strings.Contains(v, testAppSecret) {
				t.Fatalf("请求头 %s 泄露了 AppSecret", k)
			}
		}
	}
}

// TestSMSEndpointHostNotSigned 签名只覆盖 path+query，不含 host。
//
// 把 host 也算进 StringToSign 是最常见的一类"本地怎么调都对、上了网关就是 403"。
func TestSMSEndpointHostNotSigned(t *testing.T) {
	g := newFakeGateway(200, `{"success":true}`)
	defer g.srv.Close()
	s, _ := notify.NewSMSSender(smsTo(g), nil)
	_ = s.Send(context.Background(), notify.Message{Title: "t", Phone: "13800138000"})
	req := g.last()
	if !strings.HasPrefix(req.URL, "/singleSendSms?") {
		t.Fatalf("URL 应为 path+query，实际 %q", req.URL)
	}
	if strings.Contains(req.URL, "127.0.0.1") || strings.Contains(req.URL, "http") {
		t.Fatalf("URL 不应包含 host: %q", req.URL)
	}
}

// TestSMSHealthChecksConfigNotSends 自检不能靠"真发一条"来验证。
func TestSMSHealthChecksConfigNotSends(t *testing.T) {
	g := newFakeGateway(200, `{"success":true}`)
	defer g.srv.Close()
	s, _ := notify.NewSMSSender(smsTo(g), nil)
	if err := s.Health(context.Background()); err != nil {
		t.Fatalf("配置完整时 Health 应通过: %v", err)
	}
	if g.count() != 0 {
		t.Fatal("Health 不得真的发短信（会计费并打扰收件人）")
	}
	bad, _ := notify.NewSMSSender(notify.SMSConfig{}, nil)
	if err := bad.Health(context.Background()); err == nil {
		t.Fatal("未配置凭据时 Health 应报错")
	}
}

// TestSMSParamTruncated 超长内容必须截断：拆条会乱序且重复扣费。
func TestSMSParamTruncated(t *testing.T) {
	g := newFakeGateway(200, `{"success":true}`)
	defer g.srv.Close()
	s, _ := notify.NewSMSSender(smsTo(g), nil)
	_ = s.Send(context.Background(), notify.Message{
		Title: "告警", Body: strings.Repeat("这是一条很长的告警内容", 50), Phone: "13800138000",
	})
	var param map[string]string
	if err := json.Unmarshal([]byte(g.last().Query["ParamString"]), &param); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(param["content"])); n > 121 {
		t.Fatalf("模板变量未截断: %d 字", n)
	}
}

// TestSMSBuildSenders 端到端装配：环境变量 → BuildSenders → 通道清单。
func TestSMSBuildSenders(t *testing.T) {
	cfg := config.Default()
	if _, err := notify.BuildSenders(cfg, nil); err != nil {
		t.Fatal(err)
	}
	cfg.Notify.SMSAppKey = testAppKey
	cfg.Notify.SMSAppSecret = testAppSecret
	cfg.Notify.SMSSignName = testSign
	cfg.Notify.SMSTemplateCode = testTpl
	senders, err := notify.BuildSenders(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	has := false
	for _, sd := range senders {
		if sd.Channel() == "sms" {
			has = true
		}
	}
	if !has {
		t.Fatal("配置完整时应装配短信通道")
	}
	if ch := cfg.NotifyChannels(); len(ch) != 1 || ch[0] != "sms" {
		t.Fatalf("NotifyChannels 应为 [sms]，实际 %v", ch)
	}
	// 半配置：装配阶段必须报错，而不是静默少一个通道
	half := config.Default()
	half.Notify.SMSAppKey = testAppKey
	if _, err := notify.BuildSenders(half, nil); err == nil {
		t.Fatal("半配置应在装配阶段报错")
	}
}

// TestNormalizeMobile 号码规范化。
func TestNormalizeMobile(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"13800138000", "13800138000", true},
		{"138 0013 8000", "13800138000", true},
		{"+86-138-0013-8000", "13800138000", true},
		{"008613800138000", "13800138000", true},
		{"19912345678", "19912345678", true},
		{"", "", false},
		{"12345678901", "", false}, // 第二位非 3-9
		{"1234567", "", false},
		{"1380013800a", "", false},
		{"+1-380-013-8000", "", false},
	}
	for _, c := range cases {
		got, err := notify.NormalizeMobile(c.in)
		if c.ok {
			if err != nil || got != c.want {
				t.Fatalf("%q 期望 %s，实际 %s/%v", c.in, c.want, got, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%q 应判非法", c.in)
		}
		if !errors.Is(err, notify.ErrNoRecipient) {
			t.Fatalf("%q 应包装 ErrNoRecipient（终态而非故障），实际 %v", c.in, err)
		}
	}
}
