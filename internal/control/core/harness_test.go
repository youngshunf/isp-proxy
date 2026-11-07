package core_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/clock"
	"github.com/go-gost/gost/internal/control/core"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/store/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var t0 = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

const (
	gb     = int64(1_000_000_000)
	day    = int64(86400)
	period = 30 * day
)

// fakeAudit 记录审计（同事务写入的语义由 kernel 保证，这里只计数）。
type fakeAudit struct {
	mu      sync.Mutex
	actions []string
}

func (a *fakeAudit) Record(_ context.Context, _ pgx.Tx, adminID, action, target string, _ any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, action+" "+target)
	return nil
}

func (a *fakeAudit) has(action string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.actions {
		if strings.HasPrefix(x, action+" ") {
			return true
		}
	}
	return false
}

type h struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	clk   *clock.Fake
	c     *core.Core
	audit *fakeAudit
	srv   *httptest.Server
	geo   kernel.StaticGeo
	box   *secret.Box

	admin  string
	node   string
	plan   string
	pack   string
	seq    atomic.Int64
	ipSeq  atomic.Int64
	portSq atomic.Int64
}

func newH(t *testing.T) *h {
	t.Helper()
	pool := pgtest.New(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secret.NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	x := &h{t: t, ctx: context.Background(), pool: pool, clk: clock.NewFake(t0), audit: &fakeAudit{}, geo: kernel.StaticGeo{}, box: box}
	x.c, err = core.New(pool, kernel.Config{Box: box, Geo: x.geo, EchoURL: "http://echo.test/ip"}, x.clk, nil, x.audit, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, rt := range x.c.Routes() {
		mux.HandleFunc(rt.Method+" "+rt.Pattern, func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if u := r.Header.Get("X-Test-User"); u != "" {
				var email, status string
				var verified *time.Time
				if err := pool.QueryRow(ctx, `SELECT email, status, email_verified_at FROM users WHERE id = $1`, u).Scan(&email, &status, &verified); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				ctx = httpx.WithUser(ctx, httpx.Principal{UserID: u, Email: email, Verified: verified != nil, Banned: status == "banned"})
			}
			if a := r.Header.Get("X-Test-Admin"); a != "" {
				ctx = httpx.WithAdmin(ctx, httpx.AdminPrincipal{AdminID: a, Email: "ops@example.com"})
			}
			rt.Handler(w, r.WithContext(ctx))
		})
	}
	x.srv = httptest.NewServer(mux)
	t.Cleanup(x.srv.Close)

	// 种子：管理员、带收款说明的规则、节点（端口池 20000-20099）、套餐（美国洛杉矶）、流量包。
	x.admin = x.exec1(`INSERT INTO admins (email, display_name, password_hash) VALUES ('ops@example.com', 'ops', 'x') RETURNING id`)
	x.exec(`INSERT INTO rules (version, effective_from, period_days, retention_days, port_cooldown_days, resource_cooldown_days,
	          access_changes_per_window, access_change_window_hours, free_exit_changes_per_window, exit_change_window_days,
	          unpaid_hold_hours, max_unpaid_new_orders, month_options, snapshot_lines, payment_payee, payment_bank, payment_account, payment_note)
	        SELECT 'R-2026-11-09', '2026-11-09T00:00:00Z', period_days, retention_days, port_cooldown_days, resource_cooldown_days,
	          access_changes_per_window, access_change_window_hours, free_exit_changes_per_window, exit_change_window_days,
	          unpaid_hold_hours, max_unpaid_new_orders, month_options, snapshot_lines, '示例公司', '示例银行', '6222000000000000', '附言填付款识别码'
	        FROM rules WHERE version = 'R-2026-11-08'`)
	x.node = x.newNode("node-a", 20000, 20099)
	x.plan = x.exec1(`INSERT INTO plans (code, status) VALUES ('us-la', 'on_sale') RETURNING id`)
	x.exec(`INSERT INTO plan_versions (plan_id, version, name, country_code, country_name, city, price_cents, period_days, included_bytes,
	          concurrency, bandwidth_mbps) VALUES ($1, 1, '美国洛杉矶', 'us', '美国', '洛杉矶', 3000, 30, $2, 10, 50)`, x.plan, 10*gb)
	x.pack = x.exec1(`INSERT INTO traffic_packs (code, status) VALUES ('p10', 'on_sale') RETURNING id`)
	x.exec(`INSERT INTO traffic_pack_versions (traffic_pack_id, version, bytes, price_cents) VALUES ($1, 1, $2, 1000)`, x.pack, 10*gb)
	return x
}

func (x *h) exec(sql string, args ...any) {
	x.t.Helper()
	if _, err := x.pool.Exec(x.ctx, sql, args...); err != nil {
		x.t.Fatalf("%v\n%s", err, sql)
	}
}

func (x *h) exec1(sql string, args ...any) string {
	x.t.Helper()
	var s string
	if err := x.pool.QueryRow(x.ctx, sql, args...).Scan(&s); err != nil {
		x.t.Fatalf("%v\n%s", err, sql)
	}
	return s
}

func (x *h) int(sql string, args ...any) int64 {
	x.t.Helper()
	var n int64
	if err := x.pool.QueryRow(x.ctx, sql, args...).Scan(&n); err != nil {
		x.t.Fatalf("%v\n%s", err, sql)
	}
	return n
}

func (x *h) str(sql string, args ...any) string {
	x.t.Helper()
	var s *string
	if err := x.pool.QueryRow(x.ctx, sql, args...).Scan(&s); err != nil {
		x.t.Fatalf("%v\n%s", err, sql)
	}
	return kernel.StrOr(s, "")
}

func (x *h) newNode(name string, from, to int) string {
	id := x.exec1(`INSERT INTO nodes (name, public_host, port_range_start, port_range_end) VALUES ($1, $2, $3, $4) RETURNING id`,
		name, name+".example.test", from, to)
	x.exec(`INSERT INTO ports (node_id, port) SELECT $1, p FROM generate_series($2::int, $3::int) p`, id, from, to)
	return id
}

func (x *h) newUser(email string) string {
	return x.exec1(`INSERT INTO users (email, email_verified_at, password_hash) VALUES ($1, $2, 'x') RETURNING id`, email, t0.Add(-time.Hour))
}

// nextIP 返回一个新的出口 IP（文档网段），并登记国家。
func (x *h) nextIP(cc string) string {
	n := x.ipSeq.Add(1)
	ip := fmt.Sprintf("203.0.%d.%d", 113+n/250, n%250+1)
	x.geo[ip] = cc
	return ip
}

// newResource 录入一个可售资源（已验证，出口 IP 已知）。
func (x *h) newResource(node, cc, city, status string) (id, ip string) {
	ip = x.nextIP(cc)
	port := 30000 + x.portSq.Add(1)
	id = x.exec1(`INSERT INTO upstream_resources (node_id, protocol, host, port, username, password_enc, country_code, country_name, city,
	                 provider, status, last_exit_ip, last_exit_country_code, last_verified_at, last_verify_ok)
	              VALUES ($1, 'socks5', '127.0.0.1', $2, 'up', $3, $4, '美国', $5, 'vendor-x', $6, $7::inet, $4, $8, true) RETURNING id`,
		node, port, x.box.Seal([]byte("uppw"), nil), cc, city, status, ip, t0)
	return id, ip
}

func (x *h) resource() string {
	id, _ := x.newResource(x.node, "us", "洛杉矶", "available")
	return id
}

// ---- HTTP ----

type who struct{ user, admin string }

func asUser(id string) who  { return who{user: id} }
func (x *h) asAdmin() who   { return who{admin: x.admin} }
func (x *h) key() string    { return fmt.Sprintf("idem-key-%06d", x.seq.Add(1)) }

type resp struct {
	status int
	body   []byte
}

func (r resp) code() string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.body, &e)
	return e.Code
}

