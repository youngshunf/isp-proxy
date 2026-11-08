// Package order 是订单、收款、退款与晚到款（C-W3，R-09、R-16；docs/06 §8.1）。
//
// 不变量：
//   - 确认收款与入账在同一事务内，以订单为唯一业务键：payments.order_id 唯一、账本来源键 (order, 订单, 种类) 唯一；
//     并发确认只有一个写入，其余读到已有记录：请求体相同返回 200，不同返回 409（R-09.3）。
//   - 新购下单对用户行加锁再数未付款新购，保证“每用户最多 3 个”在并发下成立；库存预留用 SKIP LOCKED，
//     最后一份资源只会被一个订单拿到（A-27、A-38）。
//   - 订单金额、权益增量在下单时写入订单行与规则快照，之后改价或下架不追溯（R-09.9）。
package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-gost/gost/internal/control/alert"
	"github.com/go-gost/gost/internal/control/catalog"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/inventory"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/ledger"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/subscription"
	"github.com/go-gost/gost/internal/control/task"
)

// Service 提供订单。
type Service struct {
	env   *kernel.Env
	tasks *task.Service
	subs  *subscription.Service
}

// New 创建服务。
func New(env *kernel.Env, tasks *task.Service, subs *subscription.Service) *Service {
	return &Service{env: env, tasks: tasks, subs: subs}
}

