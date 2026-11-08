package subscription

import (
	"context"
	"fmt"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/inventory"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/task"
	"github.com/jackc/pgx/v5"
)

// Service 提供订阅生命周期与视图。
type Service struct {
	env   *kernel.Env
	tasks *task.Service
}

// New 创建服务。
func New(env *kernel.Env, tasks *task.Service) *Service { return &Service{env: env, tasks: tasks} }

// lockState 对订阅加锁并返回生命周期等状态。
type state struct {
	Lifecycle, UserID      string
	Paused                 bool
	CurAccess, CurExit     *string
	Entitled, Used         int64
	Expires, RetentionEnds *time.Time
	RulesVersion           string
}

func lock(ctx context.Context, tx *kernel.Tx, id string) (*state, error) {
	st := &state{}
	err := tx.QueryRow(ctx, `SELECT lifecycle, user_id, admin_paused, current_access_id, current_exit_id, entitled_bytes_total,
	                            used_bytes_total, expires_at, retention_ends_at, rules_version
	                          FROM subscriptions WHERE id = $1 FOR UPDATE`, id).
		Scan(&st.Lifecycle, &st.UserID, &st.Paused, &st.CurAccess, &st.CurExit, &st.Entitled, &st.Used, &st.Expires, &st.RetentionEnds,
			&st.RulesVersion)
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("没有找到这个订阅")
	}
	return st, err
}

// Rename 改名（用户本人或管理员）。userID 非空时要求属于该用户且未删除。
func (s *Service) Rename(ctx context.Context, tx *kernel.Tx, id, userID, name string, actorKind string, actorID *string) error {
	n, ok := kernel.TrimName(name)
	if !ok {
		return httpx.Invalid("名称需为 1～24 个字符")
	}
	st, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if userID != "" && (st.UserID != userID || st.Lifecycle == "deleted") {
		return httpx.NotFound("没有找到这个订阅")
	}
	var old string
	if err := tx.QueryRow(ctx, `SELECT name FROM subscriptions WHERE id = $1`, id).Scan(&old); err != nil {
		return err
	}
	if old == n {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET name = $2, row_version = row_version + 1 WHERE id = $1`, id, n); err != nil {
		return err
	}
	return tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: "renamed", Detail: fmt.Sprintf("名称由“%s”改为“%s”", old, n),
		ActorKind: actorKind, ActorID: actorID})
}

// Pause 管理员暂停：立即拒绝新连接，节点 ≤1 秒关闭存量连接；不顺延期限（R-10.1）。已暂停时幂等。
func (s *Service) Pause(ctx context.Context, tx *kernel.Tx, id, adminID, reason string) error {
	if reason == "" {
		return httpx.Invalid("请填写暂停原因")
	}
	st, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	switch st.Lifecycle {
	case "deleted":
		return httpx.NotFound("没有找到这个订阅")
	case "released":
		return httpx.Precondition("已释放的订阅没有可暂停的服务")
	}
	if st.Paused {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET admin_paused = true, paused_reason = $2, paused_at = $3, paused_by = $4,
	                            row_version = row_version + 1 WHERE id = $1`, id, reason, tx.Now(), adminID); err != nil {
		return err
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: "paused", Detail: "管理员暂停：" + reason, ActorKind: "admin", ActorID: &adminID}); err != nil {
		return err
	}
	if err := tx.Audit(ctx, adminID, "subscription.pause", "subscription:"+id, map[string]any{"reason": reason}); err != nil {
		return err
	}
	return tx.BumpSub(ctx, id)
}

