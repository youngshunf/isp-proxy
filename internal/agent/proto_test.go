package agent

// 计量原型验证：对应 docs/04-计量与执行方案.md 第 8 节 S-1～S-10。
// 每个测试的断言都按需求写，失败即说明方案或内核接缝不成立。

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/x/config"
	service_parser "github.com/go-gost/x/config/parsing/service"
)

const stopLimit = time.Second

var protos = []proto{pHTTP, pHTTPS, pSOCKS5}

// S-2/A-02：三种协议都经绑定出口到达目标，且字节计入订阅。
func TestThreeProtocolsReachExitAndAreMetered(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	for _, p := range protos {
		before := e.a.Used("s1")
		body, n, err := fetch(p, acc, e.canary.addr, "/bytes?n=100000")
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if len(body) != 100000 {
			t.Fatalf("%s: body %d", p, len(body))
		}
		got := e.a.Used("s1") - before
		// 认证成功的连接全部计量，含 CONNECT/SOCKS5 握手；差值只是握手字节。
		if got < uint64(n) || got-uint64(n) > 1024 {
			t.Fatalf("%s: metered %d, client app bytes %d", p, got, n)
		}
		ip, _, err := fetch(p, acc, e.canary.addr, "/ip")
		if err != nil || ip != "exit-A" {
			t.Fatalf("%s: exit %q err %v", p, ip, err)
		}
	}
	if e.canary.hits.Load() != 0 {
		t.Fatal("direct connection reached target")
	}
}

// S-1/A-04～A-06：三种协议并发争用 1 MB 额度。下行严格不超额；
// 上行每连接最多透支一个读缓冲。耗尽后新连接被明确拒绝。
func TestConcurrentProtocolsCannotExceedQuota(t *testing.T) {
	e := newEnv(t, time.Second)
	const quota = 1_000_000
	acc := e.provision("s1", quota, time.Hour)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var clientBytes int64
	conns := 0
	for _, p := range protos {
		for i := 0; i < 4; i++ {
			conns++
			wg.Add(1)
			go func(p proto) {
				defer wg.Done()
				c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr)
				if err != nil {
					return
				}
				defer c.Close()
				get(c, "/bytes?n=20000000")
				mu.Lock()
				clientBytes += c.read.Load() + c.wrote.Load()
				mu.Unlock()
			}(p)
		}
	}
	wg.Wait()

	used := e.a.Used("s1")
	if used > quota+uint64(conns)*readChunk {
		t.Fatalf("used %d exceeds quota %d + %d conns × read buffer", used, quota, conns)
	}
	if clientBytes > int64(used) {
		t.Fatalf("client received %d > metered %d", clientBytes, used)
	}
	if e.a.Usable("s1") {
		t.Fatal("subscription still usable after exhaustion")
	}
	for _, p := range protos {
		if _, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr); err == nil {
			t.Fatalf("%s: new connection accepted after exhaustion", p)
		}
	}
	t.Logf("quota=%d used=%d client=%d overshoot=%d", quota, used, clientBytes, int64(used)-quota)
}

// S-2/A-07：耗尽时空闲隧道在 1 秒内被关闭。
func TestIdleTunnelClosedOnExhaustion(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 300_000, time.Hour)
	for _, p := range protos {
		t.Run(p.String(), func(t *testing.T) {
			e.update(SubState{ID: "s1", EntitledTotal: e.a.Used("s1") + 300_000, ExpiresAt: time.Now().Add(time.Hour), Enabled: true})
			idle, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer idle.Close()
			fetch(p, acc, e.canary.addr, "/bytes?n=5000000")
			if e.a.Usable("s1") {
				t.Fatal("not exhausted")
			}
			if d, closed := waitClosed(idle, stopLimit); !closed {
				t.Fatalf("idle tunnel still open after %v", d)
			}
		})
	}
}

