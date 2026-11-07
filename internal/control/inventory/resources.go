package inventory

import (
	"context"
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

// 选取可售资源：节点可用、出口 IP 未被任何生效/待生效绑定占用（平台内独占，D-13、R-11.3）。
const pickAvailable = `SELECT r.id FROM upstream_resources r JOIN nodes n ON n.id = r.node_id
	WHERE r.status = 'available' AND r.country_code = $1 AND r.city = $2 AND n.status = 'active'
	  AND NOT EXISTS (SELECT 1 FROM exit_bindings b WHERE b.verified_ip = r.last_exit_ip AND b.state IN ('pending', 'active'))
	ORDER BY r.created_at, r.id LIMIT 1 FOR UPDATE OF r SKIP LOCKED`

// ReserveForOrder 为未付款新购订单预留一个可售资源（R-16.4）。并发下用 SKIP LOCKED，
// 最后一份资源只会被一个订单拿到（A-27）。无货返回 ErrNoStock。
func ReserveForOrder(ctx context.Context, tx *kernel.Tx, country, city, orderID string, until time.Time) (string, error) {
	var id string
	if err := tx.QueryRow(ctx, pickAvailable, country, city).Scan(&id); err != nil {
		if kernel.IsNoRows(err) {
			return "", ErrNoStock
		}
		return "", err
	}
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'reserved', reserved_order_id = $2, reserved_until = $3,
	                          row_version = row_version + 1 WHERE id = $1`, id, orderID, until)
	return id, err
}

// ReleaseOrderReservation 释放订单的库存预留（取消、超时）。
func ReleaseOrderReservation(ctx context.Context, tx *kernel.Tx, orderID string) error {
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'available', reserved_order_id = NULL, reserved_until = NULL,
	                          row_version = row_version + 1 WHERE reserved_order_id = $1 AND status = 'reserved'`, orderID)
	return err
}

// PickForFulfill 为履约选资源：显式指定的资源 > 订单预留的资源 > 同地域任一可售资源。
func PickForFulfill(ctx context.Context, tx *kernel.Tx, orderID *string, explicit, country, city string) (string, error) {
	if explicit != "" {
		var status string
		var resOrder *string
		var conflict bool
		err := tx.QueryRow(ctx, `SELECT r.status, r.reserved_order_id,
		        EXISTS (SELECT 1 FROM exit_bindings b WHERE b.verified_ip = r.last_exit_ip AND b.state IN ('pending', 'active'))
		      FROM upstream_resources r WHERE r.id = $1 FOR UPDATE OF r`, explicit).Scan(&status, &resOrder, &conflict)
		if kernel.IsNoRows(err) {
			return "", httpx.NotFound("没有找到指定的上游资源")
		}
		if err != nil {
			return "", err
		}
		ok := status == "available" || (status == "reserved" && orderID != nil && resOrder != nil && *resOrder == *orderID)
		if !ok {
			return "", httpx.Conflict("指定的资源不可分配（状态 " + status + "）")
		}
		if conflict {
			return "", httpx.Conflict("指定资源的出口 IP 已被其他订阅占用")
		}
		return explicit, nil
	}
	if orderID != nil {
		var id string
		err := tx.QueryRow(ctx, `SELECT id FROM upstream_resources WHERE status = 'reserved' AND reserved_order_id = $1 FOR UPDATE`,
			*orderID).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !kernel.IsNoRows(err) {
			return "", err
		}
	}
	var id string
	if err := tx.QueryRow(ctx, pickAvailable, country, city).Scan(&id); err != nil {
		if kernel.IsNoRows(err) {
			return "", ErrNoStock
		}
		return "", err
	}
	return id, nil
}

// Bind 把资源标记为被订阅占用。
func Bind(ctx context.Context, tx *kernel.Tx, resID, subID string) error {
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'bound', bound_subscription_id = $2, reserved_order_id = NULL,
	                          reserved_task_id = NULL, reserved_until = NULL, cooldown_until = NULL, row_version = row_version + 1
	                        WHERE id = $1`, resID, subID)
	return err
}

// ReserveForTask 在换出口任务进行中预留候选资源。
func ReserveForTask(ctx context.Context, tx *kernel.Tx, resID, taskID string, until time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'reserved', reserved_task_id = $2, reserved_order_id = NULL,
	                          reserved_until = $3, cooldown_until = NULL, row_version = row_version + 1 WHERE id = $1`, resID, taskID, until)
	return err
}

// Cooldown 解除占用并进入冷却（换出口后的原资源、释放的资源；R-11.4，不立即转售）。
func Cooldown(ctx context.Context, tx *kernel.Tx, resID string, until time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = 'cooldown', cooldown_until = $2, bound_subscription_id = NULL,
	                          reserved_order_id = NULL, reserved_task_id = NULL, reserved_until = NULL, row_version = row_version + 1
	                        WHERE id = $1`, resID, until)
	return err
}

// ReturnToStock 把未成功使用的资源退回可售；quarantine 为 true 时隔离（如上游配置无效）。
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

// EndResourceCooldowns 把冷却到期的资源放回可售（最近一次验证通过）或待验证（调度，可重入）。
func EndResourceCooldowns(ctx context.Context, q kernel.Q, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE upstream_resources SET status = CASE WHEN last_verify_ok THEN 'available' ELSE 'pending_verify' END,
	                           cooldown_until = NULL, row_version = row_version + 1 WHERE status = 'cooldown' AND cooldown_until <= $1`, now)
	return tag.RowsAffected(), err
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

