package order

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/ledger"
	"github.com/go-gost/gost/internal/control/store"
)

// Line 是 OpenAPI OrderLine。
type Line struct {
	Label  string       `json:"label"`
	Detail string       `json:"detail"`
	Amount kernel.Money `json:"amount"`
}

// Step 是 OpenAPI OrderStep。
type Step struct {
	Label string     `json:"label"`
	State string     `json:"state"`
	At    *time.Time `json:"at"`
}

// View 是 OpenAPI Order。
type View struct {
	ID                string       `json:"id"`
	No                string       `json:"no"`
	Kind              string       `json:"kind"`
	CreatedAt         time.Time    `json:"createdAt"`
	HoldUntil         *time.Time   `json:"holdUntil"`
	Amount            kernel.Money `json:"amount"`
	Payment           string       `json:"payment"`
	Fulfillment       string       `json:"fulfillment"`
	UserClaimedPaidAt *time.Time   `json:"userClaimedPaidAt"`
	PayRef            string       `json:"payRef"`
	SubscriptionID    *string      `json:"subscriptionId"`
	SubscriptionName  *string      `json:"subscriptionName"`
	Lines             []Line       `json:"lines"`
	Steps             []Step       `json:"steps"`
	RulesSnapshot     []string     `json:"rulesSnapshot"`
}

// PaymentRecord 是 OpenAPI PaymentRecord。
type PaymentRecord struct {
	ID          string          `json:"id"`
	Amount      kernel.Money    `json:"amount"`
	Method      string          `json:"method"`
	ExternalRef string          `json:"externalRef"`
	LateArrival bool            `json:"lateArrival"`
	Note        string          `json:"note"`
	ConfirmedBy kernel.AdminRef `json:"confirmedBy"`
	ConfirmedAt time.Time       `json:"confirmedAt"`
}

// Refund 是 OpenAPI Refund。
type Refund struct {
	ID             string           `json:"id"`
	OrderID        string           `json:"orderId"`
	Status         string           `json:"status"`
	Amount         kernel.Money     `json:"amount"`
	ReclaimBytes   int64            `json:"reclaimBytes"`
	ReclaimSeconds int64            `json:"reclaimSeconds"`
	Reason         string           `json:"reason"`
	ExternalRef    *string          `json:"externalRef"`
	RequestedBy    kernel.AdminRef  `json:"requestedBy"`
	RequestedAt    time.Time        `json:"requestedAt"`
	DecidedBy      *kernel.AdminRef `json:"decidedBy"`
	DecidedAt      *time.Time       `json:"decidedAt"`
	DecisionNote   *string          `json:"decisionNote"`
}

// AdminView 是 OpenAPI AdminOrder。
type AdminView struct {
	View
	User             kernel.UserRef     `json:"user"`
	PaymentMethod    string             `json:"paymentMethod"`
	PaymentRecord    *PaymentRecord     `json:"paymentRecord"`
	LedgerEntries    []ledger.EntryView `json:"ledgerEntries"`
	Refunds          []Refund           `json:"refunds"`
	RulesVersion     string             `json:"rulesVersion"`
	PlanVersionID    *string            `json:"planVersionId"`
	FulfillmentError *string            `json:"fulfillmentError"`
	CancelReason     *string            `json:"cancelReason"`
	ConfirmedAt      *time.Time         `json:"confirmedAt"`
	DeliveredAt      *time.Time         `json:"deliveredAt"`
	CreatedByAdmin   *kernel.AdminRef   `json:"createdByAdmin"`
}

const cols = `o.id, o.no, o.kind, o.created_at, o.hold_until, o.amount_cents, o.payment_status, o.fulfillment_status,
	o.user_claimed_paid_at, o.pay_ref, o.subscription_id, s.name, o.rules_snapshot, o.user_id, u.email, o.payment_method,
	o.rules_version, o.plan_version_id, o.fulfillment_error, o.cancel_reason, o.confirmed_at, o.delivered_at, o.cancelled_at,
	o.created_by_admin, ad.email`

const from = ` FROM orders o JOIN users u ON u.id = o.user_id LEFT JOIN subscriptions s ON s.id = o.subscription_id
	LEFT JOIN admins ad ON ad.id = o.created_by_admin`

type scanned struct {
	AdminView
	cancelledAt *time.Time
}