// S-3/A-29/A-35：到期时（无需控制面参与）全部连接 1 秒内关闭，新连接被拒，余额保留。
func TestExpiryClosesAllConnections(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<50, 1500*time.Millisecond)
	expiresAt := time.Now().Add(1500 * time.Millisecond)

	var idles []*countingConn
	for _, p := range protos {
		c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		idles = append(idles, c)
	}
	streaming, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer streaming.Close()
	go get(streaming, "/bytes?n=2000000000") // 仅在后台持续读取

	for i, c := range idles {
		wait := time.Until(expiresAt) + stopLimit
		if _, closed := waitClosed(c, wait); !closed {
			t.Fatalf("%s idle tunnel open %v after expiry", protos[i], time.Since(expiresAt))
		}
	}
	if late := time.Since(expiresAt); late > stopLimit {
		t.Fatalf("closed %v after expiry", late)
	}
	// 以代理侧为准：客户端套接字里积压的数据读完需要时间，不代表代理仍在转发。
	eventually(t, time.Until(expiresAt.Add(stopLimit)), func() bool { return e.a.ConnCount("s1") == 0 },
		"connections still tracked 1s after expiry")
	u1 := e.a.Used("s1")
	time.Sleep(300 * time.Millisecond)
	if u2 := e.a.Used("s1"); u2 != u1 {
		t.Fatalf("forwarding continued after expiry: %d -> %d", u1, u2)
	}
	if _, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr); err == nil {
		t.Fatal("new connection accepted after expiry")
	}
	if e.a.Used("s1") >= 1<<50 {
		t.Fatal("closed by exhaustion, not expiry")
	}
}

// S-4/A-18：错误密码和垃圾数据不计入任何订阅。
func TestUnauthenticatedBytesNotMetered(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	junk := bytes.Repeat([]byte("x"), 1<<20)
	for _, p := range protos {
		for i := 0; i < 5; i++ {
			if _, err := tunnel(p, p.addr(acc), acc.Username, "wrong", e.canary.addr); err == nil {
				t.Fatalf("%s: wrong password accepted", p)
			}
			c, err := net.Dial("tcp", p.addr(acc))
			if err != nil {
				t.Fatal(err)
			}
			c.SetDeadline(time.Now().Add(time.Second))
			c.Write(junk)
			c.Close()
		}
	}
	if u := e.a.Used("s1"); u != 0 {
		t.Fatalf("unauthenticated traffic metered: %d", u)
	}
}

