package core_test

// 共享出口套餐（D-24，docs/06 §9.1）：名额、挑选、换出口、冷却、隔离告警与对客户的展示。

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/store"
)

// sharedPlan 建一个共享套餐（同地域，价格与默认套餐相同）。
func (x *h) sharedPlan() string {
	x.t.Helper()
	id := x.exec1(`INSERT INTO plans (code, status, exit_mode) VALUES ('us-la-shared', 'on_sale', 'shared') RETURNING id`)
	x.exec(`INSERT INTO plan_versions (plan_id, version, name, country_code, country_name, city, price_cents, period_days, included_bytes,
	          concurrency, bandwidth_mbps) VALUES ($1, 1, '美国洛杉矶共享', 'us', '美国', '洛杉矶', 3000, 30, $2, 10, 50)`, id, 10*gb)
	return id
}

// sharedResource 录入一个绑定上限为 n 的可售资源。
func (x *h) sharedResource(n int) (id, ip string) {
	x.t.Helper()
	id, ip = x.newResource(x.node, "us", "洛杉矶", "available")
	x.exec(`UPDATE upstream_resources SET max_tenants = $2 WHERE id = $1`, id, n)
	return id, ip
}

// provision 管理员代建指定套餐的订阅（不指定资源，按出口类型自动挑选）。
func (x *h) provision(user, plan string) (sub, task string) {
	x.t.Helper()
	var res struct {
		Subscription struct{ ID string }  `json:"subscription"`
		Task         *struct{ ID string } `json:"task"`
	}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions", map[string]any{"userId": user, "planId": plan, "months": 1,
		"payment": map[string]any{"method": "manual_transfer", "amount": map[string]any{"amount": 3000, "currency": "CNY"},
			"externalRef": "BANK-" + x.key()}}, x.key()), 201, &res)
	if res.Task != nil {
		task = res.Task.ID
	}
	return res.Subscription.ID, task
}

// resOf 返回订阅当前（待生效或生效）出口所在的资源。
func (x *h) resOf(sub string) string {
	return x.str(`SELECT resource_id FROM exit_bindings WHERE subscription_id = $1 AND state IN ('pending', 'active')
	               ORDER BY state = 'active' DESC LIMIT 1`, sub)
}

func (x *h) occupancy(res string) int64 {
	return x.int(`SELECT isp_shared_occupancy($1)`, res)
}

type upstreamResp struct {
	Status     string `json:"status"`
	MaxTenants int    `json:"maxTenants"`
	Occupancy  struct {
		Mode     string `json:"mode"`
		Used     int    `json:"used"`
		Bound    int    `json:"bound"`
		Reserved int    `json:"reserved"`
		Cooldown int    `json:"cooldown"`
	} `json:"occupancy"`
	Tenants []struct {
		SubscriptionID string `json:"subscriptionId"`
		ExitMode       string `json:"exitMode"`
	} `json:"tenants"`
	RowVersion int64 `json:"rowVersion"`
}

func (x *h) upstream(id string) upstreamResp {
	x.t.Helper()
	var u upstreamResp
	x.ok(x.do(x.asAdmin(), "GET", "/admin-api/upstreams/"+id, nil), 200, &u)
	return u
}

