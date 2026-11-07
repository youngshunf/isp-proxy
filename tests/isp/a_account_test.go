//go:build ispe2e

package isp

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// A-23 注册、验证、登录完整；未验证用户不能购买或获取连接；公开接口不泄露邮箱是否注册。
func TestA23_RegisterVerifyLoginNoEnumeration(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{ControlOnly: true})

	// 注册：201 且不建会话。
	u := e.NewClientUser("a23-"+randHex(3)+"@e2e.test", "a23-password-1")
	var reg map[string]any
	u.Must("POST", "/account/register", map[string]string{"email": strings.ToUpper(u.Email), "password": u.Password}, &reg)
	if reg["email"] != u.Email || len(reg) != 1 {
		t.Fatalf("注册响应应只含规范化邮箱：%v", reg)
	}
	u.ExpectStatus(401, "GET", "/me", nil)

	// 未验证时登录可以，但不能购买、不能获取连接（403 unverified）。
	if err := u.Login(); err != nil {
		t.Fatalf("未验证用户登录：%v", err)
	}
	var me Account
	u.Get("/me", &me)
	if me.Verified {
		t.Fatal("未验证用户显示已验证")
	}
	if ae := u.ExpectStatus(403, "GET", "/subscriptions", nil); ae.Code != "unverified" {
		t.Fatalf("未验证用户列订阅：%+v", ae)
	}
	if ae := u.ExpectStatus(403, "POST", "/orders", OrderInput{Kind: "new", PlanID: "pln_x", Months: 1, Quantity: 1}, Idem(NewKey())); ae.Code != "unverified" {
		t.Fatalf("未验证用户下单：%+v", ae)
	}
	if ae := u.ExpectStatus(403, "GET", "/orders", nil); ae.Code != "unverified" {
		t.Fatalf("未验证用户列订单：%+v", ae)
	}

	// 已登录（未验证）只传 code 完成验证。
	code := e.NextCode(u.Email, "邮箱验证码")
	u.Must("POST", "/account/verify-email", map[string]string{"code": code}, &me)
	if !me.Verified {
		t.Fatalf("验证后：%+v", me)
	}
	var subs []Subscription
	u.Get("/subscriptions", &subs)

	// 退出后用密码重新登录。
	u.Must("DELETE", "/sessions/current", nil, nil)
	u.ExpectStatus(401, "GET", "/me", nil)
	if err := u.Login(); err != nil {
		t.Fatal(err)
	}
	u.Get("/me", &me)

	// 不泄露：已注册（已验证）与未注册邮箱的注册、找回、重发、错误登录响应一致。
	anon := e.NewClientUser("", "")
	unknown := "nobody-" + randHex(3) + "@e2e.test"
	probe := func(method, path string, body any) (int, string) {
		res, err := anon.Do(method, path, body, nil)
		if res == nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		msg := ""
		if ae, ok := err.(*APIError); ok {
			msg = ae.Code + "|" + ae.Message
		}
		return res.StatusCode, msg
	}
	type pair struct{ known, unknown any }
	cases := []struct {
		name, path string
		body       pair
	}{
		{"注册", "/account/register", pair{map[string]string{"email": u.Email, "password": "another-pass-1"},
			map[string]string{"email": unknown, "password": "another-pass-1"}}},
		{"找回", "/account/password-reset", pair{map[string]string{"email": u.Email}, map[string]string{"email": unknown}}},
		{"重发", "/account/resend-verification", pair{map[string]string{"email": u.Email}, map[string]string{"email": unknown}}},
		{"错误密码登录", "/sessions", pair{map[string]string{"email": u.Email, "password": "wrong-password"},
			map[string]string{"email": unknown, "password": "wrong-password"}}},
	}
	for _, c := range cases {
		s1, m1 := probe("POST", c.path, c.body.known)
		s2, m2 := probe("POST", c.path, c.body.unknown)
		if s1 != s2 || m1 != m2 {
			t.Errorf("%s 泄露邮箱是否注册：已注册 %d %q，未注册 %d %q", c.name, s1, m1, s2, m2)
		}
	}
	// 已注册邮箱再注册收到的是“已有账号”提醒，不是验证码；未注册邮箱收到验证码。
	notice := e.NextMail(u.Email, "该邮箱已有账号")
	if codeRe.MatchString(notice.Body) {
		t.Fatalf("“已有账号”提醒不应含验证码：%q", notice.Body)
	}
	e.NextCode(unknown, "邮箱验证码")
	// 已注册用户的密码不受“再次注册”影响。
	if err := u.Login(); err != nil {
		t.Fatalf("再次注册后原密码登录失败：%v", err)
	}
	// 目录对访客公开。
	var cat Catalog
	anon.Get("/catalog", &cat)
	if cat.Payment.Payee == "" {
		t.Fatalf("发布带收款说明的规则后目录应带收款说明：%+v", cat.Payment)
	}
}

