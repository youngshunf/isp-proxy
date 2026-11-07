// Package session 定义会话 Cookie 与 CSRF 双提交（docs/06 §4、§8.2）。
//
//   - 用户 Cookie isp_session（Path=/api）、管理员 Cookie isp_admin（Path=/admin-api），互不通用：
//     两者路径不同，浏览器不会把用户 Cookie 发给管理端；即使伪造同名 Cookie，库里会话的 kind 也不符。
//   - Cookie 里只放 32 字节随机令牌，库里存 SHA-256（sessions.token_hash）。
//   - CSRF：任何缺少 isp_csrf 的响应都会下发（非 HttpOnly、Path=/）；所有非 GET/HEAD/OPTIONS 请求必须带
//     值相同的 X-CSRF-Token 头，否则 403 forbidden。登录成功时换发新值。
package session

import (
	"net/http"
	"time"

	"github.com/go-gost/gost/internal/control/secret"
)

// Cookie 与请求头名称。
const (
	UserCookie  = "isp_session"
	AdminCookie = "isp_admin"
	CSRFCookie  = "isp_csrf"
	CSRFHeader  = "X-CSRF-Token"

	UserPath  = "/api"
	AdminPath = "/admin-api"
)

// NewToken 生成会话令牌及其库内哈希。
func NewToken() (token string, hash []byte) {
	token = secret.RandomToken(32)
	return token, secret.HashToken(token)
}

// Cookies 按部署环境（生产为 Secure）设置 Cookie。
type Cookies struct {
	Secure bool
}

func (c Cookies) set(w http.ResponseWriter, name, value, path string, expires time.Time, httpOnly bool) {
	ck := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: httpOnly, Secure: c.Secure, SameSite: http.SameSiteLaxMode}
	if value == "" {
		ck.MaxAge = -1
		ck.Expires = time.Unix(1, 0)
	} else if !expires.IsZero() {
		ck.Expires = expires.UTC()
		ck.MaxAge = int(time.Until(expires).Seconds())
		if ck.MaxAge <= 0 {
			ck.MaxAge = 1
		}
	}
	replaceCookie(w, ck)
}

// replaceCookie 写入 Set-Cookie，并移除同一响应里已写入的同名同路径 Cookie。
func replaceCookie(w http.ResponseWriter, ck *http.Cookie) {
	h := w.Header()
	var kept []string
	for _, v := range h.Values("Set-Cookie") {
		if old, err := http.ParseSetCookie(v); err == nil && old.Name == ck.Name && old.Path == ck.Path {
			continue
		}
		kept = append(kept, v)
	}
	h.Del("Set-Cookie")
	for _, v := range kept {
		h.Add("Set-Cookie", v)
	}
	h.Add("Set-Cookie", ck.String())
}

// SetUser 设置用户会话 Cookie。
func (c Cookies) SetUser(w http.ResponseWriter, token string, expires time.Time) {
	c.set(w, UserCookie, token, UserPath, expires, true)
}

// ClearUser 清除用户会话 Cookie。
func (c Cookies) ClearUser(w http.ResponseWriter) {
	c.set(w, UserCookie, "", UserPath, time.Time{}, true)
}

// SetAdmin 设置管理员会话 Cookie。
func (c Cookies) SetAdmin(w http.ResponseWriter, token string, expires time.Time) {
	c.set(w, AdminCookie, token, AdminPath, expires, true)
}

// ClearAdmin 清除管理员会话 Cookie。
func (c Cookies) ClearAdmin(w http.ResponseWriter) {
	c.set(w, AdminCookie, "", AdminPath, time.Time{}, true)
}

// csrfLifetime 是 isp_csrf 的有效期；过期后下一个响应会重新下发。
const csrfLifetime = 365 * 24 * time.Hour

// EnsureCSRF 在请求没有 isp_csrf 时下发一个。
func (c Cookies) EnsureCSRF(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(CSRFCookie); err == nil && len(ck.Value) >= 16 {
		return
	}
	c.RotateCSRF(w)
}

// RotateCSRF 下发新的 isp_csrf（登录成功时调用），返回新值。
func (c Cookies) RotateCSRF(w http.ResponseWriter) string {
	v := secret.RandomToken(32)
	c.set(w, CSRFCookie, v, "/", time.Now().Add(csrfLifetime), false)
	return v
}

// SafeMethod 报告方法是否不需要 CSRF 校验。
func SafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// CheckCSRF 校验双提交：请求必须同时带 isp_csrf Cookie 与相同值的 X-CSRF-Token 头（至少 16 个字符）。
func CheckCSRF(r *http.Request) bool {
	ck, err := r.Cookie(CSRFCookie)
	if err != nil || len(ck.Value) < 16 {
		return false
	}
	h := r.Header.Get(CSRFHeader)
	return secret.Equal([]byte(h), []byte(ck.Value))
}

// TokenFrom 返回请求里指定 Cookie 的值。
func TokenFrom(r *http.Request, name string) string {
	ck, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return ck.Value
}
