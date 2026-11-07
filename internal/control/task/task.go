// Package task 是变更任务与声明式对账（C-W5，R-03.3～R-03.5、R-04、R-11；docs/04 §4、§7.3）。
//
// 模型：任务把新的接入版本、出口绑定写成 pending 并提升订阅条目版本，记录 target_version；期望状态里订阅的
// 接入/出口取“待生效优先、否则当前”，所以组合更换时新接入直接绑定新出口、旧接入只走旧出口，没有中间态（R-11.6）。
// 节点回报 applied_version ≥ target_version 即成功：pending → active，旧版本撤销、端口与资源进入冷却；
// 回报 apply_error（版本 ≥ target）或超时即失败：pending → failed，新端口与资源退回，期望状态以更高版本回到原样。
// 每订阅同一时刻至多一个进行中任务（部分唯一索引 tasks_one_in_progress）。重试只是重新对账，不重复副作用。
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/alert"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/inventory"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/probe"
	"github.com/go-gost/gost/internal/control/store"
)

// Service 提供变更任务。
type Service struct {
	env    *kernel.Env
	probes *probe.Service
}

// New 创建服务。
func New(env *kernel.Env, probes *probe.Service) *Service { return &Service{env: env, probes: probes} }

// Actor 是任务发起方。
type Actor struct {
	Kind    string // user / admin / system
	UserID  *string
	AdminID *string
}

// AdminActor 返回管理员发起方。
func AdminActor(id string) Actor { return Actor{Kind: "admin", AdminID: &id} }

// UserActor 返回用户发起方。
func UserActor(id string) Actor { return Actor{Kind: "user", UserID: &id} }

// SystemActor 是系统发起方。
var SystemActor = Actor{Kind: "system"}

func (a Actor) eventActor() (string, *string) {
	switch a.Kind {
	case "user":
		return "user", a.UserID
	case "admin":
		return "admin", a.AdminID
	}
	return "system", nil
}

// sub 是加锁后的订阅快照。
type sub struct {
	ID, UserID, Lifecycle, RulesVersion, PlanVersionID string
	Node                                               *string
	AdminPaused                                        bool
	CurAccess, CurExit                                 *string
	UserBanned                                         bool
	FirstReadyAt                                       *time.Time
	CreatedFromOrder                                   *string
}

func lockSub(ctx context.Context, tx *kernel.Tx, id string) (*sub, error) {
	s := &sub{}
	err := tx.QueryRow(ctx, `SELECT s.id, s.user_id, s.lifecycle, s.rules_version, s.plan_version_id, s.node_id, s.admin_paused,
	                            s.current_access_id, s.current_exit_id, u.status = 'banned', s.first_ready_at, s.created_from_order_id
	                          FROM subscriptions s JOIN users u ON u.id = s.user_id WHERE s.id = $1 FOR UPDATE OF s`, id).Scan(
		&s.ID, &s.UserID, &s.Lifecycle, &s.RulesVersion, &s.PlanVersionID, &s.Node, &s.AdminPaused, &s.CurAccess, &s.CurExit,
		&s.UserBanned, &s.FirstReadyAt, &s.CreatedFromOrder)
	if kernel.IsNoRows(err) || (err == nil && s.Lifecycle == "deleted") {
		return nil, httpx.NotFound("没有找到这个订阅")
	}
	return s, err
}

// LockSub 对订阅行加锁并检查归属（userID 非空时要求属于该用户，否则 404）。
func LockSub(ctx context.Context, tx *kernel.Tx, id, userID string) error {
	s, err := lockSub(ctx, tx, id)
	if err != nil {
		return err
	}
	if userID != "" && s.UserID != userID {
		return httpx.NotFound("没有找到这个订阅")
	}
	return nil
}

var errBusy = httpx.Conflict("该订阅有进行中的变更任务，请等待完成后再试")

func ensureIdle(ctx context.Context, tx *kernel.Tx, subID string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE subscription_id = $1 AND state IN ('queued', 'running')`, subID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errBusy
	}
	return nil
}

func (s *Service) newTask(ctx context.Context, tx *kernel.Tx, subID, kind string, a Actor, order, exitReq *string, params map[string]any) (string, error) {
	id := store.NewID("tsk")
	if params == nil {
		params = map[string]any{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO tasks (id, subscription_id, kind, state, requested_by_kind, requested_by_user, requested_by_admin,
	                          order_id, exit_request_id, params, attempts, created_at, started_at)
	                        VALUES ($1, $2, $3, 'running', $4, $5, $6, $7, $8, $9, 1, $10, $10)`,
		id, subID, kind, a.Kind, a.UserID, a.AdminID, order, exitReq, params, tx.Now())
	if store.IsUniqueViolation(err, "tasks_one_in_progress") {
		return "", errBusy
	}
	return id, err
}

