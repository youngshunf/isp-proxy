package httpx

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// AuthLevel 是路由要求的身份。由 internal/control/api 的路由器在调用处理器前检查。
type AuthLevel int

const (
	// Public 不要求登录。用户端（/api/）路由若带有效会话，路由器仍会注入 Principal（例如 verify-email）。
	Public AuthLevel = iota
	// User 要求有效的用户会话（未验证邮箱也可以）。封禁用户只读：非 GET 返回 412 precondition。
	User
	// VerifiedUser 在 User 之上要求邮箱已验证，否则 403 unverified（购买、获取连接等，R-15.1）。
	VerifiedUser
	// Admin 要求已完成 TOTP 的管理员会话。viewer 角色的非 GET 返回 403 forbidden。
	Admin
)

func (l AuthLevel) String() string {
	switch l {
	case Public:
		return "public"
	case User:
		return "user"
	case VerifiedUser:
		return "verified_user"
	case Admin:
		return "admin"
	}
	return "unknown"
}

// Route 是一个 HTTP 操作。Pattern 与 api/openapi.yaml 中的路径完全一致，例如 "/api/subscriptions/{subscriptionId}"；
// 处理器用 r.PathValue("subscriptionId") 取参数。
type Route struct {
	Method  string
	Pattern string
	Auth    AuthLevel
	Handler http.HandlerFunc
}

// Principal 是当前用户。Banned 的用户仍可读（docs/06 §8.2）。
type Principal struct {
	UserID, Email    string
	Verified, Banned bool
}

// AdminPrincipal 是当前管理员（已完成 TOTP）。
type AdminPrincipal struct {
	AdminID, Email string
}

type ctxKey int

const (
	userKey ctxKey = iota
	adminKey
	clientIPKey
)

// UserFrom 返回请求上下文中的用户。
func UserFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(userKey).(Principal)
	return p, ok
}

// AdminFrom 返回请求上下文中的管理员。
func AdminFrom(ctx context.Context) (AdminPrincipal, bool) {
	a, ok := ctx.Value(adminKey).(AdminPrincipal)
	return a, ok
}

// WithUser 把用户放入上下文（路由器与测试使用）。
func WithUser(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, userKey, p)
}

// WithAdmin 把管理员放入上下文（路由器与测试使用）。
func WithAdmin(ctx context.Context, a AdminPrincipal) context.Context {
	return context.WithValue(ctx, adminKey, a)
}

// IdempotencyKey 返回 Idempotency-Key 头（去掉首尾空白）；没有时为空串。
// 长度 8～128 的校验由使用方按契约完成（缺少或不合法返回 Invalid）。
func IdempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

var (
	proxyMu        sync.RWMutex
	trustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
)

// SetTrustedProxies 设置可信反向代理网段（默认只信任本机回环）。只有直连地址落在其中时才读取 X-Forwarded-For。
func SetTrustedProxies(prefixes []netip.Prefix) {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	trustedProxies = append([]netip.Prefix(nil), prefixes...)
}

func trusted(a netip.Addr) bool {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	a = a.Unmap()
	for _, p := range trustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// WithClientIP 把已计算的来源 IP 放入上下文（路由器使用），之后 ClientIP 直接返回该值。
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

// ClientIP 返回请求的来源 IP（不含端口）。直连地址是可信代理时，从 X-Forwarded-For 自右向左取第一个不可信地址；
// 否则用直连地址。用于限流与审计，不作为身份依据。
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey).(string); ok && ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	remote = remote.Unmap()
	if !trusted(remote) {
		return remote.String()
	}
	xff := r.Header.Values("X-Forwarded-For")
	var parts []string
	for _, v := range xff {
		parts = append(parts, strings.Split(v, ",")...)
	}
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !trusted(a) {
			return a.String()
		}
	}
	return remote.String()
}
