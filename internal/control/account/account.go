// Package account 是用户邮箱账号（C-W10，R-15）：注册、邮箱验证、重发、登录、退出、当前会话、
// 找回密码（申请与确认）、修改密码，以及路由器使用的会话校验。
//
// 不泄露邮箱是否注册（R-15.3）：注册、重发、找回对任何格式正确的邮箱给出相同响应；限流事件对任何邮箱都记录，
// 429 因此也不泄露；账号不存在时仍做一次等价的 argon2id 计算，响应时间不可区分。
//
// 验证材料（D-15）：6 位、15 分钟、最多 5 次尝试、一次性、用途绑定；重发使旧材料失效（superseded）。
// 库里只存 HMAC（secret.Box.MAC），邮件数据在发件箱里加密。
package account

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	netmail "net/mail"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/mail"
	"github.com/go-gost/gost/internal/control/ratelimit"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/session"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 验证材料用途。
const (
	PurposeVerifyEmail   = "verify_email"
	PurposeResetPassword = "reset_password"
)

// Limits 是服务端限流（D-15）。零值字段取默认。
type Limits struct {
	MailPerMinute   ratelimit.Rule // 同一邮箱 60 秒 1 封
	MailPerHour     ratelimit.Rule // 同一邮箱每小时 5 封
	LoginPerEmail   ratelimit.Rule // 同一邮箱登录失败次数
	LoginPerIP      ratelimit.Rule // 同一来源 IP 登录尝试次数
	RegisterPerIP   ratelimit.Rule // 同一来源 IP 注册次数
	ResetPerIP      ratelimit.Rule // 同一来源 IP 找回申请次数
	VerifyPerIP     ratelimit.Rule // 同一来源 IP 提交验证码次数（跨邮箱撞码）
	ChangePWPerUser ratelimit.Rule // 同一用户改密时当前密码错误次数
}

// DefaultLimits 是首期取值。
var DefaultLimits = Limits{
	MailPerMinute:   ratelimit.Rule{Scope: "mail_send_1m", Limit: 1, Window: time.Minute},
	MailPerHour:     ratelimit.Rule{Scope: "mail_send_1h", Limit: 5, Window: time.Hour},
	LoginPerEmail:   ratelimit.Rule{Scope: "login_email", Limit: 10, Window: 15 * time.Minute},
	LoginPerIP:      ratelimit.Rule{Scope: "login_ip", Limit: 50, Window: 15 * time.Minute},
	RegisterPerIP:   ratelimit.Rule{Scope: "register_ip", Limit: 20, Window: time.Hour},
	ResetPerIP:      ratelimit.Rule{Scope: "reset_ip", Limit: 20, Window: time.Hour},
	VerifyPerIP:     ratelimit.Rule{Scope: "verify_ip", Limit: 50, Window: 15 * time.Minute},
	ChangePWPerUser: ratelimit.Rule{Scope: "change_pw_user", Limit: 10, Window: 15 * time.Minute},
}

func (l *Limits) fill() {
	d := DefaultLimits
	for _, p := range []struct{ dst, def *ratelimit.Rule }{
		{&l.MailPerMinute, &d.MailPerMinute}, {&l.MailPerHour, &d.MailPerHour}, {&l.LoginPerEmail, &d.LoginPerEmail},
		{&l.LoginPerIP, &d.LoginPerIP}, {&l.RegisterPerIP, &d.RegisterPerIP}, {&l.ResetPerIP, &d.ResetPerIP},
		{&l.VerifyPerIP, &d.VerifyPerIP}, {&l.ChangePWPerUser, &d.ChangePWPerUser},
	} {
		if p.dst.Scope == "" {
			*p.dst = *p.def
		}
	}
}

// Config 配置 Service。
type Config struct {
	Box         *secret.Box
	Cookies     session.Cookies
	Limits      Limits
	SessionTTL  time.Duration    // 用户会话有效期，默认 30 天
	TokenTTL    time.Duration    // 验证材料有效期，默认 15 分钟
	MaxAttempts int              // 验证材料尝试上限，默认 5
	Now         func() time.Time // 默认 time.Now；测试可替换
	Mail        interface{ Kick() }
	Logger      *slog.Logger
}

