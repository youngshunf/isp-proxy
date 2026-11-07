//go:build ispe2e

package isp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agent/agenttest"
	"github.com/go-gost/gost/internal/control/store/pgtest"
)

// 平台 E2E 的环境：每个场景一套独立的 数据库 + isp-control + gost(agent) + 假上游，可以并行运行，也可以放心地
// kill -9、拨时钟、从备份恢复，不影响其他场景。

// ExitSpec 描述一个假 ISP 出口：上游代理忽略客户请求的目标地址，一律连到自己的 Backend，
// 所以 Backend 的 /ip 返回的 Label 就是“目标侧看到的出口”，/echo 返回的 IP 是出口检测看到的出口 IP。
type ExitSpec struct {
	Label    string
	IP       string // 回显 IP（203.0.113.x 等文档地址）
	Geo      string // 控制面按 IP 解析出的国家；默认 us。空串表示未知
	Country  string // 录入时声明的国家代码；默认 us
	City     string // 默认 Los Angeles
	Protocol string // http / https / socks5；默认按序号轮换
	// RegisterPassword 非空时录入错误的上游密码（认证失败的候选）。
	RegisterPassword string
	// Down 为 true 时录入后立即关闭上游（不可达的候选）。
	Down bool
	// NoVerify 为 true 时录入但不验证（保持 pending_verify）。
	NoVerify bool
}

// FakeExit 是一个运行中的假出口。
type FakeExit struct {
	ExitSpec
	Backend    *agenttest.Backend
	Up         *agenttest.Upstream
	ResourceID string
}

// EnvOptions 调整环境。
type EnvOptions struct {
	// Exits 是默认生成的可售美国出口数（默认 4）。
	Exits int
	// Extra 是额外的出口（候选、故障出口等）；默认不验证的需显式设 NoVerify。
	Extra []ExitSpec
	// PortRange 是节点端口池大小（默认 240，最大 250）。
	PortRange int
	// Rules 修改默认规则（发布前调用）。
	Rules func(*RulesInput)
	// Plan 修改默认套餐版本。
	Plan func(*PlanVersionInput)
	// FlushInterval 是 agent 用量落盘间隔（默认 200ms）。
	FlushInterval time.Duration
	// ControlOnly 只起数据库、isp-control、管理员与规则（账号类场景用），不起节点与库存。
	ControlOnly bool
}

// Env 是一套运行中的平台。
type Env struct {
	t    *testing.T
	Name string
	Dir  string

	DBURL     string
	SecretKey string
	Base      string // http://127.0.0.1:port
	AgentAddr string

	CfgPath, MailDir, CertDir, ClockFile, DataDir, AgentCfg string
	Control, Gost                                           *proc
	controlEnv                                              []string

	Admin  *Admin
	NodeID string
	Ports  [2]int

	Canary *agenttest.Backend
	Exits  []*FakeExit // 按录入顺序；前 opts.Exits 个为默认可售出口
	byIP   map[string]*FakeExit
	geo    map[string]string

	Plan  AdminPlan
	Pack  AdminPack
	Rules RulesInput

	offset   time.Duration
	mailSeen sync.Map
	userSeq  atomic.Int64
	upCA     string
}

var envSeq atomic.Int64

// portBase 为每个环境分配互不重叠的节点端口池：20000～48999 切成每段 250 个端口，按序轮转；
// 段内有端口已被占用（其他进程或系统临时端口）时跳到下一段。
func portBase(n int) int {
	const lo, stride, slots = 20000, 250, 116
	for range slots {
		base := lo + int(envSeq.Add(1)-1)%slots*stride
		if portsFree(base, n) {
			return base
		}
	}
	return lo + int(envSeq.Add(1)-1)%slots*stride
}

