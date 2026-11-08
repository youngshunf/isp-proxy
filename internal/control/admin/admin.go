// Package admin 是管理员身份与管理端的平台部分（C-W8，R-15.5、R-15.6）：
//
//   - 两步登录：邮箱 + 密码 → TOTP（RFC 6238）或恢复码；首次登录即绑定 TOTP 并发放 10 个一次性恢复码；
//   - 管理员会话与用户会话分开（sessions.kind = 'admin'，Cookie isp_admin，Path=/admin-api）；
//   - 管理员只由受控流程（CLI，见 Create）建立，不开放自助注册或角色修改；
//   - 审计查询、用户列表/详情/代建/封禁/解封/发重设邮件；敏感操作都在同一事务内写 audit_logs。
package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

// 角色。
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Hooks 是由订阅、订单等业务包（ctl-core）提供的回调；字段为 nil 时跳过。
type Hooks struct {
	// UserStatusChanged 在封禁/解封的同一事务内调用：订阅包据此让该用户全部订阅生成新的期望版本，
	// agent 断开存量连接（R-13.8，阻断原因 account_banned）。
	UserStatusChanged func(ctx context.Context, tx pgx.Tx, userID string) error
	// UserSubscriptions 返回用户详情的 subscriptions（openapi AdminSubscription 数组）。
	UserSubscriptions func(ctx context.Context, userID string) (any, error)
	// UserRecentOrders 返回用户详情的 recentOrders（openapi AdminOrder 数组）。
	UserRecentOrders func(ctx context.Context, userID string) (any, error)
	// UserDirect 返回用户详情的 direct（openapi AdminUserDirect：是否已赠送直出代理、带宽上限与默认值，D-25）。
	UserDirect func(ctx context.Context, userID string) (any, error)
}

// Limits 是管理员登录限流。
type Limits struct {
	LoginPerEmail ratelimit.Rule // 同一邮箱密码错误次数
	LoginPerIP    ratelimit.Rule // 同一来源 IP 登录尝试次数
	MFAPerAdmin   ratelimit.Rule // 同一管理员第二步失败次数
}

// DefaultLimits 是首期取值。
var DefaultLimits = Limits{
	LoginPerEmail: ratelimit.Rule{Scope: "admin_login_email", Limit: 5, Window: 15 * time.Minute},
	LoginPerIP:    ratelimit.Rule{Scope: "admin_login_ip", Limit: 30, Window: 15 * time.Minute},
	MFAPerAdmin:   ratelimit.Rule{Scope: "admin_mfa", Limit: 5, Window: 15 * time.Minute},
}

// Config 配置 Service。
type Config struct {
	Box        *secret.Box
	Cookies    session.Cookies
	Account    *account.Service
	Hooks      Hooks
	Limits     Limits
	Issuer     string        // TOTP 发行方名称，显示在验证器 App 中
	SessionTTL time.Duration // 完成第二步后的会话有效期，默认 12 小时
	IdleTTL    time.Duration // 无操作超时，默认 1 小时
	PendingTTL time.Duration // 第一步通过后完成第二步的时限，默认 10 分钟
	Now        func() time.Time
	Logger     *slog.Logger
}