// S-5/A-40：更换接入。旧凭据和旧端口失效，旧连接关闭，新接入可用，用量连续。
func TestRotateAccess(t *testing.T) {
	e := newEnv(t, time.Second)
	old := e.provision("s1", 1<<30, time.Hour)
	if _, _, err := fetch(pSOCKS5, old, e.canary.addr, "/bytes?n=50000"); err != nil {
		t.Fatal(err)
	}
	live, err := tunnel(pHTTP, old.HTTPAddr, old.Username, old.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	usedBefore := e.a.Used("s1")

	next := e.newAccess(2, "u-s1-v2")
	if err := e.a.ApplyAccess("s1", next, nil); err != nil {
		t.Fatal(err)
	}
	waitListening(t, next.HTTPAddr, next.HTTPSAddr, next.SOCKS5Addr)

	if _, closed := waitClosed(live, stopLimit); !closed {
		t.Fatal("connection on old access still open")
	}
	for _, p := range protos {
		if _, err := tunnel(p, p.addr(old), old.Username, old.Password, e.canary.addr); err == nil {
			t.Fatalf("%s: old access still works", p)
		}
		if _, err := tunnel(p, p.addr(next), old.Username, old.Password, e.canary.addr); err == nil {
			t.Fatalf("%s: old credentials work on new ports", p)
		}
		ip, _, err := fetch(p, next, e.canary.addr, "/ip")
		if err != nil || ip != "exit-A" {
			t.Fatalf("%s: new access exit %q err %v", p, ip, err)
		}
	}
	if e.a.Used("s1") < usedBefore {
		t.Fatal("usage went backwards")
	}
}

// A-41：重置密码。端口不变，旧密码失效，存量连接关闭。
func TestResetPassword(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	live, err := tunnel(pSOCKS5, acc.SOCKS5Addr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := e.a.ResetPassword("s1", "new-secret"); err != nil {
		t.Fatal(err)
	}
	if _, closed := waitClosed(live, stopLimit); !closed {
		t.Fatal("connection survived password reset")
	}
	if _, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr); err == nil {
		t.Fatal("old password accepted")
	}
	acc.Password = "new-secret"
	if ip, _, err := fetch(pHTTP, acc, e.canary.addr, "/ip"); err != nil || ip != "exit-A" {
		t.Fatalf("new password: %q %v", ip, err)
	}
}

// 一个订阅的凭据不能在另一个订阅的端口上使用。
func TestCredentialsBoundToOwnPorts(t *testing.T) {
	e := newEnv(t, time.Second)
	a1 := e.provision("s1", 1<<30, time.Hour)
	a2 := e.provision("s2", 1<<30, time.Hour)
	for _, p := range protos {
		if _, err := tunnel(p, p.addr(a2), a1.Username, a1.Password, e.canary.addr); err == nil {
			t.Fatalf("%s: s1 credentials accepted on s2 port", p)
		}
	}
}

// S-6/A-11：换出口。接入不变，存量连接关闭，之后观察到新出口。
func TestSwapExit(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	live, err := tunnel(pHTTPS, acc.HTTPSAddr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := e.a.ApplyExit("s1", e.exitB); err != nil {
		t.Fatal(err)
	}
	if _, closed := waitClosed(live, stopLimit); !closed {
		t.Fatal("connection via old exit still open")
	}
	for _, p := range protos {
		if ip, _, err := fetch(p, acc, e.canary.addr, "/ip"); err != nil || ip != "exit-B" {
			t.Fatalf("%s: exit %q err %v", p, ip, err)
		}
	}
}

// S-6/A-42：组合更换。新接入直接走新出口，旧接入下线；失败时回到原状态。
func TestCombinedSwap(t *testing.T) {
	e := newEnv(t, time.Second)
	old := e.provision("s1", 1<<30, time.Hour)

	// 注入失败：新接入的 HTTP 端口被占用，整个操作必须回滚。
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bad := e.newAccess(2, "u-s1-bad")
	bad.HTTPAddr = busy.Addr().String()
	if err := e.a.ApplyAccess("s1", bad, &e.exitB); err == nil {
		t.Fatal("expected failure on busy port")
	}
	busy.Close()
	if ip, _, err := fetch(pHTTP, old, e.canary.addr, "/ip"); err != nil || ip != "exit-A" {
		t.Fatalf("after failed swap: %q %v", ip, err)
	}
	if _, err := tunnel(pSOCKS5, bad.SOCKS5Addr, bad.Username, bad.Password, e.canary.addr); err == nil {
		t.Fatal("half-created access is reachable")
	}

	next := e.newAccess(3, "u-s1-v3")
	if err := e.a.ApplyAccess("s1", next, &e.exitB); err != nil {
		t.Fatal(err)
	}
	waitListening(t, next.HTTPAddr, next.HTTPSAddr, next.SOCKS5Addr)
	for _, p := range protos {
		if ip, _, err := fetch(p, next, e.canary.addr, "/ip"); err != nil || ip != "exit-B" {
			t.Fatalf("%s: new access exit %q err %v", p, ip, err)
		}
		if _, err := tunnel(p, p.addr(old), old.Username, old.Password, e.canary.addr); err == nil {
			t.Fatalf("%s: old access still reachable", p)
		}
	}
}

// A-14：上游故障时显式失败，不退化为从中转直连目标。
func TestUpstreamDownNeverFallsBackToDirect(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	e.upA.ln.Close()
	for i := 0; i < 10; i++ { // 多次失败会让 GOST 选择器把节点标记为不可用
		for _, p := range protos {
			if body, _, err := fetch(p, acc, e.canary.addr, "/ip"); err == nil {
				t.Fatalf("%s: request succeeded with upstream down: %q", p, body)
			}
		}
	}
	if n := e.canary.hits.Load(); n != 0 {
		t.Fatalf("target reached directly %d times", n)
	}
}

// 特征测试：证明未经包装时，GOST 在链缺失时会直连目标（04 第 2 节的结论）。
func TestCharacterizeGOSTDirectFallback(t *testing.T) {
	e := newEnv(t, time.Second)
	addr := freeAddr(t)
	svc, err := service_parser.ParseService(&config.ServiceConfig{
		Name:     "characterize-direct",
		Addr:     addr,
		Listener: &config.ListenerConfig{Type: "tcp"},
		Handler:  &config.HandlerConfig{Type: "http", Chain: "chain-that-does-not-exist"},
	})
	if err != nil {
		t.Fatal(err)
	}
	go svc.Serve()
	defer svc.Close()
	waitListening(t, addr)
	c, err := tunnel(pHTTP, addr, "", "", e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if body, err := get(c, "/ip"); err != nil || body != "DIRECT" {
		t.Fatalf("expected direct fallback, got %q %v", body, err)
	}
}

// 认证器必须找到被跟踪的连接才放行。若认证器被挂在未跟踪的监听器上，
// 该连接无法计量，应当被拒绝。
func TestAuthRequiresTrackedConn(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	addr := freeAddr(t)
	svc, err := service_parser.ParseService(&config.ServiceConfig{
		Name:     serviceNames("s1", 1)[0], // 冒用已开通的服务名，凭据本身有效
		Addr:     addr,
		Listener: &config.ListenerConfig{Type: "tcp"},
		Handler:  &config.HandlerConfig{Type: "http", Auther: AutherName, Chain: "exit-s1-a1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	go svc.Serve()
	defer svc.Close()
	waitListening(t, addr)
	if _, err := tunnel(pHTTP, addr, acc.Username, acc.Password, e.canary.addr); err == nil {
		t.Fatal("untracked connection authenticated")
	}
}

// S-7/A-10：模拟崩溃（不做最终落盘）后重启，累计用量不回退到最后持久值以下，
// 损失不超过一个持久化间隔内的流量；控制面的 UsedFloor 只能抬高用量。
func TestCrashRecoveryLosesAtMostOneFlush(t *testing.T) {
	const flush = 200 * time.Millisecond
	e := newEnv(t, flush)
	acc := e.provision("s1", 1<<30, time.Hour)
	if _, _, err := fetch(pHTTP, acc, e.canary.addr, "/bytes?n=3000000"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * flush)
	persisted := e.a.Used("s1")
	if _, _, err := fetch(pHTTP, acc, e.canary.addr, "/bytes?n=500000"); err != nil {
		t.Fatal(err)
	}
	actual := e.a.Used("s1")

	// “崩溃”：直接用同一存储新建 Agent，原 Agent 不执行 Close 的最终落盘。
	b, err := New(Options{Store: FileStore{Path: e.store}, FlushEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	restored := b.Used("s1")
	if restored < persisted || restored > actual {
		t.Fatalf("restored %d, persisted %d, actual %d", restored, persisted, actual)
	}
	b.Upsert(SubState{ID: "s1", Version: 1, EntitledTotal: 1 << 30, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, UsedFloor: actual})
	if b.Used("s1") != actual {
		t.Fatalf("UsedFloor not applied: %d", b.Used("s1"))
	}
	b.Upsert(SubState{ID: "s1", Version: 2, EntitledTotal: 1 << 30, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, UsedFloor: 0})
	if b.Used("s1") != actual {
		t.Fatal("UsedFloor lowered usage")
	}
	t.Logf("persisted=%d actual=%d restored=%d lost=%d", persisted, actual, restored, actual-restored)
}

// A-20：持久化失败时停止授权并断开连接。
func TestPersistFailureStopsForwarding(t *testing.T) {
	e := newEnv(t, 100*time.Millisecond)
	acc := e.provision("s1", 1<<30, time.Hour)
	live, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	e.ts.fail.Store(true)
	if _, closed := waitClosed(live, stopLimit+200*time.Millisecond); !closed {
		t.Fatal("connection open after persistence failure")
	}
	if _, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr); err == nil {
		t.Fatal("new connection accepted while persistence is failing")
	}
}

// A-09：管理员暂停立即断开；买流量不能解除暂停；恢复后可用。
func TestPauseOverridesTopUp(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	live, err := tunnel(pSOCKS5, acc.SOCKS5Addr, acc.Username, acc.Password, e.canary.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	exp := time.Now().Add(time.Hour)
	e.update(SubState{ID: "s1", EntitledTotal: 1 << 30, ExpiresAt: exp, Enabled: false})
	if _, closed := waitClosed(live, stopLimit); !closed {
		t.Fatal("connection open after pause")
	}
	e.update(SubState{ID: "s1", EntitledTotal: 2 << 30, ExpiresAt: exp, Enabled: false})
	if _, err := tunnel(pHTTP, acc.HTTPAddr, acc.Username, acc.Password, e.canary.addr); err == nil {
		t.Fatal("top-up lifted the pause")
	}
	e.update(SubState{ID: "s1", EntitledTotal: 2 << 30, ExpiresAt: exp, Enabled: true})
	if _, _, err := fetch(pHTTP, acc, e.canary.addr, "/ip"); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

// S-9：普通 HTTP 转发（非 CONNECT）的下载与上传都要计量。
func TestPlainHTTPForwardMetered(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 1<<30, time.Hour)
	proxyURL := &url.URL{Scheme: "http", User: url.UserPassword(acc.Username, acc.Password), Host: acc.HTTPAddr}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}, Timeout: 5 * time.Second}
	target := "http://" + e.canary.addr

	before := e.a.Used("s1")
	resp, err := client.Get(target + "/bytes?n=400000")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if d := e.a.Used("s1") - before; d < 400000 {
		t.Fatalf("plain HTTP download metered %d < 400000", d)
	}

	before = e.a.Used("s1")
	body := strings.Repeat("u", 2_000_000)
	resp, err = client.Post(target+"/sink", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != fmt.Sprint(len(body)) {
		t.Fatalf("upload not delivered: %q", got)
	}
	if d := e.a.Used("s1") - before; d < uint64(len(body)) {
		t.Fatalf("plain HTTP upload metered %d < %d (first request body bypasses the meter)", d, len(body))
	}

	// 每个连接只发一个小请求：正文落在处理器认证前预读的缓冲里，也必须计量。
	before = e.a.Used("s1")
	small := strings.Repeat("s", 3000)
	for i := 0; i < 20; i++ {
		resp, err := client.Post(target+"/sink", "text/plain", strings.NewReader(small))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if d := e.a.Used("s1") - before; d < 20*uint64(len(small)) {
		t.Fatalf("small uploads metered %d < %d", d, 20*len(small))
	}
}

// S-10/A-05：耗尽后上行不会让 goroutine 空转或泄漏。
func TestExhaustedUploadDoesNotSpin(t *testing.T) {
	e := newEnv(t, time.Second)
	acc := e.provision("s1", 200_000, time.Hour)
	runtime.GC()
	base := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for _, p := range protos {
		wg.Add(1)
		go func(p proto) {
			defer wg.Done()
			c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, e.canary.addr)
			if err != nil {
				return
			}
			defer c.Close()
			body := strings.Repeat("u", 5_000_000)
			fmt.Fprintf(c, "POST /sink HTTP/1.1\r\nHost: t\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		}(p)
	}
	wg.Wait()
	if e.a.Usable("s1") {
		t.Fatal("not exhausted")
	}
	eventually(t, 2*time.Second, func() bool { return runtime.NumGoroutine() <= base+2 }, "goroutines did not return to baseline")
	if n := e.a.ConnCount("s1"); n != 0 {
		t.Fatalf("%d connections still tracked", n)
	}
}
