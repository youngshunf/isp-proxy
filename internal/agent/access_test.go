package agent

// M3 转正时补充的接入与出口测试：服务配置不开放绕过计量的能力、未认证连接不能经出口转发、
// 无出口的接入失败即拒绝、原地替换凭据、https/socks5 上游的链配置。

import (
	"errors"
	"testing"
	"time"

	"github.com/go-gost/x/config"
	service_parser "github.com/go-gost/x/config/parsing/service"
)

// docs/04 §2、§8 局限 7：生成的接入服务必须显式关闭 SOCKS5 UDP/BIND/Tor 与 HTTP UDP，
// 并且都挂在跟踪监听器、isp-auth 与指定出口链上。
func TestAccessServiceConfigsDisableBypasses(t *testing.T) {
	acc := Access{Version: 3, Username: "u", Password: "p", HTTPAddr: ":1", HTTPSAddr: ":2", SOCKS5Addr: ":3"}
	cfgs := accessServiceConfigs(serviceNames("s1", 3), acc, "exit-s1-a3")
	if len(cfgs) != 3 {
		t.Fatalf("got %d services", len(cfgs))
	}
	want := []struct{ name, addr, ln, h string }{
		{"acc-s1-v3-http", ":1", ListenerTCP, "http"},
		{"acc-s1-v3-https", ":2", ListenerTLS, "http"},
		{"acc-s1-v3-socks5", ":3", ListenerTCP, "socks5"},
	}
	for i, c := range cfgs {
		w := want[i]
		if c.Name != w.name || c.Addr != w.addr || c.Listener.Type != w.ln || c.Handler.Type != w.h {
			t.Fatalf("service %d: %+v / %+v", i, c, c.Handler)
		}
		if c.Handler.Auther != AutherName || c.Handler.Chain != "exit-s1-a3" || c.Handler.Auth != nil || len(c.Handler.Authers) != 0 {
			t.Fatalf("%s: auther %q chain %q", c.Name, c.Handler.Auther, c.Handler.Chain)
		}
		if c.Logger != LoggerName || len(c.Loggers) != 0 {
			t.Fatalf("%s: logger must be %s (redacting), got %q %v", c.Name, LoggerName, c.Logger, c.Loggers)
		}
		if c.Listener.Auther != "" || c.Listener.Chain != "" {
			t.Fatalf("%s: listener must not carry auther/chain", c.Name)
		}
		md := c.Handler.Metadata
		if v, ok := md["udp"]; !ok || v != false {
			t.Fatalf("%s: udp must be explicitly false, got %v", c.Name, md)
		}
		if w.h == "socks5" {
			for _, k := range []string{"bind", "tor"} {
				if v, ok := md[k]; !ok || v != false {
					t.Fatalf("%s: %s must be explicitly false, got %v", c.Name, k, md)
				}
			}
		}
	}
}

