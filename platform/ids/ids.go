// Package ids 提供全局唯一、时间有序的 ID（ULID 风格：10 位时间戳 + 16 位随机）。
// 审计日志与事件总线依赖时间有序性做哈希链与增量拉取。
package ids

import (
	"crypto/rand"
	"crypto/subtle"
	"time"
)

// Crockford base32（去掉 I/L/O/U，避免人工抄写歧义）。
const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New 返回 26 字符 ULID。
func New() string { return NewAt(time.Now()) }

// NewAt 用指定时间生成，便于测试中构造确定性 ID。
func NewAt(t time.Time) string {
	ms := uint64(t.UnixMilli())
	var out [26]byte
	for i := 9; i >= 0; i-- {
		out[i] = encoding[int(ms&31)]
		ms >>= 5
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic("ids: 随机数不可用: " + err.Error())
	}
	for i := 0; i < 16; i++ {
		out[10+i] = encoding[int(rnd[i]&31)]
	}
	return string(out[:])
}

// RandomBytes 生成 n 字节密码学安全随机数（盐、TOTP 种子、API 密钥）。
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("ids: 随机数不可用: " + err.Error())
	}
	return b
}

// ConstantTimeEqual 常量时间比较，防止时序侧信道。
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
