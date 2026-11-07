package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/control/account"
	"github.com/go-gost/gost/internal/control/api"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/mail"
	"github.com/go-gost/gost/internal/control/ratelimit"
	"github.com/go-gost/gost/internal/control/session"
)

// A-23：注册、验证、登录完整；公开接口不泄露邮箱是否注册。
func TestRegisterVerifyLogin(t *testing.T) {
	e := newEnv(t)
	c := e.client()

	// 未登录 /api/me 返回 401，且下发 isp_csrf。
	r := c.do("GET", "/api/me", nil)
	expect(t, r, 401, "unauthorized")
	if c.cookie(session.CSRFCookie, "/") == "" {
		t.Fatal("缺少 isp_csrf")
	}

	r1 := c.do("POST", "/api/account/register", map[string]string{"email": "  New@Example.com ", "password": pw})
	expect(t, r1, 201, "")
	if string(r1.body) != `{"email":"new@example.com"}`+"\n" {
		t.Fatalf("%s", r1.body)
	}
	// 注册不建会话。
	expect(t, c.do("GET", "/api/me", nil), 401, "unauthorized")
	code1 := e.codeOf("new@example.com")
	if len(code1) != 6 {
		t.Fatal("没有收到验证码")
	}

	// 同一邮箱 60 秒内再次注册被限流（对已注册与未注册邮箱一样）。
	expect(t, c.do("POST", "/api/account/register", map[string]string{"email": "new@example.com", "password": "another-password"}), 429, "rate_limited")
	e.clock.Advance(61 * time.Second)

	// 已注册（未验证）再次注册：响应完全相同，旧验证码作废，密码不被改掉。
	r2 := c.do("POST", "/api/account/register", map[string]string{"email": "new@example.com", "password": "another-password"})
	expect(t, r2, 201, "")
	if !bytes.Equal(r1.body, r2.body) || r1.header.Get("Set-Cookie") != "" && strings.Contains(r2.header.Get("Set-Cookie"), "isp_session") {
		t.Fatal("重复注册的响应不同")
	}
	code2 := e.codeOf("new@example.com")
	if code2 == code1 {
		code2 = e.codeOf("new@example.com")
	}
	expect(t, c.do("POST", "/api/account/verify-email", map[string]string{"email": "new@example.com", "code": code1}), 422, "invalid")

	// 验证成功即登录。
	r = c.do("POST", "/api/account/verify-email", map[string]string{"email": "new@example.com", "code": code2})
	expect(t, r, 200, "")
	var acct account.Account
	r.json(t, &acct)
	if !acct.Verified || acct.Email != "new@example.com" || acct.Status != "active" {
		t.Fatalf("%+v", acct)
	}
	if c.cookie(session.UserCookie, "/api") == "" {
		t.Fatal("验证成功应建立会话")
	}
	expect(t, c.do("GET", "/api/me", nil), 200, "")

	// 已验证邮箱再注册：同样 201，收到“已有账号”提醒而不是验证码。
	e.clock.Advance(61 * time.Second)
	r3 := e.client().do("POST", "/api/account/register", map[string]string{"email": "new@example.com", "password": "third-password!"})
	if r3.status != 201 || !bytes.Equal(r3.body, r1.body) {
		t.Fatalf("%d %s", r3.status, r3.body)
	}
	if m := e.lastMail("new@example.com"); m == nil || !strings.Contains(m.Subject, "该邮箱已有账号") {
		t.Fatalf("%+v", m)
	}

	// 登录：原密码可用；后两次注册用的密码都不生效。
	c2 := e.client()
	expect(t, c2.do("POST", "/api/sessions", map[string]string{"email": "new@example.com", "password": "another-password"}), 422, "invalid")
	expect(t, c2.do("POST", "/api/sessions", map[string]string{"email": "new@example.com", "password": "third-password!"}), 422, "invalid")
	r = c2.do("POST", "/api/sessions", map[string]string{"email": "NEW@example.com", "password": pw})
	expect(t, r, 200, "")

	// 错误密码与不存在的邮箱给出相同的错误。
	ra := e.client().do("POST", "/api/sessions", map[string]string{"email": "new@example.com", "password": "wrong-password"})
	rb := e.client().do("POST", "/api/sessions", map[string]string{"email": "nobody@example.com", "password": "wrong-password"})
	if ra.status != 422 || rb.status != 422 || !bytes.Equal(ra.body, rb.body) {
		t.Fatalf("%s / %s", ra.body, rb.body)
	}

	// 退出后会话失效。
	expect(t, c2.do("DELETE", "/api/sessions/current", nil), 204, "")
	expect(t, c2.do("GET", "/api/me", nil), 401, "unauthorized")
}

