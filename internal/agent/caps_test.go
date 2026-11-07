package agent

// A-W3：每订阅并发连接上限与带宽上限（A-46）。

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func (e *env) setCaps(id string, maxConns int, bps int64) {
	e.update(SubState{ID: id, EntitledTotal: 1 << 40, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, MaxConns: maxConns, BandwidthBps: bps})
}

// 超过并发上限的新连接在认证阶段被明确拒绝（407 / SOCKS5 认证失败），不挂起；
// 未认证和认证失败的连接不占名额；连接关闭或被踢后名额立即归还。
func TestMaxConnsRejectsBeyondCap(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<40, time.Hour)
	e.setCaps("s1", 3, 0)

	// 未认证的空连接与错误密码都不占名额。
	for i := 0; i < 5; i++ {
		raw, err := net.Dial("tcp", acc.HTTPAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		if _, err := tunnel(pSOCKS5, acc.SOCKS5Addr, acc.Username, "wrong", e.canary.addr); err == nil {
			t.Fatal("wrong password accepted")
		}
	}

	var held []*countingConn
	for _, p := range protos {
		c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr)
		if err != nil {
			t.Fatalf("%s within cap: %v", p, err)
		}
		held = append(held, c)
	}
	before := e.a.Used("s1")
	for _, p := range protos {
		start := time.Now()
		_, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr)
		if err == nil {
			t.Fatalf("%s: connection beyond cap accepted", p)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("%s: rejection took %v (must fail fast, not hang)", p, d)
		}
		want := "407"
		if p == pSOCKS5 {
			want = "auth failed"
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: rejection %q, want explicit %q", p, err, want)
		}
	}
	if u := e.a.Used("s1"); u != before {
		t.Fatalf("rejected connections metered: %d -> %d", before, u)
	}
	// 被拒绝不影响已有连接。
	if body, err := get(held[0], "/ip"); err != nil || body != "exit-A" {
		t.Fatalf("held connection broken: %q %v", body, err)
	}

	// 关闭一条后名额归还。
	held[1].Close()
	eventually(t, time.Second, func() bool {
		c, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr)
		if err != nil {
			return false
		}
		held[1] = c
		return true
	}, "slot not released after close")

	// 踢掉全部连接（重置密码）后名额全部归还，可立即建满。
	if err := e.a.ResetPassword("s1", "pw2"); err != nil {
		t.Fatal(err)
	}
	for _, c := range held {
		waitClosed(c, stopLimit)
		c.Close()
	}
	for i := 0; i < 3; i++ {
		c, err := tunnel(protos[i], protos[i].addr(acc), acc.Username, "pw2", e.canary.addr)
		if err != nil {
			t.Fatalf("after kick, conn %d: %v", i, err)
		}
		defer c.Close()
	}
	if _, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, "pw2", e.canary.addr); err == nil {
		t.Fatal("cap not enforced after kick")
	}

	// 上限放宽立即生效；0 表示不限。
	e.setCaps("s1", 0, 0)
	for i := 0; i < 5; i++ {
		c, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, "pw2", e.canary.addr)
		if err != nil {
			t.Fatalf("unlimited: %v", err)
		}
		defer c.Close()
	}
}

// 同一连接重复认证（HTTP keep-alive 等）只占一个名额。
func TestMarkAuthedIdempotentSlot(t *testing.T) {
	a := newBareAgent(t)
	a.Upsert(SubState{ID: "s", Version: 1, EntitledTotal: 1 << 30, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, MaxConns: 1})
	s := a.lookup("s")
	c := fakeTracked(a, "svc", 1)
	c.sub = s
	if !c.markAuthed() || !c.markAuthed() {
		t.Fatal("re-auth of the same connection rejected")
	}
	c2 := fakeTracked(a, "svc", 2)
	c2.sub = s
	if c2.markAuthed() {
		t.Fatal("second connection beyond cap accepted")
	}
	c.Close()
	if !c2.markAuthed() {
		t.Fatal("slot not released")
	}
	if n := s.active.Load(); n != 1 {
		t.Fatalf("active %d", n)
	}
}

// download 经 n 条不同协议的隧道并发下载 total 字节，返回耗时。
func download(t *testing.T, e *env, acc Access, n, total int) time.Duration {
	t.Helper()
	per := total / n
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		p := protos[i%len(protos)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			body, err := get(c, fmt.Sprintf("/bytes?n=%d", per))
			if err == nil && len(body) != per {
				err = fmt.Errorf("%s: got %d bytes", p, len(body))
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	return time.Since(start)
}

// upload 经 n 条隧道并发上传 total 字节到 /sink，返回耗时。
func upload(t *testing.T, e *env, acc Access, n, total int) time.Duration {
	t.Helper()
	per := total / n
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		p := protos[i%len(protos)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			fmt.Fprintf(c, "POST /sink HTTP/1.1\r\nHost: t\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", per)
			if _, err := io.Copy(c, io.LimitReader(zeros{}, int64(per))); err != nil {
				errs <- err
				return
			}
			b, err := io.ReadAll(c)
			if err == nil && !strings.HasSuffix(string(b), strconv.Itoa(per)) {
				err = fmt.Errorf("%s: sink response %q", p, b)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	return time.Since(start)
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// 带宽上限由订阅的全部连接共享（三种协议），上下行各自一份预算、各自等于上限。
func TestBandwidthCapSharedAcrossConnections(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<40, time.Hour)
	const total = 4_000_000

	if d := download(t, e, acc, 3, total); d > 700*time.Millisecond {
		t.Fatalf("baseline without cap took %v; loopback too slow for a meaningful bound", d)
	}

	const bps = 4_000_000
	e.setCaps("s1", 0, bps)
	// 下限：总量扣掉一次突发额度后按速率传完；上限留出调度余量。
	minD := time.Duration(float64(total-burstOf(bps)) / bps * float64(time.Second))
	check := func(name string, d time.Duration) {
		t.Helper()
		t.Logf("%s: %v (%.2f MB/s)", name, d, float64(total)/d.Seconds()/1e6)
		if d < minD*9/10 {
			t.Fatalf("%s finished in %v, faster than cap allows (%v)", name, d, minD)
		}
		if d > 3*minD {
			t.Fatalf("%s took %v, cap throttles far below %d B/s", name, d, bps)
		}
	}
	check("download x3", download(t, e, acc, 3, total))
	check("upload x3", upload(t, e, acc, 3, total))

	// 上下行同时各传 total/2：预算分开，所以总耗时约为单向的一半。
	var wg sync.WaitGroup
	start := time.Now()
	wg.Add(2)
	go func() { defer wg.Done(); download(t, e, acc, 3, total/2) }()
	go func() { defer wg.Done(); upload(t, e, acc, 3, total/2) }()
	wg.Wait()
	mixed := time.Since(start)
	t.Logf("down+up concurrently: %v", mixed)
	if mixed > minD*3/4 {
		t.Fatalf("concurrent up+down took %v; directions appear to share one budget", mixed)
	}
}

// 等待带宽令牌的连接也必须在停用时 1 秒内关闭。
func TestBandwidthWaitInterruptedByKick(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<40, time.Hour)
	e.setCaps("s1", 0, 64*1024)
	c, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go get(c, "/bytes?n=100000000")
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	e.update(SubState{ID: "s1", EntitledTotal: 1 << 40, ExpiresAt: time.Now().Add(time.Hour), Enabled: false, BandwidthBps: 64 * 1024})
	eventually(t, stopLimit, func() bool { return e.a.ConnCount("s1") == 0 }, "throttled connection survived pause")
	t.Logf("closed %v after pause", time.Since(start))
}
