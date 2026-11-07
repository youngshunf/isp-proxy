// Package api 组装控制面对外 HTTP 接口：把各业务包的 []httpx.Route 挂到同一个 ServeMux 上，
// 并统一执行请求来源记录、CSRF 双提交、会话校验与按 Route.Auth 的授权（docs/06 §4、§8.2）。
//
// 授权规则：
//   - /api/ 路由：带有效 isp_session 时注入 httpx.Principal（Public 路由也注入）。User 要求登录（否则 401）；
//     VerifiedUser 还要求邮箱已验证（否则 403 unverified）；封禁用户只读，非 GET 返回 412（退出登录除外）。
//   - /admin-api/ 路由：只认 isp_admin 且已完成 TOTP 的管理员会话（用户会话、未完成第二步的会话一律 401）；
//     viewer 角色的非 GET 返回 403 forbidden 并记审计（退出、重新生成恢复码等本人操作除外）。
//   - 所有非 GET/HEAD/OPTIONS 请求先校验 CSRF（X-CSRF-Token 与 isp_csrf 相同），失败 403 forbidden；
//     任何缺少 isp_csrf 的响应都会下发它。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/go-gost/gost/internal/control/audit"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/session"
)

// UserAuth 校验用户会话令牌（account.Service 实现）。有效时返回带 httpx.Principal 的上下文。
type UserAuth interface {
	AuthenticateUser(ctx context.Context, token string) (context.Context, bool, error)
}

// AdminAuth 校验管理员会话令牌（admin.Service 实现）。只有已完成 TOTP 的会话有效；返回带 httpx.AdminPrincipal 的上下文与角色。
type AdminAuth interface {
	AuthenticateAdmin(ctx context.Context, token string) (ctx2 context.Context, role string, ok bool, err error)
}

// RoleViewer 是只读管理员角色。
const RoleViewer = "viewer"

// Options 配置 NewRouter。
type Options struct {
	Users   UserAuth
	Admins  AdminAuth
	Cookies session.Cookies
	// Audit 用于记录 viewer 写操作被拒（可为 nil，不记录）。
	Audit audit.Execer
	// ViewerWritable 是 viewer 也可以调用的非 GET 路由（"METHOD /pattern"），即管理员本人操作。
	ViewerWritable map[string]bool
	// BannedWritable 是封禁用户也可以调用的非 GET 路由。默认只有退出登录。
	BannedWritable map[string]bool
	// Health 是 /healthz 的检查（例如数据库 Ping）；nil 时总是健康。
	Health func(ctx context.Context) error
	Logger *slog.Logger
}

// DefaultBannedWritable 是封禁用户可用的写操作。
var DefaultBannedWritable = map[string]bool{"DELETE /api/sessions/current": true}

