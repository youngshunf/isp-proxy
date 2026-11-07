//go:build ispe2e

package isp

import (
	"sync"
	"testing"
	"time"
)

// 时长、流量与保留期：A-29～A-35、A-44。

// idleAll 在三种隧道接入（CONNECT、HTTPS 入口、SOCKS5）上各开一条空闲隧道。
// HTTP 普通转发的 keep-alive 连接会被 GOST 自行回收，不能用来判断“何时被停用关闭”。
func (e *Env) idleAll(c Creds) []*PConn {
	e.t.Helper()
	var out []*PConn
	for _, m := range TunnelModes {
		p, err := Open(m, c, e.Target())
		if err != nil {
			e.t.Fatalf("%s：%v", m, err)
		}
		e.t.Cleanup(func() { p.Close() })
		out = append(out, p)
	}
	return out
}

// closedNear 断言 conns 都在 at 前后 maxStopDelay 内关闭（不早于 at 前 200ms：节点按到期时间停用，不是提前停）。
func (e *Env) closedNear(conns []*PConn, at time.Time, why string) {
	e.t.Helper()
	times := make([]time.Time, len(conns))
	var wg sync.WaitGroup
	for i, p := range conns {
		wg.Add(1)
		go func(i int, p *PConn) {
			defer wg.Done()
			if _, ok := WaitClosed(p, time.Until(at)+10*time.Second); ok {
				times[i] = time.Now()
			}
		}(i, p)
	}
	wg.Wait()
	for i, ct := range times {
		if ct.IsZero() {
			e.t.Fatalf("%s：%s 存量连接未关闭", why, TunnelModes[i])
		}
		d := ct.Sub(at)
		if d > maxStopDelay || d < -200*time.Millisecond {
			e.t.Fatalf("%s：%s 存量连接在到期后 %v 关闭（要求 0～1 秒）", why, TunnelModes[i], d)
		}
		e.t.Logf("%s：%s 在到期后 %v 关闭", why, TunnelModes[i], d.Round(time.Millisecond))
	}
}

// A-29 到期但有流量：拒绝新连接，≤ 1 秒关闭存量，余额保留，其他订阅正常。
func TestA29_ExpiryStopsWithin1sKeepsBalance(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	other := e.Provision(e.NewUser(), ProvisionOpts{ResourceID: e.Exits[1].ResourceID})
	c := CredsOf(s.Access)
	if _, _, err := Download(Connect, c, e.Target(), 2*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	used := e.SettledUsed(s.ID)
	conns := e.idleAll(c)
	exp := e.Admin.ExpireIn(s.ID, 3*time.Second)
	e.WaitApplied(s.ID)
	e.closedNear(conns, exp, "到期")
	e.MustReject(c, "到期后")
	x := e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return x.Has("expired") && x.Lifecycle == "retained" }, "到期进入保留期")
	if x.Traffic.TotalBytes != s.Traffic.TotalBytes || x.Traffic.UsedBytes < used || x.Traffic.TotalBytes-x.Traffic.UsedBytes <= 0 {
		t.Fatalf("到期后余额应保留：%+v", x.Traffic)
	}
	if x.Has("traffic_exhausted") {
		t.Fatal("有流量的订阅不应显示耗尽")
	}
	e.MustReach(CredsOf(other.Access), e.Exits[1].Label)
}

// A-30 时长有效、流量耗尽：买流量后恢复，到期时间不变；只续时长不能恢复。
func TestA30_ExhaustedRenewTimeDoesNotRestore(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	e.Admin.SetRemainingBytes(s.ID, 0)
	e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return x.Has("traffic_exhausted") }, "耗尽")
	e.WaitApplied(s.ID)
	c := CredsOf(s.Access)
	e.MustReject(c, "耗尽")
	exp0 := *u.Sub(s.ID).Time.ExpiresAt

	section(t, "只续时长")
	e.Renew(u, OrderInput{Kind: "renew_time", SubscriptionID: s.ID, Months: 1})
	x := e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return x.Time.ExpiresAt.After(exp0) }, "续时长入账")
	if !x.Has("traffic_exhausted") || x.Traffic.TotalBytes-x.Traffic.UsedBytes > 0 {
		t.Fatalf("只续时长不应恢复或加流量：%v %+v", x.Blockers, x.Traffic)
	}
	if d := x.Time.ExpiresAt.Sub(exp0); d != 30*24*time.Hour {
		t.Fatalf("续 1 个月应顺延 30 天：%v", d)
	}
	e.WaitApplied(s.ID)
	e.MustReject(c, "只续时长后")

	section(t, "买流量")
	exp1 := *x.Time.ExpiresAt
	e.Renew(u, OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID})
	x = e.WaitUsable(u, s.ID)
	if !x.Time.ExpiresAt.Equal(exp1) {
		t.Fatalf("买流量改变了到期时间：%v → %v", exp1, x.Time.ExpiresAt)
	}
	// 调整到 0 之后还可能结算到几百字节的探活用量，允许 10 KB 误差。
	if rem := x.Traffic.TotalBytes - x.Traffic.UsedBytes; rem > e.Pack.Current.Bytes || rem < e.Pack.Current.Bytes-10*KB {
		t.Fatalf("买流量后剩余应为流量包大小：%+v", x.Traffic)
	}
	e.MustReach(c, e.Exits[0].Label)
}