// Resume 解除管理员暂停；到期、耗尽、封禁等其他阻断仍然生效。未暂停时幂等。
func (s *Service) Resume(ctx context.Context, tx *kernel.Tx, id, adminID string) error {
	st, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if st.Lifecycle == "deleted" {
		return httpx.NotFound("没有找到这个订阅")
	}
	if !st.Paused {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET admin_paused = false, paused_reason = NULL, paused_at = NULL, paused_by = NULL,
	                            row_version = row_version + 1 WHERE id = $1`, id); err != nil {
		return err
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: "resumed", Detail: "管理员解除暂停", ActorKind: "admin", ActorID: &adminID}); err != nil {
		return err
	}
	if err := tx.Audit(ctx, adminID, "subscription.resume", "subscription:"+id, nil); err != nil {
		return err
	}
	return tx.BumpSub(ctx, id)
}

// releaseResources 撤销当前接入与出口：端口与资源（共享为名额）进入冷却（R-10.4、D-14a、D-24）。
func releaseResources(ctx context.Context, tx *kernel.Tx, id string, st *state, reason string) error {
	rules, err := kernel.CurrentRules(ctx, tx, tx.Now())
	if err != nil {
		return err
	}
	now := tx.Now()
	if st.CurAccess != nil {
		if _, err := tx.Exec(ctx, `UPDATE access_versions SET state = 'revoked', revoked_at = $2, revoke_reason = $3
		                            WHERE id = $1 AND state <> 'revoked'`, *st.CurAccess, now, reason); err != nil {
			return err
		}
		if err := inventory.CooldownPorts(ctx, tx, *st.CurAccess, now.Add(kernel.Days(rules.PortCooldownDays))); err != nil {
			return err
		}
	}
	if st.CurExit != nil {
		if _, err := tx.Exec(ctx, `UPDATE exit_bindings SET state = 'unbound', unbound_at = $2, unbound_reason = $3
		                             WHERE id = $1`, *st.CurExit, now, reason); err != nil {
			return err
		}
		// 独享资源整份冷却；共享订阅腾出的名额冷却同样天数后才再分配（D-24）。
		if err := inventory.ReleaseBinding(ctx, tx, *st.CurExit, now.Add(kernel.Days(rules.ResourceCooldownDays))); err != nil {
			return err
		}
	}
	return nil
}

// Delete 删除订阅：停止入口、撤销凭据、释放资源，保留订单与消费历史（R-10.7）。有剩余流量必须明确确认（不静默没收）。
func (s *Service) Delete(ctx context.Context, tx *kernel.Tx, id, adminID, reason string, ackRemaining bool) error {
	if reason == "" {
		return httpx.Invalid("请填写删除原因")
	}
	st, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	switch st.Lifecycle {
	case "deleted":
		return httpx.NotFound("没有找到这个订阅")
	case "active", "retained", "released":
	default:
		return httpx.Precondition("只能删除有效或已释放的订阅；待履约或履约异常的请先处理订单")
	}
	var busy int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE subscription_id = $1 AND state IN ('queued', 'running')`, id).Scan(&busy); err != nil {
		return err
	}
	if busy > 0 {
		return httpx.Conflict("该订阅有进行中的变更任务，请等待完成后再删除")
	}
	remaining := max(0, st.Entitled-st.Used)
	if remaining > 0 && !ackRemaining {
		return httpx.Precondition(fmt.Sprintf("该订阅还有 %s 剩余流量；确认放弃或先划转到同用户的其他订阅后再删除",
			kernel.FormatBytes(remaining)))
	}
	if err := releaseResources(ctx, tx, id, st, "deleted"); err != nil {
		return err
	}
	now := tx.Now()
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'deleted', lifecycle_reason = $2, deleted_at = $3,
	                            current_access_id = NULL, current_exit_id = NULL, row_version = row_version + 1 WHERE id = $1`,
		id, reason, now); err != nil {
		return err
	}
	// 未付款的续费/加流量订单与未结束的换出口申请随订阅关闭。
	if _, err := tx.Exec(ctx, `UPDATE orders SET payment_status = 'cancelled', cancelled_at = $2, cancel_reason = 'admin', hold_until = hold_until,
	                            row_version = row_version + 1 WHERE subscription_id = $1 AND payment_status = 'awaiting'`, id, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE exit_requests SET status = 'cancelled', finished_at = $2, internal_note = internal_note || '（订阅已删除）'
	                            WHERE subscription_id = $1 AND status IN ('pending', 'processing')`, id, now); err != nil {
		return err
	}
	detail := "管理员删除：" + reason
	if remaining > 0 {
		detail += fmt.Sprintf("；已确认放弃剩余流量 %s", kernel.FormatBytes(remaining))
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: "deleted", Detail: detail, ActorKind: "admin", ActorID: &adminID}); err != nil {
		return err
	}
	if err := tx.Audit(ctx, adminID, "subscription.delete", "subscription:"+id,
		map[string]any{"reason": reason, "remainingBytes": remaining, "acknowledgeRemainingTraffic": ackRemaining}); err != nil {
		return err
	}
	return tx.BumpSub(ctx, id)
}

