// Package overview 是管理端运营概览（R-19.1）。连接数与速率只采用新鲜上报（15 秒内），否则为 null，不显示为 0。
package overview

import (
	"context"
	"net/http"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/subscription"
)

// Service 提供概览。
type Service struct {
	env  *kernel.Env
	subs *subscription.Service
}

// New 创建服务。
func New(env *kernel.Env, subs *subscription.Service) *Service { return &Service{env: env, subs: subs} }

// View 是 OpenAPI Overview。
type View struct {
	GeneratedAt   time.Time  `json:"generatedAt"`
	DataAt        *time.Time `json:"dataAt"`
	Subscriptions struct {
		Total         int `json:"total"`
		Usable        int `json:"usable"`
		Exhausted     int `json:"exhausted"`
		Expired       int `json:"expired"`
		Paused        int `json:"paused"`
		Pending       int `json:"pending"`
		FulfillFailed int `json:"fulfillFailed"`
		Changing      int `json:"changing"`
		Dataplane     int `json:"dataplane"`
		Released      int `json:"released"`
	} `json:"subscriptions"`
	Traffic struct {
		CurrentConns *int   `json:"currentConns"`
		RateBps      *int64 `json:"rateBps"`
		TodayBytes   int64  `json:"todayBytes"`
		TotalBytes   int64  `json:"totalBytes"`
	} `json:"traffic"`
	Orders struct {
		AwaitingPayment     int `json:"awaitingPayment"`
		AwaitingFulfillment int `json:"awaitingFulfillment"`
		FulfillFailed       int `json:"fulfillFailed"`
		RefundReview        int `json:"refundReview"`
	} `json:"orders"`
	ExitRequests struct {
		Pending             int `json:"pending"`
		AwaitingFeeDecision int `json:"awaitingFeeDecision"`
		Processing          int `json:"processing"`
	} `json:"exitRequests"`
	Alerts struct {
		Open     int `json:"open"`
		Critical int `json:"critical"`
	} `json:"alerts"`
	Nodes struct {
		Online  int `json:"online"`
		Offline int `json:"offline"`
	} `json:"nodes"`
}

var shanghai = func() *time.Location {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return l
}()

// Get 计算概览。
func (s *Service) Get(ctx context.Context) (*View, error) {
	now := s.env.Now()
	v := &View{GeneratedAt: now}
	total, usable, c, err := s.subs.BlockerCounts(ctx)
	if err != nil {
		return nil, err
	}
	sv := &v.Subscriptions
	sv.Total, sv.Usable = total, usable
	sv.Exhausted, sv.Expired, sv.Paused, sv.Pending = c["traffic_exhausted"], c["expired"], c["admin_paused"], c["pending"]
	sv.FulfillFailed, sv.Changing, sv.Dataplane, sv.Released = c["fulfill_failed"], c["changing"], c["dataplane"], c["released"]

	p := s.env.Pool
	fresh := now.Add(-s.env.Cfg.FreshWindow)
	var conns *int
	if err := p.QueryRow(ctx, `SELECT sum(active_conns)::int FROM subscriptions WHERE lifecycle <> 'deleted' AND usage_reported_at >= $1`, fresh).
		Scan(&conns); err != nil {
		return nil, err
	}
	v.Traffic.CurrentConns = conns
	// 速率：最近 60 秒内各订阅累计值的增量 / 时间跨度，求和（字节/秒）。只在有新鲜上报时给出。
	if conns != nil {
		var rate *float64
		if err := p.QueryRow(ctx, `SELECT sum(CASE WHEN span > 0 THEN delta / span END) FROM (
		      SELECT max(used_total) - min(used_total) AS delta, extract(epoch FROM max(sampled_at) - min(sampled_at)) AS span
		        FROM usage_samples WHERE sampled_at >= $1 GROUP BY subscription_id) x`, now.Add(-60*time.Second)).Scan(&rate); err != nil {
			return nil, err
		}
		r := int64(0)
		if rate != nil {
			r = int64(*rate)
		}
		v.Traffic.RateBps = &r
	}
	y, m, d := now.In(shanghai).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, shanghai).UTC()
	if err := p.QueryRow(ctx, `SELECT COALESCE(sum(GREATEST(t.hi - COALESCE(b.base, t.lo), 0)), 0)::bigint FROM (
	      SELECT subscription_id, max(used_total) AS hi, min(used_total) AS lo FROM usage_samples WHERE sampled_at >= $1 GROUP BY subscription_id) t
	      LEFT JOIN LATERAL (SELECT used_total AS base FROM usage_samples u WHERE u.subscription_id = t.subscription_id AND u.sampled_at < $1
	                          ORDER BY sampled_at DESC LIMIT 1) b ON true`, today).Scan(&v.Traffic.TodayBytes); err != nil {
		return nil, err
	}
	if err := p.QueryRow(ctx, `SELECT COALESCE(sum(used_bytes_total), 0)::bigint FROM subscriptions WHERE lifecycle <> 'deleted'`).
		Scan(&v.Traffic.TotalBytes); err != nil {
		return nil, err
	}
	o := &v.Orders
	if err := p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE payment_status = 'awaiting'),
	      count(*) FILTER (WHERE payment_status = 'confirmed' AND fulfillment_status = 'processing'),
	      count(*) FILTER (WHERE fulfillment_status = 'failed' AND payment_status IN ('confirmed', 'refund_review')),
	      count(*) FILTER (WHERE payment_status = 'refund_review') FROM orders`).
		Scan(&o.AwaitingPayment, &o.AwaitingFulfillment, &o.FulfillFailed, &o.RefundReview); err != nil {
		return nil, err
	}
	e := &v.ExitRequests
	if err := p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'pending' AND fee_status <> 'awaiting_decision'),
	      count(*) FILTER (WHERE status = 'pending' AND fee_status = 'awaiting_decision'),
	      count(*) FILTER (WHERE status = 'processing') FROM exit_requests`).Scan(&e.Pending, &e.AwaitingFeeDecision, &e.Processing); err != nil {
		return nil, err
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'open'), count(*) FILTER (WHERE status = 'open' AND severity = 'critical')
	      FROM alerts`).Scan(&v.Alerts.Open, &v.Alerts.Critical); err != nil {
		return nil, err
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE last_seen_at >= $1), count(*) FILTER (WHERE last_seen_at IS NULL OR last_seen_at < $1),
	      max(last_report_at) FROM nodes WHERE status <> 'disabled'`, fresh).Scan(&v.Nodes.Online, &v.Nodes.Offline, &v.DataAt); err != nil {
		return nil, err
	}
	v.DataAt = kernel.TimePtr(v.DataAt)
	return v, nil
}

// Routes 返回概览路由。
func (s *Service) Routes() []httpx.Route {
	return []httpx.Route{{Method: "GET", Pattern: "/admin-api/overview", Auth: httpx.Admin,
		Handler: kernel.AdminH(func(r *http.Request, _ httpx.AdminPrincipal) (int, any, error) {
			v, err := s.Get(r.Context())
			return 200, v, err
		})}}
}