// A-23：公开接口对已注册与未注册邮箱给出相同结果。
func TestPublicEndpointsDoNotLeak(t *testing.T) {
	e := newEnv(t)
	e.registerVerified("known@example.com")
	e.clock.Advance(2 * time.Minute)
	for _, path := range []string{"/api/account/resend-verification", "/api/account/password-reset"} {
		ra := e.client().do("POST", path, map[string]string{"email": "known@example.com"})
		rb := e.client().do("POST", path, map[string]string{"email": "unknown@example.com"})
		if ra.status != 204 || rb.status != 204 || !bytes.Equal(ra.body, rb.body) {
			t.Fatalf("%s: %d %s / %d %s", path, ra.status, ra.body, rb.status, rb.body)
		}
		// 第二次都被同样限流。
		ra = e.client().do("POST", path, map[string]string{"email": "known@example.com"})
		rb = e.client().do("POST", path, map[string]string{"email": "unknown@example.com"})
		if ra.status != 429 || rb.status != 429 || !bytes.Equal(ra.body, rb.body) || ra.header.Get("Retry-After") == "" {
			t.Fatalf("%s 限流: %d %s / %d %s", path, ra.status, ra.body, rb.status, rb.body)
		}
		e.clock.Advance(61 * time.Second)
	}
	if n := e.mailCount("unknown@example.com"); n != 0 {
		t.Fatalf("未注册邮箱不应收到邮件: %d", n)
	}
	// 格式不对的邮箱：重发与找回也一律 204。
	expect(t, e.client().do("POST", "/api/account/password-reset", map[string]string{"email": "not-an-email"}), 204, "")
	expect(t, e.client().do("POST", "/api/account/resend-verification", ""), 204, "")
}

// A-23：未验证用户不能购买或获取连接（VerifiedUser 级别）；路由器按 Route.Auth 执行。
func TestUnverifiedCannotBuyOrReadConnections(t *testing.T) {
	e := newEnv(t)
	called := 0
	ok := func(w http.ResponseWriter, r *http.Request) {
		called++
		p, _ := httpx.UserFrom(r.Context())
		httpx.WriteJSON(w, 200, map[string]string{"user": p.UserID})
	}
	// 用与 ctl-core 相同的级别挂两个代表性路由（下单、读取连接），替换真实处理器以便计数。
	var routes []httpx.Route
	for _, rt := range e.platform.Routes {
		if k := rt.Method + " " + rt.Pattern; k != "POST /api/orders" && k != "GET /api/subscriptions/{subscriptionId}" {
			routes = append(routes, rt)
		}
	}
	routes = append(routes,
		httpx.Route{Method: "POST", Pattern: "/api/orders", Auth: httpx.VerifiedUser, Handler: ok},
		httpx.Route{Method: "GET", Pattern: "/api/subscriptions/{subscriptionId}", Auth: httpx.VerifiedUser, Handler: ok})
	h, err := api.NewRouter(api.Options{Users: e.platform.Account, Admins: e.platform.Admin}, routes)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler = h

	c := e.client()
	expect(t, c.do("POST", "/api/account/register", map[string]string{"email": "u@example.com", "password": pw}), 201, "")
	expect(t, c.do("POST", "/api/sessions", map[string]string{"email": "u@example.com", "password": pw}), 200, "")
	r := c.do("GET", "/api/me", nil)
	var acct account.Account
	r.json(t, &acct)
	if acct.Verified {
		t.Fatal("应当未验证")
	}
	expect(t, c.do("POST", "/api/orders", map[string]any{}), 403, "unverified")
	expect(t, c.do("GET", "/api/subscriptions/sub_aaaaaaaaaaaaaaaaaaaa", nil), 403, "unverified")
	expect(t, e.client().do("GET", "/api/subscriptions/sub_aaaaaaaaaaaaaaaaaaaa", nil), 401, "unauthorized")
	if called != 0 {
		t.Fatal("处理器不应被调用")
	}
	// 已登录未验证时只传 code 完成验证。
	expect(t, c.do("POST", "/api/account/verify-email", map[string]string{"code": e.codeOf("u@example.com")}), 200, "")
	expect(t, c.do("POST", "/api/orders", map[string]any{}), 200, "")
	expect(t, c.do("GET", "/api/subscriptions/sub_aaaaaaaaaaaaaaaaaaaa", nil), 200, "")
}