// A-24 验证/找回材料过期、重放、用途不符或尝试过多均失败；发送失败可重试。
func TestA24_VerificationMaterialMisuse(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{ControlOnly: true})
	verify := func(c *User, email, code string) error {
		_, err := c.Do("POST", "/account/verify-email", map[string]string{"email": email, "code": code}, nil)
		return err
	}
	wrong := func(code string) string {
		if code == "000000" {
			return "000001"
		}
		return "000000"
	}

	section(t, "重放")
	{
		u := e.NewClientUser("a24-replay-"+randHex(3)+"@e2e.test", "a24-password-1")
		u.Register()
		code := e.NextCode(u.Email, "邮箱验证码")
		if err := verify(u, u.Email, code); err != nil {
			t.Fatal(err)
		}
		other := e.NewClientUser(u.Email, u.Password)
		if err := verify(other, u.Email, code); StatusOf(err) != 422 {
			t.Fatalf("重放验证码应 422：%v", err)
		}
		// 已验证账号的找回码只能用一次（同一邮箱 60 秒只发一封：先拨过限流窗口）。
		e.ShiftClock(61 * time.Second)
		other.Must("POST", "/account/password-reset", map[string]string{"email": u.Email}, nil)
		rc := e.NextCode(u.Email, "找回密码")
		other.Must("POST", "/account/password-reset/confirm", map[string]string{"email": u.Email, "code": rc, "newPassword": "a24-password-2"}, nil)
		if _, err := other.Do("POST", "/account/password-reset/confirm", map[string]string{"email": u.Email, "code": rc, "newPassword": "a24-password-3"}, nil); StatusOf(err) != 422 {
			t.Fatalf("重放找回码应 422：%v", err)
		}
		other.Password = "a24-password-2"
		if err := other.Login(); err != nil {
			t.Fatalf("找回后新密码登录：%v", err)
		}
	}

	section(t, "用途不符")
	{
		u := e.NewClientUser("a24-purpose-"+randHex(3)+"@e2e.test", "a24-password-1")
		u.Register()
		vcode := e.NextCode(u.Email, "邮箱验证码")
		// 验证码不能用来重设密码。
		if _, err := u.Do("POST", "/account/password-reset/confirm", map[string]string{"email": u.Email, "code": vcode, "newPassword": "a24-password-9"}, nil); StatusOf(err) != 422 {
			t.Fatalf("注册验证码用于重设密码应 422：%v", err)
		}
		if err := verify(u, u.Email, vcode); err != nil {
			t.Fatalf("验证码在错误用途尝试后仍应可用于原用途：%v", err)
		}
		// 找回码不能用来验证另一个未验证账号的邮箱（用途绑定 + 用户绑定）。
		v := e.NewClientUser("a24-purpose2-"+randHex(3)+"@e2e.test", "a24-password-1")
		v.Register()
		e.NextCode(v.Email, "邮箱验证码")
		e.ShiftClock(61 * time.Second)
		u.Must("POST", "/account/password-reset", map[string]string{"email": u.Email}, nil)
		rc := e.NextCode(u.Email, "找回密码")
		if err := verify(v, v.Email, rc); StatusOf(err) != 422 {
			t.Fatalf("他人的找回码用于验证邮箱应 422：%v", err)
		}
	}

	section(t, "尝试过多")
	{
		u := e.NewClientUser("a24-attempts-"+randHex(3)+"@e2e.test", "a24-password-1")
		u.Register()
		code := e.NextCode(u.Email, "邮箱验证码")
		for i := 0; i < 5; i++ {
			if err := verify(u, u.Email, wrong(code)); StatusOf(err) != 422 {
				t.Fatalf("第 %d 次错误码应 422：%v", i+1, err)
			}
		}
		if err := verify(u, u.Email, code); StatusOf(err) != 422 {
			t.Fatalf("5 次错误后正确码也应失效（422）：%v", err)
		}
	}

	section(t, "发送失败可重试")
	{
		u := e.NewClientUser("a24-retry-"+randHex(3)+"@e2e.test", "a24-password-1")
		if err := os.MkdirAll(e.MailDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(e.MailDir, 0o500); err != nil {
			t.Fatal(err)
		}
		u.Register()
		time.Sleep(2 * time.Second) // 至少一次投递失败
		if ps, _ := e.Mails(u.Email); len(ps) != 0 {
			t.Fatal("邮件目录不可写时不应投递成功")
		}
		if err := os.Chmod(e.MailDir, 0o700); err != nil {
			t.Fatal(err)
		}
		code := e.NextCodeWithin(u.Email, "邮箱验证码", 40*time.Second)
		if err := verify(u, u.Email, code); err != nil {
			t.Fatalf("重试投递的验证码应可用：%v", err)
		}
	}

	section(t, "重发后旧码失效")
	{
		u := e.NewClientUser("a24-resend-"+randHex(3)+"@e2e.test", "a24-password-1")
		u.Register()
		old := e.NextCode(u.Email, "邮箱验证码")
		// 同一邮箱 60 秒只发一封：拨时钟越过限流窗口。
		e.ShiftClock(61 * time.Second)
		u.Must("POST", "/account/resend-verification", map[string]string{"email": u.Email}, nil)
		fresh := e.NextCode(u.Email, "邮箱验证码")
		if old != fresh {
			if err := verify(u, u.Email, old); StatusOf(err) != 422 {
				t.Fatalf("重发后旧码应 422：%v", err)
			}
		}
		if err := verify(u, u.Email, fresh); err != nil {
			t.Fatal(err)
		}
	}

	section(t, "过期")
	{
		u := e.NewClientUser("a24-expire-"+randHex(3)+"@e2e.test", "a24-password-1")
		u.Register()
		code := e.NextCode(u.Email, "邮箱验证码")
		e.ShiftClock(16 * time.Minute) // 15 分钟有效
		if err := verify(u, u.Email, code); StatusOf(err) != 422 {
			t.Fatalf("过期验证码应 422：%v", err)
		}
	}
}

