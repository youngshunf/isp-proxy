package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/api"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/mail"
	"github.com/go-gost/gost/internal/control/ratelimit"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/session"
	"github.com/go-gost/gost/internal/control/store"
)

// serveHooks 供测试得知实际监听地址。
type serveHooks struct {
	ready func(publicAddr, agentAddr string)
}

func newLogger(c LogConfig, w io.Writer) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(c.Level) {
	case "", "info":
		lv = slog.LevelInfo
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("log.level 不正确: %q", c.Level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch c.Format {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("log.format 只能是 text 或 json")
}

// unavailableAgent 是 ctl-core 的节点同步尚未接入时的占位：所有节点按“未知节点”拒绝（403），
// 不会给节点下发任何期望状态。
type unavailableAgent struct{}

func (unavailableAgent) Desired(context.Context, string) (*agentproto.Desired, error) {
	return nil, agentproto.ErrUnknownNode
}

func (unavailableAgent) Report(context.Context, string, *agentproto.Report) (*agentproto.ReportAck, error) {
	return nil, agentproto.ErrUnknownNode
}

func (unavailableAgent) ProbeResults(context.Context, string, []agentproto.ProbeResult) error {
	return agentproto.ErrUnknownNode
}

func cmdServe(ctx context.Context, args []string, stderr io.Writer, hooks *serveHooks) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "YAML 配置文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.LoadSecretKey(); err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	log, err := newLogger(cfg.Log, stderr)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	if prefixes, err := cfg.ProxyPrefixes(); err != nil {
		return err
	} else if len(prefixes) > 0 {
		httpx.SetTrustedProxies(prefixes)
	}
	sender, err := cfg.Sender()
	if err != nil {
		return err
	}
	var agentTLS *tls.Config
	if cfg.Agent.Listen != "off" {
		if cfg.Agent.CAFile == "" || cfg.Agent.CertFile == "" || cfg.Agent.KeyFile == "" {
			return errors.New("节点接口需要 agent.ca_file、agent.cert_file 与 agent.key_file（或 ISP_AGENT_TLS_KEY_FILE）；" +
				"开发环境可先运行 `isp-control dev-certs`，或设 agent.listen: off 关闭")
		}
		if agentTLS, err = agentproto.LoadServerTLSConfig(cfg.Agent.CAFile, cfg.Agent.CertFile, cfg.Agent.KeyFile); err != nil {
			return err
		}
	}

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, err := store.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	for _, m := range applied {
		log.Info("已执行迁移", "file", m.Name)
	}
	box, err := secret.NewBox(cfg.SecretKey)
	if err != nil {
		return err
	}
	worker := &mail.Worker{Pool: pool, Box: box, Sender: sender, From: cfg.Mail.From,
		Brand: mail.Brand{Name: cfg.Mail.Brand, SiteURL: cfg.Mail.SiteURL}, Logger: log}
	worker.Init()
	deps := api.Deps{Pool: pool, Box: box, Cookies: session.Cookies{Secure: cfg.SecureCookies},
		Mail: worker, Issuer: cfg.TOTPIssuer, Logger: log, Core: cfg.Core.Kernel()}
	var devClock *offsetClock
	if cfg.Dev.Enabled() {
		log.Warn("已启用 dev 配置（只用于开发与平台 E2E，生产不得配置）", "clock_offset_file", cfg.Dev.ClockOffsetFile)
		if cfg.Dev.ClockOffsetFile != "" {
			devClock = newOffsetClock(cfg.Dev.ClockOffsetFile, log)
			deps.Now = devClock.Now
		}
	}
	plat, err := api.Assemble(deps)
	if err != nil {
		return err
	}
	if !cfg.SecureCookies {
		log.Warn("secure_cookies 未开启：Cookie 不带 Secure，只能用于本机开发")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errc := make(chan error, 2)
	goRun := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	goRun(func() { worker.Run(ctx) })
	goRun(func() { pruneLoop(ctx, pool, log) })
	if devClock != nil {
		goRun(func() { devClock.run(ctx) })
	}
	for _, bg := range plat.Background {
		bg := bg
		goRun(func() { bg(ctx) })
	}

	publicLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	public := &http.Server{Handler: plat.Handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	servers := []*http.Server{public}
	goRun(func() {
		if err := public.Serve(publicLn); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("对外接口: %w", err)
		}
	})
	log.Info("对外接口已监听", "addr", publicLn.Addr().String())

	agentAddr := ""
	if agentTLS != nil {
		srv := plat.Agent
		if srv == nil {
			log.Warn("节点同步（ctl-core）尚未接入：节点接口暂时拒绝所有节点")
			srv = unavailableAgent{}
		}
		h := agentproto.NewHandler(srv, plat.AgentHub, agentproto.HandlerOptions{MaxWait: cfg.Agent.LongPoll, Logger: log})
		ln, err := net.Listen("tcp", cfg.Agent.Listen)
		if err != nil {
			return err
		}
		agent := &http.Server{Handler: h, TLSConfig: agentTLS, ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout: 120 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
		servers = append(servers, agent)
		agentAddr = ln.Addr().String()
		goRun(func() {
			if err := agent.Serve(tls.NewListener(ln, agentTLS)); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("节点接口: %w", err)
			}
		})
		log.Info("节点接口（mTLS）已监听", "addr", agentAddr)
	}
	if hooks != nil && hooks.ready != nil {
		hooks.ready(publicLn.Addr().String(), agentAddr)
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	log.Info("正在停止")
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	wg.Wait()
	return runErr
}

// pruneLoop 每小时清理一天前的限流事件。
func pruneLoop(ctx context.Context, pool interface {
	ratelimit.DB
}, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := ratelimit.Prune(ctx, pool, time.Now().Add(-25*time.Hour)); err != nil && ctx.Err() == nil {
			log.Warn("清理限流事件失败", "err", err)
		} else if n > 0 {
			log.Debug("已清理限流事件", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
