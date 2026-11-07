package node

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agent/agenttest"
	"github.com/go-gost/gost/internal/agentproto"
)

const readChunk = 16 * 1024 // 与 agent 的上行单次读取上限一致

// A-01/A-02（数据面部分）：控制面下发订阅后，三种接入协议都经绑定出口到达目标；
// 上游协议覆盖 http、https（TLS 到上游并校验证书）与 socks5。用量上报到控制面。
func TestProvisionThreeProtocolsThroughExit(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		id    string
		exit  agentproto.Exit
		label string
	}{
		{"sub_http", e.exitA, "exit-A"},
		{"sub_https", e.exitH, "exit-H"},
		{"sub_socks5", e.exitS, "exit-S"},
	}
	accs := map[string]*agentproto.Access{}
	var entries []agentproto.SubscriptionEntry
	for _, c := range cases {
		exit := c.exit
		accs[c.id] = e.access(1, "u-"+c.id)
		entries = append(entries, e.entry(c.id, 1<<30, time.Hour, accs[c.id], &exit))
	}
	e.put(entries...)
	for _, c := range cases {
		e.expectExit(accs[c.id], c.label)
		var client int64
		for _, p := range agenttest.Protos {
			body, n, err := e.fetch(p, accs[c.id], "/bytes?n=100000")
			if err != nil || len(body) != 100000 {
				t.Fatalf("%s %s: %d bytes, %v", c.id, p, len(body), err)
			}
			client += n
		}
		agenttest.Eventually(t, 3*time.Second, func() bool { return e.ctl.Used(c.id) >= uint64(client) },
			"%s: reported %d < client bytes %d", c.id, e.ctl.Used(c.id), client)
	}
	e.noDirect()
	if e.upA.Accepted.Load() == 0 || e.upH.Accepted.Load() == 0 || e.upS.Accepted.Load() == 0 {
		t.Fatal("an upstream saw no traffic")
	}
}

// docs/04 §2、§8 局限 7：生成的 SOCKS5 服务拒绝 UDP ASSOCIATE 与 BIND（UDP 中继会绕过计量与出站封禁）。
func TestSOCKS5UDPAndBindDisabled(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	e.put(e.entry("s1", 1<<30, time.Hour, acc, &e.exitA))
	for _, cmd := range []struct {
		name string
		code byte
	}{{"bind", 2}, {"udp-associate", 3}} {
		c, err := agenttest.DialSOCKS5(addr(acc.Ports.SOCKS5), acc.Username, acc.Password)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := agenttest.SOCKS5Command(c, cmd.code, "127.0.0.1:0")
		c.Close()
		if err != nil || rep != 2 { // 2 = connection not allowed by ruleset
			t.Fatalf("%s: reply %d err %v, want 2 (not allowed)", cmd.name, rep, err)
		}
	}
	// 普通 CONNECT 仍然可用。
	e.expectExit(acc, "exit-A")
}

// A-05/A-29：额度与期限来自期望状态并在数据面执行：耗尽后存量 ≤ 1 秒关闭、新连接被拒；
// 充值（更高版本）后恢复；到期时 ≤ 1 秒关闭全部连接。
func TestQuotaAndExpiryFromDesiredState(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	const quota = 300_000
	e.put(e.entry("s1", quota, time.Hour, acc, &e.exitA))
	idle := e.mustTunnel(agenttest.SOCKS5, acc)
	e.fetch(agenttest.HTTP, acc, "/bytes?n=5000000")
	if d, closed := agenttest.WaitClosed(idle, stopLimit); !closed {
		t.Fatalf("idle tunnel still open %v after exhaustion", d)
	}
	e.expectRejected(acc, "exhausted subscription")
	rep := e.waitApplied("s1", e.current("s1").Version)
	agenttest.Eventually(t, 3*time.Second, func() bool { return e.ctl.Used("s1") >= quota }, "exhaustion not reported")
	if used := e.ctl.Used("s1"); used > quota+3*readChunk {
		t.Fatalf("reported %d exceeds quota %d", used, quota)
	}
	_ = rep

	// 充值：新版本额度高于已用量。
	used := e.rt.Agent().Used("s1")
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { en.EntitledTotal = used + 1<<30 })
	e.expectExit(acc, "exit-A")

	// 到期：约 1.2 秒后到期，不需要控制面参与。
	exp := time.Now().Add(1200 * time.Millisecond)
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { en.ExpiresAt = exp.UTC() })
	var tunnels []*agenttest.Conn
	for _, p := range agenttest.Protos {
		tunnels = append(tunnels, e.mustTunnel(p, acc))
	}
	time.Sleep(time.Until(exp))
	for i, c := range tunnels {
		if d, closed := agenttest.WaitClosed(c, stopLimit); !closed {
			t.Fatalf("%s tunnel open %v after expiry", agenttest.Protos[i], d)
		}
	}
	e.expectRejected(acc, "expired subscription")
	e.noDirect()
}

