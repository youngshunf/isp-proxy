package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/apispec"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
)

func (x *h) alertOpen(key string) bool {
	x.t.Helper()
	return x.int(`SELECT count(*) FROM alerts WHERE dedupe_key = $1 AND status <> 'resolved'`, key) > 0
}

func (x *h) used(sub string) int64 {
	x.t.Helper()
	return x.int(`SELECT used_bytes_total FROM subscriptions WHERE id = $1`, sub)
}

func (x *h) setBan(user string, banned bool) {
	x.t.Helper()
	st := "active"
	if banned {
		st = "banned"
	}
	err := pgx.BeginFunc(x.ctx, x.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(x.ctx, `UPDATE users SET status = $2, banned_at = CASE WHEN $2 = 'banned' THEN now() END WHERE id = $1`, user, st); err != nil {
			return err
		}
		return x.c.UserStatusChanged(context.Background(), tx, user)
	})
	if err != nil {
		x.t.Fatal(err)
	}
}

// 每种场景的期望状态：是否出现、是否启用、额度/期限/连接/带宽/接入/出口字段，且整份期望状态通过 Validate。
func TestDesiredStatePerScenario(t *testing.T) {
	type check func(x *h, sub string, e *agentproto.SubscriptionEntry)
	cases := []struct {
		name    string
		setup   func(x *h, u, sub string)
		present bool
		enabled bool
		check   check
	}{
		{name: "有效", present: true, enabled: true, check: func(x *h, sub string, e *agentproto.SubscriptionEntry) {
			if e.EntitledTotal != uint64(10*gb) || !e.ExpiresAt.Equal(x.expires(sub)) || e.MaxConns != 10 || e.BandwidthBps != 50_000_000 {
				x.t.Fatalf("%+v", e)
			}
			if e.Access == nil || e.Exit == nil || e.Exit.Protocol != agentproto.ExitSOCKS5 || e.Exit.Password != "uppw" ||
				!strings.HasPrefix(e.Exit.Addr, "127.0.0.1:") {
				x.t.Fatalf("接入/出口: %+v %+v", e.Access, e.Exit)
			}
			p := e.Access.Ports
			for _, port := range []int{p.HTTP, p.HTTPS, p.SOCKS5} {
				if port < 20000 || port > 20099 {
					x.t.Fatalf("端口应来自节点端口池: %+v", p)
				}
			}
		}},
		{name: "管理员暂停", present: true, setup: func(x *h, _, sub string) {
			x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/pause", map[string]any{"reason": "核查"}), 200, nil)
		}},
		{name: "账号封禁", present: true, setup: func(x *h, u, _ string) { x.setBan(u, true) }},
		{name: "流量耗尽（节点按额度执行）", present: true, enabled: true, setup: func(x *h, _, sub string) { x.exhaust(sub) },
			check: func(x *h, sub string, e *agentproto.SubscriptionEntry) {
				if e.UsedFloor != e.EntitledTotal {
					x.t.Fatalf("used_floor 应等于额度: %+v", e)
				}
			}},
		{name: "保留期", present: true, setup: func(x *h, _, sub string) {
			x.clk.Set(x.expires(sub).Add(time.Second))
			x.tick()
		}},
		{name: "已释放", setup: func(x *h, _, sub string) {
			x.clk.Set(x.expires(sub).Add(time.Second))
			x.tick()
			x.clk.Advance(7*24*time.Hour + time.Second)
			x.tick()
		}},
		{name: "节点停用", present: true, setup: func(x *h, _, _ string) {
			x.ok(x.do(x.asAdmin(), "PATCH", "/admin-api/nodes/"+x.node, map[string]any{"status": "disabled"}), 200, nil)
		}},
		{name: "重置密码进行中（下发新密码，同端口，不停用）", present: true, enabled: true, setup: func(x *h, u, sub string) {
			x.ok(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password"}, x.key()), 200, nil)
		}, check: func(x *h, sub string, e *agentproto.SubscriptionEntry) {
			cur := x.int(`SELECT a.version FROM subscriptions s JOIN access_versions a ON a.id = s.current_access_id WHERE s.id = $1`, sub)
			if int64(e.Access.Version) != cur+1 {
				x.t.Fatalf("应下发待生效接入: %d vs %d", e.Access.Version, cur)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := newH(t)
			u := x.newUser("ds@example.com")
			sub := x.activeSub(u)
			if c.setup != nil {
				c.setup(x, u, sub)
			}
			d := x.desired(x.node)
			if err := d.Validate(); err != nil {
				t.Fatalf("期望状态应通过校验: %v", err)
			}
			e := entry(d, sub)
			if (e != nil) != c.present {
				t.Fatalf("出现 = %v，期望 %v", e != nil, c.present)
			}
			if e == nil {
				return
			}
			if e.Enabled != c.enabled {
				t.Fatalf("enabled = %v，期望 %v", e.Enabled, c.enabled)
			}
			if c.check != nil {
				c.check(x, sub, e)
			}
		})
	}

	// 待开通（节点尚未应用）：带待生效接入与出口，未启用。
	t.Run("待开通", func(t *testing.T) {
		x := newH(t)
		x.resource()
		sub, task := x.createSub(x.newUser("pending@example.com"), 1)
		if task == "" {
			t.Fatal("应有履约任务")
		}
		d := x.desired(x.node)
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		e := entry(d, sub)
		if e == nil || e.Enabled || e.Access == nil || e.Exit == nil {
			t.Fatalf("%+v", e)
		}
	})
}

// 同一份期望状态里两个订阅不得共用端口：端口池分配 + 迁移 0002 的约束兜底；并发开通也成立。
func TestPortsNeverSharedAcrossSubscriptions(t *testing.T) {
	x := newH(t)
	const n = 8
	users := make([]string, n)
	for i := range users {
		users[i] = x.newUser(fmt.Sprintf("port%d@example.com", i))
		x.resource()
	}
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range users {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = x.do(x.asAdmin(), "POST", "/admin-api/subscriptions", map[string]any{"userId": users[i], "planId": x.plan, "months": 1,
				"payment": map[string]any{"method": "manual_transfer", "amount": map[string]any{"amount": 3000, "currency": "CNY"},
					"externalRef": fmt.Sprintf("BANK-P%d", i)}}, x.key()).status
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 201 {
			t.Fatalf("第 %d 个代建失败: %d", i, c)
		}
	}
	d := x.desired(x.node)
	if err := d.Validate(); err != nil {
		t.Fatalf("并发开通后期望状态应无端口冲突: %v", err)
	}
	seen := map[int]string{}
	for _, e := range d.Subscriptions {
		p := e.Access.Ports
		for _, port := range []int{p.HTTP, p.HTTPS, p.SOCKS5} {
			if o, ok := seen[port]; ok {
				t.Fatalf("端口 %d 同时属于 %s 与 %s", port, o, e.ID)
			}
			seen[port] = e.ID
		}
	}
	if len(d.Subscriptions) != n || len(seen) != 3*n {
		t.Fatalf("%d 个订阅，%d 个端口", len(d.Subscriptions), len(seen))
	}
	x.apply(x.node)

	// 数据库兜底：给另一订阅插入与他人端口重叠（跨协议）的接入版本 → unique_violation。
	a, b := d.Subscriptions[0], d.Subscriptions[1]
	_, err := x.pool.Exec(x.ctx, `INSERT INTO access_versions (subscription_id, version, node_id, change_kind, username, password_enc,
	                                http_port, https_port, socks5_port, state)
	                              VALUES ($1, 99, $2, 'ports_and_password', 'zzzzzzzz', '\x00', 20990, 20991, $3, 'pending')`,
		b.ID, x.node, a.Access.Ports.HTTP)
	if !store.IsUniqueViolation(err, "access_versions_ports_exclusive") {
		t.Fatalf("应被端口互斥约束拒绝: %v", err)
	}
	// 已撤销的接入不占端口。
	if _, err := x.pool.Exec(x.ctx, `INSERT INTO access_versions (subscription_id, version, node_id, change_kind, username, password_enc,
	                                http_port, https_port, socks5_port, state, revoked_at)
	                              VALUES ($1, 99, $2, 'ports_and_password', 'zzzzzzzz', '\x00', 20990, 20991, $3, 'revoked', now())`,
		b.ID, x.node, a.Access.Ports.HTTP); err != nil {
		t.Fatal(err)
	}
}

// A-03 / A-22：用量按订阅取较大值累计、互不串账；节点比控制面“新”（备份恢复）时版本前跳、不回退；其他节点不能改写。
func TestA03_A22_UsageIngest(t *testing.T) {
	x := newH(t)
	u1, u2 := x.newUser("a03a@example.com"), x.newUser("a03b@example.com")
	subs := []string{x.activeSub(u1), x.activeSub(u1), x.activeSub(u2), x.activeSub(u2)}
	want := map[string]uint64{}
	for i, s := range subs {
		want[s] = uint64(i+1) * 123_456_789
	}
	x.apply(x.node, want)
	for _, s := range subs {
		if x.used(s) != int64(want[s]) {
			t.Fatalf("%s: %d ≠ %d", s, x.used(s), want[s])
		}
	}
	// 用户只看到自己的订阅与用量。
	var list []subView
	x.ok(x.do(asUser(u1), "GET", "/api/subscriptions", nil), 200, &list)
	if len(list) != 2 || list[0].Traffic.UsedBytes+list[1].Traffic.UsedBytes != int64(want[subs[0]]+want[subs[1]]) {
		t.Fatalf("%+v", list)
	}
	// 较低的上报（例如乱序）不降低累计。
	lower := map[string]uint64{subs[0]: 1}
	x.apply(x.node, lower)
	if x.used(subs[0]) != int64(want[subs[0]]) {
		t.Fatal("累计用量不能回退")
	}
	// 另一节点上报本节点的订阅：忽略。
	other := x.newNode("node-b", 21000, 21099)
	x.report(other, &agentproto.Report{NodeTime: x.clk.Now(), PersistOK: true, Subscriptions: []agentproto.SubscriptionReport{
		{ID: subs[1], UsedTotal: 99 * uint64(gb), AppliedVersion: 999}}})
	if x.used(subs[1]) != int64(want[subs[1]]) || x.int(`SELECT applied_version FROM subscriptions WHERE id = $1`, subs[1]) >= 999 {
		t.Fatal("其他节点不能改写订阅用量与版本")
	}
	// 备份恢复：节点的已应用版本与用量都比控制面新 → 用量取较大值，版本前跳到节点之后，下发不会被节点当作旧版本。
	d := x.desired(x.node)
	e := entry(d, subs[2])
	ahead := &agentproto.Report{NodeTime: x.clk.Now(), PersistOK: true, AppliedRevision: d.Revision + 5,
		Subscriptions: []agentproto.SubscriptionReport{{ID: subs[2], UsedTotal: 7 * uint64(gb), AppliedVersion: e.Version + 3}}}
	ack := x.report(x.node, ahead)
	if ack.Revision <= ahead.AppliedRevision {
		t.Fatalf("节点修订号应前跳: %d ≤ %d", ack.Revision, ahead.AppliedRevision)
	}
	d2 := x.desired(x.node)
	e2 := entry(d2, subs[2])
	if d2.Revision <= ahead.AppliedRevision || e2.Version <= e.Version+3 || e2.UsedFloor != 7*uint64(gb) {
		t.Fatalf("恢复后期望状态: rev %d, ver %d, floor %d", d2.Revision, e2.Version, e2.UsedFloor)
	}
	if v := x.sub(u2, subs[2]); v.Traffic.UsedBytes != 7*gb {
		t.Fatalf("已消费流量不应重新开放: %+v", v.Traffic)
	}
	// 耗尽时记一次事件。
	x.apply(x.node, map[string]uint64{subs[3]: uint64(10 * gb)})
	x.apply(x.node, map[string]uint64{subs[3]: uint64(11 * gb)})
	if n := x.int(`SELECT count(*) FROM subscription_events WHERE subscription_id = $1 AND kind = 'exhausted'`, subs[3]); n != 1 {
		t.Fatalf("耗尽事件应恰好一次: %d", n)
	}
}

// A-16：猜测他人的订阅、订单、申请 ID，服务端一律 404，与不存在的 ID 无法区分，且不产生副作用。
func TestA16_OwnershipIsolation(t *testing.T) {
	x := newH(t)
	a, b := x.newUser("a16a@example.com"), x.newUser("a16b@example.com")
	sub := x.activeSub(a)
	renewOrder := x.renew(a, sub)
	var open idOnly
	x.ok(x.do(asUser(a), "POST", "/api/orders", map[string]any{"kind": "add_traffic", "subscriptionId": sub, "packId": x.pack}, x.key()), 201, &open)
	var req idOnly
	x.ok(x.do(asUser(a), "POST", "/api/exit-requests", map[string]any{"subscriptionId": sub, "reason": "quality"}, x.key()), 201, &req)
	name0 := x.str(`SELECT name FROM subscriptions WHERE id = $1`, sub)
	v0 := x.int(`SELECT desired_version FROM subscriptions WHERE id = $1`, sub)

	probes := func(subID, orderID, openID string) []struct {
		method, path string
		body         any
	} {
		return []struct {
			method, path string
			body         any
		}{
			{"GET", "/api/subscriptions/" + subID, nil},
			{"PATCH", "/api/subscriptions/" + subID, map[string]any{"name": "偷改"}},
			{"GET", "/api/subscriptions/" + subID + "/events", nil},
			{"POST", "/api/subscriptions/" + subID + "/access-changes", map[string]any{"mode": "password"}},
			{"POST", "/api/subscriptions/" + subID + "/access/ack", nil},
			{"GET", "/api/orders/" + orderID, nil},
			{"POST", "/api/orders/" + openID + "/claim-paid", map[string]any{}},
			{"POST", "/api/orders/" + openID + "/cancel", nil},
			{"POST", "/api/orders", map[string]any{"kind": "renew_time", "subscriptionId": subID, "months": 1}},
			{"POST", "/api/orders", map[string]any{"kind": "add_traffic", "subscriptionId": subID, "packId": x.pack}},
			{"POST", "/api/exit-requests", map[string]any{"subscriptionId": subID, "reason": "quality"}},
		}
	}
	real := probes(sub, renewOrder, open.ID)
	fake := probes(store.NewID("sub"), store.NewID("ord"), store.NewID("ord"))
	for i := range real {
		r := x.do(asUser(b), real[i].method, real[i].path, real[i].body, x.key())
		f := x.do(asUser(b), fake[i].method, fake[i].path, fake[i].body, x.key())
		if r.status != 404 || f.status != 404 || r.code() != f.code() {
			t.Fatalf("%s %s: 他人 %d %s / 不存在 %d %s", real[i].method, real[i].path, r.status, r.body, f.status, f.body)
		}
		if strings.Contains(string(r.body), sub) || strings.Contains(string(r.body), a) {
			t.Fatalf("%s %s: 响应泄露了 ID: %s", real[i].method, real[i].path, r.body)
		}
	}
	// 列表只含本人数据。
	for _, p := range []string{"/api/subscriptions", "/api/orders", "/api/exit-requests"} {
		r := x.do(asUser(b), "GET", p, nil)
		if r.status != 200 || strings.Contains(string(r.body), sub) || strings.Contains(string(r.body), open.ID) || strings.Contains(string(r.body), req.ID) {
			t.Fatalf("%s: %d %s", p, r.status, r.body)
		}
	}
	// 没有副作用。
	if x.str(`SELECT name FROM subscriptions WHERE id = $1`, sub) != name0 || x.int(`SELECT desired_version FROM subscriptions WHERE id = $1`, sub) != v0 {
		t.Fatal("越权请求不应改动订阅")
	}
	if st := x.str(`SELECT payment_status FROM orders WHERE id = $1`, open.ID); st != "awaiting" {
		t.Fatalf("越权请求不应改动订单: %s", st)
	}
	if n := x.int(`SELECT count(*) FROM orders WHERE user_id = $1`, b); n != 0 {
		t.Fatalf("不应替他人订阅下单: %d", n)
	}
	// 本人可以访问。
	x.ok(x.do(asUser(a), "GET", "/api/orders/"+renewOrder, nil), 200, nil)
}

// A-21：退款、赠送、调整、划转都是独立账本条目；账本只追加；不能直接改余额或已支付订单；扣回不得超过剩余。
func TestA21_LedgerIsAppendOnly(t *testing.T) {
	x := newH(t)
	u := x.newUser("a21@example.com")
	sub := x.activeSub(u)
	x.apply(x.node, map[string]uint64{sub: uint64(4 * gb)})
	entries := func() int64 { return x.int(`SELECT count(*) FROM ledger_entries WHERE subscription_id = $1`, sub) }
	n0 := entries()

	post := func(body map[string]any) resp {
		return x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/ledger-entries", body, x.key())
	}
	x.fail(post(map[string]any{"sourceType": "gift", "entitlementKind": "traffic", "amount": -gb, "reason": "x"}), 422, "invalid")
	x.fail(post(map[string]any{"sourceType": "adjust", "entitlementKind": "traffic", "amount": -11 * gb, "reason": "扣回超过合计"}), 412, "precondition")
	x.fail(post(map[string]any{"sourceType": "adjust", "entitlementKind": "time", "amount": -31 * day, "reason": "超额扣回"}), 412, "precondition")
	k := x.key()
	body := map[string]any{"sourceType": "gift", "entitlementKind": "traffic", "amount": 2 * gb, "reason": "补偿故障"}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/ledger-entries", body, k), 201, nil)
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/ledger-entries", body, k), 201, nil) // 幂等重放
	x.ok(post(map[string]any{"sourceType": "adjust", "entitlementKind": "traffic", "amount": -gb, "reason": "核对后扣回"}), 201, nil)
	if v := x.sub(u, sub); v.Traffic.TotalBytes != 11*gb || v.Traffic.UsedBytes != 4*gb {
		t.Fatalf("%+v", v.Traffic)
	}
	if entries() != n0+2 {
		t.Fatalf("应新增 2 条: %d", entries()-n0)
	}

	// 退款：续费订单退款并回收时长，经复核后写负数条目；订单与付款记录不变。
	ord := x.renew(u, sub)
	e1 := x.expires(sub)
	amount0 := x.int(`SELECT amount_cents FROM orders WHERE id = $1`, ord)
	pay0 := x.int(`SELECT count(*) FROM payments WHERE order_id = $1`, ord)
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/orders/"+ord+"/refunds", map[string]any{"amount": map[string]any{"amount": 3000, "currency": "CNY"},
		"reclaimBytes": 100 * gb, "reason": "客户申请"}), 412, "precondition")
	var rf idOnly
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/orders/"+ord+"/refunds", map[string]any{"amount": map[string]any{"amount": 3000, "currency": "CNY"},
		"reclaimSeconds": period, "reason": "客户申请"}), 201, &rf)
	if !x.expires(sub).Equal(e1) {
		t.Fatal("复核前不应回收")
	}
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/orders/"+ord+"/refunds", map[string]any{"amount": map[string]any{"amount": 100, "currency": "CNY"},
		"reason": "重复"}), 409, "conflict")
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/refunds/"+rf.ID+"/decision", map[string]any{"approve": true, "note": "同意", "externalRef": "RF-1"}), 200, nil)
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/refunds/"+rf.ID+"/decision", map[string]any{"approve": true, "note": "再来", "externalRef": "RF-2"}), 409, "conflict")
	if got := x.expires(sub); !got.Equal(e1.Add(-time.Duration(period) * time.Second)) {
		t.Fatalf("回收时长后到期应提前 30 天: %v → %v", e1, got)
	}
	if x.int(`SELECT count(*) FROM ledger_entries WHERE subscription_id = $1 AND source_type = 'refund' AND amount < 0`, sub) != 1 {
		t.Fatal("退款应写一条负数条目")
	}
	if x.int(`SELECT amount_cents FROM orders WHERE id = $1`, ord) != amount0 || x.int(`SELECT count(*) FROM payments WHERE order_id = $1`, ord) != pay0 {
		t.Fatal("退款不能改写已支付订单或付款记录")
	}

	// 数据库层面只追加。
	for _, sql := range []string{
		`UPDATE ledger_entries SET amount = amount + 1 WHERE subscription_id = '` + sub + `'`,
		`DELETE FROM ledger_entries WHERE subscription_id = '` + sub + `'`,
		`UPDATE subscriptions SET entitled_bytes_total = entitled_bytes_total + 1 WHERE id = '` + sub + `'`,
		`UPDATE subscriptions SET used_bytes_total = 0 WHERE id = '` + sub + `'`,
		`UPDATE payments SET amount_cents = 1 WHERE order_id = '` + ord + `'`,
		`DELETE FROM subscription_events WHERE subscription_id = '` + sub + `'`,
	} {
		if _, err := x.pool.Exec(x.ctx, sql); err == nil {
			t.Fatalf("应被拒绝: %s", sql)
		}
	}
	if !x.audit.has("ledger.create_entry") {
		t.Fatal("账本调整应记审计")
	}
}

