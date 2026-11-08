// Package agentproto 定义控制面（isp-control）与节点 agent 之间的内部协议（docs/06 §5，docs/04 §7）。
// 两端共用本包：消息类型与 JSON 编码、agent 侧 HTTP 客户端、控制面侧处理器与 mTLS 配置。
//
// 传输：agent 主动连接控制面 :8443，HTTPS + 双向证书，节点身份取客户端证书 CN（"node:<id>"）。
//
//	GET  /agent/v1/desired?after=<revision>  长轮询（最多 25 秒）；修订号与 after 不同时立即返回整份期望状态，
//	                                         超时返回 304；不带 after 时立即返回
//	POST /agent/v1/report                    每 ≤ 5 秒一次：各订阅累计 used_total、已应用版本、连接数、persist_ok、节点时钟
//	POST /agent/v1/probe-results             探测结果，按探测 ID 幂等
//
// 语义：期望状态整份下发、声明式对账。agent 只应用版本比已知更高的订阅条目；列表中不再出现的订阅
// 由 agent 撤销接入并断开连接（已释放、已删除）。used_total 是累计值，控制面以 GREATEST 合并，
// 重复、乱序、重放天然幂等。
//
// 兼容性：本包是 M1 冻结的契约。新增字段只能是可选的（零值保持旧语义），不改已有字段含义；
// 不兼容的变化走新路径前缀 /agent/v2/。
package agentproto

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// HTTP 路径。
const (
	PathDesired      = "/agent/v1/desired"
	PathReport       = "/agent/v1/report"
	PathProbeResults = "/agent/v1/probe-results"
)

// 时序约定（docs/06 §5，docs/04 §7）。
const (
	// MaxLongPoll 是期望状态长轮询的最长等待。
	MaxLongPoll = 25 * time.Second
	// ReportInterval 是上报的最长间隔。
	ReportInterval = 5 * time.Second
)

// 上游出口协议（R-03.1）。
const (
	ExitHTTP   = "http"
	ExitHTTPS  = "https" // 到上游的 TLS 上跑 HTTP 代理（GOST http 连接器 + tls 拨号器）
	ExitSOCKS5 = "socks5"
)

// 探测种类（与 probes.kind 一致）。
const (
	ProbeExitCheck        = "exit_check"        // 经订阅当前出口检测实际出口
	ProbeCandidateCompare = "candidate_compare" // 经候选上游检测，控制面再与当前出口比较
	ProbeUpstreamVerify   = "upstream_verify"   // 录入后验证上游认证、连通性与实际出口
)

// 下发失败代码（SubscriptionReport.ApplyError.Code）。
const (
	ApplyPortInUse       = "port_in_use"
	ApplyCertUnavailable = "cert_unavailable"
	ApplyExitInvalid     = "exit_invalid"
	ApplyInternal        = "internal"
)

// Desired 是控制面给某个节点的完整期望状态。
type Desired struct {
	// Revision 是该节点期望状态的全局修订号，单调递增；任何订阅条目或探测列表变化都会使其增大。
	Revision      uint64              `json:"revision"`
	NodeID        string              `json:"node_id"`
	GeneratedAt   time.Time           `json:"generated_at"`
	Subscriptions []SubscriptionEntry `json:"subscriptions"`
	Probes        []Probe             `json:"probes"`
}

// SubscriptionEntry 是一个订阅在节点上的期望状态（docs/04 §5.2）。
// 接入、出口、额度、期限、启用任一变化，控制面都会生成更高的 Version；agent 只接受更高版本。
type SubscriptionEntry struct {
	ID      string `json:"id"`
	Version uint64 `json:"version"`
	// EntitledTotal 是流量账本求和（字节）。
	EntitledTotal uint64 `json:"entitled_total"`
	// ExpiresAt 是到期时间（UTC）。零值表示尚未起算（不可用）。
	ExpiresAt time.Time `json:"expires_at"`
	// Enabled 汇总了管理意图、账号状态、生命周期与任务阻断；为 false 时拒绝新连接并断开存量连接。
	Enabled bool `json:"enabled"`
	// UsedFloor 是控制面已确认的累计用量；agent 取 max(本地值, UsedFloor)。
	UsedFloor uint64 `json:"used_floor"`
	// MaxConns 是并发连接上限（套餐属性）；0 表示不限制。
	MaxConns int `json:"max_conns"`
	// BandwidthBps 是带宽上限（bit/s，1 Mbps = 1,000,000）；0 表示不限制。
	BandwidthBps uint64 `json:"bandwidth_bps"`
	// Access 是当前接入；nil 表示没有接入（不监听任何端口）。
	Access *Access `json:"access"`
	// Exit 是当前出口；nil 表示没有出口，所有连接失败即拒绝（不得直连，R-01.3）。
	Exit *Exit `json:"exit"`
	// Direct 显式标记“服务器直出代理”（D-25，docs/06 §9.2）：只有为 true 的条目经节点本机拨号访问目标
	// （仍经出站策略，另封 465、587）。为 true 时 Exit 必须为空。为 false 且 Exit 为空的条目照旧失败即拒绝，
	// 不得因为本字段的存在而退化为直连（R-01.3）。可选字段，旧 agent 忽略它时直出订阅只会失败即拒绝。
	Direct bool `json:"direct,omitempty"`
	// Unmetered 显式标记“不限流量”（D-25）：agent 不做额度耗尽停用，EntitledTotal 被忽略；仍计量、仍上报累计用量。
	// 期限、启用、并发与带宽上限照常执行。可选字段，旧 agent 忽略它时按 EntitledTotal（直出为 0）拒绝，不会放开额度。
	Unmetered bool `json:"unmetered,omitempty"`
}

