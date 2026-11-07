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

// 出口协议（与 agentproto.ExitHTTP 等一致）。
const (
	ExitHTTP   = "http"
	ExitHTTPS  = "https" // 到上游的 TLS 上跑 HTTP 代理：GOST http 连接器 + tls 拨号器
	ExitSOCKS5 = "socks5"
)

// Exit 是一个出口绑定版本，对应一个 ISP 上游资源。
type Exit struct {
	Version  uint64
	Protocol string // ExitHTTP、ExitHTTPS 或 ExitSOCKS5
	Addr     string
	Username string
	Password string

	// 仅 ExitHTTPS：到上游的 TLS 校验。由节点配置给出，不来自控制面。
	// 默认按系统根证书（TLSCAFile 非空时改用该文件）校验上游证书与主机名；
	// TLSInsecure 跳过校验，只用于开发环境。
	TLSCAFile   string
	TLSInsecure bool
}

// exitChain 包装真实链。GOST 在链缺失或选不出节点时会退化为直连
// （x v0.28.1 chain/router.go 与 chain/route.go 的 DefaultRoute），
// 这里在这两种情况下都返回一个必然失败的路由。
//
// 路由入口同时执行出站策略（A-W4）：被封目标同样得到必然失败的路由，
// 上游收不到连接，也不会退化为直连。拒绝不踢连接、不影响同订阅的其它连接。
type exitChain struct {
	a     *Agent // 提供出站策略；为空时使用默认策略
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
	// address 是路由器解析后的地址；节点未配置解析器时仍是主机名，由策略自行解析。
	if err := e.a.egressPolicy().Check(ctx, network, address); err != nil {
		return failRoute{err: err}
	}
	r := h.c.Route(ctx, network, address, opts...)
	if r == nil || len(r.Nodes()) == 0 {
		return failRoute{}
	}
	// 纵深防御：只有经认证器标记为已认证的跟踪连接才能经出口转发。
	// GOST 的认证器注册表在名字缺失时放行（x v0.28.1 registry/auther.go 的 autherWrapper），
	// 这里保证即使认证器被意外注销，未认证、不计量的连接也到不了上游。
	if c := connFromContext(ctx); c == nil || !c.authed.Load() {
		return failRoute{err: ErrNotAuthenticated}
	}
	return r
}

// ErrNotAuthenticated 表示路由请求不来自已认证的跟踪连接。
var ErrNotAuthenticated = errors.New("agent: route requested by unauthenticated connection")

func (e *exitChain) current() Exit {
	if h := e.inner.Load(); h != nil {
		return h.exit
	}
	return Exit{}
}

// failRoute 是必然失败的路由；err 为空时为 ErrNoExit。
type failRoute struct{ err error }

func (f failRoute) error() error {
	if f.err != nil {
		return f.err
	}
	return ErrNoExit
}

func (f failRoute) Dial(context.Context, string, string, ...chain.DialOption) (net.Conn, error) {
	return nil, f.error()
}
func (f failRoute) Bind(context.Context, string, string, ...chain.BindOption) (net.Listener, error) {
	return nil, f.error()
}
func (failRoute) Nodes() []*chain.Node { return nil }

