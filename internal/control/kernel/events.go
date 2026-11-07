package kernel

import (
	"context"
	"fmt"
)

// 订阅事件中默认给客户看的种类（与 OpenAPI SubscriptionEventKind 一致）。
var customerKinds = map[string]bool{
	"ready": true, "time_added": true, "traffic_added": true, "password_reset": true, "access_changed": true,
	"exit_changed": true, "expired": true, "exhausted": true, "renamed": true,
}

// Event 是一条订阅事件（R-17 IP 详情的记录与管理端时间线）。
type Event struct {
	Sub       string
	Kind      string
	Detail    string
	OrderID   *string
	LedgerID  *string
	TaskID    *string
	ActorKind string // user / admin / system / node，默认 system
	ActorID   *string
	// Internal 为 true 时即使是客户种类也只给管理端。
	Internal bool
}

// AddEvent 追加订阅事件，时间取事务时间。detail 是给客户的中文说明，不含供应商信息。
func (t *Tx) AddEvent(ctx context.Context, e Event) error {
	if e.ActorKind == "" {
		e.ActorKind = "system"
	}
	visible := customerKinds[e.Kind] && !e.Internal
	_, err := t.Exec(ctx, `INSERT INTO subscription_events (subscription_id, at, kind, detail, customer_visible, order_id,
	                         ledger_entry_id, task_id, actor_kind, actor_id)
	                       VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.Sub, t.Now(), e.Kind, e.Detail, visible, e.OrderID, e.LedgerID, e.TaskID, e.ActorKind, e.ActorID)
	if err != nil {
		return fmt.Errorf("写订阅事件: %w", err)
	}
	return nil
}
