// Package core 组装 ctl-core 的全部业务包（C-W1～C-W12），对平台只暴露一个构造函数：
//
//	c, err := core.New(pool, cfg, clock, mailer, auditor)
//	c.Domains()        // 各业务包的路由组（实现 Routes() []httpx.Route）
//	c.Routes()         // 全部路由（契约 98 个中除账号、会话、管理员登录、用户管理、审计查询外的 77 个）
//	c.AgentServer()    // agentproto.Server
//	c.Hub()            // 唤醒长轮询节点
//	c.RunScheduler(ctx) // 每 SchedulerInterval（默认 5 秒）执行一次 Tick，直到 ctx 结束
//	c.AdminHooks()     // 封禁/解封、用户详情的订阅与订单（admin.Hooks 的三个字段）
package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/agentsrv"
	"github.com/go-gost/gost/internal/control/alert"
	"github.com/go-gost/gost/internal/control/catalog"
	"github.com/go-gost/gost/internal/control/clock"
	"github.com/go-gost/gost/internal/control/exitreq"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/inventory"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/order"
	"github.com/go-gost/gost/internal/control/overview"
	"github.com/go-gost/gost/internal/control/probe"
	"github.com/go-gost/gost/internal/control/subscription"
	"github.com/go-gost/gost/internal/control/task"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Core 是组装好的业务内核。
type Core struct {
	Env           *kernel.Env
	Catalog       *catalog.Service
	Inventory     *inventory.Service
	Probes        *probe.Service
	Tasks         *task.Service
	Subscriptions *subscription.Service
	Orders        *order.Service
	ExitRequests  *exitreq.Service
	Alerts        *alert.Service
	Overview      *overview.Service
	Agent         *agentsrv.Server
}

// New 构造业务内核。mail、audit 可为 nil（不发通知、不记审计，仅用于测试）。
func New(pool *pgxpool.Pool, cfg kernel.Config, clk clock.Clock, mail kernel.Mailer, audit kernel.Auditor, log *slog.Logger) (*Core, error) {
	env, err := kernel.NewEnv(pool, cfg, clk, mail, audit, log)
	if err != nil {
		return nil, err
	}
	c := &Core{Env: env, Catalog: catalog.New(env), Inventory: inventory.New(env), Probes: probe.New(env), Alerts: alert.New(env)}
	c.Tasks = task.New(env, c.Probes)
	c.Subscriptions = subscription.New(env, c.Tasks)
	c.Orders = order.New(env, c.Tasks, c.Subscriptions)
	c.ExitRequests = exitreq.New(env)
	c.Overview = overview.New(env, c.Subscriptions)
	c.Agent = agentsrv.New(env, c.Tasks, c.Probes)
	return c, nil
}

type routesFunc func() []httpx.Route

func (f routesFunc) Routes() []httpx.Route { return f() }

// Domain 是导出路由的业务包。
type Domain interface{ Routes() []httpx.Route }

// Domains 返回全部路由组。
func (c *Core) Domains() []Domain {
	return []Domain{c.Catalog, routesFunc(c.Inventory.UpstreamRoutes), routesFunc(c.Inventory.NodeRoutes), c.Probes, c.Tasks,
		c.Alerts, c.Subscriptions, c.Orders, c.ExitRequests, c.Overview}
}

// Routes 返回全部路由。
func (c *Core) Routes() []httpx.Route {
	var out []httpx.Route
	for _, d := range c.Domains() {
		out = append(out, d.Routes()...)
	}
	return out
}

// AgentServer 返回节点接口实现。
func (c *Core) AgentServer() agentproto.Server { return c.Agent }

// Hub 返回节点唤醒器。
func (c *Core) Hub() *agentproto.Hub { return c.Env.Hub }

// UserStatusChanged 是 admin.Hooks.UserStatusChanged：封禁/解封同一事务内让该用户全部订阅生成新期望版本，提交后唤醒节点。
func (c *Core) UserStatusChanged(ctx context.Context, tx pgx.Tx, userID string) error {
	return c.Subscriptions.UserStatusChanged(ctx, tx, userID)
}

// UserSubscriptions 是 admin.Hooks.UserSubscriptions。
func (c *Core) UserSubscriptions(ctx context.Context, userID string) (any, error) {
	return c.Subscriptions.UserSubscriptions(ctx, userID)
}

// UserRecentOrders 是 admin.Hooks.UserRecentOrders。
func (c *Core) UserRecentOrders(ctx context.Context, userID string) (any, error) {
	return c.Orders.UserRecentOrders(ctx, userID)
}

