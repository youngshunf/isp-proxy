package inventory

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/agentproto"
	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
)

// ErrNoStock 表示没有可分配的上游资源。
var ErrNoStock = httpx.Conflict("该地区暂时无货，请选择其他地区或稍后再试")

// ReserveForOrder 为未付款新购订单预留库存（R-16.4）：独享预留整份资源（状态 reserved），共享预留一个名额（resource_slots）。
// 并发下最后一份资源 / 最后一个名额只会被一个订单拿到（A-27、A-47）。无货返回 ErrNoStock。
func ReserveForOrder(ctx context.Context, tx *kernel.Tx, mode, country, city, orderID string, until time.Time) (string, error) {
	id, err := pick(ctx, tx, mode, country, city)
	if err != nil {
		return "", err
	}
	if mode == ModeShared {
		_, err = tx.Exec(ctx, `INSERT INTO resource_slots (id, resource_id, kind, order_id, until, created_at)
		                       VALUES ($1, $2, 'order_hold', $3, $4, $5)`, store.NewID("slt"), id, orderID, until, tx.Now())
		if IsTenancyViolation(err) {
			return "", ErrNoStock
		}
		return id, err
	}
	_, err = tx.Exec(ctx, `UPDATE upstream_resources SET status = 'reserved', reserved_order_id = $2, reserved_until = $3,
	                          row_version = row_version + 1 WHERE id = $1`, id, orderID, until)
	if IsTenancyViolation(err) {
		return "", ErrNoStock
	}
	return id, err
}

// ReleaseOrderReservation 释放订单的库存预留（取消、超时、确认收款后转入履约）。
func ReleaseOrderReservation(ctx context.Context, tx *kernel.Tx, orderID string) error {
	if _, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'available', reserved_order_id = NULL, reserved_until = NULL,
	                          row_version = row_version + 1 WHERE reserved_order_id = $1 AND status = 'reserved'`, orderID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM resource_slots WHERE order_id = $1`, orderID)
	return err
}

// ExpireStaleHolds 释放到期仍未回收的订单预留（例如代建失败遗留；调度，可重入）。
func ExpireStaleHolds(ctx context.Context, tx *kernel.Tx, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'available', reserved_order_id = NULL, reserved_until = NULL,
	                         row_version = row_version + 1
	                        WHERE status = 'reserved' AND reserved_order_id IS NOT NULL AND reserved_until <= $1`, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM resource_slots WHERE kind = 'order_hold' AND until <= $1`, now)
	return err
}

// PickForFulfill 为履约选资源：显式指定的资源 > 订单预留的资源 > 同地域按出口类型挑选（R-16.7）。资源行已锁。
func PickForFulfill(ctx context.Context, tx *kernel.Tx, mode string, orderID *string, explicit, country, city string) (string, error) {
	if explicit != "" {
		var status string
		var resOrder *string
		var conflict bool
		var occ int
		err := tx.QueryRow(ctx, `SELECT r.status, r.reserved_order_id,
		        EXISTS (SELECT 1 FROM exit_bindings b WHERE b.verified_ip = r.last_exit_ip AND b.state IN ('pending', 'active')),
		        isp_shared_occupancy(r.id)
		      FROM upstream_resources r WHERE r.id = $1 FOR UPDATE OF r`, explicit).Scan(&status, &resOrder, &conflict, &occ)
		if kernel.IsNoRows(err) {
			return "", httpx.NotFound("没有找到指定的上游资源")
		}
		if err != nil {
			return "", err
		}
		if mode == ModeShared {
			if orderID != nil {
				if _, err := tx.Exec(ctx, `DELETE FROM resource_slots WHERE order_id = $1`, *orderID); err != nil {
					return "", err
				}
			}
			if err := explicitShared(ctx, tx, explicit); err != nil {
				return "", err
			}
			return explicit, nil
		}
		ok := status == "available" || (status == "reserved" && orderID != nil && resOrder != nil && *resOrder == *orderID)
		if !ok {
			return "", httpx.Conflict("指定的资源不可分配（状态 " + status + "）")
		}
		if conflict {
			return "", httpx.Conflict("指定资源的出口 IP 已被其他订阅占用")
		}
		if occ > 0 {
			return "", httpx.Conflict("指定资源上已有共享订阅或冷却中的共享名额，不能分给独享订阅")
		}
		return explicit, nil
	}
	if orderID != nil {
		var id string
		var err error
		if mode == ModeShared {
			// 订单预留的名额转为绑定：先删名额行，绑定在同一事务内写入，名额不会被他人拿走。
			err = tx.QueryRow(ctx, `SELECT resource_id FROM resource_slots WHERE order_id = $1`, *orderID).Scan(&id)
			if err == nil {
				if _, err := tx.Exec(ctx, `SELECT 1 FROM upstream_resources WHERE id = $1 FOR UPDATE`, id); err != nil {
					return "", err
				}
				if _, err := tx.Exec(ctx, `DELETE FROM resource_slots WHERE order_id = $1`, *orderID); err != nil {
					return "", err
				}
				return id, nil
			}
		} else {
			err = tx.QueryRow(ctx, `SELECT id FROM upstream_resources WHERE status = 'reserved' AND reserved_order_id = $1 FOR UPDATE`,
				*orderID).Scan(&id)
			if err == nil {
				return id, nil
			}
		}
		if !kernel.IsNoRows(err) {
			return "", err
		}
	}
	return pick(ctx, tx, mode, country, city)
}

