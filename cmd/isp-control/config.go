package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/mail"
	"github.com/go-gost/gost/internal/control/secret"
	"gopkg.in/yaml.v3"
)

// 环境变量（docs/06 §2）：敏感值只从环境变量或文件读取，配置文件里不放密钥。
const (
	EnvDatabaseURL   = "ISP_DATABASE_URL"
	EnvSecretKey     = "ISP_SECRET_KEY"      // 64 位十六进制或 base64 的 32 字节
	EnvSecretKeyFile = "ISP_SECRET_KEY_FILE" // 或者从文件读取
	EnvSMTPPassword  = "ISP_SMTP_PASSWORD"
	EnvSMTPPassFile  = "ISP_SMTP_PASSWORD_FILE"
	EnvAgentKeyFile  = "ISP_AGENT_TLS_KEY_FILE" // 覆盖 agent.key_file
	EnvAdminPassword = "ISP_ADMIN_PASSWORD"     // admin create 的密码（否则从标准输入读一行）
)

// Config 是 isp-control 的 YAML 配置（示例见 deploy/isp-control.example.yaml）。
type Config struct {
	// Listen 是对外 HTTP 监听地址（反向代理之后），默认 :8080。
	Listen string `yaml:"listen"`
	// SecureCookies 为 true 时 Cookie 带 Secure（生产必须开启，经 HTTPS 访问）。
	SecureCookies bool `yaml:"secure_cookies"`
	// TrustedProxies 是可信反向代理网段（读取 X-Forwarded-For）；默认只信任本机回环。
	TrustedProxies []string `yaml:"trusted_proxies"`
	// TOTPIssuer 显示在验证器 App 中。
	TOTPIssuer string `yaml:"totp_issuer"`

	Agent AgentConfig `yaml:"agent"`
	Core  CoreConfig  `yaml:"core"`
	Mail  MailConfig  `yaml:"mail"`
	Log   LogConfig   `yaml:"log"`
	// Dev 只用于开发与平台 E2E（tests/isp），生产配置不得出现；配置了任一项时 serve 启动打印警告。
	Dev DevConfig `yaml:"dev"`

	// 以下来自环境变量。
	DatabaseURL  string `yaml:"-"`
	SecretKey    []byte `yaml:"-"`
	SMTPPassword string `yaml:"-"`
}

// AgentConfig 是节点内部接口（mTLS）。
type AgentConfig struct {
	// Listen 默认 :8443；设为 "off" 关闭（例如只跑对外 API 的开发环境）。
	Listen   string `yaml:"listen"`
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"` // 也可由 ISP_AGENT_TLS_KEY_FILE 提供
	// LongPoll 是期望状态长轮询最长等待，默认 25s。
	LongPoll time.Duration `yaml:"long_poll"`
}

// MailConfig 是发信配置。
type MailConfig struct {
	// Mode：smtp（生产）或 file（开发与测试，把邮件写成 .eml 到 file_dir）。
	Mode     string `yaml:"mode"`
	FileDir  string `yaml:"file_dir"`
	From     string `yaml:"from"`
	Brand    string `yaml:"brand"`
	SiteURL  string `yaml:"site_url"`
	SMTPAddr string `yaml:"smtp_addr"`
	// SMTPSecurity：starttls（默认）、tls、plain。
	SMTPSecurity string `yaml:"smtp_security"`
	SMTPUsername string `yaml:"smtp_username"`
}

// CoreConfig 是业务内核（kernel.Config）的配置；零值取 kernel 默认值。
type CoreConfig struct {
	// EchoURL 是节点探测使用的 IP 回显端点（经候选/当前出口访问，返回出口 IP）。节点 probe.echo_url 非空时以节点配置为准。
	EchoURL string `yaml:"echo_url"`
	// ExitCountries 是静态“出口 IP → 国家代码”映射：探测结果只带 IP，控制面据此判定国家是否符合套餐。
	// 开发与测试用；生产应接 GeoIP 库（kernel.GeoLookup）。未列出的 IP 视为国家未知，换出口比较不通过。
	ExitCountries map[string]string `yaml:"exit_countries"`
	// SchedulerInterval 是调度循环间隔（默认 5 秒，docs/06 §3）。
	SchedulerInterval time.Duration `yaml:"scheduler_interval"`
	// TaskTimeout 是变更任务等待节点应用的上限（默认 2 分钟）。
	TaskTimeout time.Duration `yaml:"task_timeout"`
	// ProbeTimeout 是探测截止时间（默认 60 秒）。
	ProbeTimeout time.Duration `yaml:"probe_timeout"`
	// ExitCheckInterval 是定时出口检测间隔（默认 10 分钟）。
	ExitCheckInterval time.Duration `yaml:"exit_check_interval"`
	// NodeOfflineAfter 超过即视为节点离线（默认 30 秒）。
	NodeOfflineAfter time.Duration `yaml:"node_offline_after"`
	// DirectDefaultBandwidthMbps 是管理员赠送直出代理时未填带宽上限的默认值（D-25，默认 10 Mbps）。
	DirectDefaultBandwidthMbps int `yaml:"direct_default_bandwidth_mbps"`
	// DirectMaxConnections 是直出代理的并发连接上限（D-25，默认 64）。
	DirectMaxConnections int `yaml:"direct_max_connections"`
	// SharedExitDefaultCapacity 是录入上游资源时不填绑定上限的默认值（D-24，默认 3）。
	SharedExitDefaultCapacity int `yaml:"shared_exit_default_capacity"`
}

