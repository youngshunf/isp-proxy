package core_test

import (
	"sync"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
)

type accessResp struct {
	Version       int      `json:"version"`
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	Ports         ports    `json:"ports"`
	UnseenChanges []string `json:"unseenChanges"`
}

// A-40：更换接入：新端口组（三个都换）、新凭据；节点应用后旧版本撤销、旧端口冷却；出口与权益连续。
func TestA40_AccessChangeIssuesThreeNewPorts(t *testing.T) {
	x := newH(t)
	u := x.newUser("a40@example.com")
	sub := x.activeSub(u)
	x.apply(x.node, map[string]uint64{sub: uint64(2 * gb)})
	before := x.sub(u, sub)
	var a accessResp
	x.ok(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "ports_and_password"}, x.key()), 200, &a)
	old := map[int]bool{}
	for _, p := range before.Access.Ports.list() {
		old[p] = true
	}
	for _, p := range a.Ports.list() {
		if old[p] {
			t.Fatalf("换接入必须三个端口全换: %v → %v", before.Access.Ports, a.Ports)
		}
	}
	if a.Username == before.Access.Username || a.Password == before.Access.Password || !eq(a.UnseenChanges, []string{"ports", "password"}) {
		t.Fatalf("%+v", a)
	}
	v := x.wantBlockers(u, sub, "changing")
	_ = v
	de := entry(x.desired(x.node), sub)
	if de.Access.Ports.HTTP != a.Ports.HTTP || de.Exit.Version != uint64(before.Exit.Version) {
		t.Fatal("期望状态应下发新接入、保持原出口")
	}
	x.apply(x.node)
	after := x.wantBlockers(u, sub)
	if after.Access.Version != a.Version || after.Exit.Version != before.Exit.Version || after.Traffic != before.Traffic ||
		!x.expires(sub).Equal(*before.Time.ExpiresAt) {
		t.Fatal("出口、时长、流量、累计用量应连续")
	}
	oldID := x.str(`SELECT id FROM access_versions WHERE subscription_id = $1 AND version = $2`, sub, before.Access.Version)
	if st := x.str(`SELECT state FROM access_versions WHERE id = $1`, oldID); st != "revoked" {
		t.Fatal(st)
	}
	if n := x.int(`SELECT count(*) FROM ports WHERE node_id = $1 AND port = ANY($2) AND state = 'cooldown'`, x.node, before.Access.Ports.list()); n != 3 {
		t.Fatalf("旧端口应全部冷却: %d", n)
	}
	x.ok(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access/ack", nil), 204, nil)
	if v := x.sub(u, sub); len(v.Access.UnseenChanges) != 0 {
		t.Fatal("ack 后应清空")
	}
}