// A-26 普通用户访问管理 API、伪造角色、替换他人 ID，服务端拒绝。
func TestA26_AdminAPIForbiddenForUsers(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{ControlOnly: true})
	u := e.NewUser()

	// 用户会话访问管理端：Cookie 路径与会话种类都不同，一律 401。
	for _, p := range []string{"/admin-api/me", "/admin-api/overview", "/admin-api/subscriptions", "/admin-api/users",
		"/admin-api/users/" + u.ID, "/admin-api/audit-logs"} {
		if res, err := u.Raw("GET", p, nil, nil); StatusOf(err) != 401 {
			t.Errorf("用户访问 %s 应 401：%v %v", p, res.Status, err)
		}
	}
	if _, err := u.Raw("POST", "/admin-api/plans", map[string]any{"code": "x"}, nil); StatusOf(err) != 401 {
		t.Errorf("用户写管理端应 401：%v", err)
	}

	// 把用户会话令牌伪造成管理员 Cookie：库里会话种类不符，仍 401。
	tok := u.cookie("isp_session")
	if tok == "" {
		t.Fatal("没有用户会话 Cookie")
	}
	req, _ := http.NewRequest("GET", e.Base+"/admin-api/me", nil)
	req.AddCookie(&http.Cookie{Name: "isp_admin", Value: tok})
	req.Header.Set("X-Role", "admin")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("伪造管理员 Cookie 应 401，得到 %d", res.StatusCode)
	}

	// 管理员会话不能当用户会话用。
	req, _ = http.NewRequest("GET", e.Base+"/api/me", nil)
	req.AddCookie(&http.Cookie{Name: "isp_session", Value: e.Admin.cookie("isp_admin")})
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("管理员令牌当用户 Cookie 应 401，得到 %d", res.StatusCode)
	}

	// 只读管理员（viewer）写操作 403 forbidden 并审计；读可以。
	vpass := "viewer-pass-" + randHex(4)
	e.run(t, []string{"ISP_ADMIN_PASSWORD=" + vpass}, "admin", "create", "-config", e.CfgPath, "-email", "viewer@e2e.test", "-role", "viewer")
	v := e.newAdmin("viewer@e2e.test", vpass)
	v.Login()
	var users Page[AdminUser]
	v.Get("/users", &users)
	if ae := v.ExpectStatus(403, "POST", "/users/"+u.ID+"/ban", map[string]string{"reason": "viewer 越权"}); ae.Code != "forbidden" {
		t.Fatalf("viewer 写操作：%+v", ae)
	}
	var logs Page[AuditLog]
	e.Admin.Get("/audit-logs?action=admin.write_denied", &logs)
	if len(logs.Items) == 0 {
		t.Fatal("viewer 写操作被拒应记审计 admin.write_denied")
	}
	var au AdminUser
	e.Admin.Get("/users/"+u.ID, &au)
	if au.Status != "active" {
		t.Fatalf("viewer 的封禁不应生效：%+v", au)
	}

	// CSRF：所有非 GET（含登录）缺少 X-CSRF-Token 一律 403 forbidden。
	anon := e.NewClientUser(u.Email, u.Password)
	if ae := anon.ExpectStatus(403, "POST", "/sessions", map[string]string{"email": u.Email, "password": u.Password}, NoCSRF()); ae.Code != "forbidden" {
		t.Fatalf("登录缺 CSRF：%+v", ae)
	}
	if ae := e.Admin.ExpectStatus(403, "POST", "/users/"+u.ID+"/ban", map[string]string{"reason": "x"}, NoCSRF()); ae.Code != "forbidden" {
		t.Fatalf("管理端缺 CSRF：%+v", ae)
	}
}
