// Package subscription 是订阅生命周期与订阅视图（C-W4，R-10、R-18、docs/01 §7.2）。
//
// 生命周期：pending →（履约就绪）active →（时长到期）retained →（保留期结束）released；续时长在保留期内回到 active
// 并恢复原出口与接入，已释放后续时长则重新履约分配新出口与新接入（R-10.5）。管理员暂停、账号封禁是独立的开关，
// 不改变生命周期，只体现为阻断原因并让节点拒绝连接。
//
// 阻断原因的唯一来源是 blockersSQL（列表筛选与视图共用），顺序即 OpenAPI Blocker 枚举顺序（docs/01 §7.2 展示优先级）。
package subscription

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/task"
)

// blockersSQL 计算阻断原因数组；$1 为当前时间。
const blockersSQL = `ARRAY_REMOVE(ARRAY[
	CASE WHEN u.status = 'banned' THEN 'account_banned' END,
	CASE WHEN s.lifecycle = 'released' THEN 'released' END,
	CASE WHEN s.admin_paused THEN 'admin_paused' END,
	CASE WHEN s.lifecycle = 'pending' THEN 'pending' END,
	CASE WHEN s.lifecycle = 'fulfill_failed' THEN 'fulfill_failed' END,
	CASE WHEN s.lifecycle = 'retained' OR (s.lifecycle = 'active' AND s.expires_at <= $1) THEN 'expired' END,
	CASE WHEN s.lifecycle IN ('active', 'retained') AND s.used_bytes_total >= s.entitled_bytes_total THEN 'traffic_exhausted' END,
	CASE WHEN EXISTS (SELECT 1 FROM tasks t WHERE t.subscription_id = s.id AND t.state IN ('queued', 'running') AND t.kind <> 'fulfill')
	     THEN 'changing' END,
	CASE WHEN s.lifecycle IN ('active', 'retained') AND (n.status = 'disabled' OR EXISTS (
	       SELECT 1 FROM exit_bindings b WHERE b.id = s.current_exit_id AND b.check_result = 'failed')) THEN 'dataplane' END
	]::text[], NULL)`

// baseSQL 选出视图所需的订阅列；外层可包一层 SELECT … FROM (baseSQL) v WHERE … 做筛选。
const baseSQL = `SELECT s.id AS id, s.user_id AS user_id, u.email AS email, u.status AS user_status, s.name AS name,
	s.lifecycle AS lifecycle, s.lifecycle_reason AS lifecycle_reason, s.node_id AS node_id, n.status AS node_status,
	n.public_host AS node_host, s.plan_id AS plan_id, s.plan_version_id AS plan_version_id, pv.version AS plan_version,
	pv.name AS plan_name, pv.country_code AS cc, pv.country_name AS country, pv.city AS city, pv.concurrency AS concurrency,
	pv.bandwidth_mbps AS bandwidth, s.admin_paused AS admin_paused, s.paused_reason AS paused_reason,
	s.entitled_bytes_total AS entitled_bytes, s.used_bytes_total AS used_bytes, s.entitled_seconds_total AS entitled_secs,
	s.seconds_before_period AS before_secs, s.first_ready_at AS first_ready_at, s.period_started_at AS period_started_at,
	s.expires_at AS expires_at, s.retention_ends_at AS retention_ends_at, s.released_at AS released_at, s.deleted_at AS deleted_at,
	s.current_access_id AS cur_access, s.current_exit_id AS cur_exit, s.desired_version AS desired_version,
	s.applied_version AS applied_version, s.active_conns AS active_conns, s.usage_reported_at AS usage_reported_at,
	s.rules_version AS rules_version, s.created_from_order_id AS created_from_order, s.created_at AS created_at,
	(SELECT t.id FROM tasks t WHERE t.subscription_id = s.id AND t.state IN ('queued', 'running') LIMIT 1) AS active_task,
	(SELECT e.id FROM exit_requests e WHERE e.subscription_id = s.id AND e.status IN ('pending', 'processing') LIMIT 1) AS open_exit_request,
	ARRAY(SELECT o.id FROM orders o WHERE o.subscription_id = s.id AND o.payment_status = 'awaiting' ORDER BY o.created_at, o.id) AS open_orders,
	` + blockersSQL + ` AS blockers
	FROM subscriptions s JOIN users u ON u.id = s.user_id JOIN plan_versions pv ON pv.id = s.plan_version_id
	LEFT JOIN nodes n ON n.id = s.node_id`

