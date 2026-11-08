package agent

// D-25 服务器直出代理的数据面（docs/04 §6.1）：只有显式标记直出的订阅经节点本机拨号，仍经出站策略且另封
// 465、587；不限流量的订阅不做额度耗尽停用但照常计量；带宽、并发、期限照常执行；没有出口、未标记直出的
// ISP 订阅照旧失败即拒绝（失败即拒绝不被直出破坏）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// provisionDirect 下发一个不限流量的直出订阅并开通接入。
func (e *env) provisionDirect(id string, ttl time.Duration) Access {
	e.t.Helper()
	e.a.Upsert(SubState{ID: id, Version: e.nextVer.Add(1), ExpiresAt: time.Now().Add(ttl), Enabled: true, Unmetered: true})
	acc := e.newAccess(1, "u-"+id)
	if err := e.a.ReplaceAccessDirect(id, acc); err != nil {
		e.t.Fatal(err)
	}
	waitListening(e.t, acc.HTTPAddr, acc.HTTPSAddr, acc.SOCKS5Addr)
	return acc
}

func forwardClient(acc Access) *http.Client {
	proxyURL := &url.URL{Scheme: "http", User: url.UserPassword(acc.Username, acc.Password), Host: acc.HTTPAddr}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}, Timeout: 3 * time.Second}
}

// 直出订阅：三种入口（含普通 HTTP 转发）都由节点直接连到目标，任何上游都收不到连接；用量照常计入。
func TestDirectDialsTargetLocally(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provisionDirect("d1", time.Hour)
	if !e.a.IsDirect("d1") {
		t.Fatal("IsDirect = false")
	}
	if _, ok := e.a.CurrentExit("d1"); ok {
		t.Fatal("direct subscription reports an upstream exit")
	}
	accepted := e.upA.accepted.Load() + e.upB.accepted.Load()
	for _, p := range protos {
		body, _, err := fetch(p, acc, e.canary.addr, "/ip")
		if err != nil || body != "DIRECT" {
			t.Fatalf("%s: got %q %v, want DIRECT", p, body, err)
		}
	}
	resp, err := forwardClient(acc).Get("http://" + e.canary.addr + "/ip")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "DIRECT" {
		t.Fatalf("forward: %q", b)
	}
	if n := e.upA.accepted.Load() + e.upB.accepted.Load() - accepted; n != 0 {
		t.Fatalf("direct traffic reached an upstream %d times", n)
	}
	if got := e.canary.hits.Load(); got != 4 {
		t.Fatalf("canary hits %d, want 4", got)
	}
	if e.a.Used("d1") == 0 {
		t.Fatal("direct traffic not metered")
	}

	// 换接入沿用直出（ApplyAccess 不带出口）。
	acc2 := e.newAccess(2, "u-d1b")
	if err := e.a.ApplyAccess("d1", acc2, nil); err != nil {
		t.Fatal(err)
	}
	waitListening(t, acc2.HTTPAddr)
	if body, _, err := fetch(pSOCKS5, acc2, e.canary.addr, "/ip"); err != nil || body != "DIRECT" {
		t.Fatalf("after access change: %q %v", body, err)
	}
}

