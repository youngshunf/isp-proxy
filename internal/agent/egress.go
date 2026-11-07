package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrEgressBlocked 表示目标地址被出站策略拒绝（A-W4）。处理器把它当作拨号失败，
// 客户端得到明确错误（HTTP 503、SOCKS5 失败应答），上游不会收到任何连接。
var ErrEgressBlocked = errors.New("agent: egress destination blocked")

// EgressPolicy 是出站封禁策略，在出口链包装器的路由入口执行（exitChain.Route）。
//
// 零值即默认策略：拒绝 25 端口，拒绝私网、环回、链路本地、CGNAT、未指定、组播、保留地址、
// IPv6 ULA/链路本地及内嵌这些 IPv4 的形式，拒绝云元数据地址与名字。主机名先在本节点解析，
// 任一结果被封即拒绝；解析失败或无结果同样拒绝（失败即拒绝）。
//
// 已知残余风险：上游代理会自己再解析一次主机名，DNS 结果在两次解析之间可能变化
// （DNS rebinding）。对目标做 IP 固定会让出口侧的地理 DNS 失效，所以首期不做；
// 由于流量总是经 ISP 上游出站、节点不直连目标，可达的私网只会是上游一侧的网络。
type EgressPolicy struct {
	// AllowCIDRs 中的地址不受地址封禁约束（端口封禁照旧），用于测试与回环 E2E。
	AllowCIDRs []netip.Prefix
	// AllowPorts 中的端口不受端口封禁约束。
	AllowPorts []int
	// Resolver 解析主机名；为空时使用 net.DefaultResolver。测试注入。
	Resolver func(ctx context.Context, host string) ([]netip.Addr, error)
}

// resolveTimeout 是出站检查中单次解析的上限，路由器自身另有 15 秒总超时。
const resolveTimeout = 5 * time.Second

var blockedPorts = []int{25}

// blockedPrefixes 是默认封禁的地址段。
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		// IPv4
		"0.0.0.0/8",      // 未指定 / “本网络”
		"10.0.0.0/8",     // RFC 1918
		"100.64.0.0/10",  // CGNAT（含阿里云元数据 100.100.100.200）
		"127.0.0.0/8",    // 环回
		"169.254.0.0/16", // 链路本地（含 AWS/Azure/GCP 元数据 169.254.169.254）
		"172.16.0.0/12",  // RFC 1918
		"192.0.0.0/24",   // IETF 协议分配（含 Oracle 元数据 192.0.0.192）
		"192.168.0.0/16", // RFC 1918
		"198.18.0.0/15",  // 基准测试网段
		"224.0.0.0/4",    // 组播
		"240.0.0.0/4",    // 保留，含广播 255.255.255.255
		// IPv6
		"::/96",          // 未指定、环回及已废弃的 IPv4 兼容地址
		"::ffff:0:0/96",  // IPv4 映射（检查前已解映射，这里兜底）
		"64:ff9b::/96",   // NAT64 知名前缀，可内嵌私网 IPv4
		"64:ff9b:1::/48", // NAT64 本地前缀
		"100::/64",       // 丢弃前缀
		"2002::/16",      // 6to4（已废弃），可内嵌私网 IPv4
		"fc00::/7",       // ULA（含 AWS 元数据 fd00:ec2::254）
		"fe80::/10",      // 链路本地
		"fec0::/10",      // 站点本地（已废弃）
		"ff00::/8",       // 组播
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// blockedNames 是不经解析直接拒绝的名字（小写、无结尾点）。
var blockedNames = map[string]bool{
	"metadata.google.internal":   true,
	"metadata.goog":              true,
	"metadata":                   true,
	"instance-data":              true,
	"instance-data.ec2.internal": true,
	"localhost":                  true,
}

// Check 判断经出口访问 address（host:port）是否允许。允许时返回 nil；
// 拒绝时返回包装了 ErrEgressBlocked 的错误，附带原因。
func (p *EgressPolicy) Check(ctx context.Context, network, address string) error {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return blockedf("malformed address %q", address)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return blockedf("invalid port in %q", address)
	}
	if slices.Contains(blockedPorts, port) && !slices.Contains(p.AllowPorts, port) {
		return blockedf("port %d", port)
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		return p.checkAddr(ip)
	}

	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if blockedNames[name] || strings.HasSuffix(name, ".localhost") {
		return blockedf("name %q", host)
	}
	// 合法的 DNS 顶级域不会全是数字；“127.1”“2130706433”“0x7f.0.0.1”一类写法
	// 可能被上游按 inet_aton 解释为私网地址，直接拒绝。
	labels := strings.Split(name, ".")
	if isDigits(labels[len(labels)-1]) || slices.ContainsFunc(labels, func(l string) bool { return strings.HasPrefix(l, "0x") }) {
		return blockedf("numeric host %q", host)
	}

	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := p.resolve(ctx, name)
	if err != nil {
		return blockedf("resolve %q: %v", host, err)
	}
	if len(addrs) == 0 {
		return blockedf("resolve %q: no address", host)
	}
	for _, ip := range addrs {
		if err := p.checkAddr(ip); err != nil {
			return fmt.Errorf("%w (resolved from %q)", err, host)
		}
	}
	return nil
}

func (p *EgressPolicy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if p.Resolver != nil {
		return p.Resolver(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (p *EgressPolicy) checkAddr(ip netip.Addr) error {
	ip = ip.WithZone("").Unmap()
	for _, pre := range p.AllowCIDRs {
		if pre.Contains(ip) {
			return nil
		}
	}
	for _, pre := range blockedPrefixes {
		if pre.Contains(ip) {
			return blockedf("address %s in %s", ip, pre)
		}
	}
	return nil
}

func blockedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrEgressBlocked, fmt.Sprintf(format, args...))
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SetEgressPolicy 替换节点的出站策略，对之后的每次路由立即生效。
func (a *Agent) SetEgressPolicy(p EgressPolicy) {
	a.egress.Store(&p)
}

func (a *Agent) egressPolicy() *EgressPolicy {
	if a != nil {
		if p := a.egress.Load(); p != nil {
			return p
		}
	}
	return &EgressPolicy{}
}