const outerCols = `id, user_id, email, user_status, name, lifecycle, lifecycle_reason, node_id, node_status, node_host, plan_id,
	plan_version_id, plan_version, plan_name, cc, country, city, concurrency, bandwidth, admin_paused, paused_reason, entitled_bytes,
	used_bytes, entitled_secs, before_secs, first_ready_at, period_started_at, expires_at, retention_ends_at, released_at, deleted_at,
	cur_access, cur_exit, desired_version, applied_version, active_conns, usage_reported_at, rules_version, created_from_order,
	created_at, active_task, open_exit_request, open_orders, blockers`

type row struct {
	ID, UserID, Email, UserStatus, Name, Lifecycle         string
	LifecycleReason, NodeID, NodeStatus, NodeHost           *string
	PlanID, PlanVersionID                                   string
	PlanVersion                                             int
	PlanName, CC, Country, City                             string
	Concurrency, Bandwidth                                  int
	AdminPaused                                             bool
	PausedReason                                            *string
	EntitledBytes, UsedBytes, EntitledSecs, BeforeSecs      int64
	FirstReady, PeriodStarted, Expires, RetentionEnds       *time.Time
	Released, Deleted                                       *time.Time
	CurAccess, CurExit                                      *string
	DesiredV, AppliedV                                      int64
	ActiveConns                                             int
	UsageReportedAt                                         *time.Time
	RulesVersion                                            string
	CreatedFromOrder                                        *string
	CreatedAt                                               time.Time
	ActiveTask, OpenExitReq                                 *string
	OpenOrders, Blockers                                    []string
}

func scanRow(r interface{ Scan(...any) error }) (*row, error) {
	x := &row{}
	err := r.Scan(&x.ID, &x.UserID, &x.Email, &x.UserStatus, &x.Name, &x.Lifecycle, &x.LifecycleReason, &x.NodeID, &x.NodeStatus,
		&x.NodeHost, &x.PlanID, &x.PlanVersionID, &x.PlanVersion, &x.PlanName, &x.CC, &x.Country, &x.City, &x.Concurrency, &x.Bandwidth,
		&x.AdminPaused, &x.PausedReason, &x.EntitledBytes, &x.UsedBytes, &x.EntitledSecs, &x.BeforeSecs, &x.FirstReady,
		&x.PeriodStarted, &x.Expires, &x.RetentionEnds, &x.Released, &x.Deleted, &x.CurAccess, &x.CurExit, &x.DesiredV, &x.AppliedV,
		&x.ActiveConns, &x.UsageReportedAt, &x.RulesVersion, &x.CreatedFromOrder, &x.CreatedAt, &x.ActiveTask, &x.OpenExitReq,
		&x.OpenOrders, &x.Blockers)
	if x.OpenOrders == nil {
		x.OpenOrders = []string{}
	}
	if x.Blockers == nil {
		x.Blockers = []string{}
	}
	return x, err
}

func loadRow(ctx context.Context, q kernel.Q, now time.Time, id, userID string) (*row, error) {
	sql := `SELECT ` + outerCols + ` FROM (` + baseSQL + ` WHERE s.id = $2) v`
	args := []any{now, id}
	if userID != "" {
		sql += ` WHERE user_id = $3 AND lifecycle <> 'deleted'`
		args = append(args, userID)
	}
	x, err := scanRow(q.QueryRow(ctx, sql, args...))
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("没有找到这个订阅")
	}
	return x, err
}