// Bind 把资源标记为被独享订阅占用。共享订阅的占用就是它的出口绑定，资源状态不变。
func Bind(ctx context.Context, tx *kernel.Tx, mode, resID, subID string) error {
	if mode == ModeShared {
		_, err := tx.Exec(ctx, `UPDATE upstream_resources SET row_version = row_version + 1 WHERE id = $1`, resID)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'bound', bound_subscription_id = $2, reserved_order_id = NULL,
	                          reserved_task_id = NULL, reserved_until = NULL, cooldown_until = NULL, row_version = row_version + 1
	                        WHERE id = $1`, resID, subID)
	return err
}

// ReserveForTask 在换出口任务进行中预留候选资源：独享整份预留；共享订阅的待生效绑定本身占一个名额，不另行预留。
func ReserveForTask(ctx context.Context, tx *kernel.Tx, mode, resID, taskID string, until time.Time) error {
	if mode == ModeShared {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'reserved', reserved_task_id = $2, reserved_order_id = NULL,
	                          reserved_until = $3, cooldown_until = NULL, row_version = row_version + 1 WHERE id = $1`, resID, taskID, until)
	return err
}

// Cooldown 解除独享占用并进入冷却（换出口后的原资源、释放的资源；R-11.4，不立即转售）。共享绑定用 ReleaseBinding。
func Cooldown(ctx context.Context, tx *kernel.Tx, resID string, until time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'cooldown', cooldown_until = $2, bound_subscription_id = NULL,
	                          reserved_order_id = NULL, reserved_task_id = NULL, reserved_until = NULL, row_version = row_version + 1
	                        WHERE id = $1`, resID, until)
	return err
}

// ReturnToStock 把未成功使用的独享资源退回可售；quarantine 为 true 时隔离（如上游配置无效）。
func ReturnToStock(ctx context.Context, tx *kernel.Tx, resID string, quarantine bool, reason string) error {
	status := "available"
	var why *string
	if quarantine {
		status = "quarantine"
		why = &reason
	}
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = $2, status_reason = COALESCE($3, status_reason),
	                          bound_subscription_id = NULL, reserved_order_id = NULL, reserved_task_id = NULL, reserved_until = NULL,
	                          cooldown_until = NULL, row_version = row_version + 1 WHERE id = $1`, resID, status, why)
	return err
}

