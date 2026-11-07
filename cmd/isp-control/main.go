// Command isp-control 是 ISP 代理平台的控制面（docs/05 §2、docs/06 §2）。
//
//	isp-control serve     -config isp-control.yaml   启动：执行迁移，:8080 对外 API，:8443 节点 mTLS 接口
//	isp-control migrate   [-config …]                只执行数据库迁移
//	isp-control dev-certs -dir ./dev-certs -node nod_… 生成开发用 CA、服务端与节点证书（不进 Git）
//	isp-control admin create -email … -name … [-role admin|viewer]   建立管理员（密码取 ISP_ADMIN_PASSWORD 或标准输入）
//	isp-control seed-dev  [-config …]                写入开发种子数据（示例价格与上游，仅开发使用）
//
// 环境变量：ISP_DATABASE_URL、ISP_SECRET_KEY（或 ISP_SECRET_KEY_FILE）、ISP_SMTP_PASSWORD、ISP_AGENT_TLS_KEY_FILE。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `用法：
  isp-control serve     [-config isp-control.yaml]
  isp-control migrate   [-config isp-control.yaml]
  isp-control dev-certs [-dir ./dev-certs] [-node <节点ID>]... [-host <服务端主机名或 IP>]...
  isp-control admin create -email <邮箱> [-name <显示名>] [-role admin|viewer]
  isp-control seed-dev  [-config isp-control.yaml] [-user-email dev@example.com]

环境变量：ISP_DATABASE_URL、ISP_SECRET_KEY（或 ISP_SECRET_KEY_FILE）、ISP_SMTP_PASSWORD、ISP_AGENT_TLS_KEY_FILE、ISP_ADMIN_PASSWORD
`

// run 执行子命令，返回退出码。
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = cmdServe(ctx, args[1:], stderr, nil)
	case "migrate":
		err = cmdMigrate(ctx, args[1:], stdout)
	case "dev-certs":
		err = cmdDevCerts(args[1:], stdout)
	case "admin":
		if len(args) < 2 || args[1] != "create" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		err = cmdAdminCreate(ctx, args[2:], stdin, stdout)
	case "seed-dev":
		err = cmdSeedDev(ctx, args[1:], stdout)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "isp-control:", err)
		return 1
	}
	return 0
}
