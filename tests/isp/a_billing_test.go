//go:build ispe2e

package isp

import (
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 订单、收款、账本：A-08、A-09、A-21、A-27、A-28、A-38、A-45。

func entriesFor(l Ledger, source string) []LedgerEntry {
	var out []LedgerEntry
	for _, e := range l.Entries {
		if e.SourceType == source {
			out = append(out, e)
		}
	}
	return out
}

func entriesOfOrder(l Ledger, orderID string) []LedgerEntry {
	var out []LedgerEntry
	for _, e := range l.Entries {
		if e.OrderID != nil && *e.OrderID == orderID {
			out = append(out, e)
		}
	}
	return out
}

// A-08 同一订单并发确认、超时重试只入账一次；未确认订单不入账。
func TestA08_ConcurrentConfirmCreditsOnce(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	o, err := u.Order(OrderInput{Kind: "new", PlanID: e.Plan.ID, Months: 1, Quantity: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ao := e.Admin.Order(o.ID); len(ao.LedgerEntries) != 0 || ao.SubscriptionID != nil || ao.Payment != "awaiting" {
		t.Fatalf("未确认订单不应入账或建订阅：%+v", ao)
	}
	in := ConfirmPaymentInput{Method: "manual_transfer", Amount: o.Amount, ExternalRef: "BANK-A08-1"}
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.Admin.Do("POST", "/orders/"+o.ID+"/payment-confirmation", in, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发确认 %d：%v（重复确认应返回 200 与已有记录）", i, err)
		}
	}
	// 超时重试（同一请求体）仍是 200，且不再入账。
	var again Order
	e.Admin.Must("POST", "/orders/"+o.ID+"/payment-confirmation", in, &again)
	ao := e.Admin.Order(o.ID)
	if len(ao.LedgerEntries) != 2 {
		t.Fatalf("新购订单应恰好入账时长与流量各一条，得到 %d 条：%+v", len(ao.LedgerEntries), ao.LedgerEntries)
	}
	if ao.SubscriptionID == nil {
		t.Fatal("确认后应建订阅")
	}
	sub := *ao.SubscriptionID
	if l := e.Admin.Ledger(sub); len(entriesFor(l, "order")) != 2 {
		t.Fatalf("订阅账本应有 2 条订单入账：%+v", l.Entries)
	}
	if subs := u.Subs(); len(subs) != 1 {
		t.Fatalf("并发确认建出 %d 个订阅", len(subs))
	}
	// 请求体不同（流水号不同）返回 409。
	in2 := in
	in2.ExternalRef = "BANK-A08-OTHER"
	if _, err := e.Admin.Do("POST", "/orders/"+o.ID+"/payment-confirmation", in2, nil); StatusOf(err) != 409 {
		t.Fatalf("不同流水号重复确认应 409：%v", err)
	}
	s := e.WaitUsable(u, sub)

	// 续时长订单：未确认不入账；并发确认只顺延一次。
	before := e.Admin.Sub(sub)
	r, err := u.Order(OrderInput{Kind: "renew_time", SubscriptionID: sub, Months: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Admin.Sub(sub); !got.Time.ExpiresAt.Equal(*before.Time.ExpiresAt) {
		t.Fatal("未确认的续费订单改变了到期时间")
	}
	rin := ConfirmPaymentInput{Method: "manual_transfer", Amount: r.Amount, ExternalRef: "BANK-A08-2"}
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.Admin.Do("POST", "/orders/"+r.ID+"/payment-confirmation", rin, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("续费并发确认 %d：%v", i, err)
		}
	}
	after := e.Admin.Sub(sub)
	if d := after.Time.ExpiresAt.Sub(*before.Time.ExpiresAt); d != 30*24*time.Hour {
		t.Fatalf("续 1 个月应顺延 30 天，实际 %v", d)
	}
	if n := len(entriesOfOrder(e.Admin.Ledger(sub), r.ID)); n != 1 {
		t.Fatalf("续费订单入账 %d 条（应为 1）", n)
	}
	_ = s
}

// A-09 耗尽后买流量恢复；管理员暂停、用户封禁、节点异常时买流量仍不放行。
func TestA09_TopUpRestoresButNotOverOtherBlockers(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	c := CredsOf(s.Access)
	label := e.Exits[0].Label
	exhaust := func() {
		t.Helper()
		e.Admin.SetRemainingBytes(s.ID, 200*KB)
		var n atomic.Int64
		_ = Stream(Connect, c, e.Target(), 5*MB, &n, 20*time.Second)
		e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return x.Has("traffic_exhausted") }, "耗尽")
		e.MustReject(c, "耗尽")
	}
	buy := func() Order {
		t.Helper()
		return e.Renew(u, OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID})
	}

	section(t, "耗尽后买流量恢复")
	exhaust()
	buy()
	e.WaitUsable(u, s.ID)
	e.MustReach(c, label)

	section(t, "管理员暂停时买流量不放行")
	exhaust()
	e.Admin.Must("POST", "/subscriptions/"+s.ID+"/pause", map[string]string{"reason": "A-09"}, nil)
	buy()
	x := e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return !x.Has("traffic_exhausted") }, "买流量入账")
	if !x.Has("admin_paused") {
		t.Fatalf("暂停应仍阻断：%v", x.Blockers)
	}
	e.WaitApplied(s.ID)
	e.MustReject(c, "暂停中买流量")
	e.Admin.Must("POST", "/subscriptions/"+s.ID+"/resume", nil, nil)
	e.WaitUsable(u, s.ID)

	section(t, "用户封禁时买流量不放行")
	exhaust()
	// 封禁前已下单、封禁期间到账：确认入账，但封禁仍阻断。
	pending, err := u.Order(OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	e.Admin.Must("POST", "/users/"+u.ID+"/ban", map[string]string{"reason": "A-09"}, nil)
	// 封禁用户不能再下单（用户与管理员代下单都 412）。
	if _, err := u.Order(OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID}, ""); StatusOf(err) != 412 && StatusOf(err) != 401 {
		t.Fatalf("封禁用户下单应被拒（412）：%v", err)
	}
	if _, err := e.Admin.Do("POST", "/orders", map[string]any{"userId": u.ID, "input": OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID}},
		nil, Idem(NewKey())); StatusOf(err) != 412 {
		t.Fatalf("为封禁用户代下单应 412：%v", err)
	}
	ao := pending
	e.Admin.Confirm(ao.ID)
	as := e.WaitAdminSub(s.ID, 10*time.Second, func(a *AdminSubscription) bool { return !a.Has("traffic_exhausted") }, "买流量入账")
	if !as.Has("account_banned") {
		t.Fatalf("封禁应仍阻断：%v", as.Blockers)
	}
	e.WaitApplied(s.ID)
	e.MustReject(c, "封禁中买流量")
	e.Admin.Must("POST", "/users/"+u.ID+"/unban", nil, nil)
	if err := u.Login(); err != nil {
		t.Fatal(err)
	}
	e.WaitUsable(u, s.ID)

	section(t, "数据面异常时买流量不放行")
	exhaust()
	e.Exits[0].Up.Close() // 上游故障
	var p Probe
	e.Admin.Must("POST", "/subscriptions/"+s.ID+"/exit-checks", nil, &p)
	e.Admin.WaitProbe(p.ID)
	buy()
	x = e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return !x.Has("traffic_exhausted") }, "买流量入账")
	if !x.Has("dataplane") {
		t.Fatalf("出口检测失败后应显示 dataplane 阻断：%v", x.Blockers)
	}
	if body, err := e.Reach(Connect, c); err == nil {
		t.Fatalf("上游故障时不应可用：%q", body)
	}
	if e.CanaryHits() != 0 {
		t.Fatal("上游故障时发生直连")
	}
}

