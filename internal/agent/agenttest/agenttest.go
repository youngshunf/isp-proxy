// Package agenttest 是节点 agent 测试共用的本机回环环境：带标签的目标端点、伪 ISP 上游
// （HTTP CONNECT、HTTPS、SOCKS5）、三种协议的客户端，以及基于 agentproto.NewHandler 的伪控制面。
//
// 只监听 127.0.0.1，不接入任何外部网络或付费上游。供 internal/agent/node 与 cmd/gost 的测试使用；
// internal/agent 包内的原型测试仍使用自己的 harness_test.go（包内测试不能依赖本包的导出类型以外的东西）。
package agenttest

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
	"encoding/pem"
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
)

// ---- 目标端点 ----

// Backend 是一个带标签的 HTTP 目标。/ip 返回标签（模拟“目标侧看到的出口”），/echo 返回 EchoIP
// （出口检测的回显端点），/bytes?n= 下发 n 字节，/sink 吞掉请求体。
type Backend struct {
	Label  string
	EchoIP string
	Addr   string
	Hits   atomic.Int64
	// IPHits 只计 /ip（客户请求），不含出口检测的 /echo。
	IPHits atomic.Int64
}

// StartBackend 启动目标端点。
func StartBackend(t testing.TB, label, echoIP string) *Backend {
	t.Helper()
	ln := listen(t)
	b := &Backend{Label: label, EchoIP: echoIP, Addr: ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/ip", func(w http.ResponseWriter, r *http.Request) {
		b.Hits.Add(1)
		b.IPHits.Add(1)
		io.WriteString(w, label)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b.Hits.Add(1)
		io.WriteString(w, echoIP)
	})
	mux.HandleFunc("/bytes", func(w http.ResponseWriter, r *http.Request) {
		b.Hits.Add(1)
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
		b.Hits.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, n)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return b
}

// ---- 伪 ISP 上游 ----

// 上游协议。
const (
	UpstreamHTTP   = "http"
	UpstreamHTTPS  = "https"
	UpstreamSOCKS5 = "socks5"
)

// Upstream 是最小的 ISP 上游代理：校验凭据，忽略请求的目标地址，一律连到自己的 Backend。
// 这样 Backend 的标签就代表“经此上游的出口”。
type Upstream struct {
	Protocol string
	Addr     string
	User     string
	Pass     string
	// CAFile 是 HTTPS 上游的证书（自签，同时作 CA），供节点配置 upstream_tls.ca_file。
	CAFile   string
	Accepted atomic.Int64

	backend string
	ln      net.Listener
}

// StartUpstream 启动一个伪上游。protocol 为 UpstreamHTTP、UpstreamHTTPS 或 UpstreamSOCKS5。
func StartUpstream(t testing.TB, protocol, backendAddr, user, pass string) *Upstream {
	t.Helper()
	ln := listen(t)
	u := &Upstream{Protocol: protocol, Addr: ln.Addr().String(), User: user, Pass: pass, backend: backendAddr, ln: ln}
	if protocol == UpstreamHTTPS {
		cert, key := CertFiles(t, t.TempDir(), "upstream.test")
		u.CAFile = cert
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			t.Fatal(err)
		}
		u.ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{pair}})
	}
	go func() {
		for {
			c, err := u.ln.Accept()
			if err != nil {
				return
			}
			u.Accepted.Add(1)
			go u.serve(c)
		}
	}()
	t.Cleanup(func() { u.ln.Close() })
	return u
}

// Close 停止上游（模拟上游宕机）。
func (u *Upstream) Close() { u.ln.Close() }

func (u *Upstream) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	var ok bool
	if u.Protocol == UpstreamSOCKS5 {
		ok = u.socks5Handshake(c, br)
	} else {
		ok = u.connectHandshake(c, br)
	}
	if !ok {
		return
	}
	bc, err := net.Dial("tcp", u.backend)
	if err != nil {
		return
	}
	defer bc.Close()
	c.SetDeadline(time.Time{})
	if u.Protocol == UpstreamSOCKS5 {
		c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	} else {
		io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(bc, br); done <- struct{}{} }()
	go func() { io.Copy(c, bc); done <- struct{}{} }()
	<-done
}

func (u *Upstream) connectHandshake(c net.Conn, br *bufio.Reader) bool {
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return false
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User+":"+u.Pass))
	if req.Header.Get("Proxy-Authorization") != want {
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
		return false
	}
	return true
}

