package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// KeyManager 密钥管理抽象（Q10 / ADR-0011）：
// Phase 0-1 用本地 KEK 实现（SOPS/age 管理的密钥文件），P6 可替换为云 KMS，
// 业务代码无需改动（只依赖 Wrap/Unwrap 两个动作）。
type KeyManager interface {
	// Wrap 用指定版本的主密钥加密 DEK。
	Wrap(dek []byte, version int) ([]byte, error)
	// Unwrap 解出 DEK 明文（仅驻内存，绝不落盘）。
	Unwrap(wrapped []byte, version int) ([]byte, error)
	// CurrentVersion 当前主密钥版本（用于新数据封包）。
	CurrentVersion() int
}

// LocalKEK 本地主密钥实现：version → 32 字节密钥（由 SOPS/age 或 KMS 导出后注入内存）。
type LocalKEK struct {
	mu      sync.RWMutex
	keys    map[int][]byte
	current int
}

// NewLocalKEK 构造本地主密钥库；keys 为空时用随机密钥（仅 dev，重启即失效，必须在日志告警）。
func NewLocalKEK(keys map[int][]byte, current int) (*LocalKEK, error) {
	if len(keys) == 0 {
		k := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, k); err != nil {
			return nil, err
		}
		keys = map[int][]byte{1: k}
		current = 1
	}
	if _, ok := keys[current]; !ok {
		return nil, errors.New("crypto: 当前密钥版本不存在")
	}
	cp := map[int][]byte{}
	for v, k := range keys {
		if len(k) != 32 {
			return nil, fmt.Errorf("crypto: 密钥版本 %d 长度必须为 32 字节", v)
		}
		cp[v] = append([]byte{}, k...)
	}
	return &LocalKEK{keys: cp, current: current}, nil
}

// AddVersion 新增/轮换主密钥版本（旧版本必须保留以便解密历史数据）。
func (k *LocalKEK) AddVersion(version int, key []byte) error {
	if len(key) != 32 {
		return errors.New("crypto: 密钥长度必须为 32 字节")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[version] = append([]byte{}, key...)
	return nil
}

func (k *LocalKEK) CurrentVersion() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.current
}

// SetCurrent 切换当前写版本（轮换：新数据用新版本封包，旧数据仍可用旧版本解开）。
func (k *LocalKEK) SetCurrent(version int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.keys[version]; !ok {
		return fmt.Errorf("crypto: 密钥版本 %d 不存在", version)
	}
	k.current = version
	return nil
}

func (k *LocalKEK) key(version int) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	key, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("crypto: 无密钥版本 %d", version)
	}
	return key, nil
}

func (k *LocalKEK) Wrap(dek []byte, version int) ([]byte, error) {
	key, err := k.key(version)
	if err != nil {
		return nil, err
	}
	return gcmSeal(key, dek)
}

func (k *LocalKEK) Unwrap(wrapped []byte, version int) ([]byte, error) {
	key, err := k.key(version)
	if err != nil {
		return nil, err
	}
	return gcmOpen(key, wrapped)
}

// EnvelopeCipher 信封加密：KEK 包 DEK，DEK 加密业务明文（L4）。
// 密文格式：v<version>.<b64(wrapped_dek)>.<b64(nonce+ciphertext)>
type EnvelopeCipher struct {
	km KeyManager
}

// NewEnvelope 构造信封加密器。
func NewEnvelope(km KeyManager) (*EnvelopeCipher, error) {
	if km == nil {
		return nil, errors.New("crypto: 缺少 KeyManager")
	}
	return &EnvelopeCipher{km: km}, nil
}

// CurrentVersion 当前写入使用的密钥版本。
func (c *EnvelopeCipher) CurrentVersion() int { return c.km.CurrentVersion() }

// Seal 封包：生成一次性 DEK → 加密明文 → 用主密钥包住 DEK。
func (c *EnvelopeCipher) Seal(plaintext string) (string, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return "", err
	}
	ct, err := gcmSeal(dek, []byte(plaintext))
	if err != nil {
		return "", err
	}
	version := c.km.CurrentVersion()
	wrapped, err := c.km.Wrap(dek, version)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v%d.%s.%s", version,
		base64.RawStdEncoding.EncodeToString(wrapped),
		base64.RawStdEncoding.EncodeToString(ct)), nil
}

// Open 解包：主密钥解出 DEK → 解出明文（明文仅驻内存）。
func (c *EnvelopeCipher) Open(enc string) (string, error) {
	version, wrapped, ct, err := parseEnvelope(enc)
	if err != nil {
		return "", err
	}
	dek, err := c.km.Unwrap(wrapped, version)
	if err != nil {
		return "", err
	}
	pt, err := gcmOpen(dek, ct)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// VersionOf 读取密文的密钥版本（用于批量重封与轮换进度统计）。
func (c *EnvelopeCipher) VersionOf(enc string) (int, error) {
	v, _, _, err := parseEnvelope(enc)
	return v, err
}

// Rekey 用当前主密钥版本重新封包（轮换时不接触业务明文以外的东西：解开 DEK 再重包）。
func (c *EnvelopeCipher) Rekey(enc string) (string, error) {
	version, wrapped, ct, err := parseEnvelope(enc)
	if err != nil {
		return "", err
	}
	dek, err := c.km.Unwrap(wrapped, version)
	if err != nil {
		return "", err
	}
	cur := c.km.CurrentVersion()
	w, err := c.km.Wrap(dek, cur)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v%d.%s.%s", cur,
		base64.RawStdEncoding.EncodeToString(w),
		base64.RawStdEncoding.EncodeToString(ct)), nil
}

func parseEnvelope(enc string) (int, []byte, []byte, error) {
	parts := strings.Split(enc, ".")
	if len(parts) != 3 {
		return 0, nil, nil, errors.New("crypto: 密文格式非法")
	}
	v, err := strconv.Atoi(strings.TrimPrefix(parts[0], "v"))
	if err != nil {
		return 0, nil, nil, errors.New("crypto: 密文版本非法")
	}
	wrapped, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, nil, nil, err
	}
	ct, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, nil, nil, err
	}
	return v, wrapped, ct, nil
}

func gcmSeal(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func gcmOpen(key, ct []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ct) < gcm.NonceSize() {
		return nil, errors.New("crypto: 密文长度不足")
	}
	return gcm.Open(nil, ct[:gcm.NonceSize()], ct[gcm.NonceSize():], nil)
}
