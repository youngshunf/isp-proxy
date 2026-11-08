package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/subscription"
)

// 服务器直出代理（D-25，docs/06 §9.2）的控制面验收：赠送、期望状态标记、跟随付费订阅、收回、各入口的拒绝。

type directSub struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Lifecycle string   `json:"lifecycle"`
	Blockers  []string `json:"blockers"`
	Plan      *struct {
		Name string `json:"name"`
	} `json:"plan"`
	Direct *struct {
		BandwidthMbps  int        `json:"bandwidthMbps"`
		MaxConnections int        `json:"maxConnections"`
		PaidUntil      *time.Time `json:"paidUntil"`
	} `json:"direct"`
	Time struct {
		ExpiresAt *time.Time `json:"expiresAt"`
	} `json:"time"`
	Traffic struct {
		TotalBytes int64 `json:"totalBytes"`
		UsedBytes  int64 `json:"usedBytes"`
	} `json:"traffic"`
	Exit   *struct{} `json:"exit"`
	Access *struct {
		Version  int    `json:"version"`
		Username string `json:"username"`
		Ports    ports  `json:"ports"`
	} `json:"access"`
}

func (x *h) userDirect(user string) subscription.UserDirect {
	x.t.Helper()
	v, err := x.c.Subscriptions.UserDirect(x.ctx, user)
	if err != nil {
		x.t.Fatal(err)
	}
	return v.(subscription.UserDirect)
}

func (x *h) grantDirect(user string, body map[string]any) directSub {
	x.t.Helper()
	var v directSub
	x.ok(x.do(x.asAdmin(), "PUT", "/admin-api/users/"+user+"/direct-proxy", body), 200, &v)
	return v
}

func (x *h) userDirectView(user string) directSub {
	x.t.Helper()
	var v directSub
	x.ok(x.do(asUser(user), "GET", "/api/subscriptions/"+x.str(`SELECT id FROM subscriptions WHERE user_id = $1 AND kind = 'direct'
	                                                              AND lifecycle <> 'deleted'`, user), nil), 200, &v)
	return v
}