// Service 实现管理员登录、会话校验与管理端平台路由。
type Service struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New 创建 Service。
func New(pool *pgxpool.Pool, cfg Config) *Service {
	if cfg.Box == nil || cfg.Account == nil {
		panic("admin: 缺少 Box 或 Account")
	}
	if cfg.Limits.LoginPerEmail.Scope == "" {
		cfg.Limits.LoginPerEmail = DefaultLimits.LoginPerEmail
	}
	if cfg.Limits.LoginPerIP.Scope == "" {
		cfg.Limits.LoginPerIP = DefaultLimits.LoginPerIP
	}
	if cfg.Limits.MFAPerAdmin.Scope == "" {
		cfg.Limits.MFAPerAdmin = DefaultLimits.MFAPerAdmin
	}
	if cfg.Issuer == "" {
		cfg.Issuer = "ISP 代理管理后台"
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = time.Hour
	}
	if cfg.PendingTTL <= 0 {
		cfg.PendingTTL = 10 * time.Minute
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

// totpAAD 绑定 TOTP 密文与管理员，防止把别人的密文挪过来用。
func totpAAD(adminID string) []byte { return []byte("totp:" + adminID) }

// ViewerWritable 是 viewer 也能调用的非 GET 路由（本人操作），供路由器使用。
var ViewerWritable = map[string]bool{
	"DELETE /admin-api/sessions/current":   true,
	"POST /admin-api/me/recovery-codes":    true,
	"POST /admin-api/sessions":             true,
	"POST /admin-api/sessions/current/mfa": true,
}

// Routes 返回管理端平台路由（admin-auth、admin-users、审计查询）。
func (s *Service) Routes() []httpx.Route {
	return []httpx.Route{
		{Method: "POST", Pattern: "/admin-api/sessions", Auth: httpx.Public, Handler: s.handleLogin},
		// 第二步只认第一步发放的待验证会话，由处理器自行校验（路由器的 Admin 级别只认已完成 TOTP 的会话）。
		{Method: "POST", Pattern: "/admin-api/sessions/current/mfa", Auth: httpx.Public, Handler: s.handleMFA},
		{Method: "DELETE", Pattern: "/admin-api/sessions/current", Auth: httpx.Admin, Handler: s.handleLogout},
		{Method: "GET", Pattern: "/admin-api/me", Auth: httpx.Admin, Handler: s.handleMe},
		{Method: "POST", Pattern: "/admin-api/me/recovery-codes", Auth: httpx.Admin, Handler: s.handleRegenerateCodes},
		{Method: "GET", Pattern: "/admin-api/audit-logs", Auth: httpx.Admin, Handler: s.handleAuditLogs},
		{Method: "GET", Pattern: "/admin-api/users", Auth: httpx.Admin, Handler: s.handleListUsers},
		{Method: "POST", Pattern: "/admin-api/users", Auth: httpx.Admin, Handler: s.handleCreateUser},
		{Method: "GET", Pattern: "/admin-api/users/{userId}", Auth: httpx.Admin, Handler: s.handleGetUser},
		{Method: "POST", Pattern: "/admin-api/users/{userId}/ban", Auth: httpx.Admin, Handler: s.handleBan},
		{Method: "POST", Pattern: "/admin-api/users/{userId}/unban", Auth: httpx.Admin, Handler: s.handleUnban},
		{Method: "POST", Pattern: "/admin-api/users/{userId}/password-reset-email", Auth: httpx.Admin, Handler: s.handleSendReset},
	}
}

type adminSessionKey struct{}

func adminSessionFrom(ctx context.Context) string {
	id, _ := ctx.Value(adminSessionKey{}).(string)
	return id
}

// touchEvery 是 last_seen_at 的最小更新间隔。
const touchEvery = time.Minute

// AuthenticateAdmin 校验 isp_admin 令牌：只有已完成第二步（stage = active）、未过期、未超时、管理员未停用的会话有效。
func (s *Service) AuthenticateAdmin(ctx context.Context, token string) (context.Context, string, bool, error) {
	if token == "" || len(token) > 128 {
		return ctx, "", false, nil
	}
	now := s.now()
	var sid, role string
	var lastSeen time.Time
	var a httpx.AdminPrincipal
	err := s.pool.QueryRow(ctx, `SELECT s.id, s.last_seen_at, a.id, a.email, a.role
		FROM sessions s JOIN admins a ON a.id = s.admin_id
		WHERE s.token_hash = $1 AND s.kind = 'admin' AND s.stage = 'active' AND s.revoked_at IS NULL
		  AND s.expires_at > $2 AND a.status = 'active'`, secret.HashToken(token), now).
		Scan(&sid, &lastSeen, &a.AdminID, &a.Email, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ctx, "", false, nil
	}
	if err != nil {
		return ctx, "", false, err
	}
	if now.Sub(lastSeen) > s.cfg.IdleTTL {
		return ctx, "", false, nil
	}
	if now.Sub(lastSeen) > touchEvery {
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, sid, now); err != nil {
			s.cfg.Logger.Warn("admin: 更新会话活跃时间失败", "err", err)
		}
	}
	ctx = context.WithValue(httpx.WithAdmin(ctx, a), adminSessionKey{}, sid)
	return ctx, role, true, nil
}

// Admin 对应 openapi Admin。
type Admin struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"displayName"`
	Role         string     `json:"role"`
	TOTPEnrolled bool       `json:"totpEnrolled"`
	LastLoginAt  *time.Time `json:"lastLoginAt"`
}

func loadAdmin(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (*Admin, error) {
	var a Admin
	err := q.QueryRow(ctx, `SELECT id, email, display_name, role, totp_enrolled_at IS NOT NULL, last_login_at FROM admins WHERE id = $1`, id).
		Scan(&a.ID, &a.Email, &a.DisplayName, &a.Role, &a.TOTPEnrolled, &a.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if a.LastLoginAt != nil {
		t := a.LastLoginAt.UTC()
		a.LastLoginAt = &t
	}
	return &a, err
}

// Create 建立管理员（CLI 使用，R-15.5）。密码至少 12 位；TOTP 在首次登录时绑定。
func Create(ctx context.Context, pool *pgxpool.Pool, email, displayName, role, password string) (string, error) {
	e, err := account.NormalizeEmail(email)
	if err != nil {
		return "", errors.New("邮箱格式不正确")
	}
	if role != RoleAdmin && role != RoleViewer {
		return "", errors.New("角色只能是 admin 或 viewer")
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = e[:strings.IndexByte(e, '@')]
	}
	if n := len([]rune(displayName)); n > 64 {
		return "", errors.New("显示名最多 64 个字符")
	}
	if len([]rune(password)) < 12 {
		return "", errors.New("管理员密码至少 12 位")
	}
	var id string
	err = store.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO admins (email, display_name, role, password_hash) VALUES ($1, $2, $3, $4) RETURNING id`,
			e, displayName, role, secret.HashPassword(password)).Scan(&id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "admin.create", ActorKind: audit.ActorSystem, TargetType: "admin", TargetID: id,
			Details: map[string]any{"email": e, "role": role, "via": "cli"}})
	})
	if err != nil {
		if strings.Contains(err.Error(), "admins_email_key") {
			return "", errors.New("该邮箱已是管理员")
		}
		return "", err
	}
	return id, nil
}

func inetOrNil(ip string) any {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return nil
	}
	return a.Unmap().String()
}