// A-24：验证与找回材料过期、重放、用途不符、尝试过多均失败；发送失败可重试。
func TestTokenRules(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	expect(t, c.do("POST", "/api/account/register", map[string]string{"email": "t@example.com", "password": pw}), 201, "")
	verify := func(code string) *resp {
		return e.client().do("POST", "/api/account/verify-email", map[string]string{"email": "t@example.com", "code": code})
	}

	// 过期：15 分钟后失效。
	code := e.codeOf("t@example.com")
	e.clock.Advance(15*time.Minute + time.Second)
	expect(t, verify(code), 422, "invalid")

	// 尝试过多：5 次错误后，正确的码也失效。
	expect(t, e.client().do("POST", "/api/account/resend-verification", map[string]string{"email": "t@example.com"}), 204, "")
	code = e.codeOf("t@example.com")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < 5; i++ {
		expect(t, verify(wrong), 422, "invalid")
	}
	expect(t, verify(code), 422, "invalid")

	// 用途不符：找回密码的验证码不能用于验证邮箱，反之亦然。
	e.clock.Advance(61 * time.Second)
	expect(t, e.client().do("POST", "/api/account/password-reset", map[string]string{"email": "t@example.com"}), 204, "")
	resetCode := e.codeOf("t@example.com")
	expect(t, verify(resetCode), 422, "invalid")
	e.clock.Advance(61 * time.Second)
	expect(t, e.client().do("POST", "/api/account/resend-verification", map[string]string{"email": "t@example.com"}), 204, "")
	verifyCode := e.codeOf("t@example.com")
	expect(t, e.client().do("POST", "/api/account/password-reset/confirm",
		map[string]string{"email": "t@example.com", "code": verifyCode, "newPassword": "brand-new-password"}), 422, "invalid")

	// 一次性：成功后同一验证码不能再用（重放）。
	expect(t, verify(verifyCode), 200, "")
	expect(t, verify(verifyCode), 422, "invalid")
	expect(t, e.client().do("POST", "/api/account/password-reset/confirm",
		map[string]string{"email": "t@example.com", "code": resetCode, "newPassword": "brand-new-password"}), 204, "")
	expect(t, e.client().do("POST", "/api/account/password-reset/confirm",
		map[string]string{"email": "t@example.com", "code": resetCode, "newPassword": "other-new-password"}), 422, "invalid")

	// 发送失败可重试：投递失败时邮件留在发件箱，退避后再次投递成功，验证码可用。
	e.clock.Advance(61 * time.Second)
	e.sender.Fail = func(*mail.Rendered) error { return errors.New("SMTP 暂时不可用") }
	expect(t, e.client().do("POST", "/api/account/password-reset", map[string]string{"email": "t@example.com"}), 204, "")
	res, err := e.worker.DeliverDue(context.Background())
	if err != nil || res.Retried != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	e.sender.Fail = nil
	e.clock.Advance(11 * time.Second)
	code = e.codeOf("t@example.com")
	expect(t, e.client().do("POST", "/api/account/password-reset/confirm",
		map[string]string{"email": "t@example.com", "code": code, "newPassword": "third-new-password"}), 204, "")
	expect(t, e.client().do("POST", "/api/sessions", map[string]string{"email": "t@example.com", "password": "third-new-password"}), 200, "")
}