// setTarget 提升订阅版本并把任务的 target_version 设为新版本。
func setTarget(ctx context.Context, tx *kernel.Tx, subID, taskID string) error {
	if err := tx.BumpSub(ctx, subID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE tasks SET target_version = (SELECT desired_version FROM subscriptions WHERE id = $1) WHERE id = $2`,
		subID, taskID)
	return err
}

// accessRow 是接入版本的必要字段。
type accessRow struct {
	ID, Username string
	Version      int
	Ports        [3]int
	Node         string
}

func loadAccess(ctx context.Context, tx *kernel.Tx, id string) (*accessRow, error) {
	a := &accessRow{ID: id}
	err := tx.QueryRow(ctx, `SELECT username, version, http_port, https_port, socks5_port, node_id FROM access_versions WHERE id = $1`, id).
		Scan(&a.Username, &a.Version, &a.Ports[0], &a.Ports[1], &a.Ports[2], &a.Node)
	return a, err
}

// issueAccess 签发新接入版本（pending）。password_reset 保留端口与用户名，只换密码；其余分配新端口组与新凭据。
func (s *Service) issueAccess(ctx context.Context, tx *kernel.Tx, subID, node, changeKind, taskID string, base *accessRow) (string, error) {
	var version int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM access_versions WHERE subscription_id = $1`, subID).Scan(&version); err != nil {
		return "", err
	}
	username := kernel.NewAccessUsername()
	var ports [3]int
	unseen := []string{}
	switch changeKind {
	case "password_reset":
		username, ports = base.Username, base.Ports
		unseen = []string{"password"}
		// 用户名在生效/待生效版本中唯一：旧版本先标为撤销（被新版本取代）；失败回滚时恢复。
		if _, err := tx.Exec(ctx, `UPDATE access_versions SET state = 'revoked', revoked_at = $2, revoke_reason = 'superseded'
		                            WHERE id = $1`, base.ID, tx.Now()); err != nil {
			return "", err
		}
	default:
		var err error
		if ports, err = inventory.PickPorts(ctx, tx, node); err != nil {
			return "", err
		}
		if changeKind == "ports_and_password" {
			unseen = []string{"ports", "password"}
		}
	}
	id := store.NewID("acc")
	_, err := tx.Exec(ctx, `INSERT INTO access_versions (id, subscription_id, version, node_id, change_kind, username, password_enc,
	                          http_port, https_port, socks5_port, state, unseen_changes, task_id, issued_at)
	                        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending', $11, $12, $13)`,
		id, subID, version, node, changeKind, username, s.env.Seal(kernel.NewAccessPassword()), ports[0], ports[1], ports[2],
		unseen, taskID, tx.Now())
	if err != nil {
		return "", err
	}
	if changeKind != "password_reset" {
		if err := inventory.AssignPorts(ctx, tx, node, ports, id); err != nil {
			return "", err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET new_access_id = $2 WHERE id = $1`, taskID, id)
	return id, err
}

// newBinding 写入待生效出口绑定。verified 为切换前验证得到的出口 IP（平台内独占判据）。
func newBinding(ctx context.Context, tx *kernel.Tx, subID, resID string, verifiedIP, verifiedCC *string, taskID string) (string, error) {
	var version int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM exit_bindings WHERE subscription_id = $1`, subID).Scan(&version); err != nil {
		return "", err
	}
	id := store.NewID("exb")
	_, err := tx.Exec(ctx, `INSERT INTO exit_bindings (id, subscription_id, version, resource_id, state, verified_ip, verified_country_code,
	                          task_id, created_at)
	                        VALUES ($1, $2, $3, $4, 'pending', $5::inet, $6, $7, $8)`,
		id, subID, version, resID, verifiedIP, verifiedCC, taskID, tx.Now())
	if store.IsUniqueViolation(err, "exit_bindings_ip_live") {
		return "", httpx.Conflict("该出口 IP 已被平台内其他订阅占用")
	}
	if store.IsUniqueViolation(err, "exit_bindings_resource_live") {
		return "", httpx.Conflict("该上游资源已被其他订阅使用")
	}
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET new_exit_id = $2 WHERE id = $1`, taskID, id)
	return id, err
}

// ---- 履约 ----

// FulfillOpts 是履约参数。
type FulfillOpts struct {
	OrderID    string
	NodeID     string // 可选：要求资源在该节点
	ResourceID string // 可选：指定资源
	Actor      Actor
	// MarkOnFail 为 true 时，无库存/端口不足/节点不可用不返回错误，而是把订阅与订单记为“履约异常”（已收款，交付异常）。
	MarkOnFail bool
}

// fulfillFailure 是可记录为“履约异常”的业务失败。
type fulfillFailure struct {
	code, reason string
	err          *httpx.Error
}

// Fulfill 为待履约 / 履约异常的订阅分配资源与端口、签发首个接入并下发（R-03.3）。返回任务 ID；
// MarkOnFail 且失败时返回空串与 nil。重复提交在已有进行中任务时返回 409，不会生成第二个接入或重复占用资源（R-03.5）。
func (s *Service) Fulfill(ctx context.Context, tx *kernel.Tx, subID string, o FulfillOpts) (string, error) {
	sb, err := lockSub(ctx, tx, subID)
	if err != nil {
		return "", err
	}
	if sb.Lifecycle != "pending" && sb.Lifecycle != "fulfill_failed" {
		return "", httpx.Precondition("只有待履约或履约异常的订阅可以开通")
	}
	if err := ensureIdle(ctx, tx, subID); err != nil {
		return "", err
	}
	var country, city string
	if err := tx.QueryRow(ctx, `SELECT country_code, city FROM plan_versions WHERE id = $1`, sb.PlanVersionID).Scan(&country, &city); err != nil {
		return "", err
	}
	var orderPtr *string
	if o.OrderID != "" {
		orderPtr = &o.OrderID
	}
	fail := func(f fulfillFailure) (string, error) {
		if !o.MarkOnFail {
			return "", f.err
		}
		return "", s.markFulfillFailed(ctx, tx, subID, orderPtr, nil, f.code, f.reason)
	}
	resID, err := inventory.PickForFulfill(ctx, tx, orderPtr, o.ResourceID, country, city)
	if errors.Is(err, inventory.ErrNoStock) {
		return fail(fulfillFailure{"no_stock", "该地区暂无可分配的出口资源", inventory.ErrNoStock})
	}
	if err != nil {
		return "", err
	}
	var node, nodeStatus string
	var verifiedIP, verifiedCC *string
	if err := tx.QueryRow(ctx, `SELECT r.node_id, n.status, host(r.last_exit_ip), r.last_exit_country_code
	                              FROM upstream_resources r JOIN nodes n ON n.id = r.node_id WHERE r.id = $1`, resID).
		Scan(&node, &nodeStatus, &verifiedIP, &verifiedCC); err != nil {
		return "", err
	}
	if o.NodeID != "" && o.NodeID != node {
		return "", httpx.Precondition("指定的资源不在所选节点上")
	}
	if nodeStatus != "active" {
		return fail(fulfillFailure{"node_unavailable", "承载节点暂不可用", httpx.Precondition("资源所属节点不可用")})
	}
	if _, err := inventory.PickPorts(ctx, tx, node); errors.Is(err, inventory.ErrNoPorts) {
		_ = alert.Raise(ctx, tx, tx.Now(), alert.Alert{Kind: "port_pool_low", Severity: "critical", Key: "port_pool_low:" + node,
			Message: "节点端口池不足，无法签发新接入", Node: &node})
		return fail(fulfillFailure{"port_pool_exhausted", "节点端口池不足", inventory.ErrNoPorts})
	} else if err != nil {
		return "", err
	}
	kind := "initial"
	if sb.FirstReadyAt != nil {
		kind = "refulfill"
	}
	taskID, err := s.newTask(ctx, tx, subID, "fulfill", o.Actor, orderPtr, nil, map[string]any{"resourceId": resID, "nodeId": node})
	if err != nil {
		return "", err
	}
	if sb.Node != nil && *sb.Node != node {
		if err := tx.BumpNode(ctx, *sb.Node); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET node_id = $2, lifecycle = 'pending', lifecycle_reason = NULL,
	                            row_version = row_version + 1 WHERE id = $1`, subID, node); err != nil {
		return "", err
	}
	if _, err := s.issueAccess(ctx, tx, subID, node, kind, taskID, nil); err != nil {
		return "", err
	}
	if _, err := newBinding(ctx, tx, subID, resID, verifiedIP, verifiedCC, taskID); err != nil {
		return "", err
	}
	if err := inventory.Bind(ctx, tx, resID, subID); err != nil {
		return "", err
	}
	if orderPtr != nil {
		if _, err := tx.Exec(ctx, `UPDATE orders SET fulfillment_status = 'processing', fulfillment_error = NULL, row_version = row_version + 1
		                            WHERE id = $1 AND fulfillment_status <> 'delivered'`, o.OrderID); err != nil {
			return "", err
		}
	}
	return taskID, setTarget(ctx, tx, subID, taskID)
}

