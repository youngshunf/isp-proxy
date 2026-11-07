// Package inventory 是库存与端口池（C-W6，R-03、R-04.6、R-16.3、D-13、D-18）：
// 节点登记与端口池预填、端口分配/迁移/冷却、上游资源录入与七种库存状态、库存预留与平台内独占。
package inventory

import (
	"context"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
)

// PickPorts 在节点上锁定 3 个空闲端口（跳过冷却与已分配），按端口号从小到大。
// 并发分配用 FOR UPDATE SKIP LOCKED，不会把同一端口分给两个接入版本。
func PickPorts(ctx context.Context, tx *kernel.Tx, node string) ([3]int, error) {
	var out [3]int
	rows, err := tx.Query(ctx, `SELECT port FROM ports WHERE node_id = $1 AND state = 'free'
	                             ORDER BY port LIMIT 3 FOR UPDATE SKIP LOCKED`, node)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := rows.Scan(&out[n]); err != nil {
			return out, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if n < 3 {
		return out, ErrNoPorts
	}
	return out, nil
}

// ErrNoPorts 表示节点端口池不足。
var ErrNoPorts = httpx.Conflict("节点端口池不足，请扩充端口范围或等待冷却结束")

// AssignPorts 把端口登记到接入版本（接入版本行须已插入）。
func AssignPorts(ctx context.Context, tx *kernel.Tx, node string, ports [3]int, access string) error {
	_, err := tx.Exec(ctx, `UPDATE ports SET state = 'allocated', access_version_id = $3, cooldown_until = NULL
	                         WHERE node_id = $1 AND port = ANY($2)`, node, ports[:], access)
	return err
}

// MovePorts 把端口从旧接入版本迁到新接入版本（重置密码：端口不变）。
func MovePorts(ctx context.Context, tx *kernel.Tx, from, to string) error {
	_, err := tx.Exec(ctx, `UPDATE ports SET access_version_id = $2 WHERE access_version_id = $1`, from, to)
	return err
}

// CooldownPorts 释放接入版本的端口并进入冷却（D-18）；冷却期内不分配给任何订阅。
func CooldownPorts(ctx context.Context, tx *kernel.Tx, access string, until time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE ports SET state = 'cooldown', access_version_id = NULL, cooldown_until = $2
	                         WHERE access_version_id = $1`, access, until)
	return err
}

// FreePorts 直接释放从未生效的接入版本的端口（下发失败回滚）。
func FreePorts(ctx context.Context, tx *kernel.Tx, access string) error {
	_, err := tx.Exec(ctx, `UPDATE ports SET state = 'free', access_version_id = NULL, cooldown_until = NULL
	                         WHERE access_version_id = $1`, access)
	return err
}

// EndPortCooldowns 把冷却到期的端口放回空闲（调度，可重入）。返回放回数量。
func EndPortCooldowns(ctx context.Context, q kernel.Q, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE ports SET state = 'free', cooldown_until = NULL WHERE state = 'cooldown' AND cooldown_until <= $1`, now)
	return tag.RowsAffected(), err
}