// A-40：接入版本变化（新端口、新凭据）即换接入：新接入可用，旧端口不再监听，旧凭据被拒，
// 旧连接 ≤ 1 秒关闭，出口与累计用量连续。
func TestAccessVersionRotates(t *testing.T) {
	e := newEnv(t)
	old := e.access(1, "u-s1")
	e.put(e.entry("s1", 1<<30, time.Hour, old, &e.exitA))
	e.fetch(agenttest.HTTP, old, "/bytes?n=50000")
	live := e.mustTunnel(agenttest.HTTPS, old)
	usedBefore := e.rt.Agent().Used("s1")

	next := e.access(2, "u-s1-v2")
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { en.Access = next })
	if d, closed := agenttest.WaitClosed(live, stopLimit); !closed {
		t.Fatalf("connection on old access open %v after rotation", d)
	}
	for _, p := range []int{old.Ports.HTTP, old.Ports.HTTPS, old.Ports.SOCKS5} {
		if !agenttest.Refused(addr(p)) {
			t.Fatalf("old port %d still listening", p)
		}
	}
	cross := *next
	cross.Username, cross.Password = old.Username, old.Password
	e.expectRejected(&cross, "old credentials on new ports")
	e.expectExit(next, "exit-A")
	if e.rt.Agent().Used("s1") < usedBefore {
		t.Fatal("usage went backwards")
	}
}

// A-41：只有密码变化（端口与用户名不变）即重置密码：端口不变，旧密码被拒，存量连接关闭。
func TestPasswordOnlyChange(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	e.put(e.entry("s1", 1<<30, time.Hour, acc, &e.exitA))
	live := e.mustTunnel(agenttest.SOCKS5, acc)
	next := *acc
	next.Version, next.Password = 2, "new-secret"
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { en.Access = &next })
	if d, closed := agenttest.WaitClosed(live, stopLimit); !closed {
		t.Fatalf("connection open %v after password reset", d)
	}
	e.expectRejected(acc, "old password")
	e.expectExit(&next, "exit-A")
}

// A-11：出口版本变化即换出口：接入不变，存量连接关闭，之后经新出口。
func TestExitVersionSwaps(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	e.put(e.entry("s1", 1<<30, time.Hour, acc, &e.exitA))
	live := e.mustTunnel(agenttest.HTTP, acc)
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { x := e.exitB; en.Exit = &x })
	if d, closed := agenttest.WaitClosed(live, stopLimit); !closed {
		t.Fatalf("connection via old exit open %v", d)
	}
	e.expectExit(acc, "exit-B")
	// 换到 socks5 上游，再去掉出口：失败即拒绝，不直连。
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { x := e.exitS; en.Exit = &x })
	e.expectExit(acc, "exit-S")
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { en.Exit = nil })
	for _, p := range agenttest.Protos {
		if body, _, err := e.fetch(p, acc, "/ip"); err == nil {
			t.Fatalf("%s: forwarded without exit: %q", p, body)
		}
	}
	e.noDirect()
}

