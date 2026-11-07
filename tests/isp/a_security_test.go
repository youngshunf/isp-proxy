//go:build ispe2e

package isp

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// 故障、隔离、停用时效、持久化、会话、用量与出站限制：A-14、A-16、A-17、A-19、A-20、A-25、A-37、A-39、A-46。

// A-14 上游故障时客户明确失败，目标侧观察不到中转服务器或其他出口。
func TestA14_UpstreamDownFailsExplicitly(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 3})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	c := CredsOf(s.Access)
	hits := make([]int64, len(e.Exits))
	for i, x := range e.Exits {
		hits[i] = x.Backend.IPHits.Load()
	}
	e.Exits[0].Up.Close()
	for _, m := range AllModes {
		start := time.Now()
		body, err := e.Reach(m, c)
		if err == nil {
			t.Fatalf("%s：上游故障时仍得到 %q", m, body)
		}
		if !IsRejected(err) {
			t.Fatalf("%s：上游故障应明确失败（代理层错误响应），得到 %v", m, err)
		}
		if d := time.Since(start); d > 15*time.Second {
			t.Fatalf("%s：失败用时 %v（应明确且及时）", m, d)
		}
		t.Logf("%s：%v", m, err)
	}
	if e.CanaryHits() != 0 {
		t.Fatalf("上游故障后目标被直连 %d 次（暴露中转服务器）", e.CanaryHits())
	}
	for i, x := range e.Exits[1:] {
		if got := x.Backend.IPHits.Load(); got != hits[i+1] {
			t.Fatalf("上游故障后流量经其他出口 %s 到达目标", x.Label)
		}
	}
	if got := e.SettledUsed(s.ID); got > 64*KB {
		t.Fatalf("失败的请求被大量计量：%d", got)
	}
}

