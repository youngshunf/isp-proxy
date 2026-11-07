// Package agent 是节点代理的计量原型，对应 docs/04-计量与执行方案.md。
//
// 它与 GOST 内核同进程运行，按订阅（subscription）在认证成功后授权字节、
// 维护连接表并在耗尽、到期、暂停或撤销接入时主动关闭全部连接。
// 本包目前只用于原型验证（S-1～S-10），不是生产实现。
package agent

import (
	"crypto/subtle"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// SubState 是控制面下发的订阅期望状态。Version 单调递增，节点只接受更高版本。
type SubState struct {
	ID            string
	Version       uint64
	EntitledTotal uint64 // 账本条目求和得到的净入账字节
	ExpiresAt     time.Time
	Enabled       bool   // 管理意图、账号、生命周期等汇总后的结果
	UsedFloor     uint64 // 控制面最后确认的累计已用量；节点取较大值

	// 套餐限制（A-W3），0 表示不限。
	MaxConns     int   // 同时在线的已认证连接数上限；超出的新连接在认证阶段被拒绝
	BandwidthBps int64 // 带宽上限（字节/秒），上行与下行各自一份、各自等于该值，由订阅全部连接共享
}

// sub 是节点内的订阅运行态。对象一经创建不再替换，连接会持有它。
type sub struct {
	id        string
	version   atomic.Uint64
	entitled  atomic.Uint64
	used      atomic.Uint64
	expiresAt atomic.Int64 // UTC 纳秒
	enabled   atomic.Bool

	maxConns atomic.Int64 // 0 表示不限
	active   atomic.Int64 // 已认证且未关闭的连接数
	up, down bucket       // 上行（读客户端）与下行（写客户端）带宽

	// 以下字段受 Agent.mu 保护。
	timer  *time.Timer
	access *accessRec
	exit   *exitChain
}

// cred 是当前生效接入版本的连接凭据。
type cred struct {
	subID     string
	accessVer uint64
	password  string
	services  map[string]bool
}

// Agent 是节点代理。首期每个节点一个实例；原型中通过 Install 挂到 GOST 注册表。
type Agent struct {
	mu       sync.Mutex
	subs     map[string]*sub
	users    map[string]*cred    // 连接用户名 -> 凭据
	services map[string]string   // GOST service 名 -> 订阅 ID
	conns    map[string]*connSet // GOST service 名 -> 存量连接

	store      Store
	flushEvery time.Duration
	persistOK  atomic.Bool
	now        func() time.Time
	egress     atomic.Pointer[EgressPolicy]

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	kicks atomic.Int64 // 断开事件计数，便于测试观察
}

// Options 配置 Agent。
type Options struct {
	Store      Store         // 为空时不持久化（仅测试）
	FlushEvery time.Duration // 持久化间隔，默认 1 秒（D-17）
	Now        func() time.Time
	Egress     EgressPolicy // 出站策略；零值为默认封禁（A-W4）
}

// New 创建 Agent，并从持久化存储恢复累计用量。
func New(opts Options) (*Agent, error) {
	a := &Agent{
		subs:       map[string]*sub{},
		users:      map[string]*cred{},
		services:   map[string]string{},
		conns:      map[string]*connSet{},
		store:      opts.Store,
		flushEvery: opts.FlushEvery,
		now:        opts.Now,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if a.flushEvery <= 0 {
		a.flushEvery = time.Second
	}
	if a.now == nil {
		a.now = time.Now
	}
	a.persistOK.Store(true)
	a.SetEgressPolicy(opts.Egress)

	if a.store != nil {
		used, err := a.store.Load()
		if err != nil {
			return nil, err
		}
		for id, u := range used {
			s := a.getOrCreate(id)
			s.used.Store(u)
		}
	}
	go a.loop()
	return a, nil
}

// Close 停止后台持久化，并做最后一次落盘。
func (a *Agent) Close() error {
	var err error
	a.stopOnce.Do(func() {
		close(a.stop)
		<-a.done
		err = a.flush()
	})
	return err
}

func (a *Agent) getOrCreate(id string) *sub {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.getOrCreateLocked(id)
}

func (a *Agent) getOrCreateLocked(id string) *sub {
	s := a.subs[id]
	if s == nil {
		s = &sub{id: id}
		a.subs[id] = s
	}
	return s
}

func (a *Agent) lookup(id string) *sub {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.subs[id]
}

// Upsert 应用控制面下发的订阅状态。版本不高于当前时忽略并返回 false。
// 应用后立即重新评估可用性：不可用则断开该订阅全部连接，并重设到期计时器。
func (a *Agent) Upsert(st SubState) bool {
	a.mu.Lock()
	s := a.getOrCreateLocked(st.ID)
	if st.Version <= s.version.Load() {
		a.mu.Unlock()
		return false
	}
	s.version.Store(st.Version)
	s.entitled.Store(st.EntitledTotal)
	s.expiresAt.Store(st.ExpiresAt.UnixNano())
	s.enabled.Store(st.Enabled)
	raiseTo(&s.used, st.UsedFloor)
	s.maxConns.Store(int64(max(st.MaxConns, 0)))
	s.up.set(st.BandwidthBps)
	s.down.set(st.BandwidthBps)

	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if d := st.ExpiresAt.Sub(a.now()); d > 0 {
		s.timer = time.AfterFunc(d, func() { a.kick(s) })
	}
	a.mu.Unlock()

	if !a.usable(s) {
		a.kick(s)
	}
	return true
}

// raiseTo 把 v 提升到至少 floor，只增不减。
func raiseTo(v *atomic.Uint64, floor uint64) {
	for {
		cur := v.Load()
		if cur >= floor || v.CompareAndSwap(cur, floor) {
			return
		}
	}
}

// Used 返回订阅的累计已用量（单调递增），用于上报。
func (a *Agent) Used(id string) uint64 {
	if s := a.lookup(id); s != nil {
		return s.used.Load()
	}
	return 0
}

// Report 返回全部订阅的累计已用量快照。控制面应以 GREATEST 合并，天然幂等。
func (a *Agent) Report() map[string]uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := make(map[string]uint64, len(a.subs))
	for id, s := range a.subs {
		m[id] = s.used.Load()
	}
	return m
}

// Usable 报告订阅当前是否允许转发。
func (a *Agent) Usable(id string) bool {
	s := a.lookup(id)
	return s != nil && a.usable(s)
}

func (a *Agent) usable(s *sub) bool {
	return a.persistOK.Load() &&
		s.enabled.Load() &&
		a.now().UnixNano() < s.expiresAt.Load() &&
		s.used.Load() < s.entitled.Load()
}

// Kicks 返回累计断开事件数。
func (a *Agent) Kicks() int64 { return a.kicks.Load() }

// kick 关闭订阅下全部服务的存量连接。
func (a *Agent) kick(s *sub) {
	a.mu.Lock()
	var victims []*trackedConn
	for svc, id := range a.services {
		if id != s.id {
			continue
		}
		for c := range a.connsLocked(svc) {
			victims = append(victims, c)
		}
	}
	a.mu.Unlock()
	a.kicks.Add(1)
	for _, c := range victims {
		c.abort()
	}
}

// kickServices 关闭指定服务上的存量连接（撤销旧接入时使用）。
func (a *Agent) kickServices(names []string) {
	a.mu.Lock()
	var victims []*trackedConn
	for _, svc := range names {
		for c := range a.connsLocked(svc) {
			victims = append(victims, c)
		}
	}
	a.mu.Unlock()
	for _, c := range victims {
		c.abort()
	}
}

// ConnCount 返回订阅当前的存量连接数。
func (a *Agent) ConnCount(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for svc, sid := range a.services {
		if sid == id {
			n += len(a.connsLocked(svc))
		}
	}
	return n
}

// authenticate 校验凭据：用户名存在、属于该服务所在的接入版本、密码一致、订阅可用。
// 返回订阅 ID 作为 GOST 的 ClientID，计量器以此为 key。
func (a *Agent) authenticate(service, user, password string) (string, bool) {
	a.mu.Lock()
	c := a.users[user]
	var s *sub
	if c != nil {
		s = a.subs[c.subID]
	}
	a.mu.Unlock()
	if c == nil || s == nil || !c.services[service] {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(c.password), []byte(password)) != 1 {
		return "", false
	}
	if !a.usable(s) {
		return "", false
	}
	return s.id, true
}

// loop 周期性持久化累计用量。持久化失败时停止全部授权并断开连接（R-08.6）。
func (a *Agent) loop() {
	defer close(a.done)
	t := time.NewTicker(a.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			if err := a.flush(); err != nil {
				if a.persistOK.Swap(false) {
					a.kickAll()
				}
			} else {
				a.persistOK.Store(true)
			}
		}
	}
}

func (a *Agent) flush() error {
	if a.store == nil {
		return nil
	}
	return a.store.Save(a.Report())
}

func (a *Agent) kickAll() {
	a.mu.Lock()
	subs := make([]*sub, 0, len(a.subs))
	for _, s := range a.subs {
		subs = append(subs, s)
	}
	a.mu.Unlock()
	for _, s := range subs {
		a.kick(s)
	}
}

// ErrUnknownSubscription 表示订阅尚未通过 Upsert 下发。
var ErrUnknownSubscription = errors.New("agent: unknown subscription")
