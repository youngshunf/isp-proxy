// Package agentsrv 是 agentproto.Server 的实现（C-W7、C-W9；docs/04 §7）：从数据库生成节点期望状态，摄入用量上报，
// 推进变更任务对账，接收探测结果。
//
// 期望状态（每个订阅条目）：
//   - 列入：承载于该节点、生命周期为待履约/有效/保留期、且有接入（待生效优先，否则当前）的订阅；已释放、已删除、
//     履约异常的订阅不列入，节点据此关闭其全部端口；
//   - enabled = 用户未封禁 ∧ 未被管理员暂停 ∧ 生命周期为有效 ∧ 有当前接入与出口 ∧ 节点未停用；时长与流量由节点
//     按 expires_at / entitled_total 自行即时执行（R-14.3），所以到期与耗尽不需要等控制面；
//   - used_floor = 已入库的累计已用（节点恢复后以此为下限，GREATEST 合并，A-22）；
//   - max_conns / bandwidth_bps 取订阅首购时的套餐版本；
//   - 接入、出口取“待生效优先，否则当前”：组合更换时新接入直接绑定新出口（R-11.6）。
//
// 用量摄入：累计值只增不减（GREATEST），已应用版本只增不减；节点回报的已应用版本高于期望版本（控制面从备份恢复）
// 时把期望版本抬到已应用 + 1，避免节点忽略新的期望状态（A-03、A-10）。
package agentsrv

import (
	"context"
	"fmt"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/alert"
	"github.com/go-gost/gost/internal/control/inventory"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/probe"
	"github.com/go-gost/gost/internal/control/task"
)

// Server 实现 agentproto.Server。
type Server struct {
	env    *kernel.Env
	tasks  *task.Service
	probes *probe.Service
}

// New 创建节点接口实现。
func New(env *kernel.Env, tasks *task.Service, probes *probe.Service) *Server {
	return &Server{env: env, tasks: tasks, probes: probes}
}

var _ agentproto.Server = (*Server)(nil)

const desiredSQL = `SELECT s.id, s.desired_version, s.entitled_bytes_total, s.expires_at, s.used_bytes_total, pv.concurrency, pv.bandwidth_mbps,
	(u.status = 'active' AND NOT s.admin_paused AND s.lifecycle = 'active' AND s.current_access_id IS NOT NULL
	 AND s.current_exit_id IS NOT NULL AND n.status <> 'disabled') AS enabled,
	a.version, a.username, a.password_enc, a.http_port, a.https_port, a.socks5_port,
	b.version, r.protocol, r.host, r.port, r.username, r.password_enc
	FROM subscriptions s
	JOIN users u ON u.id = s.user_id
	JOIN nodes n ON n.id = s.node_id
	JOIN plan_versions pv ON pv.id = s.plan_version_id
	JOIN LATERAL (SELECT * FROM access_versions x WHERE x.subscription_id = s.id
	                AND (x.state = 'pending' OR x.id = s.current_access_id) ORDER BY (x.state = 'pending') DESC LIMIT 1) a ON true
	LEFT JOIN LATERAL (SELECT * FROM exit_bindings x WHERE x.subscription_id = s.id
	                AND (x.state = 'pending' OR x.id = s.current_exit_id) ORDER BY (x.state = 'pending') DESC LIMIT 1) b ON true
	LEFT JOIN upstream_resources r ON r.id = b.resource_id
	WHERE s.node_id = $1 AND s.lifecycle IN ('pending', 'active', 'retained')
	ORDER BY s.id`

