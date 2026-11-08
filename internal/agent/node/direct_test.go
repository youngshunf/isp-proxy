package node

import (
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agent/agenttest"
	"github.com/go-gost/gost/internal/agentproto"
)

// D-25：期望状态显式标记直出与不限流量的条目经节点本机拨号、额度为 0 仍可用且照常上报用量；
// 同一份期望状态里没有出口、未标记直出的条目仍失败即拒绝。重启后按 desired.json 恢复同样的行为。
func TestDirectEntryAndNoExitEntry(t *testing.T) {
	e := newEnv(t)
	dAcc := e.access(1, "u-direct")
	direct := e.entry("direct", 0, time.Hour, dAcc, nil)
	direct.Direct, direct.Unmetered = true, true
	nAcc := e.access(1, "u-noexit")
	noExit := e.entry("noexit", 1<<30, time.Hour, nAcc, nil)
	e.put(direct, noExit)

	check := func(when string) {
		t.Helper()
		before := e.canary.Hits.Load()
		for _, p := range agenttest.Protos {
			if body, _, err := e.fetch(p, dAcc, "/ip"); err != nil || body != "DIRECT" {
				t.Fatalf("%s direct %s: %q %v", when, p, body, err)
			}
			if body, _, err := e.fetch(p, nAcc, "/ip"); err == nil {
				t.Fatalf("%s no-exit %s: got %q, want fail-closed", when, p, body)
			}
		}
		if got := e.canary.Hits.Load() - before; got != int64(len(agenttest.Protos)) {
			t.Fatalf("%s: canary hits %d, want only the %d direct requests", when, got, len(agenttest.Protos))
		}
	}
	check("initial")
	body, n, err := e.fetch(agenttest.HTTP, dAcc, "/bytes?n=3000000")
	if err != nil || len(body) != 3_000_000 {
		t.Fatalf("unmetered download: %d %v", len(body), err)
	}
	agenttest.Eventually(t, 3*time.Second, func() bool { return e.ctl.Used("direct") >= uint64(n) },
		"direct usage not reported: %d < %d", e.ctl.Used("direct"), n)
	e.restart()
	check("after restart")

	// 撤销直出标记（变为没有出口的普通条目）：立即失败即拒绝。
	e.bump("direct", func(en *agentproto.SubscriptionEntry) { en.Direct = false })
	for _, p := range agenttest.Protos {
		if body, _, err := e.fetch(p, dAcc, "/ip"); err == nil {
			t.Fatalf("%s: unflagged entry still dials directly: %q", p, body)
		}
	}
}
