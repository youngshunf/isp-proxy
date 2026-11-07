// Package httpx 是控制面 HTTP 层的公共件：错误映射、JSON 编解码、鉴权级别与主体上下文。
//
// 临时副本：本文件按与 ctl-platform 约定的签名先行实现，供 ctl-core 编译与测试。
// ctl-platform 的正式实现合入后以其为准（合并时直接取对方版本）。
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Action 是错误响应中可选的恢复动作。
type Action struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}

// Error 是面向客户的业务错误（docs/06 §4）。
type Error struct {
	Status     int
	Code       string
	Message    string
	Action     *Action
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newErr(status int, code, msg string) *Error { return &Error{Status: status, Code: code, Message: msg} }

// NotFound 404。
func NotFound(msg string) *Error { return newErr(http.StatusNotFound, "not_found", msg) }

// Conflict 409。
func Conflict(msg string) *Error { return newErr(http.StatusConflict, "conflict", msg) }

// Precondition 412。
func Precondition(msg string) *Error { return newErr(http.StatusPreconditionFailed, "precondition", msg) }

// Invalid 422。
func Invalid(msg string) *Error { return newErr(http.StatusUnprocessableEntity, "invalid", msg) }

// QuotaExhausted 429 quota_exhausted。
func QuotaExhausted(msg string) *Error { return newErr(http.StatusTooManyRequests, "quota_exhausted", msg) }

// RateLimited 429 rate_limited，带 Retry-After。
func RateLimited(msg string, after time.Duration) *Error {
	e := newErr(http.StatusTooManyRequests, "rate_limited", msg)
	e.RetryAfter = after
	return e
}

// Unauthorized 401。
func Unauthorized() *Error { return newErr(http.StatusUnauthorized, "unauthorized", "登录已失效，请重新登录") }

// Unverified 403 unverified。
func Unverified() *Error { return newErr(http.StatusForbidden, "unverified", "请先完成邮箱验证") }

// Forbidden 403 forbidden。
func Forbidden(msg string) *Error { return newErr(http.StatusForbidden, "forbidden", msg) }

// WriteJSON 写 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

type errBody struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Action  *Action `json:"action,omitempty"`
}

// WriteError 把 err 映射为错误响应；非 *Error 一律 500 且不泄露细节。
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		WriteJSON(w, http.StatusInternalServerError, errBody{Code: "internal", Message: "服务暂时不可用，请稍后再试"})
		return
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((e.RetryAfter+time.Second-1)/time.Second)))
	}
	WriteJSON(w, e.Status, errBody{Code: e.Code, Message: e.Message, Action: e.Action})
}

const maxBody = 1 << 20

// DecodeJSON 解析请求体；格式错误返回 Invalid。空请求体解析为零值。
func DecodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return Invalid("请求格式不正确")
	}
	return nil
}

// AuthLevel 是路由的鉴权级别。
type AuthLevel int

const (
	Public AuthLevel = iota
	User
	VerifiedUser
	Admin
)

// Route 是一个业务路由；Pattern 与 OpenAPI 路径完全一致（如 /api/subscriptions/{subscriptionId}）。
type Route struct {
	Method  string
	Pattern string
	Auth    AuthLevel
	Handler http.HandlerFunc
}

// Principal 是已登录的用户。
type Principal struct {
	UserID, Email    string
	Verified, Banned bool
}

// AdminPrincipal 是已登录（两步完成）的管理员。
type AdminPrincipal struct {
	AdminID, Email string
}

type userKey struct{}
type adminKey struct{}

// UserFrom 取当前用户。
func UserFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(userKey{}).(Principal)
	return p, ok
}

// AdminFrom 取当前管理员。
func AdminFrom(ctx context.Context) (AdminPrincipal, bool) {
	p, ok := ctx.Value(adminKey{}).(AdminPrincipal)
	return p, ok
}

// WithUser 把用户放入上下文。
func WithUser(ctx context.Context, p Principal) context.Context { return context.WithValue(ctx, userKey{}, p) }

// WithAdmin 把管理员放入上下文。
func WithAdmin(ctx context.Context, p AdminPrincipal) context.Context {
	return context.WithValue(ctx, adminKey{}, p)
}

// IdempotencyKey 返回 Idempotency-Key 头。
func IdempotencyKey(r *http.Request) string { return strings.TrimSpace(r.Header.Get("Idempotency-Key")) }

// ClientIP 返回来源 IP（不信任转发头；反向代理配置由平台侧负责）。
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
