// Package ledger 是权益账本（C-W2，R-08、R-09、R-18；docs/04 §7.3）。
//
// 规则：
//   - 入账一律 INSERT … ON CONFLICT ON CONSTRAINT ledger_entries_source_key DO NOTHING（docs/06 §8.1）；
//     AFTER INSERT 触发器在同一事务内更新订阅合计，跳过的重复条目不会加权益。
//   - 订阅合计不能直接写；扣成负数由库约束整体拒绝，这里先行校验给出 412。
//   - 时长模型（docs/06 §8.1）：有效期内入账由触发器顺延到期时间；已到期（保留期）的订阅入账前
//     先“冻结”已耗尽的秒数——seconds_before_period := 当前合计、period_started_at := 现在——
//     这样续费从现在起算，停用空档不计入（R-18）。已释放的订阅在释放时已冻结并清空当前段
//     （period_started_at、expires_at 为空），续费只增加合计，重新履约就绪时才起算（R-10.5）。
package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
)

// Entry 是一条待入账的条目。
type Entry struct {
	Sub        string
	Kind       string // time（秒）/ traffic（字节）
	Amount     int64
	SourceType string // order / adjust / gift / compensation / refund / transfer_out / transfer_in
	SourceID   string
	OrderID    *string
	RefundID   *string
	TransferID *string
	Reason     string
	AdminID    *string
}

// Insert 入账。返回条目 ID 与是否真正插入（重复的业务来源键返回 false 与已有条目 ID）。
// 调用方必须已对订阅行加锁（SELECT … FOR UPDATE），时长正向入账前会按需冻结已耗尽的时长。
func Insert(ctx context.Context, tx *kernel.Tx, e Entry) (string, bool, error) {
	if e.Amount == 0 {
		return "", false, httpx.Invalid("入账数量不能为 0")
	}
	if e.Kind == "time" {
		if err := prepareTime(ctx, tx, e.Sub, e.Amount); err != nil {
			return "", false, err
		}
	} else if e.Amount < 0 {
		var entitled int64
		if err := tx.QueryRow(ctx, `SELECT entitled_bytes_total FROM subscriptions WHERE id = $1`, e.Sub).Scan(&entitled); err != nil {
			return "", false, err
		}
		if entitled+e.Amount < 0 {
			return "", false, httpx.Precondition("扣回的流量超过订阅的流量合计")
		}
	}
	id := store.NewID("led")
	var got string
	err := tx.QueryRow(ctx, `INSERT INTO ledger_entries (id, subscription_id, entitlement_kind, amount, source_type, source_id,
	                            order_id, refund_id, transfer_id, reason, admin_id, created_at)
	                          VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	                          ON CONFLICT ON CONSTRAINT ledger_entries_source_key DO NOTHING
	                          RETURNING id`,
		id, e.Sub, e.Kind, e.Amount, e.SourceType, e.SourceID, e.OrderID, e.RefundID, e.TransferID, e.Reason, e.AdminID, tx.Now()).Scan(&got)
	if kernel.IsNoRows(err) {
		var existing string
		if err := tx.QueryRow(ctx, `SELECT id FROM ledger_entries WHERE source_type = $1 AND source_id = $2 AND entitlement_kind = $3`,
			e.SourceType, e.SourceID, e.Kind).Scan(&existing); err != nil {
			return "", false, err
		}
		return existing, false, nil
	}
	if err != nil {
		if store.PgCode(err) == store.CodeCheckViolation {
			return "", false, httpx.Precondition("扣回不能超过订阅的剩余权益")
		}
		return "", false, fmt.Errorf("入账: %w", err)
	}
	return got, true, nil
}

