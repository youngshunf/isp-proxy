package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/control/account"
	"github.com/go-gost/gost/internal/control/admin"
	"github.com/go-gost/gost/internal/control/session"
)

type adminSessionBody struct {
	Admin struct {
		ID           string `json:"id"`
		Email        string `json:"email"`
		Role         string `json:"role"`
		TOTPEnrolled bool   `json:"totpEnrolled"`
	} `json:"admin"`
	RecoveryCodes []string `json:"recoveryCodes"`
}

// 管理员两步登录：首次绑定、TOTP 防重放、恢复码一次性、第二步之前不能访问管理端。
func TestAdminLoginTOTP(t *testing.T) {
	e := newEnv(t)
	if _, err := admin.Create(context.Background(), e.pool, "Boss@Example.com", "老板", "admin", "admin-password-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Create(context.Background(), e.pool, "boss@example.com", "x", "admin", "admin-password-123"); err == nil {
		t.Fatal("重复邮箱应当失败")
	}
	if _, err := admin.Create(context.Background(), e.pool, "short@example.com", "x", "admin", "short"); err == nil {
		t.Fatal("短密码应当失败")
	}
	c := e.client()
	login := map[string]string{"email": "boss@example.com", "password": "admin-password-123"}
	expect(t, c.do("POST", "/admin-api/sessions", map[string]string{"email": "boss@example.com", "password": "wrong-password"}), 422, "invalid")

	r := c.do("POST", "/admin-api/sessions", login)
	expect(t, r, 200, "")
	var ch struct {
		Stage      string `json:"stage"`
		Enrollment struct {
			Secret     string `json:"secret"`
			OTPAuthURI string `json:"otpauthUri"`
		} `json:"enrollment"`
	}
	r.json(t, &ch)
	if ch.Stage != "enrollment_required" || !strings.HasPrefix(ch.Enrollment.OTPAuthURI, "otpauth://totp/") {
		t.Fatalf("%s", r.body)
	}
	sec, _ := admin.DecodeSecret(ch.Enrollment.Secret)

	// 第二步之前：管理端一律 401。
	expect(t, c.do("GET", "/admin-api/me", nil), 401, "unauthorized")
	expect(t, c.do("GET", "/admin-api/users", nil), 401, "unauthorized")
	// 首次绑定不能用恢复码；错误验证码 422。
	expect(t, c.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"recoveryCode": "aaaaa-bbbbb"}), 422, "invalid")
	bad := admin.TOTPCode(sec, e.clock.Now().Add(-5*time.Minute))
	expect(t, c.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"code": bad}), 422, "invalid")

	now := e.clock.Now()
	code := admin.TOTPCode(sec, now)
	r = c.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"code": code})
	expect(t, r, 200, "")
	var s adminSessionBody
	r.json(t, &s)
	if len(s.RecoveryCodes) != 10 || !s.Admin.TOTPEnrolled || s.Admin.Role != "admin" || s.Admin.Email != "boss@example.com" {
		t.Fatalf("%s", r.body)
	}
	expect(t, c.do("GET", "/admin-api/me", nil), 200, "")
	// 第二步换发了令牌：同一会话不能再走第二步。
	expect(t, c.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"code": code}), 401, "unauthorized")

	// 再次登录：同一时间步的验证码不能重放。
	c2 := e.client()
	r = c2.do("POST", "/admin-api/sessions", login)
	ch.Stage = ""
	r.json(t, &ch)
	if ch.Stage != "totp_required" {
		t.Fatalf("%s", r.body)
	}
	if strings.Contains(string(r.body), "enrollment") {
		t.Fatalf("已绑定不应再返回密钥: %s", r.body)
	}
	expect(t, c2.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"code": code}), 422, "invalid")
	e.clock.Advance(30 * time.Second)
	expect(t, c2.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"code": admin.TOTPCode(sec, e.clock.Now())}), 200, "")

	// 恢复码登录：一次性，大小写与连字符不敏感。
	rc := s.RecoveryCodes[0]
	c3 := e.client()
	expect(t, c3.do("POST", "/admin-api/sessions", login), 200, "")
	expect(t, c3.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"recoveryCode": strings.ToUpper(strings.ReplaceAll(rc, "-", ""))}), 200, "")
	c4 := e.client()
	expect(t, c4.do("POST", "/admin-api/sessions", login), 200, "")
	expect(t, c4.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"recoveryCode": rc}), 422, "invalid")

	// 重新生成恢复码：要求 TOTP，旧码全部作废。
	e.clock.Advance(30 * time.Second)
	r = c.do("POST", "/admin-api/me/recovery-codes", map[string]string{"code": admin.TOTPCode(sec, e.clock.Now())})
	expect(t, r, 200, "")
	var codes struct {
		Codes []string `json:"codes"`
	}
	r.json(t, &codes)
	if len(codes.Codes) != 10 {
		t.Fatal(codes)
	}
	expect(t, c4.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"recoveryCode": s.RecoveryCodes[1]}), 422, "invalid")
	expect(t, c4.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"recoveryCode": codes.Codes[0]}), 200, "")

	// 退出。
	expect(t, c.do("DELETE", "/admin-api/sessions/current", nil), 204, "")
	expect(t, c.do("GET", "/admin-api/me", nil), 401, "unauthorized")

	// 审计：失败与成功都有记录，details 不含验证码。
	for _, a := range []struct{ action, outcome string }{
		{"admin.create", "ok"}, {"admin.login_failed", "denied"}, {"admin.mfa_failed", "denied"}, {"admin.totp_enrolled", "ok"},
		{"admin.login", "ok"}, {"admin.recovery_codes_regenerated", "ok"}, {"admin.logout", "ok"}, {"admin.login_password", "ok"},
	} {
		if e.auditCount(a.action, a.outcome) == 0 {
			t.Errorf("缺少审计 %s/%s", a.action, a.outcome)
		}
	}
	var leaked int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE details::text LIKE '%' || $1 || '%'`, code).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("审计泄露了验证码")
	}
	// TOTP 密钥加密存储，不是明文 Base32。
	var enc []byte
	_ = e.pool.QueryRow(context.Background(), `SELECT totp_secret_enc FROM admins WHERE email = 'boss@example.com'`).Scan(&enc)
	if strings.Contains(string(enc), ch.Enrollment.Secret) || len(enc) < 20 {
		t.Fatal("TOTP 密钥未加密")
	}
}

// 第二步失败次数限流。
func TestAdminMFARateLimit(t *testing.T) {
	e := newEnv(t)
	_, sec := e.newAdmin("m@example.com", "admin")
	e.clock.Advance(30 * time.Second)
	c := e.client()
	expect(t, c.do("POST", "/admin-api/sessions", map[string]string{"email": "m@example.com", "password": "admin-password-123"}), 200, "")
	for i := 0; i < 5; i++ {
		expect(t, c.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"code": "000000"}), 422, "invalid")
	}
	expect(t, c.do("POST", "/admin-api/sessions/current/mfa", map[string]string{"code": admin.TOTPCode(sec, e.clock.Now())}), 429, "rate_limited")
}

// A-26：普通用户访问管理 API、伪造 Cookie 或角色，服务端拒绝。
func TestUserCannotUseAdminAPI(t *testing.T) {
	e := newEnv(t)
	u := e.registerVerified("user@example.com")
	userTok := u.cookie(session.UserCookie, "/api")

	// 用户会话 Cookie 只作用于 /api，访问管理端 401。
	expect(t, u.do("GET", "/admin-api/users", nil), 401, "unauthorized")
	expect(t, u.do("GET", "/admin-api/me", nil), 401, "unauthorized")
	// 把用户令牌伪装成 isp_admin。
	u.setCookie(session.AdminCookie, userTok, "/admin-api")
	expect(t, u.do("GET", "/admin-api/users", nil), 401, "unauthorized")
	expect(t, u.do("POST", "/admin-api/users", map[string]string{"email": "x@example.com"}), 401, "unauthorized")
	// 伪造角色头无效。
	expect(t, u.do("GET", "/admin-api/audit-logs", nil, "X-Role", "admin", "X-Admin-Id", "adm_aaaaaaaaaaaaaaaaaaaa"), 401, "unauthorized")
	// 随机令牌。
	f := e.client()
	f.setCookie(session.AdminCookie, "forged-token-forged-token-forged-token", "/admin-api")
	expect(t, f.do("GET", "/admin-api/me", nil), 401, "unauthorized")

	// 管理员会话也不能当用户会话用。
	a, _ := e.newAdmin("ops@example.com", "admin")
	adminTok := a.cookie(session.AdminCookie, "/admin-api")
	x := e.client()
	x.setCookie(session.UserCookie, adminTok, "/api")
	expect(t, x.do("GET", "/api/me", nil), 401, "unauthorized")

	// 只读管理员不能写，且被拒记审计；本人操作（退出）可以。
	v, _ := e.newAdmin("viewer@example.com", "viewer")
	expect(t, v.do("GET", "/admin-api/users", nil), 200, "")
	expect(t, v.do("POST", "/admin-api/users", map[string]string{"email": "y@example.com"}), 403, "forbidden")
	if e.auditCount("admin.write_denied", "denied") != 1 {
		t.Fatal("viewer 被拒应记审计")
	}
	expect(t, v.do("DELETE", "/admin-api/sessions/current", nil), 204, "")

	// 停用的管理员会话立即失效。
	if _, err := e.pool.Exec(context.Background(), `UPDATE admins SET status = 'disabled' WHERE email = 'ops@example.com'`); err != nil {
		t.Fatal(err)
	}
	expect(t, a.do("GET", "/admin-api/me", nil), 401, "unauthorized")
}

// 管理端用户管理：代建、设置密码、封禁（撤销会话、只读）、解封、发重设邮件、列表与详情、审计查询。
func TestAdminUsers(t *testing.T) {
	e := newEnv(t)
	a, _ := e.newAdmin("ops@example.com", "admin")

	// 代建：只建邮箱；邮箱已存在 409。
	r := a.do("POST", "/admin-api/users", map[string]string{"email": "Cust@Example.com"})
	expect(t, r, 201, "")
	var u admin.User
	r.json(t, &u)
	if u.Email != "cust@example.com" || u.PasswordSet || u.Verified || u.CreatedByAdmin == nil || u.CreatedByAdmin.Email != "ops@example.com" {
		t.Fatalf("%s", r.body)
	}
	expect(t, a.do("POST", "/admin-api/users", map[string]string{"email": "cust@example.com"}), 409, "conflict")

	// 用户收到“设置登录密码”的验证码，用 confirmPasswordReset 设置，同时完成验证。
	m := e.lastMail("cust@example.com")
	if m == nil || !strings.Contains(m.Subject, "设置登录密码") {
		t.Fatalf("%+v", m)
	}
	cust := e.client()
	expect(t, cust.do("POST", "/api/sessions", map[string]string{"email": "cust@example.com", "password": "anything-here"}), 422, "invalid")
	expect(t, cust.do("POST", "/api/account/password-reset/confirm",
		map[string]string{"email": "cust@example.com", "code": e.codeOf("cust@example.com"), "newPassword": "customer-pass-1"}), 204, "")
	r = cust.do("POST", "/api/sessions", map[string]string{"email": "cust@example.com", "password": "customer-pass-1"})
	expect(t, r, 200, "")
	var acct account.Account
	r.json(t, &acct)
	if !acct.Verified {
		t.Fatal("设置密码后应视为已验证")
	}

	// 列表与筛选、分页。
	e.registerVerified("other@example.com")
	expect(t, e.client().do("POST", "/api/account/register", map[string]string{"email": "pending@example.com", "password": pw}), 201, "")
	var page struct {
		Items      []admin.User `json:"items"`
		NextCursor *string      `json:"nextCursor"`
	}
	r = a.do("GET", "/admin-api/users?limit=2", nil)
	expect(t, r, 200, "")
	r.json(t, &page)
	if len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("%s", r.body)
	}
	r = a.do("GET", "/admin-api/users?limit=2&cursor="+*page.NextCursor, nil)
	r.json(t, &page)
	if len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("%s", r.body)
	}
	r = a.do("GET", "/admin-api/users?status=unverified", nil)
	r.json(t, &page)
	if len(page.Items) != 1 || page.Items[0].Email != "pending@example.com" {
		t.Fatalf("%s", r.body)
	}
	r = a.do("GET", "/admin-api/users?q=CUST", nil)
	r.json(t, &page)
	if len(page.Items) != 1 || page.Items[0].ID != u.ID {
		t.Fatalf("%s", r.body)
	}
	expect(t, a.do("GET", "/admin-api/users?limit=500", nil), 422, "invalid")
	expect(t, a.do("GET", "/admin-api/users?cursor=@@", nil), 422, "invalid")

	// 封禁：撤销会话；用户可再登录查看但所有写操作 412（退出除外）。
	expect(t, a.do("POST", "/admin-api/users/"+u.ID+"/ban", map[string]string{"reason": ""}), 422, "invalid")
	r = a.do("POST", "/admin-api/users/"+u.ID+"/ban", map[string]string{"reason": "滥用投诉"})
	expect(t, r, 200, "")
	r.json(t, &u)
	if u.Status != "banned" || u.BannedReason == nil || *u.BannedReason != "滥用投诉" {
		t.Fatalf("%s", r.body)
	}
	expect(t, cust.do("GET", "/api/me", nil), 401, "unauthorized")
	r = cust.do("POST", "/api/sessions", map[string]string{"email": "cust@example.com", "password": "customer-pass-1"})
	expect(t, r, 200, "")
	r.json(t, &acct)
	if acct.Status != "banned" {
		t.Fatalf("%s", r.body)
	}
	expect(t, cust.do("POST", "/api/account/change-password", map[string]string{"current": "customer-pass-1", "next": "customer-pass-2"}), 412, "precondition")
	expect(t, cust.do("GET", "/api/me", nil), 200, "")

	// 详情。
	r = a.do("GET", "/admin-api/users/"+u.ID, nil)
	expect(t, r, 200, "")
	var d struct {
		admin.User
		Subscriptions  []any `json:"subscriptions"`
		RecentOrders   []any `json:"recentOrders"`
		ActiveSessions int   `json:"activeSessions"`
	}
	r.json(t, &d)
	if d.ActiveSessions != 1 || d.Subscriptions == nil || d.RecentOrders == nil || d.LastLoginAt == nil || strings.Contains(string(r.body), "argon2") {
		t.Fatalf("%s", r.body)
	}
	expect(t, a.do("GET", "/admin-api/users/usr_aaaaaaaaaaaaaaaaaaaa", nil), 404, "not_found")

	// 解封后可写。
	expect(t, a.do("POST", "/admin-api/users/"+u.ID+"/unban", nil), 200, "")
	expect(t, cust.do("POST", "/api/account/change-password", map[string]string{"current": "customer-pass-1", "next": "customer-pass-2"}), 204, "")

	// 发重设邮件：受发信限流；用户不存在 404。
	e.clock.Advance(2 * time.Minute)
	expect(t, a.do("POST", "/admin-api/users/"+u.ID+"/password-reset-email", nil), 204, "")
	expect(t, a.do("POST", "/admin-api/users/"+u.ID+"/password-reset-email", nil), 429, "rate_limited")
	expect(t, a.do("POST", "/admin-api/users/usr_aaaaaaaaaaaaaaaaaaaa/password-reset-email", nil), 404, "not_found")
	if m := e.lastMail("cust@example.com"); m == nil || !strings.Contains(m.Subject, "找回密码") {
		t.Fatalf("%+v", m)
	}

	// 审计查询。
	var logs struct {
		Items []struct {
			Action   string `json:"action"`
			TargetID string `json:"targetId"`
			Actor    struct {
				Kind  string `json:"kind"`
				Email string `json:"email"`
			} `json:"actor"`
			IP *string `json:"ip"`
		} `json:"items"`
	}
	r = a.do("GET", "/admin-api/audit-logs?action=user.&targetId="+u.ID, nil)
	expect(t, r, 200, "")
	r.json(t, &logs)
	var actions []string
	for _, l := range logs.Items {
		actions = append(actions, l.Action)
		if l.Actor.Kind != "admin" || l.Actor.Email != "ops@example.com" || l.IP == nil {
			t.Fatalf("%s", r.body)
		}
	}
	if strings.Join(actions, ",") != "user.password_reset_email,user.unban,user.ban,user.create" {
		t.Fatalf("%v", actions)
	}
	expect(t, a.do("GET", "/admin-api/audit-logs?from=yesterday", nil), 422, "invalid")
	expect(t, a.do("GET", "/admin-api/audit-logs?actorKind=robot", nil), 422, "invalid")
}
