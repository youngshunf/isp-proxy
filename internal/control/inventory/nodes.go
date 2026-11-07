package inventory

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
)

// Service 提供节点与上游资源的管理端接口。
type Service struct{ env *kernel.Env }

// New 创建服务。
func New(env *kernel.Env) *Service { return &Service{env: env} }

// NodeView 是 OpenAPI Node。
type NodeView struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	PublicHost      string         `json:"publicHost"`
	PortRange       PortRange      `json:"portRange"`
	Status          string         `json:"status"`
	Online          bool           `json:"online"`
	LastSeenAt      *time.Time     `json:"lastSeenAt"`
	LastReportAt    *time.Time     `json:"lastReportAt"`
	DesiredRevision int64          `json:"desiredRevision"`
	AppliedRevision int64          `json:"appliedRevision"`
	AgentVersion    *string        `json:"agentVersion"`
	PersistOK       *bool          `json:"persistOk"`
	ClockSkewMs     *int           `json:"clockSkewMs"`
	Capabilities    map[string]any `json:"capabilities"`
	Ports           PortCounts     `json:"ports"`
	Subscriptions   int            `json:"subscriptions"`
	ActiveConns     *int           `json:"activeConns"`
	CreatedAt       time.Time      `json:"createdAt"`
}

// PortRange 是端口范围。
type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// PortCounts 是端口池占用。
type PortCounts struct {
	Total     int `json:"total"`
	Free      int `json:"free"`
	Allocated int `json:"allocated"`
	Cooldown  int `json:"cooldown"`
}

const nodeSelect = `SELECT n.id, n.name, n.public_host, n.port_range_start, n.port_range_end, n.status, n.last_seen_at,
	n.last_report_at, n.desired_revision, n.applied_revision, n.agent_version, n.persist_ok, n.clock_skew_ms, n.capabilities,
	n.created_at,
	(SELECT count(*) FROM ports p WHERE p.node_id = n.id),
	(SELECT count(*) FROM ports p WHERE p.node_id = n.id AND p.state = 'free'),
	(SELECT count(*) FROM ports p WHERE p.node_id = n.id AND p.state = 'allocated'),
	(SELECT count(*) FROM ports p WHERE p.node_id = n.id AND p.state = 'cooldown'),
	(SELECT count(*) FROM subscriptions s WHERE s.node_id = n.id AND s.lifecycle IN ('active', 'retained')),
	(SELECT sum(s.active_conns) FROM subscriptions s WHERE s.node_id = n.id AND s.usage_reported_at >= $1)
	FROM nodes n`

func (s *Service) scanNode(row interface{ Scan(...any) error }, now time.Time) (*NodeView, error) {
	v := &NodeView{}
	var conns *int64
	if err := row.Scan(&v.ID, &v.Name, &v.PublicHost, &v.PortRange.Start, &v.PortRange.End, &v.Status, &v.LastSeenAt,
		&v.LastReportAt, &v.DesiredRevision, &v.AppliedRevision, &v.AgentVersion, &v.PersistOK, &v.ClockSkewMs, &v.Capabilities,
		&v.CreatedAt, &v.Ports.Total, &v.Ports.Free, &v.Ports.Allocated, &v.Ports.Cooldown, &v.Subscriptions, &conns); err != nil {
		return nil, err
	}
	if v.Capabilities == nil {
		v.Capabilities = map[string]any{}
	}
	v.Online = v.LastSeenAt != nil && now.Sub(*v.LastSeenAt) <= s.env.Cfg.FreshWindow
	if conns != nil {
		c := int(*conns)
		v.ActiveConns = &c
	}
	return v, nil
}

// GetNode 读节点视图。
func (s *Service) GetNode(ctx context.Context, q kernel.Q, id string) (*NodeView, error) {
	now := s.env.Now()
	v, err := s.scanNode(q.QueryRow(ctx, nodeSelect+` WHERE n.id = $2`, now.Add(-s.env.Cfg.FreshWindow), id), now)
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("没有找到这个节点")
	}
	return v, err
}

