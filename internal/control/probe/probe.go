// Package probe 是出口检测与候选比较（C-W9，R-05、R-11.2、R-11.3；docs/06 §8.3）。
//
// 需要从节点发起的检测作为期望状态的 probes 下发，agent 只报告观测到的出口 IP、国家、耗时与错误；
// “与当前不同、平台内独占、国家符合”的比较由控制面在收到结果时计算并落库。探测直接经上游访问回显端点，
// 不经过客户接入，因此不计入任何订阅的用量（R-05.5、A-15）。结果按探测 ID 幂等。
package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/alert"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/inventory"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
)

// Service 提供探测的创建、结果处理与查询。
type Service struct{ env *kernel.Env }

// New 创建服务。
func New(env *kernel.Env) *Service { return &Service{env: env} }

func (s *Service) insert(ctx context.Context, tx *kernel.Tx, node, kind string, sub, res, binding, admin *string) (string, error) {
	id := store.NewID("prb")
	by := "system"
	if admin != nil {
		by = "admin"
	}
	_, err := tx.Exec(ctx, `INSERT INTO probes (id, node_id, kind, subscription_id, resource_id, exit_binding_id, requested_by_kind,
	                          requested_by_admin, deadline_at, created_at)
	                        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, node, kind, sub, res, binding, by, admin, tx.Now().Add(s.env.Cfg.ProbeTimeout), tx.Now())
	if err != nil {
		return "", err
	}
	return id, tx.BumpNode(ctx, node)
}

// CreateExitCheck 下发经订阅当前出口的检测（开通、换出口后立即检测；手动检测；定时检测）。
func (s *Service) CreateExitCheck(ctx context.Context, tx *kernel.Tx, sub string, admin *string) (string, error) {
	var node, binding *string
	err := tx.QueryRow(ctx, `SELECT s.node_id, s.current_exit_id FROM subscriptions s WHERE s.id = $1 AND s.lifecycle <> 'deleted'`,
		sub).Scan(&node, &binding)
	if kernel.IsNoRows(err) {
		return "", httpx.NotFound("没有找到这个订阅")
	}
	if err != nil {
		return "", err
	}
	if node == nil || binding == nil {
		return "", httpx.Precondition("该订阅当前没有出口，无法检测")
	}
	var res string
	if err := tx.QueryRow(ctx, `SELECT resource_id FROM exit_bindings WHERE id = $1`, *binding).Scan(&res); err != nil {
		return "", err
	}
	return s.insert(ctx, tx, *node, agentproto.ProbeExitCheck, &sub, &res, binding, admin)
}

// CreateBindingCheck 下发经指定绑定（尚未生效的新出口）的出口检测：换出口任务在节点应用后据此判定成败（R-11.6）。
func (s *Service) CreateBindingCheck(ctx context.Context, tx *kernel.Tx, sub, binding string) (string, error) {
	var node, res string
	if err := tx.QueryRow(ctx, `SELECT s.node_id, b.resource_id FROM exit_bindings b JOIN subscriptions s ON s.id = b.subscription_id
	                             WHERE b.id = $1 AND s.id = $2 AND s.node_id IS NOT NULL`, binding, sub).Scan(&node, &res); err != nil {
		return "", err
	}
	return s.insert(ctx, tx, node, agentproto.ProbeExitCheck, &sub, &res, &binding, nil)
}

// CreateCandidateCompare 下发候选资源验证并与当前出口比较（换出口第二、三步）。
func (s *Service) CreateCandidateCompare(ctx context.Context, tx *kernel.Tx, sub, res string, admin *string) (string, error) {
	var node *string
	var lifecycle string
	var cur *string
	err := tx.QueryRow(ctx, `SELECT node_id, lifecycle, current_exit_id FROM subscriptions WHERE id = $1`, sub).Scan(&node, &lifecycle, &cur)
	if kernel.IsNoRows(err) || lifecycle == "deleted" {
		return "", httpx.NotFound("没有找到这个订阅")
	}
	if err != nil {
		return "", err
	}
	if (lifecycle != "active" && lifecycle != "retained") || cur == nil || node == nil {
		return "", httpx.Precondition("只有有效或保留期内、已有出口的订阅可以换出口")
	}
	var resNode, status string
	err = tx.QueryRow(ctx, `SELECT node_id, status FROM upstream_resources WHERE id = $1 FOR UPDATE`, res).Scan(&resNode, &status)
	if kernel.IsNoRows(err) {
		return "", httpx.NotFound("没有找到这个上游资源")
	}
	if err != nil {
		return "", err
	}
	if status != "available" && status != "pending_verify" {
		return "", httpx.Conflict("候选资源必须是可售或待验证状态（当前 " + status + "）")
	}
	if resNode != *node {
		return "", httpx.Precondition("候选资源不在该订阅所在的节点上")
	}
	return s.insert(ctx, tx, *node, agentproto.ProbeCandidateCompare, &sub, &res, nil, admin)
}

// CreateUpstreamVerify 下发上游验证（录入后验证认证、连通性与实际出口，R-03.2）。
func (s *Service) CreateUpstreamVerify(ctx context.Context, tx *kernel.Tx, res string, admin *string) (string, error) {
	var node, status, nodeStatus string
	err := tx.QueryRow(ctx, `SELECT r.node_id, r.status, n.status FROM upstream_resources r JOIN nodes n ON n.id = r.node_id
	                          WHERE r.id = $1`, res).Scan(&node, &status, &nodeStatus)
	if kernel.IsNoRows(err) {
		return "", httpx.NotFound("没有找到这个上游资源")
	}
	if err != nil {
		return "", err
	}
	if status == "retired" {
		return "", httpx.Precondition("已停用的资源不能验证")
	}
	if nodeStatus == "disabled" {
		return "", httpx.Precondition("资源所属节点已停用")
	}
	return s.insert(ctx, tx, node, agentproto.ProbeUpstreamVerify, nil, &res, nil, admin)
}

var ccRe = regexp.MustCompile(`^[a-z]{2}$`)

// ApplyResult 记录一条探测结果并按种类处理。已完成或不属于该节点的探测直接忽略（幂等）。
func (s *Service) ApplyResult(ctx context.Context, tx *kernel.Tx, node string, r agentproto.ProbeResult) error {
	result := "failed"
	var ip, cc *string
	errText := strings.TrimSpace(r.Error)
	if r.OK {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.ObservedIP)); err == nil {
			v := a.Unmap().String()
			ip = &v
			result = "ok"
			if time.Duration(r.LatencyMs)*time.Millisecond > s.env.Cfg.SlowLatency {
				result = "slow"
			}
		} else if errText == "" {
			errText = "回显端点返回的 IP 无法解析"
		}
	}
	// 节点探测只带观测 IP 与延迟，不带国家（M3）：国家由控制面按 IP 解析（可插拔，见 kernel.GeoLookup）。
	// 节点若附带了国家也不采信，避免以节点侧数据做独占/国家判定。
	if ip != nil {
		if c, ok := s.env.Cfg.Geo.Country(ctx, *ip); ok {
			if c = strings.ToLower(strings.TrimSpace(c)); ccRe.MatchString(c) {
				cc = &c
			}
		}
	}
	if errText == "" && result == "failed" {
		errText = "探测失败"
	}
	var errp *string
	if errText != "" {
		errp = &errText
	}
	latency := int(r.LatencyMs)
	if latency < 0 {
		latency = 0
	}
	var nodeTime *time.Time
	if !r.CompletedAt.IsZero() {
		t := r.CompletedAt.UTC()
		nodeTime = &t
	}
	var kind string
	var sub, res, binding *string
	err := tx.QueryRow(ctx, `UPDATE probes SET status = 'done', result = $3, observed_ip = $4::inet, observed_country_code = $5,
	                           latency_ms = $6, error = $7, completed_at = $8, node_time = $9
	                         WHERE id = $1 AND node_id = $2 AND status IN ('queued', 'dispatched')
	                         RETURNING kind, subscription_id, resource_id, exit_binding_id`,
		r.ProbeID, node, result, ip, cc, latency, errp, tx.Now(), nodeTime).Scan(&kind, &sub, &res, &binding)
	if kernel.IsNoRows(err) {
		return nil // 已完成、已过期或不属于该节点
	}
	if err != nil {
		return err
	}
	if err := tx.BumpNode(ctx, node); err != nil {
		return err
	}
	now := tx.Now()
	switch kind {
	case agentproto.ProbeExitCheck:
		var verified *string
		var resID string
		if err := tx.QueryRow(ctx, `UPDATE exit_bindings SET check_result = $2, check_ip = $3::inet, check_country_code = $4,
		                              latency_ms = $5, check_error = $6, checked_at = $7
		                            WHERE id = $1 RETURNING host(verified_ip), resource_id`,
			*binding, result, ip, cc, latency, errp, now).Scan(&verified, &resID); err != nil {
			return err
		}
		key := "upstream_error:" + resID
		if result == "failed" {
			if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: "upstream_error", Severity: "warning", Key: key,
				Message: "出口检测失败：" + errText, Node: &node, Sub: sub, Resource: &resID}); err != nil {
				return err
			}
		} else {
			if err := alert.Resolve(ctx, tx, now, key); err != nil {
				return err
			}
		}
		ipKey := "exit_ip_changed:" + *binding
		if ip != nil && verified != nil && *ip != *verified {
			if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: "upstream_error", Severity: "warning", Key: ipKey,
				Message: fmt.Sprintf("出口 IP 与切换前验证结果不同（%s → %s），请核对平台内独占", *verified, *ip),
				Details: map[string]any{"verifiedIp": *verified, "checkIp": *ip},
				Node:    &node, Sub: sub, Resource: &resID}); err != nil {
				return err
			}
		} else if ip != nil {
			if err := alert.Resolve(ctx, tx, now, ipKey); err != nil {
				return err
			}
		}
	case agentproto.ProbeCandidateCompare:
		if err := s.updateResourceVerify(ctx, tx, *res, result, ip, cc, errp); err != nil {
			return err
		}
		var differs, unique, country bool
		if ip != nil {
			if err := tx.QueryRow(ctx, `SELECT
			      NOT EXISTS (SELECT 1 FROM exit_bindings b WHERE b.id = s.current_exit_id
			                   AND (b.verified_ip = $2::inet OR b.check_ip = $2::inet)),
			      NOT EXISTS (SELECT 1 FROM exit_bindings b WHERE b.subscription_id <> s.id AND b.state IN ('pending', 'active')
			                   AND (b.verified_ip = $2::inet OR b.check_ip = $2::inet)),
			      COALESCE(v.country_code = $3, false)
			    FROM subscriptions s JOIN plan_versions v ON v.id = s.plan_version_id WHERE s.id = $1`,
				*sub, *ip, cc).Scan(&differs, &unique, &country); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE probes SET differs_from_current = $2, unique_on_platform = $3, country_matches = $4 WHERE id = $1`,
			r.ProbeID, differs, unique, country); err != nil {
			return err
		}
	case agentproto.ProbeUpstreamVerify:
		if err := s.updateResourceVerify(ctx, tx, *res, result, ip, cc, errp); err != nil {
			return err
		}
		var declared, status string
		if err := tx.QueryRow(ctx, `SELECT country_code, status FROM upstream_resources WHERE id = $1 FOR UPDATE`, *res).Scan(&declared, &status); err != nil {
			return err
		}
		key := "upstream_verify:" + *res
		switch {
		case result != "failed" && cc != nil && *cc == declared:
			if status == "pending_verify" {
				if _, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'available', status_reason = NULL,
				                            row_version = row_version + 1 WHERE id = $1`, *res); err != nil {
					return err
				}
			}
			if err := alert.Resolve(ctx, tx, now, key); err != nil {
				return err
			}
		case result != "failed":
			msg := "检测到的国家与供应商声明不符，资源保持待验证"
			if _, err := tx.Exec(ctx, `UPDATE upstream_resources SET last_verify_ok = false, last_error = $2 WHERE id = $1`, *res, msg); err != nil {
				return err
			}
		default:
			if status == "available" {
				if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: "upstream_error", Severity: "warning", Key: key,
					Message: "可售资源验证失败：" + errText, Node: &node, Resource: res}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) updateResourceVerify(ctx context.Context, tx *kernel.Tx, res, result string, ip, cc, errp *string) error {
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET last_verified_at = $2, last_verify_ok = $3, last_exit_ip = $4::inet,
	                          last_exit_country_code = $5, last_error = $6, row_version = row_version + 1 WHERE id = $1`,
		res, tx.Now(), result != "failed", ip, cc, errp)
	return err
}

// Desired 返回节点待执行的探测（期望状态的 probes 列表）。
func (s *Service) Desired(ctx context.Context, q kernel.Q, node string, now time.Time) ([]agentproto.Probe, error) {
	rows, err := q.Query(ctx, `SELECT p.id, p.kind, p.subscription_id, p.deadline_at, r.protocol, r.host, r.port, r.username, r.password_enc
	                             FROM probes p JOIN upstream_resources r ON r.id = p.resource_id
	                            WHERE p.node_id = $1 AND p.status IN ('queued', 'dispatched') AND p.deadline_at > $2
	                            ORDER BY p.created_at, p.id`, node, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []agentproto.Probe{}
	for rows.Next() {
		var p agentproto.Probe
		var sub *string
		var protocol, host, user string
		var port int
		var enc []byte
		if err := rows.Scan(&p.ID, &p.Kind, &sub, &p.Deadline, &protocol, &host, &port, &user, &enc); err != nil {
			return nil, err
		}
		p.SubscriptionID = kernel.StrOr(sub, "")
		p.Exit, err = inventory.ExitOf(s.env, protocol, host, port, user, enc, 0)
		if err != nil {
			return nil, err
		}
		p.EchoURL = s.env.Cfg.EchoURL
		out = append(out, p)
	}
	return out, rows.Err()
}

// ExpireDue 把超过截止时间仍未回传的探测标为 expired，并提升受影响节点的修订号（调度，可重入）。
func (s *Service) ExpireDue(ctx context.Context) error {
	return s.env.InTx(ctx, func(tx *kernel.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE probes SET status = 'expired' WHERE status IN ('queued', 'dispatched') AND deadline_at <= $1
		                             RETURNING node_id`, tx.Now())
		if err != nil {
			return err
		}
		nodes := map[string]bool{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			nodes[n] = true
		}
		rows.Close()
		for n := range nodes {
			if err := tx.BumpNode(ctx, n); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// ScheduleExitChecks 为检测结果过期的有效订阅下发定时检测（有预算，R-05.3）。
func (s *Service) ScheduleExitChecks(ctx context.Context) error {
	return s.env.InTx(ctx, func(tx *kernel.Tx) error {
		rows, err := tx.Query(ctx, `SELECT s.id FROM subscriptions s
		      JOIN exit_bindings b ON b.id = s.current_exit_id AND b.state = 'active'
		      JOIN nodes n ON n.id = s.node_id AND n.status = 'active'
		     WHERE s.lifecycle = 'active' AND (b.checked_at IS NULL OR b.checked_at < $1)
		       AND NOT EXISTS (SELECT 1 FROM probes p WHERE p.exit_binding_id = b.id AND p.status IN ('queued', 'dispatched'))
		     ORDER BY b.checked_at NULLS FIRST LIMIT $2`, tx.Now().Add(-s.env.Cfg.ExitCheckInterval), s.env.Cfg.ExitCheckBudget)
		if err != nil {
			return err
		}
		var subs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			subs = append(subs, id)
		}
		rows.Close()
		for _, id := range subs {
			if _, err := s.CreateExitCheck(ctx, tx, id, nil); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// View 是 OpenAPI Probe。
type View struct {
	ID                  string      `json:"id"`
	Kind                string      `json:"kind"`
	NodeID              string      `json:"nodeId"`
	SubscriptionID      *string     `json:"subscriptionId"`
	ResourceID          *string     `json:"resourceId"`
	Status              string      `json:"status"`
	Result              *string     `json:"result"`
	ObservedIP          *string     `json:"observedIp"`
	ObservedCountryCode *string     `json:"observedCountryCode"`
	ObservedCity        *string     `json:"observedCity"`
	LatencyMs           *int        `json:"latencyMs"`
	Error               *string     `json:"error"`
	Comparison          *Comparison `json:"comparison"`
	CreatedAt           time.Time   `json:"createdAt"`
	CompletedAt         *time.Time  `json:"completedAt"`
	DeadlineAt          time.Time   `json:"deadlineAt"`
}

// Comparison 是 OpenAPI ExitComparison。
type Comparison struct {
	CurrentIP          *string `json:"currentIp"`
	DiffersFromCurrent bool    `json:"differsFromCurrent"`
	UniqueOnPlatform   bool    `json:"uniqueOnPlatform"`
	CountryMatches     bool    `json:"countryMatches"`
	Passed             bool    `json:"passed"`
}

// Get 读探测视图。
func Get(ctx context.Context, q kernel.Q, id string) (*View, error) {
	v := &View{}
	var differs, unique, country *bool
	var curIP *string
	err := q.QueryRow(ctx, `SELECT p.id, p.kind, p.node_id, p.subscription_id, p.resource_id, p.status, p.result, host(p.observed_ip),
	                          p.observed_country_code, p.observed_city, p.latency_ms, p.error, p.differs_from_current,
	                          p.unique_on_platform, p.country_matches, p.created_at, p.completed_at, p.deadline_at,
	                          (SELECT COALESCE(host(b.check_ip), host(b.verified_ip)) FROM subscriptions s
	                             JOIN exit_bindings b ON b.id = s.current_exit_id WHERE s.id = p.subscription_id)
	                        FROM probes p WHERE p.id = $1`, id).Scan(&v.ID, &v.Kind, &v.NodeID, &v.SubscriptionID, &v.ResourceID,
		&v.Status, &v.Result, &v.ObservedIP, &v.ObservedCountryCode, &v.ObservedCity, &v.LatencyMs, &v.Error, &differs, &unique,
		&country, &v.CreatedAt, &v.CompletedAt, &v.DeadlineAt, &curIP)
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("没有找到这个探测")
	}
	if err != nil {
		return nil, err
	}
	if v.Kind == agentproto.ProbeCandidateCompare && v.Status == "done" && differs != nil {
		c := &Comparison{CurrentIP: curIP, DiffersFromCurrent: *differs, UniqueOnPlatform: *unique, CountryMatches: *country}
		c.Passed = c.DiffersFromCurrent && c.UniqueOnPlatform && c.CountryMatches && v.Result != nil && *v.Result != "failed"
		v.Comparison = c
	}
	return v, nil
}

// Routes 返回探测相关路由。
func (s *Service) Routes() []httpx.Route {
	type ap = httpx.AdminPrincipal
	return []httpx.Route{
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/exit-checks", Auth: httpx.Admin,
			Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
				var v *View
				err := s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
					id, err := s.CreateExitCheck(r.Context(), tx, r.PathValue("subscriptionId"), &a.AdminID)
					if err != nil {
						return err
					}
					if err := tx.Audit(r.Context(), a.AdminID, "subscription.check_exit", "subscription:"+r.PathValue("subscriptionId"),
						map[string]any{"probeId": id}); err != nil {
						return err
					}
					v, err = Get(r.Context(), tx, id)
					return err
				})
				return 202, v, err
			})},
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/exit-candidates", Auth: httpx.Admin,
			Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
				in, err := kernel.Body[struct {
					ResourceID string `json:"resourceId"`
				}](r)
				if err != nil {
					return 0, nil, err
				}
				if in.ResourceID == "" {
					return 0, nil, httpx.Invalid("缺少 resourceId")
				}
				var v *View
				err = s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
					sub := r.PathValue("subscriptionId")
					id, err := s.CreateCandidateCompare(r.Context(), tx, sub, in.ResourceID, &a.AdminID)
					if err != nil {
						return err
					}
					if err := tx.Audit(r.Context(), a.AdminID, "subscription.compare_exit", "subscription:"+sub,
						map[string]any{"probeId": id, "resourceId": in.ResourceID}); err != nil {
						return err
					}
					v, err = Get(r.Context(), tx, id)
					return err
				})
				return 202, v, err
			})},
		{Method: "GET", Pattern: "/admin-api/probes/{probeId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := Get(r.Context(), s.env.Pool, r.PathValue("probeId"))
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/upstreams/{upstreamId}/verify", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			var v *View
			err := s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
				res := r.PathValue("upstreamId")
				id, err := s.CreateUpstreamVerify(r.Context(), tx, res, &a.AdminID)
				if err != nil {
					return err
				}
				if err := tx.Audit(r.Context(), a.AdminID, "upstream.verify", "upstream:"+res, map[string]any{"probeId": id}); err != nil {
					return err
				}
				v, err = Get(r.Context(), tx, id)
				return err
			})
			return 202, v, err
		})},
	}
}