// A-25：修改登录密码后其他会话失效，当前会话换发新令牌；找回密码撤销全部会话。
func TestChangePasswordRevokesSessions(t *testing.T) {
	e := newEnv(t)
	a := e.registerVerified("p@example.com")
	b := e.client()
	expect(t, b.do("POST", "/api/sessions", map[string]string{"email": "p@example.com", "password": pw}), 200, "")
	oldA := a.cookie(session.UserCookie, "/api")

	expect(t, a.do("POST", "/api/account/change-password", map[string]string{"current": "wrong-password", "next": "new-password-123"}), 422, "invalid")
	expect(t, a.do("POST", "/api/account/change-password", map[string]string{"current": pw, "next": "short"}), 422, "invalid")
	expect(t, a.do("POST", "/api/account/change-password", map[string]string{"current": pw, "next": "new-password-123"}), 204, "")

	expect(t, a.do("GET", "/api/me", nil), 200, "")
	if a.cookie(session.UserCookie, "/api") == oldA {
		t.Fatal("当前会话应换发新令牌")
	}
	expect(t, b.do("GET", "/api/me", nil), 401, "unauthorized")
	stale := e.client()
	stale.setCookie(session.UserCookie, oldA, "/api")
	expect(t, stale.do("GET", "/api/me", nil), 401, "unauthorized")

	expect(t, e.client().do("POST", "/api/sessions", map[string]string{"email": "p@example.com", "password": pw}), 422, "invalid")
	expect(t, e.client().do("POST", "/api/sessions", map[string]string{"email": "p@example.com", "password": "new-password-123"}), 200, "")

	// 找回密码：撤销全部会话（包括发起者自己的）。
	e.clock.Advance(2 * time.Minute)
	expect(t, e.client().do("POST", "/api/account/password-reset", map[string]string{"email": "p@example.com"}), 204, "")
	expect(t, e.client().do("POST", "/api/account/password-reset/confirm",
		map[string]string{"email": "p@example.com", "code": e.codeOf("p@example.com"), "newPassword": "reset-password-123"}), 204, "")
	expect(t, a.do("GET", "/api/me", nil), 401, "unauthorized")
	var live int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE kind = 'user' AND revoked_at IS NULL`).Scan(&live)
	if live != 0 {
		t.Fatalf("仍有 %d 个有效会话", live)
	}
	var reasons []string
	rows, _ := e.pool.Query(context.Background(), `SELECT DISTINCT revoke_reason FROM sessions ORDER BY 1`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		reasons = append(reasons, s)
	}
	if strings.Join(reasons, ",") != "password_changed,password_reset" {
		t.Fatalf("撤销原因: %v", reasons)
	}
}

// CSRF 双提交：所有非 GET 都要 X-CSRF-Token 与 isp_csrf 相同。
func TestCSRF(t *testing.T) {
	e := newEnv(t)
	e.registerVerified("c@example.com")
	c := e.client()
	c.raw = true
	login := map[string]string{"email": "c@example.com", "password": pw}
	// 没有 Cookie 也没有头。
	expect(t, c.do("POST", "/api/sessions", login), 403, "forbidden")
	// 上一个响应已下发 isp_csrf；只有 Cookie 没有头仍然拒绝。
	tok := c.cookie(session.CSRFCookie, "/")
	if len(tok) < 16 {
		t.Fatal("响应应下发 isp_csrf")
	}
	expect(t, c.do("POST", "/api/sessions", login), 403, "forbidden")
	expect(t, c.do("POST", "/api/sessions", login, session.CSRFHeader, tok+"x"), 403, "forbidden")
	// 只有头没有 Cookie（跨站伪造的典型情形）。
	other := e.client()
	other.raw = true
	expect(t, other.do("POST", "/api/sessions", login, session.CSRFHeader, tok), 403, "forbidden")
	// 正确的双提交。
	expect(t, c.do("POST", "/api/sessions", login, session.CSRFHeader, tok), 200, "")
	// 登录成功换发 isp_csrf，旧值失效。
	if c.cookie(session.CSRFCookie, "/") == tok {
		t.Fatal("登录后应换发 isp_csrf")
	}
	expect(t, c.do("DELETE", "/api/sessions/current", nil, session.CSRFHeader, tok), 403, "forbidden")
	expect(t, c.do("DELETE", "/api/sessions/current", nil, session.CSRFHeader, c.cookie(session.CSRFCookie, "/")), 204, "")
	// 管理端同样校验。
	expect(t, other.do("POST", "/admin-api/sessions", login), 403, "forbidden")
}

// 登录、发信限流（D-15）。
func TestLoginRateLimits(t *testing.T) {
	e := newEnv(t, func(d *api.Deps) {
		l := account.DefaultLimits
		l.LoginPerEmail = ratelimit.Rule{Scope: "login_email", Limit: 3, Window: 15 * time.Minute}
		l.LoginPerIP = ratelimit.Rule{Scope: "login_ip", Limit: 6, Window: 15 * time.Minute}
		d.AccountLimits = &l
	})
	e.registerVerified("r@example.com")
	e.registerVerified("s@example.com")
	c := e.client()
	c.ip = "198.51.100.10"
	for i := 0; i < 3; i++ {
		expect(t, c.do("POST", "/api/sessions", map[string]string{"email": "r@example.com", "password": "bad-password"}), 422, "invalid")
	}
	// 该邮箱失败次数到上限：正确密码也被限流（带 Retry-After），其他邮箱不受影响。
	r := c.do("POST", "/api/sessions", map[string]string{"email": "r@example.com", "password": pw})
	expect(t, r, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Fatal("缺少 Retry-After")
	}
	expect(t, c.do("POST", "/api/sessions", map[string]string{"email": "s@example.com", "password": pw}), 200, "")
	// 同一来源 IP 的尝试总数到上限后，任何邮箱都被限流；换一个来源 IP 不受影响。
	// （被限流的那次请求整体回滚，不计入次数：3 次失败 + 3 次成功 = 6。）
	expect(t, c.do("POST", "/api/sessions", map[string]string{"email": "s@example.com", "password": pw}), 200, "")
	expect(t, c.do("POST", "/api/sessions", map[string]string{"email": "s@example.com", "password": pw}), 200, "")
	expect(t, c.do("POST", "/api/sessions", map[string]string{"email": "s@example.com", "password": pw}), 429, "rate_limited")
	c2 := e.client()
	c2.ip = "198.51.100.11"
	expect(t, c2.do("POST", "/api/sessions", map[string]string{"email": "s@example.com", "password": pw}), 200, "")
	// 窗口过后恢复。
	e.clock.Advance(16 * time.Minute)
	expect(t, c.do("POST", "/api/sessions", map[string]string{"email": "r@example.com", "password": pw}), 200, "")
}

func TestStrictJSON(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	expect(t, c.do("POST", "/api/account/register", map[string]any{"email": "j@example.com", "password": pw, "role": "admin"}), 422, "invalid")
	expect(t, c.do("POST", "/api/account/register", `{"email":"j@example.com"`), 422, "invalid")
	expect(t, c.do("POST", "/api/account/register", map[string]any{"email": "bad", "password": pw}), 422, "invalid")
	expect(t, c.do("GET", "/api/no-such-route", nil), 404, "not_found")
}