func portsFree(base, n int) bool {
	for p := base; p < base+n; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			return false
		}
		ln.Close()
	}
	return true
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func freePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// NewEnv 启动一套完整平台：数据库、isp-control、管理员（含 TOTP 绑定）、带收款说明的规则、节点与 gost、
// 已验证的上游库存、上架的套餐与流量包。
func NewEnv(t *testing.T, opts EnvOptions) *Env {
	t.Helper()
	start := time.Now()
	if opts.Exits == 0 {
		opts.Exits = 4
	}
	if opts.PortRange == 0 {
		opts.PortRange = 240
	}
	e := &Env{t: t, Name: strings.ReplaceAll(t.Name(), "/", "_"), Dir: t.TempDir(), byIP: map[string]*FakeExit{}, geo: map[string]string{}}
	e.DBURL = pgtest.NewDatabase(t)
	e.SecretKey = randHex(32)
	e.CfgPath = filepath.Join(e.Dir, "isp-control.yaml")
	e.MailDir = filepath.Join(e.Dir, "mail")
	e.CertDir = filepath.Join(e.Dir, "certs")
	e.ClockFile = filepath.Join(e.Dir, "clock-offset")
	e.DataDir = filepath.Join(e.Dir, "agent-data")
	e.AgentCfg = filepath.Join(e.Dir, "agent.yaml")
	pb := portBase(opts.PortRange)
	e.Ports = [2]int{pb, pb + opts.PortRange - 1}
	t.Cleanup(e.cleanup)

	// 目标侧金丝雀：客户请求的目标地址都指向它；只要发生直连（绕过上游），就会被它记到。
	e.Canary = agenttest.StartBackend(t, "DIRECT", "192.0.2.99")

	// 出口：默认 opts.Exits 个美国出口 + 额外出口。
	protos := []string{agenttest.UpstreamHTTP, agenttest.UpstreamSOCKS5, agenttest.UpstreamHTTPS}
	var specs []ExitSpec
	for i := 0; i < opts.Exits; i++ {
		specs = append(specs, ExitSpec{Label: fmt.Sprintf("exit-%c", 'A'+i), IP: fmt.Sprintf("203.0.113.%d", 10+i),
			Protocol: protos[i%len(protos)]})
	}
	for i, s := range opts.Extra {
		if s.Label == "" {
			s.Label = fmt.Sprintf("extra-%d", i)
		}
		if s.IP == "" {
			s.IP = fmt.Sprintf("198.51.100.%d", 10+i)
		}
		if s.Protocol == "" {
			s.Protocol = protos[i%len(protos)]
		}
		specs = append(specs, s)
	}
	var cas []string
	for _, s := range specs {
		if s.Country == "" {
			s.Country = "us"
		}
		if s.City == "" {
			s.City = "Los Angeles"
		}
		if s.Geo == "" {
			s.Geo = "us"
		} else if s.Geo == "-" {
			s.Geo = ""
		}
		x := &FakeExit{ExitSpec: s}
		x.Backend = agenttest.StartBackend(t, s.Label, s.IP)
		x.Up = agenttest.StartUpstream(t, s.Protocol, x.Backend.Addr, "isp-"+strings.ToLower(s.Label), "pw-"+randHex(4))
		if x.Up.CAFile != "" {
			b, err := os.ReadFile(x.Up.CAFile)
			if err != nil {
				t.Fatal(err)
			}
			cas = append(cas, string(b))
		}
		if s.Geo != "" {
			e.geo[s.IP] = s.Geo
		}
		e.Exits = append(e.Exits, x)
		if _, dup := e.byIP[s.IP]; !dup {
			e.byIP[s.IP] = x
		}
	}
	e.upCA = filepath.Join(e.Dir, "upstream-ca.pem")
	if err := os.WriteFile(e.upCA, []byte(strings.Join(cas, "")), 0o600); err != nil {
		t.Fatal(err)
	}

	// 开发证书（CLI）：先生成 CA 与服务端证书，节点登记后再追加节点证书。
	e.run(t, nil, "dev-certs", "-dir", e.CertDir, "-node", "nod_bootstrap", "-host", "127.0.0.1")

	publicPort, agentPort := freePort(t), freePort(t)
	e.Base = fmt.Sprintf("http://127.0.0.1:%d", publicPort)
	e.AgentAddr = fmt.Sprintf("127.0.0.1:%d", agentPort)
	e.writeControlConfig(publicPort, agentPort)
	e.controlEnv = []string{"ISP_DATABASE_URL=" + e.DBURL, "ISP_SECRET_KEY=" + e.SecretKey}
	e.Control = newProc("isp-control", filepath.Join(e.Dir, "isp-control.log"), bins.control, e.controlEnv, "serve", "-config", e.CfgPath)
	e.StartControl()

	// 管理员：CLI 创建，API 登录并绑定 TOTP。
	adminEmail, adminPass := "ops@e2e.test", "admin-pass-"+randHex(6)
	e.run(t, []string{"ISP_ADMIN_PASSWORD=" + adminPass}, "admin", "create", "-config", e.CfgPath, "-email", adminEmail, "-name", "E2E 运维")
	e.Admin = e.newAdmin(adminEmail, adminPass)
	e.Admin.Login()

	// 规则：上线收费前必须发布带收款说明的规则（内置规则收款说明为空，下单 412）。
	e.Rules = DefaultRules()
	if opts.Rules != nil {
		opts.Rules(&e.Rules)
	}
	e.Admin.PublishRules(&e.Rules)
	if opts.ControlOnly {
		t.Logf("环境 %s 就绪（仅控制面，%.1fs）：%s", e.Name, time.Since(start).Seconds(), e.Base)
		return e
	}

	// 节点：API 登记 → 追加节点证书 → 启动 gost。
	var node Node
	e.Admin.Must("POST", "/nodes", map[string]any{"name": "e2e-" + randHex(3), "publicHost": "127.0.0.1",
		"portRange": map[string]int{"start": e.Ports[0], "end": e.Ports[1]}}, &node)
	e.NodeID = node.ID
	e.run(t, nil, "dev-certs", "-dir", e.CertDir, "-node", e.NodeID, "-host", "127.0.0.1")
	if opts.FlushInterval == 0 {
		opts.FlushInterval = 200 * time.Millisecond
	}
	e.writeAgentConfig(opts.FlushInterval)
	e.Gost = newProc("gost", filepath.Join(e.Dir, "gost.log"), bins.gost, nil, "-agent-config", e.AgentCfg)
	e.Gost.start(t)
	e.WaitNodeOnline()

	// 库存：录入全部出口，验证需要可售的那些（并行下发验证探测）。
	var wg sync.WaitGroup
	for _, x := range e.Exits {
		x.ResourceID = e.Admin.CreateUpstream(e.UpstreamInputFor(x)).ID
		if x.Down {
			x.Up.Close()
		}
		if x.NoVerify || x.Down || x.RegisterPassword != "" {
			continue
		}
		wg.Add(1)
		go func(x *FakeExit) {
			defer wg.Done()
			e.Admin.VerifyUpstream(x.ResourceID, x.Geo == x.Country)
		}(x)
	}
	wg.Wait()

	// 套餐与流量包。
	pv := DefaultPlanVersion()
	if opts.Plan != nil {
		opts.Plan(&pv)
	}
	e.Plan = e.Admin.CreatePlan("us-lax-"+randHex(2), pv)
	e.Pack = e.Admin.CreatePack("tp-"+randHex(2), 100*MB, 1000)
	t.Logf("环境 %s 就绪（%.1fs）：%s 节点 %s 端口 %d-%d", e.Name, time.Since(start).Seconds(), e.Base, e.NodeID, e.Ports[0], e.Ports[1])
	return e
}

