package core_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func (x *h) expires(sub string) time.Time {
	x.t.Helper()
	var t time.Time
	if err := x.pool.QueryRow(x.ctx, `SELECT expires_at FROM subscriptions WHERE id = $1`, sub).Scan(&t); err != nil {
		x.t.Fatal(err)
	}
	return t.UTC()
}

func (x *h) exhaust(sub string) {
	x.t.Helper()
	total := x.int(`SELECT entitled_bytes_total FROM subscriptions WHERE id = $1`, sub)
	x.apply(x.node, map[string]uint64{sub: uint64(total)})
}

func (x *h) wantBlockers(user, sub string, want ...string) subView {
	x.t.Helper()
	v := x.sub(user, sub)
	if want == nil {
		want = []string{}
	}
	if !eq(v.Blockers, want) {
		x.t.Fatalf("阻断原因期望 %v，得到 %v", want, v.Blockers)
	}
	e := entry(x.desired(x.node), sub)
	enabled := e != nil && e.Enabled
	// 到期与耗尽由节点按 expires_at / entitled_total 执行；控制面的 enabled 只反映其余开关。
	gate := true
	for _, b := range want {
		if b != "traffic_exhausted" && b != "expired" && b != "changing" {
			gate = false
		}
	}
	if b := len(want) > 0 && x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, sub) == "retained"; b {
		gate = false
	}
	if enabled != gate {
		x.t.Fatalf("enabled 期望 %v，得到 %v（阻断 %v）", gate, enabled, want)
	}
	return v
}

// A-33：有效期内提前续时长接在原到期之后；纯续时长不加流量。
func TestA33_EarlyRenewAppendsToExpiry(t *testing.T) {
	x := newH(t)
	u := x.newUser("a33@example.com")
	sub := x.activeSub(u)
	e0 := x.expires(sub)
	x.clk.Advance(5 * 24 * time.Hour)
	x.renew(u, sub)
	if got := x.expires(sub); !got.Equal(e0.Add(30 * 24 * time.Hour)) {
		t.Fatalf("应接在原到期之后: %v → %v", e0, got)
	}
	if b := x.int(`SELECT entitled_bytes_total FROM subscriptions WHERE id = $1`, sub); b != 10*gb {
		t.Fatalf("纯续时长不应加流量: %d", b)
	}
	if de := entry(x.desired(x.node), sub); !de.ExpiresAt.Equal(e0.Add(30 * 24 * time.Hour)) {
		t.Fatal("期望状态应带新的到期时间")
	}
}

// A-29 / A-31：到期但有流量 → 停用、余额保留、进入保留期；保留期内只买流量仍停用，续时长恢复原出口与原接入。
func TestA29_A31_ExpiryRetentionAndRenewRestoresSameAccessAndExit(t *testing.T) {
	x := newH(t)
	u := x.newUser("a29@example.com")
	sub := x.activeSub(u)
	other := x.activeSub(x.newUser("other29@example.com"))
	acc0 := x.str(`SELECT current_access_id FROM subscriptions WHERE id = $1`, sub)
	exit0 := x.str(`SELECT current_exit_id FROM subscriptions WHERE id = $1`, sub)
	x.apply(x.node, map[string]uint64{sub: uint64(3 * gb)})
	e0 := x.expires(sub)
	x.clk.Set(e0.Add(time.Second))
	// 调度尚未运行：期望状态里的 expires_at 已过，节点按本地计时器停用（A-35）；阻断原因已经是 expired。
	if de := entry(x.desired(x.node), sub); de.ExpiresAt.After(x.clk.Now()) {
		t.Fatal("expires_at 应已过")
	}
	x.tick()
	v := x.wantBlockers(u, sub, "expired")
	if v.Lifecycle != "retained" || v.Time.RetentionEndsAt == nil || !v.Time.RetentionEndsAt.Equal(e0.Add(7*24*time.Hour)) {
		t.Fatalf("%+v", v)
	}
	if v.Traffic.UsedBytes != 3*gb || v.Traffic.TotalBytes != 10*gb {
		t.Fatal("余额应保留")
	}
	x.wantBlockers(x.str(`SELECT user_id FROM subscriptions WHERE id = $1`, other), other, "expired") // 同时到期
	// 只买流量：仍然到期停用。
	x.clk.Advance(2 * 24 * time.Hour)
	x.addTraffic(u, sub)
	x.wantBlockers(u, sub, "expired")
	// 续时长：恢复为有效，原接入与原出口不变，从现在起算 30 天（停用空档不计）。
	x.renew(u, sub)
	v = x.wantBlockers(u, sub)
	if v.Lifecycle != "active" || !x.expires(sub).Equal(x.clk.Now().Add(30*24*time.Hour)) {
		t.Fatalf("续时长应从现在起算: %v", x.expires(sub))
	}
	if x.str(`SELECT current_access_id FROM subscriptions WHERE id = $1`, sub) != acc0 || x.str(`SELECT current_exit_id FROM subscriptions WHERE id = $1`, sub) != exit0 {
		t.Fatal("保留期内续时长应恢复原接入与原出口")
	}
	if v.Traffic.UsedBytes != 3*gb || v.Traffic.TotalBytes != 20*gb {
		t.Fatalf("%+v", v.Traffic)
	}
}