// A-47：上限为 3 的资源最多分给 3 个共享订阅，第 4 个分到其他资源或无货；多个共享资源时优先人数最少的；
// 独享套餐不分到有共享订阅的资源，共享套餐不分到独享资源或上限为 1 的资源。
func TestA47_SharedSlotsAndPicking(t *testing.T) {
	x := newH(t)
	shared := x.sharedPlan()
	r1, _ := x.sharedResource(3)
	r2, _ := x.sharedResource(3)
	solo, _ := x.sharedResource(1) // 只给独享
	// 独享订阅先占一份上限为 3 的资源：之后共享不能再分到它。
	exclRes, _ := x.sharedResource(3)
	excl, _ := x.provision(x.newUser("excl@example.com"), x.plan)
	if got := x.resOf(excl); got != solo {
		t.Fatalf("独享应优先上限为 1 的资源 %s，得到 %s", solo, got)
	}
	excl2, _ := x.provision(x.newUser("excl2@example.com"), x.plan)
	got := x.resOf(excl2)
	if got != r1 && got != r2 && got != exclRes {
		t.Fatalf("独享第二单应分到空闲资源，得到 %s", got)
	}
	// 让 excl2 固定在 exclRes 上，便于后续断言：若随机到 r1/r2，把 r1/r2 的角色互换。
	free := []string{r1, r2, exclRes}
	var sharedPool []string
	for _, r := range free {
		if r != got {
			sharedPool = append(sharedPool, r)
		}
	}
	exclRes = got
	x.apply(x.node)

	// 6 个共享订阅：交替落在两个共享资源上（人数最少优先），每个资源 3 个后满。
	var subs []string
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		s, task := x.provision(x.newUser("s"+string(rune('a'+i))+"@example.com"), shared)
		if task == "" {
			t.Fatalf("第 %d 个共享订阅不应无货", i+1)
		}
		r := x.resOf(s)
		if r != sharedPool[0] && r != sharedPool[1] {
			t.Fatalf("共享订阅分到了独享或上限为 1 的资源 %s", r)
		}
		counts[r]++
		if d := counts[sharedPool[0]] - counts[sharedPool[1]]; d > 1 || d < -1 {
			t.Fatalf("应优先人数最少的资源：%v", counts)
		}
		subs = append(subs, s)
	}
	x.apply(x.node)
	for _, r := range sharedPool {
		if n := x.occupancy(r); n != 3 {
			t.Fatalf("资源 %s 占用 %d", r, n)
		}
		u := x.upstream(r)
		if u.Status != "bound" || u.Occupancy.Mode != "shared" || u.Occupancy.Used != 3 || u.Occupancy.Bound != 3 || len(u.Tenants) != 3 {
			t.Fatalf("满员共享资源视图：%+v", u)
		}
	}
	if u := x.upstream(exclRes); u.Occupancy.Mode != "exclusive" || u.Status != "bound" || len(u.Tenants) != 1 || u.Tenants[0].ExitMode != "exclusive" {
		t.Fatalf("独享资源视图：%+v", u)
	}
	// 第 7 个：无货（共享资源都满了，独享资源与上限为 1 的资源不可用）。
	s7, task := x.provision(x.newUser("s7@example.com"), shared)
	if task != "" || x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, s7) != "fulfill_failed" {
		t.Fatal("共享名额用完后应无货")
	}
	// 用户自助下单同样无货。
	x.fail(x.do(asUser(x.newUser("buyer7@example.com")), "POST", "/api/orders",
		map[string]any{"kind": "new", "planId": shared, "months": 1, "quantity": 1}, x.key()), 409, "conflict")

	// 独享套餐不分到有共享订阅的资源：新录入一份才有货。
	e3, task := x.provision(x.newUser("excl3@example.com"), x.plan)
	if task != "" || x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, e3) != "fulfill_failed" {
		t.Fatalf("独享不应分到共享资源：%s", x.resOf(e3))
	}
	// 管理员指定资源同样受规则约束。
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+e3+"/fulfillment", map[string]any{"resourceId": sharedPool[0]}), 409, "conflict")
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+s7+"/fulfillment", map[string]any{"resourceId": solo}), 409, "conflict")

	// 目录库存：共享按剩余名额（0 → out）。
	var cat struct {
		Plans []struct {
			ID, ExitMode, Stock string
		} `json:"plans"`
	}
	x.ok(x.do(who{}, "GET", "/api/catalog", nil), 200, &cat)
	for _, p := range cat.Plans {
		if p.ID == shared && (p.ExitMode != "shared" || p.Stock != "out") {
			t.Fatalf("共享套餐目录：%+v", p)
		}
		if p.ID == x.plan && p.ExitMode != "exclusive" {
			t.Fatalf("独享套餐目录：%+v", p)
		}
	}
	// 新录入一个上限为 2 的共享资源：剩余名额 2，s7 重试履约即可开通。
	r3, _ := x.sharedResource(2)
	var plan struct {
		ExitMode string `json:"exitMode"`
		Stock    struct{ Available, Reserved, Bound int }
	}
	x.ok(x.do(x.asAdmin(), "GET", "/admin-api/plans/"+shared, nil), 200, &plan)
	if plan.ExitMode != "shared" || plan.Stock.Available != 2 || plan.Stock.Bound != 6 {
		t.Fatalf("共享套餐库存：%+v", plan)
	}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+s7+"/fulfillment", map[string]any{}), 200, nil)
	if x.resOf(s7) != r3 {
		t.Fatal("重试履约应分到新共享资源")
	}
}

