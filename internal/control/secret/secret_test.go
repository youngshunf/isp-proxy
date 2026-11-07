package secret

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseKey(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 32)
	for _, s := range []string{strings.Repeat("07", 32), base64.StdEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw)} {
		k, err := ParseKey(s)
		if err != nil || !bytes.Equal(k, raw) {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"", "short", base64.StdEncoding.EncodeToString([]byte("16 bytes only!!!"))} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("%q 应当被拒绝", s)
		}
	}
}

func TestBox(t *testing.T) {
	b, err := NewBox(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ct := b.Seal([]byte("hello"), []byte("ctx"))
	pt, err := b.Open(ct, []byte("ctx"))
	if err != nil || string(pt) != "hello" {
		t.Fatal(pt, err)
	}
	if _, err := b.Open(ct, []byte("other")); err == nil {
		t.Fatal("aad 不符应当失败")
	}
	ct[len(ct)-1] ^= 1
	if _, err := b.Open(ct, []byte("ctx")); err == nil {
		t.Fatal("篡改应当失败")
	}
	if Equal(b.MAC("a", "bc"), b.MAC("ab", "c")) {
		t.Fatal("MAC 分隔失效")
	}
	b2, _ := NewBox(bytes.Repeat([]byte{2}, 32))
	if Equal(b.MAC("x"), b2.MAC("x")) {
		t.Fatal("不同密钥 MAC 相同")
	}
}

func TestPassword(t *testing.T) {
	h := HashPassword("correct horse")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatal(h)
	}
	if !VerifyPassword(h, "correct horse") || VerifyPassword(h, "wrong") || VerifyPassword("garbage", "x") {
		t.Fatal("校验结果不对")
	}
	if HashPassword("a") == HashPassword("a") {
		t.Fatal("盐没有生效")
	}
}
