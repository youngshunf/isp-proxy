package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-gost/gost/internal/control/mail"
)

// mail-test 按配置直接投递一封测试邮件（不经发件箱、不需要数据库），用于部署后核对 SMTP。
func TestMailTestCommand(t *testing.T) {
	dir := t.TempDir()
	mailDir := filepath.Join(dir, "mail")
	cfgPath := filepath.Join(dir, "isp-control.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
mail:
  mode: file
  file_dir: `+mailDir+`
  from: 驿卡 <no-reply@example.com>
  brand: 驿卡
  site_url: https://portal.example.test
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDatabaseURL, "")

	out := runCmd(t, "", "mail-test", "-config", cfgPath, "-to", "Someone@Example.com")
	if !strings.Contains(out, "someone@example.com") {
		t.Fatalf("输出没有收件人：%s", out)
	}
	files, _ := filepath.Glob(filepath.Join(mailDir, "*.eml"))
	if len(files) != 1 {
		t.Fatalf("应投递 1 封，实际 %d", len(files))
	}
	raw, _ := os.ReadFile(files[0])
	p, err := mail.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.To != "someone@example.com" || !strings.Contains(p.Subject, "驿卡") || !strings.Contains(p.Body, "https://portal.example.test") {
		t.Fatalf("邮件内容不对：%+v", p)
	}
	if !strings.Contains(string(raw), "From: 驿卡 <no-reply@example.com>") {
		t.Fatalf("发件人不对：%s", raw)
	}

	// 缺少收件人、收件人格式不对：报错，不投递。
	for _, args := range [][]string{
		{"mail-test", "-config", cfgPath},
		{"mail-test", "-config", cfgPath, "-to", "not-an-email"},
	} {
		var o, e bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &o, &e); code == 0 {
			t.Fatalf("%v 应失败", args)
		}
	}
	files, _ = filepath.Glob(filepath.Join(mailDir, "*.eml"))
	if len(files) != 1 {
		t.Fatalf("失败的调用不应投递，实际 %d 封", len(files))
	}
}