// A-47：并发抢最后一个名额只有一个成功；未付款预留按名额计、超时回收。
func TestA47_ConcurrentLastSharedSlot(t *testing.T) {
	x := newH(t)
	shared := x.sharedPlan()
	res, _ := x.sharedResource(2)
	first, _ := x.provision(x.newUser("first@example.com"), shared)
	if x.resOf(first) != res {
		t.Fatal("应分到唯一的共享资源")
	}
	x.apply(x.node)
	users := make([]string, 6)
	for i := range users {
		users[i] = x.newUser("race" + string(rune('a'+i)) + "@example.com")
	}
	var wg sync.WaitGroup
	codes := make([]int, len(users))
	for i, u := range users {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			r := x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "new", "planId": shared, "months": 1, "quantity": 1}, x.key())
			codes[i] = r.status
		}(i, u)
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		switch c {
		case 201:
			won++
		case 409:
		default:
			t.Fatalf("意外状态: %v", codes)
		}
	}
	if won != 1 {
		t.Fatalf("最后一个名额被 %d 个订单拿到: %v", won, codes)
	}
	if n := x.occupancy(res); n != 2 {
		t.Fatalf("占用应为 2（1 绑定 + 1 订单预留），得到 %d", n)
	}
	if u := x.upstream(res); u.Occupancy.Reserved != 1 || u.Occupancy.Bound != 1 || u.Status != "bound" {
		t.Fatalf("%+v", u)
	}
	// 数据库兜底：直接再写一个订单预留名额被触发器拒绝。
	order := x.exec1(`INSERT INTO orders (user_id, kind, plan_id, plan_version_id, months, amount_cents, rules_version, rules_snapshot, hold_until)
	                  SELECT $1, 'new', p.id, v.id, 1, 3000, 'R-2026-11-08', '{}', now() + interval '1 day'
	                    FROM plans p JOIN plan_versions v ON v.plan_id = p.id WHERE p.id = $2 RETURNING id`, users[0], shared)
	if _, err := x.pool.Exec(x.ctx, `INSERT INTO resource_slots (resource_id, kind, order_id, until) VALUES ($1, 'order_hold', $2, now())`,
		res, order); store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("超出上限的名额应被拒绝: %v", err)
	}
	// 24 小时未付款 → 预留名额回收。
	x.clk.Advance(24*time.Hour + time.Second)
	x.tick()
	if n := x.occupancy(res); n != 1 {
		t.Fatalf("未付款预留应回收，占用 %d", n)
	}
}

