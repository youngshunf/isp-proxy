package agentproto

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const nodeA = "nod_a"

type fakeControl struct {
	mu      sync.Mutex
	rev     uint64
	reports map[string][]Report
	probes  map[string]ProbeResult
	known   map[string]bool
}

func newFake(rev uint64) *fakeControl {
	return &fakeControl{rev: rev, reports: map[string][]Report{}, probes: map[string]ProbeResult{}, known: map[string]bool{nodeA: true}}
}

func (f *fakeControl) setRev(r uint64) {
	f.mu.Lock()
	f.rev = r
	f.mu.Unlock()
}

func (f *fakeControl) Desired(_ context.Context, node string) (*Desired, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known[node] {
		return nil, ErrUnknownNode
	}
	return &Desired{Revision: f.rev, NodeID: node, GeneratedAt: time.Now().UTC(), Subscriptions: []SubscriptionEntry{}, Probes: []Probe{}}, nil
}

func (f *fakeControl) Report(_ context.Context, node string, r *Report) (*ReportAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known[node] {
		return nil, ErrUnknownNode
	}
	f.reports[node] = append(f.reports[node], *r)
	return &ReportAck{Revision: f.rev, ServerTime: time.Now().UTC()}, nil
}

func (f *fakeControl) ProbeResults(_ context.Context, node string, rs []ProbeResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rs {
		if _, done := f.probes[r.ProbeID]; !done {
			f.probes[r.ProbeID] = r
		}
	}
	return nil
}

type certs struct {
	dir   string
	paths *DevCerts
}

func genCerts(t *testing.T, nodes ...string) certs {
	t.Helper()
	dir := t.TempDir()
	p, err := GenerateDevCerts(dir, DevCertOptions{NodeIDs: nodes})
	if err != nil {
		t.Fatal(err)
	}
	return certs{dir, p}
}