func TestDirectGrantAndPaidFollow(t *testing.T) {
	x := newH(t)
	u := x.newUser("direct@example.com")

	// 未赠送时用户详情给出默认值（core 默认 10 Mbps、64 并发）。
	if d := x.userDirect(u); d.SubscriptionID != nil || d.DefaultBandwidthMbps != 10 || d.MaxConnections != 64 {
		t.Fatalf("未赠送: %+v", d)
	}

	g := x.grantDirect(u, map[string]any{"reason": "体验"})
	if g.Kind != "direct" || g.Plan != nil || g.Direct == nil || g.Direct.BandwidthMbps != 10 || g.Direct.MaxConnections != 64 {
		t.Fatalf("赠送结果: %+v", g)
	}
	if g.Lifecycle != "pending" || !x.audit.has("direct.grant") {
		t.Fatalf("赠送后应待节点应用并记审计: %+v", g)
	}
	if n := x.int(`SELECT count(*) FROM tasks WHERE subscription_id = $1 AND kind = 'direct_grant' AND state = 'running'`, g.ID); n != 1 {
		t.Fatalf("赠送任务 %d", n)
	}

	// 期望状态：显式直出、不限流量、没有出口；没有付费订阅时不启用。
	e := entry(x.desired(x.node), g.ID)
	if e == nil || !e.Direct || !e.Unmetered || e.Exit != nil || e.Enabled || e.MaxConns != 64 || e.BandwidthBps != 10_000_000 {
		t.Fatalf("直出条目: %+v", e)
	}
	x.apply(x.node)
	v := x.userDirectView(u)
	if v.Lifecycle != "active" || !eq(v.Blockers, []string{"no_paid_subscription"}) || v.Direct.PaidUntil != nil || v.Access == nil || v.Exit != nil {
		t.Fatalf("没有付费订阅时: %+v", v)
	}

	// 同一用户不能有第二个：再次赠送只调整带宽（幂等），库内唯一索引兜底。
	again := x.grantDirect(u, map[string]any{"bandwidthMbps": 20})
	if again.ID != g.ID || again.Direct.BandwidthMbps != 20 || !x.audit.has("direct.update") {
		t.Fatalf("再次赠送: %+v", again)
	}
	if _, err := x.pool.Exec(x.ctx, `INSERT INTO subscriptions (user_id, kind, rules_version, name, direct_bandwidth_mbps, direct_max_conns)
	                                  VALUES ($1, 'direct', 'R-2026-11-09', 'x', 1, 1)`, u); !store.IsUniqueViolation(err, "subscriptions_one_direct") {
		t.Fatalf("库内应拒绝第二个直出订阅: %v", err)
	}
	if e := entry(x.desired(x.node), g.ID); e.BandwidthBps != 20_000_000 {
		t.Fatalf("带宽调整应下发: %+v", e)
	}
	x.fail(x.do(x.asAdmin(), "PUT", "/admin-api/users/"+u+"/direct-proxy", map[string]any{"bandwidthMbps": 0}), 422, "")

	// 开通付费订阅：直出跟随其到期时间启用。
	before := entry(x.desired(x.node), g.ID).Version
	paid := x.activeSub(u)
	paidExp := x.sub(u, paid).Time.ExpiresAt
	e = entry(x.desired(x.node), g.ID)
	if !e.Enabled || !e.ExpiresAt.Equal(*paidExp) || e.Version <= before {
		t.Fatalf("开通付费订阅后直出应启用至 %v: %+v", paidExp, e)
	}
	if isp := entry(x.desired(x.node), paid); isp.Direct || isp.Unmetered || isp.Exit == nil {
		t.Fatalf("ISP 订阅不得标记直出: %+v", isp)
	}
	v = x.userDirectView(u)
	if len(v.Blockers) != 0 || v.Direct.PaidUntil == nil || !v.Direct.PaidUntil.Equal(*paidExp) || !v.Time.ExpiresAt.Equal(*paidExp) {
		t.Fatalf("有付费订阅时: %+v", v)
	}

	// 多个付费订阅取最晚到期；续时长后延长。
	x.renew(u, paid)
	renewed := x.sub(u, paid).Time.ExpiresAt
	if e := entry(x.desired(x.node), g.ID); !e.ExpiresAt.Equal(*renewed) {
		t.Fatalf("续时长后直出截止应为 %v，得到 %v", renewed, e.ExpiresAt)
	}
	second := x.activeSub(u)
	if e := entry(x.desired(x.node), g.ID); !e.ExpiresAt.Equal(*renewed) {
		t.Fatalf("较早到期的第二个订阅 %s 不应缩短截止: %v", second, e.ExpiresAt)
	}

	// 用量照常累计，超过任何套餐流量也不阻断。
	x.apply(x.node, map[string]uint64{g.ID: uint64(50 * gb)})
	v = x.userDirectView(u)
	if v.Traffic.UsedBytes != 50*gb || v.Traffic.TotalBytes != 0 || len(v.Blockers) != 0 {
		t.Fatalf("不限流量: %+v", v)
	}

	// 付费订阅全部到期：直出阻断为 no_paid_subscription；期望状态的截止仍是最晚到期（节点据此判定已到期）。
	x.clk.Set(renewed.Add(time.Second))
	x.tick()
	v = x.userDirectView(u)
	if !eq(v.Blockers, []string{"no_paid_subscription"}) || v.Direct.PaidUntil != nil {
		t.Fatalf("付费到期后: %+v", v)
	}
	if e := entry(x.desired(x.node), g.ID); !e.ExpiresAt.Equal(*renewed) {
		t.Fatalf("到期后截止: %+v", e)
	}
	ver := entry(x.desired(x.node), g.ID).Version
	x.renew(u, paid)
	if e := entry(x.desired(x.node), g.ID); e.Version <= ver || !e.ExpiresAt.After(x.clk.Now()) {
		t.Fatalf("续时长后直出应恢复: %+v", e)
	}
	x.apply(x.node)

	// 换接入规则同 ISP 订阅。
	acc := x.userDirectView(u).Access
	x.ok(x.do(asUser(u), "POST", "/api/subscriptions/"+g.ID+"/access-changes", map[string]any{"mode": "ports_and_password"}, x.key()), 200, nil)
	x.apply(x.node)
	if a := x.userDirectView(u).Access; a.Version != acc.Version+1 || a.Ports == acc.Ports {
		t.Fatalf("换接入: %+v → %+v", acc, a)
	}

	// 直出不能续时长、加流量、换出口、调账、走通用删除。
	x.fail(x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "renew_time", "subscriptionId": g.ID, "months": 1}, x.key()), 412, "")
	x.fail(x.do(asUser(u), "POST", "/api/orders", map[string]any{"kind": "add_traffic", "subscriptionId": g.ID, "packId": x.pack}, x.key()), 412, "")
	x.fail(x.do(asUser(u), "POST", "/api/exit-requests", map[string]any{"subscriptionId": g.ID, "reason": "quality"}, x.key()), 412, "")
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+g.ID+"/ledger-entries",
		map[string]any{"sourceType": "gift", "entitlementKind": "traffic", "amount": gb, "reason": "x"}, x.key()), 412, "")
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+g.ID+"/deletion", map[string]any{"reason": "x"}), 412, "")
	if _, err := x.pool.Exec(x.ctx, `INSERT INTO ledger_entries (subscription_id, entitlement_kind, amount, source_type, source_id, reason, admin_id)
	                                  VALUES ($1, 'traffic', 1, 'gift', 'x', 'x', $2)`, g.ID, x.admin); err == nil ||
		!strings.Contains(err.Error(), "D-25") {
		t.Fatalf("库内应拒绝直出订阅的账本条目: %v", err)
	}

	// 用户详情给出赠送状态；管理端列表可按类型筛选。
	if d := x.userDirect(u); d.SubscriptionID == nil || *d.SubscriptionID != g.ID || *d.BandwidthMbps != 20 || *d.Lifecycle != "active" {
		t.Fatalf("用户详情: %+v", d)
	}
	var page struct {
		Items []directSub `json:"items"`
	}
	x.ok(x.do(x.asAdmin(), "GET", "/admin-api/subscriptions?kind=direct&userId="+u, nil), 200, &page)
	if len(page.Items) != 1 || page.Items[0].ID != g.ID || page.Items[0].Plan != nil {
		t.Fatalf("按类型筛选: %+v", page.Items)
	}
	x.ok(x.do(x.asAdmin(), "GET", "/admin-api/subscriptions?kind=isp&userId="+u, nil), 200, &page)
	if len(page.Items) != 2 {
		t.Fatalf("ISP 订阅: %+v", page.Items)
	}

	// 收回：订阅删除、凭据撤销、端口冷却，期望状态里不再有该条目（节点移除监听）。
	cur := x.userDirectView(u).Access.Ports
	x.ok(x.do(x.asAdmin(), "DELETE", "/admin-api/users/"+u+"/direct-proxy?reason=到期不续", nil), 204, nil)
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, g.ID); lc != "deleted" || !x.audit.has("direct.revoke") {
		t.Fatalf("收回后 %s", lc)
	}
	if entry(x.desired(x.node), g.ID) != nil {
		t.Fatal("收回后不应再下发")
	}
	if n := x.int(`SELECT count(*) FROM access_versions WHERE subscription_id = $1 AND state <> 'revoked'`, g.ID); n != 0 {
		t.Fatalf("未撤销的接入 %d", n)
	}
	if n := x.int(`SELECT count(*) FROM ports WHERE node_id = $1 AND port = ANY($2) AND state = 'cooldown'`, x.node, cur.list()); n != 3 {
		t.Fatalf("冷却端口 %d", n)
	}
	// 收回后可再次赠送（新订阅），再次收回幂等。
	if again := x.grantDirect(u, nil); again.ID == g.ID {
		t.Fatal("再次赠送应是新订阅")
	}
	x.ok(x.do(x.asAdmin(), "DELETE", "/admin-api/users/"+u+"/direct-proxy", nil), 204, nil)
	x.ok(x.do(x.asAdmin(), "DELETE", "/admin-api/users/"+u+"/direct-proxy", nil), 204, nil)
}

