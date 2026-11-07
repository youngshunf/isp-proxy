package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP（RFC 6238）：HMAC-SHA1，30 秒步长，6 位，验证时允许 ±1 步（D-15）。

const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret 生成 20 字节（160 位，RFC 4226 建议长度）随机密钥。
func NewTOTPSecret() []byte {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("admin: crypto/rand 不可用: " + err.Error())
	}
	return b
}

// EncodeSecret 返回密钥的 Base32（无填充），即验证器 App 里手动输入的串。
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// DecodeSecret 解析 Base32 密钥（忽略空格与大小写）。
func DecodeSecret(s string) ([]byte, error) {
	return b32.DecodeString(strings.ToUpper(strings.ReplaceAll(strings.TrimRight(s, "="), " ", "")))
}

// HOTP 计算 RFC 4226 一次性密码。
func HOTP(secret []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, v%mod)
}

// TOTPStep 返回 t 所在的时间步。
func TOTPStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// TOTPCode 返回 t 时刻的 6 位验证码。
func TOTPCode(secret []byte, t time.Time) string {
	return HOTP(secret, uint64(TOTPStep(t)), totpDigits)
}

// VerifyTOTP 在 now 的 ±1 步内查找与 code 匹配且大于 lastStep 的时间步（防重放：同一时间步只能用一次）。
// 匹配时返回该时间步。
func VerifyTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	if len(code) != totpDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	matched, ok := int64(0), false
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := cur + d
		if step <= lastStep || step < 0 {
			continue
		}
		// 逐个比较不提前返回，耗时与匹配位置无关。
		if hmac.Equal([]byte(HOTP(secret, uint64(step), totpDigits)), []byte(code)) && !ok {
			matched, ok = step, true
		}
	}
	return matched, ok
}

// OTPAuthURI 返回供验证器 App 扫码的 otpauth:// URI。
func OTPAuthURI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}