// A-21 退款、赠送、调整、划转保留账本条目，不能改已支付订单或覆盖余额。
func TestA21_LedgerAppendOnly(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	s, o := e.Purchase(u, 1)
	l0 := e.Admin.Ledger(s.ID)
	if len(l0.Entries) != 2 {
		t.Fatalf("初始账本：%+v", l0.Entries)
	}
	if _, err := e.Admin.AddLedger(s.ID, "gift", "traffic", 10*MB, "A-21 赠送"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Admin.AddLedger(s.ID, "adjust", "traffic", -5*MB, "A-21 扣回"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Admin.AddLedger(s.ID, "compensation", "time", 3600, "A-21 补时"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Admin.AddLedger(s.ID, "gift", "traffic", -1, "负赠送"); StatusOf(err) != 422 {
		t.Fatalf("赠送为负应 422：%v", err)
	}
	if _, err := e.Admin.AddLedger(s.ID, "adjust", "traffic", -10*GB, "扣成负数"); StatusOf(err) != 412 {
		t.Fatalf("扣回使合计为负应 412：%v", err)
	}
	l1 := e.Admin.Ledger(s.ID)
	if len(l1.Entries) != 5 {
		t.Fatalf("应追加 3 条：%d", len(l1.Entries))
	}
	for i, en := range l0.Entries {
		if l1.Entries[i].ID != en.ID || l1.Entries[i].Amount != en.Amount {
			t.Fatalf("已有条目被改：%+v → %+v", en, l1.Entries[i])
		}
	}
	if l1.EntitledBytesTotal != l0.EntitledBytesTotal+5*MB || l1.EntitledSecondsTotal != l0.EntitledSecondsTotal+3600 {
		t.Fatalf("合计应等于条目之和：%+v", l1)
	}
	var sum, secs int64
	for _, en := range l1.Entries {
		if en.EntitlementKind == "traffic" {
			sum += en.Amount
		} else {
			secs += en.Amount
		}
	}
	if sum != l1.EntitledBytesTotal || secs != l1.EntitledSecondsTotal {
		t.Fatalf("合计 %d/%d 与条目求和 %d/%d 不符", l1.EntitledBytesTotal, l1.EntitledSecondsTotal, sum, secs)
	}

	// 退款：复核 → 批准，追加负向条目；订单金额与行不变。
	orderBefore := e.Admin.Order(o.ID)
	if _, err := e.Admin.Do("POST", "/orders/"+o.ID+"/refunds", map[string]any{"amount": CNY(o.Amount.Amount + 1), "reclaimBytes": 0,
		"reclaimSeconds": 0, "reason": "超额"}, nil); StatusOf(err) != 422 && StatusOf(err) != 412 {
		t.Fatalf("退款超过订单金额应被拒：%v", err)
	}
	if _, err := e.Admin.Do("POST", "/orders/"+o.ID+"/refunds", map[string]any{"amount": CNY(1000), "reclaimBytes": 100 * GB,
		"reclaimSeconds": 0, "reason": "超额回收"}, nil); StatusOf(err) != 412 {
		t.Fatalf("回收超过剩余应 412：%v", err)
	}
	var rf Refund
	e.Admin.Must("POST", "/orders/"+o.ID+"/refunds", map[string]any{"amount": CNY(5000), "reclaimBytes": 1 * MB,
		"reclaimSeconds": 3600, "reason": "A-21 部分退款"}, &rf)
	if e.Admin.Order(o.ID).Payment != "refund_review" {
		t.Fatal("发起退款后订单应为 refund_review")
	}
	if _, err := e.Admin.Do("POST", "/orders/"+o.ID+"/refunds", map[string]any{"amount": CNY(1), "reclaimBytes": 0,
		"reclaimSeconds": 0, "reason": "第二个"}, nil); StatusOf(err) != 409 {
		t.Fatalf("同一订单第二个待复核退款应 409：%v", err)
	}
	e.Admin.Must("POST", "/refunds/"+rf.ID+"/decision", map[string]any{"approve": true, "note": "同意", "externalRef": "RF-1"}, &rf)
	if _, err := e.Admin.Do("POST", "/refunds/"+rf.ID+"/decision", map[string]any{"approve": false, "note": "再决定"}, nil); StatusOf(err) != 409 {
		t.Fatalf("重复决定应 409：%v", err)
	}
	orderAfter := e.Admin.Order(o.ID)
	if orderAfter.Payment != "refunded" || orderAfter.Amount != orderBefore.Amount || len(orderAfter.Lines) != len(orderBefore.Lines) {
		t.Fatalf("退款后订单：%+v（之前 %+v）", orderAfter, orderBefore)
	}
	l2 := e.Admin.Ledger(s.ID)
	refunds := entriesFor(l2, "refund")
	if len(refunds) != 2 {
		t.Fatalf("批准退款应追加时长、流量各一条负向条目：%+v", refunds)
	}
	for _, en := range refunds {
		if en.Amount >= 0 {
			t.Fatalf("退款条目应为负：%+v", en)
		}
	}
	if l2.EntitledBytesTotal != l1.EntitledBytesTotal-1*MB || l2.EntitledSecondsTotal != l1.EntitledSecondsTotal-3600 {
		t.Fatalf("退款回收后合计：%+v", l2)
	}
	// 已支付（已退款）订单不能取消。
	if _, err := e.Admin.Do("POST", "/orders/"+o.ID+"/cancel", map[string]string{"reason": "x"}, nil); StatusOf(err) != 412 {
		t.Fatalf("取消已支付订单应 412：%v", err)
	}
	if _, err := u.Do("POST", "/orders/"+o.ID+"/cancel", nil, nil); StatusOf(err) != 412 {
		t.Fatalf("用户取消已支付订单应 412：%v", err)
	}
	// 管理端没有覆盖余额的接口：契约中订阅只能改名。
	if _, err := e.Admin.Do("PATCH", "/subscriptions/"+s.ID, map[string]any{"name": "A21", "traffic": map[string]int64{"totalBytes": 1}}, nil); err == nil {
		if got := e.Admin.Ledger(s.ID); got.EntitledBytesTotal != l2.EntitledBytesTotal {
			t.Fatal("PATCH 改动了余额")
		}
	}
}

// A-27 并发购买最后一份资源只占用一次；未付预留 24 小时后回收；晚到款进入人工处置。
func TestA27_LastUnitHoldExpiryLatePayment(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	var users []*User
	for i := 0; i < 5; i++ {
		users = append(users, e.NewUser())
	}
	if cat := users[0].Catalog(); len(cat.Plans) != 1 || cat.Plans[0].Stock != "low" {
		t.Fatalf("1 份库存应为 low：%+v", cat.Plans)
	}
	orders := make([]Order, len(users))
	errs := make([]error, len(users))
	var wg sync.WaitGroup
	for i, u := range users {
		wg.Add(1)
		go func(i int, u *User) {
			defer wg.Done()
			orders[i], errs[i] = u.Order(OrderInput{Kind: "new", PlanID: e.Plan.ID, Months: 1, Quantity: 1}, "")
		}(i, u)
	}
	wg.Wait()
	var won int
	var winner Order
	for i, err := range errs {
		switch {
		case err == nil:
			won++
			winner = orders[i]
		case StatusOf(err) != 409:
			t.Fatalf("无货应 409：%v", err)
		}
	}
	if won != 1 {
		t.Fatalf("最后一份资源被 %d 个订单占用", won)
	}
	up := e.Admin.Upstream(e.Exits[0].ResourceID)
	if up.Status != "reserved" {
		t.Fatalf("资源应被预留：%+v", up)
	}
	if cat := users[0].Catalog(); cat.Plans[0].Stock != "out" {
		t.Fatalf("预留后库存应为 out：%s", cat.Plans[0].Stock)
	}
	if winner.HoldUntil == nil || winner.HoldUntil.Sub(winner.CreatedAt) != 24*time.Hour {
		t.Fatalf("未付款保留应为 24 小时：%+v", winner.HoldUntil)
	}

	// 24 小时未付款：调度取消订单并释放预留。
	e.ShiftClock(24*time.Hour + time.Minute)
	e.Eventually(10*time.Second, func() bool { return e.Admin.Order(winner.ID).Payment == "cancelled" }, "未付款订单 24 小时后未取消")
	ao := e.Admin.Order(winner.ID)
	if ao.CancelReason == nil || *ao.CancelReason != "timeout" {
		t.Fatalf("取消原因应为 timeout：%+v", ao.CancelReason)
	}
	e.Eventually(5*time.Second, func() bool { return e.Admin.Upstream(e.Exits[0].ResourceID).Status == "available" }, "预留未回收")

	// 晚到款：不勾选晚到款 412；勾选后入账但不自动开通，告警并等待人工处置。
	in := ConfirmPaymentInput{Method: "manual_transfer", Amount: winner.Amount, ExternalRef: "BANK-LATE"}
	if _, err := e.Admin.Do("POST", "/orders/"+winner.ID+"/payment-confirmation", in, nil); StatusOf(err) != 412 {
		t.Fatalf("超时订单直接确认应 412：%v", err)
	}
	in.LateArrival = true
	e.Admin.Must("POST", "/orders/"+winner.ID+"/payment-confirmation", in, &ao)
	if ao.PaymentRecord == nil || !ao.PaymentRecord.LateArrival {
		t.Fatalf("收款记录应标记晚到款：%+v", ao.PaymentRecord)
	}
	if ao.SubscriptionID == nil {
		t.Fatal("晚到款新购应建订阅并入账")
	}
	as := e.Admin.Sub(*ao.SubscriptionID)
	if as.Lifecycle != "fulfill_failed" {
		t.Fatalf("晚到款不应自动开通：%s", as.Lifecycle)
	}
	alerts := e.Admin.Alerts(url.Values{"kind": {"late_payment"}})
	if len(alerts) == 0 {
		t.Fatal("晚到款应产生 late_payment 告警")
	}
	// 人工处置：重新履约。
	e.Admin.Must("POST", "/orders/"+winner.ID+"/fulfillment-retry", nil, nil)
	for _, u := range users {
		if u.ID == as.User.ID {
			e.WaitUsable(u, as.ID)
		}
	}
}

// A-28 已收款但无库存或开通失败显示履约异常；重试不多建订阅、不重复入账。
func TestA28_FulfillFailureRetryIdempotent(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1, Extra: []ExitSpec{{Label: "nowhere", City: "Nowhere", NoVerify: true}}})
	u := e.NewUser()
	pv := DefaultPlanVersion()
	pv.City, pv.Name = "Nowhere", "无货城市"
	plan := e.Admin.CreatePlan("nowhere-"+randHex(2), pv)
	res, err := e.AdminProvision(u, ProvisionOpts{PlanID: plan.ID})
	if err != nil {
		t.Fatalf("无库存时代建应进入“已收款，交付异常”，而不是报错：%v", err)
	}
	sub := res.Subscription.ID
	as := e.WaitAdminSub(sub, 10*time.Second, func(a *AdminSubscription) bool { return a.Lifecycle == "fulfill_failed" }, "履约异常")
	if !as.Has("fulfill_failed") {
		t.Fatalf("阻断应含 fulfill_failed：%v", as.Blockers)
	}
	us := u.Sub(sub)
	if us.Lifecycle != "fulfill_failed" || len(us.Blockers) == 0 || us.Access != nil {
		t.Fatalf("用户视图应显示履约异常且无接入：%+v", us)
	}
	o := e.Admin.Order(res.Order.ID)
	if o.Payment != "confirmed" || o.Fulfillment != "failed" {
		t.Fatalf("订单应为已收款、交付异常：%s/%s", o.Payment, o.Fulfillment)
	}
	// 补货后并发重试。
	e.Admin.VerifyUpstream(e.Exit("nowhere").ResourceID, true)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.Admin.Do("POST", "/orders/"+o.ID+"/fulfillment-retry", nil, nil)
		}()
	}
	wg.Wait()
	e.WaitUsable(u, sub)
	if subs := u.Subs(); len(subs) != 1 {
		t.Fatalf("重试建出 %d 个订阅", len(subs))
	}
	if n := len(entriesOfOrder(e.Admin.Ledger(sub), o.ID)); n != 2 {
		t.Fatalf("重试后订单入账 %d 条（应为 2）", n)
	}
	// 再重试一次（已交付）不改变任何东西。
	_, _ = e.Admin.Do("POST", "/orders/"+o.ID+"/fulfillment-retry", nil, nil)
	if subs := u.Subs(); len(subs) != 1 {
		t.Fatal("已交付订单重试后订阅数变化")
	}
	e.MustReach(CredsOf(u.Sub(sub).Access), "nowhere")
}

