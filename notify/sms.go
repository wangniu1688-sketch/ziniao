package notify

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SMSChannel 通道名（落 notifications.channels / delivered_channels）。
const SMSChannel = "sms"

// 云市场「阿里短信服务」API 网关：GET /singleSendSms，AppKey+AppSecret 签名认证。
// 这里是**云市场**而非 dysmsapi（AccessKey 的 LTAI… 那套）：ck_/cs_ 前缀的
// Consumer Key/Secret 只有 API 网关认，签名算法也完全不同（X-Ca-* 而不是 RPC 的
// Signature=HMAC-SHA1）。选错一套的表现是稳定的 403 签名错误，而不是偶发失败。
const (
	defaultSMSEndpoint    = "https://sms.market.alicloudapi.com"
	smsPath               = "/singleSendSms"
	defaultSMSTemplateKey = "content"
	// 短信不是邮件：变量内容过长会被运营商拆条（按条计费且收件人看到的是乱码顺序），
	// 更常见的结果是网关直接拒收。120 字足够说清"发生了什么"，再多就该点进控制台看。
	smsParamMaxRunes = 120
)

// SMSConfig 短信通道配置（凭据只经环境变量注入，见 platform/config）。
type SMSConfig struct {
	Endpoint      string        // 留空回落云市场默认地址
	AppKey        string        // Consumer Key（ck_…）
	AppSecret     string        // Consumer Secret（cs_…）
	SignName      string        // 短信签名（控制台审核通过的那个，如"佳圆"）
	TemplateCode  string        // 模板 CODE（SMS_…）
	TemplateKey   string        // 模板变量名；留空回落 content
	Timeout       time.Duration // 单次请求超时
	MaxRetry      int           // 同一次投递内的重试次数（默认 0，见 NewSMSSender 说明）
	AllowLoopback bool          // dev/local 自测用，生产必须为 false
}

// SMSSender 短信驱动。
//
// 两条与邮件/Webhook 不同的口径，都不是实现细节而是成本问题：
//  1. 默认不重试：短信的"失败"可能是超时——对方其实已经收下了。再发一次就是
//     同一条告警发两遍，既重复扣费又把人烦到静音。漏发的代价由重投任务
//     （5 分钟后、有次数上限、且只补未送达通道）承担，比重复发便宜得多。
//  2. 号码非法按**终态**处理（包装 ErrNoRecipient）：1 个格式错的手机号不该让
//     一条 high 告警在板上反复失败，最后连真告警一起淹没。
type SMSSender struct {
	cfg SMSConfig
	cl  *http.Client
	log *slog.Logger
	now func() time.Time // 注入以测试时间戳/签名
}

