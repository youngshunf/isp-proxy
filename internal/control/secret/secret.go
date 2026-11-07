// Package secret 提供控制面的密码学原语（docs/06 §3）：
//
//   - Box：AES-256-GCM 可逆加密（接入密码、上游密码、TOTP 密钥），密钥来自 ISP_SECRET_KEY，密文格式 nonce || 密文；
//   - MAC：HMAC-SHA256（邮件验证码等一次性材料只存 MAC，不存明文），子密钥由主密钥派生，与加密密钥分开；
//   - 登录密码：argon2id，PHC 串格式；
//   - 随机令牌与令牌哈希（会话 Cookie 只放随机令牌，库里存 SHA-256）。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// KeySize 是 ISP_SECRET_KEY 解码后的字节数。
const KeySize = 32

// ParseKey 解析 ISP_SECRET_KEY：64 位十六进制或标准/URL base64（含或不含填充），解码后必须是 32 字节。
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("secret: 密钥为空")
	}
	if len(s) == 2*KeySize {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != KeySize {
				return nil, fmt.Errorf("secret: 密钥解码后为 %d 字节，需要 %d 字节", len(b), KeySize)
			}
			return b, nil
		}
	}
	return nil, errors.New("secret: 密钥必须是 64 位十六进制或 base64 编码的 32 字节")
}

// Box 用主密钥做 AES-256-GCM 加密，并派生 MAC 子密钥。可并发使用。
type Box struct {
	aead   cipher.AEAD
	macKey []byte
}

// NewBox 用 32 字节主密钥创建 Box。
func NewBox(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secret: 密钥长度 %d，需要 %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("isp-control/mac/v1"))
	return &Box{aead: aead, macKey: m.Sum(nil)}, nil
}

// Seal 加密 plaintext，返回 nonce || 密文。aad 为附加认证数据（可为 nil），解密时必须相同。
func (b *Box) Seal(plaintext, aad []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic("secret: crypto/rand 不可用: " + err.Error())
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad)
}

// ErrDecrypt 表示密文被篡改、密钥不对或 aad 不符。
var ErrDecrypt = errors.New("secret: 解密失败")

// Open 解密 Seal 的输出。
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrDecrypt
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}

// MAC 返回各部分（以 0 字节分隔）的 HMAC-SHA256。用于一次性材料：库里只存 MAC，验证时重算比较。
func (b *Box) MAC(parts ...string) []byte {
	m := hmac.New(sha256.New, b.macKey)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{0})
		}
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

// Equal 常量时间比较。
func Equal(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// RandomToken 返回 n 字节随机数的 base64url（无填充）编码。
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("secret: crypto/rand 不可用: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken 返回令牌的 SHA-256，用于会话等高熵令牌在库里的存储与查找。
func HashToken(token string) []byte {
	s := sha256.Sum256([]byte(token))
	return s[:]
}

// Argon2Params 是 argon2id 参数。
type Argon2Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	KeyLen  uint32
	SaltLen int
}

// DefaultArgon2 采用 OWASP 建议的 argon2id 下限（19 MiB、2 次迭代、1 线程）。
var DefaultArgon2 = Argon2Params{Memory: 19 * 1024, Time: 2, Threads: 1, KeyLen: 32, SaltLen: 16}

// HashPassword 用 DefaultArgon2 计算 PHC 串：$argon2id$v=19$m=…,t=…,p=…$salt$hash。
func HashPassword(password string) string {
	p := DefaultArgon2
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		panic("secret: crypto/rand 不可用: " + err.Error())
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// VerifyPassword 校验密码与 PHC 串。串格式不正确时返回 false。
func VerifyPassword(phc, password string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false
	}
	var m, t uint32
	var th uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &th); err != nil || m == 0 || t == 0 || th == 0 || m > 1<<22 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, th, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash 用于“账号不存在”分支的等时校验，避免通过响应时间判断邮箱是否注册。
var dummyHash = HashPassword("isp-control-dummy-password")

// DummyVerify 做一次与 VerifyPassword 等价的计算并丢弃结果。
func DummyVerify(password string) { _ = VerifyPassword(dummyHash, password) }