// EndResourceCooldowns 把冷却到期的独享资源放回可售（最近一次验证通过）或待验证，并删除到期的共享冷却名额（调度，可重入）。
func EndResourceCooldowns(ctx context.Context, q kernel.Q, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE upstream_resources SET status = CASE WHEN last_verify_ok THEN 'available' ELSE 'pending_verify' END,
	                           cooldown_until = NULL, row_version = row_version + 1 WHERE status = 'cooldown' AND cooldown_until <= $1`, now)
	if err != nil {
		return 0, err
	}
	slots, err := q.Exec(ctx, `DELETE FROM resource_slots WHERE kind = 'cooldown' AND until <= $1`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected() + slots.RowsAffected(), nil
}

// ExitOf 构造资源对应的 agentproto.Exit（含解密后的上游凭据，只在 mTLS 通道内下发）。
func ExitOf(env *kernel.Env, protocol, host string, port int, username string, passwordEnc []byte, version uint64) (agentproto.Exit, error) {
	e := agentproto.Exit{Version: version, Protocol: protocol, Addr: net.JoinHostPort(host, strconv.Itoa(port)), Username: username}
	if len(passwordEnc) > 0 {
		p, err := env.Open(passwordEnc)
		if err != nil {
			return e, err
		}
		e.Password = p
	}
	return e, nil
}

// StockLevel 把可售数量映射为对客户的档位（docs/06 §8.1）。
func StockLevel(available, lowThreshold int) string {
	switch {
	case available <= 0:
		return "out"
	case available <= lowThreshold:
		return "low"
	default:
		return "plenty"
	}
}

// StockCounts 返回地域内按出口类型计的库存：
// 独享为可售（无共享占用）、已预留、已占用的资源数；共享为剩余名额、订单预留名额、生效与待生效的共享绑定数（R-16.7）。
func StockCounts(ctx context.Context, q kernel.Q, mode, country, city string) (available, reserved, bound int, err error) {
	if mode == ModeShared {
		err = q.QueryRow(ctx, `SELECT
		        COALESCE((SELECT sum(GREATEST(0, r.max_tenants - isp_shared_occupancy(r.id)))
		                    FROM upstream_resources r JOIN nodes n ON n.id = r.node_id
		                   WHERE r.country_code = $1 AND r.city = $2 AND r.status = 'available' AND n.status = 'active'
		                     AND r.max_tenants >= 2 AND NOT isp_exclusive_occupied(r.id)
		                     AND NOT EXISTS (SELECT 1 FROM exit_bindings b WHERE b.verified_ip = r.last_exit_ip
		                                      AND b.state IN ('pending', 'active') AND b.exit_mode = 'exclusive')), 0),
		        (SELECT count(*) FROM resource_slots s JOIN upstream_resources r ON r.id = s.resource_id
		          WHERE s.kind = 'order_hold' AND r.country_code = $1 AND r.city = $2),
		        (SELECT count(*) FROM exit_bindings b JOIN upstream_resources r ON r.id = b.resource_id
		          WHERE b.state IN ('pending', 'active') AND b.exit_mode = 'shared' AND r.country_code = $1 AND r.city = $2)`,
			country, city).Scan(&available, &reserved, &bound)
		return
	}
	err = q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE r.status = 'available' AND n.status = 'active' AND isp_shared_occupancy(r.id) = 0),
	                              count(*) FILTER (WHERE r.status = 'reserved'),
	                              count(*) FILTER (WHERE r.status = 'bound')
	                         FROM upstream_resources r JOIN nodes n ON n.id = r.node_id
	                        WHERE r.country_code = $1 AND r.city = $2`, country, city).Scan(&available, &reserved, &bound)
	return
}

// ---- 管理端视图与接口 ----

// UpstreamView 是 OpenAPI Upstream（不含密码明文）。
type UpstreamView struct {
	ID                  string        `json:"id"`
	NodeID              string        `json:"nodeId"`
	Protocol            string        `json:"protocol"`
	Host                string        `json:"host"`
	Port                int           `json:"port"`
	Username            string        `json:"username"`
	HasPassword         bool          `json:"hasPassword"`
	CountryCode         string        `json:"countryCode"`
	Country             string        `json:"country"`
	City                string        `json:"city"`
	Provider            string        `json:"provider"`
	Note                string        `json:"note"`
	Status              string        `json:"status"`
	StatusReason        *string       `json:"statusReason"`
	ReservedOrderID     *string       `json:"reservedOrderId"`
	ReservedTaskID      *string       `json:"reservedTaskId"`
	ReservedUntil       *time.Time    `json:"reservedUntil"`
	BoundSubscriptionID *string       `json:"boundSubscriptionId"`
	CooldownUntil       *time.Time    `json:"cooldownUntil"`
	MaxTenants          int           `json:"maxTenants"`
	Occupancy           Occupancy     `json:"occupancy"`
	LastVerify          *UpstreamTest `json:"lastVerify"`
	RowVersion          int64         `json:"rowVersion"`
	CreatedAt           time.Time     `json:"createdAt"`
	UpdatedAt           time.Time     `json:"updatedAt"`

	// rawStatus 是库里的状态；Status 对外展示，共享名额用满时显示为 bound（R-16.3）。
	rawStatus string
}

// Occupancy 是 OpenAPI UpstreamOccupancy：资源当前被谁、占了多少（D-24）。
type Occupancy struct {
	// Mode：none 空闲；exclusive 独享占用（预留、占用或冷却整份资源）；shared 有共享名额被占用。
	Mode string `json:"mode"`
	// Used 是占用的名额数：共享为下面三项之和；独享为 1。
	Used     int `json:"used"`
	Bound    int `json:"bound"`
	Reserved int `json:"reserved"`
	Cooldown int `json:"cooldown"`
}

