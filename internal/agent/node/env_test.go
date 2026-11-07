package node

// 节点运行时测试的环境：伪控制面（agentproto.NewHandler + 开发证书）驱动运行时，
// 伪 ISP 上游（http / https / socks5）各连到一个带标签的目标端点；客户端请求的目标地址指向
// 标签为 DIRECT 的金丝雀端点，只要发生直连就会被观察到。全部在本机回环上。

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gost/core/logger"
	xlogger "github.com/go-gost/x/logger"

	"github.com/go-gost/gost/internal/agent/agenttest"
	"github.com/go-gost/gost/internal/agentproto"
)

const (
	stopLimit = time.Second // D-17：停用 ≤ 1 秒
	nodeID    = "nod_test"
	// 回显端点给出的“出口 IP”（TEST-NET-3）。
	ipA, ipB, ipS, ipH = "203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4"
)

func TestMain(m *testing.M) {
	logger.SetDefault(xlogger.Nop())
	os.Exit(m.Run())
}

func testLogger() *slog.Logger {
	if os.Getenv("ISP_AGENT_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type env struct {
	t      *testing.T
	ctl    *agenttest.Control
	cfg    *Config
	rt     *Runtime
	canary *agenttest.Backend
	// 出口：A、B 为 http 上游，S 为 socks5 上游，H 为 https 上游。
	upA, upB, upS, upH         *agenttest.Upstream
	exitA, exitB, exitS, exitH agentproto.Exit
	ver                        atomic.Uint64 // 订阅版本发号器（全部订阅共用，单调递增即可）
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t}
	e.ctl = agenttest.StartControl(t, nodeID, 2*time.Second)
	e.canary = agenttest.StartBackend(t, "DIRECT", "192.0.2.99")
	backA := agenttest.StartBackend(t, "exit-A", ipA)
	backB := agenttest.StartBackend(t, "exit-B", ipB)
	backS := agenttest.StartBackend(t, "exit-S", ipS)
	backH := agenttest.StartBackend(t, "exit-H", ipH)
	e.upA = agenttest.StartUpstream(t, agenttest.UpstreamHTTP, backA.Addr, "ua", "pa")
	e.upB = agenttest.StartUpstream(t, agenttest.UpstreamHTTP, backB.Addr, "ub", "pb")
	e.upS = agenttest.StartUpstream(t, agenttest.UpstreamSOCKS5, backS.Addr, "us", "ps")
	e.upH = agenttest.StartUpstream(t, agenttest.UpstreamHTTPS, backH.Addr, "uh", "ph")
	e.exitA = agentproto.Exit{Version: 1, Protocol: agentproto.ExitHTTP, Addr: e.upA.Addr, Username: "ua", Password: "pa"}
	e.exitB = agentproto.Exit{Version: 2, Protocol: agentproto.ExitHTTP, Addr: e.upB.Addr, Username: "ub", Password: "pb"}
	e.exitS = agentproto.Exit{Version: 3, Protocol: agentproto.ExitSOCKS5, Addr: e.upS.Addr, Username: "us", Password: "ps"}
	e.exitH = agentproto.Exit{Version: 4, Protocol: agentproto.ExitHTTPS, Addr: e.upH.Addr, Username: "uh", Password: "ph"}

	certFile, keyFile := agenttest.CertFiles(t, t.TempDir(), "proxy.test")
	node := e.ctl.NodeCert()
	e.cfg = &Config{
		NodeID: nodeID,
		Control: ControlConfig{
			URL: e.ctl.URL(), CAFile: e.ctl.Certs.CA.Cert, CertFile: node.Cert, KeyFile: node.Key,
		},
		DataDir:        t.TempDir(),
		Entry:          EntryConfig{BindHost: "127.0.0.1", CertFile: certFile, KeyFile: keyFile},
		Egress:         EgressConfig{AllowCIDRs: []string{"127.0.0.0/8"}},
		UpstreamTLS:    UpstreamTLSConfig{CAFile: e.upH.CAFile},
		Probe:          ProbeConfig{EchoURL: "http://" + e.canary.Addr + "/echo", Timeout: 3 * time.Second},
		ReportInterval: 300 * time.Millisecond,
		FlushInterval:  100 * time.Millisecond,
	}
	if err := e.cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	e.start()
	t.Cleanup(func() { e.stop() })
	return e
}

func (e *env) start() {
	e.t.Helper()
	rt, err := Start(e.cfg, Options{AgentVersion: "test", Logger: testLogger(), RetryAfterReject: 200 * time.Millisecond})
	if err != nil {
		e.t.Fatal(err)
	}
	e.rt = rt
}

func (e *env) stop() {
	if e.rt != nil {
		e.rt.Close()
		e.rt = nil
	}
}

// restart 模拟节点进程重启：关闭运行时（最终落盘），用同一数据目录重新启动。
func (e *env) restart() {
	e.t.Helper()
	e.stop()
	e.start()
}