func scan(row interface{ Scan(...any) error }) (*scanned, error) {
	v := &scanned{}
	var amount int64
	var snap []byte
	var by, byEmail *string
	if err := row.Scan(&v.ID, &v.No, &v.Kind, &v.CreatedAt, &v.HoldUntil, &amount, &v.Payment, &v.Fulfillment, &v.UserClaimedPaidAt,
		&v.PayRef, &v.SubscriptionID, &v.SubscriptionName, &snap, &v.User.ID, &v.User.Email, &v.PaymentMethod, &v.RulesVersion,
		&v.PlanVersionID, &v.FulfillmentError, &v.CancelReason, &v.ConfirmedAt, &v.DeliveredAt, &v.cancelledAt, &by, &byEmail); err != nil {
		return nil, err
	}
	v.Amount = kernel.CNY(amount)
	if v.Payment != "awaiting" {
		v.HoldUntil = nil
	}
	var sn struct {
		Lines []line   `json:"lines"`
		Rules []string `json:"rules"`
	}
	_ = json.Unmarshal(snap, &sn)
	v.Lines = make([]Line, 0, len(sn.Lines))
	for _, l := range sn.Lines {
		v.Lines = append(v.Lines, Line{Label: l.Label, Detail: l.Detail, Amount: l.Amount})
	}
	v.RulesSnapshot = sn.Rules
	if v.RulesSnapshot == nil {
		v.RulesSnapshot = []string{}
	}
	if by != nil {
		v.CreatedByAdmin = &kernel.AdminRef{ID: *by, Email: kernel.StrOr(byEmail, "")}
	}
	v.Steps = steps(v)
	return v, nil
}

// steps 是订单进度：提交 → 核对付款 → 开通（新购）/ 入账（续购）。
func steps(v *scanned) []Step {
	created := v.CreatedAt
	out := []Step{{Label: "提交订单", State: "done", At: &created}}
	pay := Step{Label: "核对付款"}
	switch v.Payment {
	case "awaiting":
		pay.State = "current"
	case "cancelled":
		pay.State, pay.At = "failed", v.cancelledAt
		if v.ConfirmedAt != nil {
			pay.State, pay.At = "done", v.ConfirmedAt
		}
	default:
		pay.State, pay.At = "done", v.ConfirmedAt
	}
	out = append(out, pay)
	last := Step{Label: "入账"}
	if v.Kind == "new" {
		last.Label = "开通"
	}
	switch v.Fulfillment {
	case "not_started":
		last.State = "todo"
		if v.Payment == "cancelled" {
			last.State = "failed"
		}
	case "processing":
		last.State = "current"
	case "delivered":
		last.State, last.At = "done", v.DeliveredAt
	case "failed":
		last.State = "failed"
		last.Label += "（已收款，交付异常，管理员处理中）"
	}
	return append(out, last)
}

func (s *Service) load(ctx context.Context, q kernel.Q, id, userID string) (*scanned, error) {
	sql := `SELECT ` + cols + from + ` WHERE o.id = $1`
	args := []any{id}
	if userID != "" {
		sql += ` AND o.user_id = $2`
		args = append(args, userID)
	}
	v, err := scan(q.QueryRow(ctx, sql, args...))
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("没有找到这个订单")
	}
	return v, err
}

// Get 读本人订单。
func (s *Service) Get(ctx context.Context, q kernel.Q, id, userID string) (*View, error) {
	v, err := s.load(ctx, q, id, userID)
	if err != nil {
		return nil, err
	}
	return &v.View, nil
}

// ListForUser 列出本人订单（新的在前）。
func (s *Service) ListForUser(ctx context.Context, userID string) ([]View, error) {
	rows, err := s.env.Pool.Query(ctx, `SELECT `+cols+from+` WHERE o.user_id = $1 ORDER BY o.created_at DESC, o.id DESC LIMIT 500`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []View{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v.View)
	}
	return out, rows.Err()
}

func (s *Service) fillAdmin(ctx context.Context, q kernel.Q, v *scanned) (*AdminView, error) {
	var p PaymentRecord
	var amount int64
	err := q.QueryRow(ctx, `SELECT p.id, p.amount_cents, p.method, p.external_ref, p.late_arrival, p.note, p.confirmed_by, a.email, p.confirmed_at
	                          FROM payments p JOIN admins a ON a.id = p.confirmed_by WHERE p.order_id = $1`, v.ID).
		Scan(&p.ID, &amount, &p.Method, &p.ExternalRef, &p.LateArrival, &p.Note, &p.ConfirmedBy.ID, &p.ConfirmedBy.Email, &p.ConfirmedAt)
	if err == nil {
		p.Amount = kernel.CNY(amount)
		v.PaymentRecord = &p
	} else if !kernel.IsNoRows(err) {
		return nil, err
	}
	if v.LedgerEntries, err = ledger.ListByOrder(ctx, q, v.ID); err != nil {
		return nil, err
	}
	if v.Refunds, err = listRefunds(ctx, q, `r.order_id = $1`, []any{v.ID}, ""); err != nil {
		return nil, err
	}
	return &v.AdminView, nil
}