func (x *h) do(w who, method, path string, body any, idem ...string) resp {
	x.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, x.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if w.user != "" {
		req.Header.Set("X-Test-User", w.user)
	}
	if w.admin != "" {
		req.Header.Set("X-Test-Admin", w.admin)
	}
	if len(idem) > 0 {
		req.Header.Set("Idempotency-Key", idem[0])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, b}
}

func (x *h) ok(r resp, status int, out any) {
	x.t.Helper()
	if r.status != status {
		x.t.Fatalf("期望 %d，得到 %d: %s", status, r.status, r.body)
	}
	if out != nil {
		if err := json.Unmarshal(r.body, out); err != nil {
			x.t.Fatalf("%v: %s", err, r.body)
		}
	}
}

func (x *h) fail(r resp, status int, code string) {
	x.t.Helper()
	if r.status != status || (code != "" && r.code() != code) {
		x.t.Fatalf("期望 %d %s，得到 %d: %s", status, code, r.status, r.body)
	}
}

// ---- 业务快捷方式 ----

type idOnly struct {
	ID string `json:"id"`
}

// createSub 管理员代建订阅（manual_transfer 收款），返回订阅 ID 与履约任务 ID（可能为空）。
func (x *h) createSub(user string, months int) (sub, task string) {
	x.t.Helper()
	var res struct {
		Order        struct{ ID string }       `json:"order"`
		Subscription struct{ ID string }       `json:"subscription"`
		Task         *struct{ ID string }      `json:"task"`
	}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions", map[string]any{"userId": user, "planId": x.plan, "months": months,
		"payment": map[string]any{"method": "manual_transfer", "amount": map[string]any{"amount": 3000 * months, "currency": "CNY"},
			"externalRef": "BANK-" + x.key()}}, x.key()), 201, &res)
	if res.Task != nil {
		task = res.Task.ID
	}
	return res.Subscription.ID, task
}