// A-16 客户 A 猜测 B 的订阅、接入、连接、订单 ID，服务端均拒绝且无数据泄露。
func TestA16_CrossUserIDsRejected(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	a, b := e.NewUser(), e.NewUser()
	bs := e.Provision(b, ProvisionOpts{Name: "B-SECRET-NAME"})
	bo, err := b.Order(OrderInput{Kind: "renew_time", SubscriptionID: bs.ID, Months: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	var bReq ExitRequest
	b.Must("POST", "/exit-requests", map[string]any{"subscriptionId": bs.ID, "reason": "quality", "note": "B-NOTE", "alsoChangeAccess": false},
		&bReq, Idem(NewKey()))
	leaks := []string{bs.ID[4:], "B-SECRET-NAME", bs.Access.Username, bs.Access.Password, b.Email, bo.No, "B-NOTE"}
	type probe struct {
		method, path string
		body         any
	}
	probes := []probe{
		{"GET", "/subscriptions/" + bs.ID, nil},
		{"PATCH", "/subscriptions/" + bs.ID, map[string]string{"name": "pwned"}},
		{"GET", "/subscriptions/" + bs.ID + "/events", nil},
		{"POST", "/subscriptions/" + bs.ID + "/access-changes", map[string]string{"mode": "password"}},
		{"POST", "/subscriptions/" + bs.ID + "/access/ack", nil},
		{"GET", "/orders/" + bo.ID, nil},
		{"POST", "/orders/" + bo.ID + "/cancel", nil},
		{"POST", "/orders/" + bo.ID + "/claim-paid", map[string]string{"note": "x"}},
		{"POST", "/orders", OrderInput{Kind: "renew_time", SubscriptionID: bs.ID, Months: 1}},
		{"POST", "/orders", OrderInput{Kind: "add_traffic", SubscriptionID: bs.ID, PackID: e.Pack.ID}},
		{"POST", "/exit-requests", map[string]any{"subscriptionId": bs.ID, "reason": "quality", "note": "", "alsoChangeAccess": false}},
	}
	for _, p := range probes {
		_, err := a.Do(p.method, p.path, p.body, nil, Idem(NewKey()))
		if StatusOf(err) != 404 {
			t.Fatalf("A 访问 B 的 %s %s 应 404，得到 %v", p.method, p.path, err)
		}
		if ae, ok := err.(*APIError); ok && containsAny(ae.Body, leaks...) != "" {
			t.Fatalf("%s %s 的 404 响应泄露了 B 的数据（%s）：%s", p.method, p.path, containsAny(ae.Body, leaks...), ae.Body)
		}
	}
	// A 的列表接口里没有 B 的任何东西。
	for _, path := range []string{"/subscriptions", "/orders", "/exit-requests"} {
		var raw json.RawMessage
		a.Get(path, &raw)
		if containsAny(string(raw), leaks...) != "" {
			t.Fatalf("A 的 %s 包含 B 的数据：%s", path, truncate(string(raw), 300))
		}
	}
	// B 的订阅与订单未被改动。
	if got := b.Sub(bs.ID); got.Name != "B-SECRET-NAME" || got.Access.Password != bs.Access.Password {
		t.Fatalf("B 的订阅被改动：%+v", got)
	}
	var o Order
	b.Get("/orders/"+bo.ID, &o)
	if o.Payment != "awaiting" {
		t.Fatalf("B 的订单被改动：%s", o.Payment)
	}
	// 接入凭据也只对本订阅有效：A 用 B 的用户名 + A 的密码访问 A 的端口被拒。
	as := e.Provision(a, ProvisionOpts{})
	ac := CredsOf(as.Access)
	e.MustReject(Creds{Host: ac.Host, Ports: ac.Ports, User: bs.Access.Username, Pass: bs.Access.Password}, "B 的凭据访问 A 的端口")
}

// A-17 开通遇端口冲突时可恢复或清理，不显示就绪，不影响其他订阅。
func TestA17_PortConflictRecoverable(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2, PortRange: 6})
	u := e.NewUser()
	s1 := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	c1 := CredsOf(s1.Access)
	used := map[int]bool{c1.Ports.HTTP: true, c1.Ports.HTTPS: true, c1.Ports.SOCKS5: true}
	var hold []net.Listener
	for p := e.Ports[0]; p <= e.Ports[1]; p++ {
		if !used[p] {
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
			if err != nil {
				t.Fatalf("占用端口 %d：%v", p, err)
			}
			hold = append(hold, ln)
		}
	}
	defer func() {
		for _, ln := range hold {
			ln.Close()
		}
	}()
	res, err := e.AdminProvision(u, ProvisionOpts{ResourceID: e.Exits[1].ResourceID})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Subscription.ID
	as := e.WaitAdminSub(id, 30*time.Second, func(a *AdminSubscription) bool {
		return a.Lifecycle == "fulfill_failed" || (a.ActiveTaskID == nil && len(a.Blockers) > 0 && a.Lifecycle != "active")
	}, "端口冲突导致开通失败")
	t.Logf("开通失败后：lifecycle=%s blockers=%v", as.Lifecycle, as.Blockers)
	us := u.Sub(id)
	if len(us.Blockers) == 0 || us.Lifecycle == "active" {
		t.Fatalf("端口冲突时不应显示就绪：%+v", us)
	}
	if res.Task != nil {
		if tk := e.Admin.Task(res.Task.ID); tk.State != "failed" || deref(tk.FailureCode) != "port_in_use" {
			t.Logf("开通任务：%+v %s", tk, deref(tk.FailureCode))
		}
	}
	// 其他订阅不受影响。
	e.MustReach(c1, e.Exits[0].Label)
	// 释放冲突端口后重试。
	for _, ln := range hold {
		ln.Close()
	}
	hold = nil
	var tk Task
	e.Admin.Must("POST", "/subscriptions/"+id+"/fulfillment", map[string]string{}, &tk)
	e.WaitUsable(u, id)
	e.MustReach(CredsOf(u.Sub(id).Access), e.Exits[1].Label)
	e.MustReach(c1, e.Exits[0].Label)
	if subs := u.Subs(); len(subs) != 2 {
		t.Fatalf("重试后订阅数 %d", len(subs))
	}
	if n := len(e.Admin.Ledger(id).Entries); n != 2 {
		t.Fatalf("重试后入账 %d 条（应为 2）", n)
	}
	t.Log("证书错误、部分下发失败：由 internal/agent 与 internal/control/core 的进程内测试覆盖（本场景只测端口冲突）")
}

