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
