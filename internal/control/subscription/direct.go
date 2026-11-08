package subscription

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-gost/gost/internal/control/alert"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/task"
)

// 服务器直出代理（D-25，docs/06 §9.2）：管理员赠送、收回；每个用户最多一个未删除的直出订阅。
// 直出订阅有接入（换接入规则同 ISP 订阅），没有套餐、订单、出口与账本；可用截止跟随该用户的付费订阅（不存储）。

// DirectName 是直出订阅的默认名称（用户可改名）。
const DirectName = "服务器直出"

// 带宽上限的取值范围（Mbps）。
const (
	minDirectBandwidth = 1
	maxDirectBandwidth = 10000
)

// DirectGrantInput 是 OpenAPI AdminDirectGrantInput。
type DirectGrantInput struct {
	BandwidthMbps *int   `json:"bandwidthMbps"`
	Reason        string `json:"reason"`
}

// UserDirect 是 OpenAPI AdminUserDirect：用户详情里的直出代理勾选状态与默认值。
type UserDirect struct {
	SubscriptionID       *string `json:"subscriptionId"`
	Lifecycle            *string `json:"lifecycle"`
	BandwidthMbps        *int    `json:"bandwidthMbps"`
	DefaultBandwidthMbps int     `json:"defaultBandwidthMbps"`
	MaxConnections       int     `json:"maxConnections"`
}

// UserDirect 返回用户详情的 direct（平台 admin.Hooks.UserDirect）。
func (s *Service) UserDirect(ctx context.Context, userID string) (any, error) {
	d := UserDirect{DefaultBandwidthMbps: s.env.Cfg.DirectDefaultBandwidthMbps, MaxConnections: s.env.Cfg.DirectMaxConnections}
	var id, lc string
	var bw, conns int
	err := s.env.Pool.QueryRow(ctx, `SELECT id, lifecycle, direct_bandwidth_mbps, direct_max_conns FROM subscriptions
	                                  WHERE user_id = $1 AND kind = 'direct' AND lifecycle <> 'deleted'`, userID).Scan(&id, &lc, &bw, &conns)
	if kernel.IsNoRows(err) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	d.SubscriptionID, d.Lifecycle, d.BandwidthMbps, d.MaxConnections = &id, &lc, &bw, conns
	return d, nil
}

// lockDirect 对用户未删除的直出订阅加锁，没有时返回空串。
func lockDirect(ctx context.Context, tx *kernel.Tx, userID string) (id, lifecycle string, bw int, err error) {
	err = tx.QueryRow(ctx, `SELECT id, lifecycle, direct_bandwidth_mbps FROM subscriptions
	                         WHERE user_id = $1 AND kind = 'direct' AND lifecycle <> 'deleted' FOR UPDATE`, userID).Scan(&id, &lifecycle, &bw)
	if kernel.IsNoRows(err) {
		return "", "", 0, nil
	}
	return
}

// lockUser 对用户行加锁（同一用户的赠送与收回串行）。
func lockUser(ctx context.Context, tx *kernel.Tx, userID string) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&status)
	if kernel.IsNoRows(err) {
		return httpx.NotFound("没有找到这个用户")
	}
	return err
}