// A-48（控制面部分）：同一共享出口上的两个订阅各有接入、共用上游；一个换接入不改变另一个的期望版本。
func TestA48_SharedTenantsIndependent(t *testing.T) {
	x := newH(t)
	shared := x.sharedPlan()
	res, ip := x.sharedResource(3)
	ua, ub := x.newUser("a48a@example.com"), x.newUser("a48b@example.com")
	a, _ := x.provision(ua, shared)
	b, _ := x.provision(ub, shared)
	x.apply(x.node)
	if x.resOf(a) != res || x.resOf(b) != res {
		t.Fatal("两个订阅应在同一共享资源上")
	}
	d := x.desired(x.node)
	ea, eb := entry(d, a), entry(d, b)
	if ea.Exit.Addr != eb.Exit.Addr || ea.Access.Username == eb.Access.Username || ea.Access.Ports.HTTP == eb.Access.Ports.HTTP {
		t.Fatalf("应共用上游、各有接入：%+v / %+v", ea.Exit, eb.Exit)
	}
	vb := eb.Version
	var acc accessResp
	x.ok(x.do(asUser(ua), "POST", "/api/subscriptions/"+a+"/access-changes", map[string]any{"mode": "ports_and_password"}, x.key()), 200, &acc)
	x.apply(x.node)
	if v := entry(x.desired(x.node), b).Version; v != vb {
		t.Fatalf("换接入不应影响同出口的其他订阅（%d → %d）", vb, v)
	}
	// 一个耗尽，另一个继续启用（耗尽由节点按各自额度执行，控制面各自展示）。
	x.apply(x.node, map[string]uint64{a: uint64(10 * gb)})
	x.wantBlockers(ua, a, "traffic_exhausted")
	x.wantBlockers(ub, b)

	// 用户视图：显示共享与上限 N，不暴露人数或其他订阅。
	var raw map[string]any
	x.ok(x.do(asUser(ub), "GET", "/api/subscriptions/"+b, nil), 200, &raw)
	plan := raw["plan"].(map[string]any)
	exit := raw["exit"].(map[string]any)
	if plan["exitMode"] != "shared" || exit["sharedCapacity"] != float64(3) || exit["ip"] != ip {
		t.Fatalf("共享订阅视图：plan=%v exit=%v", plan, exit)
	}
	body := string(x.do(asUser(ub), "GET", "/api/subscriptions/"+b, nil).body)
	for _, leak := range []string{a, res, ua, "tenants", "occupancy", "maxTenants"} {
		if strings.Contains(body, leak) {
			t.Fatalf("用户视图不应包含 %q：%s", leak, body)
		}
	}
	// 独享订阅：sharedCapacity 为 null。
	ue := x.newUser("a48e@example.com")
	e := x.activeSub(ue)
	x.ok(x.do(asUser(ue), "GET", "/api/subscriptions/"+e, nil), 200, &raw)
	if raw["plan"].(map[string]any)["exitMode"] != "exclusive" || raw["exit"].(map[string]any)["sharedCapacity"] != nil {
		t.Fatalf("独享订阅视图：%v", raw)
	}
}