// A-36：换出口申请：同一订阅只保留一条未完成申请（含并发）；免费次数按 D-19；客户视图不含内部字段；处理后时长与流量不变。
func TestA36_ExitRequests(t *testing.T) {
	x := newH(t)
	u := x.newUser("a36@example.com")
	sub := x.activeSub(u)
	x.apply(x.node, map[string]uint64{sub: uint64(3 * gb)})
	before := x.sub(u, sub)
	e0 := x.expires(sub)

	var wg sync.WaitGroup
	results := make([]resp, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = x.do(asUser(u), "POST", "/api/exit-requests", map[string]any{"subscriptionId": sub, "reason": "target_blocked",
				"note": "某站打不开"}, x.key())
		}(i)
	}
	wg.Wait()
	var created map[string]any
	var first string
	conflicts := 0
	for _, r := range results {
		switch r.status {
		case 201:
			if first != "" {
				t.Fatal("并发提交只能创建一条")
			}
			if err := json.Unmarshal(r.body, &created); err != nil {
				t.Fatal(err)
			}
			first = created["id"].(string)
		case 409:
			conflicts++
		default:
			t.Fatalf("%d %s", r.status, r.body)
		}
	}
	if first == "" || conflicts != len(results)-1 {
		t.Fatalf("创建 %q，冲突 %d", first, conflicts)
	}
	if created["charge"] != "free" || created["status"] != "pending" {
		t.Fatalf("%v", created)
	}
	for _, k := range []string{"internalNote", "feeStatus", "feeAmount", "taskId", "handledBy", "user"} {
		if _, ok := created[k]; ok {
			t.Fatalf("客户视图不应包含 %s", k)
		}
	}
	var conflict struct {
		Code     string `json:"code"`
		Existing idOnly `json:"existing"`
	}
	r := x.do(asUser(u), "POST", "/api/exit-requests", map[string]any{"subscriptionId": sub, "reason": "quality"}, x.key())
	x.ok(r, 409, &conflict)
	if conflict.Code != "conflict" || conflict.Existing.ID != first {
		t.Fatalf("%s", r.body)
	}

	// 免费申请：受理时只能选 free。
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/exit-requests/"+first+"/accept", map[string]any{"fee": "charged"}), 422, "invalid")
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/exit-requests/"+first+"/accept", map[string]any{"fee": "free", "internalNote": "供应商 vendor-x 换段"}), 200, nil)
	rs := x.do(asUser(u), "GET", "/api/exit-requests", nil)
	if rs.status != 200 || strings.Contains(string(rs.body), "vendor-x") || !strings.Contains(string(rs.body), `"status":"processing"`) {
		t.Fatalf("客户列表应显示处理中且不含内部备注: %s", rs.body)
	}

	// 关联换出口任务：节点应用后申请自动成功，新 IP 可见；时长、流量不变。
	res, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
	newIP := x.nextIP("us")
	probe, pr := x.compare(sub, res, newIP)
	if probe == "" {
		t.Fatalf("%d %s", pr.status, pr.body)
	}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/exit-changes", map[string]any{"resourceId": res, "compareProbeId": probe,
		"exitRequestId": first, "reason": "客户申请"}, x.key()), 200, nil)
	x.apply(x.node)
	x.answerProbes(x.node, x.probeIP) // 切换后检测通过
	var done struct {
		Status string  `json:"status"`
		NewIP  *string `json:"newIp"`
		TaskID *string `json:"taskId"`
	}
	x.ok(x.do(x.asAdmin(), "GET", "/admin-api/exit-requests/"+first, nil), 200, &done)
	if done.Status != "succeeded" || done.NewIP == nil || *done.NewIP != newIP || done.TaskID == nil {
		t.Fatalf("%+v", done)
	}
	after := x.sub(u, sub)
	if after.Traffic != before.Traffic || !x.expires(sub).Equal(e0) || after.Exit.IP != newIP {
		t.Fatalf("换出口后权益应不变: %+v → %+v", before.Traffic, after.Traffic)
	}

	// 免费次数用完：新申请进入人工确认，受理须选 charged（金额与参考）或 waived。
	var second struct {
		ID     string `json:"id"`
		Charge string `json:"charge"`
	}
	x.ok(x.do(asUser(u), "POST", "/api/exit-requests", map[string]any{"subscriptionId": sub, "reason": "region"}, x.key()), 201, &second)
	if second.Charge != "manual" {
		t.Fatalf("超出免费次数应为 manual: %s", second.Charge)
	}
	var adm struct {
		FeeStatus string `json:"feeStatus"`
	}
	x.ok(x.do(x.asAdmin(), "GET", "/admin-api/exit-requests/"+second.ID, nil), 200, &adm)
	if adm.FeeStatus != "awaiting_decision" {
		t.Fatal(adm.FeeStatus)
	}
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/exit-requests/"+second.ID+"/accept", map[string]any{"fee": "free"}), 422, "invalid")
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/exit-requests/"+second.ID+"/accept", map[string]any{"fee": "charged"}), 422, "invalid")
	// 未受理不能关联任务。
	res2, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
	probe2, _ := x.compare(sub, res2, x.nextIP("us"))
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/exit-changes", map[string]any{"resourceId": res2, "compareProbeId": probe2,
		"exitRequestId": second.ID, "reason": "客户申请"}, x.key()), 412, "precondition")
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/exit-requests/"+second.ID+"/accept", map[string]any{"fee": "waived"}), 200, nil)
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/exit-requests/"+second.ID+"/reject", map[string]any{}), 422, "invalid")
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/exit-requests/"+second.ID+"/reject", map[string]any{"failureReason": "该地域暂无可用资源"}), 200, nil)
	var mine []struct {
		ID            string  `json:"id"`
		Status        string  `json:"status"`
		FailureReason *string `json:"failureReason"`
	}
	x.ok(x.do(asUser(u), "GET", "/api/exit-requests", nil), 200, &mine)
	found := false
	for _, m := range mine {
		if m.ID == second.ID {
			found = m.Status == "failed" && m.FailureReason != nil && *m.FailureReason == "该地域暂无可用资源"
		}
	}
	if !found {
		t.Fatalf("%+v", mine)
	}
	if x.int(`SELECT count(*) FROM tasks WHERE subscription_id = $1 AND state IN ('queued', 'running')`, sub) != 0 {
		t.Fatal("驳回不应留下任务")
	}
}