// A-42：同一版本内接入与出口同时变化即组合更换：新接入经新出口，旧接入与旧出口同时下线。
func TestCombinedAccessAndExitChange(t *testing.T) {
	e := newEnv(t)
	old := e.access(1, "u-s1")
	e.put(e.entry("s1", 1<<30, time.Hour, old, &e.exitA))
	live := e.mustTunnel(agenttest.HTTP, old)
	next := e.access(2, "u-s1-v2")
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { en.Access = next; x := e.exitB; en.Exit = &x })
	if _, closed := agenttest.WaitClosed(live, stopLimit); !closed {
		t.Fatal("old access connection survived combined change")
	}
	e.expectRejected(old, "old access")
	e.expectExit(next, "exit-B")
}

// 列表中消失的订阅：撤销接入（端口关闭、连接断开），最终用量上报后从节点删除。
func TestRemovedSubscriptionRevoked(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	keep := e.access(1, "u-s2")
	e.put(e.entry("s1", 1<<30, time.Hour, acc, &e.exitA), e.entry("s2", 1<<30, time.Hour, keep, &e.exitB))
	_, n, _ := e.fetch(agenttest.HTTP, acc, "/bytes?n=20000")
	live := e.mustTunnel(agenttest.SOCKS5, acc)
	e.remove("s1")
	if _, closed := agenttest.WaitClosed(live, stopLimit); !closed {
		t.Fatal("connection survived removal")
	}
	agenttest.Eventually(t, 2*time.Second, func() bool { return agenttest.Refused(addr(acc.Ports.HTTP)) }, "port still listening")
	e.expectRejected(acc, "removed subscription")
	// 删除前的用量已上报；之后的上报不再包含它。
	agenttest.Eventually(t, 3*time.Second, func() bool { return e.ctl.Used("s1") >= uint64(n) },
		"final usage %d < %d", e.ctl.Used("s1"), n)
	e.ctl.WaitReport(t, 3*time.Second, func(r *agentproto.Report) bool {
		_, has := agenttest.SubIn(r, "s1")
		_, other := agenttest.SubIn(r, "s2")
		return !has && other
	})
	e.expectExit(keep, "exit-B")
}

