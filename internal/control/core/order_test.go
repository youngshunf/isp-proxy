package core_test

import (
	"sync"
	"testing"
	"time"
)

// A-01：代建订阅生成唯一订阅、首个接入（三个端口）与出口；同一幂等键重复提交不多建；就绪后才起算。
func TestA01_FulfillCreatesOneSubscriptionAccessAndExit(t *testing.T) {
	x := newH(t)
	u := x.newUser("a01@example.com")
	res := x.resource()
	body := map[string]any{"userId": u, "planId": x.plan, "months": 1,
		"payment": map[string]any{"method": "manual_transfer", "amount": map[string]any{"amount": 3000, "currency": "CNY"}, "externalRef": "BANK-1"}}
	key := x.key()
	var r1, r2 struct {
		Subscription struct {
			ID        string `json:"id"`
			Lifecycle string `json:"lifecycle"`
		} `json:"subscription"`
		Task *struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"task"`
	}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions", body, key), 201, &r1)
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions", body, key), 201, &r2)
	if r1.Subscription.ID != r2.Subscription.ID || r1.Task == nil || r1.Task.Kind != "fulfill" {
		t.Fatalf("重复提交应返回同一结果: %+v %+v", r1, r2)
	}
	sub := r1.Subscription.ID
	if n := x.int(`SELECT count(*) FROM subscriptions WHERE user_id = $1`, u); n != 1 {
		t.Fatalf("订阅数 %d", n)
	}
	if n := x.int(`SELECT count(*) FROM access_versions WHERE subscription_id = $1`, sub); n != 1 {
		t.Fatalf("接入版本数 %d", n)
	}
	if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, res); st != "bound" {
		t.Fatalf("资源应已绑定: %s", st)
	}
	// 重复履约：已有进行中任务 → 409，不生成第二个接入（R-03.5）。
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/fulfillment", nil), 409, "conflict")
	v := x.sub(u, sub)
	if v.Lifecycle != "pending" || v.Time.ExpiresAt != nil || !eq(v.Blockers, []string{"pending"}) {
		t.Fatalf("就绪前不应起算: %+v", v)
	}
	e := entry(x.desired(x.node), sub)
	if e == nil || e.Enabled || e.Access == nil || e.Exit == nil {
		t.Fatalf("待履约条目应下发但不启用: %+v", e)
	}
	x.clk.Advance(10 * time.Second)
	x.apply(x.node)
	v = x.sub(u, sub)
	if v.Lifecycle != "active" || len(v.Blockers) != 0 || v.Time.ExpiresAt == nil || !v.Time.ExpiresAt.Equal(x.clk.Now().Add(30*24*time.Hour)) {
		t.Fatalf("就绪后从就绪时刻起算 30 天: %+v", v.Time)
	}
	if v.Access == nil || v.Exit == nil || v.Traffic.TotalBytes != 10*gb {
		t.Fatalf("%+v", v)
	}
	seen := map[int]bool{}
	for _, p := range v.Access.Ports.list() {
		if p < 20000 || p > 20099 || seen[p] {
			t.Fatalf("端口不合法: %+v", v.Access.Ports)
		}
		seen[p] = true
	}
	if e := entry(x.desired(x.node), sub); !e.Enabled || e.MaxConns != 10 || e.BandwidthBps != 50_000_000 || e.EntitledTotal != uint64(10*gb) {
		t.Fatalf("就绪后条目: %+v", e)
	}
	if !x.audit.has("subscription.create") || !x.audit.has("order.confirm_payment") {
		t.Fatalf("缺审计: %v", x.audit.actions)
	}
}

// A-08：同一订单并发确认、重试只入账一次；金额不同 422、流水号不同 409；未确认订单不入账。
func TestA08_ConcurrentConfirmCreditsOnce(t *testing.T) {
	x := newH(t)
	u := x.newUser("a08@example.com")
	sub := x.activeSub(u)
	var o struct {
		ID string `json:"id"`
	}
	x.ok(x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "add_traffic", "subscriptionId": sub, "packId": x.pack}, x.key()), 201, &o)
	if n := x.int(`SELECT count(*) FROM ledger_entries WHERE order_id = $1`, o.ID); n != 0 {
		t.Fatal("未确认不应入账")
	}
	x.fail(x.confirm(o.ID, 999, "REF-A", false), 422, "invalid")
	var wg sync.WaitGroup
	codes := make([]int, 12)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = x.confirm(o.ID, 1000, "REF-A", false).status
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != 200 {
			t.Fatalf("并发确认应全部 200: %v", codes)
		}
	}
	if n := x.int(`SELECT count(*) FROM ledger_entries WHERE order_id = $1`, o.ID); n != 1 {
		t.Fatalf("应只入账一次，得到 %d", n)
	}
	if b := x.int(`SELECT entitled_bytes_total FROM subscriptions WHERE id = $1`, sub); b != 20*gb {
		t.Fatalf("流量合计 %d", b)
	}
	x.fail(x.confirm(o.ID, 1000, "REF-B", false), 409, "conflict")
}

// A-27：并发购买最后一份资源只占用一次；未付预留 24 小时后回收；晚到款进入人工处置。
func TestA27_LastUnitAndHoldExpiryAndLatePayment(t *testing.T) {
	x := newH(t)
	res := x.resource()
	users := make([]string, 6)
	for i := range users {
		users[i] = x.newUser("buyer" + string(rune('a'+i)) + "@example.com")
	}
	var wg sync.WaitGroup
	codes := make([]int, len(users))
	ids := make([]string, len(users))
	for i, u := range users {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			r := x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "new", "planId": x.plan, "months": 1, "quantity": 1}, x.key())
			codes[i] = r.status
			if r.status == 201 {
				var o idOnly
				x.ok(r, 201, &o)
				ids[i] = o.ID
			}
		}(i, u)
	}
	wg.Wait()
	won := -1
	for i, c := range codes {
		switch c {
		case 201:
			if won >= 0 {
				t.Fatalf("最后一份资源被两次预留: %v", codes)
			}
			won = i
		case 409:
		default:
			t.Fatalf("意外状态: %v", codes)
		}
	}
	if won < 0 {
		t.Fatalf("应有一个成功: %v", codes)
	}
	if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, res); st != "reserved" {
		t.Fatal(st)
	}
	// 24 小时未付款 → 超时取消，预留回收。
	x.clk.Advance(24*time.Hour + time.Second)
	x.tick()
	if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, res); st != "available" {
		t.Fatalf("预留应回收: %s", st)
	}
	if st := x.str(`SELECT payment_status || '/' || cancel_reason FROM orders WHERE id = $1`, ids[won]); st != "cancelled/timeout" {
		t.Fatal(st)
	}
	// 晚到款：不勾选 lateArrival 拒绝；勾选后建订阅、入账，但不自动开通，进入人工处置并告警。
	x.fail(x.confirm(ids[won], 3000, "LATE-1", false), 412, "precondition")
	x.ok(x.confirm(ids[won], 3000, "LATE-1", true), 200, nil)
	sub := x.str(`SELECT subscription_id FROM orders WHERE id = $1`, ids[won])
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, sub); lc != "fulfill_failed" {
		t.Fatal(lc)
	}
	if n := x.int(`SELECT count(*) FROM alerts WHERE kind = 'late_payment' AND order_id = $1`, ids[won]); n != 1 {
		t.Fatal("缺晚到款告警")
	}
	if st := x.str(`SELECT fulfillment_status FROM orders WHERE id = $1`, ids[won]); st != "failed" {
		t.Fatal(st)
	}
	// 人工重试履约后正常开通，账本不重复。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/orders/"+ids[won]+"/fulfillment-retry", nil), 200, nil)
	x.apply(x.node)
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, sub); lc != "active" {
		t.Fatal(lc)
	}
	if n := x.int(`SELECT count(*) FROM ledger_entries WHERE subscription_id = $1`, sub); n != 2 {
		t.Fatalf("账本条目 %d", n)
	}
}

// R-16.4：每用户最多 3 个未付款新购；收款说明未配置时 412。
func TestUnpaidNewOrderLimitAndPaymentInfo(t *testing.T) {
	x := newH(t)
	for i := 0; i < 5; i++ {
		x.resource()
	}
	u := x.newUser("limit@example.com")
	in := map[string]any{"kind": "new", "planId": x.plan, "months": 1, "quantity": 1}
	for i := 0; i < 3; i++ {
		x.ok(x.do(asUser(u), "POST", "/api/orders", in, x.key()), 201, nil)
	}
	x.fail(x.do(asUser(u), "POST", "/api/orders", in, x.key()), 409, "conflict")
	x.fail(x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "new", "planId": x.plan, "months": 2, "quantity": 1}, x.key()), 422, "invalid")
	// 新规则版本未配置收款说明 → 拒绝下单。
	x.exec(`INSERT INTO rules (version, effective_from, period_days, retention_days, port_cooldown_days, resource_cooldown_days,
	          access_changes_per_window, access_change_window_hours, free_exit_changes_per_window, exit_change_window_days,
	          unpaid_hold_hours, max_unpaid_new_orders, month_options, snapshot_lines)
	        SELECT 'R-2026-11-09.1', '2026-11-09T12:00:00Z', period_days, retention_days, port_cooldown_days, resource_cooldown_days,
	          access_changes_per_window, access_change_window_hours, free_exit_changes_per_window, exit_change_window_days,
	          unpaid_hold_hours, max_unpaid_new_orders, month_options, snapshot_lines FROM rules WHERE version = 'R-2026-11-08'`)
	u2 := x.newUser("nopay@example.com")
	x.fail(x.do(asUser(u2), "POST", "/api/orders", in, x.key()), 412, "precondition")
}

// A-28：已收款但无库存 → 履约异常（订单与订阅都可见）；补货后重试不多建订阅、不重复入账。
func TestA28_NoStockShowsFulfillFailedAndRetryIsIdempotent(t *testing.T) {
	x := newH(t)
	u := x.newUser("a28@example.com")
	sub, task := x.createSub(u, 1)
	if task != "" {
		t.Fatal("无库存不应有履约任务")
	}
	order := x.str(`SELECT created_from_order_id FROM subscriptions WHERE id = $1`, sub)
	if st := x.str(`SELECT payment_status || '/' || fulfillment_status FROM orders WHERE id = $1`, order); st != "confirmed/failed" {
		t.Fatal(st)
	}
	v := x.sub(u, sub)
	if v.Lifecycle != "fulfill_failed" || !eq(v.Blockers, []string{"fulfill_failed"}) {
		t.Fatalf("%+v", v)
	}
	var o struct {
		Steps []struct {
			Label, State string
		} `json:"steps"`
	}
	x.ok(x.do(asUser(u), "GET", "/api/orders/"+order, nil), 200, &o)
	if last := o.Steps[len(o.Steps)-1]; last.State != "failed" {
		t.Fatalf("%+v", o.Steps)
	}
	if x.int(`SELECT count(*) FROM alerts WHERE kind = 'fulfill_failed' AND status = 'open'`) != 1 {
		t.Fatal("缺履约告警")
	}
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/orders/"+order+"/fulfillment-retry", nil), 409, "conflict") // 仍无库存
	x.resource()
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/orders/"+order+"/fulfillment-retry", map[string]any{}), 200, nil)
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/orders/"+order+"/fulfillment-retry", nil), 409, "conflict")
	x.apply(x.node)
	if x.int(`SELECT count(*) FROM subscriptions WHERE user_id = $1`, u) != 1 || x.int(`SELECT count(*) FROM ledger_entries WHERE subscription_id = $1`, sub) != 2 {
		t.Fatal("重试不应多建订阅或重复入账")
	}
	if st := x.str(`SELECT fulfillment_status FROM orders WHERE id = $1`, order); st != "delivered" {
		t.Fatal(st)
	}
	if x.int(`SELECT count(*) FROM alerts WHERE kind = 'fulfill_failed' AND status = 'open'`) != 0 {
		t.Fatal("就绪后履约告警应关闭")
	}
}

// A-38：套餐改价不追改旧订单；资源释放后再分配不带旧凭据或旧余额。
func TestA38_PriceChangeDoesNotRewriteOldOrders(t *testing.T) {
	x := newH(t)
	x.resource()
	u := x.newUser("a38@example.com")
	var o struct {
		ID     string
		Amount struct{ Amount int64 } `json:"amount"`
	}
	x.ok(x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "new", "planId": x.plan, "months": 3, "quantity": 1}, x.key()), 201, &o)
	oldVersion := x.str(`SELECT id FROM plan_versions WHERE plan_id = $1`, x.plan)
	x.exec(`INSERT INTO plan_versions (plan_id, version, name, country_code, country_name, city, price_cents, period_days, included_bytes,
	          concurrency, bandwidth_mbps) VALUES ($1, 2, '美国洛杉矶', 'us', '美国', '洛杉矶', 5000, 30, $2, 10, 50)`, x.plan, 10*gb)
	var again struct {
		Amount struct{ Amount int64 } `json:"amount"`
	}
	x.ok(x.do(asUser(u), "GET", "/api/orders/"+o.ID, nil), 200, &again)
	if o.Amount.Amount != 9000 || again.Amount.Amount != 9000 {
		t.Fatalf("旧订单金额被改: %d %d", o.Amount.Amount, again.Amount.Amount)
	}
	// 带旧版本号下新单 → 409（价格已变化）。
	x.fail(x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "new", "planId": x.plan, "planVersionId": oldVersion, "months": 1,
		"quantity": 1}, x.key()), 409, "conflict")
	x.ok(x.confirm(o.ID, 9000, "REF-38", false), 200, nil)
	x.apply(x.node)
	sub := x.str(`SELECT subscription_id FROM orders WHERE id = $1`, o.ID)
	if x.int(`SELECT entitled_seconds_total FROM subscriptions WHERE id = $1`, sub) != 3*period {
		t.Fatal("按下单时的订单行入账")
	}
}