// NewRouter 挂载 routes 并返回处理器。路由模式重复、前缀不对、授权级别与前缀不符时返回错误。
func NewRouter(opts Options, routes []httpx.Route) (http.Handler, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.BannedWritable == nil {
		opts.BannedWritable = DefaultBannedWritable
	}
	if opts.Users == nil || opts.Admins == nil {
		return nil, fmt.Errorf("api: 缺少会话校验器")
	}
	mux := http.NewServeMux()
	seen := map[string]bool{}
	for _, rt := range routes {
		key := rt.Method + " " + rt.Pattern
		if seen[key] {
			return nil, fmt.Errorf("api: 路由重复: %s", key)
		}
		seen[key] = true
		if rt.Handler == nil {
			return nil, fmt.Errorf("api: %s 缺少处理器", key)
		}
		switch {
		case strings.HasPrefix(rt.Pattern, "/api/"):
			if rt.Auth == httpx.Admin {
				return nil, fmt.Errorf("api: 用户端路由 %s 不能要求管理员", key)
			}
		case strings.HasPrefix(rt.Pattern, "/admin-api/"):
			if rt.Auth != httpx.Admin && rt.Auth != httpx.Public {
				return nil, fmt.Errorf("api: 管理端路由 %s 只能是 Admin 或 Public", key)
			}
		default:
			return nil, fmt.Errorf("api: 路由 %s 必须以 /api/ 或 /admin-api/ 开头", key)
		}
		mux.Handle(key, &routeHandler{opts: &opts, route: rt, key: key})
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if opts.Health != nil {
			if err := opts.Health(r.Context()); err != nil {
				opts.Logger.Error("api: 健康检查失败", "err", err)
				http.Error(w, "unhealthy", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	notFound := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		opts.Cookies.EnsureCSRF(w, r)
		httpx.WriteError(w, r, httpx.NotFound("接口不存在"))
	}
	mux.HandleFunc("/api/", notFound)
	mux.HandleFunc("/admin-api/", notFound)
	return mux, nil
}

type routeHandler struct {
	opts  *Options
	route httpx.Route
	key   string
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o := h.opts
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			o.Logger.Error("api: 处理器 panic", "route", h.key, "panic", p, "stack", string(debug.Stack()))
			httpx.WriteError(w, r, fmt.Errorf("panic: %v", p))
		}
	}()
	ctx := r.Context()
	ip := httpx.ClientIP(r)
	reqID := newRequestID()
	ctx = httpx.WithClientIP(ctx, ip)
	ctx = audit.WithRequest(ctx, audit.RequestInfo{IP: ip, UserAgent: r.UserAgent(), RequestID: reqID})
	w.Header().Set("X-Request-ID", reqID)
	w.Header().Set("Cache-Control", "no-store")
	o.Cookies.EnsureCSRF(w, r)
	r = r.WithContext(ctx)

	safe := session.SafeMethod(r.Method)
	if !safe && !session.CheckCSRF(r) {
		httpx.WriteError(w, r, httpx.Forbidden("页面已过期或请求来源不可信，请刷新页面后重试"))
		return
	}

	if strings.HasPrefix(h.route.Pattern, "/admin-api/") {
		ok, role := false, ""
		if tok := session.TokenFrom(r, session.AdminCookie); tok != "" {
			c2, rl, valid, err := o.Admins.AuthenticateAdmin(ctx, tok)
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			if valid {
				ctx, ok, role = c2, true, rl
			}
		}
		if h.route.Auth == httpx.Admin {
			if !ok {
				httpx.WriteError(w, r, httpx.Unauthorized())
				return
			}
			if role == RoleViewer && !safe && !o.ViewerWritable[h.key] {
				if o.Audit != nil {
					if err := audit.Record(ctx, o.Audit, audit.Entry{Action: "admin.write_denied", Outcome: audit.Denied,
						Details: map[string]any{"route": h.key, "path": r.URL.Path}}); err != nil {
						o.Logger.Error("api: 记录审计失败", "err", err)
					}
				}
				httpx.WriteError(w, r, httpx.Forbidden("只读管理员不能执行该操作"))
				return
			}
		}
	} else {
		ok := false
		if tok := session.TokenFrom(r, session.UserCookie); tok != "" {
			c2, valid, err := o.Users.AuthenticateUser(ctx, tok)
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			if valid {
				ctx, ok = c2, true
			}
		}
		if h.route.Auth == httpx.User || h.route.Auth == httpx.VerifiedUser {
			if !ok {
				httpx.WriteError(w, r, httpx.Unauthorized())
				return
			}
			p, _ := httpx.UserFrom(ctx)
			if p.Banned && !safe && !o.BannedWritable[h.key] {
				httpx.WriteError(w, r, httpx.Precondition("账号已被封禁，只能查看，不能进行该操作"))
				return
			}
			if h.route.Auth == httpx.VerifiedUser && !p.Verified {
				httpx.WriteError(w, r, httpx.Unverified())
				return
			}
		}
	}
	h.route.Handler(w, r.WithContext(ctx))
}
