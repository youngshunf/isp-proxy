package subscription

import (
	"net/http"
	"strconv"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/ledger"
	"github.com/go-gost/gost/internal/control/task"
)

type reasonInput struct {
	Reason string `json:"reason"`
}

func (s *Service) adminDetailTx(r *http.Request, id string, fn func(tx *kernel.Tx) error) (*AdminDetail, error) {
	var d *AdminDetail
	err := s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		var err error
		d, err = s.AdminGet(r.Context(), tx, id)
		return err
	})
	return d, err
}

// Routes 返回订阅相关路由（用户端 6 个、管理端 15 个，含用户详情里的直出代理赠送与收回；代建订阅在 order 包，任务与探测在 task / probe 包）。
func (s *Service) Routes() []httpx.Route {
	type up = httpx.Principal
	type ap = httpx.AdminPrincipal
	sid := func(r *http.Request) string { return r.PathValue("subscriptionId") }
	return append(s.directRoutes(), []httpx.Route{
		// ---- 用户端 ----
		{Method: "GET", Pattern: "/api/subscriptions", Auth: httpx.VerifiedUser, Handler: kernel.UserH(func(r *http.Request, p up) (int, any, error) {
			v, err := s.ListForUser(r.Context(), p.UserID)
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/api/subscriptions/{subscriptionId}", Auth: httpx.VerifiedUser, Handler: kernel.UserH(func(r *http.Request, p up) (int, any, error) {
			v, err := s.Get(r.Context(), s.env.Pool, sid(r), p.UserID)
			return 200, v, err
		})},
		{Method: "PATCH", Pattern: "/api/subscriptions/{subscriptionId}", Auth: httpx.VerifiedUser, Handler: kernel.UserW(func(r *http.Request, p up) (int, any, error) {
			in, err := kernel.Body[struct {
				Name string `json:"name"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			var v *View
			err = s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
				if err := s.Rename(r.Context(), tx, sid(r), p.UserID, in.Name, "user", &p.UserID); err != nil {
					return err
				}
				var err error
				v, err = s.Get(r.Context(), tx, sid(r), p.UserID)
				return err
			})
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/api/subscriptions/{subscriptionId}/events", Auth: httpx.VerifiedUser, Handler: kernel.UserH(func(r *http.Request, p up) (int, any, error) {
			v, err := UserEvents(r.Context(), s.env.Pool, sid(r), p.UserID)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/api/subscriptions/{subscriptionId}/access-changes", Auth: httpx.VerifiedUser,
			Handler: kernel.UserW(func(r *http.Request, p up) (int, any, error) {
				in, err := kernel.Body[struct {
					Mode string `json:"mode"`
				}](r)
				if err != nil {
					return 0, nil, err
				}
				id := sid(r)
				v, err := kernel.Idempotent(r.Context(), s.env, "user:"+p.UserID+":change_access", httpx.IdempotencyKey(r),
					map[string]any{"sub": id, "mode": in.Mode}, func(tx *kernel.Tx) (*Access, error) {
						if _, err := s.tasks.ChangeAccess(r.Context(), tx, id, in.Mode, task.UserActor(p.UserID), ""); err != nil {
							return nil, err
						}
						x, err := loadRow(r.Context(), tx, tx.Now(), id, p.UserID)
						if err != nil {
							return nil, err
						}
						a, err := displayAccess(r.Context(), tx, x)
						if err != nil {
							return nil, err
						}
						return s.accessView(a, x)
					})
				return 200, v, err
			})},
		{Method: "POST", Pattern: "/api/subscriptions/{subscriptionId}/access/ack", Auth: httpx.VerifiedUser,
			Handler: kernel.UserW(func(r *http.Request, p up) (int, any, error) {
				err := s.env.InTx(r.Context(), func(tx *kernel.Tx) error { return s.AckAccess(r.Context(), tx, sid(r), p.UserID) })
				return 204, nil, err
			})},

		// ---- 管理端 ----
		{Method: "GET", Pattern: "/admin-api/subscriptions", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			f, err := parseFilter(r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.AdminList(r.Context(), f)
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/subscriptions/{subscriptionId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.AdminGet(r.Context(), s.env.Pool, sid(r))
			return 200, v, err
		})},
		{Method: "PATCH", Pattern: "/admin-api/subscriptions/{subscriptionId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[struct {
				Name string `json:"name"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.adminDetailTx(r, sid(r), func(tx *kernel.Tx) error {
				if err := s.Rename(r.Context(), tx, sid(r), "", in.Name, "admin", &a.AdminID); err != nil {
					return err
				}
				return tx.Audit(r.Context(), a.AdminID, "subscription.rename", "subscription:"+sid(r), in)
			})
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/pause", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[reasonInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.adminDetailTx(r, sid(r), func(tx *kernel.Tx) error { return s.Pause(r.Context(), tx, sid(r), a.AdminID, in.Reason) })
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/resume", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			v, err := s.adminDetailTx(r, sid(r), func(tx *kernel.Tx) error { return s.Resume(r.Context(), tx, sid(r), a.AdminID) })
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/deletion", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[struct {
				Reason                      string `json:"reason"`
				AcknowledgeRemainingTraffic bool   `json:"acknowledgeRemainingTraffic"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.adminDetailTx(r, sid(r), func(tx *kernel.Tx) error {
				return s.Delete(r.Context(), tx, sid(r), a.AdminID, in.Reason, in.AcknowledgeRemainingTraffic)
			})
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/access/reveal", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[reasonInput](r)
			if err != nil {
				return 0, nil, err
			}
			var v *AccessSecret
			err = s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
				var err error
				v, err = s.Reveal(r.Context(), tx, sid(r), a.AdminID, in.Reason)
				return err
			})
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/subscriptions/{subscriptionId}/access-versions", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := AccessVersions(r.Context(), s.env.Pool, sid(r))
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/subscriptions/{subscriptionId}/exit-bindings", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := ExitBindings(r.Context(), s.env.Pool, sid(r))
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/subscriptions/{subscriptionId}/ledger", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := ledger.Load(r.Context(), s.env.Pool, sid(r))
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/ledger-entries", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[LedgerEntryInput](r)
			if err != nil {
				return 0, nil, err
			}
			id := sid(r)
			v, err := kernel.Idempotent(r.Context(), s.env, "admin:"+a.AdminID+":ledger_entry", httpx.IdempotencyKey(r),
				map[string]any{"sub": id, "in": in}, func(tx *kernel.Tx) (*ledger.EntryView, error) {
					return s.AddLedgerEntry(r.Context(), tx, id, a.AdminID, in)
				})
			return 201, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/subscriptions/{subscriptionId}/events", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := AdminEvents(r.Context(), s.env.Pool, sid(r))
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/subscriptions/{subscriptionId}/usage", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			from, err := kernel.TimeParam(r, "from")
			if err != nil {
				return 0, nil, err
			}
			to, err := kernel.TimeParam(r, "to")
			if err != nil {
				return 0, nil, err
			}
			if from == nil || to == nil {
				return 0, nil, httpx.Invalid("from 与 to 必填")
			}
			v, err := s.Usage(r.Context(), sid(r), *from, *to, r.URL.Query().Get("bucket"))
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/transfers", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[struct {
				From   string `json:"fromSubscriptionId"`
				To     string `json:"toSubscriptionId"`
				Reason string `json:"reason"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			if in.From == "" || in.To == "" || in.Reason == "" {
				return 0, nil, httpx.Invalid("fromSubscriptionId、toSubscriptionId 与 reason 必填")
			}
			if in.From == in.To {
				return 0, nil, httpx.Invalid("来源与目标不能是同一个订阅")
			}
			v, err := kernel.Idempotent(r.Context(), s.env, "admin:"+a.AdminID+":transfer", httpx.IdempotencyKey(r), in,
				func(tx *kernel.Tx) (*ledger.TransferView, error) {
					t, err := ledger.Transfer(r.Context(), tx, in.From, in.To, in.Reason, a.AdminID)
					if err != nil {
						return nil, err
					}
					if err := tx.Audit(r.Context(), a.AdminID, "ledger.transfer", "transfer:"+t.ID, in); err != nil {
						return nil, err
					}
					return t, nil
				})
			return 201, v, err
		})},
	}...)
}

func parseFilter(r *http.Request) (ListFilter, error) {
	cur, limit, err := kernel.ParsePage(r)
	if err != nil {
		return ListFilter{}, err
	}
	q := r.URL.Query()
	f := ListFilter{Q: q.Get("q"), UserID: q.Get("userId"), NodeID: q.Get("nodeId"), Cursor: cur, Limit: limit}
	if f.Lifecycle, err = kernel.ListParam(r, "lifecycle", "pending", "fulfill_failed", "active", "retained", "released", "deleted"); err != nil {
		return f, err
	}
	if b := q.Get("blocker"); b != "" {
		switch b {
		case "account_banned", "released", "admin_paused", "pending", "fulfill_failed", "expired", "no_paid_subscription", "traffic_exhausted", "changing",
			"dataplane":
			f.Blocker = b
		default:
			return f, httpx.Invalid("blocker 取值不正确")
		}
	}
	switch k := q.Get("kind"); k {
	case "", "isp", "direct":
		f.Kind = k
	default:
		return f, httpx.Invalid("kind 只能是 isp 或 direct")
	}
	if u := q.Get("usable"); u != "" {
		b, err := strconv.ParseBool(u)
		if err != nil {
			return f, httpx.Invalid("usable 只能是 true 或 false")
		}
		f.Usable = &b
	}
	if f.ExpiresBefore, err = kernel.TimeParam(r, "expiresBefore"); err != nil {
		return f, err
	}
	return f, nil
}