// A-49：共享订阅换出口可换到另一个有空位的共享资源，不能换到独享出口；原资源其他订阅不受影响，腾出的名额冷却期内不再分配；
// 共享资源被隔离时其上每个订阅各自告警，后台列出受影响订阅；绑定上限不能改到低于占用。
func TestA49_SharedExitChangeCooldownQuarantine(t *testing.T) {
	x := newH(t)
	shared := x.sharedPlan()
	r1, _ := x.sharedResource(2)
	ua, ub := x.newUser("a49a@example.com"), x.newUser("a49b@example.com")
	a, _ := x.provision(ua, shared)
	b, _ := x.provision(ub, shared)
	x.apply(x.node)
	if x.resOf(a) != r1 || x.resOf(b) != r1 {
		t.Fatal("两个订阅应在 r1")
	}
	// 候选：独享订阅的出口（已占用）、上限为 1 的资源、当前资源都被拒绝。
	exclSub := x.activeSub(x.newUser("excl49@example.com"))
	exclRes := x.resOf(exclSub)
	if _, r := x.compare(a, exclRes, x.nextIP("us")); r.status != 409 {
		t.Fatalf("共享订阅不能换到独享出口：%d %s", r.status, r.body)
	}
	soloRes, _ := x.sharedResource(1)
	if _, r := x.compare(a, soloRes, x.nextIP("us")); r.status != 409 {
		t.Fatalf("共享订阅不能换到上限为 1 的资源：%d %s", r.status, r.body)
	}
	// 候选 IP 与独享订阅的出口相同 → 比较不通过（平台内独占）。
	exclIP := x.str(`SELECT host(b.verified_ip) FROM subscriptions s JOIN exit_bindings b ON b.id = s.current_exit_id WHERE s.id = $1`, exclSub)
	r2, r2ip := x.sharedResource(2)
	p, _ := x.compare(a, r2, exclIP)
	x.fail(x.exitChange(a, r2, p, false), 412, "precondition")

	// 换到 r2 成功；b 不受影响，a 在 r1 的名额冷却。
	vb := entry(x.desired(x.node), b).Version
	x.clk.Advance(time.Second)
	p, _ = x.compare(a, r2, r2ip)
	x.ok(x.exitChange(a, r2, p, false), 200, nil)
	if n := x.occupancy(r2); n != 1 {
		t.Fatalf("换出口进行中，待生效绑定应占 r2 一个名额：%d", n)
	}
	x.apply(x.node)
	x.answerProbes(x.node, x.probeIP)
	if x.resOf(a) != r2 || x.str(`SELECT state FROM tasks WHERE subscription_id = $1 AND kind = 'exit_change'`, a) != "succeeded" {
		t.Fatal("换出口应成功")
	}
	if v := entry(x.desired(x.node), b).Version; v != vb {
		t.Fatal("原资源上的其他订阅不应受影响")
	}
	if u := x.upstream(r1); u.Occupancy.Bound != 1 || u.Occupancy.Cooldown != 1 || u.Status != "bound" {
		t.Fatalf("r1 应为 1 绑定 + 1 冷却（满）：%+v", u)
	}
	// 冷却期内：新共享订阅只能去 r2；r2 满后无货。
	c, _ := x.provision(x.newUser("a49c@example.com"), shared)
	if x.resOf(c) != r2 {
		t.Fatal("冷却名额不应再分配")
	}
	x.apply(x.node)
	dsub, task := x.provision(x.newUser("a49d@example.com"), shared)
	if task != "" {
		t.Fatalf("冷却期内不应分到 r1：%s", x.resOf(dsub))
	}
	// 独享订阅也不能拿到只剩冷却名额的资源。
	x.exec(`UPDATE upstream_resources SET max_tenants = 3 WHERE id = $1`, r1)
	if _, r := x.compare(exclSub, r1, x.nextIP("us")); r.status != 409 {
		t.Fatalf("独享订阅不能换到有共享占用的资源：%d %s", r.status, r.body)
	}
	x.exec(`UPDATE upstream_resources SET max_tenants = 2 WHERE id = $1`, r1)
	// 冷却期满（D-18 7 天）后名额回来。
	x.clk.Advance(7*24*time.Hour + time.Minute)
	x.tick()
	if n := x.occupancy(r1); n != 1 {
		t.Fatalf("冷却到期后 r1 占用应为 1：%d", n)
	}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+dsub+"/fulfillment", map[string]any{}), 200, nil)
	if x.resOf(dsub) != r1 {
		t.Fatal("冷却到期后应分到 r1")
	}
	x.apply(x.node)

	// 绑定上限不能低于当前占用，有共享占用时不能改为 1。
	u := x.upstream(r2)
	x.fail(x.do(x.asAdmin(), "PATCH", "/admin-api/upstreams/"+r2, map[string]any{"rowVersion": u.RowVersion, "maxTenants": 1}), 412, "precondition")
	if _, err := x.pool.Exec(x.ctx, `UPDATE upstream_resources SET max_tenants = 1 WHERE id = $1`, r2); store.PgCode(err) != store.CodeCheckViolation {
		t.Fatalf("触发器应拒绝把有共享占用的资源上限改为 1：%v", err)
	}
	x.ok(x.do(x.asAdmin(), "PATCH", "/admin-api/upstreams/"+r2, map[string]any{"rowVersion": u.RowVersion, "maxTenants": 4}), 200, nil)

	// 隔离 r2（a、c 在上面）：每个订阅各自告警，详情列出受影响订阅。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/upstreams/"+r2+"/status", map[string]any{"status": "quarantine", "reason": "供应商通知线路故障"}), 200, nil)
	if n := x.int(`SELECT count(*) FROM alerts WHERE status = 'open' AND resource_id = $1 AND subscription_id IN ($2, $3)`, r2, a, c); n != 2 {
		t.Fatalf("应为两个订阅各自告警，得到 %d", n)
	}
	u = x.upstream(r2)
	got := map[string]bool{}
	for _, t := range u.Tenants {
		got[t.SubscriptionID] = true
	}
	if u.Status != "quarantine" || len(u.Tenants) != 2 || !got[a] || !got[c] {
		t.Fatalf("受影响订阅：%+v", u)
	}
	// 逐个换出口：a 换走后它的告警关闭，c 的仍在。
	r4, r4ip := x.sharedResource(3)
	p, _ = x.compare(a, r4, r4ip)
	x.ok(x.exitChange(a, r4, p, false), 200, nil)
	x.apply(x.node)
	x.answerProbes(x.node, x.probeIP)
	if x.resOf(a) != r4 {
		t.Fatal("a 应换到 r4")
	}
	if n := x.int(`SELECT count(*) FROM alerts WHERE status = 'open' AND resource_id = $1`, r2); n != 1 {
		t.Fatalf("a 换走后只剩 c 的告警，得到 %d", n)
	}
	// 重新上架关闭剩余告警。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/upstreams/"+r2+"/status", map[string]any{"status": "available", "reason": "已恢复"}), 200, nil)
	if n := x.int(`SELECT count(*) FROM alerts WHERE status = 'open' AND resource_id = $1`, r2); n != 0 {
		t.Fatalf("重新上架应关闭告警，剩 %d", n)
	}
}

