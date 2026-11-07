package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/go-gost/core/auth"
	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/service"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/config/parsing"
	chain_parser "github.com/go-gost/x/config/parsing/chain"
	service_parser "github.com/go-gost/x/config/parsing/service"
	xctx "github.com/go-gost/x/ctx"
	xlogger "github.com/go-gost/x/logger"
	"github.com/go-gost/x/registry"
)

// 注册到 GOST 的组件名。
const (
	ListenerTCP = "isp-tcp"
	ListenerTLS = "isp-tls"
	AutherName  = "isp-auth"
)

// ErrNoExit 表示订阅没有可用出口。出口失败必须显式失败，不能退化为直连（R-01.3）。
var ErrNoExit = errors.New("agent: no usable exit")

var (
	current     atomic.Pointer[Agent]
	installOnce sync.Once
)

// Install 把 a 设为当前节点代理，并（仅首次）向 GOST 注册带连接表的监听器和认证器。
// tlsCfg 用作 HTTPS 入口的默认证书配置；为空且 GOST 尚无默认配置时 HTTPS 入口不可用。
func Install(a *Agent, tlsCfg *tls.Config) {
	current.Store(a)
	if tlsCfg != nil {
		parsing.SetDefaultTLSConfig(tlsCfg)
	}
	installOnce.Do(func() {
		registry.ListenerRegistry().Register(ListenerTCP, trackingFactory("tcp"))
		registry.ListenerRegistry().Register(ListenerTLS, trackingFactory("tls"))
		registry.AutherRegistry().Register(AutherName, globalAuther{})
	})
}

type globalAuther struct{}

func (globalAuther) Authenticate(ctx context.Context, user, password string, opts ...auth.Option) (string, bool) {
	a := current.Load()
	if a == nil {
		return "", false
	}
	var o auth.Options
	for _, opt := range opts {
		opt(&o)
	}
	id, ok := a.authenticate(o.Service, user, password)
	if !ok {
		return "", false
	}
	// 找到本次认证对应的连接并标记为已认证；找不到就拒绝，
	// 绝不允许“认证通过但不计量”的连接存在。
	c := connFromContext(ctx)
	if c == nil {
		c = a.connBySrc(o.Service, xctx.SrcAddrFromContext(ctx))
	}
	if c == nil || c.sub == nil || c.sub.id != id {
		return "", false
	}
	// 并发连接已满时同样以认证失败明确拒绝（HTTP 407、SOCKS5 认证失败），不挂起。
	if !c.markAuthed() {
		return "", false
	}
	return id, true
}

// ---- 连接表 ----

func trackingFactory(base string) registry.NewListener {
	return func(opts ...listener.Option) listener.Listener {
		var o listener.Options
		for _, opt := range opts {
			opt(&o)
		}
		orig := registry.ListenerRegistry().Get(base)
		return &trackingListener{Listener: orig(opts...), service: o.Service}
	}
}

type trackingListener struct {
	listener.Listener
	service string
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	a := current.Load()
	if a == nil {
		c.Close()
		return nil, errors.New("agent: not installed")
	}
	tc := newTrackedConn(c, a, l.service)
	a.track(tc) // 同时根据服务名确定所属订阅
	return tc, nil
}

// ---- 出口：失败即拒绝的链 ----

// Exit 是一个出口绑定版本，对应一个 ISP 上游资源。
type Exit struct {
	Version  uint64
	Protocol string // "http" 或 "socks5"
	Addr     string
	Username string
	Password string
}

// exitChain 包装真实链。GOST 在链缺失或选不出节点时会退化为直连
// （x v0.28.1 chain/router.go 与 chain/route.go 的 DefaultRoute），
// 这里在这两种情况下都返回一个必然失败的路由。
type exitChain struct {
	inner atomic.Pointer[chainHolder]
}

type chainHolder struct {
	c    chain.Chainer
	exit Exit
}

func (e *exitChain) Route(ctx context.Context, network, address string, opts ...chain.RouteOption) chain.Route {
	h := e.inner.Load()
	if h == nil {
		return failRoute{}
	}
	r := h.c.Route(ctx, network, address, opts...)
	if r == nil || len(r.Nodes()) == 0 {
		return failRoute{}
	}
	return r
}

func (e *exitChain) current() Exit {
	if h := e.inner.Load(); h != nil {
		return h.exit
	}
	return Exit{}
}

type failRoute struct{}

func (failRoute) Dial(context.Context, string, string, ...chain.DialOption) (net.Conn, error) {
	return nil, ErrNoExit
}
func (failRoute) Bind(context.Context, string, string, ...chain.BindOption) (net.Listener, error) {
	return nil, ErrNoExit
}
func (failRoute) Nodes() []*chain.Node { return nil }

func buildChain(name string, e Exit) (chain.Chainer, error) {
	node := &config.NodeConfig{
		Name:      name + "-node",
		Addr:      e.Addr,
		Connector: &config.ConnectorConfig{Type: e.Protocol},
		Dialer:    &config.DialerConfig{Type: "tcp"},
	}
	if e.Username != "" {
		node.Connector.Auth = &config.AuthConfig{Username: e.Username, Password: e.Password}
	}
	return chain_parser.ParseChain(&config.ChainConfig{
		Name: name,
		Hops: []*config.HopConfig{{Name: name + "-hop", Nodes: []*config.NodeConfig{node}}},
	}, xlogger.Nop())
}

// ---- 接入 ----

// Access 是一个接入版本：三个协议端口加一组连接凭据。
type Access struct {
	Version    uint64
	Username   string
	Password   string
	HTTPAddr   string
	HTTPSAddr  string
	SOCKS5Addr string
}

