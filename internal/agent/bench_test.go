package agent

// A-W2：压测基线。比较“带计量的接入服务”（isp-tcp + isp-auth + 失败即拒绝的出口链）
// 与“不带计量的 GOST 服务”（tcp 监听器、无认证、直接注册的同一条链）。两者都经同一个
// 伪 ISP 上游到同一个目标，差值即计量、连接表、认证与出站检查的开销。
//
// 运行：internal/agent/scripts/bench.sh（或 go test -run '^$' -bench Proxy -benchmem ./internal/agent/）。
// 全部组件（客户端、GOST、上游、目标）在同一进程内，allocs/op 与 cpu-ns/op 为整个进程的合计。

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gost/x/config"
	service_parser "github.com/go-gost/x/config/parsing/service"
	"github.com/go-gost/x/registry"
)

// ---- 目标：按命令收发字节的 TCP 服务 ----
//
//	'D' + uint64 n：服务端写 n 字节
//	'U' + uint64 n：服务端读 n 字节，回 1 字节
//	'P'：回 1 字节（往返延迟）
func startBenchTarget(b testing.TB) string {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveBenchConn(c)
		}
	}()
	return ln.Addr().String()
}

func serveBenchConn(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 64*1024)
	hdr := make([]byte, 9)
	for {
		if _, err := io.ReadFull(c, hdr[:1]); err != nil {
			return
		}
		switch hdr[0] {
		case 'P':
			if _, err := c.Write(hdr[:1]); err != nil {
				return
			}
		case 'D', 'U':
			if _, err := io.ReadFull(c, hdr[1:]); err != nil {
				return
			}
			n := int64(binary.BigEndian.Uint64(hdr[1:]))
			var err error
			if hdr[0] == 'D' {
				for n > 0 && err == nil {
					k := min(n, int64(len(buf)))
					_, err = c.Write(buf[:k])
					n -= k
				}
			} else {
				_, err = io.CopyBuffer(io.Discard, io.LimitReader(c, n), buf)
				if err == nil {
					_, err = c.Write(hdr[:1])
				}
			}
			if err != nil {
				return
			}
		default:
			return
		}
	}
}

type benchClient struct {
	c   net.Conn
	buf []byte
	hdr [9]byte
}

func (bc *benchClient) download(n int64) error {
	bc.hdr[0] = 'D'
	binary.BigEndian.PutUint64(bc.hdr[1:], uint64(n))
	if _, err := bc.c.Write(bc.hdr[:]); err != nil {
		return err
	}
	_, err := io.CopyBuffer(io.Discard, io.LimitReader(bc.c, n), bc.buf)
	return err
}

func (bc *benchClient) upload(n int64) error {
	bc.hdr[0] = 'U'
	binary.BigEndian.PutUint64(bc.hdr[1:], uint64(n))
	if _, err := bc.c.Write(bc.hdr[:]); err != nil {
		return err
	}
	for n > 0 {
		k := min(n, int64(len(bc.buf)))
		if _, err := bc.c.Write(bc.buf[:k]); err != nil {
			return err
		}
		n -= k
	}
	_, err := io.ReadFull(bc.c, bc.hdr[:1])
	return err
}

func (bc *benchClient) ping() error {
	bc.hdr[0] = 'P'
	if _, err := bc.c.Write(bc.hdr[:1]); err != nil {
		return err
	}
	_, err := io.ReadFull(bc.c, bc.hdr[:1])
	return err
}

// ---- 被测服务 ----

type benchSetup struct {
	target string
	accs   []Access // 每个订阅一组接入；plain 时只有 HTTPAddr 有意义且无凭据
}

var benchSeq atomic.Uint64

// newBenchSetup 建立被测服务。kind：plain（不带计量）、metered、metered-limits（同时启用
// 并发与带宽上限，但上限远高于负载，只测执行开销）。subs 为订阅数。
func newBenchSetup(b *testing.B, kind string, subs int) *benchSetup {
	b.Helper()
	e := newEnv(b, time.Second)
	target := startBenchTarget(b)
	up := startUpstream(b, target, "ub", "pb")
	exit := Exit{Version: 1, Protocol: "http", Addr: up.addr, Username: "ub", Password: "pb"}
	s := &benchSetup{target: target}

	if kind == "plain" {
		name := fmt.Sprintf("bench-plain-%d", benchSeq.Add(1))
		ch, err := buildChain(name, exit)
		if err != nil {
			b.Fatal(err)
		}
		if err := registry.ChainRegistry().Register(name, ch); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { registry.ChainRegistry().Unregister(name) })
		addr := freeAddr(b)
		svc, err := service_parser.ParseService(&config.ServiceConfig{
			Name:     name,
			Addr:     addr,
			Listener: &config.ListenerConfig{Type: "tcp"},
			Handler:  &config.HandlerConfig{Type: "http", Chain: name},
		})
		if err != nil {
			b.Fatal(err)
		}
		go svc.Serve()
		b.Cleanup(func() { svc.Close() })
		waitListening(b, addr)
		s.accs = []Access{{HTTPAddr: addr}}
		return s
	}

	for i := 0; i < subs; i++ {
		id := fmt.Sprintf("b%d", i)
		e.a.Upsert(SubState{ID: id, Version: 1, EntitledTotal: 1 << 62, ExpiresAt: time.Now().Add(24 * time.Hour), Enabled: true})
		if kind == "metered-limits" {
			e.a.Upsert(SubState{ID: id, Version: 2, EntitledTotal: 1 << 62, ExpiresAt: time.Now().Add(24 * time.Hour), Enabled: true,
				MaxConns: 1 << 20, BandwidthBps: 1 << 40})
		}
		acc := e.newAccess(1, "u-"+id)
		if err := e.a.ApplyAccess(id, acc, &exit); err != nil {
			b.Fatal(err)
		}
		waitListening(b, acc.HTTPAddr)
		s.accs = append(s.accs, acc)
	}
	return s
}

