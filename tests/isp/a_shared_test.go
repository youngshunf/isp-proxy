//go:build ispe2e

package isp

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// 共享出口套餐（D-24、docs/06 §9.1）：名额模型、分配、各自计量、换出口与隔离。

// sharedPlan 新建并上架一个共享出口套餐（与默认独享套餐同地区）。
func (e *Env) sharedPlan() AdminPlan {
	e.t.Helper()
	pv := DefaultPlanVersion()
	pv.Name = "洛杉矶共享 E2E"
	pv.Price = CNY(3900)
	p := e.Admin.CreateSharedPlan("us-lax-shared-"+randHex(2), pv)
	if p.ExitMode != "shared" {
		e.t.Fatalf("新建共享套餐：exitMode=%q", p.ExitMode)
	}
	return p
}

// catalogPlan 从用户看到的目录里找套餐。
func catalogPlan(c Catalog, id string) Plan {
	for _, p := range c.Plans {
		if p.ID == id {
			return p
		}
	}
	return Plan{}
}

// A-47 共享套餐：上限内分配、人数最少优先、并发抢最后一个名额只有一个成功、两种模式互不混用。
func TestA47_SharedSlotsAndPicking(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 3})
	shared := e.sharedPlan()
	r0, r1, solo := e.Exits[0], e.Exits[1], e.Exits[2]
	e.Admin.SetMaxTenants(solo.ResourceID, 1)
	if u := e.Admin.Upstream(r0.ResourceID); u.MaxTenants != 3 || u.Occupancy.Mode != "none" || u.Occupancy.Used != 0 {
		t.Fatalf("新录入资源默认上限 3、无人占用：%+v", u)
	}

	section(t, "独享订阅优先分到上限为 1 的资源")
	excl := e.Provision(e.NewUser(), ProvisionOpts{})
	if got := e.CurrentExitLabel(excl.ID); got != solo.Label {
		t.Fatalf("独享订阅应优先分到上限为 1 的资源 %s，得到 %s", solo.Label, got)
	}
	if excl.Plan.ExitMode != "exclusive" || excl.Exit == nil || excl.Exit.SharedCapacity != nil {
		t.Fatalf("独享订阅视图：plan=%+v exit=%+v", excl.Plan, excl.Exit)
	}

	section(t, "共享订阅按人数最少分配，不分到独享资源")
	for i := 0; i < 5; i++ {
		s := e.Provision(e.NewUser(), ProvisionOpts{PlanID: shared.ID})
		if s.Plan.ExitMode != "shared" || s.Exit == nil || s.Exit.SharedCapacity == nil || *s.Exit.SharedCapacity != 3 {
			t.Fatalf("共享订阅视图：plan=%+v exit=%+v", s.Plan, s.Exit)
		}
		label := e.CurrentExitLabel(s.ID)
		if label == solo.Label {
			t.Fatal("共享订阅分到了独享资源")
		}
		a, b := e.Admin.Upstream(r0.ResourceID), e.Admin.Upstream(r1.ResourceID)
		if d := a.Occupancy.Used - b.Occupancy.Used; d > 1 || d < -1 || a.Occupancy.Used+b.Occupancy.Used != i+1 {
			t.Fatalf("第 %d 个共享订阅后占用不均衡：%s=%d %s=%d", i+1, r0.Label, a.Occupancy.Used, r1.Label, b.Occupancy.Used)
		}
		if a.Occupancy.Mode != "shared" && a.Occupancy.Used > 0 {
			t.Fatalf("共享占用模式：%+v", a.Occupancy)
		}
		if a.Status != "available" && a.Occupancy.Used < 3 {
			t.Fatalf("未满的共享资源应保持 available：%s %+v", a.Status, a.Occupancy)
		}
	}
	// 满了的那个显示为 bound，详情列出 3 个受影响订阅。
	full := r0
	if e.Admin.Upstream(r1.ResourceID).Occupancy.Used == 3 {
		full = r1
	}
	fu := e.Admin.Upstream(full.ResourceID)
	if fu.Status != "bound" || len(fu.Tenants) != 3 {
		t.Fatalf("满员共享资源应显示 bound 并列出 3 个订阅：%s %d", fu.Status, len(fu.Tenants))
	}
	for _, tn := range fu.Tenants {
		if tn.ExitMode != "shared" {
			t.Fatalf("受影响订阅模式：%+v", tn)
		}
	}

	section(t, "独享套餐不分到有共享订阅的资源")
	c := e.NewUser().Catalog()
	if st := catalogPlan(c, e.Plan.ID).Stock; st != "out" {
		t.Fatalf("独享套餐库存应为无货（剩余资源都有共享订阅）：%s", st)
	}
	if p := catalogPlan(c, shared.ID); p.ExitMode != "shared" || p.Stock == "out" {
		t.Fatalf("共享套餐还剩 1 个名额：%+v", p)
	}
	if _, err := e.NewUser().Order(OrderInput{Kind: "new", PlanID: e.Plan.ID, Months: 1, Quantity: 1}, ""); StatusOf(err) != 409 {
		t.Fatalf("独享套餐下单应无货 409：%v", err)
	}

	section(t, "并发抢最后一个名额只有一个成功")
	var users []*User
	for i := 0; i < 5; i++ {
		users = append(users, e.NewUser())
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var won []Order
	var errs []error
	for _, u := range users {
		wg.Add(1)
		go func(u *User) {
			defer wg.Done()
			o, err := u.Order(OrderInput{Kind: "new", PlanID: shared.ID, Months: 1, Quantity: 1}, "")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won = append(won, o)
			} else {
				errs = append(errs, err)
			}
		}(u)
	}
	wg.Wait()
	if len(won) != 1 {
		t.Fatalf("应只有一个订单拿到最后一个名额：成功 %d，失败 %v", len(won), errs)
	}
	for _, err := range errs {
		if StatusOf(err) != 409 {
			t.Fatalf("其余订单应无货 409：%v", err)
		}
	}
	if st := catalogPlan(e.NewUser().Catalog(), shared.ID).Stock; st != "out" {
		t.Fatalf("名额被订单预留后共享库存应为无货：%s", st)
	}
	// 确认收款后订阅分到预留的名额。
	e.Admin.Confirm(won[0].ID)
	var sid string
	e.Eventually(20*time.Second, func() bool {
		o := e.Admin.Order(won[0].ID)
		if o.SubscriptionID != nil {
			sid = *o.SubscriptionID
			return true
		}
		return false
	}, "订单确认后没有关联订阅")
	e.WaitApplied(sid)
	if a, b := e.Admin.Upstream(r0.ResourceID), e.Admin.Upstream(r1.ResourceID); a.Occupancy.Used != 3 || b.Occupancy.Used != 3 ||
		a.Occupancy.Bound != 3 || b.Occupancy.Bound != 3 {
		t.Fatalf("两个共享资源都应满员：%+v %+v", a.Occupancy, b.Occupancy)
	}
}