// prepareTime 处理时长入账前的段落调整与扣回校验。
func prepareTime(ctx context.Context, tx *kernel.Tx, sub string, amount int64) error {
	var entitled, before int64
	var started, expires *time.Time
	if err := tx.QueryRow(ctx, `SELECT entitled_seconds_total, seconds_before_period, period_started_at, expires_at
	                              FROM subscriptions WHERE id = $1`, sub).Scan(&entitled, &before, &started, &expires); err != nil {
		return err
	}
	now := tx.Now()
	if amount > 0 {
		if started != nil && !expires.After(now) {
			// 已到期：之前的时长全部耗尽，续费从现在（或之后的重新就绪时间）起算。
			_, err := tx.Exec(ctx, `UPDATE subscriptions SET seconds_before_period = entitled_seconds_total,
			                          period_started_at = $2, expires_at = $2 WHERE id = $1`, sub, now)
			return err
		}
		return nil
	}
	// 扣回：不能超过剩余时长。
	var remaining int64
	if started == nil {
		remaining = entitled - before
	} else if expires.After(now) {
		remaining = int64(expires.Sub(now) / time.Second)
	}
	if -amount > remaining {
		return httpx.Precondition("扣回的时长超过订阅剩余时长")
	}
	return nil
}

// EntryView 是 OpenAPI LedgerEntry。
type EntryView struct {
	ID              string           `json:"id"`
	SubscriptionID  string           `json:"subscriptionId"`
	EntitlementKind string           `json:"entitlementKind"`
	Amount          int64            `json:"amount"`
	Unit            string           `json:"unit"`
	SourceType      string           `json:"sourceType"`
	SourceID        string           `json:"sourceId"`
	OrderID         *string          `json:"orderId"`
	RefundID        *string          `json:"refundId"`
	TransferID      *string          `json:"transferId"`
	Reason          string           `json:"reason"`
	Admin           *kernel.AdminRef `json:"admin"`
	CreatedAt       time.Time        `json:"createdAt"`
}

