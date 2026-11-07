package agent

// A-W6：SOCKS5 认证按“服务 + 源地址”查找连接（04 §8 局限 4）。

import (
	"fmt"
	"net"
	"testing"
)

// addrConn 只提供 RemoteAddr，用于不经网络地构造连接表。
type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }
func (c addrConn) Close() error         { return nil }

func fakeTracked(a *Agent, service string, i int) *trackedConn {
	src := &net.TCPAddr{IP: net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)), Port: 40000 + i%20000}
	return newTrackedConn(addrConn{remote: src}, a, service)
}

// 索引必须随连接登记与注销同步；同一服务上源地址重复时视为无法确定，拒绝而不是任选一个
// （任选会把认证结果记到另一条连接上，留下不计量的连接）。
func TestConnBySrcIndex(t *testing.T) {
	a := newBareAgent(t)
	c1 := fakeTracked(a, "svc", 1)
	c2 := fakeTracked(a, "svc", 2)
	other := fakeTracked(a, "svc-other", 1)
	for _, c := range []*trackedConn{c1, c2, other} {
		a.track(c)
	}
	if got := a.connBySrc("svc", c1.RemoteAddr()); got != c1 {
		t.Fatalf("lookup c1: %p", got)
	}
	if got := a.connBySrc("svc-other", c1.RemoteAddr()); got != other {
		t.Fatal("lookup must be scoped by service")
	}
	if a.connBySrc("svc", &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}) != nil {
		t.Fatal("unknown source matched")
	}
	if a.connBySrc("svc", nil) != nil {
		t.Fatal("nil source matched")
	}

	dup := fakeTracked(a, "svc", 1) // 与 c1 源地址相同
	a.track(dup)
	if a.connBySrc("svc", c1.RemoteAddr()) != nil {
		t.Fatal("ambiguous source must not match")
	}
	a.untrack(dup)
	if a.connBySrc("svc", c1.RemoteAddr()) != c1 {
		t.Fatal("index not restored after untrack")
	}
	a.untrack(c1)
	if a.connBySrc("svc", c1.RemoteAddr()) != nil {
		t.Fatal("untracked conn still indexed")
	}
	if a.connBySrc("svc", c2.RemoteAddr()) != c2 {
		t.Fatal("unrelated entry lost")
	}
}

func BenchmarkConnBySrc(b *testing.B) {
	for _, n := range []int{1, 100, 10_000} {
		b.Run(fmt.Sprintf("conns=%d", n), func(b *testing.B) {
			a, err := New(Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer a.Close()
			var last *trackedConn
			for i := 0; i < n; i++ {
				last = fakeTracked(a, "svc", i)
				a.track(last)
			}
			src := last.RemoteAddr()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if a.connBySrc("svc", src) != last {
					b.Fatal("not found")
				}
			}
		})
	}
}
