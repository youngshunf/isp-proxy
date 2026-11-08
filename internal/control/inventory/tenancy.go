package inventory

// 出口名额（D-24，docs/06 §9.1，迁移 0005）。
//
// 独享订阅占用整份资源，沿用资源状态 reserved / bound / cooldown；共享订阅按名额占用：
// 待生效与生效的共享绑定（换出口任务的待生效绑定就是任务预留）+ resource_slots 名额行（订单预留、冷却）。
// 数据库触发器在锁住资源行后检查“共享合计 ≤ 上限、不与独享混用、上限为 1 只给独享、共享 IP 不与独享相同”；
// 这里的挑选先按条件筛出候选，逐个加锁后在新语句里重新核对，并发抢最后一个名额只有一个事务拿到。

import (
	"context"
	"time"

	"github.com/go-gost/gost/internal/control/alert"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
)

// 出口类型（plans.exit_mode、orders.exit_mode、subscriptions.exit_mode、exit_bindings.exit_mode）。
const (
	ModeExclusive = "exclusive"
	ModeShared    = "shared"
)

// ValidMode 报告 m 是否为合法的出口类型。
func ValidMode(m string) bool { return m == ModeExclusive || m == ModeShared }

// 触发器抛出的约束名（迁移 0005）。
const (
	consCapacity = "upstream_resources_shared_capacity"
	consMixed    = "upstream_resources_exit_mode_mixed"
)

// IsTenancyViolation 报告 err 是否为名额触发器拒绝（名额已满、独享与共享混用、上限为 1 的资源分给共享）。
func IsTenancyViolation(err error) bool {
	if store.PgCode(err) != store.CodeCheckViolation {
		return false
	}
	n := store.ConstraintName(err)
	return n == consCapacity || n == consMixed
}

// 可分给独享订阅：可售、节点可用、没有任何共享占用（含冷却名额）、出口 IP 未被任何生效/待生效绑定占用（D-13、R-11.3）。
// r 为 upstream_resources，n 为 nodes。
const exclusiveOK = `r.status = 'available' AND n.status = 'active' AND isp_shared_occupancy(r.id) = 0
	AND NOT EXISTS (SELECT 1 FROM exit_bindings b WHERE b.verified_ip = r.last_exit_ip AND b.state IN ('pending', 'active'))`

// 可分给共享订阅：可售、节点可用、上限 ≥ 2、没有独享占用、还有空位、出口 IP 不是任何独享订阅的出口（D-24）。
const sharedOK = `r.status = 'available' AND n.status = 'active' AND r.max_tenants >= 2 AND NOT isp_exclusive_occupied(r.id)
	AND isp_shared_occupancy(r.id) < r.max_tenants
	AND NOT EXISTS (SELECT 1 FROM exit_bindings b WHERE b.verified_ip = r.last_exit_ip AND b.state IN ('pending', 'active')
	                 AND b.exit_mode = 'exclusive')`

// recheck 在已锁住资源行之后，用新语句（新快照）核对资源是否仍满足 cond。
func recheck(ctx context.Context, tx *kernel.Tx, id, cond string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM upstream_resources r JOIN nodes n ON n.id = r.node_id
	                           WHERE r.id = $1 AND `+cond+`)`, id).Scan(&ok)
	return ok, err
}

// pick 在地域内挑一个可分配的资源并锁住资源行。无货返回 ErrNoStock。
//
// 独享：空闲资源中优先上限为 1 的，再随机（R-16.7）；SKIP LOCKED，被并发事务锁住的资源视为已被拿走（A-27）。
// 共享：有空位的共享资源按当前占用升序、同数随机；逐个等待加锁后重新核对，名额被并发占满就看下一个（A-47）。
func pick(ctx context.Context, tx *kernel.Tx, mode, country, city string) (string, error) {
	if mode == ModeShared {
		rows, err := tx.Query(ctx, `SELECT r.id FROM upstream_resources r JOIN nodes n ON n.id = r.node_id
		                             WHERE r.country_code = $1 AND r.city = $2 AND `+sharedOK+`
		                             ORDER BY isp_shared_occupancy(r.id), random() LIMIT 50`, country, city)
		if err != nil {
			return "", err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", err
		}
		for _, id := range ids {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM upstream_resources WHERE id = $1 FOR UPDATE`, id); err != nil {
				return "", err
			}
			ok, err := recheck(ctx, tx, id, sharedOK)
			if err != nil {
				return "", err
			}
			if ok {
				return id, nil
			}
		}
		return "", ErrNoStock
	}
	tried := []string{}
	for range 8 {
		var id string
		err := tx.QueryRow(ctx, `SELECT r.id FROM upstream_resources r JOIN nodes n ON n.id = r.node_id
		                          WHERE r.country_code = $1 AND r.city = $2 AND `+exclusiveOK+` AND NOT (r.id = ANY($3))
		                          ORDER BY (r.max_tenants = 1) DESC, random() LIMIT 1 FOR UPDATE OF r SKIP LOCKED`,
			country, city, tried).Scan(&id)
		if kernel.IsNoRows(err) {
			return "", ErrNoStock
		}
		if err != nil {
			return "", err
		}
		// 加锁前的快照可能没看到刚提交的共享名额：锁住后再核对一次。
		ok, err := recheck(ctx, tx, id, exclusiveOK)
		if err != nil {
			return "", err
		}
		if ok {
			return id, nil
		}
		tried = append(tried, id)
	}
	return "", ErrNoStock
}