// A-41：重置密码：端口与用户名不变、密码变化；24 小时 5 次上限（D-18），超出 429；管理员代办不计次且暂停时可用。
func TestA41_PasswordResetKeepsPortsAndIsRateLimited(t *testing.T) {
	x := newH(t)
	u := x.newUser("a41@example.com")
	sub := x.activeSub(u)
	before := x.sub(u, sub)
	for i := 0; i < 5; i++ {
		var a accessResp
		x.ok(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password"}, x.key()), 200, &a)
		if a.Ports != before.Access.Ports || a.Username != before.Access.Username || a.Password == before.Access.Password {
			t.Fatalf("重置密码只换密码: %+v", a)
		}
		de := entry(x.desired(x.node), sub)
		if de.Access.Password != a.Password || de.Access.Ports.HTTP != before.Access.Ports.HTTP {
			t.Fatal("期望状态应是同端口的新密码")
		}
		if err := de2Validate(x.desired(x.node)); err != nil {
			t.Fatal(err)
		}
		x.apply(x.node)
		if n := x.int(`SELECT count(*) FROM ports WHERE access_version_id = (SELECT current_access_id FROM subscriptions WHERE id = $1)`, sub); n != 3 {
			t.Fatalf("端口应随新版本保留: %d", n)
		}
	}
	if q := x.sub(u, sub).Quotas.AccessChangesLeft; q != 0 {
		t.Fatal(q)
	}
	x.fail(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password"}, x.key()), 429, "quota_exhausted")
	// 管理员代办：暂停时仍可，不计入用户次数。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/pause", map[string]any{"reason": "核查"}), 200, nil)
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password", "reason": "客户要求"}, x.key()), 200, nil)
	// 进行中任务：再提交 409。
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password", "reason": "x"}, x.key()), 409, "conflict")
	x.apply(x.node)
	x.fail(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password"}, x.key()), 412, "precondition")
	// 窗口滚动后恢复。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/resume", nil), 200, nil)
	x.clk.Advance(24*time.Hour + time.Minute)
	x.ok(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password"}, x.key()), 200, nil)
}

func de2Validate(d *agentproto.Desired) error { return d.Validate() }

// compare 下发候选比较并让节点回传结果（observed 为空表示探测失败），返回探测 ID。
func (x *h) compare(sub, res, observed string) (string, resp) {
	x.t.Helper()
	var p idOnly
	r := x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/exit-candidates", map[string]any{"resourceId": res})
	if r.status != 202 {
		return "", r
	}
	x.ok(r, 202, &p)
	x.answerProbes(x.node, func(pr agentproto.Probe) (string, bool) {
		if pr.ID == p.ID {
			return observed, observed != ""
		}
		return x.probeIP(pr)
	})
	return p.ID, r
}

func (x *h) exitChange(sub, res, probe string, alsoAccess bool) resp {
	return x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/exit-changes", map[string]any{"resourceId": res,
		"compareProbeId": probe, "alsoChangeAccess": alsoAccess, "reason": "客户反馈被封"}, x.key())
}

// A-12：候选认证失败、国家不符、IP 未变、IP 已被其他订阅占用时，不能提交换出口，原绑定不变。
func TestA12_CandidateRejectedKeepsBinding(t *testing.T) {
	x := newH(t)
	u := x.newUser("a12@example.com")
	sub := x.activeSub(u)
	other := x.activeSub(x.newUser("o12@example.com"))
	curIP := x.str(`SELECT host(b.verified_ip) FROM subscriptions s JOIN exit_bindings b ON b.id = s.current_exit_id WHERE s.id = $1`, sub)
	otherIP := x.str(`SELECT host(b.verified_ip) FROM subscriptions s JOIN exit_bindings b ON b.id = s.current_exit_id WHERE s.id = $1`, other)
	exit0 := x.str(`SELECT current_exit_id FROM subscriptions WHERE id = $1`, sub)
	cases := []struct {
		name, observed string
	}{
		{"认证失败", ""},
		{"IP 未变", curIP},
		{"IP 被占用", otherIP},
		{"国家不符", x.nextIP("jp")},
		{"国家未知", "198.51.100.77"},
	}
	for _, c := range cases {
		res, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
		probe, r := x.compare(sub, res, c.observed)
		if probe == "" {
			t.Fatalf("%s: %d %s", c.name, r.status, r.body)
		}
		if r := x.exitChange(sub, res, probe, false); r.status != 412 {
			t.Fatalf("%s: 应 412，得到 %d %s", c.name, r.status, r.body)
		}
		if x.str(`SELECT current_exit_id FROM subscriptions WHERE id = $1`, sub) != exit0 {
			t.Fatalf("%s: 不应替换原绑定", c.name)
		}
	}
	// 用旧的比较结果提交 → 412（必须是最近一次）。
	res, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
	p1, _ := x.compare(sub, res, x.nextIP("us"))
	x.clk.Advance(time.Second)
	p2, _ := x.compare(sub, res, x.nextIP("us"))
	x.fail(x.exitChange(sub, res, p1, false), 412, "precondition")
	x.ok(x.exitChange(sub, res, p2, false), 200, nil)
}

// A-42：组合更换：新接入经新出口，期望状态里同时切换；注入失败整体回到原状态；成功后旧接入与旧出口同时下线。
func TestA42_CombinedChangeAtomicWithRollback(t *testing.T) {
	x := newH(t)
	u := x.newUser("a42@example.com")
	sub := x.activeSub(u)
	before := x.sub(u, sub)
	oldRes := x.str(`SELECT b.resource_id FROM subscriptions s JOIN exit_bindings b ON b.id = s.current_exit_id WHERE s.id = $1`, sub)
	res1, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
	p, _ := x.compare(sub, res1, x.nextIP("us"))
	var task struct {
		ID, Kind, State string
	}
	x.ok(x.exitChange(sub, res1, p, true), 200, &task)
	if task.Kind != "combined" || task.State != "running" {
		t.Fatalf("%+v", task)
	}
	de := entry(x.desired(x.node), sub)
	if de.Exit.Version == uint64(before.Exit.Version) || de.Access.Version == uint64(before.Access.Version) {
		t.Fatal("组合更换应同时下发新接入与新出口")
	}
	// 注入失败：出口不可用。
	x.failApply(x.node, sub, agentproto.ApplyExitInvalid)
	if st := x.str(`SELECT state FROM tasks WHERE id = $1`, task.ID); st != "failed" {
		t.Fatal(st)
	}
	de = entry(x.desired(x.node), sub)
	if de.Exit.Version != uint64(before.Exit.Version) || de.Access.Version != uint64(before.Access.Version) || de.Access.Password != before.Access.Password {
		t.Fatal("失败后期望状态应整体回到原接入与原出口")
	}
	if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, res1); st != "quarantine" {
		t.Fatalf("不可用的候选应隔离: %s", st)
	}
	if x.int(`SELECT count(*) FROM alerts WHERE kind = 'exit_change_failed' AND status = 'open'`) != 1 {
		t.Fatal("缺换出口失败告警")
	}
	x.apply(x.node)
	if v := x.wantBlockers(u, sub); v.Access.Version != before.Access.Version || v.Exit.Version != before.Exit.Version {
		t.Fatal("原接入与原出口应继续可用")
	}
	// 再试一次并成功。
	res2, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
	newIP := x.nextIP("us")
	p, _ = x.compare(sub, res2, newIP)
	x.ok(x.exitChange(sub, res2, p, true), 200, &task)
	x.apply(x.node)
	x.answerProbes(x.node, x.probeIP) // 切换后检测通过（失败见 verify_test.go）
	v := x.wantBlockers(u, sub)
	if v.Exit.IP != newIP || v.Access.Version == before.Access.Version {
		t.Fatalf("%+v", v)
	}
	if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, oldRes); st != "cooldown" {
		t.Fatal(st)
	}
	if x.int(`SELECT count(*) FROM access_versions WHERE subscription_id = $1 AND state IN ('active', 'pending')`, sub) != 1 ||
		x.int(`SELECT count(*) FROM exit_bindings WHERE subscription_id = $1 AND state IN ('active', 'pending')`, sub) != 1 {
		t.Fatal("不应有双活通道")
	}
	if x.int(`SELECT count(*) FROM alerts WHERE kind = 'exit_change_failed' AND status = 'open'`) != 0 {
		t.Fatal("成功后告警应关闭")
	}
	// 成功前已经经新出口检测过（切换后检测），新绑定带检测结果。
	if x.int(`SELECT count(*) FROM probes p JOIN subscriptions s ON s.current_exit_id = p.exit_binding_id
	           WHERE p.kind = 'exit_check' AND s.id = $1 AND p.status = 'done' AND p.result = 'ok'`, sub) == 0 {
		t.Fatal("换出口成功前应已检测新出口")
	}
}

