package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agent/agenttest"
)

// 种子数据（deploy/seed.example.sql）里的节点：开发栈另建一个 publicHost 为 127.0.0.1 的节点，并停用它。
const seedNodeID = "nod_dev00000000000000001"

// exitSpec 是一个假出口。国家与城市要与 seed-dev 的套餐一致（库存按 国家 + 城市 计档）。
// maxTenants 是录入时的绑定上限（D-24）：1 只给独享订阅；≥2 可由多个共享订阅共用，空闲时也可整个分给独享订阅。
type exitSpec struct {
	label, ip, proto, cc, country, city string
	maxTenants                          int
}

var (
	exitSpecs = []exitSpec{
		{"exit-A", "203.0.113.10", agenttest.UpstreamHTTP, "us", "美国", "洛杉矶", 1},
		{"exit-B", "203.0.113.11", agenttest.UpstreamSOCKS5, "us", "美国", "洛杉矶", 1},
		{"exit-C", "203.0.113.12", agenttest.UpstreamHTTPS, "us", "美国", "洛杉矶", 1},
		{"exit-D", "203.0.113.13", agenttest.UpstreamHTTP, "us", "美国", "洛杉矶", 1},
		{"exit-E", "203.0.113.14", agenttest.UpstreamSOCKS5, "us", "美国", "洛杉矶", 3},
		{"exit-F", "203.0.113.15", agenttest.UpstreamHTTP, "us", "美国", "洛杉矶", 3},
		{"exit-G", "203.0.113.16", agenttest.UpstreamHTTP, "us", "美国", "洛杉矶", 3},
		{"exit-H", "203.0.113.17", agenttest.UpstreamSOCKS5, "us", "美国", "洛杉矶", 3},
		{"exit-J1", "203.0.113.20", agenttest.UpstreamSOCKS5, "jp", "日本", "东京", 1},
		{"exit-J2", "203.0.113.21", agenttest.UpstreamHTTP, "jp", "日本", "东京", 1},
		{"exit-J3", "203.0.113.22", agenttest.UpstreamHTTPS, "jp", "日本", "东京", 1},
	}
	// 备用出口：启动但不录入，供管理后台换出口向导“当场录入候选”使用。
	spareSpecs = []exitSpec{
		{"spare-S1", "203.0.113.30", agenttest.UpstreamSOCKS5, "us", "美国", "洛杉矶", 1},
		{"spare-S2", "203.0.113.31", agenttest.UpstreamHTTP, "us", "美国", "洛杉矶", 3},
	}
)

type fakeExit struct {
	spec    exitSpec
	backend *agenttest.Backend
	up      *agenttest.Upstream
	resID   string
}

func (x *fakeExit) url() string {
	return fmt.Sprintf("%s://%s:%s@%s", x.spec.proto, url.QueryEscape(x.up.User), url.QueryEscape(x.up.Pass), x.up.Addr)
}

type stack struct {
	dir string
	o   options
	tb  *tbShim

	bin                                 struct{ control, gost string }
	pgCtl, pgData                       string
	dbURL, secretKey                    string
	base, agentAddr                     string
	certDir, mailDir, cfgPath, agentCfg string
	control, gost                       *proc
	canary                              *agenttest.Backend
	exits, spares                       []*fakeExit
	upCA                                string
	nodeID                              string
	totpSecret                          string
	totpStep                            int64
	adm                                 *apiClient
	pgStarted                           bool
}

func newStack(dir string, o options) *stack {
	return &stack{dir: dir, o: o, tb: &tbShim{dir: dir}}
}

func (s *stack) logf(format string, args ...any) {
	if !s.o.quiet {
		fmt.Fprintf(os.Stderr, "· "+format+"\n", args...)
	}
}

func (s *stack) env() []string {
	return []string{"ISP_DATABASE_URL=" + s.dbURL, "ISP_SECRET_KEY=" + s.secretKey}
}