// A-31 保留期内有流量：续时长恢复原出口和原接入，流量不变；只买流量仍停用。
func TestA31_RetentionRenewRestoresSameExitAndAccess(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	c := CredsOf(s.Access)
	e.Admin.ExpireIn(s.ID, 2*time.Second)
	e.WaitSub(u, s.ID, 15*time.Second, func(x *Subscription) bool { return x.Lifecycle == "retained" }, "保留期")
	e.WaitApplied(s.ID)
	e.MustReject(c, "保留期")
	if up := e.Admin.Upstream(e.Exits[0].ResourceID); up.Status != "bound" || deref(up.BoundSub) != s.ID {
		t.Fatalf("保留期内资源应仍为该订阅保留：%+v", up)
	}

	section(t, "只买流量")
	tb := u.Sub(s.ID).Traffic.TotalBytes
	e.Renew(u, OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID})
	x := e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return x.Traffic.TotalBytes > tb }, "买流量入账")
	if x.Lifecycle != "retained" || !x.Has("expired") {
		t.Fatalf("只买流量仍应停用：%s %v", x.Lifecycle, x.Blockers)
	}
	e.WaitApplied(s.ID)
	e.MustReject(c, "保留期只买流量")

	section(t, "续时长")
	tb = x.Traffic.TotalBytes
	e.Renew(u, OrderInput{Kind: "renew_time", SubscriptionID: s.ID, Months: 1})
	x = e.WaitUsable(u, s.ID)
	if x.Access.Version != s.Access.Version || x.Access.Ports != s.Access.Ports || x.Access.Password != s.Access.Password {
		t.Fatalf("续时长应恢复原接入：%+v → %+v", s.Access, x.Access)
	}
	if x.Traffic.TotalBytes != tb {
		t.Fatalf("续时长不应改变流量：%d → %d", tb, x.Traffic.TotalBytes)
	}
	if !x.Time.ExpiresAt.After(e.Now()) {
		t.Fatalf("续时长后应未到期：%v", x.Time.ExpiresAt)
	}
	e.MustReach(c, e.Exits[0].Label)
}

// A-32 时长和流量都用尽：只补一项仍不可用，两项补足才恢复；前台同时展示两个阻断。
func TestA32_BothExhaustedNeedBoth(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	c := CredsOf(s.Access)
	e.Admin.SetRemainingBytes(s.ID, 0)
	e.Admin.ExpireIn(s.ID, 2*time.Second)
	x := e.WaitSub(u, s.ID, 15*time.Second, func(x *Subscription) bool { return x.Has("expired") && x.Has("traffic_exhausted") }, "两个阻断")
	t.Logf("阻断：%v", x.Blockers)
	e.WaitApplied(s.ID)
	e.MustReject(c, "两项用尽")

	section(t, "只补流量")
	e.Renew(u, OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID})
	x = e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return !x.Has("traffic_exhausted") }, "流量入账")
	if !x.Has("expired") {
		t.Fatalf("只补流量仍应因到期停用：%v", x.Blockers)
	}
	e.WaitApplied(s.ID)
	e.MustReject(c, "只补流量")

	section(t, "再耗尽、只补时长")
	e.Admin.SetRemainingBytes(s.ID, 0)
	e.Renew(u, OrderInput{Kind: "renew_time", SubscriptionID: s.ID, Months: 1})
	x = e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return !x.Has("expired") }, "时长入账")
	if !x.Has("traffic_exhausted") {
		t.Fatalf("只补时长仍应因耗尽停用：%v", x.Blockers)
	}
	e.WaitApplied(s.ID)
	e.MustReject(c, "只补时长")

	section(t, "两项补足")
	e.Renew(u, OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID})
	e.WaitUsable(u, s.ID)
	e.MustReach(c, e.Exits[0].Label)
}