// chainConfig 把出口转换为 GOST 链配置：
//
//	http   → http 连接器 + tcp 拨号器
//	https  → http 连接器 + tls 拨号器（校验上游证书，见 Exit.TLSCAFile）
//	socks5 → socks5 连接器（notls：标准 SOCKS5，不协商 GOST 的 TLS 扩展方法）+ tcp 拨号器
func chainConfig(name string, e Exit) (*config.ChainConfig, error) {
	node := &config.NodeConfig{
		Name: name + "-node",
		Addr: e.Addr,
	}
	switch e.Protocol {
	case ExitHTTP:
		node.Connector = &config.ConnectorConfig{Type: "http"}
		node.Dialer = &config.DialerConfig{Type: "tcp"}
	case ExitHTTPS:
		node.Connector = &config.ConnectorConfig{Type: "http"}
		node.Dialer = &config.DialerConfig{Type: "tls", TLS: &config.TLSConfig{
			CAFile: e.TLSCAFile,
			Secure: !e.TLSInsecure,
		}}
	case ExitSOCKS5:
		node.Connector = &config.ConnectorConfig{Type: "socks5", Metadata: map[string]any{"notls": true}}
		node.Dialer = &config.DialerConfig{Type: "tcp"}
	default:
		return nil, fmt.Errorf("%w: protocol %q", ErrExitInvalid, e.Protocol)
	}
	if _, _, err := net.SplitHostPort(e.Addr); err != nil {
		return nil, fmt.Errorf("%w: addr %q", ErrExitInvalid, e.Addr)
	}
	if e.Username != "" {
		node.Connector.Auth = &config.AuthConfig{Username: e.Username, Password: e.Password}
	}
	return &config.ChainConfig{
		Name: name,
		Hops: []*config.HopConfig{{Name: name + "-hop", Nodes: []*config.NodeConfig{node}}},
	}, nil
}

// ErrExitInvalid 表示出口配置无法构造为 GOST 链（协议不支持、地址格式错误、CA 文件不可读等）。
var ErrExitInvalid = errors.New("agent: invalid exit")

// ValidateExit 检查出口能否构造为 GOST 链，不注册、不拨号。节点运行时在多步变更前先校验，
// 避免前一步已生效、后一步才发现出口无效。
func ValidateExit(e Exit) error {
	_, err := buildChain("validate", e)
	return err
}

func buildChain(name string, e Exit) (chain.Chainer, error) {
	cfg, err := chainConfig(name, e)
	if err != nil {
		return nil, err
	}
	ch, err := chain_parser.ParseChain(cfg, xlogger.Nop())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExitInvalid, err)
	}
	return ch, nil
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
// exit 非空时新接入直接绑定新出口（组合更换）；为空时沿用当前出口，没有当前出口时返回 ErrNoExit。
// 任一步失败时新建对象全部回收，订阅保持原状。
func (a *Agent) ApplyAccess(subID string, acc Access, exit *Exit) error {
	if exit == nil {
		a.mu.Lock()
		s := a.subs[subID]
		var cur *chainHolder
		if s != nil && s.exit != nil {
			cur = s.exit.inner.Load()
		}
		a.mu.Unlock()
		if s == nil {
			return ErrUnknownSubscription
		}
		if cur == nil {
			return ErrNoExit
		}
		e := cur.exit
		exit = &e
	}
	return a.ReplaceAccess(subID, acc, exit)
}