// NewSMSSender 构造短信驱动；半配置一律返回错误（fail-closed）。
//
// 任何一个 SMS 字段被设置就必须全部设置：只填了 AppKey 而没填模板 CODE 的通道
// 看起来"配过"、实际一发就 400，运营会以为告警发出去了——这比不配更糟。
func NewSMSSender(cfg SMSConfig, log *slog.Logger) (*SMSSender, error) {
	cfg.AppKey = strings.TrimSpace(cfg.AppKey)
	cfg.AppSecret = strings.TrimSpace(cfg.AppSecret)
	cfg.SignName = strings.TrimSpace(cfg.SignName)
	cfg.TemplateCode = strings.TrimSpace(cfg.TemplateCode)
	cfg.TemplateKey = strings.TrimSpace(cfg.TemplateKey)
	partial := cfg.AppKey != "" || cfg.AppSecret != "" || cfg.SignName != "" || cfg.TemplateCode != ""
	if partial {
		if cfg.AppKey == "" {
			return nil, errors.New("notify: 短信 AppKey 未配置（ZINIAO_SMS_APP_KEY）")
		}
		if cfg.AppSecret == "" {
			return nil, errors.New("notify: 短信 AppSecret 未配置（ZINIAO_SMS_APP_SECRET）")
		}
		if cfg.SignName == "" {
			return nil, errors.New("notify: 短信签名未配置（ZINIAO_SMS_SIGN_NAME）")
		}
		if cfg.TemplateCode == "" {
			return nil, errors.New("notify: 短信模板 CODE 未配置（ZINIAO_SMS_TEMPLATE_CODE）")
		}
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultSMSEndpoint
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("notify: 短信端点非法（需 http/https）: %s", cfg.Endpoint)
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxRetry < 0 {
		cfg.MaxRetry = 0
	}
	if cfg.TemplateKey == "" {
		cfg.TemplateKey = defaultSMSTemplateKey
	}
	if log == nil {
		log = slog.Default()
	}
	return &SMSSender{
		cfg: cfg,
		cl:  &http.Client{Timeout: cfg.Timeout, Transport: newSafeTransport(cfg.AllowLoopback)},
		log: log,
		now: time.Now,
	}, nil
}

// Channel 通道名。
func (s *SMSSender) Channel() string { return SMSChannel }

// Send 发一条短信。
func (s *SMSSender) Send(ctx context.Context, m Message) error {
	to, err := NormalizeMobile(m.Phone)
	if err != nil {
		return err
	}
	// 四个查询参数固定：RecNum / SignName / TemplateCode / ParamString。
	// 排序由 buildQuery 统一负责——同一份字符串既要送去签名又要真的发出去，
	// 任何一处重排都会让网关算出另一个签名。
	q := buildQuery(to, s.cfg.SignName, s.cfg.TemplateCode, s.param(m))
	var lastErr error
	for attempt := 0; attempt <= s.cfg.MaxRetry; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt*attempt)*200*time.Millisecond); err != nil {
				return lastErr
			}
		}
		req, rErr := s.request(ctx, q)
		if rErr != nil {
			return rErr // 请求构造失败（非法 URL 等）重试没有意义
		}
		lastErr = s.do(req)
		if lastErr == nil {
			return nil
		}
		if !retryable(lastErr) {
			break
		}
		if attempt < s.cfg.MaxRetry && s.log != nil {
			s.log.Warn("短信投递失败，准备重试", "attempt", attempt+1, "err", lastErr)
		}
	}
	return lastErr
}

// Health 校验通道配置与端点可达性。
//
// 不做"真的发一条"：短信没有握手这种免费探测方式，任何真实调用都会计费并打扰
// 收件人。能在这里拦住的是"配了但拼不出来"——端点非法、DNS 不解析。
func (s *SMSSender) Health(_ context.Context) error {
	if s.cfg.AppKey == "" || s.cfg.AppSecret == "" {
		return errors.New("notify: 短信凭据未配置")
	}
	if s.cfg.SignName == "" || s.cfg.TemplateCode == "" {
		return errors.New("notify: 短信签名或模板 CODE 未配置")
	}
	u, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("notify: 短信端点非法: %w", err)
	}
	if _, err := safeResolver.LookupIPAddr(context.Background(), u.Hostname()); err != nil {
		return fmt.Errorf("notify: 短信端点域名解析失败: %w", err)
	}
	return nil
}

// request 构造带 X-Ca-* 签名的 GET 请求。
func (s *SMSSender) request(ctx context.Context, query string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.Endpoint+smsPath+"?"+query, nil)
	if err != nil {
		return nil, fmt.Errorf("notify: 短信请求构造失败: %w", err)
	}
	const accept = "application/json"
	// Date 参与签名：签名用的是"发出去的那个字符串"，不能一个用本地时间、
	// 另一个用 Go 自动补的 Date（网关会算错）。
	date := s.now().UTC().Format(http.TimeFormat)
	ts := strconv.FormatInt(s.now().UnixMilli(), 10)
	nonce := randomNonce()
	req.Header.Set("Accept", accept)
	req.Header.Set("Date", date)
	req.Header.Set("User-Agent", "ziniao-notify/1")
	req.Header.Set("X-Ca-Key", s.cfg.AppKey)
	req.Header.Set("X-Ca-Timestamp", ts)
	req.Header.Set("X-Ca-Nonce", nonce)
	// 参与签名的头必须逐一列出：网关按这个列表重算，多签一个/少签一个都是 403。
	req.Header.Set("X-Ca-Signature-Headers", "x-ca-key,x-ca-nonce,x-ca-timestamp")
	req.Header.Set("X-Ca-Signature", s.sign(accept, date, map[string]string{
		"x-ca-key":       s.cfg.AppKey,
		"x-ca-nonce":     nonce,
		"x-ca-timestamp": ts,
	}, smsPath+"?"+query))
	return req, nil
}