// 纵深防御：处理器没有认证器时（等同于 GOST 注册表里 isp-auth 缺失时 autherWrapper 放行），
// 连接未被标记为已认证，出口链拒绝路由：请求失败，上游收不到连接，也不会直连。
func TestRouteRequiresAuthenticatedConn(t *testing.T) {
	e := newEnv(t, time.Second)
	e.provision("s1", 1<<30, time.Hour)
	upBefore := e.upA.accepted.Load()
	addr := freeAddr(t)
	svc, err := service_parser.ParseService(&config.ServiceConfig{
		Name:     serviceNames("s1", 1)[0], // 冒用已开通的服务名：连接被跟踪并归属 s1
		Addr:     addr,
		Listener: &config.ListenerConfig{Type: ListenerTCP},
		Handler:  &config.HandlerConfig{Type: "http", Chain: "exit-s1-a1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	go svc.Serve()
	defer svc.Close()
	waitListening(t, addr)
	for i := 0; i < 3; i++ {
		if c, err := tunnel(pHTTP, addr, "", "", e.canary.addr); err == nil {
			body, _ := get(c, "/ip")
			c.Close()
			t.Fatalf("unauthenticated connection forwarded: %q", body)
		}
	}
	if n := e.upA.accepted.Load() - upBefore; n != 0 {
		t.Fatalf("upstream saw %d connections", n)
	}
	if e.canary.hits.Load() != 0 {
		t.Fatal("direct connection reached target")
	}
}

// 期望状态里 access 非空、exit 为空：端口监听、认证照常，转发全部失败，不直连；之后补上出口即可用。
func TestReplaceAccessWithoutExitFailsClosed(t *testing.T) {
	e := newEnv(t, time.Second)
	e.a.Upsert(SubState{ID: "s1", Version: 1, EntitledTotal: 1 << 30, ExpiresAt: time.Now().Add(time.Hour), Enabled: true})
	acc := e.newAccess(1, "u-s1")
	if err := e.a.ReplaceAccess("s1", acc, nil); err != nil {
		t.Fatal(err)
	}
	waitListening(t, acc.HTTPAddr, acc.HTTPSAddr, acc.SOCKS5Addr)
	for _, p := range protos {
		if body, _, err := fetch(p, acc, e.canary.addr, "/ip"); err == nil {
			t.Fatalf("%s: forwarded without exit: %q", p, body)
		}
	}
	if _, ok := e.a.CurrentExit("s1"); ok {
		t.Fatal("CurrentExit reports an exit")
	}
	if err := e.a.ApplyExit("s1", e.exitA); err != nil {
		t.Fatal(err)
	}
	for _, p := range protos {
		if ip, _, err := fetch(p, acc, e.canary.addr, "/ip"); err != nil || ip != "exit-A" {
			t.Fatalf("%s: after exit set: %q %v", p, ip, err)
		}
	}
	live, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := e.a.ClearExit("s1"); err != nil {
		t.Fatal(err)
	}
	if _, closed := waitClosed(live, stopLimit); !closed {
		t.Fatal("connection survived ClearExit")
	}
	if _, _, err := fetch(pSOCKS5, acc, e.canary.addr, "/ip"); err == nil {
		t.Fatal("forwarded after ClearExit")
	}
	if e.canary.hits.Load() != 0 {
		t.Fatal("direct connection reached target")
	}
	// 旧 API 在没有出口时仍返回 ErrNoExit。
	if err := e.a.ApplyAccess("s1", e.newAccess(2, "u-s1-v2"), nil); !errors.Is(err, ErrNoExit) {
		t.Fatalf("ApplyAccess without exit: %v", err)
	}
}

// 原地替换用户名与密码：端口不变，旧凭据立即失效，存量连接关闭。
func TestUpdateCredentials(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	other := e.provision("s2", 1<<30, time.Hour)
	live, err := tunnel(pSOCKS5, acc.SOCKS5Addr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := e.a.UpdateCredentials("s1", 2, other.Username, "x"); err == nil {
		t.Fatal("took another subscription's username")
	}
	if err := e.a.UpdateCredentials("s1", 2, "u-s1-new", "pw-new"); err != nil {
		t.Fatal(err)
	}
	if _, closed := waitClosed(live, stopLimit); !closed {
		t.Fatal("connection survived credential change")
	}
	for _, p := range protos {
		if _, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr); err == nil {
			t.Fatalf("%s: old credentials accepted", p)
		}
	}
	next := acc
	next.Username, next.Password = "u-s1-new", "pw-new"
	for _, p := range protos {
		if ip, _, err := fetch(p, next, e.canary.addr, "/ip"); err != nil || ip != "exit-A" {
			t.Fatalf("%s: new credentials: %q %v", p, ip, err)
		}
	}
	if snap := e.a.Snapshot(); snap.Subs[0].AccessVersion != 2 {
		t.Fatalf("access version %d", snap.Subs[0].AccessVersion)
	}
}

// 出口协议到 GOST 链配置的映射（docs/06 §8.3：https 上游 = http 连接器 + tls 拨号器）。
func TestChainConfigProtocols(t *testing.T) {
	for _, tc := range []struct {
		e                Exit
		connector, dial  string
		secure, wantNoTL bool
	}{
		{Exit{Protocol: ExitHTTP, Addr: "198.51.100.1:8080"}, "http", "tcp", false, false},
		{Exit{Protocol: ExitHTTPS, Addr: "198.51.100.1:8443"}, "http", "tls", true, false},
		{Exit{Protocol: ExitHTTPS, Addr: "198.51.100.1:8443", TLSInsecure: true}, "http", "tls", false, false},
		{Exit{Protocol: ExitSOCKS5, Addr: "198.51.100.1:1080", Username: "u", Password: "p"}, "socks5", "tcp", false, true},
	} {
		cfg, err := chainConfig("c", tc.e)
		if err != nil {
			t.Fatal(err)
		}
		n := cfg.Hops[0].Nodes[0]
		if n.Connector.Type != tc.connector || n.Dialer.Type != tc.dial {
			t.Fatalf("%s: %s/%s", tc.e.Protocol, n.Connector.Type, n.Dialer.Type)
		}
		if tc.dial == "tls" && n.Dialer.TLS.Secure != tc.secure {
			t.Fatalf("%s: secure %v", tc.e.Protocol, n.Dialer.TLS.Secure)
		}
		if tc.wantNoTL && n.Connector.Metadata["notls"] != true {
			t.Fatalf("socks5 connector must not negotiate GOST TLS methods")
		}
		if tc.e.Username != "" && (n.Connector.Auth == nil || n.Connector.Auth.Username != "u") {
			t.Fatal("upstream credentials missing")
		}
	}
	for _, bad := range []Exit{{Protocol: "ftp", Addr: "1.2.3.4:1"}, {Protocol: ExitHTTP, Addr: "nope"}} {
		if _, err := buildChain("c", bad); !errors.Is(err, ErrExitInvalid) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

// 零值到期时间（未起算）必须视为已到期；time.Time{}.UnixNano() 溢出，不能直接比较。
// 2262 年之后的到期时间钳到上界，仍视为未到期。
func TestExpiryOutOfInt64Range(t *testing.T) {
	a := newBareAgent(t)
	a.Upsert(SubState{ID: "zero", Version: 1, EntitledTotal: 1 << 30, Enabled: true})
	if a.Usable("zero") {
		t.Fatal("zero expires_at is usable")
	}
	a.Upsert(SubState{ID: "far", Version: 1, EntitledTotal: 1 << 30, Enabled: true, ExpiresAt: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if !a.Usable("far") {
		t.Fatal("far-future expires_at not usable")
	}
	a.Upsert(SubState{ID: "old", Version: 1, EntitledTotal: 1 << 30, Enabled: true, ExpiresAt: time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)})
	if a.Usable("old") {
		t.Fatal("pre-1970 expires_at is usable")
	}
}
