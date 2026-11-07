package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-gost/core/chain"
	xlogger "github.com/go-gost/x/logger"
)

// ExitReason 是出口检测与比较的结论代码，对应控制面检测结果与告警（C-W9）。
type ExitReason string

const (
	ReasonOK ExitReason = "ok"

	// 检测阶段（ProbeExit）。
	ReasonAuthFailed    ExitReason = "auth_failed"    // 上游拒绝凭据（HTTP 407、SOCKS5 认证失败）
	ReasonUnreachable   ExitReason = "unreachable"    // 连不上上游，或在期限内没有应答
	ReasonUpstreamError ExitReason = "upstream_error" // 上游应答了但无法建立隧道（如 502），或出口配置无效
	ReasonBadResponse   ExitReason = "bad_response"   // 回显端点的应答不是 200 加一个 IP

	// 比较阶段（CompareExit）。
	ReasonIPUnchanged     ExitReason = "ip_unchanged"     // 候选出口与当前出口 IP 相同（R-11.3）
	ReasonIPTaken         ExitReason = "ip_taken"         // IP 正被平台内其它订阅使用（平台内独占）
	ReasonCountryMismatch ExitReason = "country_mismatch" // 国家与订阅要求不符
	ReasonCountryUnknown  ExitReason = "country_unknown"  // 无法确定 IP 所属国家
)

// ProbeResult 是一次出口检测的结果。
type ProbeResult struct {
	Class   ExitReason    // ReasonOK 或检测阶段的失败代码
	IP      string        // 回显端点看到的出口 IP（规范形式）；失败时为空
	Latency time.Duration // 从开始拨号到读完应答
	At      time.Time     // 检测开始时间，结果过期可辨（C-W9）
}

// probeSeq 只用于给检测链取名，便于在日志中区分。
var probeSeq atomic.Uint64