// sign 计算 X-Ca-Signature = Base64(HMAC-SHA256(AppSecret, StringToSign))。
//
// StringToSign 逐行拼接（LF 结尾，含最后一行 Header），顺序固定：
// method / Accept / Content-MD5 / Content-Type / Date / 规范化头 / 规范化 URL。
// GET 无正文 ⇒ Content-MD5 与 Content-Type 是**空字符串而不是省略这一行**，
// 少一行就等于给网关送了一个它算不出来的签名。
//
// URL 只含 path + 排好序的 query（不含 scheme/host）：把 host 也算进去是最常见的
// 签名不一致来源。
func (s *SMSSender) sign(accept, date string, headers map[string]string, canonicalURL string) string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var hb strings.Builder
	for _, k := range keys {
		hb.WriteString(k)
		hb.WriteByte(':')
		hb.WriteString(headers[k])
		hb.WriteByte('\n')
	}
	var sb strings.Builder
	sb.WriteString(http.MethodGet)
	sb.WriteByte('\n')
	sb.WriteString(accept)
	sb.WriteByte('\n')
	sb.WriteByte('\n') // Content-MD5（无正文）
	sb.WriteByte('\n') // Content-Type（无正文）
	sb.WriteString(date)
	sb.WriteByte('\n')
	sb.WriteString(hb.String())
	sb.WriteByte('\n')
	sb.WriteString(canonicalURL)
	mac := hmac.New(sha256.New, []byte(s.cfg.AppSecret))
	mac.Write([]byte(sb.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// do 发请求并判定结果。
func (s *SMSSender) do(req *http.Request) error {
	resp, err := s.cl.Do(req)
	if err != nil {
		return fmt.Errorf("notify: 短信请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	body := strings.TrimSpace(string(snippet))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode, body: body}
	}
	// 网关把"模板不存在""余额不足""签名未审核"也放在 200 里返回：
	// 只看状态码会把每一条短信都记成送达，控制台上一片"已外发"而用户一条没收到。
	var out smsResult
	if json.Unmarshal([]byte(body), &out) == nil && !out.ok() {
		return &smsBizError{msg: truncate(out.errorMsg(), 200), throttle: out.throttled()}
	}
	return nil
}

// param 模板变量 JSON：{"<TemplateKey>":"标题，正文"}。
func (s *SMSSender) param(m Message) string {
	key := s.cfg.TemplateKey
	text := strings.TrimSpace(m.Title)
	if body := strings.TrimSpace(m.Body); body != "" {
		if text != "" {
			text += "，"
		}
		text += body
	}
	if text == "" {
		text = "系统通知"
	}
	buf, err := json.Marshal(map[string]string{key: truncateRunes(text, smsParamMaxRunes)})
	if err != nil {
		return fmt.Sprintf(`{"%s":"系统通知"}`, key)
	}
	return string(buf)
}

// buildQuery 生成规范化查询串（key 字典序，与签名用的同一份）。
//
// 用 QueryEscape 而不是 url.Values.Encode：Encode 对排序和空格的处理与网关
// 期望的百分号编码不完全一致，两边任何一个字节不同都是 403。
func buildQuery(recNum, signName, templateCode, paramString string) string {
	pairs := [][2]string{
		{"ParamString", paramString},
		{"RecNum", recNum},
		{"SignName", signName},
		{"TemplateCode", templateCode},
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p[0]))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[1]))
	}
	return b.String()
}