func list(ctx context.Context, q kernel.Q, where string, arg any) ([]EntryView, error) {
	rows, err := q.Query(ctx, `SELECT l.id, l.subscription_id, l.entitlement_kind, l.amount, l.source_type, l.source_id,
	                              l.order_id, l.refund_id, l.transfer_id, l.reason, l.admin_id, a.email, l.created_at
	                            FROM ledger_entries l LEFT JOIN admins a ON a.id = l.admin_id
	                           WHERE `+where+` ORDER BY l.created_at, l.id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EntryView{}
	for rows.Next() {
		var v EntryView
		var adminID, adminEmail *string
		if err := rows.Scan(&v.ID, &v.SubscriptionID, &v.EntitlementKind, &v.Amount, &v.SourceType, &v.SourceID,
			&v.OrderID, &v.RefundID, &v.TransferID, &v.Reason, &adminID, &adminEmail, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Unit = "bytes"
		if v.EntitlementKind == "time" {
			v.Unit = "seconds"
		}
		if adminID != nil {
			v.Admin = &kernel.AdminRef{ID: *adminID, Email: kernel.StrOr(adminEmail, "")}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListBySub 返回订阅的账本（按时间正序）。
func ListBySub(ctx context.Context, q kernel.Q, sub string) ([]EntryView, error) {
	return list(ctx, q, "l.subscription_id = $1", sub)
}

// ListByOrder 返回订单产生的入账条目。
func ListByOrder(ctx context.Context, q kernel.Q, order string) ([]EntryView, error) {
	return list(ctx, q, "l.order_id = $1", order)
}

// Ledger 是 OpenAPI Ledger。
type Ledger struct {
	Entries              []EntryView `json:"entries"`
	EntitledBytesTotal   int64       `json:"entitledBytesTotal"`
	UsedBytesTotal       int64       `json:"usedBytesTotal"`
	EntitledSecondsTotal int64       `json:"entitledSecondsTotal"`
}

// Load 读订阅账本与合计；订阅不存在返回 404。
func Load(ctx context.Context, q kernel.Q, sub string) (*Ledger, error) {
	l := &Ledger{}
	if err := q.QueryRow(ctx, `SELECT entitled_bytes_total, used_bytes_total, entitled_seconds_total FROM subscriptions WHERE id = $1`,
		sub).Scan(&l.EntitledBytesTotal, &l.UsedBytesTotal, &l.EntitledSecondsTotal); err != nil {
		if kernel.IsNoRows(err) {
			return nil, httpx.NotFound("没有找到这个订阅")
		}
		return nil, err
	}
	var err error
	l.Entries, err = ListBySub(ctx, q, sub)
	return l, err
}

// TransferView 是 OpenAPI Transfer。
type TransferView struct {
	ID                 string          `json:"id"`
	UserID             string          `json:"userId"`
	FromSubscriptionID string          `json:"fromSubscriptionId"`
	ToSubscriptionID   string          `json:"toSubscriptionId"`
	Bytes              int64           `json:"bytes"`
	Reason             string          `json:"reason"`
	Admin              kernel.AdminRef `json:"admin"`
	CreatedAt          time.Time       `json:"createdAt"`
}

// Transfer 把已释放订阅的剩余流量全额划转到同一用户的另一个订阅（R-10.6、A-45）：
// 成对账本条目（transfer_out / transfer_in），总额守恒；两端必须同属一个用户（否则 412）。
func Transfer(ctx context.Context, tx *kernel.Tx, from, to, reason, adminID string) (*TransferView, error) {
	if from == to {
		return nil, httpx.Invalid("来源与目标不能是同一个订阅")
	}
	type subRow struct {
		user, lifecycle string
		entitled, used  int64
	}
	load := func(id string) (*subRow, error) {
		s := &subRow{}
		err := tx.QueryRow(ctx, `SELECT user_id, lifecycle, entitled_bytes_total, used_bytes_total FROM subscriptions
		                          WHERE id = $1 FOR UPDATE`, id).Scan(&s.user, &s.lifecycle, &s.entitled, &s.used)
		if kernel.IsNoRows(err) {
			return nil, httpx.NotFound("没有找到订阅 " + id)
		}
		return s, err
	}
	// 固定加锁顺序，避免并发划转死锁。
	first, second := from, to
	if second < first {
		first, second = second, first
	}
	a, err := load(first)
	if err != nil {
		return nil, err
	}
	b, err := load(second)
	if err != nil {
		return nil, err
	}
	src, dst := a, b
	if first != from {
		src, dst = b, a
	}
	if src.user != dst.user {
		return nil, httpx.Precondition("只能在同一用户的订阅之间划转")
	}
	if src.lifecycle != "released" {
		return nil, httpx.Precondition("只有已释放的订阅可以划转剩余流量")
	}
	if dst.lifecycle == "deleted" {
		return nil, httpx.Precondition("目标订阅已删除")
	}
	bytes := src.entitled - src.used
	if bytes <= 0 {
		return nil, httpx.Precondition("来源订阅没有剩余流量可划转")
	}
	v := &TransferView{ID: store.NewID("trf"), UserID: src.user, FromSubscriptionID: from, ToSubscriptionID: to,
		Bytes: bytes, Reason: reason, CreatedAt: tx.Now()}
	if _, err := tx.Exec(ctx, `INSERT INTO transfers (id, user_id, from_subscription_id, to_subscription_id, bytes, reason, admin_id, created_at)
	                          VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, v.ID, v.UserID, from, to, bytes, reason, adminID, v.CreatedAt); err != nil {
		return nil, err
	}
	outID, _, err := Insert(ctx, tx, Entry{Sub: from, Kind: "traffic", Amount: -bytes, SourceType: "transfer_out", SourceID: v.ID,
		TransferID: &v.ID, Reason: reason, AdminID: &adminID})
	if err != nil {
		return nil, err
	}
	inID, _, err := Insert(ctx, tx, Entry{Sub: to, Kind: "traffic", Amount: bytes, SourceType: "transfer_in", SourceID: v.ID,
		TransferID: &v.ID, Reason: reason, AdminID: &adminID})
	if err != nil {
		return nil, err
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: from, Kind: "transferred", LedgerID: &outID, ActorKind: "admin", ActorID: &adminID,
		Detail: fmt.Sprintf("剩余流量 %s 已全额划转到订阅 %s", kernel.FormatBytes(bytes), to)}); err != nil {
		return nil, err
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: to, Kind: "traffic_added", LedgerID: &inID, ActorKind: "admin", ActorID: &adminID,
		Detail: fmt.Sprintf("从你的另一个 IP 划入流量 +%s", kernel.FormatBytes(bytes))}); err != nil {
		return nil, err
	}
	for _, s := range []string{from, to} {
		if err := tx.BumpSub(ctx, s); err != nil {
			return nil, err
		}
	}
	ref, err := kernel.LoadAdminRef(ctx, tx, &adminID)
	if err != nil {
		return nil, err
	}
	v.Admin = *ref
	return v, nil
}
