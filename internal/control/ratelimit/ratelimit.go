// Package ratelimit 用 rate_events 表做滑动窗口限流（D-15：发信、登录、注册、找回、验证尝试）。
//
// 每条规则是“scope 下同一 key 在 Window 内最多 Limit 次”。限流计数在库里，多进程共享，重启不丢；
// 检查与记录在同一事务内用咨询锁串行化（同一 scope+key），避免并发请求同时越过上限。
// 过期事件由 Prune 清理（调度定期调用）。
package ratelimit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB 是 pgx.Tx 的子集。
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Rule 是一条限流规则。
type Rule struct {
	Scope  string
	Limit  int
	Window time.Duration
}

// lock 对 scope+key 取事务级咨询锁（db 必须是事务，否则锁立即释放，只是退化为非原子检查）。
func lock(ctx context.Context, db DB, scope, key string) error {
	_, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "rl:"+scope+":"+key)
	return err
}

// Check 返回 key 在规则下还需等待的时间；0 表示允许。不记录事件。
func Check(ctx context.Context, db DB, now time.Time, key string, rules ...Rule) (time.Duration, error) {
	var wait time.Duration
	for _, r := range rules {
		if err := lock(ctx, db, r.Scope, key); err != nil {
			return 0, err
		}
		var n int
		var oldest *time.Time
		// 取窗口内第 Limit 新的事件：它离开窗口时才再次允许。
		err := db.QueryRow(ctx, `
			SELECT count(*) OVER (), at FROM rate_events
			 WHERE scope = $1 AND key = $2 AND at > $3
			 ORDER BY at DESC OFFSET $4 - 1 LIMIT 1`,
			r.Scope, key, now.Add(-r.Window), r.Limit).Scan(&n, &oldest)
		if err == pgx.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, err
		}
		if oldest != nil {
			if w := oldest.Add(r.Window).Sub(now); w > wait {
				wait = w
			}
		}
	}
	if wait < 0 {
		wait = 0
	}
	return wait, nil
}

// Hit 为 key 在每条规则的 scope 下记录一次事件。
func Hit(ctx context.Context, db DB, now time.Time, key string, rules ...Rule) error {
	seen := map[string]bool{}
	for _, r := range rules {
		if seen[r.Scope] {
			continue
		}
		seen[r.Scope] = true
		if _, err := db.Exec(ctx, `INSERT INTO rate_events (scope, key, at) VALUES ($1, $2, $3)`, r.Scope, key, now); err != nil {
			return err
		}
	}
	return nil
}

// Take 先 Check，允许时再 Hit；返回需要等待的时间（>0 表示被限流，此时不记录）。
func Take(ctx context.Context, db DB, now time.Time, key string, rules ...Rule) (time.Duration, error) {
	wait, err := Check(ctx, db, now, key, rules...)
	if err != nil || wait > 0 {
		return wait, err
	}
	return 0, Hit(ctx, db, now, key, rules...)
}

// Reset 删除 key 在 scope 下的全部事件（例如登录成功后清空该邮箱的失败计数）。
func Reset(ctx context.Context, db DB, scope, key string) error {
	_, err := db.Exec(ctx, `DELETE FROM rate_events WHERE scope = $1 AND key = $2`, scope, key)
	return err
}

// Prune 删除早于 before 的事件，返回删除条数。
func Prune(ctx context.Context, db DB, before time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM rate_events WHERE at < $1`, before)
	return tag.RowsAffected(), err
}