// A-48 同一共享出口上的两个订阅各自计量、各自耗尽、各自换接入。
func TestA48_SharedTenantsIndependent(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	shared := e.sharedPlan()
	x := e.Exits[0]
	ua, ub := e.NewUser(), e.NewUser()
	a := e.Provision(ua, ProvisionOpts{PlanID: shared.ID})
	b := e.Provision(ub, ProvisionOpts{PlanID: shared.ID})
	ca, cb := CredsOf(a.Access), CredsOf(b.Access)

	section(t, "两者经同一出口到达目标")
	e.MustReach(ca, x.Label)
	e.MustReach(cb, x.Label)
	if a.Exit.IP != b.Exit.IP || a.Access.Ports == b.Access.Ports || a.Access.Username == b.Access.Username {
		t.Fatalf("同出口、不同接入：%+v %+v", a.Access, b.Access)
	}

	section(t, "用户视图只显示共享与上限，不暴露同出口的其他订阅与人数")
	var raw json.RawMessage
	ua.Get("/subscriptions/"+a.ID, &raw)
	for _, leak := range []string{b.ID, b.Access.Username, ub.Email, `"occupancy"`, `"tenants"`, `"maxTenants"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("用户视图泄露了 %q", leak)
		}
	}
	if av := ua.Sub(a.ID); av.Plan.ExitMode != "shared" || av.Exit.SharedCapacity == nil || *av.Exit.SharedCapacity != 3 {
		t.Fatalf("用户视图：plan=%+v exit=%+v", av.Plan, av.Exit)
	}

	section(t, "各自计量")
	if _, _, err := Download(Connect, ca, e.Target(), 2*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	usedA := e.SettledUsed(a.ID)
	usedB := e.SettledUsed(b.ID)
	if usedA < 2*MB || usedB >= MB {
		t.Fatalf("A 下载 2 MB 只应计入 A：A=%d B=%d", usedA, usedB)
	}
	if _, _, err := Download(Connect, cb, e.Target(), 1*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	usedB = e.SettledUsed(b.ID)
	if usedB < MB || e.SettledUsed(a.ID) != usedA {
		t.Fatalf("B 下载 1 MB 只应计入 B：A=%d→%d B=%d", usedA, e.Admin.Sub(a.ID).Traffic.UsedBytes, usedB)
	}

	section(t, "B 换接入不影响 A")
	verA := e.Admin.Sub(a.ID).DesiredVersion
	nb := e.userChangeAccess(ub, b.ID, "ports_and_password")
	if e.Admin.Sub(a.ID).DesiredVersion != verA {
		t.Fatal("B 换接入不应改动 A 的期望版本")
	}
	e.MustReach(ca, x.Label)
	cb = CredsOf(&nb)
	e.MustReach(cb, x.Label)
	usedA = e.SettledUsed(a.ID)
	usedB = e.SettledUsed(b.ID)

	section(t, "A 耗尽，B 继续可用且用量不变")
	e.Admin.SetRemainingBytes(a.ID, 0)
	e.WaitSub(ua, a.ID, 10*time.Second, func(s *Subscription) bool { return s.Has("traffic_exhausted") }, "A 耗尽")
	e.WaitApplied(a.ID)
	e.MustReject(ca, "A 耗尽")
	if got := e.SettledUsed(b.ID); got != usedB {
		t.Fatalf("A 耗尽不应改变 B 的用量：%d → %d", usedB, got)
	}
	if s := ub.Sub(b.ID); len(s.Blockers) != 0 || s.Lifecycle != "active" {
		t.Fatalf("B 应仍可用：%s %v", s.Lifecycle, s.Blockers)
	}
	e.MustReach(cb, x.Label)
	if _, _, err := Download(Connect, cb, e.Target(), 1*MB, 20*time.Second); err != nil {
		t.Fatalf("A 耗尽后 B 下载：%v", err)
	}
	if got := e.SettledUsed(a.ID); got != usedA {
		t.Fatalf("B 的流量不应计入耗尽的 A：%d → %d", usedA, got)
	}
}

// A-49 共享订阅换出口：只能换到有空位的共享资源；原资源其他订阅不受影响；腾出的名额冷却；隔离时逐订阅告警。
func TestA49_SharedExitChangeCooldownQuarantine(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 4})
	shared := e.sharedPlan()
	r0, r1, solo, r3 := e.Exits[0], e.Exits[1], e.Exits[2], e.Exits[3]
	e.Admin.SetMaxTenants(r0.ResourceID, 2)
	e.Admin.SetMaxTenants(solo.ResourceID, 1)
	ua, ub := e.NewUser(), e.NewUser()
	a := e.Provision(ua, ProvisionOpts{PlanID: shared.ID, ResourceID: r0.ResourceID})
	b := e.Provision(ub, ProvisionOpts{PlanID: shared.ID, ResourceID: r0.ResourceID})
	excl := e.Provision(e.NewUser(), ProvisionOpts{ResourceID: solo.ResourceID})
	ca, cb := CredsOf(a.Access), CredsOf(b.Access)

	section(t, "不能换到独享出口")
	if _, err := e.Admin.CompareCandidate(a.ID, solo.ResourceID); StatusOf(err) != 409 {
		t.Fatalf("共享订阅候选独享出口应 409：%v", err)
	}
	if _, err := e.Admin.CompareCandidate(excl.ID, r0.ResourceID); StatusOf(err) != 409 {
		t.Fatalf("独享订阅候选共享资源应 409：%v", err)
	}

	section(t, "换到另一个共享资源，原资源上的订阅不受影响")
	verB := e.Admin.Sub(b.ID).DesiredVersion
	e.Admin.ChangeExit(a.ID, r1.ResourceID, false)
	e.MustReach(ca, r1.Label)
	e.MustReach(cb, r0.Label)
	if e.Admin.Sub(b.ID).DesiredVersion != verB {
		t.Fatal("换出口不应改动同资源其他订阅的期望版本")
	}
	if u := e.Admin.Upstream(r0.ResourceID); u.Occupancy.Bound != 1 || u.Occupancy.Cooldown != 1 || u.Status != "bound" {
		t.Fatalf("原资源应为 1 绑定 + 1 冷却（满）：%s %+v", u.Status, u.Occupancy)
	}

	section(t, "冷却名额不再分配")
	uc := e.NewUser()
	res, err := e.AdminProvision(uc, ProvisionOpts{PlanID: shared.ID, ResourceID: r0.ResourceID})
	if err == nil {
		e.WaitAdminSub(res.Subscription.ID, 10*time.Second, func(s *AdminSubscription) bool { return s.Lifecycle == "fulfill_failed" }, "指定冷却中的资源应履约失败")
	} else if StatusOf(err) != 409 && StatusOf(err) != 412 {
		t.Fatalf("指定冷却中的资源开通：%v", err)
	}
	c := e.Provision(e.NewUser(), ProvisionOpts{PlanID: shared.ID})
	if got := e.CurrentExitLabel(c.ID); got != r3.Label {
		t.Fatalf("新共享订阅应分到人数最少的 %s，得到 %s", r3.Label, got)
	}

	section(t, "冷却期满后名额回来")
	e.ShiftClock(7*24*time.Hour + time.Hour)
	e.Eventually(15*time.Second, func() bool {
		u := e.Admin.Upstream(r0.ResourceID)
		return u.Occupancy.Cooldown == 0 && u.Occupancy.Used == 1 && u.Status == "available"
	}, "原资源冷却名额未释放：%+v", e.Admin.Upstream(r0.ResourceID).Occupancy)
	if err := uc.Login(); err != nil {
		t.Fatal(err)
	}
	d := e.Provision(uc, ProvisionOpts{PlanID: shared.ID, ResourceID: r0.ResourceID})
	if got := e.CurrentExitLabel(d.ID); got != r0.Label {
		t.Fatalf("冷却期满后应能分到 %s，得到 %s", r0.Label, got)
	}

	section(t, "隔离共享资源：每个订阅各自告警，后台列出受影响订阅")
	// r1 上有 a，r3 上有 c；把 c 换到 r1 让 r1 上有两个订阅。
	e.Admin.ChangeExit(c.ID, r1.ResourceID, false)
	e.Admin.SetUpstreamStatus(r1.ResourceID, "quarantine", "供应商通知线路故障")
	u := e.Admin.Upstream(r1.ResourceID)
	got := map[string]bool{}
	for _, tn := range u.Tenants {
		got[tn.SubscriptionID] = true
	}
	if u.Status != "quarantine" || len(u.Tenants) != 2 || !got[a.ID] || !got[c.ID] {
		t.Fatalf("隔离后受影响订阅：%s %+v", u.Status, u.Tenants)
	}
	alertsFor := func() map[string]bool {
		m := map[string]bool{}
		for _, al := range e.Admin.Alerts(url.Values{"status": {"open"}}) {
			if deref(al.ResourceID) == r1.ResourceID && al.SubscriptionID != nil {
				m[*al.SubscriptionID] = true
			}
		}
		return m
	}
	if m := alertsFor(); len(m) != 2 || !m[a.ID] || !m[c.ID] {
		t.Fatalf("应为 a、c 各自告警：%v", m)
	}
	// 共享名额满时选出口：a 换到 r3（c 已离开，空），a 的告警关闭，c 的仍在。
	e.Admin.ChangeExit(a.ID, r3.ResourceID, false)
	e.MustReach(ca, r3.Label)
	if m := alertsFor(); len(m) != 1 || !m[c.ID] {
		t.Fatalf("a 换走后只剩 c 的告警：%v", m)
	}
}
