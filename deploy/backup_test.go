package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBackupScript 用桩 pg_dump/pg_restore 运行 backup.sh：系统自带的 bash（macOS 为 3.2）在 set -euo pipefail 下必须退出 0，
// 并输出备份路径。曾因 "$out（" 被 bash 3.2 解析成变量名 out\xef… 报 unbound variable（A-22）。
func TestBackupScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash 脚本")
	}
	bashes := []string{"/bin/bash"}
	if p, err := exec.LookPath("bash"); err == nil && p != "/bin/bash" {
		bashes = append(bashes, p)
	}
	for _, bash := range bashes {
		if _, err := os.Stat(bash); err != nil {
			continue
		}
		t.Run(bash, func(t *testing.T) {
			bin := t.TempDir()
			stub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// pg_dump --file=<path> …：写一个非空文件。
			stub("pg_dump", `for a in "$@"; do case "$a" in --file=*) printf 'PGDMP' > "${a#--file=}";; esac; done`)
			stub("pg_restore", `exit 0`)
			dir := filepath.Join(t.TempDir(), "备份")
			for _, lang := range []string{"C", "en_US.UTF-8", "zh_CN.UTF-8"} {
				cmd := exec.Command(bash, "backup.sh", dir)
				cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "ISP_DATABASE_URL=postgres://stub", "LANG=" + lang, "LC_ALL=" + lang}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("LANG=%s：backup.sh 失败：%v\n%s", lang, err, out)
				}
				if !strings.Contains(string(out), "备份完成："+dir+"/isp-control-") {
					t.Fatalf("LANG=%s：输出缺少备份路径：%s", lang, out)
				}
			}
			dumps, _ := filepath.Glob(filepath.Join(dir, "isp-control-*.dump"))
			partial, _ := filepath.Glob(filepath.Join(dir, ".*.partial"))
			if len(dumps) == 0 || len(partial) != 0 {
				t.Fatalf("备份文件：%v，残留：%v", dumps, partial)
			}
		})
	}
}