// ---- 用户视图（OpenAPI Subscription） ----

// Plan 是 OpenAPI SubscriptionPlan。
type Plan struct {
	Name          string `json:"name"`
	CountryCode   string `json:"countryCode"`
	Country       string `json:"country"`
	City          string `json:"city"`
	Concurrency   int    `json:"concurrency"`
	BandwidthMbps int    `json:"bandwidthMbps"`
}

// Time 是 OpenAPI SubscriptionTime。
type Time struct {
	PeriodDays       int64      `json:"periodDays"`
	StartedAt        *time.Time `json:"startedAt"`
	ExpiresAt        *time.Time `json:"expiresAt"`
	RemainingSeconds int64      `json:"remainingSeconds"`
	RetentionEndsAt  *time.Time `json:"retentionEndsAt"`
}

// Traffic 是 OpenAPI SubscriptionTraffic。
type Traffic struct {
	TotalBytes int64 `json:"totalBytes"`
	UsedBytes  int64 `json:"usedBytes"`
}

// Exit 是 OpenAPI Exit（客户可见，不含供应商）。
type Exit struct {
	Version     int       `json:"version"`
	IP          string    `json:"ip"`
	CountryCode string    `json:"countryCode"`
	Country     string    `json:"country"`
	City        string    `json:"city"`
	Check       string    `json:"check"`
	LatencyMs   *int      `json:"latencyMs"`
	CheckedAt   time.Time `json:"checkedAt"`
}

// Ports 是 OpenAPI Ports。
type Ports struct {
	HTTP   int `json:"http"`
	HTTPS  int `json:"https"`
	SOCKS5 int `json:"socks5"`
}

