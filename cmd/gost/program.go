package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-gost/core/auth"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/service"
	api_service "github.com/go-gost/x/api/service"
	xauth "github.com/go-gost/x/auth"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/config/loader"
	auth_parser "github.com/go-gost/x/config/parsing/auth"
	"github.com/go-gost/x/config/parsing/parser"
	xmetrics "github.com/go-gost/x/metrics"
	metrics "github.com/go-gost/x/metrics/service"
	xplugin "github.com/go-gost/x/plugin"
	"github.com/go-gost/x/registry"
	"github.com/judwhite/go-svc"

	"github.com/go-gost/gost/internal/agent/node"
)

type program struct {
	srvApi       service.Service
	srvMetrics   service.Service
	srvProfiling *http.Server

	// agent 是 ISP 节点 agent 运行时（-agent-config）；为空表示上游 GOST 模式。
	agent *node.Runtime

	cancel context.CancelFunc
}

func (p *program) Init(env svc.Environment) error {
	files := cfgFiles
	if agentConfig != "" && len(files) == 0 && len(services) == 0 && len(nodes) == 0 {
		// agent 模式下接入服务全部来自控制面：没有 -C/-L/-F 时不去查找默认的 gost.yml。
		files = []string{"{}"}
	}
	parser.Init(parser.Args{
		CfgFiles:    files,
		Services:    services,
		Nodes:       nodes,
		Debug:       debug,
		Trace:       trace,
		ApiAddr:     apiAddr,
		MetricsAddr: metricsAddr,
	})

	return nil
}

func (p *program) Start() error {
	var agentCfg *node.Config
	if agentConfig != "" {
		var err error
		if agentCfg, err = node.LoadConfig(agentConfig); err != nil {
			return err
		}
	}

	cfg, err := parser.Parse()
	if err != nil {
		return err
	}
	if agentCfg != nil {
		if err := checkAgentMode(cfg); err != nil {
			return err
		}
	}

	if outputFormat != "" {
		if err := cfg.Write(os.Stdout, outputFormat); err != nil {
			return err
		}
		os.Exit(0)
	}

	config.Set(cfg)

	// Enable metrics before loading services so that listener wrappers
	// can observe the enabled state at Init time.
	if cfg.Metrics != nil && cfg.Metrics.Addr != "" {
		xmetrics.Enable(true)
	}

	if err := loader.Load(cfg); err != nil {
		return err
	}

	if err := p.run(cfg); err != nil {
		return err
	}

	// agent 在 GOST 配置加载之后启动：loader.Load 会重设默认 TLS 配置，agent 随后把它换成 HTTPS 入口证书。
	if agentCfg != nil {
		rt, err := node.Start(agentCfg, node.Options{AgentVersion: getVersion()})
		if err != nil {
			return err
		}
		p.agent = rt
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.reload(ctx)

	return nil
}

// errAgentReload 表示 agent 模式不支持重载 GOST 配置。
var errAgentReload = errors.New("agent 模式不支持重载 GOST 配置（SIGHUP、-R）：重载会清空认证器与出口链注册表，" +
	"使 isp-auth 缺失时放行、出口链缺失时直连；改配置请重启进程")

// checkAgentMode 检查 agent 模式的约束（docs/06 §2）：不开放 GOST 原生 API（-api、配置文件 api 段、GOST_API），
// 不允许定时重载。GOST 原生 API 能增删服务、链与认证器，绕过 agent 的计量与出口保护。
func checkAgentMode(cfg *config.Config) error {
	if cfg.API != nil {
		return errors.New("agent 模式不开放 GOST 原生 API：去掉 -api、配置文件中的 api 段或 GOST_API 环境变量")
	}
	if reload > 0 {
		return errAgentReload
	}
	return nil
}

func (p *program) run(cfg *config.Config) error {
	for _, svc := range registry.ServiceRegistry().GetAll() {
		svc := svc
		go func() {
			svc.Serve()
		}()
	}

	if p.srvApi != nil {
		p.srvApi.Close()
		p.srvApi = nil
	}
	if cfg.API != nil {
		s, err := buildApiService(cfg.API)
		if err != nil {
			return err
		}

		p.srvApi = s

		go func() {
			defer s.Close()

			log := logger.Default().WithFields(map[string]any{"kind": "service", "service": "@api"})

			log.Info("listening on ", s.Addr())
			if err := s.Serve(); !errors.Is(err, http.ErrServerClosed) {
				log.Error(err)
			}
		}()
	}

	if p.srvMetrics != nil {
		p.srvMetrics.Close()
		p.srvMetrics = nil
	}
	if cfg.Metrics != nil && cfg.Metrics.Addr != "" {
		s, err := buildMetricsService(cfg.Metrics)
		if err != nil {
			return err
		}

		p.srvMetrics = s

		go func() {
			defer s.Close()

			log := logger.Default().WithFields(map[string]any{"kind": "service", "service": "@metrics"})

			log.Info("listening on ", s.Addr())
			if err := s.Serve(); !errors.Is(err, http.ErrServerClosed) {
				log.Error(err)
			}
		}()
	}

	if p.srvProfiling != nil {
		p.srvProfiling.Close()
		p.srvProfiling = nil
	}
	if cfg.Profiling != nil {
		addr := cfg.Profiling.Addr
		if addr == "" {
			addr = ":6060"
		}
		s := &http.Server{
			Addr: addr,
		}
		p.srvProfiling = s

		go func() {
			defer s.Close()

			log := logger.Default().WithFields(map[string]any{"kind": "service", "service": "@profiling"})

			log.Info("listening on ", addr)
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.Error(err)
			}
		}()
	}

	return nil
}

func (p *program) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}

	if p.agent != nil {
		if err := p.agent.Close(); err != nil {
			logger.Default().Errorf("agent shutdown: %v", err)
		}
		logger.Default().Debug("agent shutdown")
	}

	for name, srv := range registry.ServiceRegistry().GetAll() {
		srv.Close()
		logger.Default().Debugf("service %s shutdown", name)
	}

	if p.srvApi != nil {
		p.srvApi.Close()
		logger.Default().Debug("service @api shutdown")
	}
	if p.srvMetrics != nil {
		p.srvMetrics.Close()
		logger.Default().Debug("service @metrics shutdown")
	}
	if p.srvProfiling != nil {
		p.srvProfiling.Close()
		logger.Default().Debug("service @profiling shutdown")
	}

	xplugin.Shutdown()

	return nil
}

func (p *program) reload(ctx context.Context) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)

	var ticker <-chan time.Time
	if reload > 0 {
		t := time.NewTicker(reload)
		defer t.Stop()
		ticker = t.C
	}

	for {
		select {
		case <-c:
			if err := p.reloadConfig(); err != nil {
				logger.Default().Error(err)
			} else {
				logger.Default().Info("config reloaded")
			}

		case <-ticker:
			if err := p.reloadConfig(); err != nil {
				logger.Default().Errorf("auto reload: %v", err)
			} else {
				logger.Default().Debug("config auto reloaded")
			}

		case <-ctx.Done():
			return
		}
	}
}

func (p *program) reloadConfig() error {
	if p.agent != nil {
		return errAgentReload
	}
	cfg, err := parser.Parse()
	if err != nil {
		return err
	}
	config.Set(cfg)

	if err := loader.Load(cfg); err != nil {
		return err
	}

	if err := p.run(cfg); err != nil {
		return err
	}

	return nil
}

func buildApiService(cfg *config.APIConfig) (service.Service, error) {
	var authers []auth.Authenticator
	if auther := auth_parser.ParseAutherFromAuth(cfg.Auth); auther != nil {
		authers = append(authers, auther)
	}
	if cfg.Auther != "" {
		authers = append(authers, registry.AutherRegistry().Get(cfg.Auther))
	}

	var auther auth.Authenticator
	if len(authers) > 0 {
		auther = xauth.AuthenticatorGroup(authers...)
	}

	network := "tcp"
	addr := cfg.Addr
	if strings.HasPrefix(addr, "unix://") {
		network = "unix"
		addr = strings.TrimPrefix(addr, "unix://")
	}
	return api_service.NewService(
		network, addr,
		api_service.PathPrefixOption(cfg.PathPrefix),
		api_service.AccessLogOption(cfg.AccessLog),
		api_service.AutherOption(auther),
	)
}

func buildMetricsService(cfg *config.MetricsConfig) (service.Service, error) {
	auther := auth_parser.ParseAutherFromAuth(cfg.Auth)
	if cfg.Auther != "" {
		auther = registry.AutherRegistry().Get(cfg.Auther)
	}

	network := "tcp"
	addr := cfg.Addr
	if strings.HasPrefix(addr, "unix://") {
		network = "unix"
		addr = strings.TrimPrefix(addr, "unix://")
	}
	return metrics.NewService(
		network, addr,
		metrics.PathOption(cfg.Path),
		metrics.AutherOption(auther),
	)
}
