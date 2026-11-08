package order

import (
	"net/http"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/subscription"
	"github.com/go-gost/gost/internal/control/task"
)

// CreateSubscriptionInput 是 OpenAPI AdminCreateSubscriptionInput。
type CreateSubscriptionInput struct {
	UserID        string `json:"userId"`
	PlanID        string `json:"planId"`
	PlanVersionID string `json:"planVersionId"`
	Months        int    `json:"months"`
	Name          string `json:"name"`
	NodeID        string `json:"nodeId"`
	ResourceID    string `json:"resourceId"`
	// City 是可选城市（D-27），规则同自助下单；指定 resourceId 时以该资源为准。
	City    *string      `json:"city"`
	Payment PaymentInput `json:"payment"`
}

// CreateSubscriptionResult 是 OpenAPI AdminCreateSubscriptionResult。
type CreateSubscriptionResult struct {
	Order        *AdminView                `json:"order"`
	Subscription *subscription.AdminDetail `json:"subscription"`
	Task         *task.View                `json:"task"`
}

// CreateSubscription 代建订阅（P0a）：与自助下单相同的订单 → 收款 → 入账 → 履约链路；无库存时订单进入“已收款，交付异常”。
func (s *Service) CreateSubscription(r *http.Request, tx *kernel.Tx, adminID string, in CreateSubscriptionInput) (*CreateSubscriptionResult, error) {
	ctx := r.Context()
	if in.UserID == "" || in.PlanID == "" {
		return nil, httpx.Invalid("userId 与 planId 必填")
	}
	if in.Name != "" {
		if _, ok := kernel.TrimName(in.Name); !ok {
			return nil, httpx.Invalid("名称需为 1～24 个字符")
		}
	}
	id, err := s.Create(ctx, tx, in.UserID, Input{Kind: "new", PlanID: in.PlanID, PlanVersionID: in.PlanVersionID, Months: in.Months, Quantity: 1, City: in.City},
		createOpts{AdminID: adminID, Gift: in.Payment.Method == "admin_gift", Name: in.Name, SkipStock: true})
	if err != nil {
		return nil, err
	}
	_, taskID, err := s.ConfirmPayment(ctx, tx, id, adminID, in.Payment, fulfillOpts{NodeID: in.NodeID, ResourceID: in.ResourceID})
	if err != nil {
		return nil, err
	}
	res := &CreateSubscriptionResult{}
	if res.Order, err = s.AdminGet(ctx, tx, id); err != nil {
		return nil, err
	}
	if res.Subscription, err = s.subs.AdminGet(ctx, tx, *res.Order.SubscriptionID); err != nil {
		return nil, err
	}
	if res.Task, err = task.GetOpt(ctx, tx, taskID); err != nil {
		return nil, err
	}
	return res, tx.Audit(ctx, adminID, "subscription.create", "subscription:"+*res.Order.SubscriptionID,
		map[string]any{"orderId": id, "userId": in.UserID, "planId": in.PlanID, "months": in.Months})
}

type fulfillmentInput struct {
	NodeID     string `json:"nodeId"`
	ResourceID string `json:"resourceId"`
}