// markFulfillFailed 记录“已收款，交付异常”（R-09.8、A-28）：订阅履约异常、订单履约失败、告警。
func (s *Service) markFulfillFailed(ctx context.Context, tx *kernel.Tx, subID string, order, taskID *string, code, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'fulfill_failed', lifecycle_reason = $2, row_version = row_version + 1
	                            WHERE id = $1 AND lifecycle IN ('pending', 'fulfill_failed')`, subID, reason); err != nil {
		return err
	}
	if order != nil {
		if _, err := tx.Exec(ctx, `UPDATE orders SET fulfillment_status = 'failed', fulfillment_error = $2, row_version = row_version + 1
		                            WHERE id = $1 AND payment_status IN ('confirmed', 'refund_review', 'refunded')
		                              AND fulfillment_status <> 'delivered'`,
			*order, "已收款，交付异常："+reason+"；管理员处理中"); err != nil {
			return err
		}
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: subID, Kind: "fulfill_failed", OrderID: order, TaskID: taskID,
		Detail: "开通未成功：" + reason}); err != nil {
		return err
	}
	return alert.Raise(ctx, tx, tx.Now(), alert.Alert{Kind: "fulfill_failed", Severity: "critical", Key: "fulfill_failed:" + subID,
		Message: "已收款订阅交付异常：" + reason, Details: map[string]any{"code": code}, Sub: &subID, Order: order, Task: taskID})
}

// StartRefulfill 已释放订阅续时长后重新履约（R-10.5）：生命周期回到待履约，分配新出口与新接入，就绪后起算。
func (s *Service) StartRefulfill(ctx context.Context, tx *kernel.Tx, subID string, o FulfillOpts) (string, error) {
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'pending', lifecycle_reason = '重新履约', released_at = released_at,
	                            row_version = row_version + 1 WHERE id = $1 AND lifecycle = 'released'`, subID); err != nil {
		return "", err
	}
	return s.Fulfill(ctx, tx, subID, o)
}

// RetryFulfillment 是管理端“重试履约”（待履约 / 履约异常 / 已释放且有剩余时长）。
func (s *Service) RetryFulfillment(ctx context.Context, tx *kernel.Tx, subID, nodeID, resID, adminID string) (string, error) {
	sb, err := lockSub(ctx, tx, subID)
	if err != nil {
		return "", err
	}
	order, err := s.fulfillOrder(ctx, tx, sb)
	if err != nil {
		return "", err
	}
	o := FulfillOpts{OrderID: order, NodeID: nodeID, ResourceID: resID, Actor: AdminActor(adminID)}
	switch sb.Lifecycle {
	case "pending", "fulfill_failed":
		return s.Fulfill(ctx, tx, subID, o)
	case "released":
		var remaining int64
		if err := tx.QueryRow(ctx, `SELECT entitled_seconds_total - seconds_before_period FROM subscriptions WHERE id = $1`, subID).Scan(&remaining); err != nil {
			return "", err
		}
		if remaining <= 0 {
			return "", httpx.Precondition("已释放的订阅需要先续时长，续费后会重新开通")
		}
		return s.StartRefulfill(ctx, tx, subID, o)
	}
	return "", httpx.Precondition("只有待履约、履约异常或已释放（已续时长）的订阅可以重试履约")
}

