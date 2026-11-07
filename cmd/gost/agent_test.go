package main

// -agent-config 的接入测试（A-W8）：
//   - 不带 -agent-config 时与上游 GOST 一致：参数集合只多一个 -agent-config，isp-* 组件不进注册表，
//     真实二进制按 -L 正常代理；
//   - 带 -agent-config 时，真实二进制连接伪控制面（mTLS），按期望状态开通接入并经绑定出口代理请求；
//     拒绝开放 GOST 原生 API 与定时重载，SIGHUP 不清空认证器与出口链。
// 只用本机回环与测试内的伪上游、伪控制面；不运行 tests/e2e/。

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-gost/x/registry"

	"github.com/go-gost/gost/internal/agent/agenttest"
	"github.com/go-gost/gost/internal/agentproto"
)

func TestFlagsUnchangedExceptAgentConfig(t *testing.T) {
	fs := flag.NewFlagSet("gost", flag.ContinueOnError)
	var v bool
	defineFlags(fs, &v)
	var got []string
	fs.VisitAll(func(f *flag.Flag) { got = append(got, f.Name) })
	slices.Sort(got)
	// 上游 GOST v3.3.3 的参数，加本仓唯一新增的 agent-config。
	want := []string{"C", "D", "DD", "F", "L", "O", "R", "V", "agent-config", "api", "metrics"}
	if !slices.Equal(got, want) {
		t.Fatalf("flags %v, want %v", got, want)
	}
	if f := fs.Lookup("agent-config"); f.DefValue != "" {
		t.Fatalf("agent-config default %q", f.DefValue)
	}
}

// isp-tcp、isp-tls、isp-auth 只在 agent 运行时启动时注册；默认模式下 GOST 的注册表与上游一致。
func TestISPComponentsNotRegisteredByDefault(t *testing.T) {
	for _, name := range []string{"isp-tcp", "isp-tls"} {
		if registry.ListenerRegistry().IsRegistered(name) {
			t.Fatalf("listener %s registered without -agent-config", name)
		}
	}
	if registry.AutherRegistry().IsRegistered("isp-auth") {
		t.Fatal("auther isp-auth registered without -agent-config")
	}
	for _, name := range []string{"tcp", "tls", "ws", "kcp"} {
		if !registry.ListenerRegistry().IsRegistered(name) {
			t.Fatalf("upstream listener %s missing", name)
		}
	}
	for _, name := range []string{"http", "socks5", "relay", "ss"} {
		if !registry.HandlerRegistry().IsRegistered(name) {
			t.Fatalf("upstream handler %s missing", name)
		}
	}
}

// ---- 真实二进制 ----

var (
	buildOnce sync.Once
	buildDir  string
	buildBin  string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// gostBinary 用 go build 构建一次 cmd/gost，返回可执行文件路径。
func gostBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the gost binary")
	}
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "gost-bin-")
		if buildErr != nil {
			return
		}
		buildBin = filepath.Join(buildDir, "gost")
		if runtime.GOOS == "windows" {
			buildBin += ".exe"
		}
		out, err := exec.Command("go", "build", "-o", buildBin, ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildBin
}

type proc struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
	err  error
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startGost 在空的临时目录里启动二进制（避免读到仓库里的 gost.yml），测试结束时终止。
func startGost(t *testing.T, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: exec.Command(gostBinary(t), args...), out: &syncBuffer{}, done: make(chan struct{})}
	p.cmd.Dir = t.TempDir()
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			p.cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			t.Logf("gost output:\n%s", p.out.String())
		}
	})
	return p
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// 默认模式（不带 -agent-config）：-L 起的 HTTP 代理照常工作，-V 照常输出。
func TestBinaryDefaultModeUnchanged(t *testing.T) {
	out, err := exec.Command(gostBinary(t), "-V").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "gost ") {
		t.Fatalf("-V: %v %q", err, out)
	}
	back := agenttest.StartBackend(t, "target", "192.0.2.1")
	port := agenttest.FreePorts(t, 1)[0]
	p := startGost(t, "-L", fmt.Sprintf("http://u:p@127.0.0.1:%d", port))
	agenttest.WaitListening(t, 10*time.Second, agenttest.Loopback(port))
	body, _, err := agenttest.Fetch(agenttest.HTTP, agenttest.Loopback(port), "u", "p", back.Addr, "/ip")
	if err != nil || body != "target" {
		t.Fatalf("proxy: %q %v", body, err)
	}
	if _, err := agenttest.Tunnel(agenttest.HTTP, agenttest.Loopback(port), "u", "wrong", back.Addr); err == nil {
		t.Fatal("wrong password accepted")
	}
	if p.exited() {
		t.Fatal("gost exited")
	}
}