// AckAccess 清空接入的未确认改动字段（本人）。
func (s *Service) AckAccess(ctx context.Context, tx *kernel.Tx, id, userID string) error {
	st, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if st.UserID != userID || st.Lifecycle == "deleted" {
		return httpx.NotFound("没有找到这个订阅")
	}
	_, err = tx.Exec(ctx, `UPDATE access_versions SET unseen_changes = '{}' WHERE subscription_id = $1 AND state IN ('pending', 'active')
	                         AND cardinality(unseen_changes) > 0`, id)
	return err
}

// AccessSecret 是 OpenAPI AccessSecret。
type AccessSecret struct {
	Version  int    `json:"version"`
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
	Ports    Ports  `json:"ports"`
}

// Reveal 管理员查看当前接入明文；审计不含明文（R-04.9）。
func (s *Service) Reveal(ctx context.Context, tx *kernel.Tx, id, adminID, reason string) (*AccessSecret, error) {
	if reason == "" {
		return nil, httpx.Invalid("请填写查看原因")
	}
	x, err := loadRow(ctx, tx, tx.Now(), id, "")
	if err != nil {
		return nil, err
	}
	a, err := displayAccess(ctx, tx, x)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, httpx.NotFound("该订阅当前没有接入")
	}
	pw, err := s.env.Open(a.Enc)
	if err != nil {
		return nil, err
	}
	if err := tx.Audit(ctx, adminID, "access.reveal", "subscription:"+id, map[string]any{"reason": reason, "accessVersion": a.Version}); err != nil {
		return nil, err
	}
	return &AccessSecret{Version: a.Version, Host: kernel.StrOr(x.NodeHost, ""), Username: a.Username, Password: pw, Ports: a.Ports}, nil
}

// AfterCredit 在时长或流量入账后调用（同一事务、订阅已加锁）：提升期望版本；保留期内续时长后恢复为有效并保留原出口与接入
// （R-10.3）。已释放订阅续时长后的重新履约由订单流程调用 task.StartRefulfill。
func (s *Service) AfterCredit(ctx context.Context, tx *kernel.Tx, id string) error {
	st, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if st.Lifecycle == "retained" && st.Expires != nil && st.Expires.After(tx.Now()) {
		if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'active', lifecycle_reason = NULL, retention_ends_at = NULL,
		                            row_version = row_version + 1 WHERE id = $1`, id); err != nil {
			return err
		}
	}
	return tx.BumpSub(ctx, id)
}

// ---- 调度（docs/06 §3） ----

// ExpireDue 把已到期的有效订阅转入保留期（R-18）；时长与流量由节点按期望状态的 expires_at / entitled_total 即时执行，
// 这里只推进生命周期并写事件。
func (s *Service) ExpireDue(ctx context.Context) error {
	return s.env.InTx(ctx, func(tx *kernel.Tx) error {
		rows, err := tx.Query(ctx, `SELECT s.id, s.expires_at, r.retention_days FROM subscriptions s JOIN rules r ON r.version = s.rules_version
		                             WHERE s.lifecycle = 'active' AND s.expires_at <= $1 ORDER BY s.id LIMIT 500 FOR UPDATE OF s SKIP LOCKED`, tx.Now())
		if err != nil {
			return err
		}
		type due struct {
			id      string
			expires time.Time
			days    int
		}
		var ds []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.id, &d.expires, &d.days); err != nil {
				rows.Close()
				return err
			}
			ds = append(ds, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range ds {
			ends := d.expires.Add(kernel.Days(d.days))
			if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'retained', retention_ends_at = $2, row_version = row_version + 1
			                            WHERE id = $1`, d.id, ends); err != nil {
				return err
			}
			if err := tx.AddEvent(ctx, kernel.Event{Sub: d.id, Kind: "expired",
				Detail: fmt.Sprintf("时长已到期，已停止服务；保留原出口与接入至 %s，期间续时长可恢复", ends.In(shanghai).Format("2006-01-02 15:04"))}); err != nil {
				return err
			}
			if err := tx.BumpSub(ctx, d.id); err != nil {
				return err
			}
		}
		return nil
	})
}

