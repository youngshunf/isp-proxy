//go:build ispe2e

package isp

import (
	"context"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 服务器直出代理（D-25）：A-50～A-52。直出订阅经节点本机拨号，目标侧金丝雀（标签 DIRECT）看到的就是直连；
// 任何 ISP 假上游的 Accepted 计数都不应增加。只访问回环目标（agent egress 放行 127.0.0.0/8）。

// GrantDirect 管理员在用户详情勾选赠送直出代理（mbps 为 0 时取默认带宽）。
func (a *Admin) GrantDirect(userID string, mbps int) AdminSubscription {
	a.t.Helper()
	body := map[string]any{"reason": "E2E 赠送"}
	if mbps > 0 {
		body["bandwidthMbps"] = mbps
	}
	var s AdminSubscription
	a.Must("PUT", "/users/"+userID+"/direct-proxy", body, &s)
	return s
}

// RevokeDirect 取消勾选（收回）。
func (a *Admin) RevokeDirect(userID string) {
	a.t.Helper()
	a.Must("DELETE", "/users/"+userID+"/direct-proxy?reason="+url.QueryEscape("E2E 收回"), nil, nil)
}

// UserDirect 读用户详情里的直出代理状态。
func (a *Admin) UserDirect(userID string) AdminUserDirect {
	a.t.Helper()
	var d struct {
		Direct AdminUserDirect `json:"direct"`
	}
	a.Get("/users/"+userID, &d)
	return d.Direct
}

// upstreamAccepted 是全部假 ISP 上游累计接受的连接数。
func (e *Env) upstreamAccepted() int64 {
	var n int64
	for _, x := range e.Exits {
		n += x.Up.Accepted.Load()
	}
	return n
}

// MustReachDirect 断言四种接入方式都经节点本机直连到达目标：金丝雀看到请求，没有任何 ISP 上游被使用。
func (e *Env) MustReachDirect(c Creds) {
	e.t.Helper()
	up, hits := e.upstreamAccepted(), e.Canary.IPHits.Load()
	e.MustReach(c, "DIRECT")
	if got := e.upstreamAccepted(); got != up {
		e.t.Fatalf("直出请求经过了 ISP 上游：Accepted %d → %d", up, got)
	}
	if got := e.Canary.IPHits.Load() - hits; got < int64(len(AllModes)) {
		e.t.Fatalf("目标侧只看到 %d 次直连请求", got)
	}
}

// WaitDirectUsable 等待直出订阅可用：blockers 为空且入口能直连到达目标。
func (e *Env) WaitDirectUsable(u *User, id string) Subscription {
	e.t.Helper()
	s := e.WaitSub(u, id, 30*time.Second, func(x *Subscription) bool {
		return len(x.Blockers) == 0 && x.Access != nil && x.Lifecycle == "active"
	}, "直出可用")
	e.Eventually(15*time.Second, func() bool {
		body, _, err := Fetch(Connect, CredsOf(s.Access), e.Target(), "/ip")
		return err == nil && body == "DIRECT"
	}, "直出订阅 %s 显示可用，但入口不通", id)
	return s
}

// A-50 管理员为用户勾选赠送直出代理（带宽上限可设）：三种协议都能用，目标看到的是中转服务器直连而不是任何 ISP 上游；
// 同一用户不能有第二个；收回后 ≤ 1 秒停用。
func TestA50_AdminGrantsDirectProxy(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()
	paid := e.Provision(u, ProvisionOpts{})

	section(t, "未赠送")
	if d := e.Admin.UserDirect(u.ID); d.SubscriptionID != nil || d.DefaultBandwidthMbps != 10 || d.MaxConnections != 64 {
		t.Fatalf("未赠送时用户详情：%+v", d)
	}

	section(t, "赠送（带宽 30 Mbps）")
	g := e.Admin.GrantDirect(u.ID, 30)
	if g.Kind != "direct" || g.Direct == nil || g.Direct.BandwidthMbps != 30 || g.Direct.MaxConnections != 64 {
		t.Fatalf("赠送结果：%+v", g)
	}
	s := e.WaitDirectUsable(u, g.ID)
	if s.Kind != "direct" || s.Plan != nil || s.Exit != nil || s.Direct == nil || s.Direct.PaidUntil == nil ||
		!s.Direct.PaidUntil.Equal(*paid.Time.ExpiresAt) {
		t.Fatalf("用户视图：%+v", s)
	}
	c := CredsOf(s.Access)
	e.MustReachDirect(c)
	// ISP 订阅照旧经上游出站，不受影响。
	e.MustReach(CredsOf(paid.Access), e.CurrentExitLabel(paid.ID))
	if d := e.Admin.UserDirect(u.ID); d.SubscriptionID == nil || *d.SubscriptionID != g.ID || deref(d.BandwidthMbps) != 30 {
		t.Fatalf("赠送后用户详情：%+v", d)
	}

	section(t, "同一用户不能有第二个")
	again := e.Admin.GrantDirect(u.ID, 30)
	n := 0
	for _, x := range u.Subs() {
		if x.Kind == "direct" {
			n++
		}
	}
	if again.ID != g.ID || n != 1 {
		t.Fatalf("再次赠送应仍是同一个直出订阅：%s vs %s，共 %d 个", again.ID, g.ID, n)
	}

	section(t, "收回后 ≤ 1 秒停用")
	conns := e.idleAll(c)
	at := time.Now()
	e.Admin.RevokeDirect(u.ID)
	e.closedNear(conns, at, "收回")
	e.MustReject(c, "收回后")
	for _, x := range u.Subs() {
		if x.ID == g.ID {
			t.Fatal("收回后用户仍能看到直出订阅")
		}
	}
	if d := e.Admin.UserDirect(u.ID); d.SubscriptionID != nil {
		t.Fatalf("收回后用户详情：%+v", d)
	}
	e.MustReach(CredsOf(paid.Access), e.CurrentExitLabel(paid.ID))
}

// A-51 直出代理不限流量但限带宽：用量超过任何套餐流量仍可用，速率受带宽上限约束；用量照常累计展示；
// 25、465、587 端口与私网目标被拒。
func TestA51_DirectUnmeteredButRateLimited(t *testing.T) {
	t.Parallel()
	const mbps = 8 // 1 MB/s
	e := NewEnv(t, EnvOptions{Exits: 1, Plan: func(p *PlanVersionInput) { p.IncludedBytes = 1 * MB }})
	u := e.NewUser()
	e.Provision(u, ProvisionOpts{})
	g := e.Admin.GrantDirect(u.ID, mbps)
	s := e.WaitDirectUsable(u, g.ID)
	c := CredsOf(s.Access)

	section(t, "超过套餐流量仍可用，速率受限")
	start := time.Now()
	got, _, err := Download(Connect, c, e.Target(), 3*MB, 30*time.Second)
	if err != nil || got != 3*MB {
		t.Fatalf("直出下载 %d：%v", got, err)
	}
	el := time.Since(start)
	rate := float64(got) / el.Seconds()
	t.Logf("直出下载 %d 字节用时 %v（%.0f B/s，上限 %d Mbps）", got, el.Round(time.Millisecond), rate, mbps)
	if limit := float64(mbps) * 1e6 / 8; rate > limit*1.3 {
		t.Fatalf("速率 %.0f B/s 超过带宽上限 %.0f B/s 的 1.3 倍", rate, limit)
	}
	used := e.SettledUsed(g.ID)
	x := u.Sub(g.ID)
	if used < 3*MB || x.Traffic.UsedBytes < 3*MB || x.Traffic.TotalBytes != 0 || len(x.Blockers) != 0 {
		t.Fatalf("不限流量、照常计量：used=%d %+v %v", used, x.Traffic, x.Blockers)
	}
	e.MustReachDirect(c)

	section(t, "出站目标限制")
	// 能绑定时在回环上真的监听 465/587：若直出没封这两个端口就会连上，能更确定地区分“被策略拒绝”与“目标没监听”。
	listening := map[string]*acceptCounter{}
	for _, p := range []string{"465", "587"} {
		if l, err := net.Listen("tcp", "127.0.0.1:"+p); err == nil {
			ac := &acceptCounter{l: l}
			go ac.run()
			t.Cleanup(func() { l.Close() })
			listening["127.0.0.1:"+p] = ac
		} else {
			t.Logf("无法在 127.0.0.1:%s 监听（%v），只断言被拒", p, err)
		}
	}
	hits := e.Canary.IPHits.Load()
	for _, target := range []string{"127.0.0.1:25", "127.0.0.1:465", "127.0.0.1:587", "10.0.0.1:80", "192.168.1.1:80",
		"172.16.0.1:80", "169.254.169.254:80", "[fd00::1]:80"} {
		for _, m := range AllModes {
			body, _, err := Fetch(m, c, target, "/ip")
			if err == nil {
				t.Fatalf("%s 到 %s 应被拒，却得到 %q", m, target, body)
			}
			if !IsRejected(err) {
				t.Fatalf("%s 到 %s 应明确拒绝：%v", m, target, err)
			}
		}
		if ac := listening[target]; ac != nil && ac.n.Load() != 0 {
			t.Fatalf("直出连上了 %s（%d 次）", target, ac.n.Load())
		}
	}
	if e.Canary.IPHits.Load() != hits {
		t.Fatal("被拒的目标不应到达金丝雀")
	}
	e.MustReachDirect(c)
}

// A-52 直出代理跟随付费订阅：没有时长有效的付费订阅时不可用并提示原因；付费订阅到期 ≤ 1 秒停用，续时长后恢复；
// 有多个付费订阅时以最晚到期为准；用户被封禁时停用；ISP 订阅缺出口链时仍拒绝而不是直连。
func TestA52_DirectFollowsPaidSubscriptions(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()

	section(t, "没有付费订阅")
	g := e.Admin.GrantDirect(u.ID, 0)
	s := e.WaitSub(u, g.ID, 30*time.Second, func(x *Subscription) bool {
		return x.Lifecycle == "active" && x.Access != nil && x.Has("no_paid_subscription")
	}, "开通且阻断 no_paid_subscription")
	if s.Direct == nil || s.Direct.PaidUntil != nil || s.Direct.BandwidthMbps != 10 {
		t.Fatalf("没有付费订阅时：%+v", s.Direct)
	}
	e.WaitApplied(g.ID)
	c := CredsOf(s.Access)
	e.MustReject(c, "没有付费订阅")

	section(t, "开通付费订阅后可用")
	p1 := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[0].ResourceID})
	s = e.WaitDirectUsable(u, g.ID)
	e.MustReachDirect(c)

	section(t, "多个付费订阅以最晚到期为准")
	p2 := e.Provision(u, ProvisionOpts{ResourceID: e.Exits[1].ResourceID})
	e.Admin.ExpireIn(p2.ID, 40*24*time.Hour)
	s = e.WaitSub(u, g.ID, 10*time.Second, func(x *Subscription) bool {
		return x.Direct != nil && x.Direct.PaidUntil != nil && x.Direct.PaidUntil.Equal(*e.Admin.Sub(p2.ID).Time.ExpiresAt)
	}, "截止跟随最晚到期的付费订阅")
	exp1 := e.Admin.ExpireIn(p1.ID, 2*time.Second)
	e.WaitSub(u, p1.ID, 15*time.Second, func(x *Subscription) bool { return x.Has("expired") }, "第一个付费订阅到期")
	if time.Now().Before(exp1.Add(time.Second)) {
		time.Sleep(time.Until(exp1.Add(time.Second)))
	}
	e.MustReachDirect(c)

	section(t, "付费订阅全部到期 ≤ 1 秒停用")
	conns := e.idleAll(c)
	exp2 := e.Admin.ExpireIn(p2.ID, 3*time.Second)
	e.WaitApplied(g.ID)
	e.closedNear(conns, exp2, "付费订阅到期")
	e.MustReject(c, "付费订阅到期后")
	e.WaitSub(u, g.ID, 10*time.Second, func(x *Subscription) bool { return x.Has("no_paid_subscription") }, "阻断 no_paid_subscription")

	section(t, "续时长后恢复")
	e.Renew(u, OrderInput{Kind: "renew_time", SubscriptionID: p2.ID, Months: 1})
	e.WaitUsable(u, p2.ID)
	e.WaitDirectUsable(u, g.ID)
	e.MustReachDirect(c)

	section(t, "封禁停用，解封恢复")
	e.Admin.Must("POST", "/users/"+u.ID+"/ban", map[string]string{"reason": "A-52"}, nil)
	e.Eventually(5*time.Second, func() bool { _, err := e.Reach(Connect, c); return IsRejected(err) }, "封禁后直出仍可用")
	e.MustReject(c, "封禁")
	e.Admin.Must("POST", "/users/"+u.ID+"/unban", nil, nil)
	if err := u.Login(); err != nil { // 封禁撤销了会话
		t.Fatal(err)
	}
	e.WaitDirectUsable(u, g.ID)
	e.MustReachDirect(c)

	section(t, "ISP 订阅缺出口链时仍拒绝而不是直连")
	// 控制面不会为 ISP 订阅生成“没有出口”的启用条目；这里在库里摘掉出口模拟故障，确认同一用户虽有直出代理，
	// 这个 ISP 订阅也只会被拒绝，不会落到直连（agent 层“启用但无出口”的组合见 internal/agent 的单元测试）。
	pc := CredsOf(u.Sub(p2.ID).Access)
	hits := e.Canary.IPHits.Load()
	ctx := context.Background()
	db, err := pgx.Connect(ctx, e.DBURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if _, err := db.Exec(ctx, `WITH s AS (UPDATE subscriptions SET current_exit_id = NULL, desired_version = desired_version + 1
	                              WHERE id = $1 RETURNING node_id)
	                            UPDATE nodes SET desired_revision = desired_revision + 1 WHERE id = (SELECT node_id FROM s)`, p2.ID); err != nil {
		t.Fatal(err)
	}
	e.Eventually(15*time.Second, func() bool { _, err := e.Reach(Connect, pc); return IsRejected(err) }, "摘掉出口后 ISP 订阅仍可用")
	e.MustReject(pc, "ISP 订阅缺出口")
	if got := e.Canary.IPHits.Load(); got != hits {
		t.Fatalf("ISP 订阅缺出口时落到了直连（金丝雀 %d → %d）", hits, got)
	}
	e.MustReachDirect(c)
}

type acceptCounter struct {
	l net.Listener
	n atomic.Int64
}

func (a *acceptCounter) run() {
	for {
		c, err := a.l.Accept()
		if err != nil {
			return
		}
		a.n.Add(1)
		c.Close()
	}
}
