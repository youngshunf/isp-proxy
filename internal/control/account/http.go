package account

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/mail"
	"github.com/go-gost/gost/internal/control/ratelimit"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/session"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
)

// Routes 返回用户端账号与会话路由（api/openapi.yaml tag account）。
func (s *Service) Routes() []httpx.Route {
	return []httpx.Route{
		{Method: "GET", Pattern: "/api/me", Auth: httpx.User, Handler: s.handleMe},
		{Method: "POST", Pattern: "/api/sessions", Auth: httpx.Public, Handler: s.handleLogin},
		{Method: "DELETE", Pattern: "/api/sessions/current", Auth: httpx.User, Handler: s.handleLogout},
		{Method: "POST", Pattern: "/api/account/register", Auth: httpx.Public, Handler: s.handleRegister},
		{Method: "POST", Pattern: "/api/account/verify-email", Auth: httpx.Public, Handler: s.handleVerifyEmail},
		{Method: "POST", Pattern: "/api/account/resend-verification", Auth: httpx.Public, Handler: s.handleResend},
		{Method: "POST", Pattern: "/api/account/password-reset", Auth: httpx.Public, Handler: s.handleResetRequest},
		{Method: "POST", Pattern: "/api/account/password-reset/confirm", Auth: httpx.Public, Handler: s.handleResetConfirm},
		{Method: "POST", Pattern: "/api/account/change-password", Auth: httpx.User, Handler: s.handleChangePassword},
	}
}

var codePattern = regexp.MustCompile(`^[0-9]{6}$`)

// errBadLogin 不区分邮箱与密码哪一项错（R-15.3）。
var errBadLogin = httpx.Invalid("邮箱或密码不正确")