// ListNodes 列出全部节点。
func (s *Service) ListNodes(ctx context.Context) ([]NodeView, error) {
	now := s.env.Now()
	rows, err := s.env.Pool.Query(ctx, nodeSelect+` ORDER BY n.created_at, n.id`, now.Add(-s.env.Cfg.FreshWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeView{}
	for rows.Next() {
		v, err := s.scanNode(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// NodeInput 是 OpenAPI NodeInput。
type NodeInput struct {
	Name       string     `json:"name"`
	PublicHost string     `json:"publicHost"`
	PortRange  *PortRange `json:"portRange"`
}

// CreateNode 登记节点并预填端口池（docs/06 §1 默认 20000-29999）。
func (s *Service) CreateNode(ctx context.Context, adminID string, in NodeInput) (*NodeView, error) {
	in.Name, in.PublicHost = strings.TrimSpace(in.Name), strings.TrimSpace(in.PublicHost)
	if in.Name == "" || len(in.Name) > 64 || in.PublicHost == "" || len(in.PublicHost) > 253 {
		return nil, httpx.Invalid("节点名称与公共域名必填")
	}
	pr := PortRange{Start: 20000, End: 29999}
	if in.PortRange != nil {
		pr = *in.PortRange
	}
	if pr.Start < 1024 || pr.End > 65535 || pr.End < pr.Start+2 {
		return nil, httpx.Invalid("端口范围必须在 1024～65535 且至少 3 个端口")
	}
	var v *NodeView
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		id := store.NewID("nod")
		_, err := tx.Exec(ctx, `INSERT INTO nodes (id, name, public_host, port_range_start, port_range_end, created_at)
		                         VALUES ($1, $2, $3, $4, $5, $6)`, id, in.Name, in.PublicHost, pr.Start, pr.End, tx.Now())
		if store.IsUniqueViolation(err, "") {
			return httpx.Conflict("节点名称已存在")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ports (node_id, port) SELECT $1, g FROM generate_series($2::int, $3::int) g`,
			id, pr.Start, pr.End); err != nil {
			return err
		}
		if err := tx.Audit(ctx, adminID, "node.create", "node:"+id, map[string]any{"name": in.Name, "publicHost": in.PublicHost,
			"portRange": pr}); err != nil {
			return err
		}
		v, err = s.GetNode(ctx, tx, id)
		return err
	})
	return v, err
}

// NodePatch 是 OpenAPI NodePatch。
type NodePatch struct {
	Name       *string `json:"name"`
	PublicHost *string `json:"publicHost"`
	Status     *string `json:"status"`
}

// UpdateNode 修改节点。状态变化影响订阅启用（停用节点上的订阅不启用），提升修订号。
func (s *Service) UpdateNode(ctx context.Context, adminID, id string, p NodePatch) (*NodeView, error) {
	var v *NodeView
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM nodes WHERE id = $1 FOR UPDATE`, id).Scan(&exists); err != nil {
			if kernel.IsNoRows(err) {
				return httpx.NotFound("没有找到这个节点")
			}
			return err
		}
		if p.Name != nil {
			n := strings.TrimSpace(*p.Name)
			if n == "" || len(n) > 64 {
				return httpx.Invalid("节点名称为 1～64 个字符")
			}
			if _, err := tx.Exec(ctx, `UPDATE nodes SET name = $2 WHERE id = $1`, id, n); err != nil {
				if store.IsUniqueViolation(err, "") {
					return httpx.Conflict("节点名称已存在")
				}
				return err
			}
		}
		if p.PublicHost != nil {
			h := strings.TrimSpace(*p.PublicHost)
			if h == "" || len(h) > 253 {
				return httpx.Invalid("公共域名不合法")
			}
			if _, err := tx.Exec(ctx, `UPDATE nodes SET public_host = $2 WHERE id = $1`, id, h); err != nil {
				return err
			}
		}
		if p.Status != nil {
			switch *p.Status {
			case "active", "draining", "disabled":
			default:
				return httpx.Invalid("status 只能是 active、draining、disabled")
			}
			if _, err := tx.Exec(ctx, `UPDATE nodes SET status = $2 WHERE id = $1`, id, *p.Status); err != nil {
				return err
			}
			// 节点状态参与 enabled 计算：提升该节点全部订阅的版本。
			rows, err := tx.Query(ctx, `SELECT id FROM subscriptions WHERE node_id = $1 AND lifecycle IN ('pending', 'active', 'retained')`, id)
			if err != nil {
				return err
			}
			var subs []string
			for rows.Next() {
				var sid string
				if err := rows.Scan(&sid); err != nil {
					rows.Close()
					return err
				}
				subs = append(subs, sid)
			}
			rows.Close()
			for _, sid := range subs {
				if err := tx.BumpSub(ctx, sid); err != nil {
					return err
				}
			}
			if err := tx.BumpNode(ctx, id); err != nil {
				return err
			}
		}
		if err := tx.Audit(ctx, adminID, "node.update", "node:"+id, p); err != nil {
			return err
		}
		var err error
		v, err = s.GetNode(ctx, tx, id)
		return err
	})
	return v, err
}

// NodeRoutes 返回节点管理路由。
func (s *Service) NodeRoutes() []httpx.Route {
	return []httpx.Route{
		{Method: "GET", Pattern: "/admin-api/nodes", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ httpx.AdminPrincipal) (int, any, error) {
			v, err := s.ListNodes(r.Context())
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/nodes", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a httpx.AdminPrincipal) (int, any, error) {
			in, err := kernel.Body[NodeInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.CreateNode(r.Context(), a.AdminID, in)
			return 201, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/nodes/{nodeId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ httpx.AdminPrincipal) (int, any, error) {
			v, err := s.GetNode(r.Context(), s.env.Pool, r.PathValue("nodeId"))
			return 200, v, err
		})},
		{Method: "PATCH", Pattern: "/admin-api/nodes/{nodeId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a httpx.AdminPrincipal) (int, any, error) {
			p, err := kernel.Body[NodePatch](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.UpdateNode(r.Context(), a.AdminID, r.PathValue("nodeId"), p)
			return 200, v, err
		})},
	}
}