// Input 是 OpenAPI OrderInput（三种形状合一，按 kind 校验）。
type Input struct {
	Kind           string `json:"kind"`
	PlanID         string `json:"planId,omitempty"`
	PlanVersionID  string `json:"planVersionId,omitempty"`
	Months         int    `json:"months,omitempty"`
	Quantity       int    `json:"quantity,omitempty"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
	PackID         string `json:"packId,omitempty"`
	PackVersionID  string `json:"packVersionId,omitempty"`
}

type line struct {
	Kind          string       `json:"kind"`
	PlanVersionID *string      `json:"planVersionId,omitempty"`
	PackVersionID *string      `json:"packVersionId,omitempty"`
	Label         string       `json:"label"`
	Detail        string       `json:"detail"`
	Amount        kernel.Money `json:"amount"`
	Seconds       int64        `json:"seconds"`
	Bytes         int64        `json:"bytes"`
}

// createOpts 是下单的附加参数。
type createOpts struct {
	AdminID   string // 管理员代下单
	Gift      bool   // 代建赠送：金额为 0
	Name      string // 新购订阅名称
	SkipStock bool   // 代建订阅：不预留（确认后立即履约并按指定资源分配）
}

// Create 下单（用户或管理员代下单）。返回订单 ID。
func (s *Service) Create(ctx context.Context, tx *kernel.Tx, userID string, in Input, o createOpts) (string, error) {
	now := tx.Now()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&status); err != nil {
		if kernel.IsNoRows(err) {
			return "", httpx.NotFound("没有找到这个用户")
		}
		return "", err
	}
	if status == "banned" {
		return "", httpx.Precondition("账号已被封禁，不能下单")
	}
	rules, err := kernel.CurrentRules(ctx, tx, now)
	if err != nil {
		return "", err
	}
	if o.AdminID == "" && !rules.PaymentConfigured() {
		return "", httpx.Precondition("暂未开放购买：收款说明尚未配置")
	}
	var lines []line
	var subID, planID, planVersionID *string
	var months *int
	var country, city string
	switch in.Kind {
	case "new":
		if in.Quantity != 1 && !(o.AdminID != "" && in.Quantity == 0) {
			return "", httpx.Invalid("一次订单只能购买一个订阅（quantity 必须为 1）")
		}
		if !rules.MonthAllowed(in.Months) {
			return "", httpx.Invalid(fmt.Sprintf("购买时长只能是 %v 个月", rules.MonthOptions))
		}
		pv, st, err := catalog.CurrentPlanVersion(ctx, tx, in.PlanID)
		if err != nil {
			return "", err
		}
		if st != "on_sale" && o.AdminID == "" {
			return "", httpx.NotFound("套餐不存在或已下架")
		}
		if in.PlanVersionID != "" && in.PlanVersionID != pv.ID {
			return "", httpx.Conflict("套餐价格或内容已变化，请刷新后重新下单")
		}
		if o.AdminID == "" {
			var unpaid int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM orders WHERE user_id = $1 AND kind = 'new' AND payment_status = 'awaiting'`,
				userID).Scan(&unpaid); err != nil {
				return "", err
			}
			if unpaid >= rules.MaxUnpaidNewOrders {
				return "", httpx.Conflict(fmt.Sprintf("最多同时保留 %d 个未付款的新购订单，请先付款或取消已有订单", rules.MaxUnpaidNewOrders))
			}
		}
		amount := pv.Price.Amount * int64(in.Months)
		if o.Gift {
			amount = 0
		}
		lines = append(lines, line{Kind: "plan", PlanVersionID: &pv.ID, Label: pv.Name,
			Detail: fmt.Sprintf("%d 个月（%d 天），含流量 %s", in.Months, in.Months*pv.PeriodDays, kernel.FormatBytes(pv.IncludedBytes*int64(in.Months))),
			Amount: kernel.CNY(amount), Seconds: int64(in.Months*pv.PeriodDays) * 86400, Bytes: pv.IncludedBytes * int64(in.Months)})
		planID, planVersionID, months = &pv.PlanID, &pv.ID, &in.Months
		country, city = pv.CountryCode, pv.City
	case "renew_time", "add_traffic":
		if in.SubscriptionID == "" {
			return "", httpx.Invalid("subscriptionId 必填")
		}
		var owner, lifecycle string
		var planOf *string
		err := tx.QueryRow(ctx, `SELECT user_id, lifecycle, plan_id FROM subscriptions WHERE id = $1 FOR UPDATE`, in.SubscriptionID).
			Scan(&owner, &lifecycle, &planOf)
		if kernel.IsNoRows(err) || (err == nil && (owner != userID || lifecycle == "deleted")) {
			return "", httpx.NotFound("没有找到这个订阅")
		}
		if err != nil {
			return "", err
		}
		if planOf == nil {
			// 直出代理（D-25）没有套餐：不限流量、跟随付费订阅有效。
			return "", httpx.Precondition("直出代理跟随你的付费 IP 有效且不限流量，不能续时长或加流量")
		}
		subPlan := *planOf
		subID = &in.SubscriptionID
		if in.Kind == "renew_time" {
			if lifecycle != "active" && lifecycle != "retained" && lifecycle != "released" {
				return "", httpx.Precondition("订阅尚未开通，暂不能续时长")
			}
			if !rules.MonthAllowed(in.Months) {
				return "", httpx.Invalid(fmt.Sprintf("续费时长只能是 %v 个月", rules.MonthOptions))
			}
			pv, _, err := catalog.CurrentPlanVersion(ctx, tx, subPlan)
			if err != nil {
				return "", err
			}
			amount := pv.Price.Amount * int64(in.Months)
			detail := fmt.Sprintf("续 %d 个月（%d 天）", in.Months, in.Months*pv.PeriodDays)
			if lifecycle == "released" {
				detail += "；已释放，付款后重新开通（新出口、新接入），剩余流量保留"
			}
			lines = append(lines, line{Kind: "renew_time", PlanVersionID: &pv.ID, Label: "续时长", Detail: detail,
				Amount: kernel.CNY(amount), Seconds: int64(in.Months*pv.PeriodDays) * 86400})
			months = &in.Months
		} else if in.PackID == "" {
			return "", httpx.Invalid("packId 必填")
		}
		if in.PackID != "" {
			tv, st, err := catalog.CurrentPackVersion(ctx, tx, in.PackID)
			if err != nil {
				return "", err
			}
			if st != "on_sale" && o.AdminID == "" {
				return "", httpx.NotFound("流量包不存在或已下架")
			}
			if in.PackVersionID != "" && in.PackVersionID != tv.ID {
				return "", httpx.Conflict("流量包价格已变化，请刷新后重新下单")
			}
			lines = append(lines, line{Kind: "traffic_pack", PackVersionID: &tv.ID, Label: "流量包",
				Detail: "加流量 " + kernel.FormatBytes(tv.Bytes) + "，不改变到期时间", Amount: tv.Price, Bytes: tv.Bytes})
		}
	default:
		return "", httpx.Invalid("kind 只能是 new、renew_time 或 add_traffic")
	}
	var total int64
	for _, l := range lines {
		total += l.Amount.Amount
	}
	id := store.NewID("ord")
	hold := now.Add(time.Duration(rules.UnpaidHoldHours) * time.Hour)
	method := "manual_transfer"
	if o.Gift {
		method = "admin_gift"
	}
	snapshot := map[string]any{"lines": lines, "rules": rules.SnapshotLines, "name": o.Name}
	var admin *string
	if o.AdminID != "" {
		admin = &o.AdminID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO orders (id, user_id, kind, subscription_id, plan_id, plan_version_id, months, amount_cents,
	                            payment_method, rules_version, rules_snapshot, hold_until, created_by_admin, created_at)
	                          VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		id, userID, in.Kind, subID, planID, planVersionID, months, total, method, rules.Version, snapshot, hold, admin, now); err != nil {
		return "", err
	}
	for i, l := range lines {
		if _, err := tx.Exec(ctx, `INSERT INTO order_lines (order_id, line_no, item_kind, plan_version_id, traffic_pack_version_id, label,
		                            detail, amount_cents, seconds_delta, bytes_delta) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			id, i+1, l.Kind, l.PlanVersionID, l.PackVersionID, l.Label, l.Detail, l.Amount.Amount, l.Seconds, l.Bytes); err != nil {
			return "", err
		}
	}
	if in.Kind == "new" && !o.SkipStock {
		if _, err := inventory.ReserveForOrder(ctx, tx, country, city, id, hold); err != nil {
			if errors.Is(err, inventory.ErrNoStock) {
				return "", httpx.Conflict("该地区暂时无货，请稍后再试或选择其他地区")
			}
			return "", err
		}
	}
	if admin != nil {
		if err := tx.Audit(ctx, o.AdminID, "order.create", "order:"+id, in); err != nil {
			return "", err
		}
	}
	return id, nil
}

// ---- 收款 ----

// PaymentInput 是 OpenAPI ConfirmPaymentInput。
type PaymentInput struct {
	Method      string       `json:"method"`
	Amount      kernel.Money `json:"amount"`
	ExternalRef string       `json:"externalRef"`
	Note        string       `json:"note"`
	LateArrival bool         `json:"lateArrival"`
}

type ord struct {
	ID, UserID, Kind, Payment, Fulfillment, Method string
	Sub                                            *string
	Amount                                         int64
	CreatedByAdmin                                 *string
	ClaimedAt                                      *time.Time
	PlanVersionID                                  *string
}

func lockOrder(ctx context.Context, tx *kernel.Tx, id, userID string) (*ord, error) {
	o := &ord{}
	err := tx.QueryRow(ctx, `SELECT id, user_id, kind, payment_status, fulfillment_status, payment_method, subscription_id, amount_cents,
	                            created_by_admin, user_claimed_paid_at, plan_version_id FROM orders WHERE id = $1 FOR UPDATE`, id).
		Scan(&o.ID, &o.UserID, &o.Kind, &o.Payment, &o.Fulfillment, &o.Method, &o.Sub, &o.Amount, &o.CreatedByAdmin, &o.ClaimedAt, &o.PlanVersionID)
	if kernel.IsNoRows(err) || (err == nil && userID != "" && o.UserID != userID) {
		return nil, httpx.NotFound("没有找到这个订单")
	}
	return o, err
}

// fulfillOpts 是确认收款后履约的附加参数（代建订阅指定节点/资源）。
type fulfillOpts struct {
	NodeID, ResourceID string
}

// ConfirmPayment 确认收款并入账（以订单为唯一业务键，幂等）。返回本次是否新确认、新建的订阅与履约任务（可能为空）。
func (s *Service) ConfirmPayment(ctx context.Context, tx *kernel.Tx, id, adminID string, in PaymentInput, fo fulfillOpts) (bool, string, error) {
	if in.Method != "manual_transfer" && in.Method != "admin_gift" {
		return false, "", httpx.Invalid("method 只能是 manual_transfer 或 admin_gift")
	}
	if in.Amount.Currency != "" && in.Amount.Currency != "CNY" {
		return false, "", httpx.Invalid("币种只能是 CNY")
	}
	if n := len([]rune(in.ExternalRef)); n < 1 || n > 200 {
		return false, "", httpx.Invalid("请填写外部收款参考（1～200 字）")
	}
	if len([]rune(in.Note)) > 500 {
		return false, "", httpx.Invalid("备注不超过 500 字")
	}
	o, err := lockOrder(ctx, tx, id, "")
	if err != nil {
		return false, "", err
	}
	var pAmount int64
	var pMethod, pRef string
	var pLate bool
	err = tx.QueryRow(ctx, `SELECT amount_cents, method, external_ref, late_arrival FROM payments WHERE order_id = $1`, id).
		Scan(&pAmount, &pMethod, &pRef, &pLate)
	if err == nil {
		if pAmount == in.Amount.Amount && pMethod == in.Method && pRef == in.ExternalRef {
			return false, "", nil
		}
		return false, "", httpx.Conflict("该订单已确认收款，且与本次填写的金额或流水号不同")
	}
	if !kernel.IsNoRows(err) {
		return false, "", err
	}
	late := false
	switch o.Payment {
	case "awaiting":
	case "cancelled":
		if !in.LateArrival {
			return false, "", httpx.Precondition("订单已取消或超时；如确实晚到款，请勾选“晚到款”后确认，进入人工处置")
		}
		late = true
	default:
		return false, "", httpx.Precondition("订单当前状态不能确认收款")
	}
	if in.Amount.Amount != o.Amount {
		return false, "", httpx.Invalid(fmt.Sprintf("收款金额必须等于订单金额 %.2f 元", float64(o.Amount)/100))
	}
	if in.Method == "admin_gift" && (o.Amount != 0 || o.CreatedByAdmin == nil) {
		return false, "", httpx.Invalid("赠送只能用于管理员代建、金额为 0 的订单")
	}
	now := tx.Now()
	if _, err := tx.Exec(ctx, `INSERT INTO payments (id, order_id, amount_cents, method, external_ref, late_arrival, note, confirmed_by, confirmed_at)
	                          VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		store.NewID("pay"), id, in.Amount.Amount, in.Method, in.ExternalRef, late, in.Note, adminID, now); err != nil {
		if store.IsUniqueViolation(err, "") {
			return false, "", httpx.Conflict("该订单已确认收款")
		}
		return false, "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET payment_status = 'confirmed', confirmed_at = $2, row_version = row_version + 1 WHERE id = $1`,
		id, now); err != nil {
		return false, "", err
	}
	if err := inventory.ReleaseOrderReservation(ctx, tx, id); err != nil && !late {
		return false, "", err
	}
	taskID, err := s.credit(ctx, tx, o, adminID, late, fo)
	if err != nil {
		return false, "", err
	}
	if err := tx.Audit(ctx, adminID, "order.confirm_payment", "order:"+id,
		map[string]any{"amount": in.Amount.Amount, "method": in.Method, "externalRef": in.ExternalRef, "lateArrival": late}); err != nil {
		return false, "", err
	}
	return true, taskID, nil
}

