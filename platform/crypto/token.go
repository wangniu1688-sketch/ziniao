package crypto

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"ziniao/platform/ids"
)

// ---------- JWT (HS256) ----------
// 仅实现平台需要的子集：签发、校验、过期。TODO(P1)：接入 RS256/密钥轮换（KMS 托管）。

// SignJWT 签发 HS256 JWT。claims 中需自带 exp 之外的业务字段。
func SignJWT(secret string, claims map[string]any, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("crypto: JWT 密钥为空")
	}
	c := make(map[string]any, len(claims)+2)
	for k, v := range claims {
		c[k] = v
	}
	now := time.Now()
	c["iat"] = now.Unix()
	c["exp"] = now.Add(ttl).Unix()
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signingInput := b64url(header) + "." + b64url(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64url(mac.Sum(nil)), nil
}

// ParseJWT 严格校验 HS256 签名、JWT 头与必需的过期时间。
// 不接受缺少 exp、非 HS256 算法或格式错误的签名，避免宽松解析造成算法混淆。
func ParseJWT(secret, token string) (map[string]any, error) {
	if secret == "" {
		return nil, fmt.Errorf("crypto: JWT 密钥为空")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("crypto: JWT 格式非法")
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("crypto: JWT header 非法")
	}
	var header map[string]any
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, fmt.Errorf("crypto: JWT header 非法")
	}
	if alg, ok := header["alg"].(string); !ok || alg != "HS256" {
		return nil, fmt.Errorf("crypto: JWT 算法不受支持")
	}
	if typ, ok := header["typ"]; ok {
		if value, valid := typ.(string); !valid || value != "JWT" {
			return nil, fmt.Errorf("crypto: JWT 类型非法")
		}
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("crypto: JWT 签名格式非法")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), signature) {
		return nil, fmt.Errorf("crypto: JWT 签名校验失败")
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("crypto: JWT payload 非法")
	}
	claims := map[string]any{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("crypto: JWT payload 非法")
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return nil, fmt.Errorf("crypto: JWT 缺少有效的 exp 声明")
	}
	if time.Now().Unix() >= int64(exp) {
		return nil, fmt.Errorf("crypto: JWT 已过期")
	}
	return claims, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ---------- TOTP (RFC 6238) ----------

const totpPeriod = 30 * time.Second

// NewTOTPSecret 生成 160 bit 种子（base32）。
func NewTOTPSecret() string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(ids.RandomBytes(20))
}

// TOTPURI 生成 otpauth:// 供扫码绑定。
func TOTPURI(issuer, account, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s:%s?issuer=%s&secret=%s&digits=6&period=30",
		url.QueryEscape(issuer), url.QueryEscape(account),
		url.QueryEscape(issuer), secret)
}

// VerifyTOTP 校验 6 位动态码，window 为允许的时间窗偏移步数（防时钟漂移）。
func VerifyTOTP(secret, code string, at time.Time, window int) bool {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(code) != 6 {
		return false
	}
	counter := uint64(at.Unix()) / uint64(totpPeriod.Seconds())
	for i := -window; i <= window; i++ {
		if hotp(key, uint64(int64(counter)+int64(i))) == code {
			return true
		}
	}
	return false
}

func hotp(key []byte, counter uint64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf)
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (int(sum[offset])&0x7f)<<24 |
		(int(sum[offset+1])&0xff)<<16 |
		(int(sum[offset+2])&0xff)<<8 |
		(int(sum[offset+3]) & 0xff)
	return fmt.Sprintf("%06d", code%1000000)
}