// A-19 暂停、封禁、重置密码后，旧授权连接 ≤ 1 秒关闭；冷却期后复用端口，旧凭据不能访问新订阅。
func TestA19_RevocationClosesWithin1sAndPortReuse(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2, PortRange: 3})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	c := CredsOf(s.Access)
	measure := func(why string, act func()) {
		t.Helper()
		var conns []*PConn
		for _, m := range TunnelModes {
			p, err := Open(m, c, e.Target())
			if err != nil {
				t.Fatalf("%s：%s：%v", why, m, err)
			}
			defer p.Close()
			conns = append(conns, p)
		}
		act()
		at := time.Now()
		for i, p := range conns {
			d, ok := WaitClosed(p, 5*time.Second)
			if !ok {
				t.Fatalf("%s：%s 存量连接 5 秒内未关闭", why, TunnelModes[i])
			}
			if total := time.Since(at); total > maxStopDelay {
				t.Fatalf("%s：%s 存量连接在操作返回后 %v 才关闭（> 1 秒）", why, TunnelModes[i], total)
			}
			_ = d
		}
		t.Logf("%s：全部存量连接在操作返回后 %v 内关闭", why, time.Since(at).Round(time.Millisecond))
	}

	measure("暂停", func() {
		e.Admin.Must("POST", "/subscriptions/"+s.ID+"/pause", map[string]string{"reason": "A-19"}, nil)
	})
	e.MustReject(c, "暂停后")
	e.Admin.Must("POST", "/subscriptions/"+s.ID+"/resume", nil, nil)
	e.WaitUsable(u, s.ID)

	measure("封禁", func() {
		e.Admin.Must("POST", "/users/"+u.ID+"/ban", map[string]string{"reason": "A-19"}, nil)
	})
	e.MustReject(c, "封禁后")
	e.Admin.Must("POST", "/users/"+u.ID+"/unban", nil, nil)
	if err := u.Login(); err != nil {
		t.Fatal(err)
	}
	e.WaitUsable(u, s.ID)

	var acc Access
	measure("重置密码", func() {
		var err error
		if acc, err = u.ChangeAccess(s.ID, "password"); err != nil {
			t.Fatal(err)
		}
	})
	e.MustReject(c, "旧密码")
	c = CredsOf(&acc)
	e.WaitApplied(s.ID)
	e.MustReach(c, e.Exits[0].Label)

	section(t, "冷却期后复用端口")
	e.Admin.Must("POST", "/subscriptions/"+s.ID+"/deletion", map[string]any{"reason": "A-19", "acknowledgeRemainingTraffic": true}, nil)
	e.Eventually(10*time.Second, func() bool { return Refused(c.Addr(Connect)) }, "删除后端口仍在监听")
	v := e.NewUser()
	// 冷却期内：端口池（只有 3 个端口）没有可用端口，开通不能成功。
	r, err := e.AdminProvision(v, ProvisionOpts{ResourceID: e.Exits[1].ResourceID})
	if err != nil {
		t.Logf("冷却期内开通被拒：%v", err)
	} else {
		as := e.WaitAdminSub(r.Subscription.ID, 20*time.Second, func(a *AdminSubscription) bool {
			return a.Lifecycle == "fulfill_failed" || a.Access != nil
		}, "冷却期内开通结果")
		if as.Access != nil && as.Lifecycle == "active" {
			t.Fatalf("冷却期内复用了端口：%+v", as.Access)
		}
		t.Logf("冷却期内开通：lifecycle=%s blockers=%v", as.Lifecycle, as.Blockers)
	}
	e.ShiftClock(7*24*time.Hour + time.Hour)
	var s2 Subscription
	if err != nil {
		s2 = e.Provision(v, ProvisionOpts{ResourceID: e.Exits[1].ResourceID})
	} else {
		e.Admin.Must("POST", "/subscriptions/"+r.Subscription.ID+"/fulfillment", map[string]string{"resourceId": e.Exits[1].ResourceID}, nil)
		if err := v.Login(); err != nil {
			t.Fatal(err)
		}
		s2 = e.WaitUsable(v, r.Subscription.ID)
	}
	if s2.Access.Ports != c.Ports && (s2.Access.Ports.HTTP < e.Ports[0] || s2.Access.Ports.HTTP > e.Ports[1]) {
		t.Fatalf("新订阅端口不在池内：%+v", s2.Access.Ports)
	}
	nc := CredsOf(s2.Access)
	e.MustReach(nc, e.Exits[1].Label)
	e.MustReject(Creds{Host: nc.Host, Ports: nc.Ports, User: c.User, Pass: c.Pass}, "旧凭据访问复用端口的新订阅")
	e.MustReject(Creds{Host: nc.Host, Ports: nc.Ports, User: nc.User, Pass: c.Pass}, "旧密码访问复用端口的新订阅")
}