// A-33 有效期内提前续时长接在原到期之后；重复核款和并发续费不重复延时；纯续时长不加流量。
func TestA33_EarlyRenewalAppendsOnce(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s, _ := e.Purchase(u, 1)
	exp0, tb0 := *s.Time.ExpiresAt, s.Traffic.TotalBytes

	r := e.Renew(u, OrderInput{Kind: "renew_time", SubscriptionID: s.ID, Months: 3})
	// 重复核款（同一请求体）不重复延时。
	in := ConfirmPaymentInput{Method: "manual_transfer", Amount: r.Amount, ExternalRef: "BANK-" + r.No}
	e.Admin.Must("POST", "/orders/"+r.ID+"/payment-confirmation", in, nil)
	x := u.Sub(s.ID)
	if d := x.Time.ExpiresAt.Sub(exp0); d != 90*24*time.Hour {
		t.Fatalf("提前续 3 个月应接在原到期后 90 天：%v", d)
	}
	if x.Traffic.TotalBytes != tb0 {
		t.Fatalf("纯续时长不应加流量：%d → %d", tb0, x.Traffic.TotalBytes)
	}
	// 并发两笔续费：各自顺延一次，共 60 天。
	exp1 := *x.Time.ExpiresAt
	var orders []Order
	for i := 0; i < 2; i++ {
		o, err := u.Order(OrderInput{Kind: "renew_time", SubscriptionID: s.ID, Months: 1}, "")
		if err != nil {
			t.Fatal(err)
		}
		orders = append(orders, o)
	}
	var wg sync.WaitGroup
	for _, o := range orders {
		for k := 0; k < 3; k++ { // 每笔再各重复确认 3 次
			wg.Add(1)
			go func(o Order) {
				defer wg.Done()
				_, _ = e.Admin.Do("POST", "/orders/"+o.ID+"/payment-confirmation",
					ConfirmPaymentInput{Method: "manual_transfer", Amount: o.Amount, ExternalRef: "BANK-" + o.No}, nil)
			}(o)
		}
	}
	wg.Wait()
	x = u.Sub(s.ID)
	if d := x.Time.ExpiresAt.Sub(exp1); d != 60*24*time.Hour {
		t.Fatalf("两笔续 1 个月应共顺延 60 天：%v", d)
	}
	if x.Traffic.TotalBytes != tb0 {
		t.Fatalf("纯续时长不应加流量：%d → %d", tb0, x.Traffic.TotalBytes)
	}
	if n := len(entriesFor(e.Admin.Ledger(s.ID), "order")); n != 2+1+2 {
		t.Fatalf("订单入账条目 %d（应为新购 2 + 续费 3）", n)
	}
	e.MustReach(CredsOf(x.Access), e.Exits[0].Label)
}