// activeSub 代建并让节点应用，返回已就绪的订阅。
func (x *h) activeSub(user string) string {
	x.t.Helper()
	x.resource()
	sub, _ := x.createSub(user, 1)
	x.apply(x.node)
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, sub); lc != "active" {
		x.t.Fatalf("订阅应已就绪，得到 %s", lc)
	}
	return sub
}

// buy 用户下单 + 管理员确认收款，返回订单 ID。
func (x *h) buy(user string, in map[string]any) string {
	x.t.Helper()
	var o struct {
		ID     string `json:"id"`
		Amount struct {
			Amount int64 `json:"amount"`
		} `json:"amount"`
	}
	x.ok(x.do(asUser(user), "POST", "/api/orders", in, x.key()), 201, &o)
	x.confirm(o.ID, o.Amount.Amount, "REF-"+o.ID, false)
	return o.ID
}

func (x *h) confirm(order string, amount int64, ref string, late bool) resp {
	return x.do(x.asAdmin(), "POST", "/admin-api/orders/"+order+"/payment-confirmation", map[string]any{"method": "manual_transfer",
		"amount": map[string]any{"amount": amount, "currency": "CNY"}, "externalRef": ref, "lateArrival": late})
}

func (x *h) renew(user, sub string) string {
	return x.buy(user, map[string]any{"kind": "renew_time", "subscriptionId": sub, "months": 1})
}

func (x *h) addTraffic(user, sub string) string {
	return x.buy(user, map[string]any{"kind": "add_traffic", "subscriptionId": sub, "packId": x.pack})
}

// ---- 节点模拟 ----

func (x *h) desired(node string) *agentproto.Desired {
	x.t.Helper()
	d, err := x.c.AgentServer().Desired(x.ctx, node)
	if err != nil {
		x.t.Fatal(err)
	}
	return d
}

func entry(d *agentproto.Desired, sub string) *agentproto.SubscriptionEntry {
	for i := range d.Subscriptions {
		if d.Subscriptions[i].ID == sub {
			return &d.Subscriptions[i]
		}
	}
	return nil
}

// apply 模拟节点：拉取期望状态并回报全部条目已应用（用量取 used，未给出时取 used_floor）。
func (x *h) apply(node string, used ...map[string]uint64) *agentproto.ReportAck {
	x.t.Helper()
	d := x.desired(node)
	r := &agentproto.Report{NodeTime: x.clk.Now(), AgentVersion: "test", AppliedRevision: d.Revision, PersistOK: true}
	for _, e := range d.Subscriptions {
		u := e.UsedFloor
		if len(used) > 0 {
			if v, ok := used[0][e.ID]; ok {
				u = v
			}
		}
		r.Subscriptions = append(r.Subscriptions, agentproto.SubscriptionReport{ID: e.ID, UsedTotal: u, AppliedVersion: e.Version})
	}
	return x.report(node, r)
}