// agent 模式拒绝开放 GOST 原生 API 与定时重载（docs/06 §2）。
func TestBinaryAgentModeRefusesAPIAndReload(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "agent.yaml")
	os.WriteFile(cfg, []byte(`
control: {url: "https://127.0.0.1:1", ca_file: ca, cert_file: c, key_file: k}
data_dir: data
entry: {cert_file: e, key_file: ek}
`), 0o600)
	for _, tc := range []struct {
		args []string
		env  string
		want string
	}{
		{[]string{"-agent-config", cfg, "-api", "127.0.0.1:0"}, "", "GOST 原生 API"},
		{[]string{"-agent-config", cfg}, "GOST_API=127.0.0.1:0", "GOST 原生 API"},
		{[]string{"-agent-config", cfg, "-R", "1s"}, "", "不支持重载"},
		{[]string{"-agent-config", filepath.Join(t.TempDir(), "missing.yaml")}, "", "agent 配置"},
	} {
		cmd := exec.Command(gostBinary(t), tc.args...)
		cmd.Dir = t.TempDir()
		cmd.Env = os.Environ()
		if tc.env != "" {
			cmd.Env = append(cmd.Env, tc.env)
		}
		done := make(chan struct{})
		var out []byte
		var err error
		go func() { out, err = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("%v: did not exit", tc.args)
		}
		if err == nil || !strings.Contains(string(out), tc.want) {
			t.Fatalf("%v %s: err %v output %q", tc.args, tc.env, err, out)
		}
	}
}

// A-01/A-02、A-W8：真实 gost 二进制带 -agent-config 连接伪控制面，按期望状态开通三种接入，
// 经绑定出口代理请求；用量上报到控制面；SIGHUP 不清空 agent 的认证器与出口链；SIGTERM 时落盘退出。
func TestBinaryAgentModeEndToEnd(t *testing.T) {
	const node = "nod_bin"
	ctl := agenttest.StartControl(t, node, 2*time.Second)
	canary := agenttest.StartBackend(t, "DIRECT", "192.0.2.99")
	back := agenttest.StartBackend(t, "exit-A", "203.0.113.1")
	up := agenttest.StartUpstream(t, agenttest.UpstreamHTTP, back.Addr, "ua", "pa")

	dir := t.TempDir()
	entryCert, entryKey := agenttest.CertFiles(t, dir, "proxy.test")
	nc := ctl.NodeCert()
	dataDir := filepath.Join(dir, "data")
	cfg := filepath.Join(dir, "agent.yaml")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`
node_id: %s
control:
  url: %s
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
probe:
  echo_url: http://%s/echo
report_interval: 300ms
flush_interval: 100ms
`, node, ctl.URL(), ctl.Certs.CA.Cert, nc.Cert, nc.Key, dataDir, entryCert, entryKey, canary.Addr)), 0o600)

	ports := agenttest.FreePorts(t, 3)
	acc := &agentproto.Access{Version: 1, Username: "cust", Password: "secret",
		Ports: agentproto.Ports{HTTP: ports[0], HTTPS: ports[1], SOCKS5: ports[2]}}
	ctl.Update(func(d *agentproto.Desired) {
		d.Subscriptions = []agentproto.SubscriptionEntry{{
			ID: "sub_bin", Version: 1, EntitledTotal: 1 << 30, ExpiresAt: time.Now().Add(time.Hour).UTC(), Enabled: true,
			Access: acc,
			Exit:   &agentproto.Exit{Version: 1, Protocol: agentproto.ExitHTTP, Addr: up.Addr, Username: "ua", Password: "pa"},
		}}
	})

	p := startGost(t, "-agent-config", cfg)
	ctl.WaitReport(t, 15*time.Second, func(r *agentproto.Report) bool {
		s, ok := agenttest.SubIn(r, "sub_bin")
		return ok && s.AppliedVersion == 1
	})
	fetchAll := func(stage string) int64 {
		var total int64
		for i, port := range ports {
			pr := agenttest.Protos[i]
			body, n, err := agenttest.Fetch(pr, agenttest.Loopback(port), acc.Username, acc.Password, canary.Addr, "/ip")
			if err != nil || body != "exit-A" {
				t.Fatalf("%s %s: %q %v", stage, pr, body, err)
			}
			total += n
		}
		return total
	}
	n := fetchAll("initial")
	if _, err := agenttest.Tunnel(agenttest.HTTP, agenttest.Loopback(ports[0]), acc.Username, "wrong", canary.Addr); err == nil {
		t.Fatal("wrong password accepted")
	}
	agenttest.Eventually(t, 5*time.Second, func() bool { return ctl.Used("sub_bin") >= uint64(n) }, "usage not reported")
	if reps := ctl.Reports(); !strings.HasPrefix(reps[len(reps)-1].AgentVersion, "3.") {
		t.Fatalf("agent version %q", reps[len(reps)-1].AgentVersion)
	}

	// SIGHUP：GOST 的重载会清空认证器与链注册表，agent 模式必须拒绝重载。
	if runtime.GOOS != "windows" {
		p.cmd.Process.Signal(syscall.SIGHUP)
		time.Sleep(500 * time.Millisecond)
		fetchAll("after SIGHUP")
		agenttest.Eventually(t, 5*time.Second, func() bool { return strings.Contains(p.out.String(), "不支持重载") }, "reload not refused")
		if p.exited() {
			t.Fatal("gost exited after SIGHUP")
		}
	}
	if canary.Hits.Load() != 0 {
		t.Fatal("target reached directly")
	}

	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("gost did not exit on SIGTERM")
	}
	for _, f := range []string{"used.json", "desired.json"} {
		if _, err := os.Stat(filepath.Join(dataDir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}