// fulfillOrder 找履约对应的订单：最近一笔已收款未交付的订单，否则新购订单。
func (s *Service) fulfillOrder(ctx context.Context, tx *kernel.Tx, sb *sub) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM orders WHERE subscription_id = $1 AND payment_status = 'confirmed'
	                           AND fulfillment_status IN ('processing', 'failed') ORDER BY confirmed_at DESC, id DESC LIMIT 1`, sb.ID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !kernel.IsNoRows(err) {
		return "", err
	}
	if sb.CreatedFromOrder != nil {
		return *sb.CreatedFromOrder, nil
	}
	return "", httpx.Precondition("找不到该订阅的订单")
}

// ---- 换接入 ----

// ChangeAccess 换接入：mode = password（重置密码，端口与用户名不变）或 ports_and_password（新端口组、用户名与密码）。
// 用户自助受 D-18 频率限制且封禁、暂停时不可用；管理员代办不计次、暂停或封禁时仍可操作（R-04.7、R-04.8）。
func (s *Service) ChangeAccess(ctx context.Context, tx *kernel.Tx, subID, mode string, a Actor, reason string) (string, error) {
	if mode != "password" && mode != "ports_and_password" {
		return "", httpx.Invalid("mode 只能是 password 或 ports_and_password")
	}
	sb, err := lockSub(ctx, tx, subID)
	if err != nil {
		return "", err
	}
	if a.Kind == "user" && sb.UserID != *a.UserID {
		return "", httpx.NotFound("没有找到这个订阅")
	}
	if err := changePrecondition(sb, a); err != nil {
		return "", err
	}
	if err := ensureIdle(ctx, tx, subID); err != nil {
		return "", err
	}
	if a.Kind == "user" {
		q, err := LoadQuotas(ctx, tx, subID, sb.RulesVersion, tx.Now())
		if err != nil {
			return "", err
		}
		if q.AccessChangesLeft <= 0 {
			return "", httpx.QuotaExhausted(fmt.Sprintf("24 小时内的自助换接入次数（%d 次）已用完，请稍后再试", q.AccessChangesPerDay))
		}
	}
	kind, change := "access_change", "ports_and_password"
	if mode == "password" {
		kind, change = "password_reset", "password_reset"
	}
	base, err := loadAccess(ctx, tx, *sb.CurAccess)
	if err != nil {
		return "", err
	}
	taskID, err := s.newTask(ctx, tx, subID, kind, a, nil, nil, map[string]any{"mode": mode, "reason": reason})
	if err != nil {
		return "", err
	}
	if _, err := s.issueAccess(ctx, tx, subID, base.Node, change, taskID, base); err != nil {
		return "", err
	}
	return taskID, setTarget(ctx, tx, subID, taskID)
}

func changePrecondition(sb *sub, a Actor) error {
	switch sb.Lifecycle {
	case "active", "retained":
	case "released":
		return httpx.Precondition("已释放的订阅没有接入或出口可换；续时长后会重新开通并获得新接入和新出口")
	default:
		return httpx.Precondition("订阅尚未开通，暂不能更换")
	}
	if sb.CurAccess == nil || sb.CurExit == nil {
		return httpx.Precondition("订阅当前没有生效的接入或出口")
	}
	if a.Kind == "user" && (sb.UserBanned || sb.AdminPaused) {
		return httpx.Precondition("当前状态不能自助更换，请联系客服")
	}
	return nil
}

// ---- 换出口 / 组合更换 ----

// ExitChangeInput 是 OpenAPI ExitChangeInput。
type ExitChangeInput struct {
	ResourceID       string `json:"resourceId"`
	CompareProbeID   string `json:"compareProbeId"`
	AlsoChangeAccess bool   `json:"alsoChangeAccess"`
	ExitRequestID    string `json:"exitRequestId"`
	Reason           string `json:"reason"`
}

// ChangeExit 提交换出口（可组合更换接入）。要求 compareProbeId 是该订阅、该资源最近一次已完成且三项比较均通过的探测（R-11.2、R-11.3）。
func (s *Service) ChangeExit(ctx context.Context, tx *kernel.Tx, subID string, in ExitChangeInput, adminID string) (string, error) {
	if in.ResourceID == "" || in.CompareProbeID == "" || in.Reason == "" {
		return "", httpx.Invalid("resourceId、compareProbeId 与 reason 必填")
	}
	sb, err := lockSub(ctx, tx, subID)
	if err != nil {
		return "", err
	}
	a := AdminActor(adminID)
	if err := changePrecondition(sb, a); err != nil {
		return "", err
	}
	if err := ensureIdle(ctx, tx, subID); err != nil {
		return "", err
	}
	var reqPtr *string
	if in.ExitRequestID != "" {
		var reqSub, status string
		err := tx.QueryRow(ctx, `SELECT subscription_id, status FROM exit_requests WHERE id = $1 FOR UPDATE`, in.ExitRequestID).Scan(&reqSub, &status)
		if kernel.IsNoRows(err) || (err == nil && reqSub != subID) {
			return "", httpx.NotFound("没有找到该订阅的这个换出口申请")
		}
		if err != nil {
			return "", err
		}
		if status != "processing" {
			return "", httpx.Precondition("申请尚未受理（超额申请需先确认收费或免收）或已结束")
		}
		reqPtr = &in.ExitRequestID
	}
	// 比较探测：必须是该订阅该资源最近一次完成的 candidate_compare，且三项通过。
	var latest string
	var result *string
	var differs, unique, country *bool
	var obsIP, obsCC *string
	err = tx.QueryRow(ctx, `SELECT id, result, differs_from_current, unique_on_platform, country_matches, host(observed_ip), observed_country_code
	                          FROM probes WHERE kind = 'candidate_compare' AND subscription_id = $1 AND resource_id = $2 AND status = 'done'
	                         ORDER BY completed_at DESC, created_at DESC, id DESC LIMIT 1`, subID, in.ResourceID).
		Scan(&latest, &result, &differs, &unique, &country, &obsIP, &obsCC)
	if kernel.IsNoRows(err) {
		return "", httpx.Precondition("请先对该候选资源完成验证与比较")
	}
	if err != nil {
		return "", err
	}
	if latest != in.CompareProbeID {
		return "", httpx.Precondition("compareProbeId 不是该资源最近一次完成的比较，请重新比较")
	}
	switch {
	case result == nil || *result == "failed":
		return "", httpx.Precondition("候选资源验证失败（认证、连通性或回显），不能切换")
	case differs == nil || !*differs:
		return "", httpx.Precondition("上游已更新，但出口 IP 未变化，不能报告为换出口成功")
	case unique == nil || !*unique:
		return "", httpx.Precondition("候选出口 IP 已被平台内其他订阅占用")
	case country == nil || !*country:
		return "", httpx.Precondition("候选出口的国家与套餐不符")
	}
	var status, resNode string
	if err := tx.QueryRow(ctx, `SELECT status, node_id FROM upstream_resources WHERE id = $1 FOR UPDATE`, in.ResourceID).Scan(&status, &resNode); err != nil {
		if kernel.IsNoRows(err) {
			return "", httpx.NotFound("没有找到这个上游资源")
		}
		return "", err
	}
	if status != "available" && status != "pending_verify" {
		return "", httpx.Conflict("候选资源已不可用（状态 " + status + "）")
	}
	if sb.Node == nil || resNode != *sb.Node {
		return "", httpx.Precondition("候选资源不在该订阅所在的节点上")
	}
	kind := "exit_change"
	if in.AlsoChangeAccess {
		kind = "combined"
	}
	taskID, err := s.newTask(ctx, tx, subID, kind, a, nil, reqPtr, map[string]any{"resourceId": in.ResourceID,
		"compareProbeId": in.CompareProbeID, "reason": in.Reason})
	if err != nil {
		return "", err
	}
	if err := inventory.ReserveForTask(ctx, tx, in.ResourceID, taskID, tx.Now().Add(2*s.env.Cfg.TaskTimeout)); err != nil {
		return "", err
	}
	if _, err := newBinding(ctx, tx, subID, in.ResourceID, obsIP, obsCC, taskID); err != nil {
		return "", err
	}
	if in.AlsoChangeAccess {
		base, err := loadAccess(ctx, tx, *sb.CurAccess)
		if err != nil {
			return "", err
		}
		if _, err := s.issueAccess(ctx, tx, subID, base.Node, "ports_and_password", taskID, base); err != nil {
			return "", err
		}
	}
	if reqPtr != nil {
		if _, err := tx.Exec(ctx, `UPDATE exit_requests SET task_id = $2 WHERE id = $1`, *reqPtr, taskID); err != nil {
			return "", err
		}
	}
	return taskID, setTarget(ctx, tx, subID, taskID)
}

// ---- 对账 ----

type running struct {
	ID, Sub, Kind        string
	Target               int64
	NewAccess, NewExit   *string
	Order, ExitRequest   *string
	ActorKind            string
	ActorUser, ActorAdmn *string
}

func lockRunning(ctx context.Context, tx *kernel.Tx, subID string) (*running, error) {
	r := &running{}
	err := tx.QueryRow(ctx, `SELECT id, subscription_id, kind, COALESCE(target_version, 0), new_access_id, new_exit_id, order_id,
	                            exit_request_id, requested_by_kind, requested_by_user, requested_by_admin
	                          FROM tasks WHERE subscription_id = $1 AND state = 'running' FOR UPDATE`, subID).
		Scan(&r.ID, &r.Sub, &r.Kind, &r.Target, &r.NewAccess, &r.NewExit, &r.Order, &r.ExitRequest, &r.ActorKind, &r.ActorUser, &r.ActorAdmn)
	if kernel.IsNoRows(err) {
		return nil, nil
	}
	return r, err
}

// OnReport 处理节点对某订阅的回报：已应用版本达到目标即成功；目标版本下发失败即失败并回滚。
// 调用方须已对订阅行加锁。
func (s *Service) OnReport(ctx context.Context, tx *kernel.Tx, subID string, applied uint64, applyErr *agentproto.ApplyError) error {
	r, err := lockRunning(ctx, tx, subID)
	if err != nil || r == nil || r.Target == 0 {
		return err
	}
	if int64(applied) >= r.Target {
		return s.succeed(ctx, tx, r)
	}
	if applyErr != nil && int64(applyErr.Version) >= r.Target {
		msg := applyErr.Message
		if msg == "" {
			msg = applyErr.Code
		}
		return s.fail(ctx, tx, r, applyErr.Code, "节点下发失败："+msg, false)
	}
	return nil
}

func (s *Service) succeed(ctx context.Context, tx *kernel.Tx, r *running) error {
	now := tx.Now()
	rules, err := kernel.CurrentRules(ctx, tx, now)
	if err != nil {
		return err
	}
	portCool := now.Add(kernel.Days(rules.PortCooldownDays))
	resCool := now.Add(kernel.Days(rules.ResourceCooldownDays))
	var curAccess, curExit *string
	if err := tx.QueryRow(ctx, `SELECT current_access_id, current_exit_id FROM subscriptions WHERE id = $1`, r.Sub).Scan(&curAccess, &curExit); err != nil {
		return err
	}
	actorKind, actorID := "system", (*string)(nil)
	switch r.ActorKind {
	case "user":
		actorKind, actorID = "user", r.ActorUser
	case "admin":
		actorKind, actorID = "admin", r.ActorAdmn
	}
	var newAccessV, oldAccessV int
	var changeKind string
	if r.NewAccess != nil {
		if err := tx.QueryRow(ctx, `SELECT version, change_kind FROM access_versions WHERE id = $1`, *r.NewAccess).Scan(&newAccessV, &changeKind); err != nil {
			return err
		}
		// 先下线旧版本再启用新版本：每订阅至多一个生效接入（access_versions_one_active）。
		if curAccess != nil && *curAccess != *r.NewAccess {
			if err := tx.QueryRow(ctx, `SELECT version FROM access_versions WHERE id = $1`, *curAccess).Scan(&oldAccessV); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE access_versions SET state = 'revoked', revoked_at = COALESCE(revoked_at, $2),
			                            revoke_reason = COALESCE(revoke_reason, 'replaced') WHERE id = $1`, *curAccess, now); err != nil {
				return err
			}
			if changeKind == "password_reset" {
				if err := inventory.MovePorts(ctx, tx, *curAccess, *r.NewAccess); err != nil {
					return err
				}
			} else if err := inventory.CooldownPorts(ctx, tx, *curAccess, portCool); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE access_versions SET state = 'active', activated_at = $2 WHERE id = $1`, *r.NewAccess, now); err != nil {
			return err
		}
	}
	var oldIP, newIP *string
	if r.NewExit != nil {
		// 先解除旧绑定（资源进入冷却）再启用新绑定：每订阅至多一个生效出口，每资源至多一个订阅。
		if curExit != nil && *curExit != *r.NewExit {
			var oldRes string
			if err := tx.QueryRow(ctx, `UPDATE exit_bindings SET state = 'unbound', unbound_at = $2, unbound_reason = 'replaced'
			                             WHERE id = $1 RETURNING resource_id, COALESCE(host(check_ip), host(verified_ip))`,
				*curExit, now).Scan(&oldRes, &oldIP); err != nil {
				return err
			}
			if err := inventory.Cooldown(ctx, tx, oldRes, resCool); err != nil {
				return err
			}
		}
		var newRes string
		if err := tx.QueryRow(ctx, `UPDATE exit_bindings SET state = 'active', bound_at = $2 WHERE id = $1
		                             RETURNING resource_id, host(verified_ip)`, *r.NewExit, now).Scan(&newRes, &newIP); err != nil {
			return err
		}
		if err := inventory.Bind(ctx, tx, newRes, r.Sub); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET current_access_id = COALESCE($2, current_access_id),
	                            current_exit_id = COALESCE($3, current_exit_id), row_version = row_version + 1 WHERE id = $1`,
		r.Sub, r.NewAccess, r.NewExit); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'succeeded', finished_at = $2 WHERE id = $1`, r.ID, now); err != nil {
		return err
	}
	ev := kernel.Event{Sub: r.Sub, TaskID: &r.ID, OrderID: r.Order, ActorKind: actorKind, ActorID: actorID}
	ipStr := func(p *string) string { return kernel.StrOr(p, "未知") }
	switch r.Kind {
	case "fulfill":
		if err := s.activate(ctx, tx, r, newAccessV, ipStr(newIP)); err != nil {
			return err
		}
		return nil
	case "access_change":
		ev.Kind, ev.Detail = "access_changed", fmt.Sprintf("接入 v%d → v%d：端口与密码已更换；出口与余量不变", oldAccessV, newAccessV)
	case "password_reset":
		ev.Kind, ev.Detail = "password_reset", fmt.Sprintf("接入 v%d → v%d：仅重置密码，端口不变；出口与余量不变", oldAccessV, newAccessV)
	case "exit_change":
		ev.Kind, ev.Detail = "exit_changed", fmt.Sprintf("出口 %s → %s；接入未变，时长与流量不变", ipStr(oldIP), ipStr(newIP))
	case "combined":
		ev.Kind, ev.Detail = "exit_changed", fmt.Sprintf("出口 %s → %s，接入 v%d → v%d：端口与密码已更换；时长与流量不变",
			ipStr(oldIP), ipStr(newIP), oldAccessV, newAccessV)
	}
	if err := tx.AddEvent(ctx, ev); err != nil {
		return err
	}
	if r.NewExit != nil {
		if r.ExitRequest != nil {
			if _, err := tx.Exec(ctx, `UPDATE exit_requests SET status = 'succeeded', new_ip = $2::inet, old_ip = COALESCE(old_ip, $3::inet),
			                            finished_at = $4 WHERE id = $1 AND status = 'processing'`, *r.ExitRequest, newIP, oldIP, now); err != nil {
				return err
			}
		}
		if err := alert.Resolve(ctx, tx, now, "exit_change_failed:"+r.Sub); err != nil {
			return err
		}
		if _, err := s.probes.CreateExitCheck(ctx, tx, r.Sub, nil); err != nil {
			return err
		}
	}
	return nil
}

// activate 履约就绪：生命周期 → 有效，起算时长（首购在首次就绪时起算；重新履约在重新就绪时起算）。
func (s *Service) activate(ctx context.Context, tx *kernel.Tx, r *running, accessV int, ip string) error {
	now := tx.Now()
	var first *time.Time
	var email string
	if err := tx.QueryRow(ctx, `SELECT s.first_ready_at, u.email FROM subscriptions s JOIN users u ON u.id = s.user_id WHERE s.id = $1`,
		r.Sub).Scan(&first, &email); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE subscriptions SET lifecycle = 'active', lifecycle_reason = NULL, retention_ends_at = NULL,
	                             first_ready_at = COALESCE(first_ready_at, $2), period_started_at = $2,
	                             expires_at = $2 + make_interval(secs => entitled_seconds_total - seconds_before_period),
	                             row_version = row_version + 1
	                           WHERE id = $1`, r.Sub, now); err != nil {
		return err
	}
	if r.Order != nil {
		if _, err := tx.Exec(ctx, `UPDATE orders SET fulfillment_status = 'delivered', delivered_at = $2, fulfillment_error = NULL,
		                             row_version = row_version + 1 WHERE id = $1 AND fulfillment_status <> 'delivered'`, *r.Order, now); err != nil {
			return err
		}
	}
	detail := fmt.Sprintf("开通就绪，开始计时；出口 %s，接入 v%d", ip, accessV)
	if first != nil {
		detail = fmt.Sprintf("重新开通就绪，从现在起算；新出口 %s，新接入 v%d；原流量余额不变", ip, accessV)
	}
	if err := tx.AddEvent(ctx, kernel.Event{Sub: r.Sub, Kind: "ready", Detail: detail, OrderID: r.Order, TaskID: &r.ID}); err != nil {
		return err
	}
	if err := alert.Resolve(ctx, tx, now, "fulfill_failed:"+r.Sub); err != nil {
		return err
	}
	if s.env.Mail != nil {
		if err := s.env.Mail.Enqueue(ctx, tx.Tx, email, "notice_ready", map[string]any{"subscriptionId": r.Sub}); err != nil {
			return err
		}
	}
	if err := tx.BumpSub(ctx, r.Sub); err != nil {
		return err
	}
	_, err := s.probes.CreateExitCheck(ctx, tx, r.Sub, nil)
	return err
}

