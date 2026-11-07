//go:build ispe2e

package isp

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-gost/gost/internal/agent/agenttest"
)

// Mode 是客户接入方式（R-02）：HTTP 普通转发、HTTP CONNECT、HTTPS 入口（TLS 上的 CONNECT）、SOCKS5。
type Mode int

const (
	Forward Mode = iota
	Connect
	HTTPSEntry
	SOCKS
)

// AllModes 是四种接入方式。
var AllModes = []Mode{Forward, Connect, HTTPSEntry, SOCKS}

// TunnelModes 是能建立隧道（可长时间空闲）的三种方式。
var TunnelModes = []Mode{Connect, HTTPSEntry, SOCKS}

func (m Mode) String() string {
	return [...]string{"http-forward", "http-connect", "https", "socks5"}[m]
}

// Creds 是一份接入（主机、三个端口、用户名、密码）。
type Creds struct {
	Host       string
	Ports      Ports
	User, Pass string
}

// CredsOf 从用户视图的接入取连接信息（主机固定为回环）。
func CredsOf(a *Access) Creds {
	return Creds{Host: "127.0.0.1", Ports: a.Ports, User: a.Username, Pass: a.Password}
}

// WithPass 返回换了密码的副本。
func (c Creds) WithPass(p string) Creds { c.Pass = p; return c }

// Addr 返回该方式使用的入口地址。
func (c Creds) Addr(m Mode) string {
	p := c.Ports.HTTP
	switch m {
	case HTTPSEntry:
		p = c.Ports.HTTPS
	case SOCKS:
		p = c.Ports.SOCKS5
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(p))
}

// countConn 统计 TCP 层收发字节（含握手、TLS 开销）。
type countConn struct {
	net.Conn
	rx, tx atomic.Int64
}

func (c *countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.rx.Add(int64(n))
	return n, err
}

func (c *countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.tx.Add(int64(n))
	return n, err
}

// PConn 是经平台入口建立的连接：隧道方式下已完成握手，Forward 方式下为已认证的保持连接。
type PConn struct {
	net.Conn            // 应用层（TLS 之上）
	Raw      *countConn // TCP 层计数
	br       *bufio.Reader
	mode     Mode
	creds    Creds
	target   string
}

func (p *PConn) Read(b []byte) (int, error) { return p.br.Read(b) }

// RawBytes 返回 TCP 层收发字节合计。
func (p *PConn) RawBytes() int64 { return p.Raw.rx.Load() + p.Raw.tx.Load() }

// ErrRejected 表示入口明确拒绝（407、SOCKS5 认证失败、CONNECT 非 200、连接被拒）。
type ErrRejected struct{ Why string }

func (e *ErrRejected) Error() string { return "rejected: " + e.Why }

// IsRejected 报告 err 是否为入口明确拒绝或连接被拒/被关。
func IsRejected(err error) bool {
	if err == nil {
		return false
	}
	var r *ErrRejected
	if errors.As(err, &r) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false // 超时不是“明确拒绝”
	}
	return true
}

func dialRaw(m Mode, c Creds) (net.Conn, *countConn, error) {
	raw, err := net.DialTimeout("tcp", c.Addr(m), 2*time.Second)
	if err != nil {
		return nil, nil, &ErrRejected{"dial: " + err.Error()}
	}
	cc := &countConn{Conn: raw}
	if m != HTTPSEntry {
		return cc, cc, nil
	}
	tc := tls.Client(cc, &tls.Config{InsecureSkipVerify: true})
	tc.SetDeadline(time.Now().Add(3 * time.Second))
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return nil, nil, &ErrRejected{"tls: " + err.Error()}
	}
	tc.SetDeadline(time.Time{})
	return tc, cc, nil
}

func basic(c Creds) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.User+":"+c.Pass))
}