// explicitShared 核对管理员指定的资源能否分给共享订阅（资源行已锁）。
func explicitShared(ctx context.Context, tx *kernel.Tx, id string) error {
	var status string
	var cap, occ int
	var excl, ipTaken bool
	if err := tx.QueryRow(ctx, `SELECT r.status, r.max_tenants, isp_shared_occupancy(r.id), isp_exclusive_occupied(r.id),
	        EXISTS (SELECT 1 FROM exit_bindings b WHERE b.verified_ip = r.last_exit_ip AND b.state IN ('pending', 'active')
	                 AND b.exit_mode = 'exclusive')
	      FROM upstream_resources r WHERE r.id = $1`, id).Scan(&status, &cap, &occ, &excl, &ipTaken); err != nil {
		return err
	}
	switch {
	case cap < 2:
		return httpx.Conflict("绑定上限为 1 的资源只给独享套餐")
	case excl:
		return httpx.Conflict("指定资源已被独享订阅占用")
	case status != "available":
		return httpx.Conflict("指定的资源不可分配（状态 " + status + "）")
	case occ >= cap:
		return httpx.Conflict("指定资源的共享名额已满")
	case ipTaken:
		return httpx.Conflict("指定资源的出口 IP 已被独享订阅占用")
	}
	return nil
}

// sharedTenantsAlertKey 是共享资源被隔离 / 停用时，资源上某个订阅的告警去重键。
func sharedTenantsAlertKey(res, sub string) string { return "shared_exit_down:" + res + ":" + sub }