// A-17：开通遇端口冲突或证书错误不显示就绪、可清理并重试，不影响其他订阅；节点超时同样回滚。
func TestA17_ApplyFailureRollsBackFulfillment(t *testing.T) {
	x := newH(t)
	other := x.activeSub(x.newUser("o17@example.com"))
	u := x.newUser("a17@example.com")
	res := x.resource()
	sub, task := x.createSub(u, 1)
	accID := x.str(`SELECT id FROM access_versions WHERE subscription_id = $1`, sub)
	x.failApply(x.node, sub, agentproto.ApplyPortInUse)
	if st := x.str(`SELECT state || ':' || failure_code FROM tasks WHERE id = $1`, task); st != "failed:port_in_use" {
		t.Fatal(st)
	}
	v := x.sub(u, sub)
	if v.Lifecycle != "fulfill_failed" || v.Time.ExpiresAt != nil || v.Access != nil {
		t.Fatalf("不应显示就绪: %+v", v)
	}
	if x.int(`SELECT count(*) FROM ports WHERE access_version_id = $1`, accID) != 0 {
		t.Fatal("失败的接入端口应释放")
	}
	if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, res); st != "available" {
		t.Fatal(st)
	}
	if x.int(`SELECT count(*) FROM alerts WHERE kind = 'config_failed' AND status = 'open'`) == 0 {
		t.Fatal("缺配置失败告警")
	}
	if entry(x.desired(x.node), sub) != nil {
		t.Fatal("失败后不再下发")
	}
	x.wantBlockers(x.str(`SELECT user_id FROM subscriptions WHERE id = $1`, other), other)
	// 重试 → 节点不回报 → 超时回滚，端口进入冷却。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/fulfillment", map[string]any{}), 200, nil)
	acc2 := x.str(`SELECT id FROM access_versions WHERE subscription_id = $1 AND state = 'pending'`, sub)
	x.clk.Advance(3 * time.Minute)
	x.tick()
	if st := x.str(`SELECT state FROM access_versions WHERE id = $1`, acc2); st != "failed" {
		t.Fatal(st)
	}
	if x.int(`SELECT count(*) FROM ports WHERE access_version_id IS NULL AND state = 'cooldown' AND node_id = $1`, x.node) != 3 {
		t.Fatal("超时的端口应冷却")
	}
	// 再重试并成功。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/fulfillment", map[string]any{}), 200, nil)
	x.apply(x.node)
	x.wantBlockers(u, sub)
}