// tx 在事务中执行 fn。fn 可以通过 *biz 返回“要提交的业务错误”（例如验证码错误需要记下尝试次数）。
func (s *Service) tx(ctx context.Context, fn func(tx pgx.Tx, biz *error) error) error {
	var biz error
	if err := store.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		biz = nil
		return fn(tx, &biz)
	}); err != nil {
		return err
	}
	return biz
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := httpx.UserFrom(r.Context())
	u, err := userByID(r.Context(), s.pool, p.UserID, false)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if u == nil {
		httpx.WriteError(w, r, httpx.Unauthorized())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u.account())
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	email, err := NormalizeEmail(in.Email)
	if err != nil || in.Password == "" || len(in.Password) > 1024 {
		httpx.WriteError(w, r, errBadLogin)
		return
	}
	ctx := r.Context()
	ip := httpx.ClientIP(r)
	var acct *Account
	var token string
	err = s.tx(ctx, func(tx pgx.Tx, biz *error) error {
		if err := s.take(ctx, tx, ip, s.cfg.Limits.LoginPerIP, "登录尝试太频繁，请稍后再试"); err != nil {
			return err
		}
		now := s.now()
		wait, err := ratelimit.Check(ctx, tx, now, email, s.cfg.Limits.LoginPerEmail)
		if err != nil {
			return err
		}
		if wait > 0 {
			return httpx.RateLimited("该账号登录失败次数过多，请稍后再试或找回密码", wait)
		}
		u, err := userByEmail(ctx, tx, email, false)
		if err != nil {
			return err
		}
		if u == nil || u.PasswordHash == nil {
			secret.DummyVerify(in.Password)
		}
		if u == nil || u.PasswordHash == nil || !secret.VerifyPassword(*u.PasswordHash, in.Password) {
			*biz = errBadLogin
			return ratelimit.Hit(ctx, tx, now, email, s.cfg.Limits.LoginPerEmail)
		}
		if err := ratelimit.Reset(ctx, tx, s.cfg.Limits.LoginPerEmail.Scope, email); err != nil {
			return err
		}
		token, _, err = s.createSession(ctx, tx, u.ID, ip, r.UserAgent())
		acct = u.account()
		return err
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.cfg.Cookies.SetUser(w, token, s.now().Add(s.cfg.SessionTTL))
	s.cfg.Cookies.RotateCSRF(w)
	httpx.WriteJSON(w, http.StatusOK, acct)
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	sid := sessionFrom(r.Context())
	if _, err := s.pool.Exec(r.Context(), `UPDATE sessions SET revoked_at = $2, revoke_reason = 'logout'
		WHERE id = $1 AND revoked_at IS NULL`, sid, s.now()); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.cfg.Cookies.ClearUser(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	email, err := NormalizeEmail(in.Email)
	if err == nil {
		err = CheckPassword(in.Password)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	// 无论邮箱是否已注册都先算哈希，响应时间不可区分。
	hash := secret.HashPassword(in.Password)
	err = s.tx(ctx, func(tx pgx.Tx, _ *error) error {
		if err := s.take(ctx, tx, httpx.ClientIP(r), s.cfg.Limits.RegisterPerIP, "注册太频繁，请稍后再试"); err != nil {
			return err
		}
		if err := s.TakeMailQuota(ctx, tx, email); err != nil {
			return err
		}
		u, err := userByEmail(ctx, tx, email, true)
		if err != nil {
			return err
		}
		if u == nil {
			var id string
			err := tx.QueryRow(ctx, `INSERT INTO users (id, email, password_hash, password_changed_at, created_at)
				VALUES ($1, $2, $3, $4, $4) ON CONFLICT (email) DO NOTHING RETURNING id`,
				store.NewID("usr"), email, hash, s.now()).Scan(&id)
			if err == pgx.ErrNoRows { // 并发注册同一邮箱：按已注册处理
				return s.enqueue(ctx, tx, email, mail.TemplateAccountExists, nil)
			}
			if err != nil {
				return err
			}
			return s.IssueToken(ctx, tx, id, email, PurposeVerifyEmail, mail.TemplateVerifyEmail)
		}
		if !u.Verified && u.PasswordHash != nil {
			// 已注册未验证：重发验证码，不改密码（防止他人抢先改掉本人设置的密码）。
			return s.IssueToken(ctx, tx, u.ID, email, PurposeVerifyEmail, mail.TemplateVerifyEmail)
		}
		return s.enqueue(ctx, tx, email, mail.TemplateAccountExists, nil)
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.kickMail()
	httpx.WriteJSON(w, http.StatusCreated, map[string]string{"email": email})
}

func (s *Service) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code  string `json:"code"`
		Email string `json:"email"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !codePattern.MatchString(in.Code) {
		httpx.WriteError(w, r, httpx.Invalid("请输入 6 位数字验证码"))
		return
	}
	ctx := r.Context()
	p, loggedIn := httpx.UserFrom(ctx)
	var email string
	if !loggedIn {
		var err error
		if email, err = NormalizeEmail(in.Email); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	ip := httpx.ClientIP(r)
	var acct *Account
	var token string
	err := s.tx(ctx, func(tx pgx.Tx, biz *error) error {
		if err := s.take(ctx, tx, ip, s.cfg.Limits.VerifyPerIP, "尝试太频繁，请稍后再试"); err != nil {
			return err
		}
		var u *userRow
		var err error
		if loggedIn {
			u, err = userByID(ctx, tx, p.UserID, true)
		} else {
			u, err = userByEmail(ctx, tx, email, true)
		}
		if err != nil {
			return err
		}
		if u == nil {
			*biz = errBadCode
			return nil
		}
		if loggedIn && u.Verified {
			acct = u.account()
			return nil
		}
		ok, err := s.consumeToken(ctx, tx, u.ID, PurposeVerifyEmail, in.Code)
		if err != nil {
			return err
		}
		if !ok {
			*biz = errBadCode
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = $2 WHERE id = $1 AND email_verified_at IS NULL`, u.ID, s.now()); err != nil {
			return err
		}
		u.Verified = true
		acct = u.account()
		if !loggedIn {
			token, _, err = s.createSession(ctx, tx, u.ID, ip, r.UserAgent())
		}
		return err
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if token != "" {
		s.cfg.Cookies.SetUser(w, token, s.now().Add(s.cfg.SessionTTL))
		s.cfg.Cookies.RotateCSRF(w)
	}
	httpx.WriteJSON(w, http.StatusOK, acct)
}

// decodeOptional 解析可选请求体：空请求体视为空对象。
func decodeOptional(r *http.Request, v any) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, httpx.MaxBodyBytes+1))
	if err != nil {
		return httpx.Invalid("读取请求体失败")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(b))
	return httpx.DecodeJSON(r2, v)
}