// 共享订阅保留期满释放：名额冷却；切换后检测失败时只作废新绑定、隔离候选并为其上订阅告警。
func TestSharedReleaseAndVerifyFailure(t *testing.T) {
	x := newH(t)
	shared := x.sharedPlan()
	r1, _ := x.sharedResource(3)
	ua := x.newUser("rel@example.com")
	a, _ := x.provision(ua, shared)
	b, _ := x.provision(x.newUser("rel2@example.com"), shared)
	x.apply(x.node)
	// b 尝试换到 r2，切换后检测失败：b 回到 r1，r2 隔离，r2 上的订阅 c 收到告警。
	r2, _ := x.sharedResource(3)
	c, _ := x.provision(x.newUser("rel3@example.com"), shared)
	if x.resOf(c) != r2 {
		t.Fatal("c 应分到人数最少的 r2")
	}
	x.apply(x.node)
	p, _ := x.compare(b, r2, x.str(`SELECT host(last_exit_ip) FROM upstream_resources WHERE id = $1`, r2))
	x.ok(x.exitChange(b, r2, p, false), 200, nil)
	x.apply(x.node)
	x.answerProbes(x.node, func(pr agentproto.Probe) (string, bool) { return "", false })
	if x.resOf(b) != r1 || x.str(`SELECT failure_code FROM tasks WHERE subscription_id = $1 AND kind = 'exit_change'`, b) != "exit_check_failed" {
		t.Fatal("检测失败应回到原出口")
	}
	if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, r2); st != "quarantine" {
		t.Fatal(st)
	}
	if n := x.int(`SELECT count(*) FROM alerts WHERE status = 'open' AND resource_id = $1 AND subscription_id = $2`, r2, c); n != 1 {
		t.Fatal("候选隔离后其上的共享订阅应告警")
	}
	if x.resOf(c) != r2 {
		t.Fatal("c 的绑定不应被改动")
	}
	// a 到期、保留期满释放：r1 上腾出的名额冷却。
	x.clk.Set(x.expires(a).Add(time.Second))
	x.tick()
	x.clk.Advance(7*24*time.Hour + time.Second)
	x.tick()
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, a); lc != "released" {
		t.Fatalf("应已释放：%s", lc)
	}
	if n := x.int(`SELECT count(*) FROM resource_slots WHERE resource_id = $1 AND kind = 'cooldown' AND subscription_id = $2`, r1, a); n != 1 {
		t.Fatal("释放后名额应冷却")
	}
}