// A-37：趋势图区分无数据（null）、真实 0、过期（stale）与无样本（no_data）；只读不扣量。
func TestA37_UsageSeriesDistinguishesNoDataZeroAndStale(t *testing.T) {
	x := newH(t)
	u := x.newUser("a37@example.com")
	start := x.clk.Now()
	sub := x.activeSub(u) // t0 上报 used 0
	q := func(from, to time.Time) (series struct {
		Freshness string `json:"freshness"`
		Unit      string `json:"unit"`
		Points    []struct {
			Bytes   *int64 `json:"bytes"`
			Samples int    `json:"samples"`
		} `json:"points"`
	}) {
		v := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}, "bucket": {"1h"}}
		x.ok(x.do(x.asAdmin(), "GET", "/admin-api/subscriptions/"+sub+"/usage?"+v.Encode(), nil), 200, &series)
		return
	}
	// 区间内没有样本：no_data，全部为 null。
	empty := q(start.Add(-48*time.Hour), start.Add(-45*time.Hour))
	if empty.Freshness != "no_data" || len(empty.Points) != 3 {
		t.Fatalf("%+v", empty)
	}
	for _, p := range empty.Points {
		if p.Bytes != nil {
			t.Fatal("无样本的桶应为 null")
		}
	}
	x.clk.Advance(10 * time.Minute)
	x.apply(x.node, map[string]uint64{sub: uint64(gb)})
	x.clk.Advance(time.Hour)
	x.apply(x.node, map[string]uint64{sub: uint64(gb)}) // 第二小时：有样本但无增长 → 0
	s := q(start, start.Add(3*time.Hour))
	if s.Freshness != "fresh" || s.Unit != "bytes" || len(s.Points) != 3 {
		t.Fatalf("%+v", s)
	}
	if s.Points[0].Bytes == nil || *s.Points[0].Bytes != gb {
		t.Fatalf("第一小时应为 1GB: %+v", s.Points[0])
	}
	if s.Points[1].Bytes == nil || *s.Points[1].Bytes != 0 {
		t.Fatalf("第二小时应为真实 0: %+v", s.Points[1])
	}
	if s.Points[2].Bytes != nil {
		t.Fatal("第三小时没有样本，应为 null")
	}
	x.clk.Advance(time.Minute)
	if s := q(start, start.Add(3*time.Hour)); s.Freshness != "stale" {
		t.Fatalf("超过 15 秒无上报应为 stale: %s", s.Freshness)
	}
	x.fail(x.do(x.asAdmin(), "GET", "/admin-api/subscriptions/"+sub+"/usage?from="+url.QueryEscape(start.Format(time.RFC3339))+
		"&to="+url.QueryEscape(start.Add(91*24*time.Hour).Format(time.RFC3339)), nil), 422, "invalid")
	if x.used(sub) != gb {
		t.Fatal("趋势查询不影响账本用量")
	}
}

