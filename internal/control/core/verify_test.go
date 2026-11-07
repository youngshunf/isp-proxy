package core_test

import (
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
)

// 换出口与组合更换的切换后检测（R-11.6、A-42）：节点应用新出口后，经新绑定做一次出口检测（走节点探测通道），
// 通过才报告成功；失败则整体回到原接入与原出口，候选资源隔离并告警，任务失败并写明原因。

// verifyProbe 返回订阅当前进行中任务的切换后检测探测（exit_check，绑定为任务的新出口）。
func (x *h) verifyProbe(sub string) string {
	x.t.Helper()
	return x.str(`SELECT COALESCE((SELECT p.id FROM tasks t JOIN probes p ON p.id = t.verify_probe_id
	                 WHERE t.subscription_id = $1 AND t.state = 'running' AND p.kind = 'exit_check' AND p.exit_binding_id = t.new_exit_id), '')`, sub)
}

// answerVerify 让切换后检测成功（ok=true，观测到候选的 IP）或失败；其他探测照常回答。
func (x *h) answerVerify(sub string, ok bool) {
	x.t.Helper()
	id := x.verifyProbe(sub)
	if id == "" {
		x.t.Fatal("没有切换后检测")
	}
	x.answerProbes(x.node, func(p agentproto.Probe) (string, bool) {
		if p.ID == id && !ok {
			return "", false
		}
		return x.probeIP(p)
	})
}

func TestA42_PostApplyExitCheckGatesSuccess(t *testing.T) {
	for _, combined := range []bool{false, true} {
		name := "exit_change"
		if combined {
			name = "combined"
		}
		t.Run(name, func(t *testing.T) {
			x := newH(t)
			u := x.newUser("v42@example.com")
			sub := x.activeSub(u)
			before := x.sub(u, sub)
			oldExit := x.str(`SELECT current_exit_id FROM subscriptions WHERE id = $1`, sub)
			oldAccess := x.str(`SELECT current_access_id FROM subscriptions WHERE id = $1`, sub)

			section := func(s string) { t.Log("== " + s) }
			section("检测失败 → 回滚")
			res1, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
			p, _ := x.compare(sub, res1, x.nextIP("us"))
			var task struct{ ID, State, Kind string }
			x.ok(x.exitChange(sub, res1, p, combined), 200, &task)
			x.apply(x.node)
			x.apply(x.node) // 重复回报不重复下发检测
			if st := x.str(`SELECT state FROM tasks WHERE id = $1`, task.ID); st != "running" {
				t.Fatalf("节点应用后、检测前任务应仍在进行：%s", st)
			}
			if n := x.int(`SELECT count(*) FROM probes p JOIN tasks t ON t.verify_probe_id = p.id WHERE t.id = $1`, task.ID); n != 1 {
				t.Fatalf("切换后检测应恰好 1 个：%d", n)
			}
			if n := x.int(`SELECT count(*) FROM probes WHERE kind = 'exit_check' AND subscription_id = $1 AND status IN ('queued', 'dispatched')`, sub); n != 1 {
				t.Fatalf("待执行的出口检测应只有切换后检测：%d", n)
			}
			if probe := x.verifyProbe(sub); probe == "" {
				t.Fatal("切换后检测应经新绑定")
			}
			x.wantBlockers(u, sub, "changing")
			x.answerVerify(sub, false)
			var code, reason string
			if err := x.pool.QueryRow(x.ctx, `SELECT state, failure_code, failure_reason FROM tasks WHERE id = $1`, task.ID).Scan(&task.State, &code, &reason); err != nil {
				t.Fatal(err)
			}
			if task.State != "failed" || code != "exit_check_failed" || reason == "" {
				t.Fatalf("检测失败应让任务失败：%s %s %s", task.State, code, reason)
			}
			if x.str(`SELECT current_exit_id FROM subscriptions WHERE id = $1`, sub) != oldExit ||
				x.str(`SELECT current_access_id FROM subscriptions WHERE id = $1`, sub) != oldAccess {
				t.Fatal("当前出口与接入应保持原样")
			}
			if st := x.str(`SELECT state FROM exit_bindings WHERE resource_id = $1`, res1); st != "failed" {
				t.Fatalf("新绑定应失败：%s", st)
			}
			if st := x.str(`SELECT status FROM upstream_resources WHERE id = $1`, res1); st != "quarantine" {
				t.Fatalf("检测失败的资源应隔离：%s", st)
			}
			if x.int(`SELECT count(*) FROM alerts WHERE kind = 'exit_change_failed' AND status = 'open'`) != 1 {
				t.Fatal("缺换出口失败告警")
			}
			de := entry(x.desired(x.node), sub)
			if de.Exit.Version != uint64(before.Exit.Version) || de.Access.Version != uint64(before.Access.Version) ||
				de.Access.Password != before.Access.Password {
				t.Fatal("失败后期望状态应整体回到原接入与原出口")
			}
			if combined {
				// 节点已经监听过新端口：失败后新端口进入冷却，不立即给别人。
				if n := x.int(`SELECT count(*) FROM ports p JOIN access_versions a ON a.subscription_id = $1 AND a.state = 'failed'
				                WHERE p.state = 'free' AND p.port IN (a.http_port, a.https_port, a.socks5_port)`, sub); n != 0 {
					t.Fatalf("已监听过的新端口不应立即释放：%d", n)
				}
			}
			x.apply(x.node)
			if v := x.wantBlockers(u, sub); v.Access.Version != before.Access.Version || v.Exit.Version != before.Exit.Version ||
				v.Access.Ports != before.Access.Ports {
				t.Fatal("原接入与原出口应继续可用")
			}

			section("检测通过 → 成功")
			res2, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
			newIP := x.nextIP("us")
			p, _ = x.compare(sub, res2, newIP)
			x.ok(x.exitChange(sub, res2, p, combined), 200, &task)
			x.apply(x.node)
			x.answerVerify(sub, true)
			if st := x.str(`SELECT state FROM tasks WHERE id = $1`, task.ID); st != "succeeded" {
				t.Fatalf("检测通过后任务应成功：%s", st)
			}
			x.apply(x.node)
			v := x.wantBlockers(u, sub)
			if v.Exit.IP != newIP || (combined && v.Access.Version == before.Access.Version) || (!combined && v.Access.Version != before.Access.Version) {
				t.Fatalf("%+v", v)
			}
			if x.int(`SELECT count(*) FROM alerts WHERE kind = 'exit_change_failed' AND status = 'open'`) != 0 {
				t.Fatal("成功后告警应关闭")
			}

			section("检测超时 → 回滚")
			res3, _ := x.newResource(x.node, "us", "洛杉矶", "pending_verify")
			p, _ = x.compare(sub, res3, x.nextIP("us"))
			cur := x.str(`SELECT current_exit_id FROM subscriptions WHERE id = $1`, sub)
			x.ok(x.exitChange(sub, res3, p, combined), 200, &task)
			x.apply(x.node)
			x.clk.Advance(2*time.Minute + time.Second)
			x.tick()
			if err := x.pool.QueryRow(x.ctx, `SELECT state, failure_code FROM tasks WHERE id = $1`, task.ID).Scan(&task.State, &code); err != nil {
				t.Fatal(err)
			}
			if task.State != "failed" || code != "exit_check_timeout" {
				t.Fatalf("检测超时应失败：%s %s", task.State, code)
			}
			if x.str(`SELECT current_exit_id FROM subscriptions WHERE id = $1`, sub) != cur {
				t.Fatal("超时后应保持原出口")
			}
		})
	}
}