// A-30 / A-32 / A-34：耗尽 → 买流量恢复且到期不变；只续时长不能恢复；两项都用尽需两项补足；余额不清零、不重复发放。
func TestA30_A32_A34_TrafficAndTimeAreIndependent(t *testing.T) {
	x := newH(t)
	u := x.newUser("a30@example.com")
	sub := x.activeSub(u)
	x.exhaust(sub)
	x.wantBlockers(u, sub, "traffic_exhausted")
	if x.int(`SELECT count(*) FROM subscription_events WHERE subscription_id = $1 AND kind = 'exhausted'`, sub) != 1 {
		t.Fatal("缺耗尽事件")
	}
	e0 := x.expires(sub)
	x.renew(u, sub)
	x.wantBlockers(u, sub, "traffic_exhausted")
	e1 := x.expires(sub)
	if !e1.Equal(e0.Add(30 * 24 * time.Hour)) {
		t.Fatal(e1)
	}
	x.addTraffic(u, sub)
	x.wantBlockers(u, sub)
	if !x.expires(sub).Equal(e1) {
		t.Fatal("买流量不改变到期时间")
	}
	// A-32：两项都用尽。
	x.exhaust(sub)
	x.clk.Set(e1.Add(time.Minute))
	x.tick()
	x.wantBlockers(u, sub, "expired", "traffic_exhausted")
	x.addTraffic(u, sub)
	x.wantBlockers(u, sub, "expired")
	x.exhaust(sub)
	x.renew(u, sub)
	x.wantBlockers(u, sub, "traffic_exhausted")
	x.addTraffic(u, sub)
	x.wantBlockers(u, sub)
	// A-34：套餐流量只在新购时发放一次（10 GB），其余都来自流量包；已用只增不减。
	if b := x.int(`SELECT entitled_bytes_total FROM subscriptions WHERE id = $1`, sub); b != 40*gb {
		t.Fatalf("流量合计 %d", b)
	}
	used := x.int(`SELECT used_bytes_total FROM subscriptions WHERE id = $1`, sub)
	x.apply(x.node, map[string]uint64{sub: 1})
	if x.int(`SELECT used_bytes_total FROM subscriptions WHERE id = $1`, sub) != used {
		t.Fatal("累计已用不能回退")
	}
}

// A-44 / A-43：保留期满释放资源、余额保留；已释放不可更换；续时长后分配新出口和新接入，余额原样可用。
func TestA44_ReleaseThenRenewRefulfillsWithNewExitAndAccess(t *testing.T) {
	x := newH(t)
	u := x.newUser("a44@example.com")
	sub := x.activeSub(u)
	x.apply(x.node, map[string]uint64{sub: uint64(4 * gb)})
	oldRes := x.str(`SELECT b.resource_id FROM subscriptions s JOIN exit_bindings b ON b.id = s.current_exit_id WHERE s.id = $1`, sub)
	oldAcc := x.str(`SELECT current_access_id FROM subscriptions WHERE id = $1`, sub)
	x.clk.Set(x.expires(sub).Add(time.Second))
	x.tick()
	x.clk.Advance(7*24*time.Hour + time.Second)
	x.tick()
	v := x.wantBlockers(u, sub, "released")
	if v.Lifecycle != "released" || v.Access != nil || v.Exit != nil || v.Traffic.UsedBytes != 4*gb || v.Traffic.TotalBytes != 10*gb {
		t.Fatalf("%+v", v)
	}
	if entry(x.desired(x.node), sub) != nil {
		t.Fatal("已释放的订阅不应出现在期望状态")
	}
	if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, oldRes); st != "cooldown" {
		t.Fatal(st)
	}
	if x.int(`SELECT count(*) FROM ports WHERE access_version_id = $1 AND state = 'allocated'`, oldAcc) != 0 {
		t.Fatal("旧端口应释放进入冷却")
	}
	x.fail(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password"}, x.key()), 412, "precondition")
	newRes := x.resource()
	x.clk.Advance(time.Hour)
	order := x.renew(u, sub)
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, sub); lc != "pending" {
		t.Fatalf("续时长后应重新履约: %s", lc)
	}
	x.apply(x.node)
	v = x.wantBlockers(u, sub)
	if x.str(`SELECT b.resource_id FROM subscriptions s JOIN exit_bindings b ON b.id = s.current_exit_id WHERE s.id = $1`, sub) != newRes {
		t.Fatal("应分配新出口")
	}
	if x.str(`SELECT current_access_id FROM subscriptions WHERE id = $1`, sub) == oldAcc || x.str(`SELECT change_kind FROM access_versions a
	      JOIN subscriptions s ON s.current_access_id = a.id WHERE s.id = $1`, sub) != "refulfill" {
		t.Fatal("应签发新接入")
	}
	if v.Traffic.UsedBytes != 4*gb || v.Traffic.TotalBytes != 10*gb || !x.expires(sub).Equal(x.clk.Now().Add(30*24*time.Hour)) {
		t.Fatalf("余额原样，重新就绪起算: %+v %v", v.Traffic, x.expires(sub))
	}
	if st := x.str(`SELECT fulfillment_status FROM orders WHERE id = $1`, order); st != "delivered" {
		t.Fatal(st)
	}
}