func read(t *testing.T, f string) []byte {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (c certs) server(t *testing.T) *tls.Config {
	cfg, err := LoadServerTLSConfig(c.paths.CA.Cert, c.paths.Server.Cert, c.paths.Server.Key)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (c certs) client(t *testing.T, node string) *tls.Config {
	n := c.paths.Nodes[node]
	cfg, err := LoadClientTLSConfig(c.paths.CA.Cert, n.Cert, n.Key, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// serve 在 addr（为空则随机端口）上起一个 mTLS 控制面，返回地址与关闭函数。
func serve(t *testing.T, addr string, h http.Handler, cfg *tls.Config) (string, func()) {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // 重启时端口可能短暂不可用
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, TLSConfig: cfg, ErrorLog: nil}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	var once sync.Once
	stop := func() { once.Do(func() { _ = srv.Close() }) }
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

func TestMTLSRejectsForeignAndMissingClientCert(t *testing.T) {
	good := genCerts(t, nodeA)
	foreign := genCerts(t, nodeA) // 另一个 CA 签发的同名节点证书
	fake := newFake(1)
	addr, _ := serve(t, "", NewHandler(fake, NewHub(), HandlerOptions{}), good.server(t))
	base := "https://" + strings.Replace(addr, "127.0.0.1", "localhost", 1)
	ctx := context.Background()

	if d, err := NewClient(base, good.client(t, nodeA)).Fetch(ctx); err != nil || d.NodeID != nodeA {
		t.Fatalf("合法节点证书应通过并识别为 %s: d=%+v err=%v", nodeA, d, err)
	}

	// 其他 CA 签发的客户端证书：信任控制面，但控制面不信任它。
	fn := foreign.paths.Nodes[nodeA]
	cfg, err := ClientTLSConfig(read(t, good.paths.CA.Cert), read(t, fn.Cert), read(t, fn.Key), "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(base, cfg).Fetch(ctx); err == nil {
		t.Fatal("其他 CA 签发的客户端证书应被拒绝")
	}

	// 不带客户端证书。
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(read(t, good.paths.CA.Cert))
	noCert := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if _, err := NewClient(base, noCert).Fetch(ctx); err == nil {
		t.Fatal("缺少客户端证书应被拒绝")
	}

	// agent 也只信任自己的 CA：拿另一个 CA 的根去连，服务端证书验证失败。
	gn := good.paths.Nodes[nodeA]
	wrongRoot, _ := ClientTLSConfig(read(t, foreign.paths.CA.Cert), read(t, gn.Cert), read(t, gn.Key), "localhost")
	if _, err := NewClient(base, wrongRoot).Fetch(ctx); err == nil {
		t.Fatal("agent 不应信任其他 CA 签发的控制面证书")
	}
}

func TestUnknownNodeIsForbidden(t *testing.T) {
	c := genCerts(t, "nod_gone")
	addr, _ := serve(t, "", NewHandler(newFake(1), nil, HandlerOptions{}), c.server(t))
	_, err := NewClient("https://"+addr, withName(c.client(t, "nod_gone"))).Report(context.Background(), &Report{NodeTime: time.Now()})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden {
		t.Fatalf("未知节点应得到 403，得到 %v", err)
	}
}

func withName(cfg *tls.Config) *tls.Config {
	cfg.ServerName = "127.0.0.1"
	return cfg
}

func TestNodeIDFromCert(t *testing.T) {
	for cn, ok := range map[string]bool{"node:nod_ab12": true, "node:": false, "nod_ab12": false, "node:A B": false, "admin:x": false} {
		_, err := NodeIDFromCert(&x509.Certificate{Subject: pkix.Name{CommonName: cn}})
		if (err == nil) != ok {
			t.Errorf("CN %q: ok=%v err=%v", cn, ok, err)
		}
	}
}

func TestLongPoll(t *testing.T) {
	c := genCerts(t, nodeA)
	fake := newFake(5)
	hub := NewHub()
	addr, _ := serve(t, "", NewHandler(fake, hub, HandlerOptions{MaxWait: 300 * time.Millisecond}), c.server(t))
	cl := NewClient("https://"+addr, withName(c.client(t, nodeA)))
	ctx := context.Background()

	start := time.Now()
	d, err := cl.Poll(ctx, 4)
	if err != nil || d == nil || d.Revision != 5 {
		t.Fatalf("修订号更新时应立即返回: d=%+v err=%v", d, err)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("应立即返回，用了 %v", el)
	}

	start = time.Now()
	d, err = cl.Poll(ctx, 5)
	if err != nil || d != nil {
		t.Fatalf("无变化时超时应返回 (nil, nil): d=%+v err=%v", d, err)
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Fatalf("应等待到超时，只用了 %v", el)
	}

	// 等待期间修订号变化并通知 → 提前返回。
	addr2, _ := serve(t, "", NewHandler(fake, hub, HandlerOptions{MaxWait: 5 * time.Second}), c.server(t))
	cl2 := NewClient("https://"+addr2, withName(c.client(t, nodeA)))
	go func() {
		time.Sleep(100 * time.Millisecond)
		fake.setRev(6)
		hub.Notify(nodeA)
	}()
	start = time.Now()
	d, err = cl2.Poll(ctx, 5)
	if err != nil || d == nil || d.Revision != 6 {
		t.Fatalf("通知后应返回新修订号: d=%+v err=%v", d, err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("通知后应尽快返回，用了 %v", el)
	}

	// 控制面修订号回退（从备份恢复）也立即返回，让 agent 重新对账。
	fake.setRev(3)
	if d, err := cl.Poll(ctx, 6); err != nil || d == nil || d.Revision != 3 {
		t.Fatalf("修订号回退时应立即返回: d=%+v err=%v", d, err)
	}
}

func TestClientSurvivesServerRestart(t *testing.T) {
	c := genCerts(t, nodeA)
	fake1 := newFake(1)
	addr, stop := serve(t, "", NewHandler(fake1, nil, HandlerOptions{MaxWait: time.Second}), c.server(t))
	cl := NewClient("https://"+addr, withName(c.client(t, nodeA)))
	cl.MinBackoff, cl.MaxBackoff = 20*time.Millisecond, 100*time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	revs := make(chan uint64, 16)
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- cl.Watch(ctx, func(d *Desired) error { revs <- d.Revision; return nil })
	}()
	expect := func(want uint64) {
		t.Helper()
		select {
		case got := <-revs:
			if got != want {
				t.Fatalf("Watch 应收到修订号 %d，得到 %d", want, got)
			}
		case <-ctx.Done():
			t.Fatalf("等待修订号 %d 超时", want)
		}
	}
	expect(1)
	if _, err := cl.Report(ctx, &Report{NodeTime: time.Now(), PersistOK: true}); err != nil {
		t.Fatal(err)
	}

	// 控制面重启：旧连接全部断开，新进程在同一地址监听，修订号已前进。
	stop()
	fake2 := newFake(2)
	serve(t, addr, NewHandler(fake2, nil, HandlerOptions{MaxWait: time.Second}), c.server(t))

	// 上报在复用的连接已失效时由传输层重试（Idempotency-Key），不需要调用方处理。
	var err error
	for i := 0; i < 50; i++ {
		if _, err = cl.Report(ctx, &Report{NodeTime: time.Now(), PersistOK: true, Subscriptions: []SubscriptionReport{{ID: "sub_x", UsedTotal: 7}}}); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("重启后上报应恢复: %v", err)
	}
	expect(2)
	if got := fake2.reports[nodeA]; len(got) == 0 || got[len(got)-1].Subscriptions[0].UsedTotal != 7 {
		t.Fatalf("新控制面应收到上报: %+v", got)
	}

	// 探测结果按 ID 幂等。
	res := []ProbeResult{{ProbeID: "prb_1", OK: true, ObservedIP: "203.0.113.9", CompletedAt: time.Now()}}
	for i := 0; i < 2; i++ {
		if err := cl.PostProbeResults(ctx, res); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake2.probes) != 1 {
		t.Fatalf("重复回传不应产生重复结果: %+v", fake2.probes)
	}
	cancel()
	if err := <-watchErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch 应在 ctx 结束时返回 ctx.Err()，得到 %v", err)
	}
}

func TestWireFormatAndValidate(t *testing.T) {
	exp := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	d := Desired{
		Revision: 9, NodeID: nodeA, GeneratedAt: exp,
		Subscriptions: []SubscriptionEntry{{
			ID: "sub_1", Version: 3, EntitledTotal: 50_000_000_000, ExpiresAt: exp, Enabled: true, UsedFloor: 10,
			MaxConns: 50, BandwidthBps: 20_000_000,
			Access: &Access{Version: 2, Username: "u7k2m", Password: "p", Ports: Ports{HTTP: 20000, HTTPS: 20001, SOCKS5: 20002}},
			Exit:   &Exit{Version: 1, Protocol: ExitSOCKS5, Addr: "[2001:db8::1]:1080", Username: "a", Password: "b"},
		}},
		Probes: []Probe{{ID: "prb_1", Kind: ProbeCandidateCompare, SubscriptionID: "sub_1",
			Exit: Exit{Version: 0, Protocol: ExitHTTP, Addr: "198.51.100.1:8080"}, EchoURL: "http://127.0.0.1:9/ip", Deadline: exp}},
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	for _, key := range []string{`"entitled_total"`, `"expires_at":"2026-12-01T00:00:00Z"`, `"used_floor"`, `"max_conns"`,
		`"bandwidth_bps"`, `"socks5":20002`, `"echo_url"`, `"subscription_id"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON 缺少 %s: %s", key, b)
		}
	}
	var back Desired
	if err := json.Unmarshal(b, &back); err != nil || back.Subscriptions[0].Access.Ports.SOCKS5 != 20002 || !back.Subscriptions[0].ExpiresAt.Equal(exp) {
		t.Fatalf("往返失败: %v %+v", err, back)
	}

	bad := []func(*Desired){
		func(d *Desired) { d.Subscriptions[0].Access.Ports.HTTPS = 20000 },
		func(d *Desired) { d.Subscriptions[0].Access.Password = "" },
		func(d *Desired) { d.Subscriptions[0].Exit.Protocol = "direct" },
		func(d *Desired) { d.Subscriptions[0].Exit.Addr = "nohost" },
		func(d *Desired) { d.Subscriptions[0].Version = 0 },
		func(d *Desired) { d.Subscriptions[0].ExpiresAt = time.Time{} },
		func(d *Desired) { d.Subscriptions = append(d.Subscriptions, d.Subscriptions[0]) },
		func(d *Desired) { d.Probes[0].Kind = "x" },
	}
	for i, mut := range bad {
		var c Desired
		_ = json.Unmarshal(b, &c)
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("用例 %d 应校验失败", i)
		}
	}
}

func TestGenerateDevCertsReusesCA(t *testing.T) {
	dir := t.TempDir()
	a, err := GenerateDevCerts(dir, DevCertOptions{NodeIDs: []string{"nod_a"}})
	if err != nil {
		t.Fatal(err)
	}
	ca1 := read(t, a.CA.Cert)
	b, err := GenerateDevCerts(dir, DevCertOptions{NodeIDs: []string{"nod_a", "nod_b"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(read(t, b.CA.Cert)) != string(ca1) {
		t.Fatal("再次生成不应替换已有 CA")
	}
	if fi, err := os.Stat(b.Nodes["nod_b"].Key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("私钥权限应为 0600: %v %v", fi, err)
	}
	// 新节点证书由原 CA 签发，可被服务端配置验证。
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca1)
	kp, err := tls.LoadX509KeyPair(b.Nodes["nod_b"].Cert, b.Nodes["nod_b"].Key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(kp.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("新节点证书应由原 CA 签发: %v", err)
	}
	if id, err := NodeIDFromCert(leaf); err != nil || id != "nod_b" {
		t.Fatalf("CN 应为 node:nod_b: %q %v", id, err)
	}
	if _, err := GenerateDevCerts(dir, DevCertOptions{NodeIDs: []string{"Bad ID"}}); err == nil {
		t.Fatal("非法节点 ID 应报错")
	}
}
