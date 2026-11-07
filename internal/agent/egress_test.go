package agent

// A-W4：出站封禁（A-46）。出口链包装器在路由入口检查目标地址；默认拒绝，例外须显式配置。

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubResolver 是测试用 DNS：未知名字返回 NXDOMAIN，并统计调用次数。
type stubResolver struct {
	m     map[string][]string
	calls atomic.Int64
}

func (r *stubResolver) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	r.calls.Add(1)
	ips, ok := r.m[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var out []netip.Addr
	for _, s := range ips {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func testResolver() *stubResolver {
	return &stubResolver{m: map[string][]string{
		"good.test":      {"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"},
		"mixed.test":     {"93.184.216.34", "10.0.0.5"},
		"v6private.test": {"fd12:3456::1"},
		"mapped.test":    {"::ffff:169.254.169.254"},
		"empty.test":     {},
		"localhost":      {"127.0.0.1", "::1"},
		"loop.test":      {"127.0.0.1"},
	}}
}

func TestEgressPolicyClassifies(t *testing.T) {
	r := testResolver()
	def := EgressPolicy{Resolver: r.lookup}
	allowed := []string{
		"8.8.8.8:443", "1.1.1.1:80", "172.32.0.1:80", "100.128.0.1:80", "198.51.100.7:8080",
		"[2001:4860:4860::8888]:443", "good.test:443", "GOOD.TEST.:80",
	}
	blocked := []string{
		// 端口 25
		"8.8.8.8:25", "good.test:25",
		// RFC 1918、环回、链路本地、CGNAT、未指定、组播、广播、保留
		"10.1.2.3:80", "172.16.0.1:80", "172.31.255.255:80", "192.168.1.1:443",
		"127.0.0.1:80", "127.255.0.1:80", "169.254.1.1:80", "100.64.0.1:80", "100.127.255.255:80",
		"0.0.0.0:80", "224.0.0.1:80", "239.255.255.250:1900", "255.255.255.255:80", "240.0.0.1:80",
		// 云元数据（AWS/Azure/GCP 169.254.169.254，AWS IPv6，阿里云 100.100.100.200，Oracle 192.0.0.192）
		"169.254.169.254:80", "[fd00:ec2::254]:80", "100.100.100.200:80", "192.0.0.192:80",
		"metadata.google.internal:80", "METADATA.GOOGLE.INTERNAL.:80", "metadata.goog:80", "metadata:80",
		"instance-data.ec2.internal:80",
		// IPv6 环回、未指定、ULA、链路本地、组播，以及内嵌 IPv4 的形式
		"[::1]:80", "[::]:80", "[fc00::1]:80", "[fe80::1]:80", "[fe80::1%25en0]:80", "[ff02::1]:80",
		"[::ffff:127.0.0.1]:80", "[::ffff:10.0.0.1]:80", "[64:ff9b::a00:1]:80", "[2002:a00:1::1]:80",
		// 解析后任一地址被封即拒绝；解析失败或无结果也拒绝
		"mixed.test:443", "v6private.test:443", "mapped.test:80", "localhost:80", "a.localhost:80",
		"loop.test:80", "nxdomain.test:80", "empty.test:80",
		// 数字形式的“主机名”，上游可能按 inet_aton 解释成私网地址
		"2130706433:80", "0x7f.0.0.1:80", "127.1:80", "0177.0.0.1:80",
		// 格式错误
		"noport", ":80", "good.test:", "good.test:0", "good.test:99999", "good.test:http",
	}
	for _, a := range allowed {
		if err := def.Check(context.Background(), "tcp", a); err != nil {
			t.Errorf("%s: unexpectedly blocked: %v", a, err)
		}
	}
	for _, a := range blocked {
		err := def.Check(context.Background(), "tcp", a)
		if !errors.Is(err, ErrEgressBlocked) {
			t.Errorf("%s: not blocked (err %v)", a, err)
		}
	}

	// 元数据名字不经 DNS 直接拒绝。
	before := r.calls.Load()
	def.Check(context.Background(), "tcp", "metadata.google.internal:80")
	if r.calls.Load() != before {
		t.Error("metadata name was resolved instead of rejected outright")
	}

	// 显式例外只放开所列网段与端口，其余照旧。
	exc := EgressPolicy{
		Resolver:   r.lookup,
		AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		AllowPorts: []int{25},
	}
	for _, a := range []string{"127.0.0.1:8080", "loop.test:80", "8.8.8.8:25"} {
		if err := exc.Check(context.Background(), "tcp", a); err != nil {
			t.Errorf("%s: exception not honoured: %v", a, err)
		}
	}
	for _, a := range []string{"10.0.0.1:80", "[::1]:80", "localhost:80", "169.254.169.254:80"} {
		if err := exc.Check(context.Background(), "tcp", a); !errors.Is(err, ErrEgressBlocked) {
			t.Errorf("%s: exception too broad (err %v)", a, err)
		}
	}
}

// 三种入口路径（HTTP CONNECT、普通 HTTP 转发、SOCKS5）对被封目标都显式失败：
// 不连上游、不直连、不挂起；只计协议字节；不影响同订阅的其它连接。
func TestEgressBlockedAcrossProtocols(t *testing.T) {
	e := newEnv(t, time.Second)
	r := testResolver()
	e.a.SetEgressPolicy(EgressPolicy{Resolver: r.lookup}) // 默认策略：不放开环回
	acc := e.provision("s1", 1<<40, time.Hour)

	const allowedTarget = "198.51.100.7:80" // 伪上游忽略目标，到达 backend A
	live, err := tunnel(pSOCKS5, acc.SOCKS5Addr, acc.Username, acc.Password, allowedTarget)
	if err != nil {
		t.Fatalf("allowed target: %v", err)
	}
	defer live.Close()

	blocked := []string{
		e.canary.addr, "10.0.0.1:80", "169.254.169.254:80", "[fd00:ec2::254]:80",
		"198.51.100.7:25", "metadata.google.internal:80", "mixed.test:443", "localhost:80",
	}
	accepted := e.upA.accepted.Load()
	kicks := e.a.Kicks()
	used := e.a.Used("s1")
	attempts := 0
	for _, p := range protos {
		for _, target := range blocked {
			attempts++
			start := time.Now()
			c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, target)
			if err == nil {
				c.Close()
				t.Fatalf("%s %s: blocked target accepted", p, target)
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("%s %s: rejection took %v", p, target, d)
			}
			want := "503"
			if p == pSOCKS5 {
				want = "socks5: reply"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s %s: got %v, want explicit failure %q", p, target, err, want)
			}
		}
	}

	// 普通 HTTP 转发。
	proxyURL := &url.URL{Scheme: "http", User: url.UserPassword(acc.Username, acc.Password), Host: acc.HTTPAddr}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}, Timeout: 3 * time.Second}
	for _, u := range []string{
		"http://" + e.canary.addr + "/ip", "http://10.0.0.1/ip", "http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/", "http://mixed.test/ip", "http://198.51.100.7:25/",
	} {
		attempts++
		resp, err := client.Get(u)
		if err != nil {
			t.Fatalf("forward %s: %v", u, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("forward %s: blocked target served", u)
		}
	}

	if n := e.upA.accepted.Load() - accepted; n != 0 {
		t.Fatalf("blocked attempts reached the upstream %d times", n)
	}
	if n := e.canary.hits.Load(); n != 0 {
		t.Fatalf("blocked target reached directly %d times", n)
	}
	if e.a.Kicks() != kicks {
		t.Fatal("blocked attempt kicked the subscription")
	}
	// 被拒绝的尝试只计协议字节（请求与错误响应），每次远小于 1 KiB。
	if d := e.a.Used("s1") - used; d > uint64(attempts)*1024 {
		t.Fatalf("blocked attempts metered %d bytes over %d attempts", d, attempts)
	}
	if body, err := get(live, "/ip"); err != nil || body != "exit-A" {
		t.Fatalf("live connection disturbed: %q %v", body, err)
	}

	// 放行的目标在三种路径上仍然可用。
	for _, p := range protos {
		if ip, _, err := fetch(p, acc, allowedTarget, "/ip"); err != nil || ip != "exit-A" {
			t.Fatalf("%s allowed: %q %v", p, ip, err)
		}
		if ip, _, err := fetch(p, acc, "good.test:80", "/ip"); err != nil || ip != "exit-A" {
			t.Fatalf("%s allowed hostname: %q %v", p, ip, err)
		}
	}
	resp, err := client.Get("http://198.51.100.7/ip")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "exit-A" {
		t.Fatalf("forward allowed: %q", b)
	}
}
