// Package crypto 密码哈希 / JWT / TOTP。
//
// 设计取舍（详见 docs/ADR.md ADR-0004）：
//   - 口令哈希先用 stdlib 实现的 PBKDF2-HMAC-SHA256（零外部依赖，可离线运行）；
//     TODO(P1)：接入 argon2id（golang.org/x/crypto/argon2）并做一次性迁移重哈希。
//   - 所有敏感值（TOTP 种子、口令密文）永不以明文出现在日志或 API 响应（L4）。
package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"ziniao/platform/ids"
)

// PasswordIterations PBKDF2 迭代次数（OWASP 2023 对 SHA256 的建议下限 210000）。
// 生产环境应由配置中心下发，此处为默认值。
const PasswordIterations = 210000

const passwordAlgo = "pbkdf2-sha256"

// pbkdf2 单块派生（输出 32 字节 = SHA256 输出长度，只需一个块）。
func pbkdf2(password, salt []byte, iter int) []byte {
	h := hmac.New(sha256.New, password)
	h.Write(salt)
	h.Write([]byte{0, 0, 0, 1})
	u := h.Sum(nil)
	dk := make([]byte, len(u))
	copy(dk, u)
	for i := 2; i <= iter; i++ {
		h = hmac.New(sha256.New, password)
		h.Write(u)
		u = h.Sum(nil)
		for j := range dk {
			dk[j] ^= u[j]
		}
	}
	return dk
}

// HashPassword 返回可存储编码串：algo$iter$salt$hash。
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("crypto: 空口令")
	}
	salt := ids.RandomBytes(16)
	dk := pbkdf2([]byte(password), salt, PasswordIterations)
	return fmt.Sprintf("%s$%d$%s$%s",
		passwordAlgo,
		PasswordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	), nil
}

// VerifyPassword 校验口令，返回 (是否匹配, 是否需要重哈希(迭代次数落后))。
func VerifyPassword(password, encoded string) (ok bool, needsRehash bool, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != passwordAlgo {
		return false, false, fmt.Errorf("crypto: 无法识别的口令编码")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, false, fmt.Errorf("crypto: 非法迭代次数")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, false, fmt.Errorf("crypto: 非法盐")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, false, fmt.Errorf("crypto: 非法哈希")
	}
	got := pbkdf2([]byte(password), salt, iter)
	return subtle.ConstantTimeCompare(got, want) == 1, iter < PasswordIterations, nil
}
