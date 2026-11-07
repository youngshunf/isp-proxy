// Package store 是控制面的 PostgreSQL 访问层：连接池、内嵌迁移与对外 ID 生成。
//
// schema 见 migrations/0001_init.sql（docs/04 §7.4）。数据库约束是业务不变量的最后一道防线：
// 账本唯一键、每订阅一个进行中任务、一个未完成换出口申请、权益合计只能由账本改变、
// used_bytes_total 只增不减等都在库里强制执行，服务层仍需先行校验并返回友好错误。
package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open 按连接串建立连接池并确认可连通。连接串通常来自 ISP_DATABASE_URL（docs/06 §2）。
// 会话时区固定为 UTC，与“时间以 UTC 存储”的约定一致。
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: 解析连接串: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	if cfg.ConnConfig.RuntimeParams["application_name"] == "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = "isp-control"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: 建立连接池: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: 连接数据库: %w", err)
	}
	return pool, nil
}

// InTx 在一个事务中执行 fn；fn 返回错误或 panic 时回滚，否则提交。
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// idAlphabet 是 Crockford base32 小写字母表（去掉 i l o u），与 SQL 函数 isp_new_id 一致。
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// idLen 个字符 × 5 位 = 100 位随机（docs/06 §4 要求至少 80 位）。
const idLen = 20

// NewID 生成带前缀的对外 ID，例如 NewID("sub") → "sub_8x2k…"。格式与库内 isp_new_id 相同，
// 满足各表的 isp_id_ok 检查约束。
func NewID(prefix string) string {
	var b [idLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: crypto/rand 不可用: " + err.Error())
	}
	out := make([]byte, 0, len(prefix)+1+idLen)
	out = append(out, prefix...)
	out = append(out, '_')
	for _, c := range b {
		out = append(out, idAlphabet[c&31])
	}
	return string(out)
}

// PostgreSQL 错误码（https://www.postgresql.org/docs/16/errcodes-appendix.html）。
const (
	CodeUniqueViolation     = "23505"
	CodeCheckViolation      = "23514"
	CodeForeignKeyViolation = "23503"
	CodeRestrictViolation   = "23001"
	CodeSerialization       = "40001"
)

// PgCode 返回错误链中 PostgreSQL 错误的 SQLSTATE；不是数据库错误时返回空串。
func PgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ConstraintName 返回违反的约束或索引名（如 tasks_one_in_progress），便于服务层映射为 conflict。
func ConstraintName(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

// IsUniqueViolation 报告 err 是否为唯一约束冲突；name 非空时还要求约束名匹配。
func IsUniqueViolation(err error, name string) bool {
	return PgCode(err) == CodeUniqueViolation && (name == "" || ConstraintName(err) == name)
}