// Service 实现账号业务与用户会话校验。
type Service struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New 创建 Service。
func New(pool *pgxpool.Pool, cfg Config) *Service {
	if cfg.Box == nil {
		panic("account: 缺少 Box")
	}
	cfg.Limits.fill()
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 15 * time.Minute
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{pool: pool, cfg: cfg}
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

func (s *Service) kickMail() {
	if s.cfg.Mail != nil {
		s.cfg.Mail.Kick()
	}
}

// NormalizeEmail 去空白、转小写并校验格式；不合法时返回 Invalid。
func NormalizeEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" || len(e) > 254 || strings.ContainsAny(e, " \t\r\n<>\"(),;:\\[]") {
		return "", httpx.Invalid("请输入正确的邮箱地址")
	}
	a, err := netmail.ParseAddress(e)
	if err != nil || a.Address != e {
		return "", httpx.Invalid("请输入正确的邮箱地址")
	}
	at := strings.LastIndexByte(e, '@')
	if at < 1 || !strings.Contains(e[at+1:], ".") || strings.HasSuffix(e, ".") {
		return "", httpx.Invalid("请输入正确的邮箱地址")
	}
	return e, nil
}

// CheckPassword 校验新密码长度（10～256 个字符）。
func CheckPassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < 10 {
		return httpx.Invalid("密码至少 10 位")
	}
	if n > 256 {
		return httpx.Invalid("密码太长")
	}
	return nil
}

func newCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic("account: crypto/rand 不可用: " + err.Error())
	}
	return fmt.Sprintf("%06d", n.Int64())
}

func (s *Service) codeMAC(userID, purpose, code string) []byte {
	return s.cfg.Box.MAC("email_token", userID, purpose, code)
}

// IssueToken 为用户签发一份新的验证材料（同用途旧材料作废）并把邮件写入发件箱，在调用方事务内执行。
// template 为 mail 模板名（验证、找回或代建设密码）。不做限流：调用方先 TakeMailQuota。
func (s *Service) IssueToken(ctx context.Context, tx pgx.Tx, userID, email, purpose, template string) error {
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE email_tokens SET superseded_at = $3
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND superseded_at IS NULL`, userID, purpose, now); err != nil {
		return err
	}
	code := newCode()
	if _, err := tx.Exec(ctx, `INSERT INTO email_tokens (id, user_id, purpose, sent_to, code_hash, max_attempts, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		store.NewID("etk"), userID, purpose, email, s.codeMAC(userID, purpose, code), s.cfg.MaxAttempts, now, now.Add(s.cfg.TokenTTL)); err != nil {
		return err
	}
	return s.enqueue(ctx, tx, email, template, map[string]string{"code": code})
}

// TakeMailQuota 按邮箱做发信限流（60 秒 1 封、每小时 5 封），对任何邮箱（无论是否注册）同样计数。
func (s *Service) TakeMailQuota(ctx context.Context, tx pgx.Tx, email string) error {
	wait, err := ratelimit.Take(ctx, tx, s.now(), email, s.cfg.Limits.MailPerMinute, s.cfg.Limits.MailPerHour)
	if err != nil {
		return err
	}
	if wait > 0 {
		return httpx.RateLimited("邮件发送太频繁，请稍后再试", wait)
	}
	return nil
}

func (s *Service) take(ctx context.Context, tx pgx.Tx, key string, rule ratelimit.Rule, msg string) error {
	wait, err := ratelimit.Take(ctx, tx, s.now(), key, rule)
	if err != nil {
		return err
	}
	if wait > 0 {
		return httpx.RateLimited(msg, wait)
	}
	return nil
}

// errBadCode 是验证码错误、过期、超次、用途不符、已使用的统一说明。
var errBadCode = httpx.Invalid("验证码不正确或已失效，请重新获取")

// consumeToken 校验并消费验证材料。错误时累加尝试次数（在 tx 内写入，调用方必须提交 tx 后再返回错误）。
// 返回 (ok, err)：ok=false 且 err=nil 表示验证码不对。
func (s *Service) consumeToken(ctx context.Context, tx pgx.Tx, userID, purpose, code string) (bool, error) {
	now := s.now()
	var id string
	var hash []byte
	var attempts, max int
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT id, code_hash, attempts, max_attempts, expires_at FROM email_tokens
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND superseded_at IS NULL FOR UPDATE`,
		userID, purpose).Scan(&id, &hash, &attempts, &max, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !now.Before(expires) || attempts >= max {
		return false, nil
	}
	if !secret.Equal(hash, s.codeMAC(userID, purpose, code)) {
		_, err := tx.Exec(ctx, `UPDATE email_tokens SET attempts = attempts + 1 WHERE id = $1`, id)
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE email_tokens SET consumed_at = $2, attempts = attempts + 1 WHERE id = $1`, id, now)
	return err == nil, err
}

