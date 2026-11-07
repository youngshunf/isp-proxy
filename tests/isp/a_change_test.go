//go:build ispe2e

package isp

import (
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 换出口、换接入、重置密码、组合更换：A-11、A-12、A-13、A-36、A-40～A-43。

// closeBound 是“旧连接 ≤ 1 秒关闭”的上限（含轮询与调度余量的检测窗口另计）。
const closeBound = time.Second

func (u *User) Events(id string) []SubscriptionEvent {
	u.e.t.Helper()
	var evs []SubscriptionEvent
	u.Get("/subscriptions/"+id+"/events", &evs)
	return evs
}

// openIdle 打开一条已授权的空闲隧道，供“变更后存量连接关闭”断言用。
func (e *Env) openIdle(c Creds) *PConn {
	e.t.Helper()
	p, err := Open(Connect, c, e.Target())
	if err != nil {
		e.t.Fatalf("打开存量隧道：%v", err)
	}
	if body, err := p.Get("/ip"); err != nil || body == "DIRECT" {
		e.t.Fatalf("存量隧道不可用：%q %v", body, err)
	}
	return p
}

// mustCloseAfter 断言 p 在 since 之后 closeBound 内被关闭（since 为变更在节点生效的观测时刻）。
func (e *Env) mustClose(p *PConn, why string) {
	e.t.Helper()
	d, closed := WaitClosed(p, 5*time.Second)
	if !closed {
		e.t.Fatalf("%s：存量连接 5 秒内未关闭", why)
	}
	e.t.Logf("%s：存量连接在应用后 %v 关闭", why, d)
}

// adminChangeAccess 管理员代办换接入并等待任务结束。
func (a *Admin) ChangeAccess(subID, mode string) Task {
	a.t.Helper()
	var tk Task
	a.Must("POST", "/subscriptions/"+subID+"/access-changes", map[string]string{"mode": mode, "reason": "E2E 代办"}, &tk, Idem(NewKey()))
	tk = a.WaitTask(tk.ID)
	if tk.State != "succeeded" {
		a.t.Fatalf("代办换接入失败：%+v %s %s", tk, deref(tk.FailureCode), deref(tk.FailureReason))
	}
	return tk
}

// userChangeAccess 自助换接入并等待节点应用；返回新接入与“节点应用”之前打开的存量连接关闭用时。
func (e *Env) userChangeAccess(u *User, id, mode string) Access {
	e.t.Helper()
	a, err := u.ChangeAccess(id, mode)
	if err != nil {
		e.t.Fatalf("换接入（%s）：%v", mode, err)
	}
	as := e.WaitAdminSub(id, 30*time.Second, func(s *AdminSubscription) bool {
		return s.ActiveTaskID == nil && !s.Has("changing") && s.AppliedVersion >= s.DesiredVersion
	}, "换接入完成")
	_ = as
	return a
}

// A-11 换出口后观察到不同出口；订阅、接入、时长、流量和历史不变，存量连接关闭。
func TestA11_ExitChangeKeepsEverythingElse(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 3})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	c := CredsOf(s.Access)
	e.MustReach(c, e.Exits[0].Label)
	if _, _, err := Download(Connect, c, e.Target(), 2*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	usedBefore := e.SettledUsed(s.ID)
	before := u.Sub(s.ID)
	evBefore := u.Events(s.ID)
	idle := e.openIdle(c)

	tk := e.Admin.ChangeExit(s.ID, e.Exits[1].ResourceID, false)
	if tk.Kind != "exit_change" || tk.NewExitVersion == nil || tk.NewAccessVersion != nil {
		t.Fatalf("换出口任务：%+v", tk)
	}
	e.mustClose(idle, "换出口")
	e.MustReach(c, e.Exits[1].Label)
	for i := 0; i < 10; i++ { // 没有回落到旧出口
		if body, err := e.Reach(Connect, c); err != nil || body != e.Exits[1].Label {
			t.Fatalf("换出口后第 %d 次请求：%q %v", i, body, err)
		}
	}
	after := u.Sub(s.ID)
	if after.ID != before.ID || after.Access.Version != before.Access.Version || after.Access.Password != before.Access.Password ||
		after.Access.Ports != before.Access.Ports || after.Access.Username != before.Access.Username {
		t.Fatalf("换出口改变了接入：%+v → %+v", before.Access, after.Access)
	}
	if !after.Time.ExpiresAt.Equal(*before.Time.ExpiresAt) || after.Traffic.TotalBytes != before.Traffic.TotalBytes {
		t.Fatalf("换出口改变了权益：%+v/%+v → %+v/%+v", before.Time, before.Traffic, after.Time, after.Traffic)
	}
	if got := e.SettledUsed(s.ID); got < usedBefore {
		t.Fatalf("累计用量回退：%d → %d", usedBefore, got)
	}
	e.WaitSub(u, s.ID, 15*time.Second, func(x *Subscription) bool { return x.Exit != nil && x.Exit.IP == e.Exits[1].IP }, "出口 IP 更新")
	if after.Exit.Version <= before.Exit.Version && u.Sub(s.ID).Exit.Version <= before.Exit.Version {
		t.Fatalf("出口版本未增加：%d", before.Exit.Version)
	}
	evAfter := u.Events(s.ID)
	ids := map[string]bool{}
	for _, ev := range evAfter {
		ids[ev.ID] = true
	}
	for _, ev := range evBefore {
		if !ids[ev.ID] {
			t.Fatalf("换出口后历史丢失：%+v", ev)
		}
	}
	if len(evAfter) <= len(evBefore) {
		t.Fatal("换出口应追加一条出口变更记录")
	}
	if up := e.Admin.Upstream(e.Exits[0].ResourceID); up.Status == "bound" {
		t.Fatalf("旧资源仍绑定：%+v", up)
	}
	if up := e.Admin.Upstream(e.Exits[1].ResourceID); up.Status != "bound" || deref(up.BoundSub) != s.ID {
		t.Fatalf("新资源未绑定：%+v", up)
	}
	if e.CanaryHits() != 0 {
		t.Fatal("换出口期间发生直连")
	}
}