// Access 是 OpenAPI Access（本人可见明文）。
type Access struct {
	Version       int       `json:"version"`
	Host          string    `json:"host"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	Ports         Ports     `json:"ports"`
	IssuedAt      time.Time `json:"issuedAt"`
	UnseenChanges []string  `json:"unseenChanges"`
}

// View 是 OpenAPI Subscription。
type View struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	Plan              Plan          `json:"plan"`
	Lifecycle         string        `json:"lifecycle"`
	Blockers          []string      `json:"blockers"`
	Time              Time          `json:"time"`
	Traffic           Traffic       `json:"traffic"`
	Exit              *Exit         `json:"exit"`
	Access            *Access       `json:"access"`
	Quotas            task.Quotas   `json:"quotas"`
	OpenExitRequestID *string       `json:"openExitRequestId"`
	OpenOrderIDs      []string      `json:"openOrderIds"`
}

func (x *row) plan() Plan {
	return Plan{Name: x.PlanName, CountryCode: x.CC, Country: x.Country, City: x.City, Concurrency: x.Concurrency, BandwidthMbps: x.Bandwidth}
}

func (x *row) timeView(now time.Time) Time {
	t := Time{PeriodDays: x.EntitledSecs / 86400, StartedAt: kernel.TimePtr(x.FirstReady), ExpiresAt: kernel.TimePtr(x.Expires),
		RetentionEndsAt: kernel.TimePtr(x.RetentionEnds)}
	switch {
	case x.Expires != nil:
		if x.Expires.After(now) {
			t.RemainingSeconds = int64(x.Expires.Sub(now) / time.Second)
		}
	default:
		t.RemainingSeconds = max(0, x.EntitledSecs-x.BeforeSecs)
	}
	return t
}

// accessRow 是一个接入版本。
type accessRow struct {
	ID, State, ChangeKind, Username string
	Version                         int
	Enc                             []byte
	Ports                           Ports
	Unseen                          []string
	Issued                          time.Time
	Activated                       *time.Time
}

func loadAccess(ctx context.Context, q kernel.Q, where string, arg any) (*accessRow, error) {
	a := &accessRow{}
	err := q.QueryRow(ctx, `SELECT id, state, change_kind, username, version, password_enc, http_port, https_port, socks5_port,
	                          unseen_changes, issued_at, activated_at FROM access_versions WHERE `+where, arg).
		Scan(&a.ID, &a.State, &a.ChangeKind, &a.Username, &a.Version, &a.Enc, &a.Ports.HTTP, &a.Ports.HTTPS, &a.Ports.SOCKS5,
			&a.Unseen, &a.Issued, &a.Activated)
	if kernel.IsNoRows(err) {
		return nil, nil
	}
	if a.Unseen == nil {
		a.Unseen = []string{}
	}
	return a, err
}

// displayAccess 是展示给客户的接入：待生效优先（节点应用前 blockers 含 changing），否则当前。
func displayAccess(ctx context.Context, q kernel.Q, x *row) (*accessRow, error) {
	a, err := loadAccess(ctx, q, `subscription_id = $1 AND state = 'pending'`, x.ID)
	if err != nil || a != nil {
		return a, err
	}
	if x.CurAccess == nil {
		return nil, nil
	}
	return loadAccess(ctx, q, `id = $1`, *x.CurAccess)
}

// exitRow 是一个出口绑定及其资源。
type exitRow struct {
	ID, State, ResourceID, Provider, Protocol, Host string
	Version, Port                                    int
	VerifiedIP, VerifiedCC                           *string
	CheckResult, CheckIP, CheckCC, CheckCity, ASN    *string
	CheckError                                       *string
	Latency                                          *int
	CheckedAt, BoundAt, UnboundAt                    *time.Time
	UnboundReason, TaskID                            *string
	CreatedAt                                        time.Time
	ResCC, ResCountry, ResCity                       string
	ResLastIP                                        *string
}

const exitCols = `b.id, b.state, b.resource_id, r.provider, r.protocol, r.host, b.version, r.port, host(b.verified_ip),
	b.verified_country_code, b.check_result, host(b.check_ip), b.check_country_code, b.check_city, b.check_asn, b.check_error,
	b.latency_ms, b.checked_at, b.bound_at, b.unbound_at, b.unbound_reason, b.task_id, b.created_at, r.country_code, r.country_name,
	r.city, host(r.last_exit_ip)`

func scanExit(r interface{ Scan(...any) error }) (*exitRow, error) {
	e := &exitRow{}
	err := r.Scan(&e.ID, &e.State, &e.ResourceID, &e.Provider, &e.Protocol, &e.Host, &e.Version, &e.Port, &e.VerifiedIP, &e.VerifiedCC,
		&e.CheckResult, &e.CheckIP, &e.CheckCC, &e.CheckCity, &e.ASN, &e.CheckError, &e.Latency, &e.CheckedAt, &e.BoundAt, &e.UnboundAt,
		&e.UnboundReason, &e.TaskID, &e.CreatedAt, &e.ResCC, &e.ResCountry, &e.ResCity, &e.ResLastIP)
	return e, err
}

func loadExit(ctx context.Context, q kernel.Q, where string, arg any) (*exitRow, error) {
	e, err := scanExit(q.QueryRow(ctx, `SELECT `+exitCols+` FROM exit_bindings b JOIN upstream_resources r ON r.id = b.resource_id
	                                     WHERE `+where, arg))
	if kernel.IsNoRows(err) {
		return nil, nil
	}
	return e, err
}

// displayExit 是展示的出口：当前优先（换出口完成前仍是旧出口），开通中显示待生效的。
func displayExit(ctx context.Context, q kernel.Q, x *row) (*exitRow, error) {
	if x.CurExit != nil {
		return loadExit(ctx, q, `b.id = $1`, *x.CurExit)
	}
	if x.Lifecycle == "pending" {
		return loadExit(ctx, q, `b.subscription_id = $1 AND b.state = 'pending'`, x.ID)
	}
	return nil, nil
}

func (e *exitRow) ip() *string {
	for _, p := range []*string{e.CheckIP, e.VerifiedIP, e.ResLastIP} {
		if p != nil {
			return p
		}
	}
	return nil
}

func (e *exitRow) cc() *string {
	for _, p := range []*string{e.CheckCC, e.VerifiedCC} {
		if p != nil {
			return p
		}
	}
	return &e.ResCC
}

// check 返回检测结果；超过检测间隔两倍未更新为 stale（OpenAPI ExitCheck）。
func (e *exitRow) check(now time.Time, interval time.Duration) string {
	if e.CheckResult == nil || e.CheckedAt == nil || now.Sub(*e.CheckedAt) > 2*interval {
		return "stale"
	}
	return *e.CheckResult
}

func (e *exitRow) userView(now time.Time, interval time.Duration) *Exit {
	checked := e.CreatedAt
	if e.CheckedAt != nil {
		checked = *e.CheckedAt
	} else if e.BoundAt != nil {
		checked = *e.BoundAt
	}
	return &Exit{Version: e.Version, IP: kernel.StrOr(e.ip(), ""), CountryCode: *e.cc(), Country: e.ResCountry,
		City: kernel.StrOr(e.CheckCity, e.ResCity), Check: e.check(now, interval), LatencyMs: e.Latency, CheckedAt: checked.UTC()}
}

func (s *Service) userView(ctx context.Context, q kernel.Q, now time.Time, x *row) (*View, error) {
	v := &View{ID: x.ID, Name: x.Name, Plan: x.plan(), Lifecycle: x.Lifecycle, Blockers: x.Blockers, Time: x.timeView(now),
		Traffic: Traffic{TotalBytes: x.EntitledBytes, UsedBytes: x.UsedBytes}, OpenExitRequestID: x.OpenExitReq, OpenOrderIDs: x.OpenOrders}
	e, err := displayExit(ctx, q, x)
	if err != nil {
		return nil, err
	}
	if e != nil {
		v.Exit = e.userView(now, s.env.Cfg.ExitCheckInterval)
	}
	a, err := displayAccess(ctx, q, x)
	if err != nil {
		return nil, err
	}
	if a != nil {
		if v.Access, err = s.accessView(a, x); err != nil {
			return nil, err
		}
	}
	if v.Quotas, err = task.LoadQuotas(ctx, q, x.ID, x.RulesVersion, now); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Service) accessView(a *accessRow, x *row) (*Access, error) {
	pw, err := s.env.Open(a.Enc)
	if err != nil {
		return nil, err
	}
	return &Access{Version: a.Version, Host: kernel.StrOr(x.NodeHost, ""), Username: a.Username, Password: pw, Ports: a.Ports,
		IssuedAt: a.Issued.UTC(), UnseenChanges: a.Unseen}, nil
}

// ---- 管理端视图（OpenAPI AdminSubscription / AdminSubscriptionDetail） ----

// AdminPlan 是 AdminSubscription.plan。
type AdminPlan struct {
	Plan
	PlanID        string `json:"planId"`
	PlanVersionID string `json:"planVersionId"`
	PlanVersion   int    `json:"planVersion"`
}

// AdminTraffic 是 AdminSubscription.traffic。
type AdminTraffic struct {
	Traffic
	RemainingBytes int64 `json:"remainingBytes"`
}

// AdminExit 是 OpenAPI AdminExit。
type AdminExit struct {
	BindingID        string     `json:"bindingId"`
	Version          int        `json:"version"`
	ResourceID       string     `json:"resourceId"`
	Provider         string     `json:"provider"`
	UpstreamProtocol string     `json:"upstreamProtocol"`
	UpstreamAddr     string     `json:"upstreamAddr"`
	VerifiedIP       *string    `json:"verifiedIp"`
	IP               *string    `json:"ip"`
	CountryCode      *string    `json:"countryCode"`
	Country          string     `json:"country"`
	City             string     `json:"city"`
	Check            *string    `json:"check"`
	LatencyMs        *int       `json:"latencyMs"`
	CheckedAt        *time.Time `json:"checkedAt"`
	CheckError       *string    `json:"checkError"`
	BoundAt          *time.Time `json:"boundAt"`
}

// AdminAccess 是 OpenAPI AdminAccess（不含密码）。
type AdminAccess struct {
	ID          string     `json:"id"`
	Version     int        `json:"version"`
	State       string     `json:"state"`
	Host        string     `json:"host"`
	Username    string     `json:"username"`
	Ports       Ports      `json:"ports"`
	IssuedAt    time.Time  `json:"issuedAt"`
	ActivatedAt *time.Time `json:"activatedAt"`
}

// AdminView 是 OpenAPI AdminSubscription。
type AdminView struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	User              kernel.UserRef `json:"user"`
	NodeID            *string        `json:"nodeId"`
	Plan              AdminPlan      `json:"plan"`
	Lifecycle         string         `json:"lifecycle"`
	Blockers          []string       `json:"blockers"`
	AdminPaused       bool           `json:"adminPaused"`
	PausedReason      *string        `json:"pausedReason"`
	Time              Time           `json:"time"`
	Traffic           AdminTraffic   `json:"traffic"`
	Exit              *AdminExit     `json:"exit"`
	Access            *AdminAccess   `json:"access"`
	ActiveConns       *int           `json:"activeConns"`
	UsageReportedAt   *time.Time     `json:"usageReportedAt"`
	DesiredVersion    int64          `json:"desiredVersion"`
	AppliedVersion    int64          `json:"appliedVersion"`
	ActiveTaskID      *string        `json:"activeTaskId"`
	OpenExitRequestID *string        `json:"openExitRequestId"`
	OpenOrderIDs      []string       `json:"openOrderIds"`
	CreatedAt         time.Time      `json:"createdAt"`
}

// AdminDetail 是 OpenAPI AdminSubscriptionDetail。
type AdminDetail struct {
	AdminView
	Quotas             task.Quotas `json:"quotas"`
	ActiveTask         *task.View  `json:"activeTask"`
	RulesVersion       string      `json:"rulesVersion"`
	LifecycleReason    *string     `json:"lifecycleReason"`
	FirstReadyAt       *time.Time  `json:"firstReadyAt"`
	ReleasedAt         *time.Time  `json:"releasedAt"`
	DeletedAt          *time.Time  `json:"deletedAt"`
	CreatedFromOrderID *string     `json:"createdFromOrderId"`
}

func (e *exitRow) adminView(now time.Time, interval time.Duration) *AdminExit {
	v := &AdminExit{BindingID: e.ID, Version: e.Version, ResourceID: e.ResourceID, Provider: e.Provider, UpstreamProtocol: e.Protocol,
		UpstreamAddr: net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), VerifiedIP: e.VerifiedIP, IP: e.CheckIP, CountryCode: e.cc(),
		Country: e.ResCountry, City: kernel.StrOr(e.CheckCity, e.ResCity), LatencyMs: e.Latency, CheckedAt: kernel.TimePtr(e.CheckedAt),
		CheckError: e.CheckError, BoundAt: kernel.TimePtr(e.BoundAt)}
	if e.CheckResult != nil {
		v.Check = kernel.Ptr(e.check(now, interval))
	}
	return v
}

func (s *Service) adminView(ctx context.Context, q kernel.Q, now time.Time, x *row) (*AdminView, error) {
	v := &AdminView{ID: x.ID, Name: x.Name, User: kernel.UserRef{ID: x.UserID, Email: x.Email}, NodeID: x.NodeID,
		Plan:      AdminPlan{Plan: x.plan(), PlanID: x.PlanID, PlanVersionID: x.PlanVersionID, PlanVersion: x.PlanVersion},
		Lifecycle: x.Lifecycle, Blockers: x.Blockers, AdminPaused: x.AdminPaused, PausedReason: x.PausedReason, Time: x.timeView(now),
		Traffic: AdminTraffic{Traffic: Traffic{TotalBytes: x.EntitledBytes, UsedBytes: x.UsedBytes},
			RemainingBytes: max(0, x.EntitledBytes-x.UsedBytes)},
		UsageReportedAt: kernel.TimePtr(x.UsageReportedAt), DesiredVersion: x.DesiredV, AppliedVersion: x.AppliedV,
		ActiveTaskID: x.ActiveTask, OpenExitRequestID: x.OpenExitReq, OpenOrderIDs: x.OpenOrders, CreatedAt: x.CreatedAt.UTC()}
	if x.UsageReportedAt != nil && now.Sub(*x.UsageReportedAt) <= s.env.Cfg.FreshWindow {
		v.ActiveConns = kernel.Ptr(x.ActiveConns)
	}
	e, err := displayExit(ctx, q, x)
	if err != nil {
		return nil, err
	}
	if e != nil {
		v.Exit = e.adminView(now, s.env.Cfg.ExitCheckInterval)
	}
	a, err := displayAccess(ctx, q, x)
	if err != nil {
		return nil, err
	}
	if a != nil {
		v.Access = &AdminAccess{ID: a.ID, Version: a.Version, State: a.State, Host: kernel.StrOr(x.NodeHost, ""), Username: a.Username,
			Ports: a.Ports, IssuedAt: a.Issued.UTC(), ActivatedAt: kernel.TimePtr(a.Activated)}
	}
	return v, nil
}

func (s *Service) adminDetail(ctx context.Context, q kernel.Q, now time.Time, x *row) (*AdminDetail, error) {
	av, err := s.adminView(ctx, q, now, x)
	if err != nil {
		return nil, err
	}
	d := &AdminDetail{AdminView: *av, RulesVersion: x.RulesVersion, LifecycleReason: x.LifecycleReason,
		FirstReadyAt: kernel.TimePtr(x.FirstReady), ReleasedAt: kernel.TimePtr(x.Released), DeletedAt: kernel.TimePtr(x.Deleted),
		CreatedFromOrderID: x.CreatedFromOrder}
	if d.Quotas, err = task.LoadQuotas(ctx, q, x.ID, x.RulesVersion, now); err != nil {
		return nil, err
	}
	if x.ActiveTask != nil {
		if d.ActiveTask, err = task.Get(ctx, q, *x.ActiveTask); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// ---- 读取入口 ----

// Get 读本人订阅（userID 非空时只认本人且不含已删除，否则 404）。
func (s *Service) Get(ctx context.Context, q kernel.Q, id, userID string) (*View, error) {
	now := s.env.Now()
	x, err := loadRow(ctx, q, now, id, userID)
	if err != nil {
		return nil, err
	}
	return s.userView(ctx, q, now, x)
}

// ListForUser 列出本人订阅（不含已删除，新的在前）。
func (s *Service) ListForUser(ctx context.Context, userID string) ([]View, error) {
	now := s.env.Now()
	rows, err := s.env.Pool.Query(ctx, `SELECT `+outerCols+` FROM (`+baseSQL+` WHERE s.user_id = $2 AND s.lifecycle <> 'deleted') v
	                                     ORDER BY created_at DESC, id DESC`, now, userID)
	if err != nil {
		return nil, err
	}
	xs, err := collect(rows)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(xs))
	for _, x := range xs {
		v, err := s.userView(ctx, s.env.Pool, now, x)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

func collect(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]*row, error) {
	defer rows.Close()
	var xs []*row
	for rows.Next() {
		x, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		xs = append(xs, x)
	}
	return xs, rows.Err()
}

// AdminGet 读管理端订阅详情（含已删除）。
func (s *Service) AdminGet(ctx context.Context, q kernel.Q, id string) (*AdminDetail, error) {
	now := s.env.Now()
	x, err := loadRow(ctx, q, now, id, "")
	if err != nil {
		return nil, err
	}
	return s.adminDetail(ctx, q, now, x)
}

// AdminList 是管理端订阅列表（筛选、游标分页，新建的在前）。
func (s *Service) AdminList(ctx context.Context, f ListFilter) (*kernel.Page[AdminView], error) {
	now := s.env.Now()
	a := kernel.Args{Vals: []any{now}}
	if f.Q != "" {
		like := "%" + f.Q + "%"
		p1, p2 := a.Next(f.Q), a.Next(like)
		a.AddRaw(`(id = ` + p1 + ` OR name ILIKE ` + p2 + ` OR email ILIKE ` + p2 +
			` OR EXISTS (SELECT 1 FROM access_versions av WHERE av.subscription_id = v.id AND av.username = ` + p1 + `)` +
			` OR EXISTS (SELECT 1 FROM exit_bindings eb WHERE eb.subscription_id = v.id AND (host(eb.check_ip) = ` + p1 +
			` OR host(eb.verified_ip) = ` + p1 + `)))`)
	}
	if f.UserID != "" {
		a.Add("user_id = ?", f.UserID)
	}
	if f.NodeID != "" {
		a.Add("node_id = ?", f.NodeID)
	}
	if len(f.Lifecycle) > 0 {
		a.Add("lifecycle = ANY(?)", f.Lifecycle)
	} else {
		a.AddRaw("lifecycle <> 'deleted'")
	}
	if f.Blocker != "" {
		a.Add("? = ANY(blockers)", f.Blocker)
	}
	if f.Usable != nil {
		if *f.Usable {
			a.AddRaw("cardinality(blockers) = 0")
		} else {
			a.AddRaw("cardinality(blockers) > 0")
		}
	}
	if f.ExpiresBefore != nil {
		a.Add("expires_at < ?", *f.ExpiresBefore)
	}
	if f.Cursor != nil {
		a.AddRaw("(created_at, id) < (" + a.Next(f.Cursor.At) + ", " + a.Next(f.Cursor.ID) + ")")
	}
	rows, err := s.env.Pool.Query(ctx, `SELECT `+outerCols+` FROM (`+baseSQL+`) v`+a.SQL()+
		` ORDER BY created_at DESC, id DESC LIMIT `+strconv.Itoa(f.Limit+1), a.Vals...)
	if err != nil {
		return nil, err
	}
	xs, err := collect(rows)
	if err != nil {
		return nil, err
	}
	page := &kernel.Page[AdminView]{Items: []AdminView{}}
	for i, x := range xs {
		if i == f.Limit {
			last := xs[i-1]
			page.NextCursor = kernel.Ptr(kernel.Cursor{At: last.CreatedAt, ID: last.ID}.Encode())
			break
		}
		v, err := s.adminView(ctx, s.env.Pool, now, x)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *v)
	}
	return page, nil
}

// ListFilter 是管理端订阅列表的筛选条件。
type ListFilter struct {
	Q, UserID, NodeID, Blocker string
	Lifecycle                  []string
	Usable                     *bool
	ExpiresBefore              *time.Time
	Cursor                     *kernel.Cursor
	Limit                      int
}

// UserSubscriptions 是管理端用户详情的订阅列表（平台 admin.Hooks.UserSubscriptions）。
func (s *Service) UserSubscriptions(ctx context.Context, userID string) (any, error) {
	p, err := s.AdminList(ctx, ListFilter{UserID: userID, Lifecycle: []string{"pending", "fulfill_failed", "active", "retained",
		"released", "deleted"}, Limit: 200})
	if err != nil {
		return nil, err
	}
	return p.Items, nil
}