// fail 让任务失败并整体回到原状态（R-03.4、R-11.6）。timeout 为 true 时节点可能已短暂监听新端口，端口进入冷却而不是直接释放。
func (s *Service) fail(ctx context.Context, tx *kernel.Tx, r *running, code, reason string, timeout bool) error {
	now := tx.Now()
	rules, err := kernel.CurrentRules(ctx, tx, now)
	if err != nil {
		return err
	}
	if r.NewAccess != nil {
		var changeKind string
		var username string
		if err := tx.QueryRow(ctx, `UPDATE access_versions SET state = 'failed' WHERE id = $1 RETURNING change_kind, username`,
			*r.NewAccess).Scan(&changeKind, &username); err != nil {
			return err
		}
		if changeKind == "password_reset" {
			// 恢复被取代的旧版本。
			if _, err := tx.Exec(ctx, `UPDATE access_versions SET state = 'active', revoked_at = NULL, revoke_reason = NULL
			                            WHERE id = (SELECT current_access_id FROM subscriptions WHERE id = $1)`, r.Sub); err != nil {
				return err
			}
		} else if timeout {
			if err := inventory.CooldownPorts(ctx, tx, *r.NewAccess, now.Add(kernel.Days(rules.PortCooldownDays))); err != nil {
				return err
			}
		} else {
			if err := inventory.FreePorts(ctx, tx, *r.NewAccess); err != nil {
				return err
			}
		}
	}
	if r.NewExit != nil {
		var res string
		if err := tx.QueryRow(ctx, `UPDATE exit_bindings SET state = 'failed' WHERE id = $1 RETURNING resource_id`, *r.NewExit).Scan(&res); err != nil {
			return err
		}
		if err := inventory.ReturnToStock(ctx, tx, res, code == agentproto.ApplyExitInvalid, reason); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'failed', failure_code = $2, failure_reason = $3, finished_at = $4 WHERE id = $1`,
		r.ID, code, reason, now); err != nil {
		return err
	}
	if r.Kind == "fulfill" {
		if err := s.markFulfillFailed(ctx, tx, r.Sub, r.Order, &r.ID, code, reason); err != nil {
			return err
		}
	} else {
		labels := map[string]string{"access_change": "换接入", "password_reset": "重置密码", "exit_change": "换出口", "combined": "组合更换"}
		if err := tx.AddEvent(ctx, kernel.Event{Sub: r.Sub, Kind: "task_failed", TaskID: &r.ID,
			Detail: labels[r.Kind] + "未成功，已保持原接入与出口：" + reason}); err != nil {
			return err
		}
		if r.NewExit != nil {
			if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: "exit_change_failed", Severity: "warning", Key: "exit_change_failed:" + r.Sub,
				Message: "换出口失败：" + reason, Details: map[string]any{"code": code}, Sub: &r.Sub, Task: &r.ID}); err != nil {
				return err
			}
		}
	}
	if code == agentproto.ApplyPortInUse || code == agentproto.ApplyCertUnavailable || code == agentproto.ApplyInternal {
		kind := "config_failed"
		if code == agentproto.ApplyCertUnavailable {
			kind = "cert_risk"
		}
		if err := alert.Raise(ctx, tx, now, alert.Alert{Kind: kind, Severity: "warning", Key: kind + ":" + r.Sub,
			Message: reason, Details: map[string]any{"code": code}, Sub: &r.Sub, Task: &r.ID}); err != nil {
			return err
		}
	}
	return tx.BumpSub(ctx, r.Sub)
}

// FailTimedOut 让超过 TaskTimeout 仍未完成的任务失败回滚（调度，可重入）。
func (s *Service) FailTimedOut(ctx context.Context) error {
	return s.env.InTx(ctx, func(tx *kernel.Tx) error {
		rows, err := tx.Query(ctx, `SELECT subscription_id FROM tasks WHERE state = 'running' AND started_at <= $1`,
			tx.Now().Add(-s.env.Cfg.TaskTimeout))
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
			if _, err := tx.Exec(ctx, `SELECT 1 FROM subscriptions WHERE id = $1 FOR UPDATE`, id); err != nil {
				return err
			}
			r, err := lockRunning(ctx, tx, id)
			if err != nil {
				return err
			}
			if r == nil {
				continue
			}
			if err := s.fail(ctx, tx, r, "node_timeout", "节点未在规定时间内应用配置（可能离线）", true); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// ---- 配额（D-18、D-19） ----

// Quotas 是 OpenAPI SubscriptionQuotas。
type Quotas struct {
	AccessChangesLeft    int        `json:"accessChangesLeft"`
	AccessChangesPerDay  int        `json:"accessChangesPerDay"`
	FreeExitChangesLeft  int        `json:"freeExitChangesLeft"`
	FreeExitWindowEndsAt *time.Time `json:"freeExitWindowEndsAt"`
}

// LoadQuotas 计算订阅的自助换接入剩余次数与免费换出口剩余次数（按订阅最近一次购买时的规则版本）。
// 换接入：滚动窗口内用户发起、未失败/取消的换接入与重置密码任务；管理员代办与申请附带的组合更换不计。
// 免费换出口：滚动窗口内 charge = free 且状态为待处理/处理中/成功的申请；失败、驳回、取消不计（docs/06 §8.2）。
func LoadQuotas(ctx context.Context, q kernel.Q, subID, rulesVersion string, now time.Time) (Quotas, error) {
	var out Quotas
	rules, err := kernel.RulesByVersion(ctx, q, rulesVersion)
	if err != nil {
		return out, err
	}
	var used int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE subscription_id = $1 AND requested_by_kind = 'user'
	                             AND kind IN ('access_change', 'password_reset') AND state NOT IN ('failed', 'cancelled')
	                             AND created_at > $2`, subID, now.Add(-time.Duration(rules.AccessChangeWindowHours)*time.Hour)).Scan(&used); err != nil {
		return out, err
	}
	out.AccessChangesPerDay = rules.AccessChangesPerWindow
	out.AccessChangesLeft = max(0, rules.AccessChangesPerWindow-used)
	window := kernel.Days(rules.ExitChangeWindowDays)
	var freeUsed int
	var oldest *time.Time
	if err := q.QueryRow(ctx, `SELECT count(*), min(created_at) FROM exit_requests WHERE subscription_id = $1 AND charge = 'free'
	                             AND status IN ('pending', 'processing', 'succeeded') AND created_at > $2`, subID, now.Add(-window)).
		Scan(&freeUsed, &oldest); err != nil {
		return out, err
	}
	out.FreeExitChangesLeft = max(0, rules.FreeExitChangesPerWindow-freeUsed)
	if out.FreeExitChangesLeft == 0 && oldest != nil {
		t := oldest.Add(window).UTC()
		out.FreeExitWindowEndsAt = &t
	}
	return out, nil
}