// directNode 选承载节点：优先该用户最新的 ISP 订阅所在的可用节点（同一台中转服务器），否则任一可用节点。
func directNode(ctx context.Context, tx *kernel.Tx, userID string) (string, error) {
	var node string
	err := tx.QueryRow(ctx, `SELECT s.node_id FROM subscriptions s JOIN nodes n ON n.id = s.node_id
	                          WHERE s.user_id = $1 AND s.kind = 'isp' AND s.lifecycle <> 'deleted' AND n.status = 'active'
	                          ORDER BY s.created_at DESC, s.id DESC LIMIT 1`, userID).Scan(&node)
	if err == nil || !kernel.IsNoRows(err) {
		return node, err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM nodes WHERE status = 'active' ORDER BY created_at, id LIMIT 1`).Scan(&node)
	if kernel.IsNoRows(err) {
		return "", httpx.Precondition("没有可用的中转节点，暂不能赠送直出代理")
	}
	return node, err
}

// GrantDirect 赠送直出代理（或调整已赠送的带宽上限；开通失败的重新开通）。返回订阅 ID。
func (s *Service) GrantDirect(ctx context.Context, tx *kernel.Tx, userID, adminID string, in DirectGrantInput) (string, error) {
	bw := s.env.Cfg.DirectDefaultBandwidthMbps
	if in.BandwidthMbps != nil {
		bw = *in.BandwidthMbps
	}
	if bw < minDirectBandwidth || bw > maxDirectBandwidth {
		return "", httpx.Invalid(fmt.Sprintf("带宽上限需为 %d～%d Mbps", minDirectBandwidth, maxDirectBandwidth))
	}
	if len([]rune(in.Reason)) > 200 {
		return "", httpx.Invalid("原因不超过 200 字")
	}
	if err := lockUser(ctx, tx, userID); err != nil {
		return "", err
	}
	id, lc, oldBW, err := lockDirect(ctx, tx, userID)
	if err != nil {
		return "", err
	}
	if id != "" {
		if oldBW != bw {
			if _, err := tx.Exec(ctx, `UPDATE subscriptions SET direct_bandwidth_mbps = $2, row_version = row_version + 1 WHERE id = $1`,
				id, bw); err != nil {
				return "", err
			}
			if err := tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: "adjusted", Detail: fmt.Sprintf("带宽上限 %d → %d Mbps", oldBW, bw),
				ActorKind: "admin", ActorID: &adminID}); err != nil {
				return "", err
			}
			if err := tx.Audit(ctx, adminID, "direct.update", "subscription:"+id,
				map[string]any{"userId": userID, "fromBandwidthMbps": oldBW, "bandwidthMbps": bw, "reason": in.Reason}); err != nil {
				return "", err
			}
			if err := tx.BumpSub(ctx, id); err != nil {
				return "", err
			}
		}
		if lc == "fulfill_failed" {
			if _, err := s.tasks.StartDirectGrant(ctx, tx, id, task.AdminActor(adminID)); err != nil {
				return "", err
			}
			if err := tx.Audit(ctx, adminID, "direct.retry", "subscription:"+id, map[string]any{"userId": userID}); err != nil {
				return "", err
			}
		}
		return id, nil
	}
	node, err := directNode(ctx, tx, userID)
	if err != nil {
		return "", err
	}
	rules, err := kernel.CurrentRules(ctx, tx, tx.Now())
	if err != nil {
		return "", err
	}
	id = store.NewID("sub")
	if _, err := tx.Exec(ctx, `INSERT INTO subscriptions (id, user_id, kind, node_id, rules_version, name, direct_bandwidth_mbps,
	                            direct_max_conns, created_at) VALUES ($1, $2, 'direct', $3, $4, $5, $6, $7, $8)`,
		id, userID, node, rules.Version, DirectName, bw, s.env.Cfg.DirectMaxConnections, tx.Now()); err != nil {
		if store.PgCode(err) == store.CodeUniqueViolation {
			return "", httpx.Conflict("该用户已有直出代理")
		}
		return "", err
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: "adjusted",
		Detail:    fmt.Sprintf("管理员赠送服务器直出代理：带宽上限 %d Mbps、并发 %d，不限流量，跟随付费 IP 有效", bw, s.env.Cfg.DirectMaxConnections),
		ActorKind: "admin", ActorID: &adminID}); err != nil {
		return "", err
	}
	if _, err := s.tasks.StartDirectGrant(ctx, tx, id, task.AdminActor(adminID)); err != nil {
		return "", err
	}
	if err := tx.Audit(ctx, adminID, "direct.grant", "subscription:"+id,
		map[string]any{"userId": userID, "nodeId": node, "bandwidthMbps": bw, "maxConnections": s.env.Cfg.DirectMaxConnections,
			"reason": in.Reason}); err != nil {
		return "", err
	}
	return id, nil
}

// RevokeDirect 收回直出代理：中止进行中的开通/换接入，撤销凭据，端口进入冷却，订阅删除。没有直出代理时什么也不做。
func (s *Service) RevokeDirect(ctx context.Context, tx *kernel.Tx, userID, adminID, reason string) error {
	if len([]rune(reason)) > 200 {
		return httpx.Invalid("原因不超过 200 字")
	}
	if err := lockUser(ctx, tx, userID); err != nil {
		return err
	}
	id, _, _, err := lockDirect(ctx, tx, userID)
	if err != nil || id == "" {
		return err
	}
	if err := s.tasks.AbortRunning(ctx, tx, id, "管理员收回直出代理"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'cancelled', finished_at = $2 WHERE subscription_id = $1 AND state = 'queued'`,
		id, tx.Now()); err != nil {
		return err
	}
	st, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := releaseResources(ctx, tx, id, st, "deleted"); err != nil {
		return err
	}
	if reason == "" {
		reason = "管理员收回"
	}
	now := tx.Now()
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'deleted', lifecycle_reason = $2, deleted_at = $3,
	                            current_access_id = NULL, row_version = row_version + 1 WHERE id = $1`, id, reason, now); err != nil {
		return err
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: id, Kind: "deleted", Detail: "管理员收回服务器直出代理：" + reason,
		ActorKind: "admin", ActorID: &adminID}); err != nil {
		return err
	}
	if err := alert.Resolve(ctx, tx, now, "fulfill_failed:"+id); err != nil {
		return err
	}
	if err := tx.Audit(ctx, adminID, "direct.revoke", "subscription:"+id, map[string]any{"userId": userID, "reason": reason}); err != nil {
		return err
	}
	return tx.BumpSub(ctx, id)
}

// directRoutes 是用户详情里的赠送与收回（管理端）。
func (s *Service) directRoutes() []httpx.Route {
	type ap = httpx.AdminPrincipal
	uid := func(r *http.Request) string { return r.PathValue("userId") }
	return []httpx.Route{
		{Method: "PUT", Pattern: "/admin-api/users/{userId}/direct-proxy", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[DirectGrantInput](r)
			if err != nil {
				return 0, nil, err
			}
			var v *AdminDetail
			err = s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
				id, err := s.GrantDirect(r.Context(), tx, uid(r), a.AdminID, in)
				if err != nil {
					return err
				}
				v, err = s.AdminGet(r.Context(), tx, id)
				return err
			})
			return 200, v, err
		})},
		{Method: "DELETE", Pattern: "/admin-api/users/{userId}/direct-proxy", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
			reason := r.URL.Query().Get("reason")
			err := s.env.InTx(r.Context(), func(tx *kernel.Tx) error { return s.RevokeDirect(r.Context(), tx, uid(r), a.AdminID, reason) })
			return 204, nil, err
		})},
	}
}