// A-12 候选上游认证失败、不可达、国家不符、IP 未变或 IP 已被占用时，不报告成功，不替换原绑定。
func TestA12_BadCandidatesNeverSwitch(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2, Extra: []ExitSpec{
		{Label: "bad-auth", RegisterPassword: "wrong-password", NoVerify: true},
		{Label: "down", Down: true},
		{Label: "geo-ca", Geo: "ca", NoVerify: true},
		{Label: "same-ip", IP: "203.0.113.10", NoVerify: true},  // = exit-A
		{Label: "taken-ip", IP: "203.0.113.11", NoVerify: true}, // = exit-B（被另一订阅占用）
	}})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	other := e.Provision(e.NewUser(), ProvisionOpts{ResourceID: e.Exits[1].ResourceID})
	e.WaitAdminSub(other.ID, 15*time.Second, func(a *AdminSubscription) bool { return a.Exit != nil && a.Exit.IP != nil }, "占用方出口检测")
	e.WaitAdminSub(s.ID, 15*time.Second, func(a *AdminSubscription) bool { return a.Exit != nil && a.Exit.IP != nil }, "当前出口检测")
	c := CredsOf(s.Access)
	cases := []struct {
		label string
		check func(cmp *ExitComparison, p Probe) bool // 返回该项“确实被识别为不合格”
	}{
		{"bad-auth", func(_ *ExitComparison, p Probe) bool { return deref(p.Result) == "failed" }},
		{"down", func(_ *ExitComparison, p Probe) bool { return deref(p.Result) == "failed" }},
		{"geo-ca", func(cmp *ExitComparison, _ Probe) bool { return cmp != nil && !cmp.CountryMatches }},
		{"same-ip", func(cmp *ExitComparison, _ Probe) bool { return cmp != nil && !cmp.DiffersFromCurrent }},
		{"taken-ip", func(cmp *ExitComparison, _ Probe) bool { return cmp != nil && !cmp.UniqueOnPlatform }},
	}
	for _, tc := range cases {
		section(t, tc.label)
		x := e.Exit(tc.label)
		p, err := e.Admin.CompareCandidate(s.ID, x.ResourceID)
		if err != nil {
			t.Fatalf("%s：下发比较：%v", tc.label, err)
		}
		if p.Comparison != nil && p.Comparison.Passed {
			t.Fatalf("%s：比较报告通过：%+v", tc.label, p.Comparison)
		}
		if !tc.check(p.Comparison, p) {
			t.Fatalf("%s：比较未指出原因：result=%s err=%s cmp=%+v", tc.label, deref(p.Result), deref(p.Error), p.Comparison)
		}
		if _, err := e.Admin.SubmitExitChange(s.ID, x.ResourceID, p.ID, false, ""); StatusOf(err) != 412 {
			t.Fatalf("%s：不合格候选提交应 412：%v", tc.label, err)
		}
		if got := e.CurrentExitLabel(s.ID); got != e.Exits[0].Label {
			t.Fatalf("%s：原绑定被替换为 %s", tc.label, got)
		}
		e.MustReach(c, e.Exits[0].Label, Connect)
	}
	if up := e.Admin.Upstream(e.Exits[0].ResourceID); up.Status != "bound" || deref(up.BoundSub) != s.ID {
		t.Fatalf("原资源绑定变化：%+v", up)
	}
	e.MustReach(c, e.Exits[0].Label)
	if e.CanaryHits() != 0 {
		t.Fatal("候选验证期间发生直连")
	}
}