// socks5Handshake 实现 RFC 1928/1929 的用户名密码认证与 CONNECT 命令（只接受 CONNECT）。
func (u *Upstream) socks5Handshake(c net.Conn, br *bufio.Reader) bool {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 5 {
		return false
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return false
	}
	hasUP := false
	for _, m := range methods {
		hasUP = hasUP || m == 2
	}
	if !hasUP {
		c.Write([]byte{5, 0xff})
		return false
	}
	c.Write([]byte{5, 2})
	ver := make([]byte, 2)
	if _, err := io.ReadFull(br, ver); err != nil {
		return false
	}
	user := make([]byte, ver[1])
	io.ReadFull(br, user)
	pl := make([]byte, 1)
	io.ReadFull(br, pl)
	pass := make([]byte, pl[0])
	io.ReadFull(br, pass)
	if string(user) != u.User || string(pass) != u.Pass {
		c.Write([]byte{1, 1})
		return false
	}
	c.Write([]byte{1, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[1] != 1 {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return false
	}
	var skip int
	switch req[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		l := make([]byte, 1)
		io.ReadFull(br, l)
		skip = int(l[0])
	default:
		return false
	}
	if _, err := io.ReadFull(br, make([]byte, skip+2)); err != nil {
		return false
	}
	return true
}

// ---- 证书 ----

// CertFiles 在 dir 中写一张自签证书（SAN 含 host、127.0.0.1）与私钥，返回文件路径。
func CertFiles(t testing.TB, dir, host string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		DNSNames:              []string{host},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile = filepath.Join(dir, host+".pem")
	keyFile = filepath.Join(dir, host+"-key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// ---- 客户端 ----

// Proto 是客户接入协议。
type Proto int

const (
	HTTP Proto = iota
	HTTPS
	SOCKS5
)

// Protos 是三种接入协议。
var Protos = []Proto{HTTP, HTTPS, SOCKS5}

func (p Proto) String() string { return [...]string{"http", "https", "socks5"}[p] }

// Conn 是经平台入口建立的隧道，统计隧道建立之后客户端收发的应用层字节。
type Conn struct {
	net.Conn
	r  io.Reader
	Rx atomic.Int64 // 隧道建立后读到的字节
	Tx atomic.Int64 // 隧道建立后写出的字节
}

func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.Rx.Add(int64(n))
	return n, err
}

func (c *Conn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.Tx.Add(int64(n))
	return n, err
}

// Tunnel 经平台入口 proxyAddr 建立到 target 的隧道（HTTP/HTTPS 用 CONNECT，SOCKS5 用 CONNECT 命令）。
func Tunnel(p Proto, proxyAddr, user, pass, target string) (*Conn, error) {
	c, err := dialEntry(p, proxyAddr)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	var r io.Reader = c
	switch p {
	case HTTP, HTTPS:
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
	case SOCKS5:
		if err := SOCKS5Auth(c, user, pass); err != nil {
			c.Close()
			return nil, err
		}
		if rep, err := SOCKS5Command(c, 1, target); err != nil || rep != 0 {
			c.Close()
			return nil, fmt.Errorf("socks5 connect: reply %d %v", rep, err)
		}
	}
	c.SetDeadline(time.Time{})
	return &Conn{Conn: c, r: r}, nil
}

func dialEntry(p Proto, addr string) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	if p != HTTPS {
		return raw, nil
	}
	tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	tc.SetDeadline(time.Now().Add(3 * time.Second))
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	return tc, nil
}

// DialSOCKS5 连接 SOCKS5 入口并完成用户名密码认证。
func DialSOCKS5(addr, user, pass string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if err := SOCKS5Auth(c, user, pass); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// SOCKS5Auth 完成方法协商与 RFC 1929 认证。
func SOCKS5Auth(c net.Conn, user, pass string) error {
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
	return nil
}

// SOCKS5Command 发送一条 SOCKS5 请求（1 CONNECT、2 BIND、3 UDP ASSOCIATE），返回应答码。
func SOCKS5Command(c net.Conn, cmd byte, target string) (byte, error) {
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)
	req := []byte{5, cmd, 0}
	switch ip := net.ParseIP(host); {
	case ip != nil && ip.To4() != nil:
		req = append(append(req, 1), ip.To4()...)
	case ip != nil:
		req = append(append(req, 4), ip.To16()...)
	default:
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return 0, err
	}
	rep := make([]byte, 4)
	if _, err := io.ReadFull(c, rep); err != nil {
		return 0, err
	}
	var rest int
	switch rep[3] {
	case 1:
		rest = 4 + 2
	case 4:
		rest = 16 + 2
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return rep[1], err
		}
		rest = int(l[0]) + 2
	default:
		return rep[1], fmt.Errorf("socks5: reply atyp %d", rep[3])
	}
	_, err := io.ReadFull(c, make([]byte, rest))
	return rep[1], err
}

// Get 在隧道上发一个 HTTP/1.1 请求并读完响应体。
func Get(c *Conn, path string) (string, error) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.SetDeadline(time.Time{})
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: target\r\nConnection: close\r\n\r\n", path)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// Fetch 建立隧道、请求 path、关闭，返回响应体与隧道上的应用层字节数。
func Fetch(p Proto, proxyAddr, user, pass, target, path string) (string, int64, error) {
	c, err := Tunnel(p, proxyAddr, user, pass, target)
	if err != nil {
		return "", 0, err
	}
	defer c.Close()
	body, err := Get(c, path)
	return body, c.Rx.Load() + c.Tx.Load(), err
}

// WaitClosed 等待对端关闭连接，返回经过的时间与是否已关闭。
func WaitClosed(c net.Conn, max time.Duration) (time.Duration, bool) {
	start := time.Now()
	c.SetReadDeadline(start.Add(max))
	buf := make([]byte, 1024)
	for {
		if _, err := c.Read(buf); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return time.Since(start), false
			}
			return time.Since(start), true
		}
	}
}

// Eventually 轮询 cond 直到为真，超时则 Fatal。
func Eventually(t testing.TB, max time.Duration, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(max)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf(format, args...)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WaitListening 等待地址开始监听。
func WaitListening(t testing.TB, max time.Duration, addrs ...string) {
	t.Helper()
	for _, addr := range addrs {
		deadline := time.Now().Add(max)
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

// Refused 报告 addr 是否拒绝连接（端口不再监听）。
func Refused(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return true
	}
	c.Close()
	return false
}

// FreePorts 返回 n 个当前空闲的本机端口（互不相同）。
func FreePorts(t testing.TB, n int) []int {
	t.Helper()
	var lns []net.Listener
	var out []int
	for i := 0; i < n; i++ {
		ln := listen(t)
		lns = append(lns, ln)
		out = append(out, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		ln.Close()
	}
	return out
}

// Loopback 返回 127.0.0.1:port。
func Loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

func listen(t testing.TB) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}