// AdminGet 读管理端订单详情。
func (s *Service) AdminGet(ctx context.Context, q kernel.Q, id string) (*AdminView, error) {
	v, err := s.load(ctx, q, id, "")
	if err != nil {
		return nil, err
	}
	return s.fillAdmin(ctx, q, v)
}

// AdminList 是管理端订单列表（待核款置顶，其余新的在前）。
func (s *Service) AdminList(r *http.Request) (*kernel.Page[AdminView], error) {
	cur, limit, err := kernel.ParsePage(r)
	if err != nil {
		return nil, err
	}
	var a kernel.Args
	pay, err := kernel.ListParam(r, "payment", "awaiting", "confirmed", "cancelled", "refund_review", "refunded")
	if err != nil {
		return nil, err
	}
	if len(pay) > 0 {
		a.Add("o.payment_status = ANY(?)", pay)
	}
	ful, err := kernel.ListParam(r, "fulfillment", "not_started", "processing", "delivered", "failed")
	if err != nil {
		return nil, err
	}
	if len(ful) > 0 {
		a.Add("o.fulfillment_status = ANY(?)", ful)
	}
	q := r.URL.Query()
	if k := q.Get("kind"); k != "" {
		if k != "new" && k != "renew_time" && k != "add_traffic" {
			return nil, httpx.Invalid("kind 取值不正确")
		}
		a.Add("o.kind = ?", k)
	}
	if v := q.Get("userId"); v != "" {
		a.Add("o.user_id = ?", v)
	}
	if v := q.Get("subscriptionId"); v != "" {
		a.Add("o.subscription_id = ?", v)
	}
	if v := q.Get("q"); v != "" {
		p1, p2 := a.Next(v), a.Next("%"+v+"%")
		a.AddRaw("(o.no = " + p1 + " OR o.pay_ref = upper(" + p1 + ") OR o.id = " + p1 + " OR u.email ILIKE " + p2 + ")")
	}
	for _, f := range []struct{ name, op string }{{"createdFrom", ">="}, {"createdTo", "<"}} {
		t, err := kernel.TimeParam(r, f.name)
		if err != nil {
			return nil, err
		}
		if t != nil {
			a.Add("o.created_at "+f.op+" ?", *t)
		}
	}
	const rank = `(CASE WHEN o.payment_status = 'awaiting' THEN 0 ELSE 1 END)`
	if cur != nil {
		pr, pa, pi := a.Next(cur.Rank), a.Next(cur.At), a.Next(cur.ID)
		a.AddRaw("(" + rank + " > " + pr + " OR (" + rank + " = " + pr + " AND (o.created_at, o.id) < (" + pa + ", " + pi + ")))")
	}
	rows, err := s.env.Pool.Query(r.Context(), `SELECT `+cols+from+a.SQL()+` ORDER BY `+rank+`, o.created_at DESC, o.id DESC LIMIT `+
		strconv.Itoa(limit+1), a.Vals...)
	if err != nil {
		return nil, err
	}
	var vs []*scanned
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		vs = append(vs, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := &kernel.Page[AdminView]{Items: []AdminView{}}
	for i, v := range vs {
		if i == limit {
			last := vs[i-1]
			rk := 1
			if last.Payment == "awaiting" {
				rk = 0
			}
			page.NextCursor = kernel.Ptr(kernel.Cursor{Rank: rk, At: last.CreatedAt, ID: last.ID}.Encode())
			break
		}
		av, err := s.fillAdmin(r.Context(), s.env.Pool, v)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *av)
	}
	return page, nil
}