// C-W9：持久化失败、时钟偏差（测量值或 node_time 粗估）、下发失败告警；条件消失后自动关闭。
func TestNodeAlertsPersistClockSkewAndApplyError(t *testing.T) {
	x := newH(t)
	u := x.newUser("alerts@example.com")
	sub := x.activeSub(u)
	base := func() *agentproto.Report {
		d := x.desired(x.node)
		return &agentproto.Report{NodeTime: x.clk.Now(), PersistOK: true, AppliedRevision: d.Revision}
	}
	r := base()
	r.PersistOK = false
	x.report(x.node, r)
	if !x.alertOpen("persist_failed:" + x.node) {
		t.Fatal("应告警持久化失败")
	}
	if sev := x.str(`SELECT severity FROM alerts WHERE dedupe_key = $1`, "persist_failed:"+x.node); sev != "critical" {
		t.Fatal(sev)
	}
	x.report(x.node, base())
	if x.alertOpen("persist_failed:" + x.node) {
		t.Fatal("恢复后应关闭")
	}

	r = base()
	r.ClockSkew = &agentproto.ClockSkew{LowMs: 1400, HighMs: 1600, Exceeded: true, MeasuredAt: x.clk.Now()}
	x.report(x.node, r)
	if !x.alertOpen("clock_skew:" + x.node) {
		t.Fatal("测量超限应告警")
	}
	if ms := x.int(`SELECT clock_skew_ms FROM nodes WHERE id = $1`, x.node); ms != 1500 {
		t.Fatalf("偏差取区间中点: %d", ms)
	}
	r = base()
	r.ClockSkew = &agentproto.ClockSkew{LowMs: -200, HighMs: 300, MeasuredAt: x.clk.Now()}
	x.report(x.node, r)
	if x.alertOpen("clock_skew:" + x.node) {
		t.Fatal("测量未超限应关闭")
	}
	r = base()
	r.NodeTime = x.clk.Now().Add(1500 * time.Millisecond) // 粗估在 1 秒余量内，不告警
	x.report(x.node, r)
	if x.alertOpen("clock_skew:" + x.node) {
		t.Fatal("粗估在余量内不应告警")
	}
	r = base()
	r.NodeTime = x.clk.Now().Add(-5 * time.Second)
	x.report(x.node, r)
	if !x.alertOpen("clock_skew:" + x.node) {
		t.Fatal("粗估偏差 5 秒应告警")
	}

	// 普通版本变化（暂停）下发时端口被占用 → critical 配置失败告警；之后应用成功 → 关闭。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/pause", map[string]any{"reason": "核查"}), 200, nil)
	x.failApply(x.node, sub, agentproto.ApplyPortInUse)
	if !x.alertOpen("config_failed:" + sub) {
		t.Fatal("应告警下发失败")
	}
	if sev := x.str(`SELECT severity FROM alerts WHERE dedupe_key = $1 AND status <> 'resolved'`, "config_failed:"+sub); sev != "critical" {
		t.Fatal(sev)
	}
	x.failApply(x.node, sub, agentproto.ApplyCertUnavailable)
	if !x.alertOpen("cert_risk:" + sub) {
		t.Fatal("证书不可用应告警")
	}
	x.apply(x.node)
	if x.alertOpen("config_failed:"+sub) || x.alertOpen("cert_risk:"+sub) {
		t.Fatal("应用成功后应关闭")
	}

	// 离线：超过阈值未上报 → 调度告警；再次上报 → 关闭。
	x.clk.Advance(10 * time.Minute)
	x.tick()
	if !x.alertOpen("node_offline:" + x.node) {
		t.Fatal("应告警节点离线")
	}
	x.report(x.node, base())
	if x.alertOpen("node_offline:" + x.node) {
		t.Fatal("上报后应关闭离线告警")
	}
}