// 直出另封 465、587（R-13.6）；25、私网、元数据照封；ISP 订阅不受 465、587 影响。
// 主机名只解析一次并按解析结果拨号（IP 固定）。
func TestDirectEgressPolicy(t *testing.T) {
	r := testResolver()
	r.m["mail.test"] = []string{"93.184.216.34"}
	r.m["canary.test"] = []string{"127.0.0.1"}
	pol := EgressPolicy{Resolver: r.lookup}
	for _, a := range []string{"8.8.8.8:465", "8.8.8.8:587", "mail.test:465", "mail.test:587", "8.8.8.8:25", "10.0.0.1:80",
		"169.254.169.254:80", "[fd00:ec2::254]:80", "metadata.google.internal:80", "mixed.test:443", "127.0.0.1:80"} {
		if err := pol.CheckDirect(context.Background(), "tcp", a); !errors.Is(err, ErrEgressBlocked) {
			t.Errorf("direct %s: not blocked (%v)", a, err)
		}
	}
	for _, a := range []string{"8.8.8.8:465", "8.8.8.8:587", "mail.test:443"} {
		if err := pol.Check(context.Background(), "tcp", a); err != nil {
			t.Errorf("isp %s: blocked: %v", a, err)
		}
	}
	if err := pol.CheckDirect(context.Background(), "tcp", "mail.test:443"); err != nil {
		t.Errorf("direct mail.test:443: %v", err)
	}
	// 显式例外对直出同样有效（管理员授权的端口）。
	exc := EgressPolicy{Resolver: r.lookup, AllowPorts: []int{587}}
	if err := exc.CheckDirect(context.Background(), "tcp", "8.8.8.8:587"); err != nil {
		t.Errorf("allow_ports not honoured for direct: %v", err)
	}

	e := newEnv(t, time.Second)
	e.a.SetEgressPolicy(EgressPolicy{Resolver: r.lookup, AllowCIDRs: loopbackEgress.AllowCIDRs})
	acc := e.provisionDirect("d1", time.Hour)
	isp := e.provision("s1", 1<<40, time.Hour)

	// 465、587 在拨号前就被拒：主机名不会被解析，客户端得到明确失败。
	before := r.calls.Load()
	for _, p := range protos {
		for _, target := range []string{"mail.test:465", "mail.test:587", "mail.test:25", "10.0.0.1:80", "169.254.169.254:80"} {
			c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, target)
			if err == nil {
				c.Close()
				t.Fatalf("%s direct %s: accepted", p, target)
			}
			want := "503"
			if p == pSOCKS5 {
				want = "socks5: reply"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s direct %s: %v, want explicit failure", p, target, err)
			}
		}
	}
	if n := r.calls.Load() - before; n != 0 {
		t.Fatalf("blocked port targets were resolved %d times", n)
	}
	// 主机名按节点解析的结果拨号：canary.test 只有测试解析器认识，能到达说明拨的是这次解析的地址。
	_, port, _ := strings.Cut(e.canary.addr, ":")
	if body, _, err := fetch(pHTTP, acc, "canary.test:"+port, "/ip"); err != nil || body != "DIRECT" {
		t.Fatalf("direct via resolved name: %q %v", body, err)
	}
	// ISP 订阅访问 465 照常经上游（伪上游忽略目标）。
	if body, _, err := fetch(pSOCKS5, isp, "198.51.100.7:465", "/ip"); err != nil || body != "exit-A" {
		t.Fatalf("isp 465: %q %v", body, err)
	}
}

// 不限流量：额度为 0 也可用，用量远超任何额度仍不停用，照常计量；期限、启用、带宽、并发照常执行。
func TestUnmeteredDirectKeepsLimitsButNoQuota(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provisionDirect("d1", time.Hour)
	const n = 2 << 20
	var client int64
	for _, p := range protos {
		body, got, err := fetch(p, acc, e.canary.addr, "/bytes?n=2097152")
		if err != nil || len(body) != n {
			t.Fatalf("%s: %d bytes %v", p, len(body), err)
		}
		client += got
	}
	if !e.a.Usable("d1") {
		t.Fatal("unmetered subscription became unusable")
	}
	if used := e.a.Used("d1"); used < uint64(client) {
		t.Fatalf("used %d < client bytes %d", used, client)
	}

	// 带宽上限 1 MB/s：下载 2 MB 至少约 1.5 秒（含一次突发）。
	e.update(SubState{ID: "d1", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, Unmetered: true, BandwidthBps: 1 << 20})
	start := time.Now()
	if body, _, err := fetch(pHTTP, acc, e.canary.addr, "/bytes?n=2097152"); err != nil || len(body) != n {
		t.Fatalf("limited download: %d %v", len(body), err)
	}
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Fatalf("bandwidth cap not enforced: 2 MiB in %v", d)
	}

	// 并发上限 1：第二条连接认证失败。
	e.update(SubState{ID: "d1", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, Unmetered: true, MaxConns: 1})
	c1, err := tunnel(pSOCKS5, acc.SOCKS5Addr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	if c2, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr); err == nil {
		c2.Close()
		t.Fatal("max_conns not enforced for unmetered subscription")
	}
	c1.Close()

	// 期限：到点 ≤ 1 秒断开空闲隧道，此后拒绝新连接。
	exp := time.Now().Add(1500 * time.Millisecond)
	e.update(SubState{ID: "d1", ExpiresAt: exp, Enabled: true, Unmetered: true})
	idle, err := tunnel(pHTTPS, acc.HTTPSAddr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(exp))
	if d, closed := waitClosed(idle, time.Second); !closed {
		t.Fatalf("expiry not enforced for unmetered subscription (%v)", d)
	}
	if _, _, err := fetch(pHTTP, acc, e.canary.addr, "/ip"); err == nil {
		t.Fatal("expired unmetered subscription still accepted")
	}

	// 取消不限流量后按额度（0）执行：不可用。
	e.update(SubState{ID: "d1", ExpiresAt: time.Now().Add(time.Hour), Enabled: true})
	if e.a.Usable("d1") {
		t.Fatal("metered subscription with zero quota usable")
	}
}

