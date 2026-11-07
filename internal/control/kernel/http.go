package kernel

import (
	"net/http"

	"github.com/go-gost/gost/internal/control/httpx"
)

// UserOf 取已验证的当前用户；缺失时返回 unauthorized / unverified（防御性检查，主体由平台中间件注入）。
func UserOf(r *http.Request) (httpx.Principal, error) {
	p, ok := httpx.UserFrom(r.Context())
	if !ok || p.UserID == "" {
		return p, httpx.Unauthorized()
	}
	if !p.Verified {
		return p, httpx.Unverified()
	}
	return p, nil
}

// WritableUserOf 同 UserOf，并拒绝封禁用户的写操作（docs/06 §8.2：封禁用户写操作 412）。
func WritableUserOf(r *http.Request) (httpx.Principal, error) {
	p, err := UserOf(r)
	if err != nil {
		return p, err
	}
	if p.Banned {
		return p, httpx.Precondition("账号已被封禁，不能进行该操作；如有疑问请联系客服")
	}
	return p, nil
}

// AdminOf 取当前管理员。
func AdminOf(r *http.Request) (httpx.AdminPrincipal, error) {
	p, ok := httpx.AdminFrom(r.Context())
	if !ok || p.AdminID == "" {
		return p, httpx.Unauthorized()
	}
	return p, nil
}

// Reply 写结果：err 非空写错误，否则以 status 写 v（v 为 nil 且 status 为 204 时不写体）。
func Reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	httpx.WriteJSON(w, status, v)
}

// Decode 严格解析请求体到 T（未知字段 422）。请求体为空时返回零值：契约中可选请求体的操作（暂停、确认等）允许不带体。
func Decode[T any](r *http.Request) (T, error) {
	var v T
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return v, nil
	}
	err := httpx.DecodeJSON(r, &v)
	return v, err
}

// AdminH 把 fn 包装为管理端处理器：取管理员、执行、按返回的状态码写结果。
func AdminH(fn func(r *http.Request, a httpx.AdminPrincipal) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := AdminOf(r)
		if err != nil {
			Reply(w, r, 0, nil, err)
			return
		}
		status, v, err := fn(r, a)
		Reply(w, r, status, v, err)
	}
}

// UserH 包装用户端只读处理器（要求已验证邮箱）。
func UserH(fn func(r *http.Request, p httpx.Principal) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := UserOf(r)
		if err != nil {
			Reply(w, r, 0, nil, err)
			return
		}
		status, v, err := fn(r, p)
		Reply(w, r, status, v, err)
	}
}

// UserW 包装用户端写处理器（要求已验证邮箱，封禁用户 412）。
func UserW(fn func(r *http.Request, p httpx.Principal) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := WritableUserOf(r)
		if err != nil {
			Reply(w, r, 0, nil, err)
			return
		}
		status, v, err := fn(r, p)
		Reply(w, r, status, v, err)
	}
}

// Body 解析请求体，失败时返回的 error 可直接作为处理器结果。
func Body[T any](r *http.Request) (T, error) { return Decode[T](r) }