// Desired 生成节点的期望状态。
func (s *Server) Desired(ctx context.Context, nodeID string) (*agentproto.Desired, error) {
	tx, err := s.env.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// 可重复读快照：修订号与条目来自同一时刻。
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return nil, err
	}
	now := s.env.Now()
	d := &agentproto.Desired{NodeID: nodeID, GeneratedAt: now, Subscriptions: []agentproto.SubscriptionEntry{}}
	if err := tx.QueryRow(ctx, `SELECT desired_revision FROM nodes WHERE id = $1`, nodeID).Scan(&d.Revision); err != nil {
		if kernel.IsNoRows(err) {
			return nil, agentproto.ErrUnknownNode
		}
		return nil, err
	}
	rows, err := tx.Query(ctx, desiredSQL, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e agentproto.SubscriptionEntry
		var version, entitled, used int64
		var expires *time.Time
		var bw int64
		var accV int64
		var pwEnc []byte
		a := &agentproto.Access{}
		var exV *int64
		var proto, host, user *string
		var port *int
		var exEnc []byte
		if err := rows.Scan(&e.ID, &version, &entitled, &expires, &used, &e.MaxConns, &bw, &e.Enabled, &accV, &a.Username, &pwEnc,
			&a.Ports.HTTP, &a.Ports.HTTPS, &a.Ports.SOCKS5, &exV, &proto, &host, &port, &user, &exEnc); err != nil {
			return nil, err
		}
		if version <= 0 {
			continue
		}
		e.Version, e.EntitledTotal, e.UsedFloor = uint64(version), uint64(entitled), uint64(used)
		e.BandwidthBps = uint64(bw) * 1_000_000
		if expires != nil {
			e.ExpiresAt = expires.UTC()
		} else {
			e.Enabled = false
		}
		a.Version = uint64(accV)
		if a.Password, err = s.env.Open(pwEnc); err != nil {
			return nil, fmt.Errorf("解密接入密码 %s: %w", e.ID, err)
		}
		e.Access = a
		if exV != nil && proto != nil {
			x, err := inventory.ExitOf(s.env, *proto, *host, *port, kernel.StrOr(user, ""), exEnc, uint64(*exV))
			if err != nil {
				return nil, fmt.Errorf("解密上游密码 %s: %w", e.ID, err)
			}
			e.Exit = &x
		} else {
			e.Enabled = false
		}
		d.Subscriptions = append(d.Subscriptions, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if d.Probes, err = s.probes.Desired(ctx, tx, nodeID, now); err != nil {
		return nil, err
	}
	if err := d.Validate(); err != nil {
		// 不应发生（端口池与库约束保证）；记录并拒绝下发，节点保持上一份期望状态。
		s.env.Log.Error("agentsrv: 期望状态校验失败", "node", nodeID, "err", err)
		return nil, err
	}
	return d, nil
}

// Report 摄入节点上报。
func (s *Server) Report(ctx context.Context, nodeID string, r *agentproto.Report) (*agentproto.ReportAck, error) {
	ack := &agentproto.ReportAck{}
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		now := tx.Now()
		var desiredRev int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision FROM nodes WHERE id = $1 FOR UPDATE`, nodeID).Scan(&desiredRev); err != nil {
			if kernel.IsNoRows(err) {
				return agentproto.ErrUnknownNode
			}
			return err
		}
		nodeTime := r.NodeTime.UTC()
		if nodeTime.IsZero() {
			nodeTime = now
		}
		skewMs := nodeTime.Sub(now).Milliseconds()
		skewed := false
		if r.ClockSkew != nil {
			skewMs = (r.ClockSkew.LowMs + r.ClockSkew.HighMs) / 2
			skewed = r.ClockSkew.Exceeded
		} else {
			// 没有往返测量时用 node_time 与收到时刻粗估：多给 1 秒余量吸收网络时延，避免误报。
			d := nodeTime.Sub(now)
			skewed = d > s.env.Cfg.ClockSkewLimit+time.Second || d < -(s.env.Cfg.ClockSkewLimit+time.Second)
		}
		applied := int64(r.AppliedRevision)
		if _, err := tx.Exec(ctx, `UPDATE nodes SET applied_revision = GREATEST(applied_revision, $2), agent_version = NULLIF($3, ''),
		                            persist_ok = $4, clock_skew_ms = $5, last_seen_at = $6, last_report_at = $6,
		                            desired_revision = CASE WHEN $2 > desired_revision THEN $2 + 1 ELSE desired_revision END
		                           WHERE id = $1`, nodeID, applied, r.AgentVersion, r.PersistOK, skewMs, now); err != nil {
			return err
		}
		if applied > desiredRev {
			tx.AfterCommit(func() { s.env.Hub.Notify(nodeID) })
		}
		var reportID int64
		if err := tx.QueryRow(ctx, `INSERT INTO usage_reports (node_id, received_at, node_time, applied_revision, persist_ok, subscriptions)
		                             VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, nodeID, now, nodeTime, applied, r.PersistOK,
			len(r.Subscriptions)).Scan(&reportID); err != nil {
			return err
		}
		for i := range r.Subscriptions {
			if err := s.ingest(ctx, tx, nodeID, reportID, nodeTime, &r.Subscriptions[i]); err != nil {
				return err
			}
		}
		if err := s.nodeAlerts(ctx, tx, nodeID, r.PersistOK, skewed, skewMs); err != nil {
			return err
		}
		var rev int64
		if err := tx.QueryRow(ctx, `SELECT desired_revision FROM nodes WHERE id = $1`, nodeID).Scan(&rev); err != nil {
			return err
		}
		ack.Revision, ack.ServerTime = uint64(rev), now
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ack, nil
}

