package task

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-gost/gost/internal/control/alert"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/inventory"
	"github.com/go-gost/gost/internal/control/kernel"
)

// 服务器直出代理（D-25，docs/06 §9.2）。
const (
	// KindDirect 是 subscriptions.kind 的直出取值。
	KindDirect = "direct"
	// KindDirectGrant 是赠送直出代理的任务种类：签发首个接入、等节点应用后生效（没有订单、出口与上游资源）。
	KindDirectGrant = "direct_grant"
)

// StartDirectGrant 为待开通或开通失败的直出订阅签发首个接入并下发（管理员赠送或重试）。返回任务 ID。
func (s *Service) StartDirectGrant(ctx context.Context, tx *kernel.Tx, subID string, a Actor) (string, error) {
	sb, err := lockSub(ctx, tx, subID)
	if err != nil {
		return "", err
	}
	if sb.Kind != KindDirect {
		return "", httpx.Precondition("不是直出代理订阅")
	}
	if sb.Lifecycle != "pending" && sb.Lifecycle != "fulfill_failed" {
		return "", httpx.Precondition("只有待开通或开通失败的直出代理可以开通")
	}
	return s.startDirectGrant(ctx, tx, sb, a)
}

func (s *Service) startDirectGrant(ctx context.Context, tx *kernel.Tx, sb *sub, a Actor) (string, error) {
	if err := ensureIdle(ctx, tx, sb.ID); err != nil {
		return "", err
	}
	if sb.Node == nil {
		return "", httpx.Precondition("直出代理没有承载节点")
	}
	node := *sb.Node
	var nodeStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM nodes WHERE id = $1`, node).Scan(&nodeStatus); err != nil {
		return "", err
	}
	if nodeStatus != "active" {
		return "", httpx.Precondition("承载节点暂不可用")
	}
	if _, err := inventory.PickPorts(ctx, tx, node); errors.Is(err, inventory.ErrNoPorts) {
		_ = alert.Raise(ctx, tx, tx.Now(), alert.Alert{Kind: "port_pool_low", Severity: "critical", Key: "port_pool_low:" + node,
			Message: "节点端口池不足，无法签发新接入", Node: &node})
		return "", inventory.ErrNoPorts
	} else if err != nil {
		return "", err
	}
	taskID, err := s.newTask(ctx, tx, sb.ID, KindDirectGrant, a, nil, nil, map[string]any{"nodeId": node})
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'pending', lifecycle_reason = NULL, row_version = row_version + 1
	                            WHERE id = $1`, sb.ID); err != nil {
		return "", err
	}
	if _, err := s.issueAccess(ctx, tx, sb.ID, node, "initial", taskID, nil); err != nil {
		return "", err
	}
	return taskID, setTarget(ctx, tx, sb.ID, taskID)
}

// activateDirect 直出代理就绪：生命周期 → 有效。直出没有时长账本，不起算、不存到期时间（可用截止跟随付费订阅计算）。
func (s *Service) activateDirect(ctx context.Context, tx *kernel.Tx, r *running, accessV int) error {
	now := tx.Now()
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'active', lifecycle_reason = NULL,
	                             first_ready_at = COALESCE(first_ready_at, $2), row_version = row_version + 1 WHERE id = $1`, r.Sub, now); err != nil {
		return err
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: r.Sub, Kind: "ready", TaskID: &r.ID,
		Detail: fmt.Sprintf("服务器直出代理已开通，接入 v%d；不限流量，跟随你的付费 IP 有效", accessV)}); err != nil {
		return err
	}
	if err := alert.Resolve(ctx, tx, now, "fulfill_failed:"+r.Sub); err != nil {
		return err
	}
	return tx.BumpSub(ctx, r.Sub)
}

// markDirectFailed 记录直出代理开通失败：订阅进入“开通失败”，告警；管理员可重试或收回。
func (s *Service) markDirectFailed(ctx context.Context, tx *kernel.Tx, r *running, code, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'fulfill_failed', lifecycle_reason = $2, row_version = row_version + 1
	                            WHERE id = $1 AND lifecycle IN ('pending', 'fulfill_failed')`, r.Sub, reason); err != nil {
		return err
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: r.Sub, Kind: "fulfill_failed", TaskID: &r.ID, Detail: "直出代理开通未成功：" + reason}); err != nil {
		return err
	}
	return alert.Raise(ctx, tx, tx.Now(), alert.Alert{Kind: "fulfill_failed", Severity: "warning", Key: "fulfill_failed:" + r.Sub,
		Message: "直出代理开通失败：" + reason, Details: map[string]any{"code": code}, Sub: &r.Sub, Task: &r.ID})
}

// AbortRunning 让订阅进行中的任务失败并回滚（收回直出代理前调用）。节点可能已监听过新端口，端口进入冷却。
// 没有进行中的任务时什么也不做。调用方须已对订阅行加锁。
func (s *Service) AbortRunning(ctx context.Context, tx *kernel.Tx, subID, reason string) error {
	r, err := lockRunning(ctx, tx, subID)
	if err != nil || r == nil {
		return err
	}
	return s.fail(ctx, tx, r, "cancelled", reason, true)
}
