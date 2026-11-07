package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/go-gost/gost/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 是一个迁移文件。
type Migration struct {
	Version  int    // 文件名前缀 NNNN
	Name     string // 完整文件名
	Checksum string // SQL 内容的 SHA-256（十六进制）
	SQL      string
}

var migrationName = regexp.MustCompile(`^(\d{4})_[^/]+\.sql$`)

// LoadMigrations 读取 fsys 根目录下的 NNNN_*.sql，按版本排序；版本重复时报错。
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("store: 读取迁移目录: %w", err)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v == 0 {
			return nil, fmt.Errorf("store: 迁移版本必须从 0001 开始: %s", e.Name())
		}
		if prev, ok := seen[v]; ok {
			return nil, fmt.Errorf("store: 迁移版本 %04d 重复: %s 与 %s", v, prev, e.Name())
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{Version: v, Name: e.Name(), Checksum: hex.EncodeToString(sum[:]), SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// migrateLockKey 是迁移用的事务级咨询锁键，保证多个进程同时启动时串行执行。
const migrateLockKey int64 = 0x69737063_6d696772 // "ispc" "migr"

const bootstrapSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    int PRIMARY KEY,
    name       text NOT NULL,
    checksum   text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// Migrate 执行内嵌的 migrations.FS，返回本次新执行的迁移。见 MigrateFS。
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]Migration, error) {
	return MigrateFS(ctx, pool, migrations.FS)
}

// MigrateFS 按版本顺序执行 fsys 中尚未执行的迁移，每个文件一个事务，并记入 schema_migrations。
//
// 只前进：已执行的迁移若内容（校验和）变化，或库里存在本程序不认识的更高版本，都直接报错，
// 不尝试修复（docs/01 §9：迁移失败不以删库重建解决）。重复执行是空操作。
func MigrateFS(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]Migration, error) {
	ms, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	known := map[int]Migration{}
	for _, m := range ms {
		known[m.Version] = m
	}
	err = InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrateLockKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, bootstrapSQL); err != nil {
			return fmt.Errorf("创建 schema_migrations: %w", err)
		}
		return checkApplied(ctx, tx, known)
	})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	var applied []Migration
	for _, m := range ms {
		ran := false
		err := InTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrateLockKey); err != nil {
				return err
			}
			if err := checkApplied(ctx, tx, known); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", m.Version).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return nil
			}
			// 无参数的 Exec 走简单查询协议，允许一个文件包含多条语句。
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return fmt.Errorf("执行 %s: %w", m.Name, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
				m.Version, m.Name, m.Checksum); err != nil {
				return err
			}
			ran = true
			return nil
		})
		if err != nil {
			return applied, fmt.Errorf("store: 迁移 %04d: %w", m.Version, err)
		}
		if ran {
			applied = append(applied, m)
		}
	}
	return applied, nil
}

// checkApplied 校验库里已记录的迁移与本程序内嵌的一致。
func checkApplied(ctx context.Context, tx pgx.Tx, known map[int]Migration) error {
	rows, err := tx.Query(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var name, sum string
		if err := rows.Scan(&v, &name, &sum); err != nil {
			return err
		}
		m, ok := known[v]
		if !ok {
			return fmt.Errorf("数据库已执行迁移 %04d（%s），但本程序没有该文件：程序版本过旧", v, name)
		}
		if m.Checksum != sum {
			return fmt.Errorf("迁移 %s 在执行后被修改（校验和不一致）；迁移只前进，改动请写新文件", m.Name)
		}
	}
	return rows.Err()
}

// AppliedVersions 返回已执行的迁移版本（升序）。
func AppliedVersions(ctx context.Context, pool *pgxpool.Pool) ([]int, error) {
	rows, err := pool.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}