// A-38 套餐改价或下架不追改旧订单；资源释放后再分配不带旧凭据或旧余额。
func TestA38_PriceChangeAndReallocationClean(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	cat := u.Catalog()
	oldVersion := cat.Plans[0].VersionID
	s, o := e.Purchase(u, 1)
	oldOrder := e.Admin.Order(o.ID)

	section(t, "改价")
	pv := DefaultPlanVersion()
	pv.Price = CNY(19900)
	var plan AdminPlan
	e.Admin.Must("POST", "/plans/"+e.Plan.ID+"/versions", pv, &plan)
	cat = u.Catalog()
	if cat.Plans[0].MonthlyPrice.Amount != 19900 || cat.Plans[0].VersionID == oldVersion {
		t.Fatalf("目录应显示新价格与新版本：%+v", cat.Plans[0])
	}
	if got := e.Admin.Order(o.ID); got.Amount != oldOrder.Amount || len(got.Lines) != len(oldOrder.Lines) ||
		got.Lines[0].Amount != oldOrder.Lines[0].Amount || deref(got.PlanVersionID) != deref(oldOrder.PlanVersionID) {
		t.Fatalf("改价追改了旧订单：%+v → %+v", oldOrder, got)
	}
	if as := e.Admin.Sub(s.ID); as.Plan.PlanVersionID != oldVersion {
		t.Fatalf("改价改变了已购订阅的套餐版本：%s → %s", oldVersion, as.Plan.PlanVersionID)
	}
	// 按旧版本下单 409（价格已变化）。
	if _, err := u.Order(OrderInput{Kind: "new", PlanID: e.Plan.ID, PlanVersionID: oldVersion, Months: 1, Quantity: 1}, ""); StatusOf(err) != 409 {
		t.Fatalf("旧价格版本下单应 409：%v", err)
	}

	section(t, "释放后再分配")
	oldCreds := CredsOf(s.Access)
	if _, _, err := Download(Connect, oldCreds, e.Target(), 2*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	e.SettledUsed(s.ID)
	e.Admin.Must("POST", "/subscriptions/"+s.ID+"/deletion", map[string]any{"reason": "A-38", "acknowledgeRemainingTraffic": true}, nil)
	e.Eventually(10*time.Second, func() bool { return Refused(oldCreds.Addr(Connect)) }, "删除后旧端口仍在监听")
	res := e.Exits[0].ResourceID
	if st := e.Admin.Upstream(res).Status; st != "cooldown" {
		t.Fatalf("删除后资源应进入冷却：%s", st)
	}
	e.ShiftClock(7*24*time.Hour + time.Hour) // 资源冷却 7 天
	e.Eventually(10*time.Second, func() bool { return e.Admin.Upstream(res).Status == "available" }, "冷却期满资源未回到可售")
	v := e.NewUser()
	s2 := e.Provision(v, ProvisionOpts{ResourceID: res})
	if s2.Traffic.UsedBytes != 0 || s2.Traffic.TotalBytes != DefaultPlanVersion().IncludedBytes {
		t.Fatalf("新订阅带了旧余额/旧用量：%+v", s2.Traffic)
	}
	if s2.Access.Username == oldCreds.User || s2.Access.Password == oldCreds.Pass {
		t.Fatal("新订阅复用了旧凭据")
	}
	nc := CredsOf(s2.Access)
	e.MustReach(nc, e.Exits[0].Label)
	// 旧凭据在新订阅的端口上被拒。
	e.MustReject(Creds{Host: nc.Host, Ports: nc.Ports, User: oldCreds.User, Pass: oldCreds.Pass}, "旧凭据访问新订阅")

	section(t, "下架")
	e.Admin.Must("POST", "/plans/"+e.Plan.ID+"/status", map[string]string{"status": "off_sale"}, nil)
	if c := u.Catalog(); len(c.Plans) != 0 {
		t.Fatalf("下架后目录仍有套餐：%+v", c.Plans)
	}
	if _, err := u.Order(OrderInput{Kind: "new", PlanID: e.Plan.ID, Months: 1, Quantity: 1}, ""); err == nil {
		t.Fatal("下架套餐仍可新购")
	}
	if got := e.Admin.Order(o.ID); got.Amount != oldOrder.Amount {
		t.Fatal("下架追改了旧订单")
	}
}

// A-45 管理员把已释放订阅的余额划转到同用户另一个订阅：两条成对账本条目，总额守恒；不能划给其他用户。
func TestA45_TransferReleasedBalance(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 3})
	u := e.NewUser()
	from := e.Provision(u, ProvisionOpts{})
	to := e.Provision(u, ProvisionOpts{})
	other := e.NewUser()
	otherSub := e.Provision(other, ProvisionOpts{})
	if _, _, err := Download(Connect, CredsOf(from.Access), e.Target(), 3*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	e.SettledUsed(from.ID)
	// 让来源订阅到期、保留期满释放。
	e.Admin.ExpireIn(from.ID, 2*time.Second)
	e.WaitAdminSub(from.ID, 15*time.Second, func(a *AdminSubscription) bool { return a.Lifecycle == "retained" }, "进入保留期")
	e.ShiftClock(7*24*time.Hour + time.Hour)
	fa := e.WaitAdminSub(from.ID, 15*time.Second, func(a *AdminSubscription) bool { return a.Lifecycle == "released" }, "释放")
	remaining := fa.Traffic.TotalBytes - fa.Traffic.UsedBytes
	if remaining <= 0 {
		t.Fatalf("释放后应保留余额：%+v", fa.Traffic)
	}
	ta := e.Admin.Sub(to.ID)
	totalBefore := fa.Traffic.TotalBytes + ta.Traffic.TotalBytes

	// 不能划给其他用户。
	if _, err := e.Admin.Do("POST", "/transfers", map[string]string{"fromSubscriptionId": from.ID, "toSubscriptionId": otherSub.ID,
		"reason": "跨用户"}, nil, Idem(NewKey())); StatusOf(err) != 412 {
		t.Fatalf("跨用户划转应 412：%v", err)
	}
	var tr Transfer
	key := NewKey()
	e.Admin.Must("POST", "/transfers", map[string]string{"fromSubscriptionId": from.ID, "toSubscriptionId": to.ID, "reason": "A-45"}, &tr, Idem(key))
	// 同键重试返回首次结果。
	var tr2 Transfer
	e.Admin.Must("POST", "/transfers", map[string]string{"fromSubscriptionId": from.ID, "toSubscriptionId": to.ID, "reason": "A-45"}, &tr2, Idem(key))
	if tr2.ID != tr.ID {
		t.Fatalf("同键重试建了第二笔划转：%s vs %s", tr.ID, tr2.ID)
	}
	if tr.Bytes != remaining {
		t.Fatalf("应全额划转 %d，实际 %d", remaining, tr.Bytes)
	}
	lf, lt := e.Admin.Ledger(from.ID), e.Admin.Ledger(to.ID)
	out, in := entriesFor(lf, "transfer_out"), entriesFor(lt, "transfer_in")
	if len(out) != 1 || len(in) != 1 || out[0].Amount != -remaining || in[0].Amount != remaining ||
		deref(out[0].TransferID) != tr.ID || deref(in[0].TransferID) != tr.ID {
		t.Fatalf("应有成对条目：out=%+v in=%+v", out, in)
	}
	fa, ta = e.Admin.Sub(from.ID), e.Admin.Sub(to.ID)
	if fa.Traffic.TotalBytes-fa.Traffic.UsedBytes > 0 {
		t.Fatalf("来源余额应归零：%+v", fa.Traffic)
	}
	if fa.Traffic.TotalBytes+ta.Traffic.TotalBytes != totalBefore {
		t.Fatalf("总额不守恒：%d → %d", totalBefore, fa.Traffic.TotalBytes+ta.Traffic.TotalBytes)
	}
	// 余额为 0 后再划转 412。
	if _, err := e.Admin.Do("POST", "/transfers", map[string]string{"fromSubscriptionId": from.ID, "toSubscriptionId": to.ID,
		"reason": "再次"}, nil, Idem(NewKey())); StatusOf(err) != 412 {
		t.Fatalf("无余额再划转应 412：%v", err)
	}
}