// access 生成一个使用空闲端口的接入。
func (e *env) access(ver uint64, user string) *agentproto.Access {
	p := agenttest.FreePorts(e.t, 3)
	return &agentproto.Access{Version: ver, Username: user, Password: "pw-" + user,
		Ports: agentproto.Ports{HTTP: p[0], HTTPS: p[1], SOCKS5: p[2]}}
}

// entry 生成一个启用的订阅条目（新版本号）。
func (e *env) entry(id string, entitled uint64, ttl time.Duration, acc *agentproto.Access, exit *agentproto.Exit) agentproto.SubscriptionEntry {
	return agentproto.SubscriptionEntry{
		ID: id, Version: e.ver.Add(1), EntitledTotal: entitled, ExpiresAt: time.Now().Add(ttl).UTC(), Enabled: true,
		Access: acc, Exit: exit,
	}
}

// put 在期望状态中新增或替换订阅条目，并等待节点上报已应用该版本。
func (e *env) put(entries ...agentproto.SubscriptionEntry) {
	e.t.Helper()
	e.putNoWait(entries...)
	for _, en := range entries {
		e.waitApplied(en.ID, en.Version)
	}
}

func (e *env) putNoWait(entries ...agentproto.SubscriptionEntry) {
	e.ctl.Update(func(d *agentproto.Desired) {
		for _, en := range entries {
			replaced := false
			for i := range d.Subscriptions {
				if d.Subscriptions[i].ID == en.ID {
					d.Subscriptions[i] = en
					replaced = true
				}
			}
			if !replaced {
				d.Subscriptions = append(d.Subscriptions, en)
			}
		}
	})
}

// remove 从期望状态中删除订阅。
func (e *env) remove(id string) {
	e.ctl.Update(func(d *agentproto.Desired) {
		out := d.Subscriptions[:0]
		for _, en := range d.Subscriptions {
			if en.ID != id {
				out = append(out, en)
			}
		}
		d.Subscriptions = out
	})
}

// current 返回期望状态中的订阅条目。
func (e *env) current(id string) agentproto.SubscriptionEntry {
	e.t.Helper()
	d, err := e.ctl.Desired(context.Background(), nodeID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, en := range d.Subscriptions {
		if en.ID == id {
			return en
		}
	}
	e.t.Fatalf("subscription %s not in desired state", id)
	return agentproto.SubscriptionEntry{}
}

// bump 复制期望状态中的条目、改动后以新版本下发并等待应用。
func (e *env) bump(id string, mut func(en *agentproto.SubscriptionEntry)) agentproto.SubscriptionEntry {
	e.t.Helper()
	en := e.current(id)
	mut(&en)
	en.Version = e.ver.Add(1)
	e.put(en)
	return en
}

func (e *env) waitApplied(id string, ver uint64) agentproto.SubscriptionReport {
	e.t.Helper()
	rep := e.ctl.WaitReport(e.t, 5*time.Second, func(r *agentproto.Report) bool {
		s, ok := agenttest.SubIn(r, id)
		return ok && s.AppliedVersion >= ver
	})
	s, _ := agenttest.SubIn(&rep, id)
	return s
}

func addr(port int) string { return agenttest.Loopback(port) }

func entryAddr(p agenttest.Proto, a *agentproto.Access) string {
	return addr([...]int{a.Ports.HTTP, a.Ports.HTTPS, a.Ports.SOCKS5}[p])
}

func (e *env) fetch(p agenttest.Proto, a *agentproto.Access, path string) (string, int64, error) {
	return agenttest.Fetch(p, entryAddr(p, a), a.Username, a.Password, e.canary.Addr, path)
}

func (e *env) tunnel(p agenttest.Proto, a *agentproto.Access) (*agenttest.Conn, error) {
	return agenttest.Tunnel(p, entryAddr(p, a), a.Username, a.Password, e.canary.Addr)
}

func (e *env) mustTunnel(p agenttest.Proto, a *agentproto.Access) *agenttest.Conn {
	e.t.Helper()
	c, err := e.tunnel(p, a)
	if err != nil {
		e.t.Fatalf("%s tunnel: %v", p, err)
	}
	e.t.Cleanup(func() { c.Close() })
	return c
}

// expectExit 断言三种协议都经出口 label 到达目标。
func (e *env) expectExit(a *agentproto.Access, label string) {
	e.t.Helper()
	for _, p := range agenttest.Protos {
		body, _, err := e.fetch(p, a, "/ip")
		if err != nil || body != label {
			e.t.Fatalf("%s via %d: got %q err %v, want %s", p, a.Ports.HTTP, body, err, label)
		}
	}
}

// expectRejected 断言三种协议都无法用 a 建立隧道。
func (e *env) expectRejected(a *agentproto.Access, what string) {
	e.t.Helper()
	for _, p := range agenttest.Protos {
		if c, err := e.tunnel(p, a); err == nil {
			c.Close()
			e.t.Fatalf("%s: %s still accepted", p, what)
		}
	}
}

func (e *env) noDirect() {
	e.t.Helper()
	if n := e.canary.Hits.Load(); n != 0 {
		e.t.Fatalf("target reached directly %d times", n)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
