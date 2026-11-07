// Package audit 写入与查询操作审计（audit_logs，只追加；R-12 审计、R-15.6、C-W8）。
//
// 敏感操作在业务事务内调用 Record，与业务写入同时提交或回滚：
//
//	err := store.InTx(ctx, pool, func(tx pgx.Tx) error {
//	    … 业务写入 …
//	    return audit.Record(ctx, tx, audit.Entry{Action: "order.confirm_payment", TargetType: "order", TargetID: id,
//	        Details: map[string]any{"amount": cents}})
//	})
//
// 操作者默认取自上下文（httpx.AdminFrom → admin，否则 httpx.UserFrom → user，否则 system）；
// 来源 IP、User-Agent、请求 ID 由路由器经 WithRequest 放入上下文。Details 不得包含明文凭据：
// 键名含 password / secret / token / code 等的值会被替换为 "[redacted]"。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execer 是 pgx.Tx、*pgxpool.Pool、*pgx.Conn 的公共子集。
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// 结果。
const (
	OK     = "ok"
	Denied = "denied"
	Failed = "failed"
)

// 操作者种类。
const (
	ActorAdmin  = "admin"
	ActorUser   = "user"
	ActorSystem = "system"
	ActorNode   = "node"
)

// Entry 是一条审计。Action 形如“对象.动作”（小写字母与下划线），如 subscription.pause、access.reveal、admin.login_failed。
type Entry struct {
	Action     string
	TargetType string
	TargetID   string
	Outcome    string         // 空为 ok
	Details    map[string]any // 可为 nil；不含明文凭据

	// 以下覆盖上下文推导的操作者，通常留空。例如登录失败时上下文里还没有管理员，可显式给出 AdminID（若邮箱对应某管理员）。
	ActorKind string
	AdminID   string
	UserID    string
	NodeID    string
}

// RequestInfo 是请求来源信息。
type RequestInfo struct {
	IP        string
	UserAgent string
	RequestID string
}

type ctxKey struct{}

// WithRequest 把请求来源放入上下文，供 Record 写入。
func WithRequest(ctx context.Context, info RequestInfo) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

// RequestFrom 返回上下文中的请求来源。
func RequestFrom(ctx context.Context) RequestInfo {
	info, _ := ctx.Value(ctxKey{}).(RequestInfo)
	return info
}

var actionPattern = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)

var sensitiveKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|code|otp|totp|cookie|credential|private)`)

// redact 递归替换敏感键的值。
func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			if sensitiveKey.MatchString(k) {
				out[k] = "[redacted]"
				continue
			}
			out[k] = redact(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = redact(vv)
		}
		return out
	}
	return v
}

// Record 写入一条审计。应在业务事务内调用（db 为 pgx.Tx）。
func Record(ctx context.Context, db Execer, e Entry) error {
	if !actionPattern.MatchString(e.Action) {
		return fmt.Errorf("audit: 动作名 %q 不符合“对象.动作”格式", e.Action)
	}
	outcome := e.Outcome
	if outcome == "" {
		outcome = OK
	}
	kind, adminID, userID, nodeID := e.ActorKind, e.AdminID, e.UserID, e.NodeID
	if kind == "" {
		switch {
		case adminID != "":
			kind = ActorAdmin
		case nodeID != "":
			kind = ActorNode
		case userID != "":
			kind = ActorUser
		default:
			if a, ok := httpx.AdminFrom(ctx); ok {
				kind, adminID = ActorAdmin, a.AdminID
			} else if p, ok := httpx.UserFrom(ctx); ok {
				kind, userID = ActorUser, p.UserID
			} else {
				kind = ActorSystem
			}
		}
	}
	details := map[string]any{}
	if e.Details != nil {
		details = redact(e.Details).(map[string]any)
	}
	b, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit: 编码 details: %w", err)
	}
	info := RequestFrom(ctx)
	var ip any
	if a, err := netip.ParseAddr(info.IP); err == nil {
		ip = a.String()
	}
	_, err = db.Exec(ctx, `INSERT INTO audit_logs
		(actor_kind, admin_id, user_id, node_id, action, target_type, target_id, outcome, ip, user_agent, request_id, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::inet, $10, $11, $12)`,
		kind, nullable(adminID), nullable(userID), nullable(nodeID), e.Action, nullable(e.TargetType), nullable(e.TargetID),
		outcome, ip, nullable(truncate(info.UserAgent, 512)), nullable(info.RequestID), b)
	if err != nil {
		return fmt.Errorf("audit: 写入 %s: %w", e.Action, err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
