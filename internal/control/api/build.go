package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/account"
	"github.com/go-gost/gost/internal/control/admin"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/session"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Domain 是导出路由的业务包（ctl-core 的 order、subscription、inventory… 都实现它）。
type Domain interface {
	Routes() []httpx.Route
}

// Deps 是组装控制面所需的依赖。cmd/isp-control 与契约测试使用同一个 Assemble，保证测试看到的路由集合就是线上的。
type Deps struct {
	Pool    *pgxpool.Pool
	Box     *secret.Box
	Cookies session.Cookies
	Mail    interface{ Kick() } // 发件箱唤醒，可为 nil
	Issuer  string              // TOTP 发行方名称
	Now     func() time.Time    // 默认 time.Now
	Logger  *slog.Logger
	// Kernel 是业务内核的可调项（检测端点、调度间隔、任务与探测超时等）；Box 由 Assemble 填写。
	Kernel kernel.Config

	// 可选：测试覆盖限流。
	AccountLimits *account.Limits
	AdminLimits   *admin.Limits
}

// Platform 是组装好的控制面。
type Platform struct {
	Handler http.Handler
	Account *account.Service
	Admin   *admin.Service
	Routes  []httpx.Route
	// Agent 是节点接口（agentproto.Server），由 ctl-core 提供；尚未接入时为 nil。
	Agent agentproto.Server
	// AgentHub 唤醒长轮询的节点；与 Agent 一起由 ctl-core 提供。
	AgentHub *agentproto.Hub
	// Background 是需要随进程运行的后台循环（调度器等），由 ctl-core 提供。
	Background []func(ctx context.Context)
}

// Assemble 构造全部业务包并挂载路由。
func Assemble(d Deps) (*Platform, error) {
	if d.Pool == nil || d.Box == nil {
		return nil, fmt.Errorf("api: 缺少数据库或密钥")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	acfg := account.Config{Box: d.Box, Cookies: d.Cookies, Now: d.Now, Mail: d.Mail, Logger: d.Logger}
	if d.AccountLimits != nil {
		acfg.Limits = *d.AccountLimits
	}
	acct := account.New(d.Pool, acfg)

	core, err := wireCore(&d)
	if err != nil {
		return nil, err
	}

	dcfg := admin.Config{Box: d.Box, Cookies: d.Cookies, Account: acct, Hooks: core.adminHooks, Issuer: d.Issuer, Now: d.Now, Logger: d.Logger}
	if d.AdminLimits != nil {
		dcfg.Limits = *d.AdminLimits
	}
	adm := admin.New(d.Pool, dcfg)

	routes := append(acct.Routes(), adm.Routes()...)
	for _, dom := range core.domains {
		routes = append(routes, dom.Routes()...)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Pattern != routes[j].Pattern {
			return routes[i].Pattern < routes[j].Pattern
		}
		return routes[i].Method < routes[j].Method
	})
	h, err := NewRouter(Options{
		Users: acct, Admins: adm, Cookies: d.Cookies, Audit: d.Pool, ViewerWritable: admin.ViewerWritable,
		Health: func(ctx context.Context) error { return d.Pool.Ping(ctx) }, Logger: d.Logger,
	}, routes)
	if err != nil {
		return nil, err
	}
	return &Platform{Handler: h, Account: acct, Admin: adm, Routes: routes,
		Agent: core.agent, AgentHub: core.hub, Background: core.background}, nil
}
