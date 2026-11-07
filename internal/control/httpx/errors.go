// Package httpx 是控制面对外 HTTP 层的公共约定：错误映射、JSON 编解码、路由描述与请求主体。
//
// 各业务包（account、admin、order、subscription…）只依赖本包：导出 Routes() []httpx.Route，
// 由 internal/control/api 统一挂载并按 Route.Auth 做会话、CSRF、封禁只读等检查（docs/06 §4、§8.2）。
// 处理器内用 UserFrom / AdminFrom 取当前主体，用 WriteJSON / WriteError 输出，用 DecodeJSON 解析请求体。
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// 错误码，与 api/openapi.yaml 的 ErrorCode、前端 ApiErrorCode 一致（docs/06 §4）。
const (
	CodeUnauthorized   = "unauthorized"
	CodeUnverified     = "unverified"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodePrecondition   = "precondition"
	CodeInvalid        = "invalid"
	CodeQuotaExhausted = "quota_exhausted"
	CodeRateLimited    = "rate_limited"
)

// Action 是错误附带的可选恢复动作，例如“去续时长”。
type Action struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}

// Error 是面向客户的业务错误。Message 是中文说明，前端原样展示；RetryAfter 只用于 rate_limited。
type Error struct {
	Status     int
	Code       string
	Message    string
	Action     *Action
	RetryAfter time.Duration
}

func (e *Error) Error() string { return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message) }

// WithAction 返回带恢复动作的副本，例如 httpx.Precondition("…").WithAction("去续时长", "/buy/renew/…")。
func (e *Error) WithAction(label, href string) *Error {
	c := *e
	c.Action = &Action{Label: label, Href: href}
	return &c
}

// errorBody 是 openapi Error 的 JSON 形状。
type errorBody struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Action  *Action `json:"action,omitempty"`
}

func newErr(status int, code, msg, def string) *Error {
	if msg == "" {
		msg = def
	}
	return &Error{Status: status, Code: code, Message: msg}
}

// NotFound 404：不存在或无权访问（两者不区分，docs/06 §4）。
func NotFound(msg string) *Error {
	return newErr(http.StatusNotFound, CodeNotFound, msg, "找不到该内容")
}

// Conflict 409：与现有状态冲突（进行中任务、库存、版本过期、幂等键重用）。
func Conflict(msg string) *Error {
	return newErr(http.StatusConflict, CodeConflict, msg, "与当前状态冲突，请刷新后重试")
}

// Precondition 412：当前状态不允许该操作（含封禁账号的写操作）。
func Precondition(msg string) *Error {
	return newErr(http.StatusPreconditionFailed, CodePrecondition, msg, "当前状态不允许该操作")
}

// Invalid 422：输入不合法。
func Invalid(msg string) *Error {
	return newErr(http.StatusUnprocessableEntity, CodeInvalid, msg, "输入不正确")
}

// QuotaExhausted 429：业务次数用完（如每 24 小时换接入 5 次）。
func QuotaExhausted(msg string) *Error {
	return newErr(http.StatusTooManyRequests, CodeQuotaExhausted, msg, "次数已用完")
}

// RateLimited 429：服务端限流，响应带 Retry-After（秒，向上取整，至少 1）。
func RateLimited(msg string, after time.Duration) *Error {
	e := newErr(http.StatusTooManyRequests, CodeRateLimited, msg, "操作太频繁，请稍后再试")
	e.RetryAfter = after
	return e
}

// Unauthorized 401：未登录、会话过期或已撤销。
func Unauthorized() *Error {
	return &Error{Status: http.StatusUnauthorized, Code: CodeUnauthorized, Message: "请先登录",
		Action: &Action{Label: "去登录", Href: "/login"}}
}

// Unverified 403：邮箱尚未验证。
func Unverified() *Error {
	return &Error{Status: http.StatusForbidden, Code: CodeUnverified, Message: "请先完成邮箱验证",
		Action: &Action{Label: "去验证", Href: "/verify-email"}}
}

// Forbidden 403：管理端权限不足或 CSRF 校验失败。
func Forbidden(msg string) *Error {
	return newErr(http.StatusForbidden, CodeForbidden, msg, "没有权限执行该操作")
}

// WriteJSON 以 status 输出 v 的 JSON。status 为 204 或 v 为 nil 时不写响应体。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	if status == http.StatusNoContent || v == nil {
		w.WriteHeader(status)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("httpx: 编码响应失败", "err", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal","message":"服务暂时不可用，请稍后再试"}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// WriteError 输出错误。*Error（含被包装的）按其状态与代码输出；其他错误一律 500，
// 响应只给通用说明，原始错误写日志（不回显给客户，避免泄露内部细节）。
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if errors.As(err, &e) {
		if e.Code == CodeRateLimited {
			secs := int((e.RetryAfter + time.Second - 1) / time.Second)
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
		WriteJSON(w, e.Status, errorBody{Code: e.Code, Message: e.Message, Action: e.Action})
		return
	}
	attrs := []any{"err", err}
	if r != nil {
		attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
	}
	if r != nil && r.Context().Err() != nil {
		slog.Debug("httpx: 请求已取消", attrs...)
	} else {
		slog.Error("httpx: 处理请求失败", attrs...)
	}
	// 500 的 code 不在 ErrorCode 枚举内；前端按状态码显示通用失败。
	WriteJSON(w, http.StatusInternalServerError, errorBody{Code: "internal", Message: "服务暂时不可用，请稍后再试"})
}

// MaxBodyBytes 是 DecodeJSON 接受的最大请求体。
const MaxBodyBytes = 1 << 20

// DecodeJSON 严格解析请求体到 v：未知字段、多余内容、超过 MaxBodyBytes、空请求体都返回 *Error（invalid）。
func DecodeJSON(r *http.Request, v any) error {
	if r.Body == nil || r.Body == http.NoBody {
		return Invalid("请求体不能为空")
	}
	lr := &io.LimitedReader{R: r.Body, N: MaxBodyBytes + 1}
	dec := json.NewDecoder(lr)
	dec.DisallowUnknownFields()
	tooBig := func() bool { return lr.N <= 0 }
	if err := dec.Decode(v); err != nil {
		switch {
		case tooBig():
			return Invalid("请求体太大")
		case errors.Is(err, io.EOF):
			return Invalid("请求体不能为空")
		}
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field != "" {
			return Invalid("字段 " + ute.Field + " 的类型不正确")
		}
		return Invalid("请求体不是有效的 JSON：" + err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		if tooBig() {
			return Invalid("请求体太大")
		}
		return Invalid("请求体只能包含一个 JSON 对象")
	}
	return nil
}