// A-45：已释放订阅余额全额划转到同用户另一订阅：成对条目、总额守恒；不能划给其他用户；来源必须已释放。
func TestA45_TransferReleasedBalance(t *testing.T) {
	x := newH(t)
	u := x.newUser("a45@example.com")
	from := x.activeSub(u)
	to := x.activeSub(u)
	stranger := x.activeSub(x.newUser("stranger@example.com"))
	x.apply(x.node, map[string]uint64{from: uint64(4 * gb)})
	body := map[string]any{"fromSubscriptionId": from, "toSubscriptionId": to, "reason": "合并"}
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/transfers", body, x.key()), 412, "precondition") // 来源未释放
	x.clk.Set(x.expires(from).Add(8 * 24 * time.Hour))
	x.tick()
	x.tick()
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/transfers", map[string]any{"fromSubscriptionId": from, "toSubscriptionId": stranger,
		"reason": "x"}, x.key()), 412, "precondition")
	before := x.int(`SELECT sum(entitled_bytes_total - used_bytes_total) FROM subscriptions WHERE id IN ($1, $2)`, from, to)
	var tr struct {
		Bytes int64 `json:"bytes"`
	}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/transfers", body, x.key()), 201, &tr)
	if tr.Bytes != 6*gb {
		t.Fatal(tr.Bytes)
	}
	after := x.int(`SELECT sum(entitled_bytes_total - used_bytes_total) FROM subscriptions WHERE id IN ($1, $2)`, from, to)
	if before != after || x.int(`SELECT entitled_bytes_total - used_bytes_total FROM subscriptions WHERE id = $1`, from) != 0 {
		t.Fatalf("总额应守恒: %d → %d", before, after)
	}
	if x.int(`SELECT sum(amount) FROM ledger_entries WHERE transfer_id IS NOT NULL`) != 0 || x.int(`SELECT count(*) FROM ledger_entries WHERE transfer_id IS NOT NULL`) != 2 {
		t.Fatal("应为成对条目")
	}
}

// A-09：耗尽后买流量恢复；管理员暂停、用户封禁、节点停用时买流量仍不放行。封禁在平台事务内生效并唤醒节点。
func TestA09_TopUpDoesNotBypassPauseBanOrNode(t *testing.T) {
	x := newH(t)
	u := x.newUser("a09@example.com")
	sub := x.activeSub(u)
	x.exhaust(sub)
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/pause", map[string]any{"reason": "核查"}), 200, nil)
	x.addTraffic(u, sub)
	x.wantBlockers(u, sub, "admin_paused")
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/resume", nil), 200, nil)
	x.wantBlockers(u, sub)

	// 封禁：平台在自己的事务里改用户状态并调用 hook；提交后 1 秒内节点被唤醒。
	ch := x.c.Hub().Changed(x.node)
	v0 := x.int(`SELECT desired_version FROM subscriptions WHERE id = $1`, sub)
	err := pgx.BeginFunc(x.ctx, x.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(x.ctx, `UPDATE users SET status = 'banned', banned_at = now() WHERE id = $1`, u); err != nil {
			return err
		}
		return x.c.UserStatusChanged(context.Background(), tx, u)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("封禁后 1 秒内应唤醒节点")
	}
	if x.int(`SELECT desired_version FROM subscriptions WHERE id = $1`, sub) <= v0 {
		t.Fatal("封禁应生成新期望版本")
	}
	x.wantBlockers(u, sub, "account_banned")
	x.fail(x.do(asUser(u), "POST", "/api/subscriptions/"+sub+"/access-changes", map[string]any{"mode": "password"}, x.key()), 412, "precondition")
	err = pgx.BeginFunc(x.ctx, x.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(x.ctx, `UPDATE users SET status = 'active', banned_at = NULL WHERE id = $1`, u); err != nil {
			return err
		}
		return x.c.UserStatusChanged(context.Background(), tx, u)
	})
	if err != nil {
		t.Fatal(err)
	}
	x.wantBlockers(u, sub)
	// 节点停用：数据面异常。
	x.ok(x.do(x.asAdmin(), "PATCH", "/admin-api/nodes/"+x.node, map[string]any{"status": "disabled"}), 200, nil)
	x.wantBlockers(u, sub, "dataplane")
}
