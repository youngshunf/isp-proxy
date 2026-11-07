package node

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agent/agenttest"
	"github.com/go-gost/gost/internal/agentproto"
)

// 仓库里的占位示例必须能被解析（字段名与代码一致）。
func TestExampleConfigParses(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", "..", "deploy", "agent.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Control.URL != "https://control.internal.example:8443" || c.DataDir != "/var/lib/isp-agent" ||
		c.ReportInterval != 5*time.Second || c.FlushInterval != time.Second || c.Entry.CertReloadInterval != time.Minute ||
		c.Probe.Timeout != 15*time.Second || c.Probe.Concurrency != 4 || c.UpstreamTLS.InsecureSkipVerify {
		t.Fatalf("%+v", c)
	}
}

func TestConfigValidation(t *testing.T) {
	base := `
control: {url: "https://c:8443", ca_file: ca, cert_file: c, key_file: k}
data_dir: /tmp/x
entry: {cert_file: e, key_file: ek}
`
	if c, err := ParseConfig([]byte(base)); err != nil || c.ReportInterval != 5*time.Second || c.FlushInterval != time.Second {
		t.Fatalf("defaults: %v %+v", err, c)
	}
	for name, extra := range map[string]string{
		"unknown field":       "typo_field: 1\n",
		"report > 5s":         "report_interval: 6s\n",
		"flush > 1s":          "flush_interval: 2s\n",
		"bad cidr":            "egress: {allow_cidrs: [nope]}\n",
		"bad port":            "egress: {allow_ports: [70000]}\n",
		"bad echo":            "probe: {echo_url: 'ftp://x'}\n",
		"control not https":   strings.Replace(base, "https://c:8443", "http://c:8443", 1),
		"missing entry cert":  strings.Replace(base, "cert_file: e,", "", 1),
		"missing data dir":    strings.Replace(base, "data_dir: /tmp/x", "", 1),
		"missing control key": strings.Replace(base, ", key_file: k", "", 1),
	} {
		doc := base + extra
		if strings.HasPrefix(name, "control") || strings.HasPrefix(name, "missing") {
			doc = extra
		}
		if _, err := ParseConfig([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// desired.json 只保存已成功应用的条目，权限 0600；含接入密码与上游凭据（因此限制权限）。
func TestSavedStateContentsAndPermissions(t *testing.T) {
	e := newEnv(t)
	acc := e.access(1, "u-s1")
	en := e.entry("s1", 1<<30, time.Hour, acc, &e.exitA)
	e.put(en)
	path := filepath.Join(e.cfg.DataDir, stateFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st savedState
	if err := json.Unmarshal(b, &st); err != nil || st.Format != stateFormat || len(st.Subscriptions) != 1 ||
		st.Subscriptions[0].Version != en.Version || st.Subscriptions[0].Access.Password != acc.Password || st.Revision == 0 {
		t.Fatalf("%v %s", err, b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// 被外部放宽的权限在加载时收紧。
	os.Chmod(path, 0o644)
	if _, err := loadState(path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode not tightened: %v", fi.Mode())
	}
	// 失败的更高版本不落盘。
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	bad := en
	bad.Version = e.ver.Add(1)
	bad.Access = e.access(2, "u-s1-v2")
	bad.Access.Ports.HTTP = busy.Addr().(*net.TCPAddr).Port
	e.putNoWait(bad)
	e.ctl.WaitReport(t, 5*time.Second, func(r *agentproto.Report) bool {
		s, ok := agenttest.SubIn(r, "s1")
		return ok && s.ApplyError != nil
	})
	if loaded, _ := loadState(path); loaded.Subscriptions[0].Version != en.Version || loaded.Subscriptions[0].Access.Version != 1 {
		t.Fatalf("saved %+v", loaded.Subscriptions[0])
	}
}