// RevokeSessions 撤销用户全部有效会话（except 非空时保留该会话），在调用方事务内执行。
func RevokeSessions(ctx context.Context, tx pgx.Tx, now time.Time, userID, reason, except string) error {
	_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2, revoke_reason = $3
		WHERE kind = 'user' AND user_id = $1 AND revoked_at IS NULL AND id <> $4`, userID, now, reason, except)
	return err
}

type userRow struct {
	ID           string
	Email        string
	Verified     bool
	PasswordHash *string
	Status       string
	CreatedAt    time.Time
}

func scanUser(row pgx.Row) (*userRow, error) {
	var u userRow
	err := row.Scan(&u.ID, &u.Email, &u.Verified, &u.PasswordHash, &u.Status, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

const userCols = `id, email, email_verified_at IS NOT NULL, password_hash, status, created_at`

func userByEmail(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, email string, lock bool) (*userRow, error) {
	sql := `SELECT ` + userCols + ` FROM users WHERE email = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanUser(q.QueryRow(ctx, sql, email))
}

func userByID(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string, lock bool) (*userRow, error) {
	sql := `SELECT ` + userCols + ` FROM users WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanUser(q.QueryRow(ctx, sql, id))
}

// Account 对应 openapi Account。
type Account struct {
	Email     string    `json:"email"`
	Verified  bool      `json:"verified"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

func (u *userRow) account() *Account {
	return &Account{Email: u.Email, Verified: u.Verified, Status: u.Status, CreatedAt: u.CreatedAt.UTC()}
}

// createSession 新建用户会话，返回令牌与到期时间。
func (s *Service) createSession(ctx context.Context, tx pgx.Tx, userID, ip, ua string) (string, time.Time, error) {
	now := s.now()
	token, hash := session.NewToken()
	exp := now.Add(s.cfg.SessionTTL)
	_, err := tx.Exec(ctx, `INSERT INTO sessions (id, kind, user_id, token_hash, ip, user_agent, created_at, last_seen_at, expires_at)
		VALUES ($1, 'user', $2, $3, $4::inet, $5, $6, $6, $7)`,
		store.NewID("ses"), userID, hash, inetOrNil(ip), truncate(ua, 512), now, exp)
	return token, exp, err
}

type sessionKey struct{}

// sessionFrom 返回路由器校验会话时放入上下文的会话 ID。
func sessionFrom(ctx context.Context) string {
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}

// touchEvery 是 last_seen_at 的最小更新间隔，避免每个请求都写库。
const touchEvery = 5 * time.Minute

// AuthenticateUser 校验 isp_session 令牌。有效时返回带 httpx.Principal（及会话 ID）的上下文。
func (s *Service) AuthenticateUser(ctx context.Context, token string) (context.Context, bool, error) {
	if token == "" || len(token) > 128 {
		return ctx, false, nil
	}
	now := s.now()
	var sid string
	var p httpx.Principal
	var status string
	var lastSeen time.Time
	err := s.pool.QueryRow(ctx, `SELECT s.id, s.last_seen_at, u.id, u.email, u.email_verified_at IS NOT NULL, u.status
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.kind = 'user' AND s.revoked_at IS NULL AND s.expires_at > $2`,
		secret.HashToken(token), now).Scan(&sid, &lastSeen, &p.UserID, &p.Email, &p.Verified, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ctx, false, nil
	}
	if err != nil {
		return ctx, false, err
	}
	p.Banned = status == "banned"
	if now.Sub(lastSeen) > touchEvery {
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, sid, now); err != nil {
			s.cfg.Logger.Warn("account: 更新会话活跃时间失败", "err", err)
		}
	}
	ctx = context.WithValue(httpx.WithUser(ctx, p), sessionKey{}, sid)
	return ctx, true, nil
}

func inetOrNil(ip string) any {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return nil
	}
	return a.Unmap().String()
}

func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, to, template string, data map[string]string) error {
	_, err := mail.Enqueue(ctx, tx, s.cfg.Box, mail.Message{To: to, Template: template, Data: data})
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