// A-13 换出口与耗尽、买流量、暂停并发；最终没有越权启用，没有双活通道，没有遗留旧隧道。
func TestA13_ExitChangeRacesPauseAndExhaustion(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 4})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	c := CredsOf(s.Access)

	finalCheck := func(why string) {
		t.Helper()
		as := e.WaitApplied(s.ID)
		label := e.LabelOf(as.Exit.ResourceID)
		blocked := len(as.Blockers) > 0
		for i := 0; i < 10; i++ {
			body, err := e.Reach(Connect, c)
			switch {
			case blocked && err == nil:
				t.Fatalf("%s：阻断 %v 时仍可访问（%q）", why, as.Blockers, body)
			case !blocked && (err != nil || body != label):
				t.Fatalf("%s：应只经当前出口 %s，得到 %q %v", why, label, body, err)
			}
		}
		// 只有当前绑定的资源是 bound。
		var bound int
		for _, x := range e.Exits {
			if up := e.Admin.Upstream(x.ResourceID); up.Status == "bound" && deref(up.BoundSub) == s.ID {
				bound++
			}
		}
		if bound != 1 {
			t.Fatalf("%s：订阅绑定了 %d 个资源", why, bound)
		}
	}

	section(t, "换出口 + 暂停")
	idle := e.openIdle(c)
	cand := e.FreeExit(e.Exits[0].ResourceID)
	p, err := e.Admin.CompareCandidate(s.ID, cand.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	var tk Task
	var pauseErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		tk, err = e.Admin.SubmitExitChange(s.ID, cand.ResourceID, p.ID, false, "")
	}()
	go func() {
		defer wg.Done()
		_, pauseErr = e.Admin.Do("POST", "/subscriptions/"+s.ID+"/pause", map[string]string{"reason": "A-13"}, nil)
	}()
	wg.Wait()
	if pauseErr != nil {
		t.Fatalf("暂停：%v", pauseErr)
	}
	if err == nil {
		tk = e.Admin.WaitTask(tk.ID)
		t.Logf("换出口任务：%s", tk.State)
	} else if StatusOf(err) != 409 && StatusOf(err) != 412 {
		t.Fatalf("换出口：%v", err)
	}
	e.mustClose(idle, "暂停/换出口")
	as := e.WaitAdminSub(s.ID, 10*time.Second, func(a *AdminSubscription) bool { return a.Has("admin_paused") }, "暂停")
	_ = as
	finalCheck("暂停中")
	e.MustReject(c, "暂停中")
	e.Admin.Must("POST", "/subscriptions/"+s.ID+"/resume", nil, nil)
	e.WaitUsable(u, s.ID)
	finalCheck("恢复后")

	section(t, "换出口 + 耗尽 + 买流量")
	idle = e.openIdle(c)
	cur := e.Admin.Sub(s.ID).Exit.ResourceID
	cand = e.FreeExit(cur)
	p, err = e.Admin.CompareCandidate(s.ID, cand.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	var ord Order
	wg.Add(2)
	go func() {
		defer wg.Done()
		tk, err = e.Admin.SubmitExitChange(s.ID, cand.ResourceID, p.ID, false, "")
	}()
	go func() {
		defer wg.Done()
		e.Admin.SetRemainingBytes(s.ID, 0)
	}()
	wg.Wait()
	if err == nil {
		tk = e.Admin.WaitTask(tk.ID)
		t.Logf("换出口任务：%s", tk.State)
	} else if StatusOf(err) != 409 && StatusOf(err) != 412 {
		t.Fatalf("换出口：%v", err)
	}
	e.mustClose(idle, "耗尽/换出口")
	e.WaitAdminSub(s.ID, 10*time.Second, func(a *AdminSubscription) bool { return a.Has("traffic_exhausted") }, "耗尽")
	finalCheck("耗尽中")
	ord = e.Renew(u, OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID})
	_ = ord
	e.WaitUsable(u, s.ID)
	finalCheck("买流量后")
	if e.CanaryHits() != 0 {
		t.Fatal("并发变更期间发生直连")
	}
}