// UpstreamDetail 是 OpenAPI UpstreamDetail：资源详情另列受影响订阅（R-11.10）。
type UpstreamDetail struct {
	*UpstreamView
	Tenants []Tenant `json:"tenants"`
}

// UpstreamTest 是 OpenAPI UpstreamVerify。
type UpstreamTest struct {
	At              time.Time `json:"at"`
	OK              bool      `json:"ok"`
	ExitIP          *string   `json:"exitIp"`
	ExitCountryCode *string   `json:"exitCountryCode"`
	Error           *string   `json:"error"`
}

// upstreamCols 不带表别名（调用方 FROM upstream_resources），子查询用表名引用。
const upstreamCols = `id, node_id, protocol, host, port, username, password_enc IS NOT NULL, country_code, country_name, city,
	provider, note, status, status_reason, reserved_order_id, reserved_task_id, reserved_until, bound_subscription_id,
	cooldown_until, last_verified_at, COALESCE(last_verify_ok, false), host(last_exit_ip), last_exit_country_code, last_error,
	row_version, created_at, updated_at, max_tenants,
	(SELECT count(*) FROM exit_bindings b WHERE b.resource_id = upstream_resources.id AND b.state IN ('pending', 'active')
	   AND b.exit_mode = 'shared'),
	(SELECT count(*) FROM resource_slots s WHERE s.resource_id = upstream_resources.id AND s.kind = 'order_hold'),
	(SELECT count(*) FROM resource_slots s WHERE s.resource_id = upstream_resources.id AND s.kind = 'cooldown')`

// displayStatusSQL 是对外展示的状态（列表筛选与视图一致）：共享名额用满算“已占用”。
const displayStatusSQL = `(CASE WHEN status = 'available' AND isp_shared_occupancy(id) >= max_tenants THEN 'bound' ELSE status END)`

func scanUpstream(row interface{ Scan(...any) error }) (*UpstreamView, error) {
	v := &UpstreamView{}
	var at *time.Time
	var ok bool
	var ip, cc, e *string
	err := row.Scan(&v.ID, &v.NodeID, &v.Protocol, &v.Host, &v.Port, &v.Username, &v.HasPassword, &v.CountryCode, &v.Country,
		&v.City, &v.Provider, &v.Note, &v.Status, &v.StatusReason, &v.ReservedOrderID, &v.ReservedTaskID, &v.ReservedUntil,
		&v.BoundSubscriptionID, &v.CooldownUntil, &at, &ok, &ip, &cc, &e, &v.RowVersion, &v.CreatedAt, &v.UpdatedAt,
		&v.MaxTenants, &v.Occupancy.Bound, &v.Occupancy.Reserved, &v.Occupancy.Cooldown)
	if err != nil {
		return nil, err
	}
	if at != nil {
		v.LastVerify = &UpstreamTest{At: *at, OK: ok, ExitIP: ip, ExitCountryCode: cc, Error: e}
	}
	v.rawStatus = v.Status
	o := &v.Occupancy
	switch shared := o.Bound + o.Reserved + o.Cooldown; {
	case shared > 0:
		o.Mode, o.Used = ModeShared, shared
		if v.Status == "available" && shared >= v.MaxTenants {
			v.Status = "bound"
		}
	case v.Status == "reserved" || v.Status == "bound" || v.Status == "cooldown":
		o.Mode, o.Used = ModeExclusive, 1
		switch v.Status {
		case "reserved":
			o.Reserved = 1
		case "bound":
			o.Bound = 1
		default:
			o.Cooldown = 1
		}
	default:
		o.Mode = "none"
	}
	return v, nil
}

// GetUpstream 读资源视图。
func GetUpstream(ctx context.Context, q kernel.Q, id string) (*UpstreamView, error) {
	v, err := scanUpstream(q.QueryRow(ctx, `SELECT `+upstreamCols+` FROM upstream_resources WHERE id = $1`, id))
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("没有找到这个上游资源")
	}
	return v, err
}