// A-20 本地持久化失败时停止授权并告警；控制面离线时节点按最后已知额度和期限继续执行停用。
func TestA20_PersistFailureAndOfflineEnforcement(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	c := CredsOf(s.Access)

	section(t, "本地持久化失败")
	idle, err := Open(Connect, c, e.Target())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	if err := os.Chmod(e.DataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			os.Chmod(e.DataDir, 0o700)
		}
	}
	defer restore()
	if _, ok := WaitClosed(idle, 5*time.Second); !ok {
		t.Fatal("持久化失败后存量连接未关闭")
	}
	e.MustReject(c, "持久化失败")
	e.Eventually(15*time.Second, func() bool {
		for _, a := range e.Admin.Alerts(url.Values{"kind": {"persist_failed"}}) {
			if deref(a.NodeID) == e.NodeID && a.Status != "resolved" {
				return true
			}
		}
		return false
	}, "持久化失败未告警")
	restore()
	e.MustReach(c, e.Exits[0].Label)
	e.Eventually(15*time.Second, func() bool {
		for _, a := range e.Admin.Alerts(url.Values{"kind": {"persist_failed"}}) {
			if deref(a.NodeID) == e.NodeID && a.Status != "resolved" {
				return false
			}
		}
		return true
	}, "持久化恢复后告警未解除")

	section(t, "控制面离线时按最后已知额度停用")
	e.Admin.SetRemainingBytes(s.ID, 2*MB)
	e.WaitApplied(s.ID)
	e.Control.kill9(t)
	var recv atomic.Int64
	err = Stream(Connect, c, e.Target(), 10*MB, &recv, 20*time.Second)
	if err == nil {
		t.Fatal("控制面离线时 10 MB 下载在 2 MB 额度下完成了")
	}
	if over := recv.Load() - 2*MB; over > maxOvershoot {
		t.Fatalf("控制面离线时超额 %d 字节", over)
	}
	e.MustReject(c, "控制面离线耗尽后")
	e.StartControl()
	e.Admin.Login()
	e.WaitSub(u, s.ID, 15*time.Second, func(x *Subscription) bool { return x.Has("traffic_exhausted") }, "恢复后显示耗尽")
	t.Log("控制面离线时按期限停用见 TestA35")
}

// A-25 修改登录密码后旧会话失效，接入不变；换接入不影响登录密码。
func TestA25_PasswordChangeVsAccessChange(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	other := e.NewClientUser(u.Email, u.Password) // 第二个浏览器
	if err := other.Login(); err != nil {
		t.Fatal(err)
	}
	other.Must("GET", "/me", nil, nil)
	newPass := "changed-pass-" + randHex(4)
	if _, err := u.Do("POST", "/account/change-password", map[string]string{"current": "wrong-" + randHex(3), "next": newPass}, nil); err == nil {
		t.Fatal("当前密码错误时修改成功了")
	}
	u.Must("POST", "/account/change-password", map[string]string{"current": u.Password, "next": newPass}, nil)
	if _, err := other.Do("GET", "/me", nil, nil); StatusOf(err) != 401 {
		t.Fatalf("改密后其他会话应失效（401）：%v", err)
	}
	if _, err := u.Do("GET", "/me", nil, nil); err != nil {
		t.Fatalf("当前会话应换发新令牌继续有效：%v", err)
	}
	old := u.Password
	u.Password = newPass
	if err := (&User{Client: other.Client, e: e, Email: u.Email, Password: old}).Login(); err == nil {
		t.Fatal("旧登录密码仍能登录")
	}
	if err := u.Login(); err != nil {
		t.Fatalf("新登录密码登录：%v", err)
	}
	got := u.Sub(s.ID)
	if got.Access.Password != s.Access.Password || got.Access.Version != s.Access.Version {
		t.Fatalf("改登录密码改变了接入：%+v → %+v", s.Access, got.Access)
	}
	e.MustReach(CredsOf(got.Access), e.CurrentExitLabel(s.ID))
	if got.Access.Password == newPass || s.Access.Password == old {
		t.Fatal("接入密码与登录密码不应相同")
	}

	section(t, "换接入不影响登录密码")
	if _, err := u.ChangeAccess(s.ID, "ports_and_password"); err != nil {
		t.Fatal(err)
	}
	e.WaitApplied(s.ID)
	fresh := e.NewClientUser(u.Email, newPass)
	if err := fresh.Login(); err != nil {
		t.Fatalf("换接入后登录密码失效：%v", err)
	}
	if _, err := u.Do("GET", "/me", nil, nil); err != nil {
		t.Fatalf("换接入使会话失效：%v", err)
	}
}