// A-17：端口被占用时开通失败：上报带 apply_error（port_in_use）、已应用版本不前进、其他订阅不受影响；
// 换接入失败时订阅保持原接入；端口释放后下发更高版本即恢复。
func TestApplyFailureReportedOthersUnaffected(t *testing.T) {
	e := newEnv(t)
	good := e.access(1, "u-good")
	e.put(e.entry("good", 1<<30, time.Hour, good, &e.exitA))

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	bad := e.access(1, "u-bad")
	bad.Ports.HTTPS = busy.Addr().(*net.TCPAddr).Port
	badEntry := e.entry("bad", 1<<30, time.Hour, bad, &e.exitB)
	e.putNoWait(badEntry)
	rep := e.ctl.WaitReport(t, 5*time.Second, func(r *agentproto.Report) bool {
		s, ok := agenttest.SubIn(r, "bad")
		return ok && s.ApplyError != nil
	})
	s, _ := agenttest.SubIn(&rep, "bad")
	if s.ApplyError.Code != agentproto.ApplyPortInUse || s.ApplyError.Version != badEntry.Version || s.AppliedVersion != 0 {
		t.Fatalf("bad: %+v / %+v", s, s.ApplyError)
	}
	if g, _ := agenttest.SubIn(&rep, "good"); g.ApplyError != nil || g.AppliedVersion == 0 {
		t.Fatalf("good affected: %+v", g)
	}
	for _, p := range []int{bad.Ports.HTTP, bad.Ports.SOCKS5} {
		if !agenttest.Refused(addr(p)) {
			t.Fatalf("half-created access listening on %d", p)
		}
	}
	e.expectExit(good, "exit-A")

	// 换接入失败：保持原接入与原版本。
	busy2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy2.Close()
	goodV := e.current("good").Version
	rot := e.access(2, "u-good-v2")
	rot.Ports.SOCKS5 = busy2.Addr().(*net.TCPAddr).Port
	failed := e.current("good")
	failed.Version, failed.Access = e.ver.Add(1), rot
	e.putNoWait(failed)
	rep = e.ctl.WaitReport(t, 5*time.Second, func(r *agentproto.Report) bool {
		s, ok := agenttest.SubIn(r, "good")
		return ok && s.ApplyError != nil
	})
	g, _ := agenttest.SubIn(&rep, "good")
	if g.AppliedVersion != goodV || g.ApplyError.Version != failed.Version || g.ApplyError.Code != agentproto.ApplyPortInUse {
		t.Fatalf("failed rotation: %+v / %+v", g, g.ApplyError)
	}
	e.expectExit(good, "exit-A")

	// 控制面回滚（更高版本、原接入）后错误消失；坏订阅换端口后成功。
	e.bump("good", func(en *agentproto.SubscriptionEntry) { en.Access = good })
	busy.Close()
	fixed := e.access(1, "u-bad")
	e.bump("bad", func(en *agentproto.SubscriptionEntry) { en.Access = fixed })
	rep = e.ctl.WaitReport(t, 5*time.Second, func(r *agentproto.Report) bool {
		b, _ := agenttest.SubIn(r, "bad")
		g, _ := agenttest.SubIn(r, "good")
		return b.ApplyError == nil && g.ApplyError == nil && b.AppliedVersion > 0
	})
	e.expectExit(fixed, "exit-B")
	e.expectExit(good, "exit-A")
}

// A-10/A-22（节点侧）：上报到达控制面；节点重启后累计用量从本地恢复、不回退；
// 控制面下发更高的 used_floor 时节点取较大值，并在之后的重启中保持。
func TestReportsGreatestAcrossRestart(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	e.put(e.entry("s1", 1<<40, time.Hour, acc, &e.exitA))
	for i := 0; i < 3; i++ {
		e.fetch(agenttest.Protos[i], acc, "/bytes?n=200000")
	}
	local := e.rt.Agent().Used("s1")
	agenttest.Eventually(t, 3*time.Second, func() bool { return e.ctl.Used("s1") == local }, "usage not reported")
	if reps := e.ctl.Reports(); reps[len(reps)-1].AgentVersion != "test" || !reps[len(reps)-1].PersistOK {
		t.Fatalf("report header: %+v", reps[len(reps)-1])
	}

	e.restart()
	rep := e.ctl.WaitReport(t, 3*time.Second, func(r *agentproto.Report) bool { _, ok := agenttest.SubIn(r, "s1"); return ok })
	s, _ := agenttest.SubIn(&rep, "s1")
	if s.UsedTotal < local {
		t.Fatalf("usage went backwards across restart: %d < %d", s.UsedTotal, local)
	}

	floor := local + 5_000_000
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { en.UsedFloor = floor })
	agenttest.Eventually(t, 3*time.Second, func() bool { return e.ctl.Used("s1") >= floor }, "floor not applied")
	// 更低的 floor（乱序、重放）不降低用量。
	e.bump("s1", func(en *agentproto.SubscriptionEntry) { en.UsedFloor = 10 })
	e.restart()
	rep = e.ctl.WaitReport(t, 3*time.Second, func(r *agentproto.Report) bool { _, ok := agenttest.SubIn(r, "s1"); return ok })
	if s, _ := agenttest.SubIn(&rep, "s1"); s.UsedTotal < floor {
		t.Fatalf("usage %d below floor %d after restart", s.UsedTotal, floor)
	}
}