func (x *h) report(node string, r *agentproto.Report) *agentproto.ReportAck {
	x.t.Helper()
	ack, err := x.c.AgentServer().Report(x.ctx, node, r)
	if err != nil {
		x.t.Fatal(err)
	}
	return ack
}

// failApply 模拟节点下发目标版本失败（已应用版本不前进）。
func (x *h) failApply(node, sub, code string) {
	x.t.Helper()
	d := x.desired(node)
	e := entry(d, sub)
	applied := x.int(`SELECT applied_version FROM subscriptions WHERE id = $1`, sub)
	x.report(node, &agentproto.Report{NodeTime: x.clk.Now(), PersistOK: true, AppliedRevision: d.Revision - 1,
		Subscriptions: []agentproto.SubscriptionReport{{ID: sub, UsedTotal: e.UsedFloor, AppliedVersion: uint64(applied),
			ApplyError: &agentproto.ApplyError{Version: e.Version, Code: code, Message: "注入失败"}}}})
}

// answerProbes 模拟节点完成全部待执行探测：经出口观测到 ipOf(资源) 返回的 IP。
func (x *h) answerProbes(node string, ipOf func(p agentproto.Probe) (string, bool)) {
	x.t.Helper()
	d := x.desired(node)
	var rs []agentproto.ProbeResult
	for _, p := range d.Probes {
		ip, ok := ipOf(p)
		r := agentproto.ProbeResult{ProbeID: p.ID, OK: ok, ObservedIP: ip, LatencyMs: 120, CompletedAt: x.clk.Now()}
		if !ok {
			r.Error = "认证失败"
		}
		rs = append(rs, r)
	}
	if err := x.c.AgentServer().ProbeResults(x.ctx, node, rs); err != nil {
		x.t.Fatal(err)
	}
}

// probeIP 让探测返回该资源的 last_exit_ip（即真实出口不变）。
func (x *h) probeIP(p agentproto.Probe) (string, bool) {
	host, port := splitAddr(p.Exit.Addr)
	ip := x.str(`SELECT host(last_exit_ip) FROM upstream_resources WHERE host = $1 AND port = $2::int`, host, port)
	return ip, ip != ""
}

func splitAddr(a string) (string, string) {
	i := strings.LastIndex(a, ":")
	return a[:i], a[i+1:]
}

type subView struct {
	ID        string   `json:"id"`
	Lifecycle string   `json:"lifecycle"`
	Blockers  []string `json:"blockers"`
	Time      struct {
		ExpiresAt        *time.Time `json:"expiresAt"`
		RemainingSeconds int64      `json:"remainingSeconds"`
		RetentionEndsAt  *time.Time `json:"retentionEndsAt"`
	} `json:"time"`
	Traffic struct {
		TotalBytes int64 `json:"totalBytes"`
		UsedBytes  int64 `json:"usedBytes"`
	} `json:"traffic"`
	Exit *struct {
		Version int    `json:"version"`
		IP      string `json:"ip"`
	} `json:"exit"`
	Access *struct {
		Version       int      `json:"version"`
		Username      string   `json:"username"`
		Password      string   `json:"password"`
		Ports         ports    `json:"ports"`
		UnseenChanges []string `json:"unseenChanges"`
	} `json:"access"`
	Quotas struct {
		AccessChangesLeft   int `json:"accessChangesLeft"`
		FreeExitChangesLeft int `json:"freeExitChangesLeft"`
	} `json:"quotas"`
}

type ports struct {
	HTTP   int `json:"http"`
	HTTPS  int `json:"https"`
	SOCKS5 int `json:"socks5"`
}

func (p ports) list() []int { return []int{p.HTTP, p.HTTPS, p.SOCKS5} }

func (x *h) sub(user, id string) subView {
	x.t.Helper()
	var v subView
	x.ok(x.do(asUser(user), "GET", "/api/subscriptions/"+id, nil), 200, &v)
	return v
}

func (x *h) tick() { x.c.Tick(x.ctx) }

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = store.NewID