var shanghai = func() *time.Location {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return l
}()

// ReleaseDue 保留期结束仍未续费的订阅：释放出口与接入（端口、资源进入冷却），时长段清空，剩余流量保留（R-10.4、R-10.6）。
func (s *Service) ReleaseDue(ctx context.Context) error {
	return s.env.InTx(ctx, func(tx *kernel.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM subscriptions WHERE lifecycle = 'retained' AND retention_ends_at <= $1
		                             AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.subscription_id = subscriptions.id AND t.state IN ('queued', 'running'))
		                             ORDER BY id LIMIT 200 FOR UPDATE SKIP LOCKED`, tx.Now())
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			st, err := lock(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := releaseResources(ctx, tx, id, st, "released"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'released', released_at = $2, retention_ends_at = NULL,
			                             seconds_before_period = entitled_seconds_total, period_started_at = NULL, expires_at = NULL,
			                             current_access_id = NULL, current_exit_id = NULL, row_version = row_version + 1 WHERE id = $1`,
				id, tx.Now()); err != nil {
				return err
			}
			remaining := max(0, st.Entitled-st.Used)
			if err := tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: "released",
				Detail: fmt.Sprintf("保留期结束，出口与接入已释放；剩余流量 %s 保留，续时长后重新开通", kernel.FormatBytes(remaining))}); err != nil {
				return err
			}
			if err := tx.BumpSub(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- 平台回调 ----

// UserStatusChanged 在封禁/解封的同一事务内调用（admin.Hooks）：该用户全部订阅生成新期望版本，节点据 enabled=false
// 断开存量连接（R-13.8）。事务由平台提交，提交后才唤醒节点：后台等待用户状态可见后再通知（最多 3 秒，之后由调度兜底）。
func (s *Service) UserStatusChanged(ctx context.Context, tx pgx.Tx, userID string) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&status); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `UPDATE subscriptions SET desired_version = desired_version + 1, row_version = row_version + 1
	                             WHERE user_id = $1 AND lifecycle <> 'deleted' RETURNING node_id`, userID)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for rows.Next() {
		var n *string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		if n != nil {
			set[*n] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	nodes := make([]string, 0, len(set))
	for n := range set {
		if _, err := tx.Exec(ctx, `UPDATE nodes SET desired_revision = desired_revision + 1 WHERE id = $1`, n); err != nil {
			return err
		}
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return nil
	}
	go s.notifyWhenVisible(userID, status, nodes)
	return nil
}

func (s *Service) notifyWhenVisible(userID, status string, nodes []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		var cur string
		if err := s.env.Pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&cur); err == nil && cur == status {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	for _, n := range nodes {
		s.env.Hub.Notify(n)
	}
}