// StockCounts 返回地域内资源的可售、已预留、已占用数量。
func StockCounts(ctx context.Context, q kernel.Q, country, city string) (available, reserved, bound int, err error) {
	err = q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE r.status = 'available' AND n.status = 'active'),
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
	LastVerify          *UpstreamTest `json:"lastVerify"`
	RowVersion          int64         `json:"rowVersion"`
	CreatedAt           time.Time     `json:"createdAt"`
	UpdatedAt           time.Time     `json:"updatedAt"`
}

// UpstreamTest 是 OpenAPI UpstreamVerify。
type UpstreamTest struct {
	At              time.Time `json:"at"`
	OK              bool      `json:"ok"`
	ExitIP          *string   `json:"exitIp"`
	ExitCountryCode *string   `json:"exitCountryCode"`
	Error           *string   `json:"error"`
}

const upstreamCols = `id, node_id, protocol, host, port, username, password_enc IS NOT NULL, country_code, country_name, city,
	provider, note, status, status_reason, reserved_order_id, reserved_task_id, reserved_until, bound_subscription_id,
	cooldown_until, last_verified_at, COALESCE(last_verify_ok, false), host(last_exit_ip), last_exit_country_code, last_error,
	row_version, created_at, updated_at`

func scanUpstream(row interface{ Scan(...any) error }) (*UpstreamView, error) {
	v := &UpstreamView{}
	var at *time.Time
	var ok bool
	var ip, cc, e *string
	err := row.Scan(&v.ID, &v.NodeID, &v.Protocol, &v.Host, &v.Port, &v.Username, &v.HasPassword, &v.CountryCode, &v.Country,
		&v.City, &v.Provider, &v.Note, &v.Status, &v.StatusReason, &v.ReservedOrderID, &v.ReservedTaskID, &v.ReservedUntil,
		&v.BoundSubscriptionID, &v.CooldownUntil, &at, &ok, &ip, &cc, &e, &v.RowVersion, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if at != nil {
		v.LastVerify = &UpstreamTest{At: *at, OK: ok, ExitIP: ip, ExitCountryCode: cc, Error: e}
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
		                          country_code, country_name, city, provider, note, created_by, created_at, updated_at)
		                        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)`,
			id, in.NodeID, protocol, host, port, user, enc, in.CountryCode, strings.TrimSpace(in.Country), strings.TrimSpace(in.City),
			in.Provider, in.Note, adminID, tx.Now())
		if store.IsUniqueViolation(err, "") {
			return httpx.Conflict("同一协议、主机、端口与用户名的上游已录入")
		}
		if err != nil {
			return err
		}
		if err := tx.Audit(ctx, adminID, "upstream.create", "upstream:"+id, map[string]any{"protocol": protocol, "host": host,
			"port": port, "username": user, "countryCode": in.CountryCode, "city": in.City}); err != nil {
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
}

// UpdateUpstream 修改资源（乐观锁）。改连接信息回到 pending_verify；已占用或预留中的资源不能改连接信息。
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
		if conn && (cur.Status == "bound" || cur.Status == "reserved") {
			return httpx.Precondition("已占用或预留中的资源不能修改连接信息，请先换出口")
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
		if err != nil {
			return err
		}
		detail := map[string]any{"connectionChanged": conn}
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
			switch cur.Status {
			case "pending_verify", "cooldown", "quarantine":
			case "available":
			default:
				return httpx.Precondition("只有待验证、冷却或隔离中的资源可以上架")
			}
			if cur.LastVerify == nil || !cur.LastVerify.OK {
				return httpx.Precondition("最近一次验证未通过，不能上架；请先验证")
			}
		case "quarantine", "retired":
			if cur.Status == "bound" {
				return httpx.Precondition("资源已被订阅占用，请先换出口")
			}
			if cur.Status == "reserved" && cur.ReservedTaskID != nil {
				return httpx.Conflict("资源正被换出口任务使用")
			}
		default:
			return httpx.Invalid("status 只能是 available、quarantine、retired")
		}
		if _, err := tx.Exec(ctx, `UPDATE upstream_resources SET status = $2, status_reason = $3, reserved_order_id = NULL,
		                            reserved_task_id = NULL, reserved_until = NULL, cooldown_until = NULL, row_version = row_version + 1
		                          WHERE id = $1`, id, status, reason); err != nil {
			return err
		}
		if err := tx.Audit(ctx, adminID, "upstream.set_status", "upstream:"+id, map[string]any{"from": cur.Status, "to": status,
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
		a.Add("status = ANY(?)", statuses)
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
			v, err := GetUpstream(r.Context(), s.env.Pool, r.PathValue("upstreamId"))
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