// RunScheduler 每 SchedulerInterval 执行一次 Tick，直到 ctx 结束。单步失败只记日志，不影响其他步骤与下一轮。
func (c *Core) RunScheduler(ctx context.Context) {
	t := time.NewTicker(c.Env.Cfg.SchedulerInterval)
	defer t.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick 执行一轮调度（docs/06 §3）。各步骤可重入，多实例并发执行也安全（行锁 + SKIP LOCKED）。
func (c *Core) Tick(ctx context.Context) {
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"unpaid_hold_expiry", c.Orders.ExpireHolds},
		{"subscription_expiry", c.Subscriptions.ExpireDue},
		{"retention_release", c.Subscriptions.ReleaseDue},
		{"task_timeout", c.Tasks.FailTimedOut},
		{"probe_expiry", c.Probes.ExpireDue},
		{"exit_checks", c.Probes.ScheduleExitChecks},
		{"cooldowns", c.endCooldowns},
		{"health_alerts", c.healthAlerts},
		{"cleanup", c.cleanup},
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			return
		}
		if err := s.fn(ctx); err != nil && ctx.Err() == nil {
			c.Env.Log.Error("调度步骤失败", "step", s.name, "err", err)
		}
	}
	// 兜底：即使某次提交后的唤醒丢失（例如平台事务内的封禁），节点也会在一个调度周期内重新拉取。
	c.Env.Hub.NotifyAll()
}

func (c *Core) endCooldowns(ctx context.Context) error {
	now := c.Env.Now()
	if _, err := inventory.EndPortCooldowns(ctx, c.Env.Pool, now); err != nil {
		return err
	}
	_, err := inventory.EndResourceCooldowns(ctx, c.Env.Pool, now)
	return err
}

// healthAlerts 维护节点离线、端口池不足、库存不足告警（条件消失时自动关闭）。
func (c *Core) healthAlerts(ctx context.Context) error {
	return c.Env.InTx(ctx, func(tx *kernel.Tx) error {
		now := tx.Now()
		cfg := c.Env.Cfg
		rows, err := tx.Query(ctx, `SELECT n.id, n.name, n.last_seen_at,
		       (SELECT count(*) FROM ports p WHERE p.node_id = n.id AND p.state = 'free'),
		       (SELECT count(*) FROM ports p WHERE p.node_id = n.id)
		      FROM nodes n WHERE n.status <> 'disabled'`)
		if err != nil {
			return err
		}
		type nodeRow struct {
			id, name    string
			seen        *time.Time
			free, total int
		}
		var ns []nodeRow
		for rows.Next() {
			var n nodeRow
			if err := rows.Scan(&n.id, &n.name, &n.seen, &n.free, &n.total); err != nil {
				rows.Close()
				return err
			}
			ns = append(ns, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, n := range ns {
			id := n.id
			if n.seen != nil && now.Sub(*n.seen) > cfg.NodeOfflineAfter {
				if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: "node_offline", Severity: "critical", Key: "node_offline:" + id, Node: &id,
					Message: "节点 " + n.name + " 超过 " + cfg.NodeOfflineAfter.String() + " 未上报（已有连接按节点本地状态继续执行）"}); err != nil {
					return err
				}
			}
			if n.total > 0 && float64(n.free) < float64(n.total)*cfg.PortPoolLowRatio {
				if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: "port_pool_low", Severity: "warning", Key: "port_pool_low:" + id, Node: &id,
					Message: "节点 " + n.name + " 空闲端口不足", Details: map[string]any{"free": n.free, "total": n.total}}); err != nil {
					return err
				}
			} else if n.free >= 3 {
				if err := alert.Resolve(ctx, tx, now, "port_pool_low:"+id); err != nil {
					return err
				}
			}
		}
		// 在售套餐所在地域、按出口类型的库存低于阈值：独享数空闲资源，共享数剩余名额（D-24）。
		rows, err = tx.Query(ctx, `SELECT DISTINCT pv.country_code, pv.city, p.exit_mode
		      FROM plans p JOIN LATERAL (SELECT * FROM plan_versions v WHERE v.plan_id = p.id ORDER BY v.version DESC LIMIT 1) pv ON true
		     WHERE p.status = 'on_sale'`)
		if err != nil {
			return err
		}
		type region struct{ cc, city, mode string }
		var rs []region
		for rows.Next() {
			var r region
			if err := rows.Scan(&r.cc, &r.city, &r.mode); err != nil {
				rows.Close()
				return err
			}
			rs = append(rs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range rs {
			n, _, _, err := inventory.StockCounts(ctx, tx, r.mode, r.cc, r.city)
			if err != nil {
				return err
			}
			key, msg := "stock_low:"+r.cc+":"+r.city, "地域 "+r.cc+"/"+r.city+" 可售资源不足"
			if r.mode == inventory.ModeShared {
				key, msg = key+":shared", "地域 "+r.cc+"/"+r.city+" 共享出口剩余名额不足"
			}
			if n < cfg.StockLowThreshold {
				if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: "stock_low", Severity: "warning", Key: key,
					Message: msg, Details: map[string]any{"available": n, "exitMode": r.mode}}); err != nil {
					return err
				}
			} else if err := alert.Resolve(ctx, tx, now, key); err != nil {
				return err
			}
		}
		return nil
	})
}

// cleanup 清理过期幂等键与超过保留期的趋势样本（D-07）。
func (c *Core) cleanup(ctx context.Context) error {
	now := c.Env.Now()
	if _, err := c.Env.Pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < $1`, now); err != nil {
		return err
	}
	_, err := c.Env.Pool.Exec(ctx, `DELETE FROM usage_reports WHERE id IN (SELECT id FROM usage_reports WHERE received_at < $1 LIMIT 5000)`,
		now.Add(-c.Env.Cfg.UsageRetention))
	return err
}