// ProbeExit 经候选出口访问 IP 回显端点 echoURL（http 或 https），返回目标侧看到的出口 IP。
//
// 检测走与客户流量相同的 GOST 链（同一 connector），但不经过连接表与认证器，
// 因此不计入任何订阅、不占并发名额、也不受客户出站策略约束（回显端点由运营配置）。
// 回显端点应返回纯文本 IP。ctx 控制总时限。失败时 error 非空，Class 给出原因。
func ProbeExit(ctx context.Context, e Exit, echoURL string) (ProbeResult, error) {
	res := ProbeResult{At: time.Now()}
	fail := func(class ExitReason, err error) (ProbeResult, error) {
		res.Class = class
		res.Latency = time.Since(res.At)
		return res, fmt.Errorf("agent: probe %s: %s: %w", e.Addr, class, err)
	}

	u, err := url.Parse(echoURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return res, fmt.Errorf("agent: probe: echo URL %q must be http(s)", echoURL)
	}
	ch, err := buildChain(fmt.Sprintf("probe-%d", probeSeq.Add(1)), e)
	if err != nil {
		return fail(ReasonUpstreamError, err)
	}

	// 拨号在 Transport 的 goroutine 里进行，超时后仍可能继续，所以结果经原子变量传回。
	var dialErr atomic.Pointer[error]
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			var c net.Conn
			err := error(ErrNoExit)
			if r := ch.Route(ctx, "tcp", addr); r != nil && len(r.Nodes()) > 0 {
				c, err = r.Dial(ctx, "tcp", addr, chain.LoggerDialOption(xlogger.Nop()))
			}
			if err != nil {
				dialErr.Store(&err)
			}
			return c, err
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, echoURL, nil)
	if err != nil {
		return fail(ReasonBadResponse, err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		if de := dialErr.Load(); de != nil {
			return fail(classifyDialErr(ctx, *de), *de)
		}
		if isTimeout(ctx, err) {
			return fail(ReasonUnreachable, err)
		}
		return fail(ReasonBadResponse, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		if isTimeout(ctx, err) {
			return fail(ReasonUnreachable, err)
		}
		return fail(ReasonBadResponse, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fail(ReasonBadResponse, fmt.Errorf("echo status %s", resp.Status))
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil {
		return fail(ReasonBadResponse, fmt.Errorf("echo body %q is not an IP", body))
	}
	res.Class = ReasonOK
	res.IP = ip.Unmap().String()
	res.Latency = time.Since(res.At)
	return res, nil
}

// classifyDialErr 区分“连不上上游”“上游拒绝凭据”“上游建不了隧道”。
// 依据 x v0.28.1 connector 的错误形式：HTTP connector 返回响应状态行（如“407 Proxy
// Authentication Required”），SOCKS5 connector 返回 gosocks5.ErrAuthFailure（“auth failure”）。
// 这不是 GOST 的公开契约，升级后由 TestProbeExit 发现变化。
func classifyDialErr(ctx context.Context, err error) ExitReason {
	var op *net.OpError
	switch {
	case errors.As(err, &op) && op.Op == "dial", isTimeout(ctx, err):
		return ReasonUnreachable
	case strings.Contains(err.Error(), "407"), strings.Contains(err.Error(), "auth failure"):
		return ReasonAuthFailed
	default:
		return ReasonUpstreamError
	}
}

func isTimeout(ctx context.Context, err error) bool {
	var ne net.Error
	return ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// ExitExpectation 是比较候选出口时的约束，全部由调用方（控制面任务）提供。
type ExitExpectation struct {
	CurrentIP string                          // 订阅当前出口的 IP；首次分配时为空
	Country   string                          // 订阅要求的国家（ISO 3166-1 alpha-2）；为空表示不限
	CountryOf func(ip string) (string, error) // IP 到国家的查询（GeoIP 库等）
	TakenIPs  map[string]string               // 平台内其它订阅正在使用的出口 IP -> 订阅 ID
}

// ExitVerdict 是比较结论。
type ExitVerdict struct {
	OK     bool
	Reason ExitReason
	Detail string // 面向运营的说明
}

// CompareExit 判断检测结果能否作为订阅的新出口（R-11.3）：检测必须成功，
// IP 必须与当前出口不同、未被平台内其它订阅占用，且国家符合要求。按此顺序给出第一个不满足的原因。
func CompareExit(r ProbeResult, x ExitExpectation) ExitVerdict {
	fail := func(reason ExitReason, format string, args ...any) ExitVerdict {
		return ExitVerdict{Reason: reason, Detail: fmt.Sprintf(format, args...)}
	}
	if r.Class != ReasonOK {
		class := r.Class
		if class == "" {
			class = ReasonBadResponse
		}
		return fail(class, "probe failed: %s", class)
	}
	ip, err := netip.ParseAddr(r.IP)
	if err != nil {
		return fail(ReasonBadResponse, "probe returned no usable IP %q", r.IP)
	}
	ip = ip.Unmap()
	if cur, err := netip.ParseAddr(x.CurrentIP); err == nil && cur.Unmap() == ip {
		return fail(ReasonIPUnchanged, "candidate exit IP %s equals current exit", ip)
	}
	for other, sub := range x.TakenIPs {
		if o, err := netip.ParseAddr(other); err == nil && o.Unmap() == ip {
			return fail(ReasonIPTaken, "IP %s is in use by subscription %s", ip, sub)
		}
	}
	if x.Country != "" {
		if x.CountryOf == nil {
			return fail(ReasonCountryUnknown, "no country lookup for %s", ip)
		}
		got, err := x.CountryOf(ip.String())
		if err != nil || got == "" {
			return fail(ReasonCountryUnknown, "country of %s unknown: %v", ip, err)
		}
		if !strings.EqualFold(got, x.Country) {
			return fail(ReasonCountryMismatch, "IP %s is in %s, want %s", ip, strings.ToUpper(got), strings.ToUpper(x.Country))
		}
	}
	return ExitVerdict{OK: true, Reason: ReasonOK}
}
