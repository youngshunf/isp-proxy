package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/store/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func runCmd(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb); code != 0 {
		t.Fatalf("isp-control %v 退出码 %d：%s%s", args, code, out.String(), errb.String())
	}
	return out.String()
}

// 冒烟：dev-certs → migrate → seed-dev → admin create → serve（对外接口 + mTLS 节点接口），然后优雅停止。
func TestServeSmoke(t *testing.T) {
	dbURL := pgtest.NewDatabase(t)
	dir := t.TempDir()
	t.Setenv(EnvDatabaseURL, dbURL)
	t.Setenv(EnvSecretKey, strings.Repeat("ab", 32))

	certs := filepath.Join(dir, "certs")
	out := runCmd(t, "", "dev-certs", "-dir", certs, "-node", "nod_dev00000000000000001")
	if !strings.Contains(out, "node-nod_dev00000000000000001.pem") {
		t.Fatal(out)
	}
	mailDir := filepath.Join(dir, "mail")
	cfgPath := filepath.Join(dir, "isp-control.yaml")
	cfg := `
listen: 127.0.0.1:0
secure_cookies: false
totp_issuer: 冒烟测试
agent:
  listen: 127.0.0.1:0
  ca_file: ` + filepath.Join(certs, "ca.pem") + `
  cert_file: ` + filepath.Join(certs, "server.pem") + `
  key_file: ` + filepath.Join(certs, "server-key.pem") + `
mail:
  mode: file
  file_dir: ` + mailDir + `
  from: 冒烟 <no-reply@example.com>
  brand: 冒烟代理
  site_url: https://portal.example.test
log:
  level: warn
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := runCmd(t, "", "migrate", "-config", cfgPath); !strings.Contains(out, "0001_init.sql") {
		t.Fatal(out)
	}
	runCmd(t, "", "migrate", "-config", cfgPath) // 重复执行是空操作
	runCmd(t, "", "seed-dev", "-config", cfgPath)
	runCmd(t, "", "seed-dev", "-config", cfgPath) // 可重复执行
	if out := runCmd(t, "admin-password-123\n", "admin", "create", "-config", cfgPath, "-email", "ops@example.com", "-name", "运维"); !strings.Contains(out, "已创建管理员 adm_") {
		t.Fatal(out)
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"admin", "create", "-config", cfgPath, "-email", "x@example.com"}, strings.NewReader("short\n"), io.Discard, &stderr); code == 0 {
		t.Fatal("短密码应当失败")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan [2]string, 1)
	done := make(chan error, 1)
	go func() {
		done <- cmdServe(ctx, []string{"-config", cfgPath}, io.Discard, &serveHooks{ready: func(p, a string) { ready <- [2]string{p, a} }})
	}()
	var addrs [2]string
	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatalf("serve 提前退出: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("serve 启动超时")
	}
	base := "http://" + addrs[0]

	res, err := http.Get(base + "/healthz")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", res, err)
	}
	res.Body.Close()

	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	res, err = hc.Get(base + "/api/me")
	if err != nil || res.StatusCode != 401 {
		t.Fatalf("/api/me: %v %v", res, err)
	}
	res.Body.Close()
	var csrf string
	for _, c := range res.Cookies() {
		if c.Name == "isp_csrf" {
			csrf = c.Value
		}
	}
	req, _ := http.NewRequest("POST", base+"/api/account/register", strings.NewReader(`{"email":"smoke@example.com","password":"smoke-password-1"}`))
	req.Header.Set("X-CSRF-Token", csrf)
	res, err = hc.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("register: %v %v", res, err)
	}
	res.Body.Close()
	// 发件箱被唤醒后很快投递到文件目录。
	deadline := time.Now().Add(10 * time.Second)
	for {
		files, _ := filepath.Glob(filepath.Join(mailDir, "*.eml"))
		if len(files) == 1 {
			b, _ := os.ReadFile(files[0])
			if !strings.Contains(string(b), "smoke@example.com") {
				t.Fatal(string(b))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("没有投递验证邮件")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 节点接口：带节点证书能完成 mTLS（ctl-core 未接入时按未知节点 403）；不带证书握手失败。
	tlsCfg, err := agentproto.LoadClientTLSConfig(filepath.Join(certs, "ca.pem"), filepath.Join(certs, "node-nod_dev00000000000000001.pem"),
		filepath.Join(certs, "node-nod_dev00000000000000001-key.pem"), "localhost")
	if err != nil {
		t.Fatal(err)
	}
	ac := agentproto.NewClient("https://"+addrs[1], tlsCfg)
	_, err = ac.Fetch(context.Background())
	if se, ok := err.(*agentproto.StatusError); !ok || se.Status != 403 {
		t.Fatalf("期望 403，得到 %v", err)
	}
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: tlsCfg.RootCAs, ServerName: "localhost", MinVersion: tls.VersionTLS13}}}
	if res, err := noCert.Get("https://" + addrs[1] + agentproto.PathDesired); err == nil {
		res.Body.Close()
		t.Fatal("没有客户端证书不应连上节点接口")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve 没有停止")
	}
}

func TestConfigErrors(t *testing.T) {
	t.Setenv(EnvSecretKey, "")
	t.Setenv(EnvSecretKeyFile, "")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.Agent.Listen != ":8443" || cfg.Mail.Mode != "file" {
		t.Fatalf("%+v", cfg)
	}
	if err := cfg.LoadSecretKey(); err == nil {
		t.Fatal("缺少密钥应当报错")
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(p, []byte("listen: :1\nunknown_field: 1\n"), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("未知字段应当报错")
	}
	_ = os.WriteFile(p, []byte("# 只有注释\n"), 0o600)
	if _, err := LoadConfig(p); err != nil {
		t.Fatal(err)
	}
	cfg.Mail.Mode = "smtp"
	if _, err := cfg.Sender(); err == nil {
		t.Fatal("smtp 缺地址应当报错")
	}
	kf := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(kf, []byte(strings.Repeat("cd", 32)+"\n"), 0o600)
	t.Setenv(EnvSecretKeyFile, kf)
	if err := cfg.LoadSecretKey(); err != nil || len(cfg.SecretKey) != 32 {
		t.Fatal(err)
	}
	if code := run(context.Background(), []string{"bogus"}, nil, io.Discard, io.Discard); code != 2 {
		t.Fatal(code)
	}
}