// A-37 指标断联、空数据、零值、过期可区分；客户只见本人用量，图表不替代账本。
func TestA37_UsageFreshnessNullVsZero(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	busy := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	// 安静订阅：不经入口发任何流量（不调用 WaitUsable）。
	r, err := e.AdminProvision(u, ProvisionOpts{ResourceID: e.Exits[1].ResourceID})
	if err != nil {
		t.Fatal(err)
	}
	quiet := r.Subscription.ID
	e.WaitAdminSub(quiet, 30*time.Second, func(a *AdminSubscription) bool { return a.Lifecycle == "active" && a.UsageReportedAt != nil }, "安静订阅上报")
	if _, _, err := Download(Connect, CredsOf(busy.Access), e.Target(), 2*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	e.SettledUsed(busy.ID)
	time.Sleep(time.Second)

	series := func(id string, from, to time.Time) (UsageSeries, map[string]any) {
		t.Helper()
		q := url.Values{"from": {from.UTC().Format(time.RFC3339)}, "to": {to.UTC().Format(time.RFC3339)}, "bucket": {"5m"}}
		var us UsageSeries
		var raw map[string]any
		e.Admin.Get("/subscriptions/"+id+"/usage?"+q.Encode(), &us)
		e.Admin.Get("/subscriptions/"+id+"/usage?"+q.Encode(), &raw)
		return us, raw
	}
	now := e.Now()
	from, to := now.Add(-time.Hour), now.Add(5*time.Minute)

	section(t, "空数据：订阅创建前的区间")
	us, _ := series(busy.ID, now.Add(-48*time.Hour), now.Add(-47*time.Hour))
	if us.Freshness != "no_data" {
		t.Fatalf("没有样本的区间应 no_data：%s", us.Freshness)
	}
	for _, p := range us.Points {
		if p.Bytes != nil || p.Samples != 0 {
			t.Fatalf("没有样本的桶应为 null：%+v", p)
		}
	}

	section(t, "有流量：fresh、非零")
	us, raw := series(busy.ID, from, to)
	if us.Freshness != "fresh" || us.LastSampleAt == nil {
		t.Fatalf("刚上报应 fresh：%s %v", us.Freshness, us.LastSampleAt)
	}
	if raw["source"] != "agent_report" || raw["unit"] != "bytes" {
		t.Fatalf("趋势来源应标为节点上报：%v %v", raw["source"], raw["unit"])
	}
	var total int64
	var nulls int
	for _, p := range us.Points {
		if p.Bytes == nil {
			nulls++
			continue
		}
		total += *p.Bytes
	}
	if total < 2*MB || nulls == 0 {
		t.Fatalf("趋势：合计 %d（应 ≥ 2 MB），null 桶 %d（创建前的桶应为 null）", total, nulls)
	}

	section(t, "真实零值")
	us, _ = series(quiet, from, to)
	var zero bool
	for _, p := range us.Points {
		if p.Samples > 0 && p.Bytes != nil && *p.Bytes == 0 {
			zero = true
		}
		if p.Samples > 0 && p.Bytes != nil && *p.Bytes > 4*KB {
			t.Fatalf("安静订阅出现用量：%+v", p)
		}
	}
	if !zero || us.Freshness != "fresh" {
		t.Fatalf("安静订阅应有样本且用量为 0（不是 null）：%s %+v", us.Freshness, us.Points)
	}

	section(t, "客户只见本人用量；图表不替代账本")
	uc := newClient(t, e.Base, "/admin-api")
	uc.hc.Jar = u.hc.Jar
	if _, err := uc.Do("GET", "/subscriptions/"+busy.ID+"/usage?"+url.Values{"from": {from.UTC().Format(time.RFC3339)},
		"to": {to.UTC().Format(time.RFC3339)}}.Encode(), nil, nil); StatusOf(err) != 401 {
		t.Fatalf("用户会话访问管理端用量应 401：%v", err)
	}
	other := e.NewUser()
	if _, err := other.Do("GET", "/subscriptions/"+busy.ID, nil, nil); StatusOf(err) != 404 {
		t.Fatalf("他人订阅（含用量）应 404：%v", err)
	}
	l := e.Admin.Ledger(busy.ID)
	if l.EntitledBytesTotal != u.Sub(busy.ID).Traffic.TotalBytes {
		t.Fatalf("订阅总额应来自账本：%d vs %d", l.EntitledBytesTotal, u.Sub(busy.ID).Traffic.TotalBytes)
	}

	section(t, "断联：节点停止后变为 stale")
	e.Gost.kill9(t)
	e.Eventually(30*time.Second, func() bool { us, _ = series(busy.ID, from, to); return us.Freshness == "stale" }, "节点断联后趋势未变为 stale（%s）", us.Freshness)
	if us.LastSampleAt == nil {
		t.Fatal("stale 时应给出最后样本时间")
	}
	e.RestartGost()
	e.WaitNodeOnline()
}

// A-39 手机上完成注册、选购、复制连接、换接入、续购；键盘可操作主要表单。
func TestA39_MobileAndKeyboard(t *testing.T) {
	t.Skip("A-39 是浏览器/移动端交互验收（视口、键盘焦点、复制按钮），不属于本 Go 平台 E2E；由 web/ 的 Playwright 与人工验收覆盖")
}

// A-46 出站 25 端口、私网、云元数据地址默认被拒；套餐的并发连接和带宽上限生效。
func TestA46_EgressPolicyConcurrencyBandwidth(t *testing.T) {
	t.Parallel()
	const mbps = 8 // 1 MB/s
	e := NewEnv(t, EnvOptions{Exits: 1, Plan: func(p *PlanVersionInput) { p.Concurrency, p.BandwidthMbps = 2, mbps }})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	c := CredsOf(s.Access)
	x := e.Exits[0]

	section(t, "出站目标限制")
	for _, target := range []string{"127.0.0.1:25", "10.0.0.1:80", "192.168.1.1:80", "172.16.0.1:80", "169.254.169.254:80", "[fd00::1]:80"} {
		for _, m := range AllModes {
			before := x.Backend.IPHits.Load()
			body, _, err := Fetch(m, c, target, "/ip")
			if err == nil {
				t.Fatalf("%s 到 %s 应被拒，却得到 %q", m, target, body)
			}
			if !IsRejected(err) {
				t.Fatalf("%s 到 %s 应明确拒绝：%v", m, target, err)
			}
			if x.Backend.IPHits.Load() != before {
				t.Fatalf("%s 到 %s 的请求被转发到了上游", m, target)
			}
		}
	}
	e.MustReach(c, x.Label)

	section(t, "并发连接上限 2")
	var open []*PConn
	for i := 0; i < 2; i++ {
		p, err := Open(Connect, c, e.Target())
		if err != nil {
			t.Fatalf("第 %d 条连接：%v", i+1, err)
		}
		open = append(open, p)
	}
	if p, err := Open(Connect, c, e.Target()); err == nil {
		if _, gerr := p.Get("/ip"); gerr == nil {
			p.Close()
			t.Fatal("第 3 条并发连接被放行")
		}
		p.Close()
	}
	for _, p := range open {
		p.Close()
	}
	time.Sleep(200 * time.Millisecond)
	e.MustReach(c, x.Label, Connect)

	section(t, "带宽上限")
	start := time.Now()
	got, _, err := Download(Connect, c, e.Target(), 3*MB, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	rate := float64(got) / el.Seconds()
	t.Logf("下载 %d 字节用时 %v（%.0f B/s，上限 %d Mbps）", got, el.Round(time.Millisecond), rate, mbps)
	if limit := float64(mbps) * 1e6 / 8; rate > limit*1.3 {
		t.Fatalf("速率 %.0f B/s 超过带宽上限 %.0f B/s 的 1.3 倍", rate, limit)
	}
}
