// Package kernel 是控制面业务包共用的内核：运行环境（连接池、时钟、配置、节点唤醒、审计、邮件）、
// 事务封装（提交后唤醒长轮询的节点）、期望状态版本提升、幂等键、分页游标、规则版本与订阅事件。
//
// 依赖方向：kernel ← ledger ← alert ← inventory ← probe ← task ← subscription ← order/exitreq ← agentsrv ← core。
// 任何影响节点期望状态的写入都必须在同一事务内调用 Tx.BumpSub / Tx.BumpNode（docs/06 §5）。
package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/clock"
	"github.com/go-gost/gost/internal/control/secret"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Q 是连接池与事务共有的查询接口；视图加载函数接受 Q，可在事务内或事务外调用。
type Q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Auditor 记录管理员操作审计（由 ctl-platform 实现，写 audit_logs）。target 形如 "order:ord_…"。
// 必须在业务事务内写入，与业务一起提交或回滚；detail 不得含明文凭据。
type Auditor interface {
	Record(ctx context.Context, tx pgx.Tx, adminID, action, target string, detail any) error
}

// Mailer 把通知邮件写入发件箱（mail_outbox），与业务同事务；可为 nil（不发通知）。
type Mailer interface {
	Enqueue(ctx context.Context, tx pgx.Tx, to, template string, payload map[string]any) error
}

// Config 是业务内核的可配置项。零值字段取默认值。
type Config struct {
	// Box 加密接入密码与上游密码（AES-256-GCM，密钥来自 ISP_SECRET_KEY，与平台共用）。必填。
	Box *secret.Box
	// Geo 按出口 IP 解析国家（探测结果不带国家，控制面自行解析后再做国家符合判定，A-12）。
	// 默认 StaticGeo{}（全部未知 → 国家不符，不放行切换）；开发与测试用静态映射，生产接 GeoIP 库。
	Geo GeoLookup
	// EchoURL 是探测使用的 IP 回显端点。
	EchoURL string
	// Brand 是对客户展示的品牌名（目录 Catalog.brand，前台标题与页头使用；与邮件署名同取 mail.brand）。默认“ISP 代理”。
	Brand string
	// ExitCheckInterval 是定时出口检测间隔；超过两倍未更新视为 stale。默认 10 分钟。
	ExitCheckInterval time.Duration
	// ExitCheckBudget 是每次调度最多新建的定时检测数。默认 20。
	ExitCheckBudget int
	// SlowLatency 超过即记为 slow。默认 800ms。
	SlowLatency time.Duration
	// TaskTimeout 是变更任务等待节点应用的上限，超时回滚。默认 2 分钟。
	TaskTimeout time.Duration
	// ProbeTimeout 是探测截止时间。默认 60 秒。
	ProbeTimeout time.Duration
	// StockLowThreshold 是库存档位 low 的上限（docs/06 §8.1，默认 3）。
	StockLowThreshold int
	// SharedExitDefaultCapacity 是录入上游资源时未填绑定上限的默认值（D-24，docs/06 §9.1，默认 3）。
	SharedExitDefaultCapacity int
	// NodeOfflineAfter 超过即视为节点离线并告警。默认 30 秒。
	NodeOfflineAfter time.Duration
	// FreshWindow 内有上报视为数据新鲜（docs/06 §8.2，15 秒）。
	FreshWindow time.Duration
	// ClockSkewLimit 节点时钟偏差超过即告警（R-14.7，1 秒）。
	ClockSkewLimit time.Duration
	// PortPoolLowRatio 空闲端口占比低于该值告警。默认 0.1。
	PortPoolLowRatio float64
	// SchedulerInterval 调度循环间隔（docs/06 §3，5 秒）。
	SchedulerInterval time.Duration
	// UsageRetention 趋势样本保留期（D-07，90 天）。
	UsageRetention time.Duration
}

// GeoLookup 按 IP 解析 ISO 3166-1 alpha-2 国家代码（大小写不限）。未知时 ok = false。
type GeoLookup interface {
	Country(ctx context.Context, ip string) (code string, ok bool)
}

// StaticGeo 是静态 IP → 国家映射（开发、测试与种子配置）。
type StaticGeo map[string]string

// Country 实现 GeoLookup。
func (m StaticGeo) Country(_ context.Context, ip string) (string, bool) {
	c, ok := m[ip]
	return c, ok
}