// randomNonce 生成防重放随机数（网关会拒重复的 nonce）。
func randomNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}

// smsResult 网关响应（不同服务商字段不一，只认"能判定成败"的那几个）。
type smsResult struct {
	Success *bool   `json:"success"`
	Message string  `json:"message"`
	Error   string  `json:"error"`
	Code    string  `json:"code"`
	Result  *string `json:"result"`
}

// ok 是否算送达：success 显式为 false 或有 error 字段即失败。
func (r smsResult) ok() bool {
	if r.Success != nil && !*r.Success {
		return false
	}
	return strings.TrimSpace(r.Error) == ""
}

func (r smsResult) errorMsg() string {
	switch {
	case strings.TrimSpace(r.Error) != "":
		return r.Error
	case strings.TrimSpace(r.Message) != "":
		return r.Message
	case r.Code != "":
		return "错误码 " + r.Code
	default:
		return "网关返回失败"
	}
}

// throttled 是否被限流：只有限流值得在同一次投递里再试一次，
// 模板/签名/余额这类错误再试一百次也一样（且会一直扣到余额耗尽）。
func (r smsResult) throttled() bool {
	s := strings.ToLower(r.errorMsg())
	for _, kw := range []string{"limit", "throttle", "flow", "frequent", "限流", "频繁"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// smsBizError 网关 200 但业务失败（模板/签名/余额/限流）。
type smsBizError struct {
	msg      string
	throttle bool
}

func (e *smsBizError) Error() string {
	if e.msg == "" {
		return "notify: 短信网关返回失败"
	}
	return "notify: 短信网关返回失败: " + e.msg
}

// NormalizeMobile 规范化中国大陆手机号；非法返回包装后的 ErrNoRecipient。
//
// 为什么要判格式而不是原样下发：网关对非法号码返回的是一条失败响应（计费/限流
// 都算在这一次上），而"号码填错"是**不会随时间好转**的终态——留它在板上反复重投，
// 只会把真正需要人看的失败挤下去。
func NormalizeMobile(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == '-' || r == '(' || r == ')' || r == ' ' || r == '\t':
			// 常见分隔符，忽略
		default:
			return "", fmt.Errorf("%w: 手机号含非法字符（%s）", ErrNoRecipient, maskMobile(trimmed))
		}
	}
	s := b.String()
	// 显式国际前缀（+ 或 00）只认 +86 / 0086，其它国家码一律拒发：
	// "+1-380-013-8000" 去掉符号正好是 11 位"看起来合法"的国内号，
	// 放行就等于把短信发到一个完全不相干的人手上。
	if strings.HasPrefix(trimmed, "+") || strings.HasPrefix(s, "00") {
		s = strings.TrimPrefix(s, "00")
		if !strings.HasPrefix(s, "86") {
			return "", fmt.Errorf("%w: 仅支持中国大陆手机号（%s）", ErrNoRecipient, maskMobile(trimmed))
		}
		s = strings.TrimPrefix(s, "86")
	} else if strings.HasPrefix(s, "86") && len(s) > 11 {
		s = s[2:] // 无前缀的 86 开头：国内写法
	}
	if len(s) != 11 || s[0] != '1' || s[1] < '3' || s[1] > '9' {
		if s == "" {
			return "", fmt.Errorf("%w: 未填写手机号", ErrNoRecipient)
		}
		return "", fmt.Errorf("%w: 手机号格式非法（%s）", ErrNoRecipient, maskMobile(s))
	}
	return s, nil
}

// maskMobile 手机号脱敏（日志/错误里只留前 3 后 4）。
func maskMobile(s string) string {
	d := []rune(s)
	if len(d) <= 7 {
		return strings.Repeat("*", len(d))
	}
	return string(d[:3]) + "****" + string(d[len(d)-4:])
}

// truncateRunes 按**字符**截断（中英文混排时不能按字节切，会切出半个汉字）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
