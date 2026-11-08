package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-gost/gost/deploy"
	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/account"
	"github.com/go-gost/gost/internal/control/admin"
	"github.com/go-gost/gost/internal/control/mail"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// openDB 读取配置与 ISP_DATABASE_URL，连库并执行迁移。
func openDB(ctx context.Context, cfgPath string) (*Config, *pgxpool.Pool, []store.Migration, error) {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return nil, nil, nil, err
	}
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, nil, err
	}
	applied, err := store.Migrate(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, nil, nil, err
	}
	return cfg, pool, applied, nil
}

func cmdMigrate(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "YAML 配置文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, pool, applied, err := openDB(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, m := range applied {
		fmt.Fprintln(stdout, "已执行", m.Name)
	}
	vs, err := store.AppliedVersions(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "数据库迁移版本：%v\n", vs)
	return nil
}

func cmdDevCerts(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("dev-certs", flag.ContinueOnError)
	dir := fs.String("dir", "./dev-certs", "输出目录（不要放进 Git）")
	var nodes, hosts stringList
	fs.Var(&nodes, "node", "节点 ID（可重复），例如 nod_dev00000000000000001")
	fs.Var(&hosts, "host", "控制面服务端证书的主机名或 IP（可重复，默认 localhost、127.0.0.1、::1）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(nodes) == 0 {
		nodes = stringList{"nod_dev00000000000000001"}
	}
	out, err := agentproto.GenerateDevCerts(*dir, agentproto.DevCertOptions{ServerHosts: hosts, NodeIDs: nodes})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "CA：%s\n服务端：%s / %s\n", out.CA.Cert, out.Server.Cert, out.Server.Key)
	for _, id := range nodes {
		p := out.Nodes[id]
		fmt.Fprintf(stdout, "节点 %s：%s / %s\n", id, p.Cert, p.Key)
	}
	fmt.Fprintln(stdout, "开发证书只用于本机，不要提交到 Git。")
	return nil
}

func readPassword(stdin io.Reader, stdout io.Writer) (string, error) {
	if pw := os.Getenv(EnvAdminPassword); pw != "" {
		return pw, nil
	}
	fmt.Fprint(stdout, "管理员密码（至少 12 位，输入后回车）：")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	fmt.Fprintln(stdout)
	return strings.TrimRight(line, "\r\n"), nil
}

func cmdAdminCreate(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin create", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "YAML 配置文件")
	email := fs.String("email", "", "管理员邮箱")
	name := fs.String("name", "", "显示名")
	role := fs.String("role", admin.RoleAdmin, "角色：admin（读写）或 viewer（只读）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("必须提供 -email")
	}
	_, pool, _, err := openDB(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	pw, err := readPassword(stdin, stdout)
	if err != nil {
		return err
	}
	id, err := admin.Create(ctx, pool, *email, *name, *role, pw)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已创建管理员 %s（%s，角色 %s）。首次登录后台时绑定 TOTP 并保存恢复码。\n", id, strings.ToLower(strings.TrimSpace(*email)), *role)
	return nil
}

// cmdMailTest 按配置（mail.* 与 ISP_SMTP_PASSWORD）直接投递一封测试邮件，不经发件箱、不连数据库，
// 用于部署后核对 SMTP 账号、端口与发件人是否可用；失败时把 SMTP 服务器的原始错误打印出来。
func cmdMailTest(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mail-test", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "YAML 配置文件")
	to := fs.String("to", "", "收件人邮箱")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return errors.New("必须提供 -to")
	}
	rcpt, err := account.NormalizeEmail(*to)
	if err != nil {
		return fmt.Errorf("-to %q 不是有效的邮箱", *to)
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	sender, err := cfg.Sender()
	if err != nil {
		return err
	}
	brand := cfg.Mail.Brand
	if brand == "" {
		brand = "驿卡"
	}
	body := fmt.Sprintf("这是一封测试邮件，用于确认%s的发信设置可用。\n\n发信方式：%s\n站点地址：%s\n\n收到即说明验证码邮件可以正常送达，无需回复。\n", brand, cfg.Mail.Mode, cfg.Mail.SiteURL)
	m := &mail.Rendered{ID: store.NewID("mtt"), From: cfg.Mail.From, To: rcpt, Subject: brand + " 发信测试", Body: body}
	if err := sender.Send(ctx, m); err != nil {
		return fmt.Errorf("发送失败: %w", err)
	}
	fmt.Fprintf(stdout, "测试邮件已交给 %s 投递：%s → %s\n", cfg.Mail.Mode, cfg.Mail.From, rcpt)
	return nil
}

func cmdSeedDev(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("seed-dev", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "YAML 配置文件")
	userEmail := fs.String("user-email", "dev@example.com", "开发用户邮箱（已验证）；为空则不建")
	userPassword := fs.String("user-password", "dev-password-123", "开发用户密码（仅开发）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, pool, _, err := openDB(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	err = store.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, deploy.SeedSQL); err != nil {
			return fmt.Errorf("执行种子数据: %w", err)
		}
		if *userEmail == "" {
			return nil
		}
		e, err := account.NormalizeEmail(*userEmail)
		if err != nil {
			return errors.New("-user-email 格式不正确")
		}
		if err := account.CheckPassword(*userPassword); err != nil {
			return errors.New("-user-password 至少 10 位")
		}
		_, err = tx.Exec(ctx, `INSERT INTO users (id, email, email_verified_at, password_hash, password_changed_at)
			VALUES ($1, $2, now(), $3, now()) ON CONFLICT (email) DO NOTHING`, store.NewID("usr"), e, secret.HashPassword(*userPassword))
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "已写入开发种子数据（示例套餐：独享 us-lax、jp-tyo 与共享 us-lax-shared；流量包；节点 nod_dev00000000000000001；回环上游，其中 res_dev00000000000000002 可共享（绑定上限 3）；带示例收款说明的规则版本）。")
	if *userEmail != "" {
		fmt.Fprintf(stdout, "开发用户：%s（已存在则保持原密码）\n", *userEmail)
	}
	fmt.Fprintln(stdout, "以上均为占位数据，只能用于开发与测试。")
	return nil
}