func (s *Server) ingest(ctx context.Context, tx *kernel.Tx, nodeID string, reportID int64, at time.Time, sr *agentproto.SubscriptionReport) error {
	var lifecycle string
	var entitled, usedBefore, desired int64
	err := tx.QueryRow(ctx, `SELECT lifecycle, entitled_bytes_total, used_bytes_total, desired_version FROM subscriptions
	                          WHERE id = $1 AND node_id = $2 FOR UPDATE`, sr.ID, nodeID).Scan(&lifecycle, &entitled, &usedBefore, &desired)
	if kernel.IsNoRows(err) {
		return nil // 不属于该节点（或已迁走）：忽略，不能让节点改写其他订阅
	}
	if err != nil {
		return err
	}
	used, applied := int64(sr.UsedTotal), int64(sr.AppliedVersion)
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET used_bytes_total = GREATEST(used_bytes_total, $2),
	                            applied_version = GREATEST(applied_version, $3), active_conns = $4, usage_reported_at = $5,
	                            desired_version = CASE WHEN $3 > desired_version THEN $3 + 1 ELSE desired_version END
	                           WHERE id = $1`, sr.ID, used, applied, max(0, sr.Conns), tx.Now()); err != nil {
		return err
	}
	if applied > desired {
		if err := tx.BumpNode(ctx, nodeID); err != nil {
			return err
		}
	}
	var byProto any
	if sr.UsedByProtocol != nil {
		byProto = sr.UsedByProtocol
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_samples (report_id, subscription_id, sampled_at, used_total, used_by_protocol, active_conns, applied_version)
	                            VALUES ($1, $2, $3, $4, $5, $6, $7)`, reportID, sr.ID, at, used, byProto, max(0, sr.Conns), applied); err != nil {
		return err
	}
	if usedBefore < entitled && used >= entitled && entitled > 0 && (lifecycle == "active" || lifecycle == "retained") {
		if err := tx.AddEvent(ctx, kernel.Event{Sub: sr.ID, Kind: "exhausted", ActorKind: "node", ActorID: &nodeID,
			Detail: "流量已用完（" + kernel.FormatBytes(entitled) + "），已停止服务；购买流量包后恢复"}); err != nil {
			return err
		}
	}
	if err := s.tasks.OnReport(ctx, tx, sr.ID, sr.AppliedVersion, sr.ApplyError); err != nil {
		return err
	}
	// 与任务无关的下发失败（例如普通版本变化时端口被占用）也要告警；应用成功后关闭。
	if ae := sr.ApplyError; ae != nil && int64(ae.Version) > applied {
		kind, sev := "config_failed", "warning"
		switch ae.Code {
		case agentproto.ApplyCertUnavailable:
			kind = "cert_risk"
		case agentproto.ApplyExitInvalid:
			kind = "upstream_error"
		case agentproto.ApplyPortInUse:
			sev = "critical"
		}
		return alert.Raise(ctx, tx, tx.Now(), alert.Alert{Kind: kind, Severity: sev, Key: kind + ":" + sr.ID, Node: &nodeID, Sub: &sr.ID,
			Message: "节点应用订阅配置失败（" + ae.Code + "）：" + ae.Message, Details: map[string]any{"code": ae.Code, "version": ae.Version}})
	}
	var cur int64
	if err := tx.QueryRow(ctx, `SELECT desired_version FROM subscriptions WHERE id = $1`, sr.ID).Scan(&cur); err != nil {
		return err
	}
	if applied >= cur {
		for _, k := range []string{"config_failed:", "cert_risk:", "upstream_error:"} {
			if err := alert.Resolve(ctx, tx, tx.Now(), k+sr.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) nodeAlerts(ctx context.Context, tx *kernel.Tx, nodeID string, persistOK, skewed bool, skewMs int64) error {
	now := tx.Now()
	if err := alert.Resolve(ctx, tx, now, "node_offline:"+nodeID); err != nil {
		return err
	}
	if !persistOK {
		if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: "persist_failed", Severity: "critical", Key: "persist_failed:" + nodeID, Node: &nodeID,
			Message: "节点本地计量持久化失败：重启可能丢失最近的用量，需尽快处理"}); err != nil {
			return err
		}
	} else if err := alert.Resolve(ctx, tx, now, "persist_failed:"+nodeID); err != nil {
		return err
	}
	if skewed {
		return alert.Raise(ctx, tx, now, alert.Alert{Kind: "clock_skew", Severity: "warning", Key: "clock_skew:" + nodeID, Node: &nodeID,
			Message: fmt.Sprintf("节点时钟偏差约 %d 毫秒，超过 1 秒（到期执行依赖节点时钟）", skewMs), Details: map[string]any{"skewMs": skewMs}})
	}
	return alert.Resolve(ctx, tx, now, "clock_skew:"+nodeID)
}

// ProbeResults 接收探测结果（重复提交幂等）。
func (s *Server) ProbeResults(ctx context.Context, nodeID string, rs []agentproto.ProbeResult) error {
	return s.env.InTx(ctx, func(tx *kernel.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT true FROM nodes WHERE id = $1`, nodeID).Scan(&ok); err != nil {
			if kernel.IsNoRows(err) {
				return agentproto.ErrUnknownNode
			}
			return err
		}
		for _, r := range rs {
			if err := s.probes.ApplyResult(ctx, tx, nodeID, r); err != nil {
				return err
			}
		}
		return nil
	})
}