// 开通失败（节点下发失败）后可重试；进行中的开通被收回时回滚。封禁停用直出。
func TestDirectGrantFailureRetryAndBan(t *testing.T) {
	x := newH(t)
	u := x.newUser("d2@example.com")
	x.activeSub(u)
	g := x.grantDirect(u, map[string]any{"bandwidthMbps": 5})
	x.failApply(x.node, g.ID, agentproto.ApplyPortInUse)
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, g.ID); lc != "fulfill_failed" {
		t.Fatalf("下发失败后 %s", lc)
	}
	r := x.grantDirect(u, map[string]any{"bandwidthMbps": 5})
	if r.ID != g.ID || r.Lifecycle != "pending" || !x.audit.has("direct.retry") {
		t.Fatalf("重试: %+v", r)
	}
	x.apply(x.node)
	if v := x.userDirectView(u); v.Lifecycle != "active" || len(v.Blockers) != 0 {
		t.Fatalf("重试后: %+v", v)
	}

	// 封禁：直出与 ISP 订阅一起停用。
	tx, err := x.pool.Begin(x.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(x.ctx, `UPDATE users SET status = 'banned', banned_at = $2, banned_reason = 'x' WHERE id = $1`, u, x.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if err := x.c.UserStatusChanged(x.ctx, tx, u); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(x.ctx); err != nil {
		t.Fatal(err)
	}
	if e := entry(x.desired(x.node), g.ID); e.Enabled {
		t.Fatal("封禁后直出应停用")
	}

	// 进行中的开通被收回：任务失败回滚，端口冷却。
	u2 := x.newUser("d3@example.com")
	g2 := x.grantDirect(u2, nil)
	x.ok(x.do(x.asAdmin(), "DELETE", "/admin-api/users/"+u2+"/direct-proxy", nil), 204, nil)
	if st := x.str(`SELECT state FROM tasks WHERE subscription_id = $1`, g2.ID); st != "failed" {
		t.Fatalf("进行中的开通应失败，得到 %s", st)
	}
	if n := x.int(`SELECT count(*) FROM alerts WHERE dedupe_key = $1 AND status <> 'resolved'`, "fulfill_failed:"+g2.ID); n != 0 {
		t.Fatalf("收回后不应留开通失败告警: %d", n)
	}
}

// 没有可用节点时不能赠送。
func TestDirectGrantNeedsNode(t *testing.T) {
	x := newH(t)
	u := x.newUser("d4@example.com")
	x.exec(`UPDATE nodes SET status = 'disabled'`)
	x.fail(x.do(x.asAdmin(), "PUT", "/admin-api/users/"+u+"/direct-proxy", map[string]any{}), 412, "")
	x.fail(x.do(x.asAdmin(), "PUT", "/admin-api/users/"+x.newUser("tmp@example.com")[:4]+"nope/direct-proxy", map[string]any{}), 404, "")
}

// 直出不显示出口 IP，只显示节点上由管理员填写的出口地域（D-25 补充）。
func TestDirectRegionFromNode(t *testing.T) {
	x := newH(t)
	u := x.newUser("region@example.com")
	g := x.grantDirect(u, map[string]any{})
	x.apply(x.node)

	type region struct {
		CountryCode string `json:"countryCode"`
		Name        string `json:"name"`
	}
	type regionView struct {
		Direct struct {
			Region *region `json:"region"`
		} `json:"direct"`
		Exit *struct{} `json:"exit"`
	}
	view := func() regionView {
		t.Helper()
		var v regionView
		x.ok(x.do(asUser(u), "GET", "/api/subscriptions/"+g.ID, nil), 200, &v)
		return v
	}
	if v := view(); v.Direct.Region != nil || v.Exit != nil {
		t.Fatalf("节点未填地域时应为 null 且不显示出口: %+v", v)
	}

	patch := func(body map[string]any) resp { return x.do(x.asAdmin(), "PATCH", "/admin-api/nodes/"+x.node, body) }
	// 校验：代码两位字母、名称 1～32 字、对象内不接受未知字段。
	x.fail(patch(map[string]any{"directRegion": map[string]any{"countryCode": "sgp", "name": "新加坡"}}), 422, "")
	x.fail(patch(map[string]any{"directRegion": map[string]any{"countryCode": "sg", "name": " "}}), 422, "")
	x.fail(patch(map[string]any{"directRegion": map[string]any{"countryCode": "sg", "name": "新加坡", "ip": "1.2.3.4"}}), 422, "")

	before := entry(x.desired(x.node), g.ID).Version
	var n struct {
		DirectRegion *region `json:"directRegion"`
	}
	x.ok(patch(map[string]any{"directRegion": map[string]any{"countryCode": " SG ", "name": " 新加坡 "}}), 200, &n)
	if n.DirectRegion == nil || *n.DirectRegion != (region{"sg", "新加坡"}) || !x.audit.has("node.update") {
		t.Fatalf("设置地域: %+v", n.DirectRegion)
	}
	if v := view(); v.Direct.Region == nil || *v.Direct.Region != (region{"sg", "新加坡"}) {
		t.Fatalf("直出视图应显示节点地域: %+v", v.Direct.Region)
	}
	var list []struct {
		ID     string `json:"id"`
		Direct *struct {
			Region *region `json:"region"`
		} `json:"direct"`
	}
	x.ok(x.do(asUser(u), "GET", "/api/subscriptions", nil), 200, &list)
	for _, s := range list {
		if s.ID == g.ID && (s.Direct == nil || s.Direct.Region == nil || *s.Direct.Region != (region{"sg", "新加坡"})) {
			t.Fatalf("列表里的直出订阅也应显示地域: %+v", s.Direct)
		}
	}
	// 只用于展示：不改期望状态版本。
	if e := entry(x.desired(x.node), g.ID); e.Version != before {
		t.Fatalf("改地域不应生成新期望版本: %d → %d", before, e.Version)
	}
	// 不带 directRegion 的补丁不改它；null 清除。
	x.ok(patch(map[string]any{"name": "node-a2"}), 200, &n)
	if n.DirectRegion == nil {
		t.Fatal("未提交 directRegion 时不应清除")
	}
	x.ok(patch(map[string]any{"directRegion": nil}), 200, &n)
	if n.DirectRegion != nil || view().Direct.Region != nil {
		t.Fatalf("null 应清除地域: %+v", n.DirectRegion)
	}
}