// GetUpstreamDetail 读资源详情（含受影响订阅）。
func GetUpstreamDetail(ctx context.Context, q kernel.Q, id string) (*UpstreamDetail, error) {
	v, err := GetUpstream(ctx, q, id)
	if err != nil {
		return nil, err
	}
	ts, err := Tenants(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return &UpstreamDetail{UpstreamView: v, Tenants: ts}, nil
}

// UpstreamInput 是 OpenAPI UpstreamInput。
type UpstreamInput struct {
	URL         string  `json:"url"`
	Protocol    string  `json:"protocol"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	Username    *string `json:"username"`
	Password    *string `json:"password"`
	NodeID      string  `json:"nodeId"`
	CountryCode string  `json:"countryCode"`
	Country     string  `json:"country"`
	City        string  `json:"city"`
	Provider    string  `json:"provider"`
	Note        string  `json:"note"`
	// MaxTenants 是绑定上限（≥ 1）；不填取配置 core.shared_exit_default_capacity（D-24）。
	MaxTenants *int `json:"maxTenants"`
}

var countryRe = regexp.MustCompile(`^[a-z]{2}$`)

// ParseUpstreamURL 解析 socks5:// http:// https:// 上游地址，正确处理 URL 编码凭据、域名与 IPv6（R-03.1）。
func ParseUpstreamURL(raw string) (protocol, host string, port int, user, pass string, err error) {
	u, perr := url.Parse(strings.TrimSpace(raw))
	if perr != nil || u.Host == "" {
		return "", "", 0, "", "", httpx.Invalid("上游地址格式不正确，例如 socks5://user:pass@host:1080")
	}
	protocol = strings.ToLower(u.Scheme)
	switch protocol {
	case "http", "https", "socks5":
	case "socks5h":
		protocol = "socks5"
	default:
		return "", "", 0, "", "", httpx.Invalid("上游协议只支持 http、https、socks5")
	}
	host = u.Hostname()
	p, perr := strconv.Atoi(u.Port())
	if perr != nil || p < 1 || p > 65535 {
		return "", "", 0, "", "", httpx.Invalid("上游地址缺少有效端口")
	}
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return protocol, host, p, user, pass, nil
}

func validHost(h string) bool {
	return len(h) >= 1 && len(h) <= 253 && !strings.ContainsAny(h, " /[]@")
}

// CreateUpstream 录入上游资源（状态 pending_verify，R-03.2）。字段优先于 url。
func (s *Service) CreateUpstream(ctx context.Context, adminID string, in UpstreamInput) (*UpstreamView, error) {
	var protocol, host, user, pass string
	var port int
	if in.URL != "" {
		var err error
		if protocol, host, port, user, pass, err = ParseUpstreamURL(in.URL); err != nil {
			return nil, err
		}
	}
	if in.Protocol != "" {
		protocol = in.Protocol
	}
	if in.Host != "" {
		host = strings.Trim(strings.TrimSpace(in.Host), "[]")
	}
	if in.Port != 0 {
		port = in.Port
	}
	if in.Username != nil {
		user = *in.Username
	}
	if in.Password != nil {
		pass = *in.Password
	}
	switch protocol {
	case "http", "https", "socks5":
	default:
		return nil, httpx.Invalid("上游协议只支持 http、https、socks5")
	}
	if !validHost(host) || port < 1 || port > 65535 {
		return nil, httpx.Invalid("上游主机或端口不合法")
	}
	if !countryRe.MatchString(in.CountryCode) || strings.TrimSpace(in.Country) == "" || strings.TrimSpace(in.City) == "" {
		return nil, httpx.Invalid("国家代码（两位小写）、国家与城市必填")
	}
	maxTenants := s.env.Cfg.SharedExitDefaultCapacity
	if in.MaxTenants != nil {
		maxTenants = *in.MaxTenants
	}
	if maxTenants < 1 || maxTenants > MaxTenantsLimit {
		return nil, httpx.Invalid(fmt.Sprintf("绑定上限为 1～%d", MaxTenantsLimit))
	}
	var v *UpstreamView
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		var nodeOK bool
		if err := tx.QueryRow(ctx, `SELECT true FROM nodes WHERE id = $1`, in.NodeID).Scan(&nodeOK); err != nil {
			if kernel.IsNoRows(err) {
				return httpx.Invalid("节点不存在")
			}
			return err
		}
		var enc []byte
		if pass != "" {
			enc = s.env.Seal(pass)
		}
		id := store.NewID("res")
		_, err := tx.Exec(ctx, `INSERT INTO upstream_resources (id, node_id, protocol, host, port, username, password_enc,
		                          country_code, country_name, city, provider, note, max_tenants, created_by, created_at, updated_at)
		                        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)`,
			id, in.NodeID, protocol, host, port, user, enc, in.CountryCode, strings.TrimSpace(in.Country), strings.TrimSpace(in.City),
			in.Provider, in.Note, maxTenants, adminID, tx.Now())
		if store.IsUniqueViolation(err, "") {
			return httpx.Conflict("同一协议、主机、端口与用户名的上游已录入")
		}
		if err != nil {
			return err
		}
		if err := tx.Audit(ctx, adminID, "upstream.create", "upstream:"+id, map[string]any{"protocol": protocol, "host": host,
			"port": port, "username": user, "countryCode": in.CountryCode, "city": in.City, "maxTenants": maxTenants}); err != nil {
			return err
		}
		v, err = GetUpstream(ctx, tx, id)
		return err
	})
	return v, err
}

// UpstreamPatch 是 OpenAPI UpstreamPatch。
type UpstreamPatch struct {
	RowVersion  *int64  `json:"rowVersion"`
	Protocol    *string `json:"protocol"`
	Host        *string `json:"host"`
	Port        *int    `json:"port"`
	Username    *string `json:"username"`
	Password    *string `json:"password"`
	CountryCode *string `json:"countryCode"`
	Country     *string `json:"country"`
	City        *string `json:"city"`
	Provider    *string `json:"provider"`
	Note        *string `json:"note"`
	MaxTenants  *int    `json:"maxTenants"`
}

// MaxTenantsLimit 是绑定上限的最大值（防止误填）。
const MaxTenantsLimit = 100

// UpdateUpstream 修改资源（乐观锁）。改连接信息回到 pending_verify；已占用或预留中的资源（含共享绑定与订单预留）不能改连接信息，
// 改连接信息同时清除冷却（含共享冷却名额）。绑定上限不能低于当前共享占用，有共享占用时也不能改为 1（触发器兜底）。
func (s *Service) UpdateUpstream(ctx context.Context, adminID, id string, p UpstreamPatch) (*UpstreamView, error) {
	if p.RowVersion == nil {
		return nil, httpx.Invalid("缺少 rowVersion")
	}
	var v *UpstreamView
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		cur, err := scanUpstream(tx.QueryRow(ctx, `SELECT `+upstreamCols+` FROM upstream_resources WHERE id = $1 FOR UPDATE`, id))
		if kernel.IsNoRows(err) {
			return httpx.NotFound("没有找到这个上游资源")
		}
		if err != nil {
			return err
		}
		if cur.RowVersion != *p.RowVersion {
			return httpx.Conflict("资源已被修改，请刷新后重试")
		}
		conn := p.Protocol != nil || p.Host != nil || p.Port != nil || p.Username != nil || p.Password != nil
		sharedLive := cur.Occupancy.Mode == ModeShared && cur.Occupancy.Bound+cur.Occupancy.Reserved > 0
		if conn && (cur.rawStatus == "bound" || cur.rawStatus == "reserved" || sharedLive) {
			return httpx.Precondition("已占用或预留中的资源不能修改连接信息，请先换出口")
		}
		if p.MaxTenants != nil {
			n := *p.MaxTenants
			switch {
			case n < 1 || n > MaxTenantsLimit:
				return httpx.Invalid(fmt.Sprintf("绑定上限为 1～%d", MaxTenantsLimit))
			case cur.Occupancy.Mode == ModeShared && n < 2:
				return httpx.Precondition("资源上有共享订阅或共享名额，绑定上限不能改为 1")
			case cur.Occupancy.Mode == ModeShared && n < cur.Occupancy.Used:
				return httpx.Precondition(fmt.Sprintf("资源当前占用 %d 个共享名额，绑定上限不能低于该数", cur.Occupancy.Used))
			}
		}
		protocol, host, port, user := cur.Protocol, cur.Host, cur.Port, cur.Username
		if p.Protocol != nil {
			protocol = *p.Protocol
		}
		if p.Host != nil {
			host = strings.Trim(strings.TrimSpace(*p.Host), "[]")
		}
		if p.Port != nil {
			port = *p.Port
		}
		if p.Username != nil {
			user = *p.Username
		}
		switch protocol {
		case "http", "https", "socks5":
		default:
			return httpx.Invalid("上游协议只支持 http、https、socks5")
		}
		if !validHost(host) || port < 1 || port > 65535 {
			return httpx.Invalid("上游主机或端口不合法")
		}
		cc, country, city := cur.CountryCode, cur.Country, cur.City
		if p.CountryCode != nil {
			cc = *p.CountryCode
		}
		if p.Country != nil {
			country = *p.Country
		}
		if p.City != nil {
			city = *p.City
		}
		if !countryRe.MatchString(cc) || country == "" || city == "" {
			return httpx.Invalid("国家代码、国家与城市不合法")
		}
		provider, note := cur.Provider, cur.Note
		if p.Provider != nil {
			provider = *p.Provider
		}
		if p.Note != nil {
			note = *p.Note
		}
		var a kernel.Args
		set := []string{
			"protocol = " + a.Next(protocol), "host = " + a.Next(host), "port = " + a.Next(port), "username = " + a.Next(user),
			"country_code = " + a.Next(cc), "country_name = " + a.Next(country), "city = " + a.Next(city),
			"provider = " + a.Next(provider), "note = " + a.Next(note), "row_version = row_version + 1",
		}
		if p.MaxTenants != nil {
			set = append(set, "max_tenants = "+a.Next(*p.MaxTenants))
		}
		if p.Password != nil {
			var enc []byte
			if *p.Password != "" {
				enc = s.env.Seal(*p.Password)
			}
			set = append(set, "password_enc = "+a.Next(enc))
		}
		if conn {
			set = append(set, "status = CASE WHEN status IN ('available', 'cooldown') THEN 'pending_verify' ELSE status END",
				"cooldown_until = NULL", "last_verified_at = NULL", "last_verify_ok = NULL", "last_exit_ip = NULL",
				"last_exit_country_code = NULL", "last_error = NULL")
		}
		_, err = tx.Exec(ctx, `UPDATE upstream_resources SET `+strings.Join(set, ", ")+` WHERE id = `+a.Next(id), a.Vals...)
		if store.IsUniqueViolation(err, "") {
			return httpx.Conflict("同一协议、主机、端口与用户名的上游已录入")
		}
		if IsTenancyViolation(err) {
			return httpx.Precondition("绑定上限不能低于当前共享占用，请刷新后重试")
		}
		if err != nil {
			return err
		}
		if conn {
			if _, err := tx.Exec(ctx, `DELETE FROM resource_slots WHERE resource_id = $1 AND kind = 'cooldown'`, id); err != nil {
				return err
			}
		}
		detail := map[string]any{"connectionChanged": conn}
		if p.MaxTenants != nil {
			detail["maxTenants"] = map[string]int{"from": cur.MaxTenants, "to": *p.MaxTenants}
		}
		if err := tx.Audit(ctx, adminID, "upstream.update", "upstream:"+id, detail); err != nil {
			return err
		}
		v, err = GetUpstream(ctx, tx, id)
		return err
	})
	return v, err
}

// SetUpstreamStatus 人工改变库存状态（上架、隔离、停用）。
func (s *Service) SetUpstreamStatus(ctx context.Context, adminID, id, status, reason string) (*UpstreamView, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, httpx.Invalid("请填写原因")
	}
	var v *UpstreamView
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		cur, err := scanUpstream(tx.QueryRow(ctx, `SELECT `+upstreamCols+` FROM upstream_resources WHERE id = $1 FOR UPDATE`, id))
		if kernel.IsNoRows(err) {
			return httpx.NotFound("没有找到这个上游资源")
		}
		if err != nil {
			return err
		}
		switch status {
		case "available":
			switch cur.rawStatus {
			case "pending_verify", "cooldown", "quarantine":
			case "available":
			default:
				return httpx.Precondition("只有待验证、冷却或隔离中的资源可以上架")
			}
			if cur.LastVerify == nil || !cur.LastVerify.OK {
				return httpx.Precondition("最近一次验证未通过，不能上架；请先验证")
			}
		case "quarantine", "retired":
			if cur.rawStatus == "bound" {
				return httpx.Precondition("资源已被订阅占用，请先换出口")
			}
			if cur.rawStatus == "reserved" && cur.ReservedTaskID != nil {
				return httpx.Conflict("资源正被换出口任务使用")
			}
			// 共享资源可以带着订阅隔离 / 停用（R-11.10），但开通或换出口进行中（待生效绑定）时先等任务结束。
			var pending int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM exit_bindings WHERE resource_id = $1 AND state = 'pending'`, id).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				return httpx.Conflict("资源正被开通或换出口任务使用，请等任务结束后再试")
			}
		default:
			return httpx.Invalid("status 只能是 available、quarantine、retired")
		}
		if _, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = $2, status_reason = $3, reserved_order_id = NULL,
		                            reserved_task_id = NULL, reserved_until = NULL, cooldown_until = NULL, row_version = row_version + 1
		                          WHERE id = $1`, id, status, reason); err != nil {
			return err
		}
		if status == "available" {
			if err := resolveSharedDown(ctx, tx, id); err != nil {
				return err
			}
		} else {
			// 订单预留的共享名额随隔离取消（与独享预留一致），这些订单在确认收款时重新挑选；冷却名额保留到期。
			if _, err := tx.Exec(ctx, `DELETE FROM resource_slots WHERE resource_id = $1 AND kind = 'order_hold'`, id); err != nil {
				return err
			}
			if err := raiseSharedDown(ctx, tx, id, status, reason); err != nil {
				return err
			}
		}
		if err := tx.Audit(ctx, adminID, "upstream.set_status", "upstream:"+id, map[string]any{"from": cur.rawStatus, "to": status,
			"reason": reason}); err != nil {
			return err
		}
		v, err = GetUpstream(ctx, tx, id)
		return err
	})
	return v, err
}

// ListUpstreams 列出资源（新录入的在前）。
func (s *Service) ListUpstreams(r *http.Request) (*kernel.Page[UpstreamView], error) {
	cur, limit, err := kernel.ParsePage(r)
	if err != nil {
		return nil, err
	}
	statuses, err := kernel.ListParam(r, "status", "pending_verify", "available", "reserved", "bound", "cooldown", "quarantine", "retired")
	if err != nil {
		return nil, err
	}
	var a kernel.Args
	if len(statuses) > 0 {
		a.Add(displayStatusSQL+" = ANY(?)", statuses)
	}
	q := r.URL.Query()
	if v := q.Get("countryCode"); v != "" {
		a.Add("country_code = ?", v)
	}
	if v := q.Get("city"); v != "" {
		a.Add("city = ?", v)
	}
	if v := q.Get("nodeId"); v != "" {
		a.Add("node_id = ?", v)
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		p := a.Next("%" + v + "%")
		a.AddRaw("(id = " + a.Next(v) + " OR host ILIKE " + p + " OR provider ILIKE " + p + " OR host(last_exit_ip) = " + a.Next(v) + ")")
	}
	if cur != nil {
		a.AddRaw("(created_at, id) < (" + a.Next(cur.At) + ", " + a.Next(cur.ID) + ")")
	}
	rows, err := s.env.Pool.Query(r.Context(), `SELECT `+upstreamCols+` FROM upstream_resources`+a.SQL()+
		` ORDER BY created_at DESC, id DESC LIMIT `+strconv.Itoa(limit+1), a.Vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &kernel.Page[UpstreamView]{Items: []UpstreamView{}}
	for rows.Next() {
		v, err := scanUpstream(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = kernel.Ptr(kernel.Cursor{At: last.CreatedAt, ID: last.ID}.Encode())
	}
	return page, nil
}

// UpstreamRoutes 返回上游资源管理路由（验证接口见 probe 包）。
func (s *Service) UpstreamRoutes() []httpx.Route {
	return []httpx.Route{
		{Method: "GET", Pattern: "/admin-api/upstreams", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ httpx.AdminPrincipal) (int, any, error) {
			v, err := s.ListUpstreams(r)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/upstreams", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a httpx.AdminPrincipal) (int, any, error) {
			in, err := kernel.Body[UpstreamInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.CreateUpstream(r.Context(), a.AdminID, in)
			return 201, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/upstreams/{upstreamId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, _ httpx.AdminPrincipal) (int, any, error) {
			v, err := GetUpstreamDetail(r.Context(), s.env.Pool, r.PathValue("upstreamId"))
			return 200, v, err
		})},
		{Method: "PATCH", Pattern: "/admin-api/upstreams/{upstreamId}", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a httpx.AdminPrincipal) (int, any, error) {
			p, err := kernel.Body[UpstreamPatch](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.UpdateUpstream(r.Context(), a.AdminID, r.PathValue("upstreamId"), p)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/upstreams/{upstreamId}/status", Auth: httpx.Admin, Handler: kernel.AdminH(func(r *http.Request, a httpx.AdminPrincipal) (int, any, error) {
			in, err := kernel.Body[struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.SetUpstreamStatus(r.Context(), a.AdminID, r.PathValue("upstreamId"), in.Status, in.Reason)
			return 200, v, err
		})},
	}
}