// A-43：耗尽但时长有效、保留期内都可换接入和换出口，换完仍按原阻断停用。
func TestA43_ChangesAllowedWhenExhaustedOrRetained(t *testing.T) {
	x := newH(t)
	u := x.newUser("a43@example.com")
	sub := x.activeSub(u)
	x.exhaust(sub)
	x.ok(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "ports_and_password"}, x.key()), 200, nil)
	x.apply(x.node)
	x.wantBlockers(u, sub, "traffic_exhausted")
	res, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
	p, _ := x.compare(sub, res, x.nextIP("us"))
	x.ok(x.exitChange(sub, res, p, false), 200, nil)
	x.apply(x.node)
	x.answerProbes(x.node, x.probeIP)
	x.wantBlockers(u, sub, "traffic_exhausted")
	x.clk.Set(x.expires(sub).Add(time.Second))
	x.tick()
	x.ok(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password"}, x.key()), 200, nil)
	x.apply(x.node)
	x.wantBlockers(u, sub, "expired", "traffic_exhausted")
}

// A-13：换接入、加流量、暂停并发；最终没有越权启用、没有双活接入、端口不重叠。
func TestA13_ConcurrentChangeTopUpPause(t *testing.T) {
	x := newH(t)
	u := x.newUser("a13@example.com")
	sub := x.activeSub(u)
	x.exhaust(sub)
	var o idOnly
	x.ok(x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "add_traffic", "subscriptionId": sub, "packId": x.pack}, x.key()), 201, &o)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "ports_and_password"}, x.key())
	}()
	go func() { defer wg.Done(); x.confirm(o.ID, 1000, "REF-13", false) }()
	go func() {
		defer wg.Done()
		x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/pause", map[string]any{"reason": "并发"})
	}()
	wg.Wait()
	d := x.desired(x.node)
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if e := entry(d, sub); e.Enabled {
		t.Fatal("暂停后不应启用")
	}
	x.apply(x.node)
	x.wantBlockers(u, sub, "admin_paused")
	if x.int(`SELECT count(*) FROM access_versions WHERE subscription_id = $1 AND state IN ('active', 'pending')`, sub) != 1 {
		t.Fatal("不应有双活接入")
	}
}
