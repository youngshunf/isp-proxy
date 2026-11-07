package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/admin"
	"github.com/go-gost/gost/internal/control/audit"
	"github.com/go-gost/gost/internal/control/core"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/mail"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/jackc/pgx/v5"
)

// coreParts 是 ctl-core（账本、订单、订阅、库存、任务、用量、节点同步、检测、告警、换出口申请、调度）
// 提供给平台的部分。
type coreParts struct {
	domains    []Domain
	adminHooks admin.Hooks
	agent      agentproto.Server
	hub        *agentproto.Hub
	background []func(ctx context.Context)
}

// wireCore 构造 ctl-core 的业务内核（internal/control/core）。这是接入 ctl-core 的唯一位置：
// 全部业务路由组、节点接口（agentproto.Server）、节点唤醒器、调度循环与 admin.Hooks（封禁/解封让该用户全部订阅
// 生成新期望版本，节点 ≤1 秒断开存量连接，R-13.8）。
func wireCore(d *Deps) (coreParts, error) {
	cfg := d.Core
	cfg.Box = d.Box
	c, err := core.New(d.Pool, cfg, nowClock(d.Now), mailer{box: d.Box, kick: d.Mail}, auditor{}, d.Logger)
	if err != nil {
		return coreParts{}, err
	}
	var domains []Domain
	for _, dom := range c.Domains() {
		domains = append(domains, dom)
	}
	return coreParts{
		domains: domains,
		adminHooks: admin.Hooks{UserStatusChanged: c.UserStatusChanged, UserSubscriptions: c.UserSubscriptions,
			UserRecentOrders: c.UserRecentOrders},
		agent:      c.AgentServer(),
		hub:        c.Hub(),
		background: []func(ctx context.Context){c.RunScheduler},
	}, nil
}

// RoutesFunc 把返回路由的函数适配为 Domain（一个包导出多组路由时使用）。
type RoutesFunc func() []httpx.Route

// Routes 实现 Domain。
func (f RoutesFunc) Routes() []httpx.Route { return f() }

// nowClock 把 Deps.Now 适配为 clock.Clock。
type nowClock func() time.Time

func (f nowClock) Now() time.Time { return f().UTC().Truncate(time.Microsecond) }

// mailer 把 kernel.Mailer 适配到发件箱（与业务同事务写入，提交后唤醒投递）。
type mailer struct {
	box  *secret.Box
	kick interface{ Kick() }
}

func (m mailer) Enqueue(ctx context.Context, tx pgx.Tx, to, template string, payload map[string]any) error {
	data := make(map[string]string, len(payload))
	for k, v := range payload {
		data[k] = fmt.Sprint(v)
	}
	if _, err := mail.Enqueue(ctx, tx, m.box, mail.Message{To: to, Template: template, Data: data}); err != nil {
		return err
	}
	if m.kick != nil {
		// 投递循环会在下一轮（≤2 秒）取到；事务未提交前取不到，提前唤醒无害。
		m.kick.Kick()
	}
	return nil
}

// auditor 把 kernel.Auditor 适配到 audit.Record。target 形如 "order:ord_…"；detail 任意可 JSON 编码的值。
type auditor struct{}

func (auditor) Record(ctx context.Context, tx pgx.Tx, adminID, action, target string, detail any) error {
	e := audit.Entry{Action: action, AdminID: adminID}
	if typ, id, ok := strings.Cut(target, ":"); ok {
		e.TargetType, e.TargetID = typ, id
	} else if target != "" {
		e.TargetType = target
	}
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("audit: 编码 detail: %w", err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			m = map[string]any{"value": json.RawMessage(b)}
		}
		e.Details = m
	}
	return audit.Record(ctx, tx, e)
}