// ReplaceAccess 与 ApplyAccess 相同，但 exit 为空表示新接入没有出口：服务照常监听与认证，
// 所有转发失败即拒绝（期望状态中 access 非空、exit 为空，docs/06 §8.3）。供节点运行时声明式对账使用。
func (a *Agent) ReplaceAccess(subID string, acc Access, exit *Exit) error {
	a.mu.Lock()
	s := a.subs[subID]
	if s == nil {
		a.mu.Unlock()
		return ErrUnknownSubscription
	}
	if old := a.users[acc.Username]; old != nil && old.subID != subID {
		a.mu.Unlock()
		return fmt.Errorf("agent: username %q in use", acc.Username)
	}
	a.mu.Unlock()

	chainName := fmt.Sprintf("exit-%s-a%d", subID, acc.Version)
	ec := &exitChain{a: a}
	if exit != nil {
		inner, err := buildChain(chainName, *exit)
		if err != nil {
			return err
		}
		ec.inner.Store(&chainHolder{c: inner, exit: *exit})
	}
	if err := registry.ChainRegistry().Register(chainName, ec); err != nil {
		return err
	}

	names := serviceNames(subID, acc.Version)
	var svcs []service.Service
	rollback := func() {
		for _, svc := range svcs {
			svc.Close()
		}
		registry.ChainRegistry().Unregister(chainName)
	}
	for _, cfg := range accessServiceConfigs(names, acc, chainName) {
		svc, err := service_parser.ParseService(cfg)
		if err != nil {
			rollback()
			return fmt.Errorf("agent: service %s: %w", cfg.Name, err)
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

// accessServiceConfigs 生成一个接入版本的三个 GOST 服务配置：http（isp-tcp）、https（isp-tls 上的 http）、
// socks5（isp-tcp），共用认证器 isp-auth 与出口链 chainName。
//
// 处理器的扩展能力一律显式关闭（docs/04 §2）：SOCKS5 的 UDP 中继与 BIND、HTTP 的 UDP over HTTP
// 走独立套接字或反向监听，不经连接表，开启即绕过计量与出站封禁；Tor 扩展命令同样关闭；
// SOCKS5 不协商 GOST 的 TLS 扩展方法（notls）。TestAccessServiceConfigsDisableBypasses 断言这些取值。
func accessServiceConfigs(names []string, acc Access, chainName string) []*config.ServiceConfig {
	specs := []struct {
		name, addr, ln, h string
		md                map[string]any
	}{
		{names[0], acc.HTTPAddr, ListenerTCP, "http", map[string]any{"udp": false}},
		{names[1], acc.HTTPSAddr, ListenerTLS, "http", map[string]any{"udp": false}},
		{names[2], acc.SOCKS5Addr, ListenerTCP, "socks5", map[string]any{"udp": false, "bind": false, "tor": false, "notls": true}},
	}
	out := make([]*config.ServiceConfig, 0, len(specs))
	for _, sp := range specs {
		out = append(out, &config.ServiceConfig{
			Name:     sp.name,
			Addr:     sp.addr,
			Listener: &config.ListenerConfig{Type: sp.ln},
			Handler: &config.HandlerConfig{
				Type:     sp.h,
				Auther:   AutherName,
				Chain:    chainName,
				Metadata: sp.md,
			},
		})
	}
	return out
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
	acc := s.access.Access
	a.mu.Unlock()
	return a.UpdateCredentials(subID, acc.Version, acc.Username, password)
}

// UpdateCredentials 原地替换当前接入的凭据（端口与服务不变），记录新的接入版本号，
// 并关闭该订阅的全部存量连接：旧用户名与旧密码立即失效。用于重置密码（A-41）。
func (a *Agent) UpdateCredentials(subID string, accessVersion uint64, username, password string) error {
	a.mu.Lock()
	s := a.subs[subID]
	if s == nil || s.access == nil {
		a.mu.Unlock()
		return ErrUnknownSubscription
	}
	if other := a.users[username]; other != nil && other.subID != subID {
		a.mu.Unlock()
		return fmt.Errorf("agent: username %q in use", username)
	}
	old := a.users[s.access.Username]
	delete(a.users, s.access.Username)
	set := map[string]bool{}
	for _, n := range s.access.names {
		set[n] = true
	}
	if old != nil {
		set = old.services
	}
	// 替换而不是原地修改凭据对象：认证在锁外比较密码。
	a.users[username] = &cred{subID: subID, accessVer: accessVersion, password: password, services: set}
	rec := *s.access
	rec.Version, rec.Username, rec.Password = accessVersion, username, password
	s.access = &rec
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

// ClearExit 移除订阅当前接入的出口（期望状态中 exit 为空）：此后全部转发失败即拒绝，
// 不会退化为直连；存量连接随即关闭。
func (a *Agent) ClearExit(subID string) error {
	a.mu.Lock()
	s := a.subs[subID]
	if s == nil || s.exit == nil || s.access == nil {
		a.mu.Unlock()
		return ErrUnknownSubscription
	}
	ec := s.exit
	a.mu.Unlock()
	ec.inner.Store(nil)
	a.kick(s)
	return nil
}

// CurrentExit 返回订阅当前出口；没有接入或没有出口时 ok 为 false。
func (a *Agent) CurrentExit(subID string) (Exit, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.subs[subID]
	if s == nil || s.exit == nil {
		return Exit{}, false
	}
	h := s.exit.inner.Load()
	if h == nil {
		return Exit{}, false
	}
	return h.exit, true
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
