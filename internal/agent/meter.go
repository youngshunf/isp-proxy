package agent

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	xctx "github.com/go-gost/x/ctx"
)

// ErrQuota 表示订阅不可用（耗尽、到期、暂停或持久化失败），连接随即关闭。
var ErrQuota = errors.New("agent: subscription not usable")

// readChunk 限制单次上行读取的大小。上行字节只能读到后才知道数量，
// 每个连接最多因此透支一个 readChunk。限速时它也是下行的写入粒度。
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
//
// 并发连接上限与带宽上限（A-W3）也在这里执行，而不用 GOST 的 limiter/conn、limiter/traffic：
//   - limiter/conn 挂在监听器上、按来源 IP 计数，不知道连接属于哪个订阅，也区分不了已认证与否；
//   - limiter/traffic 挂在处理器的同一接缝上，与计量一样会被普通 HTTP 转发的首个请求正文绕过，
//     且等待令牌时不响应本包的“踢连接”。
//
// 一个订阅的全部连接（三种协议）共享两只令牌桶：上行（读客户端）与下行（写客户端）各一只，
// 速率都等于套餐的 BandwidthBps。
type trackedConn struct {
	net.Conn
	a       *Agent
	service string
	src     string // RemoteAddr().String()，Accept 时确定，用于 connBySrc 索引
	sub     *sub   // 服务所属订阅；接入撤销后仍指向原订阅

	authMu  sync.Mutex
	authed  atomic.Bool
	slot    atomic.Bool   // 是否占用订阅的并发名额
	preRead atomic.Uint64 // 认证前读写字节，认证成功时计入

	closed   chan struct{} // 连接被关闭或被踢时关闭，用于打断限速等待
	downOnce sync.Once
}

func newTrackedConn(c net.Conn, a *Agent, service string) *trackedConn {
	tc := &trackedConn{Conn: c, a: a, service: service, closed: make(chan struct{})}
	if ra := c.RemoteAddr(); ra != nil {
		tc.src = ra.String()
	}
	return tc
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

// markAuthed 在认证成功时调用：占用一个并发名额，把认证前的握手字节计入订阅，
// 此后按订阅授权。名额已满时返回 false，认证随之失败。同一连接重复调用只占一个名额。
func (c *trackedConn) markAuthed() bool {
	if c.sub == nil {
		return false
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.authed.Load() {
		return true
	}
	if !c.sub.acquireSlot() {
		return false
	}
	c.slot.Store(true)
	select {
	case <-c.closed: // 与关闭竞争：已关闭的连接不应占着名额
		c.releaseSlot()
		return false
	default:
	}
	c.authed.Store(true)
	if n := c.preRead.Swap(0); n > 0 {
		c.sub.used.Add(n)
	}
	return true
}

func (c *trackedConn) releaseSlot() {
	if c.slot.Swap(false) {
		c.sub.active.Add(-1)
	}
}

// shutdown 注销连接、归还名额并打断等待。由 Close 与“踢连接”共同调用，只执行一次。
func (c *trackedConn) shutdown() {
	c.downOnce.Do(func() {
		close(c.closed)
		c.a.untrack(c)
		c.releaseSlot()
	})
}

// abort 是“踢连接”：立即关闭底层连接，正在读写或等待令牌的 goroutine 随即返回错误。
func (c *trackedConn) abort() {
	c.shutdown()
	c.Conn.Close()
}

// pause 等待 d，连接被关闭时提前返回错误。
func (c *trackedConn) pause(d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-c.closed:
		return net.ErrClosed
	}
}

func (c *trackedConn) Read(b []byte) (int, error) {
	if !c.authed.Load() {
		n, err := c.Conn.Read(b)
		c.preRead.Add(uint64(n))
		return n, err
	}
	s := c.sub
	if s.up.limited() {
		if err := c.pause(s.up.debt()); err != nil {
			return 0, err
		}
	}
	if !c.a.usable(s) {
		c.a.kick(s)
		return 0, ErrQuota
	}
	if len(b) > readChunk {
		b = b[:readChunk]
	}
	n, err := c.Conn.Read(b)
	if n > 0 {
		s.up.take(n)
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
		piece := len(b)
		if s.down.limited() {
			// 先拿带宽令牌再授权额度：等待期间被踢的字节不会被计入。
			piece = min(piece, readChunk)
			if err := c.pause(s.down.take(piece)); err != nil {
				return written, err
			}
		}
		if !c.a.usable(s) {
			c.a.kick(s)
			return written, ErrQuota
		}
		g := s.grant(piece)
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

// acquireSlot 占用一个并发连接名额；maxConns 为 0 表示不限。
// 上限调低时不踢已有连接，只拒绝新连接。
func (s *sub) acquireSlot() bool {
	for {
		cur := s.active.Load()
		if max := s.maxConns.Load(); max > 0 && cur >= max {
			return false
		}
		if s.active.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (c *trackedConn) Close() error {
	c.shutdown()
	return c.Conn.Close()
}

func (c *trackedConn) SyscallConn() (syscall.RawConn, error) {
	if sc, ok := c.Conn.(syscall.Conn); ok {
		return sc.SyscallConn()
	}
	return nil, errors.New("unsupported")
}

// ---- 令牌桶 ----

// bucket 是一个订阅一个方向的带宽令牌桶，被该订阅的全部连接共享。
// 令牌可以透支为负：调用方先扣再等，并发连接因此按到达顺序排队，
// 总速率不超过上限；单次透支不超过一个写块或读块。
type bucket struct {
	bps    atomic.Int64 // 字节/秒；0 表示不限
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// burstOf 返回突发额度：约 100 ms 的流量，至少 32 KiB。
func burstOf(bps int64) int64 {
	return max(bps/10, 32*1024)
}

func (b *bucket) limited() bool { return b.bps.Load() > 0 }

// set 更新速率。速率变化时令牌重置为满桶。
func (b *bucket) set(bps int64) {
	if bps < 0 {
		bps = 0
	}
	b.mu.Lock()
	if b.bps.Swap(bps) != bps {
		b.tokens = float64(burstOf(bps))
		b.last = time.Now()
	}
	b.mu.Unlock()
}

func (b *bucket) refillLocked(now time.Time, bps int64) {
	b.tokens += now.Sub(b.last).Seconds() * float64(bps)
	if burst := float64(burstOf(bps)); b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
}

// take 扣除 n 个令牌，返回令牌回到非负前需要等待的时长。
func (b *bucket) take(n int) time.Duration {
	bps := b.bps.Load()
	if bps <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now(), bps)
	b.tokens -= float64(n)
	return b.waitLocked(bps)
}

// debt 返回当前欠账还清前需要等待的时长，不扣令牌。
func (b *bucket) debt() time.Duration {
	bps := b.bps.Load()
	if bps <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now(), bps)
	return b.waitLocked(bps)
}

func (b *bucket) waitLocked(bps int64) time.Duration {
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / float64(bps) * float64(time.Second))
}