func (c Config) withDefaults() Config {
	if c.Geo == nil {
		c.Geo = StaticGeo{}
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.ExitCheckInterval, 10*time.Minute)
	def(&c.SlowLatency, 800*time.Millisecond)
	def(&c.TaskTimeout, 2*time.Minute)
	def(&c.ProbeTimeout, 60*time.Second)
	def(&c.NodeOfflineAfter, 30*time.Second)
	def(&c.FreshWindow, 15*time.Second)
	def(&c.ClockSkewLimit, time.Second)
	def(&c.SchedulerInterval, 5*time.Second)
	def(&c.UsageRetention, 90*24*time.Hour)
	if c.ExitCheckBudget <= 0 {
		c.ExitCheckBudget = 20
	}
	if c.StockLowThreshold <= 0 {
		c.StockLowThreshold = 3
	}
	if c.SharedExitDefaultCapacity <= 0 {
		c.SharedExitDefaultCapacity = 3
	}
	if c.PortPoolLowRatio <= 0 {
		c.PortPoolLowRatio = 0.1
	}
	if c.Brand == "" {
		c.Brand = "ISP 代理"
	}
	if c.EchoURL == "" {
		c.EchoURL = "http://127.0.0.1:8099/ip"
	}
	return c
}

// Env 是业务包共享的运行环境。
type Env struct {
	Pool  *pgxpool.Pool
	Clock clock.Clock
	Cfg   Config
	Hub   *agentproto.Hub
	Mail  Mailer
	Log   *slog.Logger

	auditor Auditor
	box     *secret.Box
}

// NewEnv 创建运行环境。cfg.Box 必填。
func NewEnv(pool *pgxpool.Pool, cfg Config, clk clock.Clock, mail Mailer, audit Auditor, log *slog.Logger) (*Env, error) {
	if pool == nil || cfg.Box == nil {
		return nil, fmt.Errorf("kernel: 缺少数据库或密钥")
	}
	if clk == nil {
		clk = clock.System{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Env{Pool: pool, Clock: clk, Cfg: cfg.withDefaults(), Hub: agentproto.NewHub(), Mail: mail, Log: log,
		auditor: audit, box: cfg.Box}, nil
}

// Now 返回注入时钟的当前时间（UTC，微秒精度）。
func (e *Env) Now() time.Time { return e.Clock.Now().UTC().Truncate(time.Microsecond) }

// Tx 是业务事务：记录本事务内需要唤醒的节点，提交成功后统一唤醒。
type Tx struct {
	pgx.Tx
	env    *Env
	now    time.Time
	nodes  map[string]bool
	bumped map[string]bool
	after  []func()
}

// Now 返回事务开始时取得的时间；同一事务内的业务时间一致。
func (t *Tx) Now() time.Time { return t.now }

// Env 返回运行环境。
func (t *Tx) Env() *Env { return t.env }

// AfterCommit 登记提交成功后执行的动作。
func (t *Tx) AfterCommit(fn func()) { t.after = append(t.after, fn) }

// BumpNode 提升节点期望状态修订号（每事务每节点一次即可），提交后唤醒该节点。
func (t *Tx) BumpNode(ctx context.Context, nodeID string) error {
	if nodeID == "" || t.bumped[nodeID] {
		return nil
	}
	if _, err := t.Exec(ctx, `UPDATE nodes SET desired_revision = desired_revision + 1 WHERE id = $1`, nodeID); err != nil {
		return fmt.Errorf("提升节点修订号: %w", err)
	}
	t.bumped[nodeID] = true
	t.nodes[nodeID] = true
	return nil
}

// BumpSub 提升订阅条目版本（接入、出口、额度、期限、启用任一变化），并提升其节点修订号。
func (t *Tx) BumpSub(ctx context.Context, subID string) error {
	var node *string
	if err := t.QueryRow(ctx, `UPDATE subscriptions SET desired_version = desired_version + 1, row_version = row_version + 1
	                             WHERE id = $1 RETURNING node_id`, subID).Scan(&node); err != nil {
		return fmt.Errorf("提升订阅版本: %w", err)
	}
	if node != nil {
		return t.BumpNode(ctx, *node)
	}
	return nil
}

// Audit 记录审计；未配置 Auditor 时跳过。
func (t *Tx) Audit(ctx context.Context, adminID, action, target string, detail any) error {
	if t.env.auditor == nil || adminID == "" {
		return nil
	}
	return t.env.auditor.Record(ctx, t.Tx, adminID, action, target, detail)
}

// InTx 在事务中执行 fn；序列化失败或死锁时重试（最多 4 次）。提交成功后唤醒受影响节点。
func (e *Env) InTx(ctx context.Context, fn func(*Tx) error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		var t *Tx
		err = store.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
			t = &Tx{Tx: tx, env: e, now: e.Now(), nodes: map[string]bool{}, bumped: map[string]bool{}}
			return fn(t)
		})
		if err == nil {
			for n := range t.nodes {
				e.Hub.Notify(n)
			}
			for _, f := range t.after {
				f()
			}
			return nil
		}
		code := store.PgCode(err)
		if code != store.CodeSerialization && code != "40P01" {
			return err
		}
	}
	return err
}

// ErrNoRows 是 pgx.ErrNoRows 的别名，便于调用方判断。
var ErrNoRows = pgx.ErrNoRows

// IsNoRows 报告 err 是否为“没有行”。
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
