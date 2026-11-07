// Package alert 是告警事件（C-W9，R-19.3）：节点离线、上游异常、配置失败、证书风险、计量持久化失败、
// 时钟偏差，以及履约失败、换出口失败、端口池不足、库存不足、晚到款。
// 同一 dedupe_key 未解决时只保留一条，重复发生累加 occurrences；条件消失时由系统标为 resolved。
package alert

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
)

// Alert 是一次告警发生。
type Alert struct {
	Kind     string
	Severity string // info / warning / critical
	Key      string // 去重键，如 node_offline:nod_…
	Message  string
	Details  map[string]any
	Node     *string
	Sub      *string
	Resource *string
	Order    *string
	Task     *string
}

// Raise 记录告警：未解决的同键告警累加次数并更新最近发生时间与内容。
func Raise(ctx context.Context, q kernel.Q, now time.Time, a Alert) error {
	if a.Details == nil {
		a.Details = map[string]any{}
	}
	_, err := q.Exec(ctx, `INSERT INTO alerts (id, kind, severity, dedupe_key, message, details, node_id, subscription_id,
	                         resource_id, order_id, task_id, first_seen_at, last_seen_at)
	                       VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
	                       ON CONFLICT (dedupe_key) WHERE status <> 'resolved'
	                       DO UPDATE SET occurrences = alerts.occurrences + 1, last_seen_at = EXCLUDED.last_seen_at,
	                         message = EXCLUDED.message, details = EXCLUDED.details, severity = EXCLUDED.severity`,
		store.NewID("alt"), a.Kind, a.Severity, a.Key, a.Message, a.Details, a.Node, a.Sub, a.Resource, a.Order, a.Task, now)
	return err
}

// Resolve 在条件消失时关闭同键告警。
func Resolve(ctx context.Context, q kernel.Q, now time.Time, key string) error {
	_, err := q.Exec(ctx, `UPDATE alerts SET status = 'resolved', resolved_at = $2 WHERE dedupe_key = $1 AND status <> 'resolved'`, key, now)
	return err
}

// View 是 OpenAPI Alert。
type View struct {
	ID             string           `json:"id"`
	Kind           string           `json:"kind"`
	Severity       string           `json:"severity"`
	Status         string           `json:"status"`
	Message        string           `json:"message"`
	Details        map[string]any   `json:"details"`
	NodeID         *string          `json:"nodeId"`
	SubscriptionID *string          `json:"subscriptionId"`
	ResourceID     *string          `json:"resourceId"`
	OrderID        *string          `json:"orderId"`
	TaskID         *string          `json:"taskId"`
	Occurrences    int              `json:"occurrences"`
	FirstSeenAt    time.Time        `json:"firstSeenAt"`
	LastSeenAt     time.Time        `json:"lastSeenAt"`
	AckedBy        *kernel.AdminRef `json:"ackedBy"`
	AckedAt        *time.Time       `json:"ackedAt"`
	AckNote        *string          `json:"ackNote"`
	ResolvedAt     *time.Time       `json:"resolvedAt"`
}

const cols = `a.id, a.kind, a.severity, a.status, a.message, a.details, a.node_id, a.subscription_id, a.resource_id, a.order_id,
	a.task_id, a.occurrences, a.first_seen_at, a.last_seen_at, a.acked_by, ad.email, a.acked_at, a.ack_note, a.resolved_at`

func scan(row interface{ Scan(...any) error }) (*View, error) {
	v := &View{}
	var by, email *string
	if err := row.Scan(&v.ID, &v.Kind, &v.Severity, &v.Status, &v.Message, &v.Details, &v.NodeID, &v.SubscriptionID, &v.ResourceID,
		&v.OrderID, &v.TaskID, &v.Occurrences, &v.FirstSeenAt, &v.LastSeenAt, &by, &email, &v.AckedAt, &v.AckNote, &v.ResolvedAt); err != nil {
		return nil, err
	}
	if by != nil {
		v.AckedBy = &kernel.AdminRef{ID: *by, Email: kernel.StrOr(email, "")}
	}
	if v.Details == nil {
		v.Details = map[string]any{}
	}
	return v, nil
}

// Service 提供告警查询与确认。
type Service struct{ env *kernel.Env }

// New 创建服务。
func New(env *kernel.Env) *Service { return &Service{env: env} }

// List 列出告警（最近发生的在前）。
func (s *Service) List(r *http.Request) (*kernel.Page[View], error) {
	cur, limit, err := kernel.ParsePage(r)
	if err != nil {
		return nil, err
	}
	var a kernel.Args
	st, err := kernel.ListParam(r, "status", "open", "acked", "resolved")
	if err != nil {
		return nil, err
	}
	if len(st) > 0 {
		a.Add("a.status = ANY(?)", st)
	}
	q := r.URL.Query()
	for _, f := range []struct{ param, col string }{{"kind", "a.kind"}, {"severity", "a.severity"}, {"nodeId", "a.node_id"},
		{"subscriptionId", "a.subscription_id"}} {
		if v := q.Get(f.param); v != "" {
			a.Add(f.col+" = ?", v)
		}
	}
	if cur != nil {
		a.AddRaw("(a.last_seen_at, a.id) < (" + a.Next(cur.At) + ", " + a.Next(cur.ID) + ")")
	}
	rows, err := s.env.Pool.Query(r.Context(), `SELECT `+cols+` FROM alerts a LEFT JOIN admins ad ON ad.id = a.acked_by`+a.SQL()+
		` ORDER BY a.last_seen_at DESC, a.id DESC LIMIT `+strconv.Itoa(limit+1), a.Vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &kernel.Page[View]{Items: []View{}}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = kernel.Ptr(kernel.Cursor{At: last.LastSeenAt, ID: last.ID}.Encode())
	}
	return page, nil
}

// Ack 确认告警（只表示已知悉，幂等）。
func (s *Service) Ack(ctx context.Context, adminID, id, note string) (*View, error) {
	var v *View
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE alerts SET status = 'acked', acked_by = $2, acked_at = $3, ack_note = NULLIF($4, '')
		                           WHERE id = $1 AND status = 'open'`, id, adminID, tx.Now(), note)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			if err := tx.Audit(ctx, adminID, "alert.ack", "alert:"+id, map[string]any{"note": note}); err != nil {
				return err
			}
		}
		v, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM alerts a LEFT JOIN admins ad ON ad.id = a.acked_by WHERE a.id = $1`, id))
		if kernel.IsNoRows(err) {
			return httpx.NotFound("没有找到这条告警")
		}
		return err
	})
	return v, err
}

// Routes 返回告警路由。
func (s *Service) Routes() []httpx.Route {
	return []httpx.Route{
		{Method: "GET", Pattern: "/admin-api/alerts", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ httpx.AdminPrincipal) (int, any, error) {
			v, err := s.List(r)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/alerts/{alertId}/ack", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a httpx.AdminPrincipal) (int, any, error) {
			in, err := kernel.Body[struct {
				Note string `json:"note"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.Ack(r.Context(), a.AdminID, r.PathValue("alertId"), in.Note)
			return 200, v, err
		})},
	}
}