// Access 是一个接入版本：三个协议端口加一组连接凭据（R-04.2）。
type Access struct {
	Version  uint64 `json:"version"`
	Username string `json:"username"`
	Password string `json:"password"`
	Ports    Ports  `json:"ports"`
}

// Ports 是接入版本的三个协议端口。
type Ports struct {
	HTTP   int `json:"http"`
	HTTPS  int `json:"https"`
	SOCKS5 int `json:"socks5"`
}

// Exit 是一个出口绑定版本，对应一个上游资源。凭据为明文，只在 mTLS 通道内传输，agent 不得写日志。
type Exit struct {
	Version  uint64 `json:"version"`
	Protocol string `json:"protocol"` // http | https | socks5
	Addr     string `json:"addr"`     // host:port，IPv6 带方括号
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Probe 是一项需要从节点发起的探测。探测流量直接经 Exit 访问 EchoURL，不经过客户接入，不计入任何订阅（R-05.5）。
type Probe struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	SubscriptionID string    `json:"subscription_id,omitempty"`
	Exit           Exit      `json:"exit"`
	EchoURL        string    `json:"echo_url"`
	Deadline       time.Time `json:"deadline"`
}

// Report 是节点的周期上报。节点身份取自客户端证书，不在消息体中声明。
type Report struct {
	NodeTime        time.Time            `json:"node_time"`
	AgentVersion    string               `json:"agent_version"`
	AppliedRevision uint64               `json:"applied_revision"`
	PersistOK       bool                 `json:"persist_ok"`
	Subscriptions   []SubscriptionReport `json:"subscriptions"`
	// ClockSkew 是节点按最近一次回执的 ServerTime 估算的本地时钟偏差（M3 新增，可选；
	// 首次上报或尚无回执时为空）。控制面也可以直接比较 NodeTime 与收到时刻。
	ClockSkew *ClockSkew `json:"clock_skew,omitempty"`
}

// ClockSkew 是一次往返测得的节点时钟偏差区间（节点时钟减控制面时钟，毫秒）：真实偏差落在
// [LowMs, HighMs]。区间由请求发出、收到回执的本地时刻与回执的 ServerTime 得出，宽度约为一次往返。
type ClockSkew struct {
	LowMs  int64 `json:"low_ms"`
	HighMs int64 `json:"high_ms"`
	// Exceeded 表示整个区间都超出 ±MaxClockSkewMs（R-14.7 时钟偏差告警），不会因往返时延误报。
	Exceeded bool `json:"exceeded"`
	// MeasuredAt 是收到该回执时的节点时间。
	MeasuredAt time.Time `json:"measured_at"`
}

// MaxClockSkewMs 是允许的节点时钟偏差（R-14.7）：1 秒。
const MaxClockSkewMs = 1000

// SubscriptionReport 是一个订阅的观测值。
type SubscriptionReport struct {
	ID string `json:"id"`
	// UsedTotal 是累计已用字节（不是增量）。
	UsedTotal uint64 `json:"used_total"`
	// UsedByProtocol 是按入口拆分的累计值（R-02.4），可选。
	UsedByProtocol *ProtocolUsage `json:"used_by_protocol,omitempty"`
	// AppliedVersion 是已成功应用的订阅条目版本。
	AppliedVersion uint64 `json:"applied_version"`
	Conns          int    `json:"conns"`
	// ApplyError 非空表示更高版本下发失败，订阅保持 AppliedVersion 的状态。
	ApplyError *ApplyError `json:"apply_error,omitempty"`
}

// ProtocolUsage 是按入口拆分的累计字节。
type ProtocolUsage struct {
	HTTP   uint64 `json:"http"`
	HTTPS  uint64 `json:"https"`
	SOCKS5 uint64 `json:"socks5"`
}