// A-34 跨 30 天边界、到期、续时长、重启都不清余额，不重复发放套餐流量。
func TestA34_BoundariesAndRestartsKeepBalance(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s, _ := e.Purchase(u, 3)
	c := CredsOf(s.Access)
	if _, _, err := Download(Connect, c, e.Target(), 3*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	used := e.SettledUsed(s.ID)
	l0 := e.Admin.Ledger(s.ID)
	check := func(why string) {
		t.Helper()
		l := e.Admin.Ledger(s.ID)
		if len(entriesFor(l, "order")) != len(entriesFor(l0, "order")) || l.EntitledBytesTotal != l0.EntitledBytesTotal {
			t.Fatalf("%s：账本流量变化：%d 条/%d → %d 条/%d", why, len(l0.Entries), l0.EntitledBytesTotal, len(l.Entries), l.EntitledBytesTotal)
		}
		a := e.Admin.Sub(s.ID)
		if a.Traffic.UsedBytes < used || a.Traffic.TotalBytes != l0.EntitledBytesTotal {
			t.Fatalf("%s：余额变化：%+v（用量应 ≥ %d）", why, a.Traffic, used)
		}
	}

	section(t, "跨 30 天边界")
	e.ShiftClock(31 * 24 * time.Hour)
	time.Sleep(time.Second) // 让调度器跑过若干轮
	check("31 天后")
	if err := u.Login(); err != nil {
		t.Fatal(err)
	}
	e.MustReach(c, e.Exits[0].Label, Connect)

	section(t, "重启控制面与节点")
	e.StopControl()
	e.StartControl()
	e.Admin.Login()
	e.RestartGost()
	e.WaitNodeOnline()
	e.WaitApplied(s.ID)
	check("重启后")
	e.MustReach(c, e.Exits[0].Label, Connect)

	section(t, "到期")
	e.ShiftClock(60 * 24 * time.Hour)
	e.WaitAdminSub(s.ID, 15*time.Second, func(a *AdminSubscription) bool { return a.Lifecycle == "retained" }, "到期")
	check("到期后")

	section(t, "续时长")
	if err := u.Login(); err != nil {
		t.Fatal(err)
	}
	e.Renew(u, OrderInput{Kind: "renew_time", SubscriptionID: s.ID, Months: 1})
	e.WaitUsable(u, s.ID)
	l := e.Admin.Ledger(s.ID)
	if l.EntitledBytesTotal != l0.EntitledBytesTotal {
		t.Fatalf("续时长重复发放了流量：%d → %d", l0.EntitledBytesTotal, l.EntitledBytesTotal)
	}
}

// A-35 控制面离线、浏览器关闭或调度延迟时，仍按数据面计时器准时停用。
func TestA35_ExpiryEnforcedWithControlPlaneDown(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	c := CredsOf(s.Access)
	exp := e.Admin.ExpireIn(s.ID, 5*time.Second)
	e.WaitApplied(s.ID)
	conns := e.idleAll(c)
	e.Control.kill9(t) // 控制面崩溃离线
	if time.Until(exp) < time.Second {
		t.Fatalf("控制面停止太慢，到期前不足 1 秒（%v）", time.Until(exp))
	}
	e.MustReach(c, e.Exits[0].Label, Connect) // 到期前仍可用
	e.closedNear(conns, exp, "控制面离线时到期")
	e.MustReject(c, "控制面离线到期后")
	e.StartControl()
	e.Admin.Login()
	x := e.WaitSub(u, s.ID, 15*time.Second, func(x *Subscription) bool { return x.Has("expired") }, "到期阻断")
	if x.Lifecycle != "retained" {
		t.Fatalf("到期后应为保留期：%s", x.Lifecycle)
	}
	e.MustReject(c, "控制面恢复后")
}

// A-44 保留期满后资源被释放，余额保留；续时长后分配新出口和新接入，余额原样可用。
func TestA44_ReleasedRenewGetsNewExitKeepsBalance(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	c := CredsOf(s.Access)
	if _, _, err := Download(Connect, c, e.Target(), 2*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	used := e.SettledUsed(s.ID)
	e.Admin.ExpireIn(s.ID, 2*time.Second)
	e.WaitAdminSub(s.ID, 15*time.Second, func(a *AdminSubscription) bool { return a.Lifecycle == "retained" }, "保留期")
	e.ShiftClock(7*24*time.Hour + time.Hour)
	a := e.WaitAdminSub(s.ID, 15*time.Second, func(a *AdminSubscription) bool { return a.Lifecycle == "released" }, "释放")
	if !a.Has("released") || a.ReleasedAt == nil {
		t.Fatalf("释放后：%v %v", a.Blockers, a.ReleasedAt)
	}
	if a.Traffic.TotalBytes != s.Traffic.TotalBytes || a.Traffic.UsedBytes < used {
		t.Fatalf("释放后余额应保留：%+v", a.Traffic)
	}
	if st := e.Admin.Upstream(e.Exits[0].ResourceID).Status; st != "cooldown" {
		t.Fatalf("释放后资源应冷却：%s", st)
	}
	e.Eventually(10*time.Second, func() bool { return Refused(c.Addr(Connect)) }, "释放后旧端口仍在监听")
	if err := u.Login(); err != nil {
		t.Fatal(err)
	}
	if x := u.Sub(s.ID); x.Access != nil {
		t.Fatalf("已释放订阅不应再展示接入：%+v", x.Access)
	}
	e.Renew(u, OrderInput{Kind: "renew_time", SubscriptionID: s.ID, Months: 1})
	x := e.WaitUsable(u, s.ID)
	if x.Access.Ports == s.Access.Ports || x.Access.Password == s.Access.Password || x.Access.Version <= s.Access.Version {
		t.Fatalf("续时长应分配新接入：%+v → %+v", s.Access, x.Access)
	}
	if got := e.CurrentExitLabel(s.ID); got != e.Exits[1].Label {
		t.Fatalf("续时长应分配新出口（得到 %s）", got)
	}
	if x.Traffic.TotalBytes != s.Traffic.TotalBytes || x.Traffic.UsedBytes < used {
		t.Fatalf("余额应原样可用：%+v", x.Traffic)
	}
	nc := CredsOf(x.Access)
	e.MustReach(nc, e.Exits[1].Label)
	if _, _, err := Download(Connect, nc, e.Target(), 1*MB, 20*time.Second); err != nil {
		t.Fatalf("续时长后余额不可用：%v", err)
	}
}
