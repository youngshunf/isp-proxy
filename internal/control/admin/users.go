package admin

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-gost/gost/internal/control/account"
	"github.com/go-gost/gost/internal/control/audit"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
)

// Ref 对应 openapi AdminRef / UserRef。
type Ref struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// User 对应 openapi AdminUser。不含任何密码信息，只给出是否已设置（R-12 用户管理）。
type User struct {
	ID                string     `json:"id"`
	Email             string     `json:"email"`
	Verified          bool       `json:"verified"`
	Status            string     `json:"status"`
	PasswordSet       bool       `json:"passwordSet"`
	BannedAt          *time.Time `json:"bannedAt"`
	BannedReason      *string    `json:"bannedReason"`
	CreatedAt         time.Time  `json:"createdAt"`
	CreatedByAdmin    *Ref       `json:"createdByAdmin"`
	SubscriptionCount int        `json:"subscriptionCount"`
	OpenOrderCount    int        `json:"openOrderCount"`
	LastLoginAt       *time.Time `json:"lastLoginAt"`
}

// UserDetail 对应 openapi AdminUserDetail。
type UserDetail struct {
	User
	Subscriptions  any `json:"subscriptions"`
	RecentOrders   any `json:"recentOrders"`
	Direct         any `json:"direct"`
	ActiveSessions int `json:"activeSessions"`
}

const userSelect = `
SELECT u.id, u.email, u.email_verified_at IS NOT NULL, u.status, u.password_hash IS NOT NULL, u.banned_at, u.banned_reason,
       u.created_at, ca.id, ca.email,
       (SELECT count(*) FROM subscriptions s WHERE s.user_id = u.id AND s.lifecycle <> 'deleted'),
       (SELECT count(*) FROM orders o WHERE o.user_id = u.id
           AND (o.payment_status = 'awaiting'
                OR (o.payment_status = 'confirmed' AND o.fulfillment_status IN ('not_started', 'processing', 'failed')))),
       (SELECT max(se.created_at) FROM sessions se WHERE se.kind = 'user' AND se.user_id = u.id)
  FROM users u LEFT JOIN admins ca ON ca.id = u.created_by_admin`

func scanAdminUser(row pgx.Row) (*User, error) {
	var u User
	var caID, caEmail *string
	if err := row.Scan(&u.ID, &u.Email, &u.Verified, &u.Status, &u.PasswordSet, &u.BannedAt, &u.BannedReason,
		&u.CreatedAt, &caID, &caEmail, &u.SubscriptionCount, &u.OpenOrderCount, &u.LastLoginAt); err != nil {
		return nil, err
	}
	if caID != nil {
		u.CreatedByAdmin = &Ref{ID: *caID, Email: *caEmail}
	}
	u.CreatedAt = u.CreatedAt.UTC()
	for _, t := range []**time.Time{&u.BannedAt, &u.LastLoginAt} {
		if *t != nil {
			v := (*t).UTC()
			*t = &v
		}
	}
	return &u, nil
}