// Open 建立连接：隧道方式完成 CONNECT / SOCKS5 握手；Forward 方式发一个保持连接的 GET /ip 完成认证。
func Open(m Mode, c Creds, target string) (*PConn, error) {
	conn, cc, err := dialRaw(m, c)
	if err != nil {
		return nil, err
	}
	p := &PConn{Conn: conn, Raw: cc, br: bufio.NewReader(conn), mode: m, creds: c, target: target}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetDeadline(time.Time{})
	switch m {
	case Connect, HTTPSEntry:
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", target, target, basic(c))
		resp, err := http.ReadResponse(p.br, nil)
		if err != nil {
			conn.Close()
			return nil, &ErrRejected{"connect: " + err.Error()}
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			conn.Close()
			return nil, &ErrRejected{"connect: " + resp.Status}
		}
	case SOCKS:
		if err := agenttest.SOCKS5Auth(conn, c.User, c.Pass); err != nil {
			conn.Close()
			return nil, &ErrRejected{"socks5 auth: " + err.Error()}
		}
		rep, err := agenttest.SOCKS5Command(conn, 1, target)
		if err != nil || rep != 0 {
			conn.Close()
			return nil, &ErrRejected{fmt.Sprintf("socks5 connect: reply %d %v", rep, err)}
		}
	case Forward:
		if _, err := p.roundTrip("GET", "/ip", nil, 0, true); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return p, nil
}

// roundTrip 在连接上发一个 HTTP/1.1 请求并读完响应体，返回响应体（下载时只计数不保存）。
func (p *PConn) roundTrip(method, path string, body io.Reader, bodyLen int64, keepAlive bool) (*response, error) {
	conn := p.Conn
	var reqLine, extra string
	if p.mode == Forward {
		reqLine = fmt.Sprintf("%s http://%s%s HTTP/1.1", method, p.target, path)
		extra = "Proxy-Authorization: " + basic(p.creds) + "\r\n"
	} else {
		reqLine = fmt.Sprintf("%s %s HTTP/1.1", method, path)
	}
	connHdr := "close"
	if keepAlive {
		connHdr = "keep-alive"
	}
	hdr := fmt.Sprintf("%s\r\nHost: %s\r\n%sConnection: %s\r\n", reqLine, p.target, extra, connHdr)
	if body != nil {
		hdr += fmt.Sprintf("Content-Length: %d\r\n", bodyLen)
	}
	if _, err := io.WriteString(conn, hdr+"\r\n"); err != nil {
		return nil, err
	}
	if body != nil {
		if _, err := io.Copy(conn, body); err != nil {
			return nil, err
		}
	}
	resp, err := http.ReadResponse(p.br, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if p.mode == Forward && resp.StatusCode == http.StatusProxyAuthRequired {
		return nil, &ErrRejected{"forward: " + resp.Status}
	}
	r := &response{status: resp.StatusCode}
	if strings.HasPrefix(path, "/bytes") {
		r.n, err = io.Copy(io.Discard, resp.Body)
	} else {
		var b []byte
		b, err = io.ReadAll(resp.Body)
		r.body, r.n = string(b), int64(len(b))
	}
	if resp.StatusCode/100 != 2 && err == nil {
		return r, &ErrRejected{fmt.Sprintf("%s %s: HTTP %d %s", method, path, resp.StatusCode, strings.TrimSpace(r.body))}
	}
	return r, err
}

type response struct {
	status int
	body   string
	n      int64
}

// Get 在已建立的连接上请求 path（保持连接）。
func (p *PConn) Get(path string) (string, error) {
	p.Conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer p.Conn.SetDeadline(time.Time{})
	r, err := p.roundTrip("GET", path, nil, 0, true)
	if err != nil {
		return "", err
	}
	return r.body, nil
}

// Fetch 建立连接、请求 path、关闭，返回响应体与 TCP 层字节。
func Fetch(m Mode, c Creds, target, path string) (string, int64, error) {
	if m == Forward {
		conn, cc, err := dialRaw(m, c)
		if err != nil {
			return "", 0, err
		}
		defer conn.Close()
		p := &PConn{Conn: conn, Raw: cc, br: bufio.NewReader(conn), mode: m, creds: c, target: target}
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		r, err := p.roundTrip("GET", path, nil, 0, false)
		if err != nil {
			return "", p.RawBytes(), err
		}
		return r.body, p.RawBytes(), nil
	}
	p, err := Open(m, c, target)
	if err != nil {
		return "", 0, err
	}
	defer p.Close()
	p.Conn.SetDeadline(time.Now().Add(15 * time.Second))
	r, err := p.roundTrip("GET", path, nil, 0, false)
	if err != nil {
		return "", p.RawBytes(), err
	}
	return r.body, p.RawBytes(), nil
}

// Download 经入口下载 n 字节，返回实际收到的正文字节与 TCP 层字节。
func Download(m Mode, c Creds, target string, n int64, timeout time.Duration) (got, raw int64, err error) {
	var p *PConn
	if m == Forward {
		conn, cc, derr := dialRaw(m, c)
		if derr != nil {
			return 0, 0, derr
		}
		p = &PConn{Conn: conn, Raw: cc, br: bufio.NewReader(conn), mode: m, creds: c, target: target}
	} else if p, err = Open(m, c, target); err != nil {
		return 0, 0, err
	}
	defer p.Close()
	p.Conn.SetDeadline(time.Now().Add(timeout))
	r, err := p.roundTrip("GET", "/bytes?n="+strconv.FormatInt(n, 10), nil, 0, false)
	if r != nil {
		got = r.n
	}
	if err == nil && got != n {
		err = fmt.Errorf("download: 收到 %d / %d 字节", got, n)
	}
	return got, p.RawBytes(), err
}

// Upload 经入口上传 n 字节（POST /sink），返回 TCP 层字节。
func Upload(m Mode, c Creds, target string, n int64, timeout time.Duration) (raw int64, err error) {
	var p *PConn
	if m == Forward {
		conn, cc, derr := dialRaw(m, c)
		if derr != nil {
			return 0, derr
		}
		p = &PConn{Conn: conn, Raw: cc, br: bufio.NewReader(conn), mode: m, creds: c, target: target}
	} else if p, err = Open(m, c, target); err != nil {
		return 0, err
	}
	defer p.Close()
	p.Conn.SetDeadline(time.Now().Add(timeout))
	_, err = p.roundTrip("POST", "/sink", io.LimitReader(zeroReader{}, n), n, false)
	return p.RawBytes(), err
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// WaitClosed 等待对端关闭连接，返回经过时间与是否已关闭。
func WaitClosed(p *PConn, max time.Duration) (time.Duration, bool) {
	start := time.Now()
	p.Conn.SetReadDeadline(start.Add(max))
	buf := make([]byte, 4096)
	for {
		if _, err := p.br.Read(buf); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return time.Since(start), false
			}
			return time.Since(start), true
		}
	}
}

// Refused 报告 addr 是否不再监听。
func Refused(addr string) bool { return agenttest.Refused(addr) }
