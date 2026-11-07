package agent

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"

	xctx "github.com/go-gost/x/ctx"
)

// ErrQuota 表示订阅不可用（耗尽、到期、暂停或持久化失败），连接随即关闭。
var ErrQuota = errors.New("agent: subscription not usable")

// readChunk 限制单次上行读取的大小。上行字节只能读到后才知道数量，
// 每个连接最多因此透支一个 readChunk。
const readChunk = 16 * 1024

// 计量位置：连接表包装的客户端连接（HTTPS 入口为 TLS 解密后）。
//
// 早期方案在 GOST 处理器认证后的 TrafficLimiter 接缝上计量，原型证明它有两个缺陷：
//   - 普通 HTTP 转发的首个请求正文经处理器内部的 bufio.Reader 直接读底层连接，
//     完全绕过计量（TestPlainHTTPForwardMetered）；
//   - 上行授权不足时 x 的读包装器进入不触碰连接的缓冲分支，关连接也无法终止空转。
//
// 现方案：GOST service 以连接的 Context() 作为请求上下文（x v0.28.1 service.go），
// 认证器从上下文取回本连接并标记“已认证”。认证前的字节暂存在连接上，
// 认证成功时一次性计入订阅；认证失败的连接从不计入。认证后的读写直接在这里授权。
type trackedConn struct {
	net.Conn
	a       *Agent
	service string
	sub     *sub // 服务所属订阅；接入撤销后仍指向原订阅

	authed  atomic.Bool
	preRead atomic.Uint64 // 认证前读写字节，认证成功时计入
	once    sync.Once
}

type connKey struct{}

func connFromContext(ctx context.Context) *trackedConn {
	c, _ := ctx.Value(connKey{}).(*trackedConn)
	return c
}

// Context 让 GOST service 把本连接放进请求上下文，供认证器回填认证结果。
func (c *trackedConn) Context() context.Context {
	base := context.Background()
	if cc, ok := c.Conn.(xctx.Context); ok {
		if v := cc.Context(); v != nil {
			base = v
		}
	}
	return context.WithValue(base, connKey{}, c)
}

// markAuthed 在认证成功时调用：把认证前的握手字节计入订阅，此后按订阅授权。
func (c *trackedConn) markAuthed() {
	if c.sub == nil || !c.authed.CompareAndSwap(false, true) {
		return
	}
	if n := c.preRead.Swap(0); n > 0 {
		c.sub.used.Add(n)
	}
}

func (c *trackedConn) Read(b []byte) (int, error) {
	if !c.authed.Load() {
		n, err := c.Conn.Read(b)
		c.preRead.Add(uint64(n))
		return n, err
	}
	s := c.sub
	if !c.a.usable(s) {
		c.a.kick(s)
		return 0, ErrQuota
	}
	if len(b) > readChunk {
		b = b[:readChunk]
	}
	n, err := c.Conn.Read(b)
	if n > 0 {
		if used := s.used.Add(uint64(n)); used >= s.entitled.Load() {
			c.a.kick(s)
		}
	}
	return n, err
}

func (c *trackedConn) Write(b []byte) (int, error) {
	if !c.authed.Load() {
		n, err := c.Conn.Write(b)
		c.preRead.Add(uint64(n))
		return n, err
	}
	s := c.sub
	written := 0
	for len(b) > 0 {
		if !c.a.usable(s) {
			c.a.kick(s)
			return written, ErrQuota
		}
		g := s.grant(len(b))
		if g == 0 {
			c.a.kick(s)
			return written, ErrQuota
		}
		n, err := c.Conn.Write(b[:g])
		written += n
		if err != nil {
			return written, err
		}
		b = b[n:]
	}
	return written, nil
}

// grant 为下行严格预授权：并发下总授权不超过剩余额度。授权即计入已用量。
func (s *sub) grant(n int) int {
	for {
		used := s.used.Load()
		ent := s.entitled.Load()
		if used >= ent {
			return 0
		}
		g := uint64(n)
		if rem := ent - used; g > rem {
			g = rem
		}
		if s.used.CompareAndSwap(used, used+g) {
			return int(g)
		}
	}
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.a.untrack(c) })
	return c.Conn.Close()
}

func (c *trackedConn) SyscallConn() (syscall.RawConn, error) {
	if sc, ok := c.Conn.(syscall.Conn); ok {
		return sc.SyscallConn()
	}
	return nil, errors.New("unsupported")
}