// raiseSharedDown 为共享资源上每个订阅各自产生告警（R-11.10）：资源被隔离或停用，需要逐个换出口。
func raiseSharedDown(ctx context.Context, tx *kernel.Tx, res, status, reason string) error {
	label := map[string]string{"quarantine": "隔离", "retired": "停用"}[status]
	rows, err := tx.Query(ctx, `SELECT b.subscription_id, r.node_id FROM exit_bindings b JOIN upstream_resources r ON r.id = b.resource_id
	                             WHERE b.resource_id = $1 AND b.state IN ('pending', 'active') AND b.exit_mode = 'shared'
	                             ORDER BY b.subscription_id`, res)
	if err != nil {
		return err
	}
	type tenant struct{ sub, node string }
	var ts []tenant
	for rows.Next() {
		var t tenant
		if err := rows.Scan(&t.sub, &t.node); err != nil {
			rows.Close()
			return err
		}
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range ts {
		sub, node := t.sub, t.node
		if err := alert.Raise(ctx, tx, tx.Now(), alert.Alert{Kind: "upstream_error", Severity: "critical",
			Key:     sharedTenantsAlertKey(res, sub),
			Message: "订阅所在的共享出口资源已被" + label + "（" + reason + "），请为该订阅换出口",
			Details: map[string]any{"resourceStatus": status, "reason": reason, "shared": true},
			Node:    &node, Sub: &sub, Resource: &res}); err != nil {
			return err
		}
	}
	return nil
}

// resolveSharedDown 关闭资源上当前订阅的“共享资源被隔离 / 停用”告警（资源重新上架）。
func resolveSharedDown(ctx context.Context, tx *kernel.Tx, res string) error {
	_, err := tx.Exec(ctx, `UPDATE alerts SET status = 'resolved', resolved_at = $2
	                         WHERE dedupe_key LIKE $1 AND status <> 'resolved'`, "shared_exit_down:"+res+":%", tx.Now())
	return err
}

// ReleaseBinding 在出口绑定解除（换出口成功后的原绑定、保留期满释放、删除订阅）之后回收占用：
// 独享资源整份进入冷却（R-11.4）；共享资源腾出的名额冷却 until 之前不再分配，资源上其他订阅不受影响（D-24）。
func ReleaseBinding(ctx context.Context, tx *kernel.Tx, bindingID string, until time.Time) error {
	var res, mode, sub string
	if err := tx.QueryRow(ctx, `SELECT resource_id, exit_mode, subscription_id FROM exit_bindings WHERE id = $1`, bindingID).
		Scan(&res, &mode, &sub); err != nil {
		return err
	}
	if mode != ModeShared {
		return Cooldown(ctx, tx, res, until)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO resource_slots (id, resource_id, kind, subscription_id, until, created_at)
	                           VALUES ($1, $2, 'cooldown', $3, $4, $5)`, store.NewID("slt"), res, sub, until, tx.Now()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE upstream_resources SET row_version = row_version + 1 WHERE id = $1`, res); err != nil {
		return err
	}
	return alert.Resolve(ctx, tx, tx.Now(), sharedTenantsAlertKey(res, sub))
}

// ReturnCandidate 处理作废（failed）的待生效绑定的资源：独享资源退回可售或隔离（ReturnToStock）；
// 共享资源的名额随绑定作废自动腾出，只有需要隔离时（上游配置无效、切换后检测失败）才隔离整份资源并为其上其他订阅告警。
func ReturnCandidate(ctx context.Context, tx *kernel.Tx, bindingID string, quarantine bool, reason string) error {
	var res, mode string
	if err := tx.QueryRow(ctx, `SELECT resource_id, exit_mode FROM exit_bindings WHERE id = $1`, bindingID).Scan(&res, &mode); err != nil {
		return err
	}
	if mode != ModeShared {
		return ReturnToStock(ctx, tx, res, quarantine, reason)
	}
	if !quarantine {
		_, err := tx.Exec(ctx, `UPDATE upstream_resources SET row_version = row_version + 1 WHERE id = $1`, res)
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'quarantine', status_reason = $2, cooldown_until = NULL,
	                            row_version = row_version + 1 WHERE id = $1 AND status IN ('available', 'pending_verify')`, res, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM resource_slots WHERE resource_id = $1 AND kind = 'order_hold'`, res); err != nil {
		return err
	}
	return raiseSharedDown(ctx, tx, res, "quarantine", reason)
}

// SubExitMode 读订阅的出口类型。
func SubExitMode(ctx context.Context, q kernel.Q, subID string) (string, error) {
	var m string
	err := q.QueryRow(ctx, `SELECT exit_mode FROM subscriptions WHERE id = $1`, subID).Scan(&m)
	return m, err
}

// CandidateError 核对资源能否作为订阅换出口的候选（资源行已锁）：共享订阅只能换到另一个有空位的共享资源，
// 不能换到独享出口或上限为 1 的资源；独享订阅不能换到已有共享占用的资源（R-11.3、D-24）。返回 nil 表示可以。
// 资源状态（可售 / 待验证）与节点由调用方检查。
func CandidateError(ctx context.Context, tx *kernel.Tx, subID, resID string) error {
	var mode string
	var cap, occ int
	var excl, current bool
	if err := tx.QueryRow(ctx, `SELECT s.exit_mode, r.max_tenants, isp_shared_occupancy(r.id), isp_exclusive_occupied(r.id),
	        EXISTS (SELECT 1 FROM exit_bindings b WHERE b.subscription_id = s.id AND b.resource_id = r.id AND b.state IN ('pending', 'active'))
	      FROM subscriptions s, upstream_resources r WHERE s.id = $1 AND r.id = $2`, subID, resID).
		Scan(&mode, &cap, &occ, &excl, &current); err != nil {
		return err
	}
	if current {
		return httpx.Conflict("候选资源就是该订阅当前的出口")
	}
	if mode == ModeShared {
		switch {
		case cap < 2:
			return httpx.Conflict("共享订阅不能换到绑定上限为 1 的资源（只给独享套餐）")
		case excl:
			return httpx.Conflict("共享订阅不能换到独享订阅的出口")
		case occ >= cap:
			return httpx.Conflict("候选共享资源的名额已满")
		}
		return nil
	}
	if occ > 0 {
		return httpx.Conflict("候选资源上已有共享订阅或冷却中的共享名额，不能分给独享订阅")
	}
	return nil
}

// Tenant 是资源上的一个订阅（管理端资源详情“受影响订阅”）。
type Tenant struct {
	SubscriptionID string `json:"subscriptionId"`
	Name           string `json:"name"`
	UserID         string `json:"userId"`
	UserEmail      string `json:"userEmail"`
	ExitMode       string `json:"exitMode"`
	BindingState   string `json:"bindingState"`
	Lifecycle      string `json:"lifecycle"`
}

// Tenants 列出资源上有待生效或生效出口绑定的订阅。
func Tenants(ctx context.Context, q kernel.Q, res string) ([]Tenant, error) {
	rows, err := q.Query(ctx, `SELECT s.id, s.name, u.id, u.email, b.exit_mode, b.state, s.lifecycle
	                            FROM exit_bindings b JOIN subscriptions s ON s.id = b.subscription_id JOIN users u ON u.id = s.user_id
	                           WHERE b.resource_id = $1 AND b.state IN ('pending', 'active')
	                           ORDER BY b.created_at, b.id`, res)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tenant{}
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.SubscriptionID, &t.Name, &t.UserID, &t.UserEmail, &t.ExitMode, &t.BindingState, &t.Lifecycle); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