// ---- 视图 ----

// View 是 OpenAPI Task。
type View struct {
	ID               string       `json:"id"`
	SubscriptionID   string       `json:"subscriptionId"`
	Kind             string       `json:"kind"`
	State            string       `json:"state"`
	RequestedBy      kernel.Actor `json:"requestedBy"`
	OrderID          *string      `json:"orderId"`
	ExitRequestID    *string      `json:"exitRequestId"`
	NewAccessVersion *int         `json:"newAccessVersion"`
	NewExitVersion   *int         `json:"newExitVersion"`
	TargetVersion    *int64       `json:"targetVersion"`
	Attempts         int          `json:"attempts"`
	FailureCode      *string      `json:"failureCode"`
	FailureReason    *string      `json:"failureReason"`
	CreatedAt        time.Time    `json:"createdAt"`
	StartedAt        *time.Time   `json:"startedAt"`
	FinishedAt       *time.Time   `json:"finishedAt"`
}

const viewSelect = `SELECT t.id, t.subscription_id, t.kind, t.state, t.requested_by_kind, COALESCE(t.requested_by_user, t.requested_by_admin),
	COALESCE(u.email, a.email), t.order_id, t.exit_request_id, av.version, eb.version, t.target_version, t.attempts, t.failure_code,
	t.failure_reason, t.created_at, t.started_at, t.finished_at
	FROM tasks t LEFT JOIN users u ON u.id = t.requested_by_user LEFT JOIN admins a ON a.id = t.requested_by_admin
	LEFT JOIN access_versions av ON av.id = t.new_access_id LEFT JOIN exit_bindings eb ON eb.id = t.new_exit_id`

