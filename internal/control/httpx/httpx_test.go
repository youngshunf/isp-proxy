package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{NotFound(""), 404, "not_found"},
		{Conflict("x"), 409, "conflict"},
		{Precondition("x"), 412, "precondition"},
		{Invalid("x"), 422, "invalid"},
		{QuotaExhausted("x"), 429, "quota_exhausted"},
		{RateLimited("x", 1500*time.Millisecond), 429, "rate_limited"},
		{Unauthorized(), 401, "unauthorized"},
		{Unverified(), 403, "unverified"},
		{Forbidden(""), 403, "forbidden"},
		{fmt.Errorf("包装: %w", NotFound("x")), 404, "not_found"},
		{errors.New("数据库密码=secret"), 500, "internal"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		WriteError(w, httptest.NewRequest("GET", "/x", nil), c.err)
		if w.Code != c.status {
			t.Errorf("%v: status %d, want %d", c.err, w.Code, c.status)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["code"] != c.code || body["message"] == "" {
			t.Errorf("%v: body %v", c.err, body)
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Errorf("500 响应泄露了内部错误: %s", w.Body.String())
		}
		if c.code == "rate_limited" && w.Header().Get("Retry-After") != "2" {
			t.Errorf("Retry-After = %q", w.Header().Get("Retry-After"))
		}
	}
	w := httptest.NewRecorder()
	WriteError(w, nil, Unverified())
	if !strings.Contains(w.Body.String(), `"action":{"label"`) {
		t.Errorf("缺少 action: %s", w.Body.String())
	}
}

func TestDecodeJSON(t *testing.T) {
	type in struct {
		A string `json:"a"`
	}
	ok := func(body string) error {
		var v in
		return DecodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(body)), &v)
	}
	if err := ok(`{"a":"x"}`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{``, `{"a":"x","b":1}`, `{"a":1}`, `{"a":"x"}{}`, `{"a":"x"} 1`, `nope`,
		`{"a":"` + strings.Repeat("x", MaxBodyBytes) + `"}`} {
		err := ok(bad)
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeInvalid {
			t.Errorf("%.40q: err = %v", bad, err)
		}
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Errorf("不可信直连不得读取 XFF: %s", got)
	}
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 198.51.100.7")
	if got := ClientIP(r); got != "198.51.100.7" {
		t.Errorf("可信代理后取最右不可信地址: %s", got)
	}
	r = r.WithContext(WithClientIP(r.Context(), "10.0.0.1"))
	if got := ClientIP(r); got != "10.0.0.1" {
		t.Errorf("上下文优先: %s", got)
	}
}

func TestPrincipalContext(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if _, ok := UserFrom(r.Context()); ok {
		t.Fatal("空上下文不应有用户")
	}
	ctx := WithAdmin(WithUser(r.Context(), Principal{UserID: "usr_x", Verified: true}), AdminPrincipal{AdminID: "adm_x"})
	if p, ok := UserFrom(ctx); !ok || p.UserID != "usr_x" {
		t.Fatal(p)
	}
	if a, ok := AdminFrom(ctx); !ok || a.AdminID != "adm_x" {
		t.Fatal(a)
	}
	r.Header.Set("Idempotency-Key", "  abcdefgh ")
	if IdempotencyKey(r) != "abcdefgh" {
		t.Fatal(IdempotencyKey(r))
	}
	w := httptest.NewRecorder()
	WriteJSON(w, http.StatusNoContent, map[string]int{"a": 1})
	if w.Body.Len() != 0 {
		t.Fatal("204 不应有响应体")
	}
}