// Kernel 转为 kernel.Config（Box 由 api.Assemble 填写）。
func (c CoreConfig) Kernel() kernel.Config {
	var geo kernel.GeoLookup
	if len(c.ExitCountries) > 0 {
		geo = kernel.StaticGeo(c.ExitCountries)
	}
	return kernel.Config{EchoURL: c.EchoURL, Geo: geo, SchedulerInterval: c.SchedulerInterval, TaskTimeout: c.TaskTimeout,
		ProbeTimeout: c.ProbeTimeout, ExitCheckInterval: c.ExitCheckInterval, NodeOfflineAfter: c.NodeOfflineAfter,
		DirectDefaultBandwidthMbps: c.DirectDefaultBandwidthMbps, DirectMaxConnections: c.DirectMaxConnections,
		SharedExitDefaultCapacity: c.SharedExitDefaultCapacity}
}

// DevConfig 是只用于开发与平台 E2E 的开关。
type DevConfig struct {
	// ClockOffsetFile 非空时，控制面业务时钟 = 系统时间 + 该文件中的 Go duration（如 "192h"、"-1m"；空文件或不存在为 0）。
	// 文件每 200 毫秒检查一次，测试据此拨动保留期、未付款预留、冷却等时间（docs/05 §7）。
	ClockOffsetFile string `yaml:"clock_offset_file"`
}

// Enabled 报告是否配置了任何开发开关。
func (d DevConfig) Enabled() bool { return d.ClockOffsetFile != "" }

// LogConfig 是日志配置。
type LogConfig struct {
	Level  string `yaml:"level"`  // debug / info（默认）/ warn / error
	Format string `yaml:"format"` // text（默认）/ json
}

func readSecret(envName, fileEnv string) (string, error) {
	if v := os.Getenv(envName); v != "" {
		return v, nil
	}
	if f := os.Getenv(fileEnv); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("读取 %s: %w", fileEnv, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

// LoadConfig 读取 YAML 配置（path 为空时全用默认值）与环境变量；密钥与数据库由 LoadSecretKey、RequireDatabase 按需检查。
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		// 空文件（只有注释）视为全默认。
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.Agent.Listen == "" {
		cfg.Agent.Listen = ":8443"
	}
	if k := os.Getenv(EnvAgentKeyFile); k != "" {
		cfg.Agent.KeyFile = k
	}
	if cfg.Mail.Mode == "" {
		cfg.Mail.Mode = "file"
	}
	if cfg.Mail.FileDir == "" {
		cfg.Mail.FileDir = "./var/mail"
	}
	if cfg.Mail.From == "" {
		cfg.Mail.From = "no-reply@example.invalid"
	}
	cfg.DatabaseURL = os.Getenv(EnvDatabaseURL)
	pw, err := readSecret(EnvSMTPPassword, EnvSMTPPassFile)
	if err != nil {
		return nil, err
	}
	cfg.SMTPPassword = pw
	return cfg, nil
}

// LoadSecretKey 读取 ISP_SECRET_KEY（或 ISP_SECRET_KEY_FILE）。
func (c *Config) LoadSecretKey() error {
	s, err := readSecret(EnvSecretKey, EnvSecretKeyFile)
	if err != nil {
		return err
	}
	if s == "" {
		return fmt.Errorf("缺少 %s（或 %s）：32 字节密钥，可用 `openssl rand -hex 32` 生成", EnvSecretKey, EnvSecretKeyFile)
	}
	k, err := secret.ParseKey(s)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvSecretKey, err)
	}
	c.SecretKey = k
	return nil
}

// RequireDatabase 检查数据库连接串。
func (c *Config) RequireDatabase() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("缺少 %s（PostgreSQL 连接串）", EnvDatabaseURL)
	}
	return nil
}

// ProxyPrefixes 解析 trusted_proxies。
func (c *Config) ProxyPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range c.TrustedProxies {
		if !strings.Contains(s, "/") {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("trusted_proxies: %q: %w", s, err)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %q: %w", s, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Sender 按配置构造发信器。
func (c *Config) Sender() (mail.Sender, error) {
	switch c.Mail.Mode {
	case "file":
		return &mail.FileSender{Dir: c.Mail.FileDir}, nil
	case "smtp":
		if c.Mail.SMTPAddr == "" {
			return nil, errors.New("mail.mode = smtp 时必须配置 mail.smtp_addr")
		}
		switch c.Mail.SMTPSecurity {
		case "", mail.SMTPStartTLS, mail.SMTPTLS, mail.SMTPPlain:
		default:
			return nil, fmt.Errorf("mail.smtp_security 只能是 starttls、tls 或 plain")
		}
		return &mail.SMTPSender{Addr: c.Mail.SMTPAddr, Security: c.Mail.SMTPSecurity, Username: c.Mail.SMTPUsername, Password: c.SMTPPassword}, nil
	}
	return nil, fmt.Errorf("mail.mode 只能是 smtp 或 file，当前 %q", c.Mail.Mode)
}
