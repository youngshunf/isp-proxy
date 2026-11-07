package kernel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- 加密（docs/06 §3：接入密码与上游密码 AES-256-GCM，nonce || 密文） ----

// Seal 加密明文。
func (e *Env) Seal(plain string) []byte {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("kernel: crypto/rand 不可用: " + err.Error())
	}
	return e.aead.Seal(nonce, nonce, []byte(plain), nil)
}

// Open 解密。
func (e *Env) Open(sealed []byte) (string, error) {
	n := e.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("kernel: 密文过短")
	}
	out, err := e.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", fmt.Errorf("kernel: 解密失败: %w", err)
	}
	return string(out), nil
}

// ---- 随机串 ----

const lowerAlnum = "abcdefghijkmnpqrstuvwxyz23456789"
const mixedAlnum = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

func randFrom(alphabet string, n int) string {
	var sb strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("kernel: crypto/rand 不可用: " + err.Error())
		}
		sb.WriteByte(alphabet[v.Int64()])
	}
	return sb.String()
}

// NewAccessUsername 生成接入用户名（^[a-z0-9]{6,32}$）。
func NewAccessUsername() string { return "u" + randFrom(lowerAlnum, 11) }

// NewAccessPassword 生成接入密码（16 位，约 93 位熵），与登录密码、上游凭据无关（R-04.2）。
func NewAccessPassword() string { return randFrom(mixedAlnum, 16) }

// ---- 视图公共件 ----

// Money 是金额（分）。
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// CNY 返回人民币金额。
func CNY(cents int64) Money { return Money{Amount: cents, Currency: "CNY"} }

// AdminRef 是管理员引用。
type AdminRef struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// UserRef 是用户引用。
type UserRef struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// Actor 是操作者。
type Actor struct {
	Kind  string  `json:"kind"`
	ID    *string `json:"id"`
	Email string  `json:"email,omitempty"`
}

// LoadAdminRef 按 ID 读管理员引用；id 为空返回 nil。
func LoadAdminRef(ctx context.Context, q Q, id *string) (*AdminRef, error) {
	if id == nil || *id == "" {
		return nil, nil
	}
	ref := &AdminRef{ID: *id}
	if err := q.QueryRow(ctx, `SELECT email FROM admins WHERE id = $1`, *id).Scan(&ref.Email); err != nil {
		if IsNoRows(err) {
			return ref, nil
		}
		return nil, err
	}
	return ref, nil
}

// TimePtr 把可空时间转成 JSON 友好的指针（UTC）。
func TimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Ptr 返回 v 的指针。
func Ptr[T any](v T) *T { return &v }

// StrOr 返回 *s 或默认值。
func StrOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// FormatBytes 用十进制 GB/MB 展示字节数（D-03：1 GB = 10^9 字节）。
func FormatBytes(b int64) string {
	f := func(v float64, unit string) string {
		s := strconv.FormatFloat(v, 'f', 2, 64)
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		return s + " " + unit
	}
	switch {
	case b >= 1_000_000_000:
		return f(float64(b)/1e9, "GB")
	case b >= 1_000_000:
		return f(float64(b)/1e6, "MB")
	default:
		return strconv.FormatInt(b, 10) + " 字节"
	}
}

// TrimName 规范化订阅名称：去首尾空白，长度 1～24 个字符。
func TrimName(name string) (string, bool) {
	n := strings.TrimSpace(name)
	c := utf8.RuneCountInString(n)
	return n, c >= 1 && c <= 24
}

// TruncateRunes 截断到 n 个字符。
func TruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// HashJSON 返回 v 的 JSON 的 SHA-256。
func HashJSON(v any) []byte {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return h[:]
}

// Days 返回 n 天。
func Days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }
