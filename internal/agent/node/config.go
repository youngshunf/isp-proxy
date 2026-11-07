package node

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是节点 agent 的启动配置（gost -agent-config <file>，YAML）。
// 示例见 deploy/agent.example.yaml。路径中不放任何密钥内容，只放文件位置。
type Config struct {
	// NodeID 只用于日志核对：节点身份以客户端证书 CN（node:<id>）为准，控制面不读取本字段。
	NodeID string `yaml:"node_id"`

	Control ControlConfig `yaml:"control"`

	// DataDir 保存本地状态（权限 0700）：used.json（各订阅累计用量）与 desired.json
	// （最后一次已应用的期望状态，含接入密码与上游凭据，权限 0600）。
	DataDir string `yaml:"data_dir"`

	Entry       EntryConfig       `yaml:"entry"`
	Egress      EgressConfig      `yaml:"egress"`
	UpstreamTLS UpstreamTLSConfig `yaml:"upstream_tls"`
	Probe       ProbeConfig       `yaml:"probe"`

	// ReportInterval 是上报间隔，默认且最大 5 秒（docs/06 §5）。
	ReportInterval time.Duration `yaml:"report_interval"`
	// FlushInterval 是累计用量落盘间隔，默认且最大 1 秒（D-17）。
	FlushInterval time.Duration `yaml:"flush_interval"`
}

// ControlConfig 是到控制面节点接口（:8443，mTLS）的连接配置。
type ControlConfig struct {
	URL      string `yaml:"url"`       // 例如 https://control.internal:8443
	CAFile   string `yaml:"ca_file"`   // 签发控制面服务端证书的 CA
	CertFile string `yaml:"cert_file"` // 节点客户端证书（CN = node:<id>）
	KeyFile  string `yaml:"key_file"`
	// ServerName 覆盖校验服务端证书时使用的名字；为空时取 URL 中的主机名。
	ServerName string `yaml:"server_name"`
}

// EntryConfig 是客户接入端口的配置。
type EntryConfig struct {
	// BindHost 是接入端口绑定的地址；为空表示全部地址。端口由控制面下发。
	BindHost string `yaml:"bind_host"`
	// CertFile、KeyFile 是 HTTPS 入口证书（必填）。文件变化后自动热更新，不重建服务（R-13）。
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// CertReloadInterval 是检查证书文件变化的间隔，默认 1 分钟。
	CertReloadInterval time.Duration `yaml:"cert_reload_interval"`
}

// EgressConfig 是出站封禁的例外（A-W4）。默认封禁 25 端口、私网、环回与云元数据地址；
// 例外只用于开发与回环 E2E，生产保持为空。
type EgressConfig struct {
	AllowCIDRs []string `yaml:"allow_cidrs"`
	AllowPorts []int    `yaml:"allow_ports"`
}

// UpstreamTLSConfig 是 https 上游的 TLS 校验设置。
type UpstreamTLSConfig struct {
	// CAFile 非空时用它校验上游证书，否则用系统根证书。
	CAFile string `yaml:"ca_file"`
	// InsecureSkipVerify 跳过上游证书校验，只用于开发。
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// ProbeConfig 是出口检测的配置。
type ProbeConfig struct {
	// EchoURL 是 IP 回显端点（返回纯文本 IP）。非空时覆盖探测任务自带的 echo_url，
	// 使节点只访问运营配置的端点；为空时使用任务中的地址。
	EchoURL string `yaml:"echo_url"`
	// Timeout 是单次检测的上限（任务的 deadline 更早时以任务为准），默认 15 秒。
	Timeout time.Duration `yaml:"timeout"`
	// Concurrency 是同时进行的检测数，默认 4。
	Concurrency int `yaml:"concurrency"`
}

// LoadConfig 读取并校验配置文件。未知字段视为错误（防止拼写错误被静默忽略）。
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent 配置: %w", err)
	}
	return ParseConfig(b)
}

// ParseConfig 解析并校验 YAML 配置，填充默认值。
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("agent 配置: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("agent 配置: %w", err)
	}
	return &c, nil
}

const (
	maxReportInterval = 5 * time.Second
	maxFlushInterval  = time.Second
)

func (c *Config) normalize() error {
	var errs []error
	req := func(v, name string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("缺少 %s", name))
		}
	}
	req(c.Control.URL, "control.url")
	req(c.Control.CAFile, "control.ca_file")
	req(c.Control.CertFile, "control.cert_file")
	req(c.Control.KeyFile, "control.key_file")
	req(c.DataDir, "data_dir")
	req(c.Entry.CertFile, "entry.cert_file")
	req(c.Entry.KeyFile, "entry.key_file")
	if c.Control.URL != "" {
		if u, err := url.Parse(c.Control.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf("control.url %q 必须是 https://host:port", c.Control.URL))
		}
	}
	if c.Probe.EchoURL != "" {
		if u, err := url.Parse(c.Probe.EchoURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("probe.echo_url %q 必须是 http(s) URL", c.Probe.EchoURL))
		}
	}
	for _, s := range c.Egress.AllowCIDRs {
		if _, err := netip.ParsePrefix(s); err != nil {
			errs = append(errs, fmt.Errorf("egress.allow_cidrs: %q 不是 CIDR", s))
		}
	}
	for _, p := range c.Egress.AllowPorts {
		if p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("egress.allow_ports: %d 越界", p))
		}
	}
	switch {
	case c.ReportInterval == 0:
		c.ReportInterval = maxReportInterval
	case c.ReportInterval < 0 || c.ReportInterval > maxReportInterval:
		errs = append(errs, fmt.Errorf("report_interval 必须在 (0, %v]", maxReportInterval))
	}
	switch {
	case c.FlushInterval == 0:
		c.FlushInterval = maxFlushInterval
	case c.FlushInterval < 0 || c.FlushInterval > maxFlushInterval:
		errs = append(errs, fmt.Errorf("flush_interval 必须在 (0, %v]", maxFlushInterval))
	}
	if c.Entry.CertReloadInterval <= 0 {
		c.Entry.CertReloadInterval = time.Minute
	}
	if c.Probe.Timeout <= 0 {
		c.Probe.Timeout = 15 * time.Second
	}
	if c.Probe.Concurrency <= 0 {
		c.Probe.Concurrency = 4
	}
	return errors.Join(errs...)
}

func (c *Config) allowCIDRs() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(c.Egress.AllowCIDRs))
	for _, s := range c.Egress.AllowCIDRs {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}
