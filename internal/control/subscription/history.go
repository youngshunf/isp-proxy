package subscription

import (
	"context"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/ledger"
	"github.com/go-gost/gost/internal/control/store"
)

// Event 是 OpenAPI SubscriptionEvent（客户可见）。
type Event struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Detail  string    `json:"detail"`
	OrderNo string    `json:"orderNo,omitempty"`
}

// AdminEvent 是 OpenAPI AdminSubscriptionEvent。
type AdminEvent struct {
	ID              string       `json:"id"`
	At              time.Time    `json:"at"`
	Kind            string       `json:"kind"`
	Detail          string       `json:"detail"`
	CustomerVisible bool         `json:"customerVisible"`
	OrderID         *string      `json:"orderId"`
	TaskID          *string      `json:"taskId"`
	Actor           kernel.Actor `json:"actor"`
}

func exists(ctx context.Context, q kernel.Q, id, userID string) error {
	var owner, lifecycle string
	err := q.QueryRow(ctx, `SELECT user_id, lifecycle FROM subscriptions WHERE id = $1`, id).Scan(&owner, &lifecycle)
	if kernel.IsNoRows(err) || (err == nil && userID != "" && (owner != userID || lifecycle == "deleted")) {
		return httpx.NotFound("没有找到这个订阅")
	}
	return err
}

// UserEvents 是客户可见的订阅记录（新的在前）：开通、续时长、加流量、换接入、换出口、到期、耗尽、改名。
func UserEvents(ctx context.Context, q kernel.Q, id, userID string) ([]Event, error) {
	if err := exists(ctx, q, id, userID); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT e.id, e.at, e.kind, e.detail, COALESCE(o.no, '') FROM subscription_events e
	                            LEFT JOIN orders o ON o.id = e.order_id
	                           WHERE e.subscription_id = $1 AND e.customer_visible ORDER BY e.at DESC, e.id DESC LIMIT 500`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.Detail, &e.OrderNo); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AdminEvents 是管理端订阅时间线（含内部条目）。
func AdminEvents(ctx context.Context, q kernel.Q, id string) ([]AdminEvent, error) {
	if err := exists(ctx, q, id, ""); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT e.id, e.at, e.kind, e.detail, e.customer_visible, e.order_id, e.task_id, e.actor_kind, e.actor_id,
	                             CASE e.actor_kind WHEN 'admin' THEN (SELECT email FROM admins WHERE id = e.actor_id)
	                                               WHEN 'user' THEN (SELECT email FROM users WHERE id = e.actor_id) END
	                            FROM subscription_events e WHERE e.subscription_id = $1 ORDER BY e.at DESC, e.id DESC LIMIT 1000`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminEvent{}
	for rows.Next() {
		var e AdminEvent
		var email *string
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.Detail, &e.CustomerVisible, &e.OrderID, &e.TaskID, &e.Actor.Kind, &e.Actor.ID, &email); err != nil {
			return nil, err
		}
		e.Actor.Email = kernel.StrOr(email, "")
		out = append(out, e)
	}
	return out, rows.Err()
}

// AccessVersion 是 OpenAPI AccessVersion（不含密码）。
type AccessVersion struct {
	ID           string     `json:"id"`
	Version      int        `json:"version"`
	ChangeKind   string     `json:"changeKind"`
	State        string     `json:"state"`
	Username     string     `json:"username"`
	Ports        Ports      `json:"ports"`
	IssuedAt     time.Time  `json:"issuedAt"`
	ActivatedAt  *time.Time `json:"activatedAt"`
	RevokedAt    *time.Time `json:"revokedAt"`
	RevokeReason *string    `json:"revokeReason"`
	TaskID       *string    `json:"taskId"`
}