const (
	KB = 1000
	MB = 1000 * 1000
	GB = 1000 * MB
)

// DefaultRules 是首期规则（docs/06 §1）加占位收款说明。
func DefaultRules() RulesInput {
	return RulesInput{
		Version: "R-2026-11-08.1", PeriodDays: 30, RetentionDays: 7, PortCooldownDays: 7, ResourceCooldownDays: 7,
		AccessChangesPerWindow: 5, AccessChangeWindowHours: 24, FreeExitChangesPerWindow: 1, ExitChangeWindowDays: 30,
		UnpaidHoldHours: 24, MaxUnpaidNewOrders: 3, MonthOptions: []int{1, 3, 6},
		Payment: PaymentInstructions{Payee: "E2E 测试收款方（占位）", Bank: "E2E 测试银行（占位）", Account: "6222 0000 0000 0000",
			Note: "转账附言填写付款识别码"},
		SnapshotLines: []string{"时长按 30×24 小时计", "流量长期保留", "到期后保留 7 天"},
	}
}

// DefaultPlanVersion 是测试套餐：200 MB/月、并发 64、带宽 2000 Mbps（足够不触发限速）。
func DefaultPlanVersion() PlanVersionInput {
	return PlanVersionInput{Name: "洛杉矶住宅 E2E", CountryCode: "us", Country: "美国", City: "Los Angeles", Price: CNY(9900),
		PeriodDays: 30, IncludedBytes: 200 * MB, Concurrency: 64, BandwidthMbps: 2000}
}