// 回归（R-01.3）：同一节点上有直出订阅时，没有出口、未标记直出的 ISP 订阅仍然失败即拒绝，不会直连；
// 清空出口的 ISP 订阅同样拒绝；直出订阅改回 ISP 出口后走上游。
func TestNoExitWithoutDirectStillFailsClosed(t *testing.T) {
	e := newEnv(t, time.Second)
	directTarget := startBackend(t, "DIRECT-OK")
	d := e.provisionDirect("d1", time.Hour)
	if body, _, err := fetch(pHTTP, d, directTarget.addr, "/ip"); err != nil || body != "DIRECT-OK" {
		t.Fatalf("direct: %q %v", body, err)
	}

	// 没有出口的 ISP 订阅（期望状态 access 非空、exit 为空、direct 为 false）。
	e.a.Upsert(SubState{ID: "s0", Version: e.nextVer.Add(1), EntitledTotal: 1 << 40, ExpiresAt: time.Now().Add(time.Hour), Enabled: true})
	noExit := e.newAccess(1, "u-s0")
	if err := e.a.ReplaceAccess("s0", noExit, nil); err != nil {
		t.Fatal(err)
	}
	waitListening(t, noExit.HTTPAddr)
	if e.a.IsDirect("s0") {
		t.Fatal("subscription without exit became direct")
	}
	// 有出口的 ISP 订阅随后清空出口。
	isp := e.provision("s1", 1<<40, time.Hour)
	if err := e.a.ClearExit("s1"); err != nil {
		t.Fatal(err)
	}
	if err := e.a.ApplyAccess("s0", e.newAccess(2, "u-s0b"), nil); !errors.Is(err, ErrNoExit) {
		t.Fatalf("ApplyAccess without exit on no-exit subscription: %v, want ErrNoExit", err)
	}
	for _, acc := range []Access{noExit, isp} {
		for _, p := range protos {
			if body, _, err := fetch(p, acc, e.canary.addr, "/ip"); err == nil {
				t.Fatalf("%s %s without exit: got %q", acc.Username, p, body)
			}
		}
		resp, err := forwardClient(acc).Get("http://" + e.canary.addr + "/ip")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("%s forward without exit served", acc.Username)
			}
		}
	}
	if n := e.canary.hits.Load(); n != 0 {
		t.Fatalf("subscriptions without exit reached the target directly %d times", n)
	}

	// 直出订阅换回 ISP 出口：此后经上游（目标侧看到 exit-A），不再直出。
	if err := e.a.ApplyExit("d1", e.exitA); err != nil {
		t.Fatal(err)
	}
	if e.a.IsDirect("d1") {
		t.Fatal("still direct after ApplyExit")
	}
	if body, _, err := fetch(pHTTP, d, directTarget.addr, "/ip"); err != nil || body != "exit-A" {
		t.Fatalf("after ApplyExit: %q %v", body, err)
	}
	if err := e.a.ApplyDirect("d1"); err != nil || !e.a.IsDirect("d1") {
		t.Fatalf("ApplyDirect: %v", err)
	}
	if body, _, err := fetch(pHTTP, d, directTarget.addr, "/ip"); err != nil || body != "DIRECT-OK" {
		t.Fatalf("after ApplyDirect: %q %v", body, err)
	}
}