func (s *benchSetup) dial(b testing.TB, i int) *benchClient {
	acc := s.accs[i%len(s.accs)]
	c, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, s.target)
	if err != nil {
		b.Fatal(err)
	}
	return &benchClient{c: c, buf: make([]byte, 64*1024)}
}

var benchKinds = []string{"plain", "metered", "metered-limits"}

// cpuMeter 统计基准期间整个进程的 CPU 时间。
type cpuMeter struct{ start time.Duration }

func startCPU() cpuMeter { return cpuMeter{processCPU()} }

func (m cpuMeter) report(b *testing.B) {
	if d := processCPU() - m.start; d > 0 {
		b.ReportMetric(float64(d.Nanoseconds())/float64(b.N), "cpu-ns/op")
	}
}

func reportPercentiles(b *testing.B, lat []time.Duration) {
	if len(lat) == 0 {
		return
	}
	slices.Sort(lat)
	pick := func(q float64) float64 {
		return float64(lat[min(len(lat)-1, int(q*float64(len(lat))))].Microseconds())
	}
	b.ReportMetric(pick(0.50), "p50-µs")
	b.ReportMetric(pick(0.99), "p99-µs")
}

// 单连接下行吞吐：每次操作下载 1 MiB。
func BenchmarkProxyDownload(b *testing.B) {
	for _, kind := range benchKinds {
		b.Run(kind, func(b *testing.B) {
			s := newBenchSetup(b, kind, 1)
			bc := s.dial(b, 0)
			defer bc.c.Close()
			b.SetBytes(1 << 20)
			b.ReportAllocs()
			cpu := startCPU()
			b.ResetTimer()
			for b.Loop() {
				if err := bc.download(1 << 20); err != nil {
					b.Fatal(err)
				}
			}
			cpu.report(b)
		})
	}
}

// 单连接上行吞吐：每次操作上传 1 MiB。
func BenchmarkProxyUpload(b *testing.B) {
	for _, kind := range benchKinds {
		b.Run(kind, func(b *testing.B) {
			s := newBenchSetup(b, kind, 1)
			bc := s.dial(b, 0)
			defer bc.c.Close()
			b.SetBytes(1 << 20)
			b.ReportAllocs()
			cpu := startCPU()
			b.ResetTimer()
			for b.Loop() {
				if err := bc.upload(1 << 20); err != nil {
					b.Fatal(err)
				}
			}
			cpu.report(b)
		})
	}
}

// 已建立隧道上的 1 字节往返延迟。
func BenchmarkProxyRTT(b *testing.B) {
	for _, kind := range benchKinds {
		b.Run(kind, func(b *testing.B) {
			s := newBenchSetup(b, kind, 1)
			bc := s.dial(b, 0)
			defer bc.c.Close()
			lat := make([]time.Duration, 0, 1<<16)
			b.ReportAllocs()
			cpu := startCPU()
			b.ResetTimer()
			for b.Loop() {
				t0 := time.Now()
				if err := bc.ping(); err != nil {
					b.Fatal(err)
				}
				lat = append(lat, time.Since(t0))
			}
			cpu.report(b)
			reportPercentiles(b, lat)
		})
	}
}

// 建立新隧道（TCP 连接 + CONNECT + 认证 + 上游握手）并完成一次往返。
// 每次操作消耗 2～3 个临时端口（TIME_WAIT），须用固定次数运行，如 -benchtime 1000x。
func BenchmarkProxyConnect(b *testing.B) {
	for _, kind := range benchKinds {
		b.Run(kind, func(b *testing.B) {
			s := newBenchSetup(b, kind, 1)
			lat := make([]time.Duration, 0, 1<<14)
			b.ReportAllocs()
			cpu := startCPU()
			b.ResetTimer()
			for b.Loop() {
				t0 := time.Now()
				bc := s.dial(b, 0)
				if err := bc.ping(); err != nil {
					b.Fatal(err)
				}
				// 客户端以 RST 关闭，少占一个 TIME_WAIT 端口（macOS 临时端口只有约 16k 个）。
				if tc, ok := bc.c.(*countingConn).Conn.(*net.TCPConn); ok {
					tc.SetLinger(0)
				}
				bc.c.Close()
				lat = append(lat, time.Since(t0))
			}
			cpu.report(b)
			reportPercentiles(b, lat)
		})
	}
}

// 多连接并发下行：GOMAXPROCS 条隧道，分布在 1 个或 16 个订阅上；每次操作下载 256 KiB。
func BenchmarkProxyParallel(b *testing.B) {
	for _, c := range []struct {
		kind string
		subs int
	}{{"plain", 1}, {"metered", 1}, {"metered", 16}, {"metered-limits", 16}} {
		b.Run(fmt.Sprintf("%s/subs=%d", c.kind, c.subs), func(b *testing.B) {
			s := newBenchSetup(b, c.kind, c.subs)
			var next atomic.Int64
			var mu sync.Mutex
			var clients []*benchClient
			b.Cleanup(func() {
				for _, bc := range clients {
					bc.c.Close()
				}
			})
			b.SetBytes(256 << 10)
			b.ReportAllocs()
			cpu := startCPU()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				bc := s.dial(b, int(next.Add(1)))
				mu.Lock()
				clients = append(clients, bc)
				mu.Unlock()
				for pb.Next() {
					if err := bc.download(256 << 10); err != nil {
						b.Error(err)
						return
					}
				}
			})
			cpu.report(b)
		})
	}
}