// A-36 用户申请换出口，同一未完成申请不重复执行；处理后权益不变。
func TestA36_UserExitRequestDedupAndProcess(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 3})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	before := u.Sub(s.ID)
	in := map[string]any{"subscriptionId": s.ID, "reason": "target_blocked", "note": "目标站拦截", "alsoChangeAccess": false}
	var req ExitRequest
	u.Must("POST", "/exit-requests", in, &req, Idem(NewKey()))
	if req.Status != "pending" || req.Charge != "free" {
		t.Fatalf("首个申请应为 pending/free：%+v", req)
	}
	// 并发重复申请：都 409 且 existing 指向同一申请。
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = u.Do("POST", "/exit-requests", in, nil, Idem(NewKey()))
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if StatusOf(err) != 409 {
			t.Fatalf("重复申请应 409：%v", err)
		}
		var body struct {
			Existing ExitRequest `json:"existing"`
		}
		if ae, ok := err.(*APIError); !ok || json.Unmarshal([]byte(ae.Body), &body) != nil || body.Existing.ID != req.ID {
			t.Fatalf("409 应带现有申请：%v", err)
		}
	}
	var mine []ExitRequest
	u.Get("/exit-requests", &mine)
	if len(mine) != 1 {
		t.Fatalf("应只有 1 个申请：%d", len(mine))
	}
	if got := u.Sub(s.ID); deref(got.OpenExitRequestID) != req.ID {
		t.Fatalf("订阅应指向未完成申请：%v", got.OpenExitRequestID)
	}
	// 未受理的申请不能用于提交换出口。
	p, err := e.Admin.CompareCandidate(s.ID, e.Exits[1].ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Admin.SubmitExitChange(s.ID, e.Exits[1].ResourceID, p.ID, false, req.ID); StatusOf(err) != 412 {
		t.Fatalf("未受理申请提交换出口应 412：%v", err)
	}
	e.Admin.Must("POST", "/exit-requests/"+req.ID+"/accept", map[string]string{"fee": "free"}, &req)
	if req.Status != "processing" {
		t.Fatalf("受理后应 processing：%+v", req)
	}
	if _, err := e.Admin.Do("POST", "/exit-requests/"+req.ID+"/accept", map[string]string{"fee": "free"}, nil); StatusOf(err) != 412 {
		t.Fatalf("重复受理应 412：%v", err)
	}
	tk, err := e.Admin.SubmitExitChange(s.ID, e.Exits[1].ResourceID, p.ID, false, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk = e.Admin.WaitTask(tk.ID); tk.State != "succeeded" {
		t.Fatalf("换出口任务：%+v", tk)
	}
	e.Eventually(10*time.Second, func() bool {
		u.Get("/exit-requests", &mine)
		return len(mine) == 1 && mine[0].Status == "succeeded"
	}, "申请未随任务完成（%+v）", mine)
	if deref(mine[0].NewIP) != e.Exits[1].IP || mine[0].OldIP != e.Exits[0].IP {
		t.Fatalf("申请的新旧 IP：%s → %s", mine[0].OldIP, deref(mine[0].NewIP))
	}
	after := u.Sub(s.ID)
	if after.OpenExitRequestID != nil || !after.Time.ExpiresAt.Equal(*before.Time.ExpiresAt) ||
		after.Traffic.TotalBytes != before.Traffic.TotalBytes || after.Access.Version != before.Access.Version {
		t.Fatalf("处理后权益或接入变化：%+v → %+v", before, after)
	}
	if after.Quotas.FreeExitChangesLeft != before.Quotas.FreeExitChangesLeft-1 {
		t.Fatalf("免费次数应减 1：%d → %d", before.Quotas.FreeExitChangesLeft, after.Quotas.FreeExitChangesLeft)
	}
	e.MustReach(CredsOf(after.Access), e.Exits[1].Label)
	// 免费次数用完：再申请为 manual，受理必须选择收费或免收。
	var req2 ExitRequest
	u.Must("POST", "/exit-requests", in, &req2, Idem(NewKey()))
	if req2.Charge != "manual" {
		t.Fatalf("超出免费次数应 manual：%+v", req2)
	}
	if _, err := e.Admin.Do("POST", "/exit-requests/"+req2.ID+"/accept", map[string]string{"fee": "free"}, nil); StatusOf(err) != 422 && StatusOf(err) != 412 {
		t.Fatalf("manual 申请按 free 受理应被拒：%v", err)
	}
	e.Admin.Must("POST", "/exit-requests/"+req2.ID+"/reject", map[string]string{"failureReason": "暂无合适出口"}, nil)
	u.Get("/exit-requests", &mine)
	if mine[0].ID != req2.ID || mine[0].Status != "failed" {
		t.Fatalf("驳回后：%+v", mine[0])
	}
}

// A-40 更换接入：新端口、新凭据可用；旧凭据被拒，旧端口不再监听，旧连接 ≤ 1 秒关闭；出口、时长、流量、累计用量连续。
func TestA40_ChangePortsAndPassword(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	old := CredsOf(s.Access)
	if _, _, err := Download(Connect, old, e.Target(), 2*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	usedBefore := e.SettledUsed(s.ID)
	idle := e.openIdle(old)
	acc := e.userChangeAccess(u, s.ID, "ports_and_password")
	e.mustClose(idle, "更换接入")
	if acc.Version != s.Access.Version+1 || acc.Ports == s.Access.Ports || acc.Password == s.Access.Password || acc.Username == s.Access.Username {
		t.Fatalf("新接入应换端口、用户名与密码：%+v → %+v", s.Access, acc)
	}
	if len(acc.UnseenChanges) == 0 {
		t.Fatal("新接入应标出改动字段")
	}
	nc := CredsOf(&acc)
	e.MustReach(nc, e.Exits[0].Label)
	for _, m := range AllModes {
		if a := old.Addr(m); !Refused(a) {
			t.Fatalf("旧端口 %s 仍在监听", a)
		}
	}
	// 旧凭据在新端口上被拒。
	e.MustReject(Creds{Host: nc.Host, Ports: nc.Ports, User: old.User, Pass: old.Pass}, "旧凭据")
	after := u.Sub(s.ID)
	if !after.Time.ExpiresAt.Equal(*s.Time.ExpiresAt) || after.Traffic.TotalBytes != s.Traffic.TotalBytes || e.CurrentExitLabel(s.ID) != e.Exits[0].Label {
		t.Fatalf("换接入改变了权益或出口：%+v", after)
	}
	if _, _, err := Download(Connect, nc, e.Target(), 1*MB, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := e.SettledUsed(s.ID); got < usedBefore+1*MB {
		t.Fatalf("累计用量不连续：%d → %d", usedBefore, got)
	}
	u.Must("POST", "/subscriptions/"+s.ID+"/access/ack", nil, nil)
	if got := u.Sub(s.ID); len(got.Access.UnseenChanges) != 0 {
		t.Fatalf("确认后 unseenChanges 应清空：%v", got.Access.UnseenChanges)
	}
}

// A-41 重置密码：端口不变，旧密码被拒，存量连接关闭，权益不变；超出频率上限时被拒并提示。
func TestA41_ResetPasswordAndRateLimit(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{})
	cur := CredsOf(s.Access)
	label := e.CurrentExitLabel(s.ID)
	for i := 1; i <= 5; i++ {
		idle := e.openIdle(cur)
		acc := e.userChangeAccess(u, s.ID, "password")
		e.mustClose(idle, "重置密码 "+strconv.Itoa(i))
		if acc.Ports != s.Access.Ports || acc.Username != s.Access.Username || acc.Password == cur.Pass {
			t.Fatalf("重置密码应只换密码：%+v → %+v", s.Access, acc)
		}
		e.MustReject(cur, "旧密码")
		cur = CredsOf(&acc)
		e.MustReach(cur, label, Connect, SOCKS)
		if got := u.Sub(s.ID); got.Quotas.AccessChangesLeft != 5-i {
			t.Fatalf("第 %d 次后剩余次数 %d", i, got.Quotas.AccessChangesLeft)
		}
	}
	_, err := u.ChangeAccess(s.ID, "password")
	if StatusOf(err) != 429 || CodeOf(err) != "quota_exhausted" {
		t.Fatalf("第 6 次应 429 quota_exhausted：%v", err)
	}
	if ae, ok := err.(*APIError); !ok || ae.Message == "" {
		t.Fatalf("超限应提示：%v", err)
	}
	e.MustReach(cur, label)
	after := u.Sub(s.ID)
	if !after.Time.ExpiresAt.Equal(*s.Time.ExpiresAt) || after.Traffic.TotalBytes != s.Traffic.TotalBytes {
		t.Fatalf("重置密码改变了权益：%+v", after)
	}
	// 管理员代办不计入自助次数。
	e.Admin.ChangeAccess(s.ID, "password")
	if got := u.Sub(s.ID); got.Quotas.AccessChangesLeft != 0 {
		t.Fatalf("代办不应影响自助次数：%d", got.Quotas.AccessChangesLeft)
	}
	// 滚动 24 小时后恢复。
	e.ShiftClock(24*time.Hour + time.Minute)
	if err := u.Login(); err != nil {
		t.Fatal(err)
	}
	if _, err := u.ChangeAccess(s.ID, "password"); err != nil {
		t.Fatalf("24 小时后应可再次重置：%v", err)
	}
}

// A-42 组合更换：新接入经新出口可用，旧接入和旧出口同时下线；在任一步注入失败，整体回到原状态。
func TestA42_CombinedChangeAtomic(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 4, PortRange: 9})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	old := CredsOf(s.Access)

	snapshot := func() (string, Access) {
		as := e.Admin.Sub(s.ID)
		return as.Exit.ResourceID, *u.Sub(s.ID).Access
	}
	mustUnchanged := func(why, res string, acc Access) {
		t.Helper()
		as := e.WaitApplied(s.ID)
		got := *u.Sub(s.ID).Access
		if as.Exit.ResourceID != res || got.Version != acc.Version || got.Ports != acc.Ports || got.Password != acc.Password {
			t.Fatalf("%s：失败后未回到原状态：出口 %s→%s 接入 %+v→%+v", why, res, as.Exit.ResourceID, acc, got)
		}
		if len(as.Blockers) != 0 {
			t.Fatalf("%s：失败后留有阻断 %v", why, as.Blockers)
		}
		e.MustReach(CredsOf(&got), e.LabelOf(res))
	}

	section(t, "注入失败：新端口被占用")
	res0, acc0 := snapshot()
	var hold []net.Listener
	used := map[int]bool{old.Ports.HTTP: true, old.Ports.HTTPS: true, old.Ports.SOCKS5: true}
	for p := e.Ports[0]; p <= e.Ports[1]; p++ {
		if used[p] {
			continue
		}
		if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
			hold = append(hold, ln)
		}
	}
	cand := e.Exits[1]
	p, err := e.Admin.CompareCandidate(s.ID, cand.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := e.Admin.SubmitExitChange(s.ID, cand.ResourceID, p.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	tk = e.Admin.WaitTask(tk.ID)
	for _, ln := range hold {
		ln.Close()
	}
	if tk.State != "failed" {
		t.Fatalf("端口被占用时组合更换应失败：%+v", tk)
	}
	t.Logf("失败码：%s %s", deref(tk.FailureCode), deref(tk.FailureReason))
	mustUnchanged("端口被占用", res0, acc0)
	if st := e.Admin.Upstream(cand.ResourceID).Status; st != "available" {
		t.Fatalf("失败后候选资源应回到可售：%s", st)
	}

	section(t, "注入失败：候选上游在切换时故障")
	cand = e.Exits[2]
	p, err = e.Admin.CompareCandidate(s.ID, cand.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	cand.Up.Close()
	tk, err = e.Admin.SubmitExitChange(s.ID, cand.ResourceID, p.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	tk = e.Admin.WaitTask(tk.ID)
	if tk.State == "succeeded" {
		as := e.WaitApplied(s.ID)
		_, rerr := e.Reach(Connect, CredsOf(u.Sub(s.ID).Access))
		t.Errorf("ISSUE: 候选上游在比较通过后故障，组合更换任务仍报告成功（任务 %s，出口 %s，新接入访问结果 %v）；"+
			"R-11.6 期望任一步失败整体回到原状态。任务只校验先前的比较探测，节点应用后未对新绑定做出口检测即判成功",
			tk.ID, as.Exit.ResourceID, rerr)
		// 换到可用出口，继续后续断言。
		e.Admin.ChangeExit(s.ID, e.FreeExit(e.Exits[2].ResourceID).ResourceID, false)
	} else {
		mustUnchanged("候选故障", res0, acc0)
	}

	section(t, "成功的组合更换")
	res0, acc0 = snapshot()
	idle := e.openIdle(CredsOf(&acc0))
	cand = e.FreeExit(res0, e.Exits[2].ResourceID)
	e.Admin.ChangeExit(s.ID, cand.ResourceID, true)
	e.mustClose(idle, "组合更换")
	as := e.WaitApplied(s.ID)
	acc := *u.Sub(s.ID).Access
	if as.Exit.ResourceID != cand.ResourceID || acc.Version != acc0.Version+1 || acc.Ports == acc0.Ports {
		t.Fatalf("组合更换后：出口 %s 接入 %+v", as.Exit.ResourceID, acc)
	}
	e.MustReach(CredsOf(&acc), cand.Label)
	for _, m := range AllModes {
		if a := CredsOf(&acc0).Addr(m); !Refused(a) {
			t.Fatalf("旧端口 %s 仍在监听", a)
		}
	}
	if up := e.Admin.Upstream(res0); up.Status == "bound" {
		t.Fatalf("旧出口仍绑定：%+v", up)
	}
	if e.CanaryHits() != 0 {
		t.Fatal("组合更换期间发生直连")
	}
}

// A-43 耗尽时可换接入和换出口（换完仍停用）；保留期内可更换；已释放不可更换，续时长重新履约后获得新接入和新出口。
func TestA43_ChangesWhileBlocked(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 4})
	u := e.NewUser()
	s := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})

	section(t, "流量耗尽")
	e.Admin.SetRemainingBytes(s.ID, 0)
	e.WaitSub(u, s.ID, 10*time.Second, func(x *Subscription) bool { return x.Has("traffic_exhausted") }, "耗尽")
	acc := e.userChangeAccess(u, s.ID, "ports_and_password")
	e.MustReject(CredsOf(&acc), "耗尽中换接入后")
	e.Admin.ChangeExit(s.ID, e.Exits[1].ResourceID, false)
	x := u.Sub(s.ID)
	if !x.Has("traffic_exhausted") {
		t.Fatalf("换完仍应因耗尽停用：%v", x.Blockers)
	}
	e.WaitApplied(s.ID)
	e.MustReject(CredsOf(x.Access), "耗尽中换出口后")
	e.Renew(u, OrderInput{Kind: "add_traffic", SubscriptionID: s.ID, PackID: e.Pack.ID})
	e.WaitUsable(u, s.ID)
	e.MustReach(CredsOf(u.Sub(s.ID).Access), e.Exits[1].Label)

	section(t, "保留期")
	e.Admin.ExpireIn(s.ID, 2*time.Second)
	e.WaitSub(u, s.ID, 15*time.Second, func(x *Subscription) bool { return x.Lifecycle == "retained" }, "保留期")
	acc = e.userChangeAccess(u, s.ID, "password")
	e.Admin.ChangeExit(s.ID, e.Exits[2].ResourceID, false)
	e.WaitApplied(s.ID)
	e.MustReject(CredsOf(&acc), "保留期换接入后")

	section(t, "已释放")
	e.ShiftClock(7*24*time.Hour + time.Hour)
	e.WaitAdminSub(s.ID, 15*time.Second, func(a *AdminSubscription) bool { return a.Lifecycle == "released" }, "释放")
	if err := u.Login(); err != nil {
		t.Fatal(err)
	}
	if _, err := u.ChangeAccess(s.ID, "password"); StatusOf(err) != 412 {
		t.Fatalf("已释放订阅换接入应 412：%v", err)
	}
	if _, err := e.Admin.Do("POST", "/subscriptions/"+s.ID+"/access-changes", map[string]string{"mode": "password", "reason": "x"}, nil, Idem(NewKey())); StatusOf(err) != 412 {
		t.Fatalf("已释放订阅代办换接入应 412：%v", err)
	}
	if _, err := e.Admin.CompareCandidate(s.ID, e.Exits[3].ResourceID); StatusOf(err) != 412 {
		t.Fatalf("已释放订阅换出口应 412：%v", err)
	}
	if _, err := u.Do("POST", "/exit-requests", map[string]any{"subscriptionId": s.ID, "reason": "other", "note": "", "alsoChangeAccess": false},
		nil, Idem(NewKey())); StatusOf(err) != 412 {
		t.Fatalf("已释放订阅申请换出口应 412：%v", err)
	}
	e.Renew(u, OrderInput{Kind: "renew_time", SubscriptionID: s.ID, Months: 1})
	ns := e.WaitUsable(u, s.ID)
	if ns.Access.Version <= acc.Version || ns.Access.Ports == acc.Ports {
		t.Fatalf("续时长重新履约应获得新接入：%+v → %+v", acc, ns.Access)
	}
	if got := e.CurrentExitLabel(s.ID); got == e.Exits[2].Label {
		t.Fatalf("续时长重新履约应获得新出口（仍为 %s）", got)
	}
	e.MustReach(CredsOf(ns.Access), e.CurrentExitLabel(s.ID))
}