// A-20/A-35，docs/04 §8 局限 6：控制面离线时节点按最后已知额度与期限继续执行；
// 离线期间节点重启也能从本地状态恢复服务：有效订阅可用，已到期与已耗尽的仍被拒。
func TestControlPlaneOfflineKeepsEnforcing(t *testing.T) {
	e := newEnv(t)
	live := e.access(1, "u-live")
	soon := e.access(1, "u-soon")
	small := e.access(1, "u-small")
	soonExp := time.Now().Add(2 * time.Second)
	soonEntry := e.entry("soon", 1<<30, time.Hour, soon, &e.exitB)
	soonEntry.ExpiresAt = soonExp.UTC()
	e.put(e.entry("live", 1<<30, time.Hour, live, &e.exitA), soonEntry, e.entry("small", 200_000, time.Hour, small, &e.exitA))
	idle := e.mustTunnel(agenttest.HTTPS, soon)

	e.ctl.Stop()
	e.expectExit(live, "exit-A")
	e.fetch(agenttest.HTTP, small, "/bytes?n=1000000")
	e.expectRejected(small, "exhausted while offline")
	time.Sleep(time.Until(soonExp))
	if d, closed := agenttest.WaitClosed(idle, stopLimit); !closed {
		t.Fatalf("expiry not enforced while offline (%v)", d)
	}
	e.expectRejected(soon, "expired while offline")

	// 离线期间重启节点。
	e.restart()
	e.expectExit(live, "exit-A")
	e.expectRejected(soon, "expired after offline restart")
	e.expectRejected(small, "exhausted after offline restart")
	st, err := os.Stat(filepath.Join(e.cfg.DataDir, stateFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("desired.json: %v mode %v", err, st.Mode())
	}
	if di, _ := os.Stat(e.cfg.DataDir); di.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode %v", di.Mode())
	}

	// 控制面恢复：上报补齐，版本不变的期望状态不引起重建。
	e.ctl.Start()
	rep := e.ctl.WaitReport(t, 8*time.Second, func(r *agentproto.Report) bool { return len(r.Subscriptions) == 3 })
	for _, id := range []string{"live", "soon", "small"} {
		s, _ := agenttest.SubIn(&rep, id)
		if s.AppliedVersion != e.current(id).Version || s.ApplyError != nil {
			t.Fatalf("%s after reconnect: %+v", id, s)
		}
	}
	if e.ctl.Used("small") < 200_000 {
		t.Fatalf("offline usage not reported: %d", e.ctl.Used("small"))
	}
	e.expectExit(live, "exit-A")
	e.noDirect()
}

