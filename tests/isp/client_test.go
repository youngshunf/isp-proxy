//go:build ispe2e

package isp

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// APIError 是非 2xx 响应（docs/06 §4 的错误体）。
type APIError struct {
	Method, Path string
	Status       int
	Code         string `json:"code"`
	Message      string `json:"message"`
	Body         string
	Header       http.Header
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d %s %s", e.Method, e.Path, e.Status, e.Code, e.Body)
}

// StatusOf 返回 err 的 HTTP 状态（非 APIError 返回 0）。
func StatusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// CodeOf 返回 err 的错误码。
func CodeOf(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// Client 是带 Cookie、CSRF 双提交与幂等键的 JSON 客户端。prefix 为 /api 或 /admin-api。
type Client struct {
	t      testing.TB
	base   string
	prefix string
	hc     *http.Client
	// XFF 非空时作为 X-Forwarded-For 发送（控制面默认信任回环代理），使各测试用户的来源 IP 互不相同，
	// 避免共享 127.0.0.1 触发按 IP 限流。
	XFF string
}

func newClient(t testing.TB, base, prefix string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{t: t, base: base, prefix: prefix, hc: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

type reqOpt func(*http.Request)

// Idem 设置 Idempotency-Key。
func Idem(key string) reqOpt { return func(r *http.Request) { r.Header.Set("Idempotency-Key", key) } }

// NoCSRF 去掉 X-CSRF-Token（测试 CSRF 校验）。
func NoCSRF() reqOpt { return func(r *http.Request) { r.Header.Del("X-CSRF-Token") } }

// Header 设置任意请求头。
func Header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// NewKey 生成随机幂等键。
func NewKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "e2e-" + hex.EncodeToString(b[:])
}

func (c *Client) cookie(name string) string {
	u, _ := url.Parse(c.base + c.prefix + "/")
	for _, ck := range c.hc.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// CSRF 返回当前 isp_csrf；没有时先 GET /me 取得。
func (c *Client) CSRF() string {
	if v := c.cookie("isp_csrf"); v != "" {
		return v
	}
	_, _ = c.Do("GET", "/me", nil, nil)
	return c.cookie("isp_csrf")
}

// Do 发请求；2xx 时把响应体解析到 out（可为 nil），否则返回 *APIError。path 相对 prefix；以 "/" 开头的绝对路径
// 用 Raw。
func (c *Client) Do(method, path string, in, out any, opts ...reqOpt) (*http.Response, error) {
	return c.Raw(method, c.prefix+path, in, out, opts...)
}

// Raw 同 Do，但 path 是完整路径（不加前缀）。
func (c *Client) Raw(method, path string, in, out any, opts ...reqOpt) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != "GET" && method != "HEAD" {
		req.Header.Set("X-CSRF-Token", c.CSRF())
	}
	if c.XFF != "" {
		req.Header.Set("X-Forwarded-For", c.XFF)
	}
	for _, o := range opts {
		o(req)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		ae := &APIError{Method: method, Path: path, Status: res.StatusCode, Body: string(raw), Header: res.Header}
		_ = json.Unmarshal(raw, ae)
		return res, ae
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return res, fmt.Errorf("%s %s: 解析响应: %w: %s", method, path, err, truncate(string(raw), 400))
		}
	}
	return res, nil
}

// Must 同 Do，失败即 Fatal。
func (c *Client) Must(method, path string, in, out any, opts ...reqOpt) {
	c.t.Helper()
	if _, err := c.Do(method, path, in, out, opts...); err != nil {
		c.t.Fatal(err)
	}
}

// Get 是 Must("GET", …)。
func (c *Client) Get(path string, out any) {
	c.t.Helper()
	c.Must("GET", path, nil, out)
}

// ExpectStatus 断言请求以给定状态失败（或成功）。
func (c *Client) ExpectStatus(want int, method, path string, in any, opts ...reqOpt) *APIError {
	c.t.Helper()
	res, err := c.Do(method, path, in, nil, opts...)
	got := 0
	if res != nil {
		got = res.StatusCode
	}
	if err != nil && res == nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	if got != want {
		c.t.Fatalf("%s %s: 期望 HTTP %d，得到 %d（%v）", method, path, want, got, err)
	}
	var ae *APIError
	errors.As(err, &ae)
	return ae
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// containsAny 报告 s 是否包含 subs 中任一（用于“无数据泄露”断言）。
func containsAny(s string, subs ...string) string {
	for _, x := range subs {
		if x != "" && strings.Contains(s, x) {
			return x
		}
	}
	return ""
}
