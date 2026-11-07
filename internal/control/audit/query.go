package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier 是 pgx.Tx、*pgxpool.Pool 的查询子集。
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Filter 是审计查询条件（admin-api GET /audit-logs）。Action 以 “.” 结尾时按前缀匹配，否则精确匹配。
type Filter struct {
	ActorKind, AdminID, UserID, Action, TargetType, TargetID string
	From, To                                                 time.Time
	Cursor                                                   string // 上一页的 NextCursor
	Limit                                                    int    // 1～200，默认 50
}

// Actor 对应 openapi Actor。
type Actor struct {
	Kind  string  `json:"kind"`
	ID    *string `json:"id"`
	Email string  `json:"email,omitempty"`
}

// Log 对应 openapi AuditLog。
type Log struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	Actor      Actor           `json:"actor"`
	Action     string          `json:"action"`
	TargetType *string         `json:"targetType"`
	TargetID   *string         `json:"targetId"`
	Outcome    string          `json:"outcome"`
	IP         *string         `json:"ip"`
	UserAgent  *string         `json:"userAgent"`
	Details    json.RawMessage `json:"details"`
}

// Page 对应 openapi AuditLogPage。
type Page struct {
	Items      []Log   `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// ErrBadCursor 表示游标无法解析。
var ErrBadCursor = fmt.Errorf("audit: 游标无效")

// List 按条件查询审计，新的在前；游标为上一页最后一条的 ID。
func List(ctx context.Context, q Querier, f Filter) (*Page, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.ActorKind != "" {
		add("l.actor_kind = ?", f.ActorKind)
	}
	if f.AdminID != "" {
		add("l.admin_id = ?", f.AdminID)
	}
	if f.UserID != "" {
		add("l.user_id = ?", f.UserID)
	}
	if f.Action != "" {
		if strings.HasSuffix(f.Action, ".") {
			add("starts_with(l.action, ?)", f.Action)
		} else {
			add("l.action = ?", f.Action)
		}
	}
	if f.TargetType != "" {
		add("l.target_type = ?", f.TargetType)
	}
	if f.TargetID != "" {
		add("l.target_id = ?", f.TargetID)
	}
	if !f.From.IsZero() {
		add("l.at >= ?", f.From)
	}
	if !f.To.IsZero() {
		add("l.at < ?", f.To)
	}
	if f.Cursor != "" {
		id, err := strconv.ParseInt(f.Cursor, 10, 64)
		if err != nil || id <= 0 {
			return nil, ErrBadCursor
		}
		add("l.id < ?", id)
	}
	sql := `SELECT l.id, l.at, l.actor_kind, l.admin_id, l.user_id, l.node_id, coalesce(a.email, u.email, ''),
	               l.action, l.target_type, l.target_id, l.outcome, host(l.ip), l.user_agent, l.details
	          FROM audit_logs l
	          LEFT JOIN admins a ON a.id = l.admin_id
	          LEFT JOIN users u ON u.id = l.user_id AND l.admin_id IS NULL`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit+1)
	sql += " ORDER BY l.id DESC LIMIT $" + strconv.Itoa(len(args))
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &Page{Items: []Log{}}
	for rows.Next() {
		var l Log
		var adminID, userID, nodeID *string
		var email string
		if err := rows.Scan(&l.ID, &l.At, &l.Actor.Kind, &adminID, &userID, &nodeID, &email,
			&l.Action, &l.TargetType, &l.TargetID, &l.Outcome, &l.IP, &l.UserAgent, &l.Details); err != nil {
			return nil, err
		}
		switch l.Actor.Kind {
		case ActorAdmin:
			l.Actor.ID = adminID
		case ActorUser:
			l.Actor.ID = userID
		case ActorNode:
			l.Actor.ID = nodeID
		}
		l.Actor.Email = email
		l.At = l.At.UTC()
		page.Items = append(page.Items, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		c := strconv.FormatInt(page.Items[limit-1].ID, 10)
		page.NextCursor = &c
	}
	return page, nil
}
