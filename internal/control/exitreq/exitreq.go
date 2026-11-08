// Package exitreq 是用户换出口申请（C-W11，R-17、D-19）。
//
// 每订阅滚动 30 天 1 次免费（按订阅最近一次购买时的规则版本）；超出的申请 charge = manual，必须由管理员先决定
// 收费（charged）或免收（waived）才进入处理。同一订阅同时只有一个未完成申请（部分唯一索引 exit_requests_one_open）。
// 实际切换由管理员按“比较候选 → 提交换出口（带 exitRequestId）”执行，任务成功时自动把申请标为成功（task 包）。
package exitreq

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/task"
)

// Service 提供换出口申请。
type Service struct{ env *kernel.Env }

// New 创建服务。
func New(env *kernel.Env) *Service { return &Service{env: env} }

// View 是 OpenAPI ExitRequest（客户可见）。
type View struct {
	ID               string    `json:"id"`
	No               string    `json:"no"`
	SubscriptionID   string    `json:"subscriptionId"`
	SubscriptionName string    `json:"subscriptionName"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Reason           string    `json:"reason"`
	Note             string    `json:"note"`
	AlsoChangeAccess bool      `json:"alsoChangeAccess"`
	Charge           string    `json:"charge"`
	Status           string    `json:"status"`
	OldIP            string    `json:"oldIp"`
	NewIP            *string   `json:"newIp"`
	FailureReason    *string   `json:"failureReason"`
}

// AdminView 是 OpenAPI AdminExitRequest。
type AdminView struct {
	View
	User           kernel.UserRef   `json:"user"`
	FeeStatus      string           `json:"feeStatus"`
	FeeAmount      *kernel.Money    `json:"feeAmount"`
	FeeExternalRef *string          `json:"feeExternalRef"`
	InternalNote   string           `json:"internalNote"`
	TaskID         *string          `json:"taskId"`
	HandledBy      *kernel.AdminRef `json:"handledBy"`
	AcceptedAt     *time.Time       `json:"acceptedAt"`
	FinishedAt     *time.Time       `json:"finishedAt"`
}

const cols = `e.id, e.no, e.subscription_id, s.name, e.created_at, e.updated_at, e.reason, e.note, e.also_change_access, e.charge,
	e.status, COALESCE(host(e.old_ip), ''), host(e.new_ip), e.failure_reason, e.user_id, u.email, e.fee_status, e.fee_amount_cents,
	e.fee_external_ref, e.internal_note, e.task_id, e.handled_by, a.email, e.accepted_at, e.finished_at`

const from = ` FROM exit_requests e JOIN subscriptions s ON s.id = e.subscription_id JOIN users u ON u.id = e.user_id
	LEFT JOIN admins a ON a.id = e.handled_by`

func scan(row interface{ Scan(...any) error }) (*AdminView, error) {
	v := &AdminView{}
	var fee *int64
	var by, email *string
	if err := row.Scan(&v.ID, &v.No, &v.SubscriptionID, &v.SubscriptionName, &v.CreatedAt, &v.UpdatedAt, &v.Reason, &v.Note,
		&v.AlsoChangeAccess, &v.Charge, &v.Status, &v.OldIP, &v.NewIP, &v.FailureReason, &v.User.ID, &v.User.Email, &v.FeeStatus, &fee,
		&v.FeeExternalRef, &v.InternalNote, &v.TaskID, &by, &email, &v.AcceptedAt, &v.FinishedAt); err != nil {
		return nil, err
	}
	if fee != nil {
		v.FeeAmount = kernel.Ptr(kernel.CNY(*fee))
	}
	if by != nil {
		v.HandledBy = &kernel.AdminRef{ID: *by, Email: kernel.StrOr(email, "")}
	}
	return v, nil
}

// Get 读申请（userID 非空时只认本人）。
func Get(ctx context.Context, q kernel.Q, id, userID string) (*AdminView, error) {
	sql, args := `SELECT `+cols+from+` WHERE e.id = $1`, []any{id}
	if userID != "" {
		sql += ` AND e.user_id = $2`
		args = append(args, userID)
	}
	v, err := scan(q.QueryRow(ctx, sql, args...))
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("没有找到这个申请")
	}
	return v, err
}

func list(ctx context.Context, q kernel.Q, where string, args []any, tail string) ([]AdminView, error) {
	rows, err := q.Query(ctx, `SELECT `+cols+from+where+` ORDER BY e.created_at DESC, e.id DESC`+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminView{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// Input 是 OpenAPI ExitRequestInput。
type Input struct {
	SubscriptionID   string `json:"subscriptionId"`
	Reason           string `json:"reason"`
	Note             string `json:"note"`
	AlsoChangeAccess bool   `json:"alsoChangeAccess"`
}

// Conflict 是 OpenAPI ExitRequestConflict。
type Conflict struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Existing View   `json:"existing"`
}

// Create 提交申请。已有未完成申请时返回现有申请（第二个返回值），由调用方以 409 输出。
func (s *Service) Create(ctx context.Context, tx *kernel.Tx, userID string, in Input) (*View, *View, error) {
	switch in.Reason {
	case "target_blocked", "quality", "region", "other":
	default:
		return nil, nil, httpx.Invalid("reason 取值不正确")
	}
	if len([]rune(in.Note)) > 200 {
		return nil, nil, httpx.Invalid("备注不超过 200 字")
	}
	var owner, lifecycle, rulesVersion, userStatus, kind string
	var paused bool
	var curAccess, curExit *string
	err := tx.QueryRow(ctx, `SELECT s.user_id, s.lifecycle, s.rules_version, s.admin_paused, s.current_access_id, s.current_exit_id, u.status,
	                            s.kind
	                          FROM subscriptions s JOIN users u ON u.id = s.user_id WHERE s.id = $1 FOR UPDATE OF s`, in.SubscriptionID).
		Scan(&owner, &lifecycle, &rulesVersion, &paused, &curAccess, &curExit, &userStatus, &kind)
	if kernel.IsNoRows(err) || (err == nil && (owner != userID || lifecycle == "deleted")) {
		return nil, nil, httpx.NotFound("没有找到这个订阅")
	}
	if err != nil {
		return nil, nil, err
	}
	if kind == "direct" {
		return nil, nil, httpx.Precondition("直出代理经中转服务器直接出站，没有出口可换")
	}
	var existing string
	if err := tx.QueryRow(ctx, `SELECT id FROM exit_requests WHERE subscription_id = $1 AND status IN ('pending', 'processing')`,
		in.SubscriptionID).Scan(&existing); err == nil {
		v, err := Get(ctx, tx, existing, userID)
		if err != nil {
			return nil, nil, err
		}
		return nil, &v.View, nil
	} else if !kernel.IsNoRows(err) {
		return nil, nil, err
	}
	switch {
	case userStatus == "banned" || paused:
		return nil, nil, httpx.Precondition("当前状态不能申请换出口，请联系客服")
	case lifecycle == "released":
		return nil, nil, httpx.Precondition("已释放的订阅没有出口可换；续时长后会重新开通并分配新出口")
	case lifecycle != "active" && lifecycle != "retained", curAccess == nil, curExit == nil:
		return nil, nil, httpx.Precondition("订阅尚未开通，暂不能申请换出口")
	}
	q, err := task.LoadQuotas(ctx, tx, in.SubscriptionID, rulesVersion, tx.Now())
	if err != nil {
		return nil, nil, err
	}
	charge, fee := "free", "not_applicable"
	if q.FreeExitChangesLeft <= 0 {
		charge, fee = "manual", "awaiting_decision"
	}
	id := store.NewID("req")
	_, err = tx.Exec(ctx, `INSERT INTO exit_requests (id, subscription_id, user_id, reason, note, also_change_access, charge, fee_status,
	                         old_ip, created_at, updated_at)
	                       VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
	                         (SELECT COALESCE(check_ip, verified_ip) FROM exit_bindings WHERE id = $9), $10, $10)`,
		id, in.SubscriptionID, userID, in.Reason, in.Note, in.AlsoChangeAccess, charge, fee, *curExit, tx.Now())
	if store.IsUniqueViolation(err, "exit_requests_one_open") {
		return nil, nil, httpx.Conflict("该订阅已有未完成的换出口申请")
	}
	if err != nil {
		return nil, nil, err
	}
	v, err := Get(ctx, tx, id, userID)
	if err != nil {
		return nil, nil, err
	}
	return &v.View, nil, nil
}

// AcceptInput 是 OpenAPI AcceptExitRequestInput。
type AcceptInput struct {
	Fee            string        `json:"fee"`
	FeeAmount      *kernel.Money `json:"feeAmount"`
	FeeExternalRef string        `json:"feeExternalRef"`
	InternalNote   string        `json:"internalNote"`
}

func lock(ctx context.Context, tx *kernel.Tx, id string) (status, charge, sub string, err error) {
	err = tx.QueryRow(ctx, `SELECT status, charge, subscription_id FROM exit_requests WHERE id = $1 FOR UPDATE`, id).Scan(&status, &charge, &sub)
	if kernel.IsNoRows(err) {
		err = httpx.NotFound("没有找到这个申请")
	}
	return
}

// Accept 受理申请：free 直接进入处理；manual 须选择 charged（金额与收款参考）或 waived（D-19）。
func (s *Service) Accept(ctx context.Context, tx *kernel.Tx, id, adminID string, in AcceptInput) error {
	if len([]rune(in.InternalNote)) > 1000 {
		return httpx.Invalid("内部备注不超过 1000 字")
	}
	status, charge, _, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if status != "pending" {
		return httpx.Precondition("只能受理待处理的申请")
	}
	fee := "not_applicable"
	var amount *int64
	var ref *string
	switch in.Fee {
	case "free":
		if charge != "free" {
			return httpx.Invalid("该申请已超出免费次数，请选择收费（charged）或免收（waived）")
		}
	case "charged", "waived":
		if charge == "free" {
			return httpx.Invalid("免费申请请选择 free")
		}
		fee = in.Fee
		if in.Fee == "charged" {
			if in.FeeAmount == nil || in.FeeAmount.Amount <= 0 || in.FeeExternalRef == "" {
				return httpx.Invalid("收费时需填写金额与收款参考")
			}
			amount, ref = &in.FeeAmount.Amount, &in.FeeExternalRef
		}
	default:
		return httpx.Invalid("fee 只能是 free、charged 或 waived")
	}
	if _, err := tx.Exec(ctx, `UPDATE exit_requests SET status = 'processing', fee_status = $2, fee_amount_cents = $3, fee_external_ref = $4,
	                            internal_note = CASE WHEN $5 = '' THEN internal_note ELSE $5 END, handled_by = $6, accepted_at = $7 WHERE id = $1`,
		id, fee, amount, ref, in.InternalNote, adminID, tx.Now()); err != nil {
		return err
	}
	return tx.Audit(ctx, adminID, "exit_request.accept", "exit_request:"+id, map[string]any{"fee": in.Fee, "feeAmount": amount, "feeExternalRef": ref})
}

// Reject 驳回：申请失败，不占免费次数。有关联进行中任务时 412。
func (s *Service) Reject(ctx context.Context, tx *kernel.Tx, id, adminID, reason, note string) error {
	if n := len([]rune(reason)); n < 1 || n > 300 {
		return httpx.Invalid("请填写面向客户的原因（1～300 字）")
	}
	status, _, _, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if status != "pending" && status != "processing" {
		return httpx.Precondition("申请已结束")
	}
	var running int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE exit_request_id = $1 AND state IN ('queued', 'running')`, id).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return httpx.Precondition("关联的换出口任务仍在进行中")
	}
	if _, err := tx.Exec(ctx, `UPDATE exit_requests SET status = 'failed', failure_reason = $2,
	                            internal_note = CASE WHEN $3 = '' THEN internal_note ELSE $3 END, handled_by = $4, finished_at = $5 WHERE id = $1`,
		id, reason, note, adminID, tx.Now()); err != nil {
		return err
	}
	return tx.Audit(ctx, adminID, "exit_request.reject", "exit_request:"+id, map[string]any{"failureReason": reason})
}

