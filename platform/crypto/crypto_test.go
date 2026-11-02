package crypto_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"ziniao/platform/crypto"
)

// TestPasswordHash 口令哈希：正确口令通过、错误口令失败、密文不含明文。
func TestPasswordHash(t *testing.T) {
	enc, err := crypto.HashPassword("Zn@12345678")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if strings.Contains(enc, "Zn@12345678") {
		t.Fatal("密文中不得出现明文（L4）")
	}
	ok, _, err := crypto.VerifyPassword("Zn@12345678", enc)
	if err != nil || !ok {
		t.Fatalf("正确口令应通过: ok=%v err=%v", ok, err)
	}
	if ok, _, _ := crypto.VerifyPassword("wrong-password", enc); ok {
		t.Fatal("错误口令不应通过")
	}
}

// TestJWT JWT 签发/校验/过期/防篡改。
func TestJWT(t *testing.T) {
	token, err := crypto.SignJWT("secret", map[string]any{"sub": "u1", "tid": "t1"}, time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	claims, err := crypto.ParseJWT("secret", token)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if claims["sub"] != "u1" || claims["tid"] != "t1" {
		t.Fatalf("声明不符: %v", claims)
	}
	if _, err := crypto.ParseJWT("other-secret", token); err == nil {
		t.Fatal("错误密钥不应通过")
	}
	expired, err := crypto.SignJWT("secret", map[string]any{"sub": "u1"}, -time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := crypto.ParseJWT("secret", expired); err == nil {
		t.Fatal("过期令牌不应通过")
	}
	parts := strings.Split(token, ".")
	if _, err := crypto.ParseJWT("secret", parts[0]+"."+parts[1]+"."+parts[2]+"x"); err == nil {
		t.Fatal("篡改签名不应通过")
	}
	if _, err := crypto.ParseJWT("", token); err == nil {
		t.Fatal("空密钥不应通过")
	}

	// 头部算法必须固定为 HS256，即使攻击者能用相同密钥重新签名也不能放行。
	algHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := parts[1]
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(algHeader + "." + payload))
	algToken := algHeader + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, err := crypto.ParseJWT("secret", algToken); err == nil {
		t.Fatal("非 HS256 算法不应通过")
	}

	// exp 是必需声明，不能接受没有过期时间的长期令牌。
	noExpPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1"}`))
	noExpInput := parts[0] + "." + noExpPayload
	noExpMAC := hmac.New(sha256.New, []byte("secret"))
	noExpMAC.Write([]byte(noExpInput))
	noExpToken := noExpInput + "." + base64.RawURLEncoding.EncodeToString(noExpMAC.Sum(nil))
	if _, err := crypto.ParseJWT("secret", noExpToken); err == nil {
		t.Fatal("缺少 exp 的令牌不应通过")
	}
}

// TestTOTP TOTP 生成与校验（含时间窗）。
func TestTOTP(t *testing.T) {
	secret := crypto.NewTOTPSecret()
	now := time.Now()
	// 用同一算法推导当前窗口的正确口令进行验证
	code := mustCode(t, secret, now)
	if !crypto.VerifyTOTP(secret, code, now, 1) {
		t.Fatal("当前窗口口令应通过")
	}
	if crypto.VerifyTOTP(secret, "000000", now, 1) {
		t.Fatal("错误口令不应通过")
	}
	if !crypto.VerifyTOTP(secret, mustCode(t, secret, now.Add(-30*time.Second)), now, 1) {
		t.Fatal("上一窗口口令在时间窗内应通过")
	}
}

// mustCode 复用被测实现推导当前窗口口令（测试辅助）。
func mustCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	for i := 0; i < 1000000; i++ {
		code := format(i)
		if crypto.VerifyTOTP(secret, code, at, 0) {
			return code
		}
	}
	t.Fatal("未能在有限空间内推出当前窗口口令")
	return ""
}

func format(i int) string {
	s := strings.Repeat("0", 6-len(itoa(i))) + itoa(i)
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	return out
}