func scanView(row interface{ Scan(...any) error }) (*View, error) {
	v := &View{}
	var email *string
	if err := row.Scan(&v.ID, &v.SubscriptionID, &v.Kind, &v.State, &v.RequestedBy.Kind, &v.RequestedBy.ID, &email, &v.OrderID,
		&v.ExitRequestID, &v.NewAccessVersion, &v.NewExitVersion, &v.TargetVersion, &v.Attempts, &v.FailureCode, &v.FailureReason,
		&v.CreatedAt, &v.StartedAt, &v.FinishedAt); err != nil {
		return nil, err
	}
	v.RequestedBy.Email = kernel.StrOr(email, "")
	return v, nil
}

// Get 读任务。
func Get(ctx context.Context, q kernel.Q, id string) (*View, error) {
	v, err := scanView(q.QueryRow(ctx, viewSelect+` WHERE t.id = $1`, id))
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("没有找到这个任务")
	}
	return v, err
}

// GetOpt 读任务；id 为空返回 nil。
func GetOpt(ctx context.Context, q kernel.Q, id string) (*View, error) {
	if id == "" {
		return nil, nil
	}
	return Get(ctx, q, id)
}

// ListBySub 列出订阅的任务（新的在前）。
func ListBySub(ctx context.Context, q kernel.Q, subID string) ([]View, error) {
	rows, err := q.Query(ctx, viewSelect+` WHERE t.subscription_id = $1 ORDER BY t.created_at DESC, t.id DESC`, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []View{}
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// ---- 路由 ----

func subExists(ctx context.Context, q kernel.Q, id string) error {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT true FROM subscriptions WHERE id = $1`, id).Scan(&ok); err != nil {
		if kernel.IsNoRows(err) {
			return httpx.NotFound("没有找到这个订阅")
		}
		return err
	}
	return nil
}

// Routes 返回管理端任务相关路由。
func (s *Service) Routes() []httpx.Route {
	type ap = httpx.AdminPrincipal
	return []httpx.Route{
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/fulfillment", Auth: httpx.Admin,
			Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
				in, err := kernel.Body[struct {
					NodeID     string `json:"nodeId"`
					ResourceID string `json:"resourceId"`
				}](r)
				if err != nil {
					return 0, nil, err
				}
				sub := r.PathValue("subscriptionId")
				var v *View
				err = s.env.InTx(r.Context(), func(tx *kernel.Tx) error {
					id, err := s.RetryFulfillment(r.Context(), tx, sub, in.NodeID, in.ResourceID, a.AdminID)
					if err != nil {
						return err
					}
					if err := tx.Audit(r.Context(), a.AdminID, "subscription.retry_fulfillment", "subscription:"+sub, in); err != nil {
						return err
					}
					v, err = Get(r.Context(), tx, id)
					return err
				})
				return 200, v, err
			})},
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/access-changes", Auth: httpx.Admin,
			Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
				in, err := kernel.Body[struct {
					Mode   string `json:"mode"`
					Reason string `json:"reason"`
				}](r)
				if err != nil {
					return 0, nil, err
				}
				if in.Reason == "" {
					return 0, nil, httpx.Invalid("请填写原因")
				}
				sub := r.PathValue("subscriptionId")
				v, err := kernel.Idempotent(r.Context(), s.env, "admin:"+a.AdminID+":change_access", httpx.IdempotencyKey(r),
					map[string]any{"sub": sub, "in": in}, func(tx *kernel.Tx) (*View, error) {
						id, err := s.ChangeAccess(r.Context(), tx, sub, in.Mode, AdminActor(a.AdminID), in.Reason)
						if err != nil {
							return nil, err
						}
						if err := tx.Audit(r.Context(), a.AdminID, "access.change", "subscription:"+sub, in); err != nil {
							return nil, err
						}
						return Get(r.Context(), tx, id)
					})
				return 200, v, err
			})},
		{Method: "POST", Pattern: "/admin-api/subscriptions/{subscriptionId}/exit-changes", Auth: httpx.Admin,
			Handler: kernel.AdminH(func(r *http.Request, a ap) (int, any, error) {
				in, err := kernel.Body[ExitChangeInput](r)
				if err != nil {
					return 0, nil, err
				}
				sub := r.PathValue("subscriptionId")
				v, err := kernel.Idempotent(r.Context(), s.env, "admin:"+a.AdminID+":exit_change", httpx.IdempotencyKey(r),
					map[string]any{"sub": sub, "in": in}, func(tx *kernel.Tx) (*View, error) {
						id, err := s.ChangeExit(r.Context(), tx, sub, in, a.AdminID)
						if err != nil {
							return nil, err
						}
						if err := tx.Audit(r.Context(), a.AdminID, "exit.change", "subscription:"+sub, in); err != nil {
							return nil, err
						}
						return Get(r.Context(), tx, id)
					})
				return 200, v, err
			})},
		{Method: "GET", Pattern: "/admin-api/subscriptions/{subscriptionId}/tasks", Auth: httpx.Admin,
			Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
				sub := r.PathValue("subscriptionId")
				if err := subExists(r.Context(), s.env.Pool, sub); err != nil {
					return 0, nil, err
				}
				v, err := ListBySub(r.Context(), s.env.Pool, sub)
				return 200, v, err
			})},
		{Method: "GET", Pattern: "/admin-api/tasks/{taskId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ ap) (int, any, error) {
			v, err := Get(r.Context(), s.env.Pool, r.PathValue("taskId"))
			return 200, v, err
		})},
	}
}

// ensure json import used (params encode as jsonb via pgx).
var _ = json.Marshal