func getUser(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (*User, error) {
	u, err := scanAdminUser(q.QueryRow(ctx, userSelect+` WHERE u.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("找不到该用户")
	}
	return u, err
}

// ParseLimit 解析分页 limit（1～200，默认 50）。
func ParseLimit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 200 {
		return 0, httpx.Invalid("limit 必须是 1～200 的整数")
	}
	return n, nil
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixMicro(), 10) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err == nil {
		if ts, id, ok := strings.Cut(string(b), "|"); ok {
			if us, err := strconv.ParseInt(ts, 10, 64); err == nil && id != "" {
				return time.UnixMicro(us).UTC(), id, nil
			}
		}
	}
	return time.Time{}, "", httpx.Invalid("cursor 无效")
}

func (s *Service) handleListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := ParseLimit(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if term := strings.TrimSpace(q.Get("q")); term != "" {
		p := arg(term)
		where = append(where, "(u.id = "+p+" OR strpos(u.email, lower("+p+")) > 0)")
	}
	switch q.Get("status") {
	case "":
	case "unverified":
		where = append(where, "u.status = 'active' AND u.email_verified_at IS NULL")
	case "active":
		where = append(where, "u.status = 'active' AND u.email_verified_at IS NOT NULL")
	case "banned":
		where = append(where, "u.status = 'banned'")
	default:
		httpx.WriteError(w, r, httpx.Invalid("status 只能是 unverified、active 或 banned"))
		return
	}
	if c := q.Get("cursor"); c != "" {
		t, id, err := decodeCursor(c)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		where = append(where, "(u.created_at, u.id) < ("+arg(t)+", "+arg(id)+")")
	}
	sql := userSelect
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY u.created_at DESC, u.id DESC LIMIT " + arg(limit+1)
	rows, err := s.pool.Query(r.Context(), sql, args...)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	defer rows.Close()
	items := []*User{}
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		items = append(items, u)
	}
	if err := rows.Err(); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		c := encodeCursor(items[limit-1].CreatedAt, items[limit-1].ID)
		next = &c
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (s *Service) handleGetUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("userId")
	u, err := getUser(ctx, s.pool, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d := UserDetail{User: *u, Subscriptions: []any{}, RecentOrders: []any{}}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE kind = 'user' AND user_id = $1 AND revoked_at IS NULL AND expires_at > $2`,
		id, s.now()).Scan(&d.ActiveSessions); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if f := s.cfg.Hooks.UserSubscriptions; f != nil {
		if d.Subscriptions, err = f(ctx, id); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	if f := s.cfg.Hooks.UserRecentOrders; f != nil {
		if d.RecentOrders, err = f(ctx, id); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	if f := s.cfg.Hooks.UserDirect; f != nil {
		if d.Direct, err = f(ctx, id); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (s *Service) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	a, _ := httpx.AdminFrom(ctx)
	var u *User
	err := store.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		id, err := s.cfg.Account.CreateByAdmin(ctx, tx, in.Email, a.AdminID)
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "user.create", TargetType: "user", TargetID: id}); err != nil {
			return err
		}
		u, err = getUser(ctx, tx, id)
		return err
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.cfg.Account.KickMail()
	httpx.WriteJSON(w, http.StatusCreated, u)
}

func (s *Service) handleBan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if n := utf8.RuneCountInString(in.Reason); n < 1 || n > 500 {
		httpx.WriteError(w, r, httpx.Invalid("请填写封禁原因（最多 500 字）"))
		return
	}
	s.setBanned(w, r, true, in.Reason)
}

func (s *Service) handleUnban(w http.ResponseWriter, r *http.Request) {
	s.setBanned(w, r, false, "")
}

// setBanned 封禁：撤销该用户全部会话并让其订阅生成新期望版本（Hooks.UserStatusChanged）；不改变权益与期限。
// 封禁后用户仍可登录查看（只读，docs/06 §8.2）。
func (s *Service) setBanned(w http.ResponseWriter, r *http.Request, ban bool, reason string) {
	ctx := r.Context()
	a, _ := httpx.AdminFrom(ctx)
	id := r.PathValue("userId")
	var u *User
	err := store.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.now()
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("找不到该用户")
		}
		if err != nil {
			return err
		}
		changed := (status == "banned") != ban
		if ban {
			if changed {
				if _, err := tx.Exec(ctx, `UPDATE users SET status = 'banned', banned_at = $2, banned_reason = $3, banned_by = $4 WHERE id = $1`,
					id, now, reason, a.AdminID); err != nil {
					return err
				}
			}
			// 重复封禁也再撤销一次会话，保证没有遗留。
			if err := account.RevokeSessions(ctx, tx, now, id, "banned", ""); err != nil {
				return err
			}
		} else if changed {
			if _, err := tx.Exec(ctx, `UPDATE users SET status = 'active', banned_at = NULL, banned_reason = NULL, banned_by = NULL WHERE id = $1`, id); err != nil {
				return err
			}
		}
		if changed && s.cfg.Hooks.UserStatusChanged != nil {
			if err := s.cfg.Hooks.UserStatusChanged(ctx, tx, id); err != nil {
				return err
			}
		}
		action, details := "user.unban", map[string]any{"changed": changed}
		if ban {
			action = "user.ban"
			details["reason"] = reason
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: action, TargetType: "user", TargetID: id, Details: details}); err != nil {
			return err
		}
		u, err = getUser(ctx, tx, id)
		return err
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (s *Service) handleSendReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("userId")
	err := store.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.cfg.Account.SendResetByAdmin(ctx, tx, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.password_reset_email", TargetType: "user", TargetID: id})
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.cfg.Account.KickMail()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := ParseLimit(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := audit.Filter{ActorKind: q.Get("actorKind"), AdminID: q.Get("adminId"), UserID: q.Get("userId"), Action: q.Get("action"),
		TargetType: q.Get("targetType"), TargetID: q.Get("targetId"), Cursor: q.Get("cursor"), Limit: limit}
	switch f.ActorKind {
	case "", audit.ActorAdmin, audit.ActorUser, audit.ActorSystem, audit.ActorNode:
	default:
		httpx.WriteError(w, r, httpx.Invalid("actorKind 不正确"))
		return
	}
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if v := q.Get(p.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				httpx.WriteError(w, r, httpx.Invalid(p.name+" 必须是 RFC 3339 时间"))
				return
			}
			*p.dst = t
		}
	}
	page, err := audit.List(r.Context(), s.pool, f)
	if errors.Is(err, audit.ErrBadCursor) {
		err = httpx.Invalid("cursor 无效")
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}