// ApplyError 描述某个版本下发失败的原因。
type ApplyError struct {
	Version uint64 `json:"version"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ReportAck 是控制面对上报的回执。Revision 大于 agent 已应用的修订号时，agent 应尽快重新拉取。
type ReportAck struct {
	Revision uint64 `json:"revision"`
	// ServerTime 是控制面处理该上报时的时钟，按 RFC 3339 带小数秒编码（time.Time 的 JSON 形式）。
	// 精度必须不低于 1 毫秒（不得截断到秒）：agent 以 1 毫秒精度据此估算时钟偏差（ClockSkew）。
	ServerTime time.Time `json:"server_time"`
}

// ProbeResult 是一项探测的结果。
type ProbeResult struct {
	ProbeID     string    `json:"probe_id"`
	OK          bool      `json:"ok"`
	ObservedIP  string    `json:"observed_ip,omitempty"`
	CountryCode string    `json:"country_code,omitempty"` // 回显端点给出时填写，小写
	LatencyMs   int64     `json:"latency_ms"`
	Error       string    `json:"error,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}

// ProbeResults 是 probe-results 的请求体。
type ProbeResults struct {
	Results []ProbeResult `json:"results"`
}

// Error 是本协议的错误响应体。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ---- 校验 ----

// Validate 检查期望状态的结构性约束；控制面下发前与 agent 应用前都应调用。
func (d *Desired) Validate() error {
	seen := map[string]bool{}
	users := map[string]string{}
	ports := map[int]string{}
	for i := range d.Subscriptions {
		e := &d.Subscriptions[i]
		if e.ID == "" {
			return fmt.Errorf("subscriptions[%d]: 缺少 id", i)
		}
		if seen[e.ID] {
			return fmt.Errorf("订阅 %s 重复", e.ID)
		}
		seen[e.ID] = true
		if e.Version == 0 {
			return fmt.Errorf("订阅 %s: version 必须 > 0", e.ID)
		}
		if e.Enabled && e.ExpiresAt.IsZero() {
			return fmt.Errorf("订阅 %s: 启用的订阅必须有 expires_at", e.ID)
		}
		if e.MaxConns < 0 {
			return fmt.Errorf("订阅 %s: max_conns 不能为负", e.ID)
		}
		if a := e.Access; a != nil {
			if err := a.validate(); err != nil {
				return fmt.Errorf("订阅 %s: %w", e.ID, err)
			}
			if other, ok := users[a.Username]; ok {
				return fmt.Errorf("订阅 %s 与 %s 的接入用户名重复", e.ID, other)
			}
			users[a.Username] = e.ID
			for _, p := range []int{a.Ports.HTTP, a.Ports.HTTPS, a.Ports.SOCKS5} {
				if other, ok := ports[p]; ok {
					return fmt.Errorf("端口 %d 同时分配给 %s 与 %s", p, other, e.ID)
				}
				ports[p] = e.ID
			}
		}
		if e.Exit != nil {
			if e.Direct {
				return fmt.Errorf("订阅 %s: 直出订阅不能带上游出口", e.ID)
			}
			if err := e.Exit.validate(); err != nil {
				return fmt.Errorf("订阅 %s: %w", e.ID, err)
			}
		}
	}
	probes := map[string]bool{}
	for i := range d.Probes {
		p := &d.Probes[i]
		if p.ID == "" || probes[p.ID] {
			return fmt.Errorf("probes[%d]: id 为空或重复", i)
		}
		probes[p.ID] = true
		switch p.Kind {
		case ProbeExitCheck, ProbeCandidateCompare, ProbeUpstreamVerify:
		default:
			return fmt.Errorf("探测 %s: 未知种类 %q", p.ID, p.Kind)
		}
		if err := p.Exit.validate(); err != nil {
			return fmt.Errorf("探测 %s: %w", p.ID, err)
		}
		if p.EchoURL == "" {
			return fmt.Errorf("探测 %s: 缺少 echo_url", p.ID)
		}
	}
	return nil
}

func (a *Access) validate() error {
	if a.Version == 0 {
		return errors.New("access.version 必须 > 0")
	}
	if a.Username == "" || a.Password == "" {
		return errors.New("access 缺少用户名或密码（平台禁止无认证入口，R-04.10）")
	}
	p := a.Ports
	for _, n := range []int{p.HTTP, p.HTTPS, p.SOCKS5} {
		if n < 1 || n > 65535 {
			return fmt.Errorf("access 端口 %d 越界", n)
		}
	}
	if p.HTTP == p.HTTPS || p.HTTP == p.SOCKS5 || p.HTTPS == p.SOCKS5 {
		return errors.New("access 三个端口必须互不相同")
	}
	return nil
}

func (e *Exit) validate() error {
	switch e.Protocol {
	case ExitHTTP, ExitHTTPS, ExitSOCKS5:
	default:
		return fmt.Errorf("exit.protocol %q 不受支持", e.Protocol)
	}
	host, port, err := net.SplitHostPort(e.Addr)
	if err != nil || host == "" {
		return fmt.Errorf("exit.addr %q 不是 host:port", e.Addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("exit.addr 端口 %q 越界", port)
	}
	return nil
}

// Validate 检查上报的结构性约束。
func (r *Report) Validate() error {
	seen := map[string]bool{}
	for i, s := range r.Subscriptions {
		if s.ID == "" || seen[s.ID] {
			return fmt.Errorf("subscriptions[%d]: id 为空或重复", i)
		}
		seen[s.ID] = true
		if s.Conns < 0 {
			return fmt.Errorf("订阅 %s: conns 不能为负", s.ID)
		}
	}
	if r.NodeTime.IsZero() {
		return errors.New("缺少 node_time")
	}
	return nil
}