// 内核路由与平台路由（账号、会话、管理员登录、用户管理、审计查询）恰好拼成契约全集，互不重叠。
func TestCoreRoutesAreContractMinusPlatform(t *testing.T) {
	x := newH(t)
	spec, err := apispec.Paths()
	if err != nil {
		t.Fatal(err)
	}
	mine := map[string]bool{}
	for _, r := range x.c.Routes() {
		p := r.Method + " " + r.Pattern
		if mine[p] {
			t.Fatalf("重复注册 %s", p)
		}
		mine[p] = true
		if _, ok := spec[p]; !ok {
			t.Fatalf("契约中没有 %s", p)
		}
	}
	platform := []string{"/api/me", "/api/sessions", "/api/account/", "/admin-api/session", "/admin-api/me", "/admin-api/users", "/admin-api/audit"}
	var rest []string
	for p := range spec {
		if mine[p] {
			continue
		}
		path := p[strings.Index(p, " ")+1:]
		ok := false
		for _, pre := range platform {
			if strings.HasPrefix(path, pre) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("未实现且不属于平台: %s", p)
		}
		rest = append(rest, p)
	}
	sort.Strings(rest)
	if len(mine) != 80 || len(rest) != 21 { // 内核含直出代理赠送与收回（D-25）、撤回待生效规则
		t.Fatalf("内核 %d、平台 %d: %v", len(mine), len(rest), rest)
	}
}