// AccessVersions 列出接入版本历史（新的在前）。
func AccessVersions(ctx context.Context, q kernel.Q, id string) ([]AccessVersion, error) {
	if err := exists(ctx, q, id, ""); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT id, version, change_kind, state, username, http_port, https_port, socks5_port, issued_at,
	                             activated_at, revoked_at, revoke_reason, task_id
	                            FROM access_versions WHERE subscription_id = $1 ORDER BY version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessVersion{}
	for rows.Next() {
		var a AccessVersion
		if err := rows.Scan(&a.ID, &a.Version, &a.ChangeKind, &a.State, &a.Username, &a.Ports.HTTP, &a.Ports.HTTPS, &a.Ports.SOCKS5,
			&a.IssuedAt, &a.ActivatedAt, &a.RevokedAt, &a.RevokeReason, &a.TaskID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CheckResult 是 OpenAPI ExitCheckResult。
type CheckResult struct {
	Result      string    `json:"result"`
	IP          *string   `json:"ip"`
	CountryCode *string   `json:"countryCode"`
	City        *string   `json:"city"`
	ASN         *string   `json:"asn"`
	LatencyMs   *int      `json:"latencyMs"`
	Error       *string   `json:"error"`
	CheckedAt   time.Time `json:"checkedAt"`
}

// ExitBinding 是 OpenAPI ExitBinding。
type ExitBinding struct {
	ID                  string       `json:"id"`
	Version             int          `json:"version"`
	State               string       `json:"state"`
	ResourceID          string       `json:"resourceId"`
	VerifiedIP          *string      `json:"verifiedIp"`
	VerifiedCountryCode *string      `json:"verifiedCountryCode"`
	Check               *CheckResult `json:"check"`
	BoundAt             *time.Time   `json:"boundAt"`
	UnboundAt           *time.Time   `json:"unboundAt"`
	UnboundReason       *string      `json:"unboundReason"`
	TaskID              *string      `json:"taskId"`
}

// ExitBindings 列出出口绑定历史（新的在前）。
func ExitBindings(ctx context.Context, q kernel.Q, id string) ([]ExitBinding, error) {
	if err := exists(ctx, q, id, ""); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+exitCols+` FROM exit_bindings b JOIN upstream_resources r ON r.id = b.resource_id
	                            WHERE b.subscription_id = $1 ORDER BY b.version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExitBinding{}
	for rows.Next() {
		e, err := scanExit(rows)
		if err != nil {
			return nil, err
		}
		b := ExitBinding{ID: e.ID, Version: e.Version, State: e.State, ResourceID: e.ResourceID, VerifiedIP: e.VerifiedIP,
			VerifiedCountryCode: e.VerifiedCC, BoundAt: kernel.TimePtr(e.BoundAt), UnboundAt: kernel.TimePtr(e.UnboundAt),
			UnboundReason: e.UnboundReason, TaskID: e.TaskID}
		if e.CheckResult != nil && e.CheckedAt != nil {
			b.Check = &CheckResult{Result: *e.CheckResult, IP: e.CheckIP, CountryCode: e.CheckCC, City: e.CheckCity, ASN: e.ASN,
				LatencyMs: e.Latency, Error: e.CheckError, CheckedAt: e.CheckedAt.UTC()}
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- 账本调整 ----

// LedgerEntryInput 是 OpenAPI LedgerEntryInput。
type LedgerEntryInput struct {
	SourceType      string `json:"sourceType"`
	EntitlementKind string `json:"entitlementKind"`
	Amount          int64  `json:"amount"`
	Reason          string `json:"reason"`
}

// AddLedgerEntry 追加调整 / 赠送 / 补偿条目（R-08.8：只追加，不能覆盖余额或回退已用）。
func (s *Service) AddLedgerEntry(ctx context.Context, tx *kernel.Tx, id, adminID string, in LedgerEntryInput) (*ledger.EntryView, error) {
	switch in.SourceType {
	case "adjust", "gift", "compensation":
	default:
		return nil, httpx.Invalid("sourceType 只能是 adjust、gift 或 compensation")
	}
	if in.EntitlementKind != "time" && in.EntitlementKind != "traffic" {
		return nil, httpx.Invalid("entitlementKind 只能是 time 或 traffic")
	}
	if in.Amount == 0 {
		return nil, httpx.Invalid("amount 不能为 0")
	}
	if in.SourceType != "adjust" && in.Amount < 0 {
		return nil, httpx.Invalid("赠送与补偿只能为正数")
	}
	if in.Reason == "" || len([]rune(in.Reason)) > 500 {
		return nil, httpx.Invalid("请填写原因（不超过 500 字）")
	}
	st, err := lock(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if st.Lifecycle == "deleted" {
		return nil, httpx.NotFound("没有找到这个订阅")
	}
	entryID, _, err := ledger.Insert(ctx, tx, ledger.Entry{Sub: id, Kind: in.EntitlementKind, Amount: in.Amount, SourceType: in.SourceType,
		SourceID: store.NewID("adj"), Reason: in.Reason, AdminID: &adminID})
	if err != nil {
		return nil, err
	}
	kind, detail := "adjusted", "管理员调整："+in.Reason
	if in.Amount > 0 && in.SourceType != "adjust" {
		label := map[string]string{"gift": "赠送", "compensation": "补偿"}[in.SourceType]
		if in.EntitlementKind == "time" {
			kind, detail = "time_added", label+"时长 "+formatSeconds(in.Amount)
		} else {
			kind, detail = "traffic_added", label+"流量 "+kernel.FormatBytes(in.Amount)
		}
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: kind, Detail: detail, LedgerID: &entryID, ActorKind: "admin", ActorID: &adminID}); err != nil {
		return nil, err
	}
	if err := tx.Audit(ctx, adminID, "ledger.create_entry", "subscription:"+id, in); err != nil {
		return nil, err
	}
	if err := s.AfterCredit(ctx, tx, id); err != nil {
		return nil, err
	}
	entries, err := ledger.ListBySub(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].ID == entryID {
			return &entries[i], nil
		}
	}
	return nil, httpx.NotFound("")
}

func formatSeconds(s int64) string {
	d := s / 86400
	h := (s % 86400) / 3600
	switch {
	case d > 0 && h > 0:
		return kernel.Itoa(d) + " 天 " + kernel.Itoa(h) + " 小时"
	case d > 0:
		return kernel.Itoa(d) + " 天"
	case h > 0:
		return kernel.Itoa(h) + " 小时"
	}
	return kernel.Itoa(s) + " 秒"
}

// ---- 用量趋势（A-37） ----

// UsagePoint 是 OpenAPI UsagePoint。
type UsagePoint struct {
	At        time.Time  `json:"at"`
	Bytes     *int64     `json:"bytes"`
	MaxConns  *int       `json:"maxConns"`
	Samples   int        `json:"samples"`
	SampledAt *time.Time `json:"sampledAt"`
}

// UsageSeries 是 OpenAPI UsageSeries。
type UsageSeries struct {
	SubscriptionID string       `json:"subscriptionId"`
	Source         string       `json:"source"`
	Unit           string       `json:"unit"`
	BucketSeconds  int          `json:"bucketSeconds"`
	From           time.Time    `json:"from"`
	To             time.Time    `json:"to"`
	LastSampleAt   *time.Time   `json:"lastSampleAt"`
	Freshness      string       `json:"freshness"`
	Points         []UsagePoint `json:"points"`
}

// Usage 按桶聚合节点上报的累计值差分（R-19.2：不作为扣量依据）。没有样本的桶 bytes 为 null，与真实 0 区分。
func (s *Service) Usage(ctx context.Context, id string, from, to time.Time, bucket string) (*UsageSeries, error) {
	if err := exists(ctx, s.env.Pool, id, ""); err != nil {
		return nil, err
	}
	step := map[string]time.Duration{"5m": 5 * time.Minute, "1h": time.Hour, "1d": 24 * time.Hour}[bucket]
	if bucket == "" {
		step = time.Hour
	}
	if step == 0 {
		return nil, httpx.Invalid("bucket 只能是 5m、1h 或 1d")
	}
	if !to.After(from) {
		return nil, httpx.Invalid("to 必须晚于 from")
	}
	if to.Sub(from) > s.env.Cfg.UsageRetention {
		return nil, httpx.Invalid("最长查询 90 天")
	}
	from, to = from.UTC(), to.UTC()
	start := from.Truncate(step)
	n := int((to.Sub(start) + step - 1) / step)
	out := &UsageSeries{SubscriptionID: id, Source: "agent_report", Unit: "bytes", BucketSeconds: int(step / time.Second), From: from, To: to,
		Points: make([]UsagePoint, n)}
	for i := range out.Points {
		out.Points[i].At = start.Add(time.Duration(i) * step)
	}
	var last *time.Time
	if err := s.env.Pool.QueryRow(ctx, `SELECT max(sampled_at) FROM usage_samples WHERE subscription_id = $1`, id).Scan(&last); err != nil {
		return nil, err
	}
	out.LastSampleAt = kernel.TimePtr(last)
	// 基线：区间开始前的最后一个样本。
	var prev *int64
	var base int64
	if err := s.env.Pool.QueryRow(ctx, `SELECT used_total FROM usage_samples WHERE subscription_id = $1 AND sampled_at < $2
	                                     ORDER BY sampled_at DESC, id DESC LIMIT 1`, id, start).Scan(&base); err == nil {
		prev = &base
	} else if !kernel.IsNoRows(err) {
		return nil, err
	}
	rows, err := s.env.Pool.Query(ctx, `SELECT sampled_at, used_total, active_conns FROM usage_samples
	                                     WHERE subscription_id = $1 AND sampled_at >= $2 AND sampled_at < $3 ORDER BY sampled_at, id`, id, start, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	inRange := 0
	for rows.Next() {
		var at time.Time
		var used int64
		var conns int
		if err := rows.Scan(&at, &used, &conns); err != nil {
			return nil, err
		}
		i := int(at.Sub(start) / step)
		if i < 0 || i >= n {
			continue
		}
		inRange++
		p := &out.Points[i]
		var delta int64
		if prev != nil && used > *prev {
			delta = used - *prev
		}
		u := used
		prev = &u
		if p.Bytes == nil {
			p.Bytes = new(int64)
		}
		*p.Bytes += delta
		if p.MaxConns == nil || conns > *p.MaxConns {
			p.MaxConns = kernel.Ptr(conns)
		}
		p.Samples++
		p.SampledAt = kernel.Ptr(at.UTC())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case inRange == 0:
		out.Freshness = "no_data"
	case last != nil && s.env.Now().Sub(*last) <= s.env.Cfg.FreshWindow:
		out.Freshness = "fresh"
	default:
		out.Freshness = "stale"
	}
	return out, nil
}