type accessRec struct {
	Access
	chainName string
	names     []string
	services  []service.Service
}

func serviceNames(subID string, ver uint64) []string {
	p := fmt.Sprintf("acc-%s-v%d-", subID, ver)
	return []string{p + "http", p + "https", p + "socks5"}
}

// ApplyAccess 为订阅签发新接入版本并撤销旧版本（换接入）。
// exit 非空时新接入直接绑定新出口（组合更换）；为空时沿用当前出口。
// 任一步失败时新建对象全部回收，订阅保持原状。
func (a *Agent) ApplyAccess(subID string, acc Access, exit *Exit) error {
	a.mu.Lock()
	s := a.subs[subID]
	if s == nil {
		a.mu.Unlock()
		return ErrUnknownSubscription
	}
	var e Exit
	switch {
	case exit != nil:
		e = *exit
	case s.exit != nil:
		e = s.exit.current()
	default:
		a.mu.Unlock()
		return ErrNoExit
	}
	if old := a.users[acc.Username]; old != nil && old.subID != subID {
		a.mu.Unlock()
		return fmt.Errorf("agent: username %q in use", acc.Username)
	}
	a.mu.Unlock()

	chainName := fmt.Sprintf("exit-%s-a%d", subID, acc.Version)
	inner, err := buildChain(chainName, e)
	if err != nil {
		return err
	}
	ec := &exitChain{}
	ec.inner.Store(&chainHolder{c: inner, exit: e})
	if err := registry.ChainRegistry().Register(chainName, ec); err != nil {
		return err
	}

	names := serviceNames(subID, acc.Version)
	specs := []struct{ name, addr, ln, h string }{
		{names[0], acc.HTTPAddr, ListenerTCP, "http"},
		{names[1], acc.HTTPSAddr, ListenerTLS, "http"},
		{names[2], acc.SOCKS5Addr, ListenerTCP, "socks5"},
	}
	var svcs []service.Service
	rollback := func() {
		for _, svc := range svcs {
			svc.Close()
		}
		registry.ChainRegistry().Unregister(chainName)
	}
	for _, sp := range specs {
		svc, err := service_parser.ParseService(&config.ServiceConfig{
			Name:     sp.name,
			Addr:     sp.addr,
			Listener: &config.ListenerConfig{Type: sp.ln},
			Handler: &config.HandlerConfig{
				Type:   sp.h,
				Auther: AutherName,
				Chain:  chainName,
			},
		})
		if err != nil {
			rollback()
			return fmt.Errorf("agent: service %s: %w", sp.name, err)
		}
		svcs = append(svcs, svc)
	}

	rec := &accessRec{Access: acc, chainName: chainName, names: names, services: svcs}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}

	a.mu.Lock()
	old := s.access
	if old != nil && old.Username != acc.Username {
		delete(a.users, old.Username) // 旧凭据立即失效
	}
	for _, n := range names {
		a.services[n] = subID
	}
	a.users[acc.Username] = &cred{subID: subID, accessVer: acc.Version, password: acc.Password, services: set}
	s.access = rec
	s.exit = ec
	a.mu.Unlock()

	for _, svc := range svcs {
		go svc.Serve()
	}

	if old != nil {
		a.retire(old)
	}
	return nil
}

// retire 关闭旧接入：停止监听、关闭存量连接、回收链。
func (a *Agent) retire(old *accessRec) {
	for _, svc := range old.services {
		svc.Close()
	}
	a.kickServices(old.names)
	a.mu.Lock()
	for _, n := range old.names {
		delete(a.services, n)
		delete(a.conns, n)
	}
	a.mu.Unlock()
	registry.ChainRegistry().Unregister(old.chainName)
}

// ResetPassword 只替换连接密码，端口不变，并关闭存量连接。
func (a *Agent) ResetPassword(subID, password string) error {
	a.mu.Lock()
	s := a.subs[subID]
	if s == nil || s.access == nil {
		a.mu.Unlock()
		return ErrUnknownSubscription
	}
	s.access.Password = password
	if c := a.users[s.access.Username]; c != nil {
		c.password = password
	}
	a.mu.Unlock()
	a.kick(s)
	return nil
}

// ApplyExit 原子替换订阅当前接入使用的出口（换出口），并关闭经旧出口的存量连接。
func (a *Agent) ApplyExit(subID string, e Exit) error {
	a.mu.Lock()
	s := a.subs[subID]
	if s == nil || s.exit == nil || s.access == nil {
		a.mu.Unlock()
		return ErrUnknownSubscription
	}
	ec, name := s.exit, s.access.chainName
	a.mu.Unlock()

	inner, err := buildChain(fmt.Sprintf("%s-x%d", name, e.Version), e)
	if err != nil {
		return err
	}
	ec.inner.Store(&chainHolder{c: inner, exit: e})
	a.kick(s)
	return nil
}

// CurrentExit 返回订阅当前出口。
func (a *Agent) CurrentExit(subID string) (Exit, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.subs[subID]
	if s == nil || s.exit == nil {
		return Exit{}, false
	}
	return s.exit.current(), true
}

// Shutdown 关闭全部接入服务（测试清理用）。
func (a *Agent) Shutdown() {
	a.mu.Lock()
	var recs []*accessRec
	for _, s := range a.subs {
		if s.access != nil {
			recs = append(recs, s.access)
			s.access = nil
		}
	}
	a.mu.Unlock()
	for _, r := range recs {
		a.retire(r)
	}
}