// CompleteInput 是 OpenAPI CompleteExitRequestInput。
type CompleteInput struct {
	Outcome       string `json:"outcome"`
	TaskID        string `json:"taskId"`
	FailureReason string `json:"failureReason"`
	InternalNote  string `json:"internalNote"`
}

// Complete 人工收尾：succeeded 需要该订阅已成功的换出口/组合更换任务；failed 需要原因。
func (s *Service) Complete(ctx context.Context, tx *kernel.Tx, id, adminID string, in CompleteInput) error {
	status, _, sub, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if status != "processing" {
		return httpx.Precondition("只能结束处理中的申请")
	}
	now := tx.Now()
	switch in.Outcome {
	case "succeeded":
		var newIP *string
		err := tx.QueryRow(ctx, `SELECT host(b.verified_ip) FROM tasks t JOIN exit_bindings b ON b.id = t.new_exit_id
		                          WHERE t.id = $1 AND t.subscription_id = $2 AND t.kind IN ('exit_change', 'combined') AND t.state = 'succeeded'`,
			in.TaskID, sub).Scan(&newIP)
		if kernel.IsNoRows(err) || (err == nil && newIP == nil) {
			return httpx.Precondition("taskId 必须是该订阅已成功的换出口或组合更换任务")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE exit_requests SET status = 'succeeded', new_ip = $2::inet, task_id = $3, handled_by = $4, finished_at = $5,
		                            internal_note = CASE WHEN $6 = '' THEN internal_note ELSE $6 END WHERE id = $1`,
			id, *newIP, in.TaskID, adminID, now, in.InternalNote); err != nil {
			return err
		}
	case "failed":
		if n := len([]rune(in.FailureReason)); n < 1 || n > 300 {
			return httpx.Invalid("失败时需填写面向客户的原因（1～300 字）")
		}
		if _, err := tx.Exec(ctx, `UPDATE exit_requests SET status = 'failed', failure_reason = $2, handled_by = $3, finished_at = $4,
		                            internal_note = CASE WHEN $5 = '' THEN internal_note ELSE $5 END WHERE id = $1`,
			id, in.FailureReason, adminID, now, in.InternalNote); err != nil {
			return err
		}
	default:
		return httpx.Invalid("outcome 只能是 succeeded 或 failed")
	}
	return tx.Audit(ctx, adminID, "exit_request.complete", "exit_request:"+id, in)
}

// Routes 返回申请路由（用户端 2 个、管理端 6 个）。
func (s *Service) Routes() []httpx.Route {
	type up = httpx.Principal
	type ap = httpx.AdminPrincipal
	rid := func(r *http.Request) string { return r.PathValue("exitRequestId") }
	adminTx := func(r *http.Request, fn func(tx *kernel.Tx) error) (*AdminView, error) {
		var v *AdminView
		err := s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			var err error
			v, err = Get(r.Context(), tx, rid(r), "")
			return err
		})
		return v, err
	}
	return []httpx.Route{
		{Method: "GET", Pattern: "/api/exit-requests", Auth: httpx.VerifiedUser, Handler: kernel.UserH(func(r *http.Request, p up) (int, any, error) {
			vs, err := list(r.Context(), s.env.Pool, ` WHERE e.user_id = $1`, []any{p.UserID}, " LIMIT 500")
			out := make([]View, 0, len(vs))
			for _, v := range vs {
				out = append(out, v.View)
			}
			return 200, out, err
		})},
		{Method: "POST", Pattern: "/api/exit-requests", Auth: httpx.VerifiedUser, Handler: kernel.UserW(func(r *http.Request, p up) (int, any, error) {
			in, err := kernel.Body[Input](r)
			if err != nil {
				return 0, nil, err
			}
			type result struct {
				Created  *View `json:"created"`
				Existing *View `json:"existing"`
			}
			res, err := kernel.Idempotent(r.Context(), s.env, "user:"+p.UserID+":exit_request", httpx.IdempotencyKey(r), in,
				func(tx *kernel.Tx) (*result, error) {
					v, ex, err := s.Create(r.Context(), tx, p.UserID, in)
					return &result{Created: v, Existing: ex}, err
				})
			if err != nil {
				return 0, nil, err
			}
			if res.Existing != nil {
				return 409, Conflict{Code: httpx.CodeConflict, Message: "该订阅已有未完成的换出口申请", Existing: *res.Existing}, nil
			}
			return 201, res.Created, nil
		})},
		{Method: "GET", Pattern: "/admin-api/exit-requests", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.adminList(r)
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/exit-requests/{exitRequestId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := Get(r.Context(), s.env.Pool, rid(r), "")
			return 200, v, err
		})},
		{Method: "PATCH", Pattern: "/admin-api/exit-requests/{exitRequestId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[struct {
				InternalNote *string `json:"internalNote"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			if in.InternalNote == nil || len([]rune(*in.InternalNote)) > 1000 {
				return 0, nil, httpx.Invalid("internalNote 必填且不超过 1000 字")
			}
			v, err := adminTx(r, func(tx *kernel.Tx) error {
				tag, err := tx.Exec(r.Context(), `UPDATE exit_requests SET internal_note = $2 WHERE id = $1`, rid(r), *in.InternalNote)
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 0 {
					return httpx.NotFound("没有找到这个申请")
				}
				return tx.Audit(r.Context(), a.AdminID, "exit_request.note", "exit_request:"+rid(r), nil)
			})
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/exit-requests/{exitRequestId}/accept", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[AcceptInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := adminTx(r, func(tx *kernel.Tx) error { return s.Accept(r.Context(), tx, rid(r), a.AdminID, in) })
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/exit-requests/{exitRequestId}/reject", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[struct {
				FailureReason string `json:"failureReason"`
				InternalNote  string `json:"internalNote"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := adminTx(r, func(tx *kernel.Tx) error {
				return s.Reject(r.Context(), tx, rid(r), a.AdminID, in.FailureReason, in.InternalNote)
			})
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/exit-requests/{exitRequestId}/complete", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[CompleteInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := adminTx(r, func(tx *kernel.Tx) error { return s.Complete(r.Context(), tx, rid(r), a.AdminID, in) })
			return 200, v, err
		})},
	}
}

func (s *Service) adminList(r *http.Request) (*kernel.Page[AdminView], error) {
	cur, limit, err := kernel.ParsePage(r)
	if err != nil {
		return nil, err
	}
	var a kernel.Args
	st, err := kernel.ListParam(r, "status", "pending", "processing", "succeeded", "failed", "cancelled")
	if err != nil {
		return nil, err
	}
	if len(st) > 0 {
		a.Add("e.status = ANY(?)", st)
	}
	q := r.URL.Query()
	if v := q.Get("feeStatus"); v != "" {
		switch v {
		case "not_applicable", "awaiting_decision", "charged", "waived":
			a.Add("e.fee_status = ?", v)
		default:
			return nil, httpx.Invalid("feeStatus 取值不正确")
		}
	}
	if v := q.Get("subscriptionId"); v != "" {
		a.Add("e.subscription_id = ?", v)
	}
	if v := q.Get("userId"); v != "" {
		a.Add("e.user_id = ?", v)
	}
	if cur != nil {
		a.AddRaw("(e.created_at, e.id) < (" + a.Next(cur.At) + ", " + a.Next(cur.ID) + ")")
	}
	vs, err := list(r.Context(), s.env.Pool, a.SQL(), a.Vals, " LIMIT "+strconv.Itoa(limit+1))
	if err != nil {
		return nil, err
	}
	page := &kernel.Page[AdminView]{Items: vs}
	if len(vs) > limit {
		page.Items = vs[:limit]
		last := vs[limit-1]
		page.NextCursor = kernel.Ptr(kernel.Cursor{At: last.CreatedAt, ID: last.ID}.Encode())
	}
	return page, nil
}