// UserRecentOrders 是管理端用户详情的最近订单（平台 admin.Hooks.UserRecentOrders）。
func (s *Service) UserRecentOrders(ctx context.Context, userID string) (any, error) {
	rows, err := s.env.Pool.Query(ctx, `SELECT `+cols+from+` WHERE o.user_id = $1 ORDER BY o.created_at DESC, o.id DESC LIMIT 20`, userID)
	if err != nil {
		return nil, err
	}
	var vs []*scanned
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		vs = append(vs, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []AdminView{}
	for _, v := range vs {
		av, err := s.fillAdmin(ctx, s.env.Pool, v)
		if err != nil {
			return nil, err
		}
		out = append(out, *av)
	}
	return out, nil
}

// ---- 退款（R-09.6） ----

const refundCols = `r.id, r.order_id, r.status, r.amount_cents, r.reclaim_bytes, r.reclaim_seconds, r.reason, r.external_ref,
	r.requested_by, ra.email, r.requested_at, r.decided_by, da.email, r.decided_at, r.decision_note`

func listRefunds(ctx context.Context, q kernel.Q, where string, args []any, tail string) ([]Refund, error) {
	rows, err := q.Query(ctx, `SELECT `+refundCols+` FROM refunds r JOIN admins ra ON ra.id = r.requested_by
	                            LEFT JOIN admins da ON da.id = r.decided_by WHERE `+where+` ORDER BY r.requested_at DESC, r.id DESC`+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Refund{}
	for rows.Next() {
		var f Refund
		var amount int64
		var by, email *string
		if err := rows.Scan(&f.ID, &f.OrderID, &f.Status, &amount, &f.ReclaimBytes, &f.ReclaimSeconds, &f.Reason, &f.ExternalRef,
			&f.RequestedBy.ID, &f.RequestedBy.Email, &f.RequestedAt, &by, &email, &f.DecidedAt, &f.DecisionNote); err != nil {
			return nil, err
		}
		f.Amount = kernel.CNY(amount)
		if by != nil {
			f.DecidedBy = &kernel.AdminRef{ID: *by, Email: kernel.StrOr(email, "")}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func getRefund(ctx context.Context, q kernel.Q, id string) (*Refund, error) {
	fs, err := listRefunds(ctx, q, `r.id = $1`, []any{id}, "")
	if err != nil {
		return nil, err
	}
	if len(fs) == 0 {
		return nil, httpx.NotFound("没有找到这笔退款")
	}
	return &fs[0], nil
}

// RefundInput 是 OpenAPI RefundInput。
type RefundInput struct {
	Amount         kernel.Money `json:"amount"`
	ReclaimBytes   int64        `json:"reclaimBytes"`
	ReclaimSeconds int64        `json:"reclaimSeconds"`
	Reason         string       `json:"reason"`
}

func remaining(ctx context.Context, tx *kernel.Tx, sub string) (bytes, secs int64, err error) {
	var entitled, used, es, before int64
	var expires *time.Time
	if err = tx.QueryRow(ctx, `SELECT entitled_bytes_total, used_bytes_total, entitled_seconds_total, seconds_before_period, expires_at
	                            FROM subscriptions WHERE id = $1 FOR UPDATE`, sub).Scan(&entitled, &used, &es, &before, &expires); err != nil {
		return
	}
	bytes = max(0, entitled-used)
	if expires == nil {
		secs = max(0, es-before)
	} else if expires.After(tx.Now()) {
		secs = int64(expires.Sub(tx.Now()) / time.Second)
	}
	return
}

// RequestRefund 发起退款复核：订单进入 refund_review；回收量不能超过订阅当前剩余。
func (s *Service) RequestRefund(ctx context.Context, tx *kernel.Tx, orderID, adminID string, in RefundInput) (string, error) {
	if in.Reason == "" || len([]rune(in.Reason)) > 500 {
		return "", httpx.Invalid("请填写退款原因（不超过 500 字）")
	}
	if in.Amount.Amount < 0 || in.ReclaimBytes < 0 || in.ReclaimSeconds < 0 {
		return "", httpx.Invalid("金额与回收量不能为负")
	}
	o, err := lockOrder(ctx, tx, orderID, "")
	if err != nil {
		return "", err
	}
	if o.Payment == "refund_review" {
		return "", httpx.Conflict("该订单已有待复核的退款")
	}
	if o.Payment != "confirmed" {
		return "", httpx.Precondition("只有已收款的订单可以发起退款")
	}
	if in.Amount.Amount > o.Amount {
		return "", httpx.Invalid("退款金额不能超过订单金额")
	}
	if o.Sub != nil {
		b, sec, err := remaining(ctx, tx, *o.Sub)
		if err != nil {
			return "", err
		}
		if in.ReclaimBytes > b || in.ReclaimSeconds > sec {
			return "", httpx.Precondition(fmt.Sprintf("回收量超过订阅当前剩余（流量 %s，时长 %d 秒）", kernel.FormatBytes(b), sec))
		}
	} else if in.ReclaimBytes > 0 || in.ReclaimSeconds > 0 {
		return "", httpx.Precondition("该订单没有关联订阅，不能回收权益")
	}
	id := store.NewID("rfd")
	if _, err := tx.Exec(ctx, `INSERT INTO refunds (id, order_id, amount_cents, reclaim_bytes, reclaim_seconds, reason, requested_by, requested_at)
	                          VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, orderID, in.Amount.Amount, in.ReclaimBytes, in.ReclaimSeconds,
		in.Reason, adminID, tx.Now()); err != nil {
		if store.IsUniqueViolation(err, "refunds_one_open") {
			return "", httpx.Conflict("该订单已有待复核的退款")
		}
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET payment_status = 'refund_review', row_version = row_version + 1 WHERE id = $1`, orderID); err != nil {
		return "", err
	}
	return id, tx.Audit(ctx, adminID, "refund.request", "order:"+orderID, map[string]any{"refundId": id, "amount": in.Amount.Amount,
		"reclaimBytes": in.ReclaimBytes, "reclaimSeconds": in.ReclaimSeconds, "reason": in.Reason})
}

// DecideRefund 批准（追加负向账本条目回收权益，订单 refunded）或驳回（订单回到 confirmed）。重复决定 409。
func (s *Service) DecideRefund(ctx context.Context, tx *kernel.Tx, refundID, adminID string, approve bool, note, externalRef string) error {
	var orderID, status string
	var bytes, secs int64
	err := tx.QueryRow(ctx, `SELECT order_id, status, reclaim_bytes, reclaim_seconds FROM refunds WHERE id = $1 FOR UPDATE`, refundID).
		Scan(&orderID, &status, &bytes, &secs)
	if kernel.IsNoRows(err) {
		return httpx.NotFound("没有找到这笔退款")
	}
	if err != nil {
		return err
	}
	if status != "review" {
		return httpx.Conflict("这笔退款已经处理过")
	}
	o, err := lockOrder(ctx, tx, orderID, "")
	if err != nil {
		return err
	}
	now := tx.Now()
	newStatus, orderStatus := "rejected", "confirmed"
	if approve {
		newStatus, orderStatus = "approved", "refunded"
		if o.Sub != nil && (bytes > 0 || secs > 0) {
			b, sec, err := remaining(ctx, tx, *o.Sub)
			if err != nil {
				return err
			}
			if bytes > b || secs > sec {
				return httpx.Precondition("订阅剩余已少于申请回收的量，请驳回后重新发起")
			}
			for _, e := range []struct {
				kind   string
				amount int64
			}{{"time", secs}, {"traffic", bytes}} {
				if e.amount == 0 {
					continue
				}
				if _, _, err := ledger.Insert(ctx, tx, ledger.Entry{Sub: *o.Sub, Kind: e.kind, Amount: -e.amount, SourceType: "refund",
					SourceID: refundID, OrderID: &orderID, RefundID: &refundID, Reason: "退款回收", AdminID: &adminID}); err != nil {
					return err
				}
			}
			if err := tx.AddEvent(ctx, kernel.Event{Sub: *o.Sub, Kind: "refunded", OrderID: &orderID, ActorKind: "admin", ActorID: &adminID,
				Detail: fmt.Sprintf("退款回收：时长 %d 秒、流量 %s", secs, kernel.FormatBytes(bytes))}); err != nil {
				return err
			}
			if err := s.subs.AfterCredit(ctx, tx, *o.Sub); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE refunds SET status = $2, decided_by = $3, decided_at = $4, decision_note = $5,
	                            external_ref = COALESCE(NULLIF($6, ''), external_ref) WHERE id = $1`,
		refundID, newStatus, adminID, now, note, externalRef); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET payment_status = $2, row_version = row_version + 1 WHERE id = $1`, orderID, orderStatus); err != nil {
		return err
	}
	return tx.Audit(ctx, adminID, "refund.decide", "refund:"+refundID, map[string]any{"approve": approve, "note": note})
}

// ListRefunds 是管理端退款列表。
func (s *Service) ListRefunds(r *http.Request) (*kernel.Page[Refund], error) {
	cur, limit, err := kernel.ParsePage(r)
	if err != nil {
		return nil, err
	}
	where, args := "true", []any{}
	if st := r.URL.Query().Get("status"); st != "" {
		if st != "review" && st != "approved" && st != "rejected" {
			return nil, httpx.Invalid("status 取值不正确")
		}
		args = append(args, st)
		where += " AND r.status = $1"
	}
	if cur != nil {
		args = append(args, cur.At, cur.ID)
		where += fmt.Sprintf(" AND (r.requested_at, r.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	fs, err := listRefunds(r.Context(), s.env.Pool, where, args, " LIMIT "+strconv.Itoa(limit+1))
	if err != nil {
		return nil, err
	}
	page := &kernel.Page[Refund]{Items: fs}
	if len(fs) > limit {
		page.Items = fs[:limit]
		last := fs[limit-1]
		page.NextCursor = kernel.Ptr(kernel.Cursor{At: last.RequestedAt, ID: last.ID}.Encode())
	}
	return page, nil
}
