package agent

// 原型测试的本地环境：伪 ISP 上游、带标签的目标端点、直连金丝雀，以及三种协议的客户端。
// 不接入任何外部网络或付费上游。

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gost/core/logger"
	xlogger "github.com/go-gost/x/logger"

	_ "github.com/go-gost/x/connector/http"
	_ "github.com/go-gost/x/dialer/tcp"
	_ "github.com/go-gost/x/handler/http"
	_ "github.com/go-gost/x/handler/socks/v5"
	_ "github.com/go-gost/x/listener/tcp"
	_ "github.com/go-gost/x/listener/tls"
)

func TestMain(m *testing.M) {
	logger.SetDefault(xlogger.Nop())
	os.Exit(m.Run())
}

// ---- 目标端点 ----

// backend 是一个带标签的 HTTP 目标。/ip 返回标签，模拟“目标侧看到的出口 IP”。
type backend struct {
	label string
	addr  string
	hits  atomic.Int64
	srv   *http.Server
}

func startBackend(t *testing.T, label string) *backend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{label: label, addr: ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/ip", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		io.WriteString(w, label)
	})
	mux.HandleFunc("/bytes", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		w.Header().Set("Content-Length", strconv.Itoa(n))
		buf := make([]byte, 32*1024)
		for n > 0 {
			k := min(n, len(buf))
			if _, err := w.Write(buf[:k]); err != nil {
				return
			}
			n -= k
		}
	})
	mux.HandleFunc("/sink", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, n)
	})
	b.srv = &http.Server{Handler: mux}
	go b.srv.Serve(ln)
	t.Cleanup(func() { b.srv.Close() })
	return b
}

// ---- 伪 ISP 上游 ----

// fakeUpstream 是最小的 HTTP CONNECT 代理：校验 Basic 认证，忽略请求的目标地址，
// 一律连到自己的 backend。这样 backend 的标签就代表“经此上游的出口”。
type fakeUpstream struct {
	addr     string
	user     string
	pass     string
	backend  string
	ln       net.Listener
	accepted atomic.Int64
}

func startUpstream(t *testing.T, backendAddr, user, pass string) *fakeUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &fakeUpstream{addr: ln.Addr().String(), user: user, pass: pass, backend: backendAddr, ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u.accepted.Add(1)
			go u.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return u
}

func (u *fakeUpstream) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(u.user+":"+u.pass))
	if req.Header.Get("Proxy-Authorization") != want {
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
		return
	}
	bc, err := net.Dial("tcp", u.backend)
	if err != nil {
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer bc.Close()
	io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { io.Copy(bc, br); done <- struct{}{} }()
	go func() { io.Copy(c, bc); done <- struct{}{} }()
	<-done
}

// ---- 证书 ----

func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "proxy.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"proxy.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// ---- 环境 ----

type env struct {
	t       *testing.T
	a       *Agent
	store   string
	ts      *toggleStore
	canary  *backend
	backA   *backend
	backB   *backend
	upA     *fakeUpstream
	upB     *fakeUpstream
	exitA   Exit
	exitB   Exit
	nextVer atomic.Uint64
}

func newEnv(t *testing.T, flush time.Duration) *env {
	t.Helper()
	e := &env{t: t, store: filepath.Join(t.TempDir(), "used.json")}
	e.ts = &toggleStore{inner: FileStore{Path: e.store}}
	a, err := New(Options{Store: e.ts, FlushEvery: flush})
	if err != nil {
		t.Fatal(err)
	}
	e.a = a
	Install(a, selfSignedTLS(t))
	t.Cleanup(func() {
		a.Shutdown()
		a.Close()
	})
	e.canary = startBackend(t, "DIRECT")
	e.backA = startBackend(t, "exit-A")
	e.backB = startBackend(t, "exit-B")
	e.upA = startUpstream(t, e.backA.addr, "ua", "pa")
	e.upB = startUpstream(t, e.backB.addr, "ub", "pb")
	e.exitA = Exit{Version: 1, Protocol: "http", Addr: e.upA.addr, Username: "ua", Password: "pa"}
	e.exitB = Exit{Version: 2, Protocol: "http", Addr: e.upB.addr, Username: "ub", Password: "pb"}
	return e
}

// toggleStore 可在运行中切换为写入失败，用于模拟磁盘故障。
type toggleStore struct {
	inner Store
	fail  atomic.Bool
}

func (s *toggleStore) Load() (map[string]uint64, error) { return s.inner.Load() }