func (s *Service) handleResend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if err := decodeOptional(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	p, loggedIn := httpx.UserFrom(ctx)
	var email string
	if loggedIn {
		email = p.Email
	} else {
		var err error
		if email, err = NormalizeEmail(in.Email); err != nil {
			// 未登录且邮箱不合法：一律 204，不给任何信息。
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	err := s.tx(ctx, func(tx pgx.Tx, _ *error) error {
		if !loggedIn {
			if err := s.take(ctx, tx, httpx.ClientIP(r), s.cfg.Limits.ResetPerIP, "操作太频繁，请稍后再试"); err != nil {
				return err
			}
		}
		if err := s.TakeMailQuota(ctx, tx, email); err != nil {
			return err
		}
		u, err := userByEmail(ctx, tx, email, true)
		if err != nil || u == nil || u.Verified || u.PasswordHash == nil {
			return err
		}
		return s.IssueToken(ctx, tx, u.ID, email, PurposeVerifyEmail, mail.TemplateVerifyEmail)
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.kickMail()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleResetRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx := r.Context()
	err = s.tx(ctx, func(tx pgx.Tx, _ *error) error {
		if err := s.take(ctx, tx, httpx.ClientIP(r), s.cfg.Limits.ResetPerIP, "操作太频繁，请稍后再试"); err != nil {
			return err
		}
		if err := s.TakeMailQuota(ctx, tx, email); err != nil {
			return err
		}
		u, err := userByEmail(ctx, tx, email, true)
		if err != nil || u == nil {
			return err
		}
		return s.issueReset(ctx, tx, u)
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.kickMail()
	w.WriteHeader(http.StatusNoContent)
}

// issueReset 签发找回密码材料；尚未设置密码（管理员代建）的用户收到“设置登录密码”邮件。
func (s *Service) issueReset(ctx context.Context, tx pgx.Tx, u *userRow) error {
	tmpl := mail.TemplateResetPassword
	if u.PasswordHash == nil {
		tmpl = mail.TemplateSetPassword
	}
	return s.IssueToken(ctx, tx, u.ID, u.Email, PurposeResetPassword, tmpl)
}

func (s *Service) handleResetConfirm(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email       string `json:"email"`
		Code        string `json:"code"`
		NewPassword string `json:"newPassword"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	email, err := NormalizeEmail(in.Email)
	if err == nil && !codePattern.MatchString(in.Code) {
		err = httpx.Invalid("请输入 6 位数字验证码")
	}
	if err == nil {
		err = CheckPassword(in.NewPassword)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	hash := secret.HashPassword(in.NewPassword)
	err = s.tx(ctx, func(tx pgx.Tx, biz *error) error {
		if err := s.take(ctx, tx, httpx.ClientIP(r), s.cfg.Limits.VerifyPerIP, "尝试太频繁，请稍后再试"); err != nil {
			return err
		}
		u, err := userByEmail(ctx, tx, email, true)
		if err != nil {
			return err
		}
		if u == nil {
			*biz = errBadCode
			return nil
		}
		ok, err := s.consumeToken(ctx, tx, u.ID, PurposeResetPassword, in.Code)
		if err != nil {
			return err
		}
		if !ok {
			*biz = errBadCode
			return nil
		}
		now := s.now()
		// 管理员代建用户用本接口首次设置密码，成功同时视为邮箱已验证（docs/06 §8.2）。
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, password_changed_at = $3,
			email_verified_at = coalesce(email_verified_at, $3) WHERE id = $1`, u.ID, hash, now); err != nil {
			return err
		}
		if err := ratelimit.Reset(ctx, tx, s.cfg.Limits.LoginPerEmail.Scope, email); err != nil {
			return err
		}
		return RevokeSessions(ctx, tx, now, u.ID, "password_reset", "")
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.cfg.Cookies.ClearUser(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := CheckPassword(in.Next); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if in.Next == in.Current {
		httpx.WriteError(w, r, httpx.Invalid("新密码不能与当前密码相同"))
		return
	}
	ctx := r.Context()
	p, _ := httpx.UserFrom(ctx)
	sid := sessionFrom(ctx)
	hash := secret.HashPassword(in.Next)
	var token string
	err := s.tx(ctx, func(tx pgx.Tx, biz *error) error {
		now := s.now()
		wait, err := ratelimit.Check(ctx, tx, now, p.UserID, s.cfg.Limits.ChangePWPerUser)
		if err != nil {
			return err
		}
		if wait > 0 {
			return httpx.RateLimited("当前密码错误次数过多，请稍后再试", wait)
		}
		u, err := userByID(ctx, tx, p.UserID, true)
		if err != nil {
			return err
		}
		if u == nil {
			return httpx.Unauthorized()
		}
		if u.PasswordHash == nil || !secret.VerifyPassword(*u.PasswordHash, in.Current) {
			*biz = httpx.Invalid("当前密码不正确")
			return ratelimit.Hit(ctx, tx, now, p.UserID, s.cfg.Limits.ChangePWPerUser)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, password_changed_at = $3 WHERE id = $1`, u.ID, hash, now); err != nil {
			return err
		}
		// 撤销其他全部会话，当前会话换发新令牌（docs/06 §8.2）。登录密码与接入密码互不影响（R-15.7）。
		if err := RevokeSessions(ctx, tx, now, u.ID, "password_changed", sid); err != nil {
			return err
		}
		var hashTok []byte
		token, hashTok = session.NewToken()
		_, err = tx.Exec(ctx, `UPDATE sessions SET token_hash = $2, last_seen_at = $3 WHERE id = $1`, sid, hashTok, now)
		return err
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var exp = s.now().Add(s.cfg.SessionTTL)
	_ = s.pool.QueryRow(ctx, `SELECT expires_at FROM sessions WHERE id = $1`, sid).Scan(&exp)
	s.cfg.Cookies.SetUser(w, token, exp)
	w.WriteHeader(http.StatusNoContent)
}
