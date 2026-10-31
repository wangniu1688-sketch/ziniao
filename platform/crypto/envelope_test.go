package crypto_test

import (
	"strings"
	"testing"

	"ziniao/platform/crypto"
)

func cipher(t *testing.T) (*crypto.EnvelopeCipher, *crypto.LocalKEK) {
	t.Helper()
	kek, err := crypto.NewLocalKEK(map[int][]byte{1: make([]byte, 32)}, 1)
	if err != nil {
		t.Fatalf("KEK 构造失败: %v", err)
	}
	c, err := crypto.NewEnvelope(kek)
	if err != nil {
		t.Fatalf("信封构造失败: %v", err)
	}
	return c, kek
}

// TestSealOpen 明文封包后可解开，且密文不含明文（L4）。
func TestSealOpen(t *testing.T) {
	c, _ := cipher(t)
	secret := "ShopeePassw0rd!"
	enc, err := c.Seal(secret)
	if err != nil {
		t.Fatalf("封包失败: %v", err)
	}
	if strings.Contains(enc, secret) {
		t.Fatal("密文不得包含明文")
	}
	got, err := c.Open(enc)
	if err != nil || got != secret {
		t.Fatalf("解包失败: %q err=%v", got, err)
	}
	// 相同明文两次封包结果应不同（DEK 随机 + nonce 随机）
	enc2, _ := c.Seal(secret)
	if enc == enc2 {
		t.Fatal("相同明文密文不应重复（缺少随机性）")
	}
}

// TestKeyRotationAndRekey 密钥轮换：旧密文仍可解，Rekey 后版本提升且明文不变。
func TestKeyRotationAndRekey(t *testing.T) {
	c, kek := cipher(t)
	enc, err := c.Seal("PayAccount-9527")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := c.VersionOf(enc)
	if err != nil || v1 != 1 {
		t.Fatalf("版本应为 1，实际 %d", v1)
	}
	// 新增 v2 并切换当前版本
	if err := kek.AddVersion(2, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err := kek.SetCurrent(2); err != nil {
		t.Fatal(err)
	}
	// 旧密文仍可解（历史数据可读）
	if got, err := c.Open(enc); err != nil || got != "PayAccount-9527" {
		t.Fatalf("轮换后旧密文应仍可解: %v", err)
	}
	re, err := c.Rekey(enc)
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := c.VersionOf(re)
	if v2 != 2 {
		t.Fatalf("Rekey 后版本应为 2，实际 %d", v2)
	}
	if got, _ := c.Open(re); got != "PayAccount-9527" {
		t.Fatal("Rekey 后明文必须不变")
	}
	// 新封包使用新版本
	newEnc, _ := c.Seal("new")
	if v, _ := c.VersionOf(newEnc); v != 2 {
		t.Fatalf("新密文应使用 v2，实际 v%d", v)
	}
}

// TestTamperDetected 密文被篡改必须解密失败（GCM 完整性校验）。
func TestTamperDetected(t *testing.T) {
	c, _ := cipher(t)
	enc, _ := c.Seal("TOTP:JBSWY3DPEHPK3PXP")
	parts := strings.Split(enc, ".")
	if len(parts) != 3 {
		t.Fatalf("密文格式异常: %s", enc)
	}
	parts[2] = parts[2][:len(parts[2])-2] + "AA"
	if _, err := c.Open(strings.Join(parts, ".")); err == nil {
		t.Fatal("篡改后的密文不应解密成功")
	}
	if _, err := c.Open("not-a-cipher"); err == nil {
		t.Fatal("非法格式应报错")
	}
}