// 出口检测（A-W5、A-15）：执行期望状态中的探测，经出口访问回显端点，按探测 ID 回传；
// 失败给出原因；探测不计入客户订阅；同一探测不重复执行，重启后重做的回传由控制面幂等处理。
func TestProbesExecutedAndPostedIdempotently(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	e.put(e.entry("s1", 1<<30, time.Hour, acc, &e.exitA))
	e.fetch(agenttest.HTTP, acc, "/bytes?n=10000")
	agenttest.Eventually(t, 3*time.Second, func() bool { return e.ctl.Used("s1") == e.rt.Agent().Used("s1") }, "not reported")
	usedBefore := e.rt.Agent().Used("s1")

	wrong := e.exitA
	wrong.Password = "wrong"
	deadline := time.Now().Add(time.Minute).UTC()
	e.ctl.Update(func(d *agentproto.Desired) {
		d.Probes = []agentproto.Probe{
			{ID: "prb_check", Kind: agentproto.ProbeExitCheck, SubscriptionID: "s1", Exit: e.exitA, EchoURL: "http://unused.invalid/", Deadline: deadline},
			{ID: "prb_cand", Kind: agentproto.ProbeCandidateCompare, SubscriptionID: "s1", Exit: e.exitS, EchoURL: "http://unused.invalid/", Deadline: deadline},
			{ID: "prb_https", Kind: agentproto.ProbeUpstreamVerify, Exit: e.exitH, EchoURL: "http://unused.invalid/", Deadline: deadline},
			{ID: "prb_auth", Kind: agentproto.ProbeUpstreamVerify, Exit: wrong, EchoURL: "http://unused.invalid/", Deadline: deadline},
		}
	})
	want := map[string]struct {
		ok  bool
		ip  string
		err string
	}{
		"prb_check": {true, ipA, ""}, "prb_cand": {true, ipS, ""}, "prb_https": {true, ipH, ""},
		"prb_auth": {false, "", "auth_failed"},
	}
	for id, w := range want {
		agenttest.Eventually(t, 5*time.Second, func() bool { _, _, ok := e.ctl.ProbeResult(id); return ok }, "probe %s not posted", id)
		r, posts, _ := e.ctl.ProbeResult(id)
		if r.OK != w.ok || r.ObservedIP != w.ip || r.Error != w.err || r.CompletedAt.IsZero() || posts != 1 {
			t.Fatalf("%s: %+v posts=%d", id, r, posts)
		}
	}
	// 再次下发（修订号变化、探测列表不变）不重复执行。
	e.bump("s1", func(en *agentproto.SubscriptionEntry) {})
	time.Sleep(500 * time.Millisecond)
	if _, posts, _ := e.ctl.ProbeResult("prb_check"); posts != 1 {
		t.Fatalf("probe re-run without restart: posts=%d", posts)
	}
	if got := e.rt.Agent().Used("s1"); got != usedBefore {
		t.Fatalf("probes metered to customer: %d → %d", usedBefore, got)
	}
	// 重启后探测仍在期望状态中：重做并重复回传，控制面保留首个结果（幂等）。
	e.restart()
	agenttest.Eventually(t, 5*time.Second, func() bool { _, posts, _ := e.ctl.ProbeResult("prb_check"); return posts == 2 }, "probe not re-posted after restart")
	if r, _, _ := e.ctl.ProbeResult("prb_check"); r.ObservedIP != ipA {
		t.Fatalf("first result replaced: %+v", r)
	}
	e.noDirect()
}

// R-14.7：按回执的毫秒级 ServerTime 估算时钟偏差，超过 1 秒时上报 exceeded；恢复后清除。
func TestClockSkewReported(t *testing.T) {
	e := newEnv(t)
	e.ctl.SetClockOffset(-3 * time.Second) // 控制面慢 3 秒 = 节点快 3 秒
	rep := e.ctl.WaitReport(t, 3*time.Second, func(r *agentproto.Report) bool { return r.ClockSkew != nil && r.ClockSkew.Exceeded })
	if rep.ClockSkew.LowMs < 2500 || rep.ClockSkew.HighMs > 3500 {
		t.Fatalf("skew %+v", rep.ClockSkew)
	}
	e.ctl.SetClockOffset(0)
	rep = e.ctl.WaitReport(t, 3*time.Second, func(r *agentproto.Report) bool { return r.ClockSkew != nil && !r.ClockSkew.Exceeded })
	if rep.ClockSkew.LowMs > 0 || rep.ClockSkew.HighMs < 0 || rep.ClockSkew.HighMs-rep.ClockSkew.LowMs > 500 {
		t.Fatalf("skew %+v", rep.ClockSkew)
	}
}

// 控制面不可用时上报退避重试，恢复后立即补报（累计值，重放安全）。
func TestReportRetriesDuringOutage(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	e.put(e.entry("s1", 1<<30, time.Hour, acc, &e.exitA))
	e.ctl.Stop()
	time.Sleep(time.Second)
	_, n, err := e.fetch(agenttest.HTTP, acc, "/bytes?n=300000")
	if err != nil {
		t.Fatal(err)
	}
	before := len(e.ctl.Reports())
	e.ctl.Start()
	agenttest.Eventually(t, 5*time.Second, func() bool { return e.ctl.Used("s1") >= uint64(n) }, "usage not reported after outage")
	if len(e.ctl.Reports()) <= before {
		t.Fatal("no report after outage")
	}
}
