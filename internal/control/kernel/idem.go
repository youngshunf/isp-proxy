package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
)

// Idempotent 以幂等键执行一次写操作（docs/06 §3）。
//
// 幂等键与业务写入在同一事务：首个请求插入 in_progress 行并在同一事务内写入响应；并发的同键请求
// 在插入时等待前者提交，随后读到已完成的响应原样返回（不再执行 fn）；前者回滚则后者正常执行。
// 键相同但请求体不同返回 409 conflict。scope 为“主体 + 操作”，如 "user:usr_…:create_order"。
func Idempotent[T any](ctx context.Context, env *Env, scope, key string, req any, fn func(tx *Tx) (T, error)) (T, error) {
	var out T
	if len(key) < 8 || len(key) > 128 {
		return out, httpx.Invalid("缺少或不合法的 Idempotency-Key（8～128 个字符）")
	}
	hash := HashJSON(req)
	err := env.InTx(ctx, func(tx *Tx) error {
		var zero T
		out = zero
		tag, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (scope, key, request_hash, created_at, expires_at)
		                           VALUES ($1, $2, $3, $4, $5) ON CONFLICT (scope, key) DO NOTHING`,
			scope, key, hash, tx.Now(), tx.Now().Add(24*time.Hour))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var prevHash []byte
			var state string
			var resp []byte
			if err := tx.QueryRow(ctx, `SELECT request_hash, state, response FROM idempotency_keys WHERE scope = $1 AND key = $2`,
				scope, key).Scan(&prevHash, &state, &resp); err != nil {
				return err
			}
			if !bytes.Equal(prevHash, hash) {
				return httpx.Conflict("同一个 Idempotency-Key 不能用于不同的请求")
			}
			if state != "done" {
				return httpx.Conflict("相同的请求正在处理，请稍后刷新")
			}
			return json.Unmarshal(resp, &out)
		}
		v, err := fn(tx)
		if err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE idempotency_keys SET state = 'done', status_code = 200, response = $3
		                            WHERE scope = $1 AND key = $2`, scope, key, b); err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}
