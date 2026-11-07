package admin

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA1 测试向量（密钥 "12345678901234567890"，8 位）。
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	cases := []struct {
		unix int64
		code string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		step := TOTPStep(time.Unix(c.unix, 0))
		if got := HOTP(secret, uint64(step), 8); got != c.code {
			t.Errorf("T=%d: %s, want %s", c.unix, got, c.code)
		}
		// 6 位是同一截断值的低 6 位。
		if got := TOTPCode(secret, time.Unix(c.unix, 0)); got != c.code[2:] {
			t.Errorf("T=%d 6 位: %s, want %s", c.unix, got, c.code[2:])
		}
	}
}

// RFC 4226 附录 D 的 HOTP 向量。
func TestHOTPRFC4226Vectors(t *testing.T) {
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for i, w := range want {
		if got := HOTP([]byte("12345678901234567890"), uint64(i), 6); got != w {
			t.Errorf("counter %d: %s, want %s", i, got, w)
		}
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_800_000_015, 0)
	cur := TOTPStep(now)
	for d := int64(-1); d <= 1; d++ {
		code := HOTP(secret, uint64(cur+d), 6)
		if step, ok := VerifyTOTP(secret, code, now, 0); !ok || step != cur+d {
			t.Errorf("偏移 %d 应当通过", d)
		}
	}
	for _, d := range []int64{-2, 2} {
		if _, ok := VerifyTOTP(secret, HOTP(secret, uint64(cur+d), 6), now, 0); ok {
			t.Errorf("偏移 %d 不应通过", d)
		}
	}
	code := TOTPCode(secret, now)
	if _, ok := VerifyTOTP(secret, code, now, cur); ok {
		t.Error("已使用的时间步不能重放")
	}
	if _, ok := VerifyTOTP(secret, "12345", now, 0); ok {
		t.Error("位数不对应当失败")
	}
	dec, err := DecodeSecret(strings.ToLower(EncodeSecret(secret)))
	if err != nil || string(dec) != string(secret) {
		t.Fatal(err)
	}
	u := OTPAuthURI("示例代理", "a@example.com", secret)
	if !strings.HasPrefix(u, "otpauth://totp/") || !strings.Contains(u, "secret="+EncodeSecret(secret)) {
		t.Fatal(u)
	}
}