// credit 入账：新购创建订阅并启动履约；续购各项权益分别入账（R-18 含流量续费）。
func (s *Service) credit(ctx context.Context, tx *kernel.Tx, o *ord, adminID string, late bool, fo fulfillOpts) (string, error) {
	now := tx.Now()
	var secs, bytes int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(seconds_delta), 0), COALESCE(sum(bytes_delta), 0) FROM order_lines WHERE order_id = $1`,
		o.ID).Scan(&secs, &bytes); err != nil {
		return "", err
	}
	var no string
	if err := tx.QueryRow(ctx, `SELECT no FROM orders WHERE id = $1`, o.ID).Scan(&no); err != nil {
		return "", err
	}
	subID := kernel.StrOr(o.Sub, "")
	var lifecycle string
	if o.Kind == "new" {
		var name, planID, rulesVersion string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(o.rules_snapshot->>'name', ''), pv.name), o.plan_id, o.rules_version
		                             FROM orders o JOIN plan_versions pv ON pv.id = o.plan_version_id WHERE o.id = $1`, o.ID).
			Scan(&name, &planID, &rulesVersion); err != nil {
			return "", err
		}
		name, _ = kernel.TrimName(kernel.TruncateRunes(name, 24))
		subID = store.NewID("sub")
		if _, err := tx.Exec(ctx, `INSERT INTO subscriptions (id, user_id, plan_id, plan_version_id, rules_version, created_from_order_id, name, created_at)
		                            VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			subID, o.UserID, planID, *o.PlanVersionID, rulesVersion, o.ID, name, now); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET subscription_id = $2 WHERE id = $1`, o.ID, subID); err != nil {
			return "", err
		}
		lifecycle = "pending"
	} else {
		if err := tx.QueryRow(ctx, `SELECT lifecycle FROM subscriptions WHERE id = $1 FOR UPDATE`, subID).Scan(&lifecycle); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE subscriptions SET rules_version = (SELECT rules_version FROM orders WHERE id = $2),
		                            row_version = row_version + 1 WHERE id = $1 AND lifecycle <> 'deleted'`, subID, o.ID); err != nil {
			return "", err
		}
	}
	if lifecycle == "deleted" {
		// 订阅已删除后的晚到款：不入账，人工退款。
		return "", s.lateAlert(ctx, tx, o, subID, "订阅已删除，款项需人工退款", true)
	}
	adminPtr := &adminID
	for _, e := range []struct {
		kind   string
		amount int64
	}{{"time", secs}, {"traffic", bytes}} {
		if e.amount == 0 {
			continue
		}
		ledgerID, _, err := ledger.Insert(ctx, tx, ledger.Entry{Sub: subID, Kind: e.kind, Amount: e.amount, SourceType: "order",
			SourceID: o.ID, OrderID: &o.ID, Reason: "订单 " + no, AdminID: adminPtr})
		if err != nil {
			return "", err
		}
		ev := kernel.Event{Sub: subID, OrderID: &o.ID, LedgerID: &ledgerID, ActorKind: "admin", ActorID: adminPtr}
		if e.kind == "time" {
			ev.Kind, ev.Detail = "time_added", fmt.Sprintf("时长 +%d 天（订单 %s）", e.amount/86400, no)
		} else {
			ev.Kind, ev.Detail = "traffic_added", fmt.Sprintf("流量 +%s（订单 %s）", kernel.FormatBytes(e.amount), no)
		}
		if err := tx.AddEvent(ctx, ev); err != nil {
			return "", err
		}
	}
	fopts := task.FulfillOpts{OrderID: o.ID, NodeID: fo.NodeID, ResourceID: fo.ResourceID, Actor: task.AdminActor(adminID), MarkOnFail: true}
	switch {
	case o.Kind == "new" && late:
		// 晚到款新购：先建订阅并入账，不自动开通，管理员决定重新履约或退款（R-16.6）。
		if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'fulfill_failed', lifecycle_reason = '晚到款，待人工处置' WHERE id = $1`, subID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET fulfillment_status = 'failed', fulfillment_error = '已收款（订单取消后到账），交付待人工处置' WHERE id = $1`,
			o.ID); err != nil {
			return "", err
		}
		return "", s.lateAlert(ctx, tx, o, subID, "新购订单取消后到账，订阅已入账但未开通，需重新履约或退款", false)
	case o.Kind == "new":
		return s.tasks.Fulfill(ctx, tx, subID, fopts)
	}
	// 续购
	if err := s.subs.AfterCredit(ctx, tx, subID); err != nil {
		return "", err
	}
	var taskID string
	if lifecycle == "released" && secs > 0 {
		id, err := s.tasks.StartRefulfill(ctx, tx, subID, fopts)
		if err != nil {
			return "", err
		}
		taskID = id
		if id == "" {
			// 无库存等：已记为交付异常。
			return "", nil
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE orders SET fulfillment_status = 'delivered', delivered_at = $2 WHERE id = $1`, o.ID, now); err != nil {
			return "", err
		}
	}
	if late {
		if err := s.lateAlert(ctx, tx, o, subID, "续购订单取消后到账，已正常入账", false); err != nil {
			return "", err
		}
	}
	return taskID, nil
}