// Routes 返回订单、收款、退款与代建订阅路由（用户端 5 个、管理端 10 个）。
func (s *Service) Routes() []httpx.Route {
	type up = httpx.Principal
	type ap = httpx.AdminPrincipal
	oid := func(r *http.Request) string { return r.PathValue("orderId") }
	adminTx := func(r *http.Request, id string, fn func(tx *kernel.Tx) error) (*AdminView, error) {
		var v *AdminView
		err := s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			var err error
			v, err = s.AdminGet(r.Context(), tx, id)
			return err
		})
		return v, err
	}
	userTx := func(r *http.Request, p up, fn func(tx *kernel.Tx) error) (*View, error) {
		var v *View
		err := s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			var err error
			v, err = s.Get(r.Context(), tx, oid(r), p.UserID)
			return err
		})
		return v, err
	}
	return []httpx.Route{
		// ---- 用户端 ----
		{Method: "GET", Pattern: "/api/orders", Auth: httpx.VerifiedUser, Handler: kernel.UserH(func(r *http.Request, p up) (int, any, error) {
			v, err := s.ListForUser(r.Context(), p.UserID)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/api/orders", Auth: httpx.VerifiedUser, Handler: kernel.UserW(func(r *http.Request, p up) (int, any, error) {
			in, err := kernel.Body[Input](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := kernel.Idempotent(r.Context(), s.env, "user:"+p.UserID+":order", httpx.IdempotencyKey(r), in,
				func(tx *kernel.Tx) (*View, error) {
					id, err := s.Create(r.Context(), tx, p.UserID, in, createOpts{})
					if err != nil {
						return nil, err
					}
					return s.Get(r.Context(), tx, id, p.UserID)
				})
			return 201, v, err
		})},
		{Method: "GET", Pattern: "/api/orders/{orderId}", Auth: httpx.VerifiedUser, Handler: kernel.UserH(func(r *http.Request, p up) (int, any, error) {
			v, err := s.Get(r.Context(), s.env.Pool, oid(r), p.UserID)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/api/orders/{orderId}/claim-paid", Auth: httpx.VerifiedUser, Handler: kernel.UserW(func(r *http.Request, p up) (int, any, error) {
			v, err := userTx(r, p, func(tx *kernel.Tx) error { return s.ClaimPaid(r.Context(), tx, oid(r), p.UserID) })
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/api/orders/{orderId}/cancel", Auth: httpx.VerifiedUser, Handler: kernel.UserW(func(r *http.Request, p up) (int, any, error) {
			v, err := userTx(r, p, func(tx *kernel.Tx) error { return s.Cancel(r.Context(), tx, oid(r), p.UserID, "", "") })
			return 200, v, err
		})},

		// ---- 管理端 ----
		{Method: "GET", Pattern: "/admin-api/orders", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.AdminList(r)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/orders", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[struct {
				UserID string `json:"userId"`
				Input  Input  `json:"input"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			if in.Input.Kind == "new" {
				return 0, nil, httpx.Invalid("新购请使用代建订阅（POST /admin-api/subscriptions）")
			}
			v, err := kernel.Idempotent(r.Context(), s.env, "admin:"+a.AdminID+":order", httpx.IdempotencyKey(r), in,
				func(tx *kernel.Tx) (*AdminView, error) {
					id, err := s.Create(r.Context(), tx, in.UserID, in.Input, createOpts{AdminID: a.AdminID})
					if err != nil {
						return nil, err
					}
					return s.AdminGet(r.Context(), tx, id)
				})
			return 201, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/orders/{orderId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.AdminGet(r.Context(), s.env.Pool, oid(r))
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/orders/{orderId}/payment-confirmation", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[PaymentInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := adminTx(r, oid(r), func(tx *kernel.Tx) error {
				_, _, err := s.ConfirmPayment(r.Context(), tx, oid(r), a.AdminID, in, fulfillOpts{})
				return err
			})
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/orders/{orderId}/cancel", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[struct {
				Reason string `json:"reason"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			if in.Reason == "" {
				return 0, nil, httpx.Invalid("请填写取消原因")
			}
			v, err := adminTx(r, oid(r), func(tx *kernel.Tx) error { return s.Cancel(r.Context(), tx, oid(r), "", a.AdminID, in.Reason) })
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/orders/{orderId}/fulfillment-retry", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[fulfillmentInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := adminTx(r, oid(r), func(tx *kernel.Tx) error {
				return s.RetryFulfillment(r.Context(), tx, oid(r), a.AdminID, in.NodeID, in.ResourceID)
			})
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/orders/{orderId}/refunds", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[RefundInput](r)
			if err != nil {
				return 0, nil, err
			}
			var v *Refund
			err = s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
				id, err := s.RequestRefund(r.Context(), tx, oid(r), a.AdminID, in)
				if err != nil {
					return err
				}
				v, err = getRefund(r.Context(), tx, id)
				return err
			})
			return 201, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/refunds", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.ListRefunds(r)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/refunds/{refundId}/decision", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[struct {
				Approve     *bool  `json:"approve"`
				Note        string `json:"note"`
				ExternalRef string `json:"externalRef"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			if in.Approve == nil {
				return 0, nil, httpx.Invalid("approve 必填")
			}
			id := r.PathValue("refundId")
			var v *Refund
			err = s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
				if err := s.DecideRefund(r.Context(), tx, id, a.AdminID, *in.Approve, in.Note, in.ExternalRef); err != nil {
					return err
				}
				var err error
				v, err = getRefund(r.Context(), tx, id)
				return err
			})
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/subscriptions", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[CreateSubscriptionInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := kernel.Idempotent(r.Context(), s.env, "admin:"+a.AdminID+":create_subscription", httpx.IdempotencyKey(r), in,
				func(tx *kernel.Tx) (*CreateSubscriptionResult, error) {
					return s.CreateSubscription(r, tx, a.AdminID, in)
				})
			return 201, v, err
		})},
	}
}