func (s *stack) bootstrap(ctx context.Context, root string) error {
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"构建 isp-control 与 gost", func(context.Context) error { return s.build(root) }},
		{"启动一次性 PostgreSQL", s.startPostgres},
		{"启动假 ISP 上游与 IP 回显端点", s.startExits},
		{"生成开发证书并启动 isp-control", s.startControl},
		{"写入 seed-dev 种子数据", s.seed},
		{"建立管理员并绑定 TOTP", s.createAdmin},
		{"检查当前规则的收款说明", s.publishRules},
		{"登记节点并启动 gost（agent）", s.startNode},
		{"录入并验证上游库存", s.registerExits},
	}
	for _, st := range steps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t0 := time.Now()
		s.logf("%s……", st.name)
		if err := guard(ctx, st.fn); err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
		s.logf("%s 完成（%.1fs）", st.name, time.Since(t0).Seconds())
	}
	return s.writeState()
}

// guard 把 agenttest 的 Fatal（tbFatal 恐慌）转成错误。
func guard(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(tbFatal)
			if !ok {
				panic(r)
			}
			err = errors.New(f.msg)
		}
	}()
	return fn(ctx)
}

func (s *stack) build(root string) error {
	binDir := filepath.Join(s.dir, "bin")
	s.bin.control = filepath.Join(binDir, "isp-control")
	s.bin.gost = filepath.Join(binDir, "gost")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, b := range []struct{ out, pkg string }{{s.bin.control, "./cmd/isp-control"}, {s.bin.gost, "./cmd/gost"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				errs[i] = fmt.Errorf("go build %s: %v\n%s", b.pkg, err, out)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (s *stack) startPostgres(context.Context) error {
	initdb, err1 := exec.LookPath("initdb")
	pgCtl, err2 := exec.LookPath("pg_ctl")
	if err1 != nil || err2 != nil {
		return errors.New("PATH 中找不到 initdb/pg_ctl（需要 PostgreSQL 16，例如 brew install postgresql@16）")
	}
	s.pgCtl = pgCtl
	s.pgData = filepath.Join(s.dir, "pg", "data")
	if out, err := exec.Command(initdb, "-D", s.pgData, "-U", "postgres", "-A", "trust", "-E", "UTF8", "--locale=C", "-N").CombinedOutput(); err != nil {
		return fmt.Errorf("initdb: %v\n%s", err, out)
	}
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		port, err := freePort()
		if err != nil {
			last = err
			continue
		}
		opts := strings.Join([]string{"-p " + itoa(port), "-c listen_addresses=127.0.0.1", "-c unix_socket_directories=''",
			"-c fsync=off", "-c synchronous_commit=off", "-c full_page_writes=off"}, " ")
		out, err := exec.Command(pgCtl, "-D", s.pgData, "-l", filepath.Join(s.dir, "pg", "postgres.log"), "-w", "-t", "60", "-o", opts, "start").CombinedOutput()
		if err != nil {
			last = fmt.Errorf("pg_ctl start: %v\n%s", err, out)
			continue
		}
		s.pgStarted = true
		s.dbURL = fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
		return nil
	}
	return last
}

func (s *stack) startExits(context.Context) error {
	s.canary = agenttest.StartBackend(s.tb, "DIRECT", "192.0.2.99")
	var cas []string
	mk := func(sp exitSpec) *fakeExit {
		x := &fakeExit{spec: sp}
		x.backend = agenttest.StartBackend(s.tb, sp.label, sp.ip)
		x.up = agenttest.StartUpstream(s.tb, sp.proto, x.backend.Addr, "isp-"+strings.ToLower(sp.label), "pw-"+randHex(4))
		if x.up.CAFile != "" {
			b, err := os.ReadFile(x.up.CAFile)
			if err == nil {
				cas = append(cas, string(b))
			}
		}
		return x
	}
	for _, sp := range exitSpecs {
		s.exits = append(s.exits, mk(sp))
	}
	for _, sp := range spareSpecs {
		s.spares = append(s.spares, mk(sp))
	}
	if s.tb.failed != nil {
		return s.tb.failed
	}
	s.upCA = filepath.Join(s.dir, "upstream-ca.pem")
	return os.WriteFile(s.upCA, []byte(strings.Join(cas, "")), 0o600)
}

func (s *stack) startControl(ctx context.Context) error {
	s.secretKey = randHex(32)
	s.certDir = filepath.Join(s.dir, "certs")
	s.mailDir = filepath.Join(s.dir, "mail")
	s.cfgPath = filepath.Join(s.dir, "isp-control.yaml")
	if err := runCmd(nil, nil, s.bin.control, "dev-certs", "-dir", s.certDir, "-node", "nod_bootstrap", "-host", "127.0.0.1"); err != nil {
		return err
	}
	agentPort, err := freePort()
	if err != nil {
		return err
	}
	s.agentAddr = fmt.Sprintf("127.0.0.1:%d", agentPort)
	host, _, err := net.SplitHostPort(s.o.listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	_, port, _ := net.SplitHostPort(s.o.listen)
	s.base = "http://" + net.JoinHostPort(host, port)

	var geo strings.Builder
	for _, x := range append(append([]*fakeExit{}, s.exits...), s.spares...) {
		fmt.Fprintf(&geo, "    %q: %q\n", x.spec.ip, x.spec.cc)
	}
	cfg := fmt.Sprintf(`# isp-devstack 生成，只用于本机开发
listen: %s
secure_cookies: false
totp_issuer: ISP 代理（开发）
agent:
  listen: %s
  ca_file: %s
  cert_file: %s
  key_file: %s
  long_poll: 10s
mail:
  mode: file
  file_dir: %s
  from: ISP 代理（开发） <no-reply@dev.test>
  brand: ISP 代理（开发）
  site_url: %s
log:
  level: info
core:
  echo_url: http://%s/echo
  exit_countries:
%s`, s.o.listen, s.agentAddr, filepath.Join(s.certDir, "ca.pem"), filepath.Join(s.certDir, "server.pem"),
		filepath.Join(s.certDir, "server-key.pem"), s.mailDir, s.o.portalURL, s.canary.Addr, geo.String())
	if err := os.WriteFile(s.cfgPath, []byte(cfg), 0o600); err != nil {
		return err
	}
	s.control, err = startProc("isp-control", filepath.Join(s.dir, "isp-control.log"), s.env(), s.bin.control, "serve", "-config", s.cfgPath)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := http.Get(s.base + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return nil
			}
		}
		if !s.control.running() {
			return fmt.Errorf("isp-control 退出：\n%s", tail(s.control.log, 40))
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("等待 %s/healthz 超时：%v\n%s", s.base, err, tail(s.control.log, 40))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *stack) seed(context.Context) error {
	return runCmd(s.env(), nil, s.bin.control, "seed-dev", "-config", s.cfgPath, "-user-email", s.o.userEmail, "-user-password", s.o.userPass)
}

func (s *stack) createAdmin(context.Context) error {
	if err := runCmd(append(s.env(), "ISP_ADMIN_PASSWORD="+s.o.adminPass), nil, s.bin.control,
		"admin", "create", "-config", s.cfgPath, "-email", s.o.adminEmail, "-name", "开发管理员"); err != nil {
		return err
	}
	s.adm = newAPIClient(s.base, "/admin-api")
	sec, step, err := adminLogin(s.adm, s.o.adminEmail, s.o.adminPass, "", 0)
	if err != nil {
		return err
	}
	s.totpSecret, s.totpStep = sec, step
	return nil
}

// publishRules 确保当前生效的规则带收款说明。内置规则自始生效但收款说明为空（下单 412）；seed-dev 的
// 带示例收款说明的版本 effective_from 是固定时刻，本机时钟早于它时还没生效。这时以它为模板发布一个立即生效的版本。
func (s *stack) publishRules(context.Context) error {
	if s.paymentReady() {
		return nil
	}
	s.logf("当前规则还没有收款说明（种子规则尚未到生效时刻），发布一个立即生效的副本")
	var items []map[string]any
	if err := s.adm.do("GET", "/rules", nil, &items, ""); err != nil {
		return err
	}
	var tmpl map[string]any
	for _, r := range items {
		if p, _ := r["payment"].(map[string]any); p != nil && p["payee"] != "" {
			tmpl = r
			break
		}
	}
	if tmpl == nil {
		return errors.New("没有带收款说明的规则版本（seed-dev 未生效？）")
	}
	now := time.Now().UTC()
	in := map[string]any{}
	for _, k := range []string{"periodDays", "retentionDays", "portCooldownDays", "resourceCooldownDays", "accessChangesPerWindow",
		"accessChangeWindowHours", "freeExitChangesPerWindow", "exitChangeWindowDays", "unpaidHoldHours", "maxUnpaidNewOrders",
		"monthOptions", "payment", "snapshotLines"} {
		in[k] = tmpl[k]
	}
	in["version"] = fmt.Sprintf("R-%s.%d", now.Format("2006-01-02"), now.Unix()%1_000_000)
	in["effectiveFrom"] = now.Format(time.RFC3339)
	return s.adm.do("POST", "/rules", in, nil, "")
}

// paymentReady 报告前台目录当前是否带收款说明（没有时目录返回 412 或收款方为空）。
func (s *stack) paymentReady() bool {
	resp, err := http.Get(s.base + "/api/catalog")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var c struct {
		Payment struct {
			Payee string `json:"payee"`
		} `json:"payment"`
	}
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&c) == nil && c.Payment.Payee != ""
}

type nodeView struct {
	ID           string     `json:"id"`
	Online       bool       `json:"online"`
	LastReportAt *time.Time `json:"lastReportAt"`
}

func (s *stack) startNode(ctx context.Context) error {
	var n nodeView
	if err := s.adm.do("POST", "/nodes", map[string]any{"name": "devstack", "publicHost": "127.0.0.1",
		"portRange": map[string]int{"start": s.o.portStart, "end": s.o.portStart + s.o.portCount - 1}}, &n, ""); err != nil {
		return err
	}
	s.nodeID = n.ID
	// 种子节点的 publicHost 是占位域名，开发栈不用它：停用节点并停用其上游，避免离线告警与误分配。
	if err := s.adm.do("PATCH", "/nodes/"+seedNodeID, map[string]any{"status": "disabled"}, nil, ""); err != nil {
		s.logf("停用种子节点失败（忽略）：%v", err)
	}
	var ups struct {
		Items []struct {
			ID     string `json:"id"`
			NodeID string `json:"nodeId"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := s.adm.do("GET", "/upstreams?limit=200", nil, &ups, ""); err == nil {
		for _, u := range ups.Items {
			if u.NodeID == seedNodeID && u.Status != "retired" {
				if err := s.adm.do("POST", "/upstreams/"+u.ID+"/status", map[string]string{"status": "retired",
					"reason": "开发栈不使用种子占位上游"}, nil, ""); err != nil {
					s.logf("停用种子上游 %s 失败（忽略）：%v", u.ID, err)
				}
			}
		}
	}
	if err := runCmd(nil, nil, s.bin.control, "dev-certs", "-dir", s.certDir, "-node", s.nodeID, "-host", "127.0.0.1"); err != nil {
		return err
	}
	entryCert, entryKey := agenttest.CertFiles(s.tb, s.dir, "entry.dev.test")
	if s.tb.failed != nil {
		return s.tb.failed
	}
	s.agentCfg = filepath.Join(s.dir, "agent.yaml")
	cfg := fmt.Sprintf(`# isp-devstack 生成，只用于本机开发
node_id: %s
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
report_interval: 1s
flush_interval: 500ms
`, s.nodeID, s.agentAddr, filepath.Join(s.certDir, "ca.pem"), filepath.Join(s.certDir, "node-"+s.nodeID+".pem"),
		filepath.Join(s.certDir, "node-"+s.nodeID+"-key.pem"), filepath.Join(s.dir, "agent-data"), entryCert, entryKey, s.upCA, s.canary.Addr)
	if err := os.WriteFile(s.agentCfg, []byte(cfg), 0o600); err != nil {
		return err
	}
	var err error
	s.gost, err = startProc("gost", filepath.Join(s.dir, "gost.log"), nil, s.bin.gost, "-agent-config", s.agentCfg)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var nv nodeView
		if err := s.adm.do("GET", "/nodes/"+s.nodeID, nil, &nv, ""); err == nil && nv.Online && nv.LastReportAt != nil {
			return nil
		}
		if !s.gost.running() {
			return fmt.Errorf("gost 退出：\n%s", tail(s.gost.log, 40))
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("节点 %s 未上线：\n%s", s.nodeID, tail(s.gost.log, 40))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *stack) registerExits(ctx context.Context) error {
	type upstream struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	type probe struct {
		ID     string  `json:"id"`
		Status string  `json:"status"`
		Error  *string `json:"error"`
	}
	var wg sync.WaitGroup
	errs := make([]error, len(s.exits))
	for i, x := range s.exits {
		var u upstream
		if err := s.adm.do("POST", "/upstreams", map[string]any{"url": x.url(), "nodeId": s.nodeID, "countryCode": x.spec.cc,
			"country": x.spec.country, "city": x.spec.city, "provider": "devstack", "note": "开发栈假上游 " + x.spec.label,
			"maxTenants": x.spec.maxTenants}, &u, ""); err != nil {
			return err
		}
		x.resID = u.ID
		wg.Add(1)
		go func() {
			defer wg.Done()
			var p probe
			if err := s.adm.do("POST", "/upstreams/"+u.ID+"/verify", nil, &p, ""); err != nil {
				errs[i] = err
				return
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				var cur upstream
				if err := s.adm.do("GET", "/upstreams/"+u.ID, nil, &cur, ""); err == nil && cur.Status == "available" {
					return
				}
				_ = s.adm.do("GET", "/probes/"+p.ID, nil, &p, "")
				if p.Status == "expired" || (p.Status == "done" && p.Error != nil) {
					errs[i] = fmt.Errorf("上游 %s 验证失败：%+v", x.spec.label, p)
					return
				}
				if time.Now().After(deadline) || ctx.Err() != nil {
					errs[i] = fmt.Errorf("上游 %s 验证超时（探测 %s %s）", x.spec.label, p.ID, p.Status)
					return
				}
				time.Sleep(300 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (s *stack) state() State {
	info := func(xs []*fakeExit) []ExitInfo {
		var out []ExitInfo
		for _, x := range xs {
			out = append(out, ExitInfo{Label: x.spec.label, IP: x.spec.ip, Country: x.spec.cc, City: x.spec.city, URL: x.url(), ResourceID: x.resID,
				MaxTenants: x.spec.maxTenants})
		}
		return out
	}
	return State{ControlURL: s.base, PortalURL: s.o.portalURL, AdminURL: s.o.adminURL, MailDir: s.mailDir, Dir: s.dir,
		Admin: Credential{s.o.adminEmail, s.o.adminPass}, User: Credential{s.o.userEmail, s.o.userPass},
		TOTPSecret: s.totpSecret, TOTPLastUse: s.totpStep, NodeID: s.nodeID, NodeHost: "127.0.0.1",
		Exits: info(s.exits), Spares: info(s.spares), ProbeURL: "http://" + s.canary.Addr + "/ip", PID: os.Getpid()}
}

func (s *stack) writeState() error {
	b, err := json.MarshalIndent(s.state(), "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, ".state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.json"))
}

func (s *stack) printSummary() {
	st := s.state()
	var b strings.Builder
	fmt.Fprintf(&b, "\n开发栈已就绪（Ctrl-C 停止并清理）\n\n")
	fmt.Fprintf(&b, "  控制面 API      %s   （/api、/admin-api；两个前端的 Vite 代理指向它）\n", st.ControlURL)
	fmt.Fprintf(&b, "  用户前台        %s   cd web && corepack pnpm@10.28.1 dev:portal\n", st.PortalURL)
	fmt.Fprintf(&b, "  管理后台        %s   cd web && corepack pnpm@10.28.1 dev:admin\n\n", st.AdminURL)
	fmt.Fprintf(&b, "  管理员          %s / %s\n", st.Admin.Email, st.Admin.Password)
	fmt.Fprintf(&b, "  TOTP 密钥       %s   （导入验证器 App，或 oathtool --totp -b %s）\n", st.TOTPSecret, st.TOTPSecret)
	fmt.Fprintf(&b, "  开发用户        %s / %s（已验证，无订阅）\n", st.User.Email, st.User.Password)
	fmt.Fprintf(&b, "  邮件投递目录    %s   （注册验证码、找回密码验证码在这里的 .eml 里）\n\n", st.MailDir)
	fmt.Fprintf(&b, "  节点            %s（接入地址 127.0.0.1，端口 %d-%d）\n", st.NodeID, s.o.portStart, s.o.portStart+s.o.portCount-1)
	fmt.Fprintf(&b, "  已录入出口      ")
	for i, x := range st.Exits {
		if i > 0 {
			b.WriteString("、")
		}
		mode := "独享"
		if x.MaxTenants > 1 {
			mode = fmt.Sprintf("可共享 %d", x.MaxTenants)
		}
		fmt.Fprintf(&b, "%s %s（%s，%s）", x.Label, x.IP, x.City, mode)
	}
	fmt.Fprintf(&b, "\n  套餐            美国 洛杉矶（独享）、美国 洛杉矶 共享（共享，分到“可共享”的出口）、日本 东京（独享）")
	fmt.Fprintf(&b, "\n  备用候选上游    （换出口向导里当场录入用）\n")
	for _, x := range st.Spares {
		fmt.Fprintf(&b, "                  %s  %s  %s  （建议绑定上限 %d）\n", x.Label, x.IP, x.URL, x.MaxTenants)
	}
	fmt.Fprintf(&b, "\n  经接入访问 127.0.0.1 上任意 http 地址的 /ip 返回出口标签（节点只放行 127.0.0.0/8 目标），例如\n  curl -x http://<用户名>:<密码>@127.0.0.1:<端口> %s\n", st.ProbeURL)
	fmt.Fprintf(&b, "\n  日志 %s/{isp-control,gost}.log；就绪信息 %s/state.json\n", s.dir, s.dir)
	fmt.Fprint(os.Stdout, b.String())
}

func (s *stack) teardown() {
	s.gost.stop()
	s.control.stop()
	s.tb.cleanup()
	if s.pgStarted {
		_ = exec.Command(s.pgCtl, "-D", s.pgData, "-m", "fast", "-w", "stop").Run()
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tbShim 让 agenttest（以 testing.TB 为参数）在普通程序里使用：只实现它用到的方法，其余方法不会被调用。
type tbShim struct {
	testing.TB
	dir      string
	mu       sync.Mutex
	cleanups []func()
	failed   error
	seq      int
}

func (t *tbShim) Helper() {}
func (t *tbShim) Cleanup(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanups = append(t.cleanups, f)
}
func (t *tbShim) TempDir() string {
	t.mu.Lock()
	t.seq++
	d := filepath.Join(t.dir, "tmp", itoa(t.seq))
	t.mu.Unlock()
	_ = os.MkdirAll(d, 0o700)
	return d
}

// Fatal 与 testing.T 一样不返回：以 tbFatal 恐慌，由 bootstrap 的步骤包装恢复成错误。
func (t *tbShim) Fatal(args ...any) { t.fail(fmt.Sprint(args...)); panic(tbFatal{fmt.Sprint(args...)}) }
func (t *tbShim) Fatalf(format string, args ...any) {
	t.fail(fmt.Sprintf(format, args...))
	panic(tbFatal{fmt.Sprintf(format, args...)})
}

type tbFatal struct{ msg string }

func (t *tbShim) Errorf(format string, args ...any) { t.fail(fmt.Sprintf(format, args...)) }
func (t *tbShim) Logf(format string, args ...any)   {}
func (t *tbShim) fail(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed == nil {
		t.failed = errors.New(msg)
	}
}
func (t *tbShim) cleanup() {
	t.mu.Lock()
	fs := t.cleanups
	t.cleanups = nil
	t.mu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}