func (s *Service) lateAlert(ctx context.Context, tx *kernel.Tx, o *ord, subID, msg string, failOrder bool) error {
	if failOrder {
		if _, err := tx.Exec(ctx, `UPDATE orders SET fulfillment_status = 'failed', fulfillment_error = $2 WHERE id = $1`, o.ID,
			"已收款（订单取消后到账），"+msg); err != nil {
			return err
		}
	}
	var sub *string
	if subID != "" {
		sub = &subID
	}
	return alert.Raise(ctx, tx, tx.Now(), alert.Alert{Kind: "late_payment", Severity: "warning", Key: "late_payment:" + o.ID,
		Message: "晚到款：" + msg, Order: &o.ID, Sub: sub})
}

// ---- 取消、反馈付款、超时 ----

// Cancel 取消待核款订单并释放库存预留。user 取消要求尚未反馈付款。
func (s *Service) Cancel(ctx context.Context, tx *kernel.Tx, id, userID, adminID, reason string) error {
	o, err := lockOrder(ctx, tx, id, userID)
	if err != nil {
		return err
	}
	if o.Payment != "awaiting" {
		return httpx.Precondition("只有待核款的订单可以取消")
	}
	by := "admin"
	if adminID == "" {
		by = "user"
		if o.ClaimedAt != nil {
			return httpx.Precondition("你已反馈付款，订单不能自行取消；如需取消请联系客服")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET payment_status = 'cancelled', cancelled_at = $2, cancel_reason = $3, row_version = row_version + 1
	                            WHERE id = $1`, id, tx.Now(), by); err != nil {
		return err
	}
	if err := inventory.ReleaseOrderReservation(ctx, tx, id); err != nil {
		return err
	}
	if adminID != "" {
		return tx.Audit(ctx, adminID, "order.cancel", "order:"+id, map[string]any{"reason": reason})
	}
	return nil
}

// ClaimPaid 记录用户“已转账”反馈（只是核对线索，不改变收款状态，R-09.2）。重复反馈保留首次时间。
func (s *Service) ClaimPaid(ctx context.Context, tx *kernel.Tx, id, userID string) error {
	o, err := lockOrder(ctx, tx, id, userID)
	if err != nil {
		return err
	}
	if o.Payment != "awaiting" {
		return httpx.Precondition("只有待核款的订单可以反馈付款")
	}
	_, err = tx.Exec(ctx, `UPDATE orders SET user_claimed_paid_at = COALESCE(user_claimed_paid_at, $2), row_version = row_version + 1
	                        WHERE id = $1`, id, tx.Now())
	return err
}

// ExpireHolds 把超过保留期仍未付款的订单标记为超时取消并释放预留（调度）。
func (s *Service) ExpireHolds(ctx context.Context) error {
	return s.env.InTx(ctx, func(tx *kernel.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE orders SET payment_status = 'cancelled', cancelled_at = $1, cancel_reason = 'timeout',
		                              row_version = row_version + 1
		                             WHERE id IN (SELECT id FROM orders WHERE payment_status = 'awaiting' AND hold_until <= $1
		                                          ORDER BY hold_until LIMIT 500 FOR UPDATE SKIP LOCKED) RETURNING id`, tx.Now())
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
		for _, id := range ids {
			if err := inventory.ReleaseOrderReservation(ctx, tx, id); err != nil {
				return err
			}
		}
		// 资源预留到期但订单仍未处理（例如代建失败遗留）：释放。
		_, err = tx.Exec(ctx, `UPDATE upstream_resources SET status = 'available', reserved_order_id = NULL, reserved_until = NULL,
		                         row_version = row_version + 1
		                        WHERE status = 'reserved' AND reserved_order_id IS NOT NULL AND reserved_until <= $1`, tx.Now())
		return err
	})
}

// RetryFulfillment 重试“已收款，交付异常”的订单。
func (s *Service) RetryFulfillment(ctx context.Context, tx *kernel.Tx, id, adminID, nodeID, resID string) error {
	o, err := lockOrder(ctx, tx, id, "")
	if err != nil {
		return err
	}
	if o.Payment != "confirmed" || o.Sub == nil || (o.Fulfillment != "failed" && o.Fulfillment != "processing") {
		return httpx.Precondition("只有已收款且交付异常的订单可以重试履约")
	}
	if _, err := s.tasks.RetryFulfillment(ctx, tx, *o.Sub, nodeID, resID, adminID); err != nil {
		return err
	}
	return tx.Audit(ctx, adminID, "order.retry_fulfillment", "order:"+id, map[string]any{"nodeId": nodeID, "resourceId": resID})
}
