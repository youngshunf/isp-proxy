package agent

// 上报快照（A-W7 的 agent 侧输入）与撤销接入。

import (
	"testing"
	"time"
)

func TestSnapshot(t *testing.T) {
	e := newEnv(t, 100*time.Millisecond)
	acc := e.provision("s2", 1<<40, time.Hour)
	e.provision("s1", 1<<40, time.Hour)
	for i := 0; i < 2; i++ {
		c, err := tunnel(protos[i], protos[i].addr(acc), acc.Username, acc.Password, e.canary.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	if _, _, err := fetch(pSOCKS5, acc, e.canary.addr, "/bytes?n=10000"); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return e.a.ConnCount("s2") == 2 }, "conn count")

	snap := e.a.Snapshot()
	if !snap.PersistOK || !e.a.PersistOK() || snap.At.IsZero() {
		t.Fatalf("node state: %+v", snap)
	}
	if len(snap.Subs) != 2 || snap.Subs[0].ID != "s1" || snap.Subs[1].ID != "s2" {
		t.Fatalf("subs not sorted by ID: %+v", snap.Subs)
	}
	s2 := snap.Subs[1]
	if s2.UsedTotal != e.a.Used("s2") || s2.UsedTotal < 10000 || s2.Conns != 2 || !s2.Usable ||
		s2.Version == 0 || s2.AccessVersion != 1 || s2.ExitVersion != e.exitA.Version {
		t.Fatalf("s2 snapshot: %+v", s2)
	}
	if s1 := snap.Subs[0]; s1.Conns != 0 || s1.UsedTotal != 0 {
		t.Fatalf("s1 snapshot: %+v", s1)
	}

	if err := e.a.ApplyExit("s2", e.exitB); err != nil {
		t.Fatal(err)
	}
	if v := e.a.Snapshot().Subs[1].ExitVersion; v != e.exitB.Version {
		t.Fatalf("exit version %d after swap", v)
	}

	e.ts.fail.Store(true)
	eventually(t, time.Second, func() bool { return !e.a.Snapshot().PersistOK }, "persist failure not reported")
	if e.a.Snapshot().Subs[1].Usable {
		t.Fatal("usable while persistence failing")
	}
	e.ts.fail.Store(false)
	eventually(t, time.Second, func() bool { return e.a.Snapshot().PersistOK }, "persist recovery not reported")
}

// 撤销接入（保留期满释放资源）：端口关闭、连接断开、凭据失效，累计用量保留。
func TestRevokeAccess(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<40, time.Hour)
	live, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	get(live, "/bytes?n=5000")
	used := e.a.Used("s1")

	if err := e.a.RevokeAccess("s1"); err != nil {
		t.Fatal(err)
	}
	if _, closed := waitClosed(live, stopLimit); !closed {
		t.Fatal("connection survived revoke")
	}
	for _, p := range protos {
		if _, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr); err == nil {
			t.Fatalf("%s: revoked access still works", p)
		}
	}
	snap := e.a.Snapshot().Subs[0]
	if snap.AccessVersion != 0 || snap.ExitVersion != 0 || snap.UsedTotal != used || snap.Conns != 0 {
		t.Fatalf("after revoke: %+v", snap)
	}
	if err := e.a.RevokeAccess("s1"); err != nil {
		t.Fatalf("revoke must be idempotent: %v", err)
	}
	if err := e.a.RevokeAccess("nope"); err != ErrUnknownSubscription {
		t.Fatalf("unknown sub: %v", err)
	}
	// 之后可以重新开通（续时长重新履约）。
	next := e.newAccess(2, "u-s1-v2")
	if err := e.a.ApplyAccess("s1", next, &e.exitA); err != nil {
		t.Fatal(err)
	}
	waitListening(t, next.HTTPAddr)
	if ip, _, err := fetch(pHTTP, next, e.canary.addr, "/ip"); err != nil || ip != "exit-A" {
		t.Fatalf("re-provision: %q %v", ip, err)
	}
}