func (s *toggleStore) Save(m map[string]uint64) error {
	if s.fail.Load() {
		return io.ErrShortWrite
	}
	return s.inner.Save(m)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func (e *env) newAccess(ver uint64, user string) Access {
	return Access{
		Version: ver, Username: user, Password: "pw-" + user,
		HTTPAddr: freeAddr(e.t), HTTPSAddr: freeAddr(e.t), SOCKS5Addr: freeAddr(e.t),
	}
}

// provision 下发订阅状态并开通首个接入，出口为 A。
func (e *env) provision(id string, entitled uint64, ttl time.Duration) Access {
	e.t.Helper()
	e.a.Upsert(SubState{ID: id, Version: e.nextVer.Add(1), EntitledTotal: entitled, ExpiresAt: time.Now().Add(ttl), Enabled: true})
	acc := e.newAccess(1, "u-"+id)
	if err := e.a.ApplyAccess(id, acc, &e.exitA); err != nil {
		e.t.Fatal(err)
	}
	waitListening(e.t, acc.HTTPAddr, acc.HTTPSAddr, acc.SOCKS5Addr)
	return acc
}

func (e *env) update(st SubState) {
	st.Version = e.nextVer.Add(1)
	e.a.Upsert(st)
}

func waitListening(t *testing.T, addrs ...string) {
	t.Helper()
	for _, addr := range addrs {
		deadline := time.Now().Add(2 * time.Second)
		for {
			c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err == nil {
				c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not listening: %v", addr, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// ---- 客户端 ----

type proto int

const (
	pHTTP proto = iota
	pHTTPS
	pSOCKS5
)

func (p proto) String() string { return [...]string{"http", "https", "socks5"}[p] }

func (p proto) addr(acc Access) string {
	return [...]string{acc.HTTPAddr, acc.HTTPSAddr, acc.SOCKS5Addr}[p]
}

// countingConn 统计隧道建立之后客户端收发的应用层字节。
type countingConn struct {
	net.Conn
	r     io.Reader
	read  atomic.Int64
	wrote atomic.Int64
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.read.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.wrote.Add(int64(n))
	return n, err
}

// tunnel 经平台入口建立到 target 的隧道（HTTP/HTTPS 用 CONNECT，SOCKS5 用 CONNECT 命令）。
func tunnel(p proto, proxyAddr, user, pass, target string) (*countingConn, error) {
	raw, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	var c net.Conn = raw
	if p == pHTTPS {
		tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
		if err := tc.Handshake(); err != nil {
			raw.Close()
			return nil, err
		}
		c = tc
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	var r io.Reader = c
	switch p {
	case pHTTP, pHTTPS:
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth)
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			c.Close()
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			c.Close()
			return nil, fmt.Errorf("connect: %s", resp.Status)
		}
		r = br
	case pSOCKS5:
		if err := socks5Connect(c, user, pass, target); err != nil {
			c.Close()
			return nil, err
		}
	}
	c.SetDeadline(time.Time{})
	return &countingConn{Conn: c, r: r}, nil
}

func socks5Connect(c net.Conn, user, pass, target string) error {
	if _, err := c.Write([]byte{5, 1, 2}); err != nil {
		return err
	}
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	if b[1] != 2 {
		return fmt.Errorf("socks5: method %d", b[1])
	}
	msg := []byte{1, byte(len(user))}
	msg = append(msg, user...)
	msg = append(msg, byte(len(pass)))
	msg = append(msg, pass...)
	if _, err := c.Write(msg); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	if b[1] != 0 {
		return errors.New("socks5: auth failed")
	}
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)
	req := []byte{5, 1, 0, 1}
	req = append(req, net.ParseIP(host).To4()...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return err
	}
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil {
		return err
	}
	if rep[1] != 0 {
		return fmt.Errorf("socks5: reply %d", rep[1])
	}
	return nil
}

// get 在隧道上发一个 HTTP/1.1 请求并读完响应体，返回响应体。
func get(c *countingConn, path string) (string, error) {
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: target\r\nConnection: close\r\n\r\n", path)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// fetch 建立隧道、请求 path、关闭，返回响应体和隧道上的应用层字节数。
func fetch(p proto, acc Access, target, path string) (string, int64, error) {
	c, err := tunnel(p, p.addr(acc), acc.Username, acc.Password, target)
	if err != nil {
		return "", 0, err
	}
	defer c.Close()
	body, err := get(c, path)
	return body, c.read.Load() + c.wrote.Load(), err
}

// waitClosed 等待对端关闭连接，返回从调用起经过的时间。
func waitClosed(c net.Conn, max time.Duration) (time.Duration, bool) {
	start := time.Now()
	c.SetReadDeadline(start.Add(max))
	buf := make([]byte, 1024)
	for {
		_, err := c.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return time.Since(start), false
			}
			return time.Since(start), true
		}
	}
}

func eventually(t *testing.T, max time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(max)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
