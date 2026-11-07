package admin

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/control/account"
	"github.com/go-gost/gost/internal/control/audit"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/ratelimit"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/session"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
)

var (
	errBadLogin = httpx.Invalid("邮箱或密码不正确")
	errBadMFA   = httpx.Invalid("验证码或恢复码不正确")
	codeRe      = regexp.MustCompile(`^[0-9]{6}$`)
)

// tx 在事务中执行 fn；fn 写入 *biz 的错误在提交之后返回（失败计数、失败审计需要落库）。
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

func rateLimited(ctx context.Context, tx pgx.Tx, now time.Time, key string, rule ratelimit.Rule, take bool, msg string) error {
	var wait time.Duration
	var err error
	if take {
		wait, err = ratelimit.Take(ctx, tx, now, key, rule)
	} else {
		wait, err = ratelimit.Check(ctx, tx, now, key, rule)
	}
	if err != nil {
		return err
	}
	if wait > 0 {
		return httpx.RateLimited(msg, wait)
	}
	return nil
}

type loginChallenge struct {
	Stage      string      `json:"stage"`
	Enrollment *enrollment `json:"enrollment,omitempty"`
}

type enrollment struct {
	Secret     string `json:"secret"`
	OTPAuthURI string `json:"otpauthUri"`
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
	email, err := account.NormalizeEmail(in.Email)
	if err != nil || in.Password == "" || len(in.Password) > 1024 {
		httpx.WriteError(w, r, errBadLogin)
		return
	}
	ctx := r.Context()
	ip := httpx.ClientIP(r)
	var token string
	var out loginChallenge
	err = s.tx(ctx, func(tx pgx.Tx, biz *error) error {
		now := s.now()
		if err := rateLimited(ctx, tx, now, ip, s.cfg.Limits.LoginPerIP, true, "登录尝试太频繁，请稍后再试"); err != nil {
			return err
		}
		if err := rateLimited(ctx, tx, now, email, s.cfg.Limits.LoginPerEmail, false, "登录失败次数过多，请稍后再试"); err != nil {
			return err
		}
		var id, hash, status string
		var enrolled bool
		err := tx.QueryRow(ctx, `SELECT id, password_hash, status, totp_enrolled_at IS NOT NULL FROM admins WHERE email = $1`, email).
			Scan(&id, &hash, &status, &enrolled)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		found := err == nil
		if !found {
			secret.DummyVerify(in.Password)
		}
		if !found || !secret.VerifyPassword(hash, in.Password) || status != "active" {
			*biz = errBadLogin
			details := map[string]any{"email": email}
			if found && status != "active" {
				details["reason"] = "disabled"
			}
			if err := audit.Record(ctx, tx, audit.Entry{Action: "admin.login_failed", Outcome: audit.Denied,
				ActorKind: audit.ActorAdmin, AdminID: id, TargetType: "admin", TargetID: id, Details: details}); err != nil {
				return err
			}
			return ratelimit.Hit(ctx, tx, now, email, s.cfg.Limits.LoginPerEmail)
		}
		var hashTok []byte
		token, hashTok = session.NewToken()
		stage := "totp_pending"
		var pending []byte
		if !enrolled {
			stage = "enroll_pending"
			sec := NewTOTPSecret()
			pending = s.cfg.Box.Seal(sec, totpAAD(id))
			out.Enrollment = &enrollment{Secret: EncodeSecret(sec), OTPAuthURI: OTPAuthURI(s.cfg.Issuer, email, sec)}
			out.Stage = "enrollment_required"
		} else {
			out.Stage = "totp_required"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, kind, admin_id, token_hash, stage, pending_totp_secret_enc,
			ip, user_agent, created_at, last_seen_at, expires_at)
			VALUES ($1, 'admin', $2, $3, $4, $5, $6::inet, $7, $8, $8, $9)`,
			store.NewID("ses"), id, hashTok, stage, pending, inetOrNil(ip), truncate(r.UserAgent(), 512), now, now.Add(s.cfg.PendingTTL)); err != nil {
			return err
		}
		if err := ratelimit.Reset(ctx, tx, s.cfg.Limits.LoginPerEmail.Scope, email); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "admin.login_password", AdminID: id, TargetType: "admin", TargetID: id,
			Details: map[string]any{"stage": out.Stage}})
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.cfg.Cookies.SetAdmin(w, token, s.now().Add(s.cfg.PendingTTL))
	s.cfg.Cookies.RotateCSRF(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

type adminSession struct {
	Admin         *Admin   `json:"admin"`
	RecoveryCodes []string `json:"recoveryCodes,omitempty"`
}

func (s *Service) handleMFA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code         string `json:"code"`
		RecoveryCode string `json:"recoveryCode"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if (in.Code == "") == (in.RecoveryCode == "") {
		httpx.WriteError(w, r, httpx.Invalid("请填写 6 位验证码或恢复码（二选一）"))
		return
	}
	if in.Code != "" && !codeRe.MatchString(in.Code) {
		httpx.WriteError(w, r, httpx.Invalid("请输入 6 位数字验证码"))
		return
	}
	tok := session.TokenFrom(r, session.AdminCookie)
	if tok == "" {
		httpx.WriteError(w, r, httpx.Unauthorized())
		return
	}
	ctx := r.Context()
	var newToken string
	var out adminSession
	var expires time.Time
	err := s.tx(ctx, func(tx pgx.Tx, biz *error) error {
		now := s.now()
		var sid, adminID, stage string
		var pending, stored []byte
		var lastStep int64
		err := tx.QueryRow(ctx, `SELECT s.id, s.admin_id, s.stage, s.pending_totp_secret_enc, a.totp_secret_enc, a.totp_last_step
			FROM sessions s JOIN admins a ON a.id = s.admin_id
			WHERE s.token_hash = $1 AND s.kind = 'admin' AND s.stage IN ('totp_pending', 'enroll_pending')
			  AND s.revoked_at IS NULL AND s.expires_at > $2 AND a.status = 'active'
			FOR UPDATE OF s, a`, secret.HashToken(tok), now).Scan(&sid, &adminID, &stage, &pending, &stored, &lastStep)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.Unauthorized()
		}
		if err != nil {
			return err
		}
		actx := httpx.WithAdmin(ctx, httpx.AdminPrincipal{AdminID: adminID})
		if err := rateLimited(ctx, tx, now, adminID, s.cfg.Limits.MFAPerAdmin, false, "验证失败次数过多，请稍后再试"); err != nil {
			return err
		}
		fail := func(method string) error {
			*biz = errBadMFA
			if err := audit.Record(actx, tx, audit.Entry{Action: "admin.mfa_failed", Outcome: audit.Denied, TargetType: "admin",
				TargetID: adminID, Details: map[string]any{"method": method}}); err != nil {
				return err
			}
			return ratelimit.Hit(ctx, tx, now, adminID, s.cfg.Limits.MFAPerAdmin)
		}
		method := "totp"
		switch {
		case stage == "enroll_pending":
			if in.Code == "" {
				*biz = httpx.Invalid("首次登录请用验证器 App 中的 6 位验证码完成绑定")
				return nil
			}
			sec, err := s.cfg.Box.Open(pending, totpAAD(adminID))
			if err != nil {
				return err
			}
			step, ok := VerifyTOTP(sec, in.Code, now, 0)
			if !ok {
				return fail("totp_enroll")
			}
			if _, err := tx.Exec(ctx, `UPDATE admins SET totp_secret_enc = $2, totp_enrolled_at = $3, totp_last_step = $4
				WHERE id = $1 AND totp_enrolled_at IS NULL`, adminID, pending, now, step); err != nil {
				return err
			}
			codes, err := s.replaceRecoveryCodes(ctx, tx, adminID, now)
			if err != nil {
				return err
			}
			out.RecoveryCodes = codes
			method = "totp_enroll"
			if err := audit.Record(actx, tx, audit.Entry{Action: "admin.totp_enrolled", TargetType: "admin", TargetID: adminID}); err != nil {
				return err
			}
		case in.Code != "":
			sec, err := s.cfg.Box.Open(stored, totpAAD(adminID))
			if err != nil {
				return err
			}
			step, ok := VerifyTOTP(sec, in.Code, now, lastStep)
			if !ok {
				return fail("totp")
			}
			if _, err := tx.Exec(ctx, `UPDATE admins SET totp_last_step = $2 WHERE id = $1`, adminID, step); err != nil {
				return err
			}
		default:
			method = "recovery_code"
			tag, err := tx.Exec(ctx, `UPDATE admin_recovery_codes SET used_at = $3
				WHERE admin_id = $1 AND code_hash = $2 AND used_at IS NULL AND revoked_at IS NULL`,
				adminID, s.recoveryMAC(adminID, in.RecoveryCode), now)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fail("recovery_code")
			}
		}
		// 第二步通过：会话换发新令牌（防会话固定）并转为 active。
		var hashTok []byte
		newToken, hashTok = session.NewToken()
		expires = now.Add(s.cfg.SessionTTL)
		if _, err := tx.Exec(ctx, `UPDATE sessions SET stage = 'active', pending_totp_secret_enc = NULL, token_hash = $2,
			last_seen_at = $3, expires_at = $4 WHERE id = $1`, sid, hashTok, now, expires); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE admins SET last_login_at = $2 WHERE id = $1`, adminID, now); err != nil {
			return err
		}
		if err := ratelimit.Reset(ctx, tx, s.cfg.Limits.MFAPerAdmin.Scope, adminID); err != nil {
			return err
		}
		if err := audit.Record(actx, tx, audit.Entry{Action: "admin.login", TargetType: "admin", TargetID: adminID,
			Details: map[string]any{"method": method}}); err != nil {
			return err
		}
		out.Admin, err = loadAdmin(ctx, tx, adminID)
		return err
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.cfg.Cookies.SetAdmin(w, newToken, expires)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// 恢复码：10 个，每个 10 个 Crockford base32 字符，显示为 xxxxx-xxxxx；只存 HMAC，一次性。
const (
	recoveryCount    = 10
	recoveryAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"
)

func newRecoveryCode() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("admin: crypto/rand 不可用: " + err.Error())
	}
	var b strings.Builder
	for i := 0; i < 10; i++ {
		if i == 5 {
			b.WriteByte('-')
		}
		b.WriteByte(recoveryAlphabet[raw[i]&31])
	}
	return b.String()
}

func normalizeRecovery(code string) string {
	c := strings.ToLower(strings.TrimSpace(code))
	c = strings.NewReplacer("-", "", " ", "").Replace(c)
	return c
}

func (s *Service) recoveryMAC(adminID, code string) []byte {
	return s.cfg.Box.MAC("admin_recovery", adminID, normalizeRecovery(code))
}

// replaceRecoveryCodes 作废旧码并生成新的一组，返回明文（只显示这一次）。
func (s *Service) replaceRecoveryCodes(ctx context.Context, tx pgx.Tx, adminID string, now time.Time) ([]string, error) {
	if _, err := tx.Exec(ctx, `UPDATE admin_recovery_codes SET revoked_at = $2 WHERE admin_id = $1 AND used_at IS NULL AND revoked_at IS NULL`,
		adminID, now); err != nil {
		return nil, err
	}
	codes := make([]string, 0, recoveryCount)
	for len(codes) < recoveryCount {
		c := newRecoveryCode()
		tag, err := tx.Exec(ctx, `INSERT INTO admin_recovery_codes (admin_id, code_hash, created_at) VALUES ($1, $2, $3)
			ON CONFLICT (admin_id, code_hash) DO NOTHING`, adminID, s.recoveryMAC(adminID, c), now)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			codes = append(codes, c)
		}
	}
	return codes, nil
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, _ := httpx.AdminFrom(ctx)
	err := store.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2, revoke_reason = 'logout' WHERE id = $1 AND revoked_at IS NULL`,
			adminSessionFrom(ctx), s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "admin.logout", TargetType: "admin", TargetID: a.AdminID})
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.cfg.Cookies.ClearAdmin(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	a, _ := httpx.AdminFrom(r.Context())
	out, err := loadAdmin(r.Context(), s.pool, a.AdminID)
	if err == nil && out == nil {
		err = httpx.Unauthorized()
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) handleRegenerateCodes(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !codeRe.MatchString(in.Code) {
		httpx.WriteError(w, r, httpx.Invalid("请输入 6 位数字验证码"))
		return
	}
	ctx := r.Context()
	a, _ := httpx.AdminFrom(ctx)
	var codes []string
	err := s.tx(ctx, func(tx pgx.Tx, biz *error) error {
		now := s.now()
		if err := rateLimited(ctx, tx, now, a.AdminID, s.cfg.Limits.MFAPerAdmin, false, "验证失败次数过多，请稍后再试"); err != nil {
			return err
		}
		var stored []byte
		var last int64
		if err := tx.QueryRow(ctx, `SELECT totp_secret_enc, totp_last_step FROM admins WHERE id = $1 FOR UPDATE`, a.AdminID).
			Scan(&stored, &last); err != nil {
			return err
		}
		sec, err := s.cfg.Box.Open(stored, totpAAD(a.AdminID))
		if err != nil {
			return err
		}
		step, ok := VerifyTOTP(sec, in.Code, now, last)
		if !ok {
			*biz = httpx.Invalid("验证码不正确")
			if err := audit.Record(ctx, tx, audit.Entry{Action: "admin.mfa_failed", Outcome: audit.Denied, TargetType: "admin",
				TargetID: a.AdminID, Details: map[string]any{"method": "totp", "purpose": "recovery_regenerate"}}); err != nil {
				return err
			}
			return ratelimit.Hit(ctx, tx, now, a.AdminID, s.cfg.Limits.MFAPerAdmin)
		}
		if _, err := tx.Exec(ctx, `UPDATE admins SET totp_last_step = $2 WHERE id = $1`, a.AdminID, step); err != nil {
			return err
		}
		if codes, err = s.replaceRecoveryCodes(ctx, tx, a.AdminID, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "admin.recovery_codes_regenerated", TargetType: "admin", TargetID: a.AdminID})
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string][]string{"codes": codes})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
