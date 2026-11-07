package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/control/account"
	"github.com/go-gost/gost/internal/control/admin"
	"github.com/go-gost/gost/internal/control/api"
	"github.com/go-gost/gost/internal/control/mail"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/session"
	"github.com/go-gost/gost/internal/control/store/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// clock 是可前移的测试时钟：真实时间 + 偏移。始终不早于数据库 now()，发件箱等用库内默认时间的记录也能按它判断到期。
type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	box      *secret.Box
	clock    *clock
	platform *api.Platform
	srv      *httptest.Server
	mailDir  string
	sender   *mail.FileSender
	worker   *mail.Worker
}

type envOpt func(*api.Deps)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	pool := pgtest.New(t)
	box, err := secret.NewBox(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, box: box, clock: &clock{}, mailDir: t.TempDir()}
	e.sender = &mail.FileSender{Dir: e.mailDir}
	e.worker = &mail.Worker{Pool: pool, Box: box, Sender: e.sender, From: "示例代理 <no-reply@example.com>",
		Brand: mail.Brand{Name: "示例代理", SiteURL: "https://portal.example.com"}, Now: e.clock.Now}
	d := api.Deps{Pool: pool, Box: box, Cookies: session.Cookies{Secure: false}, Issuer: "示例代理", Now: e.clock.Now}
	for _, o := range opts {
		o(&d)
	}
	e.platform, err = api.Assemble(d)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.platform.Handler)
	t.Cleanup(e.srv.Close)
	return e
}

// codeOf 投递发件箱并返回最近一封发给 email 的邮件中的 6 位验证码；没有邮件时返回空串。
func (e *env) codeOf(email string) string {
	e.t.Helper()
	m := e.lastMail(email)
	if m == nil {
		return ""
	}
	return regexp.MustCompile(`(?m)^    ([0-9]{6})$`).FindStringSubmatch(m.Body + "\n")[1]
}

// lastMail 投递发件箱并返回最近一封已发给 email 的邮件。
func (e *env) lastMail(email string) *mail.Parsed {
	e.t.Helper()
	if _, err := e.worker.DeliverDue(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	var id string
	err := e.pool.QueryRow(context.Background(), `SELECT id FROM mail_outbox WHERE to_email = $1 AND status = 'sent'
		ORDER BY created_at DESC, sent_at DESC LIMIT 1`, email).Scan(&id)
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(e.mailDir, id+".eml"))
	if err != nil {
		e.t.Fatal(err)
	}
	p, err := mail.Parse(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) mailCount(email string) int {
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM mail_outbox WHERE to_email = $1`, email).Scan(&n)
	return n
}

// auditCount 返回某动作（与结果）的审计条数。
func (e *env) auditCount(action, outcome string) int {
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = $1 AND ($2 = '' OR outcome = $2)`,
		action, outcome).Scan(&n)
	return n
}

// client 是带独立 Cookie 罐的浏览器模拟。
type client struct {
	e   *env
	hc  *http.Client
	ip  string // 非空时作为 X-Forwarded-For（服务器把回环视为可信代理）
	raw bool   // true 时不自动带 CSRF 头
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, hc: &http.Client{Jar: jar}}
}

func (c *client) url() *url.URL {
	u, _ := url.Parse(c.e.srv.URL)
	return u
}

// cookie 返回罐中名为 name 的 Cookie 值（按 path 选择作用域）。
func (c *client) cookie(name, path string) string {
	u := c.url()
	u.Path = path
	for _, ck := range c.hc.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (c *client) setCookie(name, value, path string) {
	u := c.url()
	c.hc.Jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: path}})
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r *resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("解析响应 %s: %v", r.body, err)
	}
}

func (r *resp) code() string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.body, &e)
	return e.Code
}

// do 发请求。非 GET 时自动从罐中取 isp_csrf 填 X-CSRF-Token（没有时先 GET /api/me 取得）。
func (c *client) do(method, path string, body any, headers ...string) *resp {
	c.e.t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !session.SafeMethod(method) && !c.raw {
		tok := c.cookie(session.CSRFCookie, "/")
		if tok == "" {
			c.do("GET", "/api/me", nil)
			tok = c.cookie(session.CSRFCookie, "/")
		}
		req.Header.Set(session.CSRFHeader, tok)
	}
	if c.ip != "" {
		req.Header.Set("X-Forwarded-For", c.ip)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.hc.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return &resp{status: res.StatusCode, header: res.Header, body: b}
}

func expect(t *testing.T, r *resp, status int, code string) {
	t.Helper()
	if r.status != status || (code != "" && r.code() != code) {
		t.Fatalf("期望 %d %s，得到 %d %s", status, code, r.status, r.body)
	}
}

const pw = "correct-horse-battery"

// registerVerified 注册并验证一个用户，返回已登录的客户端。
func (e *env) registerVerified(email string) *client {
	e.t.Helper()
	c := e.client()
	expect(e.t, c.do("POST", "/api/account/register", map[string]string{"email": email, "password": pw}), 201, "")
	expect(e.t, c.do("POST", "/api/account/verify-email", map[string]string{"email": email, "code": e.codeOf(email)}), 200, "")
	return c
}

// newAdmin 用 CLI 同款函数创建管理员，并完成首次登录与 TOTP 绑定，返回已登录客户端与 TOTP 密钥。
func (e *env) newAdmin(email, role string) (*client, []byte) {
	e.t.Helper()
	if _, err := admin.Create(context.Background(), e.pool, email, "测试管理员", role, "admin-password-123"); err != nil {
		e.t.Fatal(err)
	}
	c := e.client()
	r := c.do("POST", "/admin-api/sessions", map[string]string{"email": email, "password": "admin-password-123"})
	expect(e.t, r, 200, "")
	var ch struct {
		Stage      string `json:"stage"`
		Enrollment struct {
			Secret string `json:"secret"`
		} `json:"enrollment"`
	}
	r.json(e.t, &ch)
	sec, err := admin.DecodeSecret(ch.Enrollment.Secret)
	if err != nil || ch.Stage != "enrollment_required" {
		e.t.Fatalf("%s %v", r.body, err)
	}
	expect(e.t, c.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"code": admin.TOTPCode(sec, e.clock.Now())}), 200, "")
	return c, sec
}

var _ = account.DefaultLimits