func (e *Env) writeControlConfig(publicPort, agentPort int) {
	var geo strings.Builder
	for ip, cc := range e.geo {
		fmt.Fprintf(&geo, "    %q: %q\n", ip, cc)
	}
	cfg := fmt.Sprintf(`listen: 127.0.0.1:%d
secure_cookies: false
totp_issuer: E2E
agent:
  listen: 127.0.0.1:%d
  ca_file: %s
  cert_file: %s
  key_file: %s
  long_poll: 5s
mail:
  mode: file
  file_dir: %s
  from: E2E <no-reply@e2e.test>
  brand: E2E 代理
  site_url: http://portal.e2e.test
log:
  level: debug
core:
  echo_url: http://%s/echo
  scheduler_interval: 300ms
  task_timeout: 20s
  probe_timeout: 15s
  node_offline_after: 6s
%s
dev:
  clock_offset_file: %s
`, publicPort, agentPort, filepath.Join(e.CertDir, "ca.pem"), filepath.Join(e.CertDir, "server.pem"),
		filepath.Join(e.CertDir, "server-key.pem"), e.MailDir, e.Canary.Addr, exitCountries(geo.String()), e.ClockFile)

	if err := os.WriteFile(e.CfgPath, []byte(cfg), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func exitCountries(lines string) string {
	if lines == "" {
		return ""
	}
	return "  exit_countries:\n" + lines
}

func (e *Env) writeAgentConfig(flush time.Duration) {
	entryCert, entryKey := agenttest.CertFiles(e.t, e.Dir, "entry.e2e.test")
	cfg := fmt.Sprintf(`node_id: %s
control:
  url: https://%s
  ca_file: %s
  cert_file: %s
  key_file: %s
data_dir: %s
entry:
  bind_host: 127.0.0.1
  cert_file: %s
  key_file: %s
egress:
  allow_cidrs: [127.0.0.0/8]
upstream_tls:
  ca_file: %s
probe:
  echo_url: http://%s/echo
  timeout: 5s
report_interval: 500ms
flush_interval: %s
`, e.NodeID, e.AgentAddr, filepath.Join(e.CertDir, "ca.pem"), filepath.Join(e.CertDir, "node-"+e.NodeID+".pem"),
		filepath.Join(e.CertDir, "node-"+e.NodeID+"-key.pem"), e.DataDir, entryCert, entryKey, e.upCA, e.Canary.Addr, flush)
	if err := os.WriteFile(e.AgentCfg, []byte(cfg), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// run 执行 isp-control 子命令（CLI）。
func (e *Env) run(t testing.TB, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bins.control, args...)
	cmd.Env = append(append(os.Environ(), "ISP_DATABASE_URL="+e.DBURL, "ISP_SECRET_KEY="+e.SecretKey), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isp-control %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// StartControl 启动（或重启）isp-control 并等待 /healthz。
func (e *Env) StartControl() {
	e.t.Helper()
	e.Control.start(e.t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := http.Get(e.Base + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return
			}
		}
		if !e.Control.running() {
			e.t.Fatalf("isp-control 启动失败：\n%s", tailFile(e.Control.logPath, 60))
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("isp-control /healthz 超时：%v\n%s", err, tailFile(e.Control.logPath, 60))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// StopControl 停止 isp-control（模拟控制面离线）。
func (e *Env) StopControl() { e.Control.stop() }

// RestartGost 重新启动 gost（崩溃后、正常停止后，或仍在运行时先正常停止）。
func (e *Env) RestartGost() {
	e.t.Helper()
	e.Gost.start(e.t)
}

// WaitNodeOnline 等待节点上报（online）。
func (e *Env) WaitNodeOnline() {
	e.t.Helper()
	e.Eventually(30*time.Second, func() bool {
		var n Node
		_, err := e.Admin.Do("GET", "/nodes/"+e.NodeID, nil, &n)
		return err == nil && n.Online && n.LastReportAt != nil
	}, "节点 %s 未上线", e.NodeID)
}

// ShiftClock 把控制面业务时钟再向前拨 d（dev.clock_offset_file），等待生效并重新登录管理员（管理员会话按业务时钟过期）。
func (e *Env) ShiftClock(d time.Duration) {
	e.t.Helper()
	e.offset += d
	if err := os.WriteFile(e.ClockFile, []byte(e.offset.String()), 0o600); err != nil {
		e.t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // 文件每 200ms 检查一次
	e.Admin.Login()
}

// Now 返回控制面业务时间（系统时间 + 偏移）。
func (e *Env) Now() time.Time { return time.Now().Add(e.offset) }

// Eventually 轮询直到 cond 为真。
func (e *Env) Eventually(max time.Duration, cond func() bool, format string, args ...any) {
	e.t.Helper()
	deadline := time.Now().Add(max)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf(format, args...)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Exit 按回显 IP 或标签找出口。
func (e *Env) Exit(key string) *FakeExit {
	if x, ok := e.byIP[key]; ok {
		return x
	}
	for _, x := range e.Exits {
		if x.Label == key || x.ResourceID == key {
			return x
		}
	}
	e.t.Fatalf("没有出口 %q", key)
	return nil
}

// ExitOfResource 返回资源对应的出口标签。
func (e *Env) LabelOf(resourceID string) string {
	for _, x := range e.Exits {
		if x.ResourceID == resourceID {
			return x.Label
		}
	}
	return ""
}

// UpstreamInputFor 返回录入出口的请求体。
func (e *Env) UpstreamInputFor(x *FakeExit) UpstreamInput {
	host, port, _ := net.SplitHostPort(x.Up.Addr)
	var p int
	fmt.Sscan(port, &p)
	pass := x.Up.Pass
	if x.RegisterPassword != "" {
		pass = x.RegisterPassword
	}
	return UpstreamInput{Protocol: x.Protocol, Host: host, Port: p, Username: x.Up.User, Password: pass, NodeID: e.NodeID,
		CountryCode: x.Country, Country: "美国", City: x.City, Provider: "e2e-" + x.Label}
}

// CanaryHits 返回金丝雀（直连）命中数。
func (e *Env) CanaryHits() int64 { return e.Canary.Hits.Load() }

func (e *Env) cleanup() {
	if e.Gost != nil {
		e.Gost.stop()
	}
	if e.Control != nil {
		e.Control.stop()
	}
	if e.t.Failed() {
		if e.Control != nil {
			e.t.Logf("---- isp-control 日志尾部（%s）----\n%s", e.Control.logPath, tailFile(e.Control.logPath, 150))
		}
		if e.Gost != nil {
			e.t.Logf("---- gost 日志尾部（%s）----\n%s", e.Gost.logPath, tailFile(e.Gost.logPath, 150))
		}
		if dir := os.Getenv("ISP_E2E_KEEP"); dir != "" {
			dst := filepath.Join(dir, e.Name)
			_ = os.MkdirAll(dst, 0o700)
			for _, f := range []string{"isp-control.log", "gost.log", "isp-control.yaml", "agent.yaml"} {
				if b, err := os.ReadFile(filepath.Join(e.Dir, f)); err == nil {
					_ = os.WriteFile(filepath.Join(dst, f), b, 0o600)
				}
			}
			e.t.Logf("日志已保存到 %s", dst)
		}
	}
}
