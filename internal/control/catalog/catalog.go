// Package catalog 是套餐、流量包与规则版本（R-16.1、R-09.9、C-W1 的版本只追加）：用户端目录与管理端维护。
// 版本只追加：发布新版本即成为当前版本，已下单的订单与已购订阅引用具体版本，不追溯（A-38）。
package catalog

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/inventory"
	"github.com/go-gost/gost/internal/control/kernel"
	"github.com/go-gost/gost/internal/control/store"
)

// Service 提供目录接口。
type Service struct{ env *kernel.Env }

// New 创建服务。
func New(env *kernel.Env) *Service { return &Service{env: env} }

// PlanVersion 是套餐版本（领域与 OpenAPI PlanVersion 共用）。
type PlanVersion struct {
	ID            string           `json:"id"`
	PlanID        string           `json:"-"`
	Version       int              `json:"version"`
	Name          string           `json:"name"`
	CountryCode   string           `json:"countryCode"`
	Country       string           `json:"country"`
	City          string           `json:"city"`
	Price         kernel.Money     `json:"price"`
	PeriodDays    int              `json:"periodDays"`
	IncludedBytes int64            `json:"includedBytes"`
	Concurrency   int              `json:"concurrency"`
	BandwidthMbps int              `json:"bandwidthMbps"`
	CreatedAt     time.Time        `json:"createdAt"`
	CreatedBy     *kernel.AdminRef `json:"createdBy"`
}

const pvCols = `v.id, v.plan_id, v.version, v.name, v.country_code, v.country_name, v.city, v.price_cents, v.period_days,
	v.included_bytes, v.concurrency, v.bandwidth_mbps, v.created_at, v.created_by, a.email`

func scanPV(row interface{ Scan(...any) error }) (*PlanVersion, error) {
	v := &PlanVersion{}
	var price int64
	var by, email *string
	if err := row.Scan(&v.ID, &v.PlanID, &v.Version, &v.Name, &v.CountryCode, &v.Country, &v.City, &price, &v.PeriodDays,
		&v.IncludedBytes, &v.Concurrency, &v.BandwidthMbps, &v.CreatedAt, &by, &email); err != nil {
		return nil, err
	}
	v.Price = kernel.CNY(price)
	if by != nil {
		v.CreatedBy = &kernel.AdminRef{ID: *by, Email: kernel.StrOr(email, "")}
	}
	return v, nil
}

// PlanVersionByID 读指定套餐版本。
func PlanVersionByID(ctx context.Context, q kernel.Q, id string) (*PlanVersion, error) {
	v, err := scanPV(q.QueryRow(ctx, `SELECT `+pvCols+` FROM plan_versions v LEFT JOIN admins a ON a.id = v.created_by WHERE v.id = $1`, id))
	if kernel.IsNoRows(err) {
		return nil, httpx.NotFound("套餐版本不存在")
	}
	return v, err
}

// CurrentPlanVersion 读套餐的当前版本（version 最大）与套餐状态。
func CurrentPlanVersion(ctx context.Context, q kernel.Q, planID string) (*PlanVersion, string, error) {
	var status string
	if err := q.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, planID).Scan(&status); err != nil {
		if kernel.IsNoRows(err) {
			return nil, "", httpx.NotFound("套餐不存在或已下架")
		}
		return nil, "", err
	}
	v, err := scanPV(q.QueryRow(ctx, `SELECT `+pvCols+` FROM plan_versions v LEFT JOIN admins a ON a.id = v.created_by
	                                   WHERE v.plan_id = $1 ORDER BY v.version DESC LIMIT 1`, planID))
	return v, status, err
}

// PackVersion 是流量包版本。
type PackVersion struct {
	ID        string           `json:"id"`
	PackID    string           `json:"-"`
	Version   int              `json:"version"`
	Bytes     int64            `json:"bytes"`
	Price     kernel.Money     `json:"price"`
	CreatedAt time.Time        `json:"createdAt"`
	CreatedBy *kernel.AdminRef `json:"createdBy"`
}

const tpvCols = `v.id, v.traffic_pack_id, v.version, v.bytes, v.price_cents, v.created_at, v.created_by, a.email`

func scanTPV(row interface{ Scan(...any) error }) (*PackVersion, error) {
	v := &PackVersion{}
	var price int64
	var by, email *string
	if err := row.Scan(&v.ID, &v.PackID, &v.Version, &v.Bytes, &price, &v.CreatedAt, &by, &email); err != nil {
		return nil, err
	}
	v.Price = kernel.CNY(price)
	if by != nil {
		v.CreatedBy = &kernel.AdminRef{ID: *by, Email: kernel.StrOr(email, "")}
	}
	return v, nil
}

// CurrentPackVersion 读流量包当前版本与状态。
func CurrentPackVersion(ctx context.Context, q kernel.Q, packID string) (*PackVersion, string, error) {
	var status string
	if err := q.QueryRow(ctx, `SELECT status FROM traffic_packs WHERE id = $1`, packID).Scan(&status); err != nil {
		if kernel.IsNoRows(err) {
			return nil, "", httpx.NotFound("流量包不存在或已下架")
		}
		return nil, "", err
	}
	v, err := scanTPV(q.QueryRow(ctx, `SELECT `+tpvCols+` FROM traffic_pack_versions v LEFT JOIN admins a ON a.id = v.created_by
	                                    WHERE v.traffic_pack_id = $1 ORDER BY v.version DESC LIMIT 1`, packID))
	return v, status, err
}

// ---- 用户端目录 ----

// PlanView 是 OpenAPI Plan。
type PlanView struct {
	ID            string       `json:"id"`
	VersionID     string       `json:"versionId"`
	CountryCode   string       `json:"countryCode"`
	Country       string       `json:"country"`
	City          string       `json:"city"`
	MonthlyPrice  kernel.Money `json:"monthlyPrice"`
	IncludedBytes int64        `json:"includedBytes"`
	Concurrency   int          `json:"concurrency"`
	BandwidthMbps int          `json:"bandwidthMbps"`
	Stock         string       `json:"stock"`
}

// PackView 是 OpenAPI TrafficPack。
type PackView struct {
	ID        string       `json:"id"`
	VersionID string       `json:"versionId"`
	Bytes     int64        `json:"bytes"`
	Price     kernel.Money `json:"price"`
}

// RulesView 是 OpenAPI Rules。
type RulesView struct {
	Version                  string `json:"version"`
	PeriodDays               int    `json:"periodDays"`
	RetentionDays            int    `json:"retentionDays"`
	AccessChangesPerDay      int    `json:"accessChangesPerDay"`
	FreeExitChangesPer30Days int    `json:"freeExitChangesPer30Days"`
	UnpaidHoldHours          int    `json:"unpaidHoldHours"`
	MaxUnpaidNewOrders       int    `json:"maxUnpaidNewOrders"`
}

// Payment 是 OpenAPI PaymentInstructions。
type Payment struct {
	Payee   string `json:"payee"`
	Bank    string `json:"bank"`
	Account string `json:"account"`
	Note    string `json:"note"`
}

// Catalog 是 OpenAPI Catalog。
type Catalog struct {
	Brand        string     `json:"brand"`
	Plans        []PlanView `json:"plans"`
	TrafficPacks []PackView `json:"trafficPacks"`
	MonthOptions []int      `json:"monthOptions"`
	Rules        RulesView  `json:"rules"`
	Payment      Payment    `json:"payment"`
}

// GetCatalog 返回上架套餐（库存只给档位）、上架流量包、当前规则与收款说明。
func (s *Service) GetCatalog(ctx context.Context) (*Catalog, error) {
	q := s.env.Pool
	rules, err := kernel.CurrentRules(ctx, q, s.env.Now())
	if err != nil {
		return nil, err
	}
	c := &Catalog{Brand: s.env.Cfg.Brand, Plans: []PlanView{}, TrafficPacks: []PackView{}, MonthOptions: []int{},
		Rules: RulesView{Version: rules.Version, PeriodDays: rules.PeriodDays, RetentionDays: rules.RetentionDays,
			AccessChangesPerDay: rules.AccessChangesPerWindow, FreeExitChangesPer30Days: rules.FreeExitChangesPerWindow,
			UnpaidHoldHours: rules.UnpaidHoldHours, MaxUnpaidNewOrders: rules.MaxUnpaidNewOrders},
		Payment: Payment{Payee: rules.PaymentPayee, Bank: rules.PaymentBank, Account: rules.PaymentAccount, Note: rules.PaymentNote}}
	for _, m := range rules.MonthOptions {
		c.MonthOptions = append(c.MonthOptions, int(m))
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (p.id) p.id, p.sort_order, v.id, v.country_code, v.country_name, v.city, v.price_cents,
	                              v.included_bytes, v.concurrency, v.bandwidth_mbps
	                            FROM plans p JOIN plan_versions v ON v.plan_id = p.id
	                           WHERE p.status = 'on_sale' ORDER BY p.id, v.version DESC`)
	if err != nil {
		return nil, err
	}
	type planRow struct {
		PlanView
		sort int
	}
	var plans []planRow
	for rows.Next() {
		var p planRow
		var price int64
		if err := rows.Scan(&p.ID, &p.sort, &p.VersionID, &p.CountryCode, &p.Country, &p.City, &price, &p.IncludedBytes,
			&p.Concurrency, &p.BandwidthMbps); err != nil {
			rows.Close()
			return nil, err
		}
		p.MonthlyPrice = kernel.CNY(price)
		plans = append(plans, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortPlans(plans, func(p planRow) (int, string) { return p.sort, p.ID })
	for _, p := range plans {
		avail, _, _, err := inventory.StockCounts(ctx, q, p.CountryCode, p.City)
		if err != nil {
			return nil, err
		}
		p.Stock = inventory.StockLevel(avail, s.env.Cfg.StockLowThreshold)
		c.Plans = append(c.Plans, p.PlanView)
	}
	prow, err := q.Query(ctx, `SELECT DISTINCT ON (t.id) t.id, t.sort_order, v.id, v.bytes, v.price_cents
	                             FROM traffic_packs t JOIN traffic_pack_versions v ON v.traffic_pack_id = t.id
	                            WHERE t.status = 'on_sale' ORDER BY t.id, v.version DESC`)
	if err != nil {
		return nil, err
	}
	type packRow struct {
		PackView
		sort int
	}
	var packs []packRow
	for prow.Next() {
		var p packRow
		var price int64
		if err := prow.Scan(&p.ID, &p.sort, &p.VersionID, &p.Bytes, &price); err != nil {
			prow.Close()
			return nil, err
		}
		p.Price = kernel.CNY(price)
		packs = append(packs, p)
	}
	prow.Close()
	sortPlans(packs, func(p packRow) (int, string) { return p.sort, p.ID })
	for _, p := range packs {
		c.TrafficPacks = append(c.TrafficPacks, p.PackView)
	}
	return c, prow.Err()
}

func sortPlans[T any](xs []T, key func(T) (int, string)) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0; j-- {
			a, as := key(xs[j-1])
			b, bs := key(xs[j])
			if a < b || (a == b && as <= bs) {
				break
			}
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// ---- 管理端：套餐 ----

// PlanStock 是 OpenAPI PlanStock。
type PlanStock struct {
	Available int    `json:"available"`
	Reserved  int    `json:"reserved"`
	Bound     int    `json:"bound"`
	Level     string `json:"level"`
}

// AdminPlan 是 OpenAPI AdminPlan。
type AdminPlan struct {
	ID        string        `json:"id"`
	Code      string        `json:"code"`
	Status    string        `json:"status"`
	SortOrder int           `json:"sortOrder"`
	Current   PlanVersion   `json:"current"`
	Stock     PlanStock     `json:"stock"`
	Versions  []PlanVersion `json:"versions"`
}

func (s *Service) loadPlan(ctx context.Context, q kernel.Q, id string, allVersions bool) (*AdminPlan, error) {
	p := &AdminPlan{ID: id}
	if err := q.QueryRow(ctx, `SELECT code, status, sort_order FROM plans WHERE id = $1`, id).Scan(&p.Code, &p.Status, &p.SortOrder); err != nil {
		if kernel.IsNoRows(err) {
			return nil, httpx.NotFound("没有找到这个套餐")
		}
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+pvCols+` FROM plan_versions v LEFT JOIN admins a ON a.id = v.created_by
	                            WHERE v.plan_id = $1 ORDER BY v.version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanPV(rows)
		if err != nil {
			return nil, err
		}
		p.Versions = append(p.Versions, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(p.Versions) == 0 {
		return nil, httpx.NotFound("套餐没有版本")
	}
	p.Current = p.Versions[0]
	if !allVersions {
		p.Versions = p.Versions[:1]
	}
	a, r, b, err := inventory.StockCounts(ctx, q, p.Current.CountryCode, p.Current.City)
	if err != nil {
		return nil, err
	}
	p.Stock = PlanStock{Available: a, Reserved: r, Bound: b, Level: inventory.StockLevel(a, s.env.Cfg.StockLowThreshold)}
	return p, nil
}

// PlanVersionInput 是 OpenAPI PlanVersionInput。
type PlanVersionInput struct {
	Name          string       `json:"name"`
	CountryCode   string       `json:"countryCode"`
	Country       string       `json:"country"`
	City          string       `json:"city"`
	Price         kernel.Money `json:"price"`
	PeriodDays    int          `json:"periodDays"`
	IncludedBytes int64        `json:"includedBytes"`
	Concurrency   int          `json:"concurrency"`
	BandwidthMbps int          `json:"bandwidthMbps"`
}

var codeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)
var countryRe = regexp.MustCompile(`^[a-z]{2}$`)

func (in *PlanVersionInput) validate() error {
	in.Name, in.Country, in.City = strings.TrimSpace(in.Name), strings.TrimSpace(in.Country), strings.TrimSpace(in.City)
	switch {
	case in.Name == "" || len([]rune(in.Name)) > 64:
		return httpx.Invalid("套餐名称为 1～64 个字符")
	case !countryRe.MatchString(in.CountryCode) || in.Country == "" || in.City == "":
		return httpx.Invalid("国家代码（两位小写）、国家与城市必填")
	case in.Price.Amount < 0 || (in.Price.Currency != "" && in.Price.Currency != "CNY"):
		return httpx.Invalid("价格必须是非负的人民币分")
	case in.PeriodDays < 1 || in.IncludedBytes < 0 || in.Concurrency < 1 || in.BandwidthMbps < 1:
		return httpx.Invalid("周期、流量、并发与带宽必须为正")
	}
	return nil
}

func insertPV(ctx context.Context, tx *kernel.Tx, planID string, version int, in PlanVersionInput, adminID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO plan_versions (id, plan_id, version, name, country_code, country_name, city, price_cents,
	                          period_days, included_bytes, concurrency, bandwidth_mbps, created_by, created_at)
	                        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		store.NewID("plv"), planID, version, in.Name, in.CountryCode, in.Country, in.City, in.Price.Amount, in.PeriodDays,
		in.IncludedBytes, in.Concurrency, in.BandwidthMbps, adminID, tx.Now())
	return err
}

// CreatePlan 新建套餐（草稿，含版本 1）。
func (s *Service) CreatePlan(ctx context.Context, adminID, code string, sort int, in PlanVersionInput) (*AdminPlan, error) {
	if !codeRe.MatchString(code) {
		return nil, httpx.Invalid("套餐代码只能是小写字母、数字与连字符（2～32 位）")
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	var p *AdminPlan
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		id := store.NewID("pln")
		_, err := tx.Exec(ctx, `INSERT INTO plans (id, code, sort_order, created_by, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5)`,
			id, code, sort, adminID, tx.Now())
		if store.IsUniqueViolation(err, "") {
			return httpx.Conflict("套餐代码已存在")
		}
		if err != nil {
			return err
		}
		if err := insertPV(ctx, tx, id, 1, in, adminID); err != nil {
			return err
		}
		if err := tx.Audit(ctx, adminID, "plan.create", "plan:"+id, map[string]any{"code": code, "version": in}); err != nil {
			return err
		}
		p, err = s.loadPlan(ctx, tx, id, true)
		return err
	})
	return p, err
}

// CreatePlanVersion 发布新版本（只追加，立即成为当前版本）。
func (s *Service) CreatePlanVersion(ctx context.Context, adminID, planID string, in PlanVersionInput) (*AdminPlan, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	var p *AdminPlan
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		var max int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(v.version), 0) FROM plans p LEFT JOIN plan_versions v ON v.plan_id = p.id
		                             WHERE p.id = $1 GROUP BY p.id`, planID).Scan(&max); err != nil {
			if kernel.IsNoRows(err) {
				return httpx.NotFound("没有找到这个套餐")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM plans WHERE id = $1 FOR UPDATE`, planID); err != nil {
			return err
		}
		if err := insertPV(ctx, tx, planID, max+1, in, adminID); err != nil {
			if store.IsUniqueViolation(err, "") {
				return httpx.Conflict("版本号冲突，请重试")
			}
			return err
		}
		if err := tx.Audit(ctx, adminID, "plan.publish_version", "plan:"+planID, map[string]any{"version": max + 1, "input": in}); err != nil {
			return err
		}
		var err error
		p, err = s.loadPlan(ctx, tx, planID, true)
		return err
	})
	return p, err
}

func validSale(st string) bool { return st == "draft" || st == "on_sale" || st == "off_sale" }

// SetPlanStatus 上架 / 下架。
func (s *Service) SetPlanStatus(ctx context.Context, adminID, planID, status string) (*AdminPlan, error) {
	if !validSale(status) {
		return nil, httpx.Invalid("status 只能是 draft、on_sale、off_sale")
	}
	var p *AdminPlan
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE plans SET status = $2 WHERE id = $1`, planID, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("没有找到这个套餐")
		}
		if err := tx.Audit(ctx, adminID, "plan.set_status", "plan:"+planID, map[string]any{"status": status}); err != nil {
			return err
		}
		p, err = s.loadPlan(ctx, tx, planID, true)
		return err
	})
	return p, err
}

// ListPlans 列出全部套餐（只含当前版本）。
func (s *Service) ListPlans(ctx context.Context) ([]AdminPlan, error) {
	rows, err := s.env.Pool.Query(ctx, `SELECT id FROM plans ORDER BY sort_order, code`)
	if err != nil {
		return nil, err
	}
	ids, err := collectStrings(rows)
	if err != nil {
		return nil, err
	}
	out := []AdminPlan{}
	for _, id := range ids {
		p, err := s.loadPlan(ctx, s.env.Pool, id, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}

func collectStrings(rows interface {
	Next() bool
	Scan(...any) error
	Close()
	Err() error
}) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- 管理端：流量包 ----

// AdminPack 是 OpenAPI AdminTrafficPack。
type AdminPack struct {
	ID        string        `json:"id"`
	Code      string        `json:"code"`
	Status    string        `json:"status"`
	SortOrder int           `json:"sortOrder"`
	Current   PackVersion   `json:"current"`
	Versions  []PackVersion `json:"versions"`
}

func (s *Service) loadPack(ctx context.Context, q kernel.Q, id string, all bool) (*AdminPack, error) {
	p := &AdminPack{ID: id}
	if err := q.QueryRow(ctx, `SELECT code, status, sort_order FROM traffic_packs WHERE id = $1`, id).Scan(&p.Code, &p.Status, &p.SortOrder); err != nil {
		if kernel.IsNoRows(err) {
			return nil, httpx.NotFound("没有找到这个流量包")
		}
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+tpvCols+` FROM traffic_pack_versions v LEFT JOIN admins a ON a.id = v.created_by
	                            WHERE v.traffic_pack_id = $1 ORDER BY v.version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanTPV(rows)
		if err != nil {
			return nil, err
		}
		p.Versions = append(p.Versions, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(p.Versions) == 0 {
		return nil, httpx.NotFound("流量包没有版本")
	}
	p.Current = p.Versions[0]
	if !all {
		p.Versions = p.Versions[:1]
	}
	return p, nil
}

// PackVersionInput 是 OpenAPI TrafficPackVersionInput。
type PackVersionInput struct {
	Bytes int64        `json:"bytes"`
	Price kernel.Money `json:"price"`
}

func (in PackVersionInput) validate() error {
	if in.Bytes < 1 || in.Price.Amount < 0 || (in.Price.Currency != "" && in.Price.Currency != "CNY") {
		return httpx.Invalid("流量必须为正，价格必须是非负的人民币分")
	}
	return nil
}

func insertTPV(ctx context.Context, tx *kernel.Tx, packID string, version int, in PackVersionInput, adminID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO traffic_pack_versions (id, traffic_pack_id, version, bytes, price_cents, created_by, created_at)
	                        VALUES ($1, $2, $3, $4, $5, $6, $7)`, store.NewID("tpv"), packID, version, in.Bytes, in.Price.Amount, adminID, tx.Now())
	return err
}

// CreatePack 新建流量包（草稿，含版本 1）。
func (s *Service) CreatePack(ctx context.Context, adminID, code string, sort int, in PackVersionInput) (*AdminPack, error) {
	if !codeRe.MatchString(code) {
		return nil, httpx.Invalid("流量包代码只能是小写字母、数字与连字符（2～32 位）")
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	var p *AdminPack
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		id := store.NewID("tpk")
		_, err := tx.Exec(ctx, `INSERT INTO traffic_packs (id, code, sort_order, created_by, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5)`,
			id, code, sort, adminID, tx.Now())
		if store.IsUniqueViolation(err, "") {
			return httpx.Conflict("流量包代码已存在")
		}
		if err != nil {
			return err
		}
		if err := insertTPV(ctx, tx, id, 1, in, adminID); err != nil {
			return err
		}
		if err := tx.Audit(ctx, adminID, "traffic_pack.create", "traffic_pack:"+id, map[string]any{"code": code, "version": in}); err != nil {
			return err
		}
		p, err = s.loadPack(ctx, tx, id, true)
		return err
	})
	return p, err
}

// CreatePackVersion 发布流量包新版本。
func (s *Service) CreatePackVersion(ctx context.Context, adminID, packID string, in PackVersionInput) (*AdminPack, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	var p *AdminPack
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		var max int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(v.version), 0) FROM traffic_packs t
		                              LEFT JOIN traffic_pack_versions v ON v.traffic_pack_id = t.id WHERE t.id = $1 GROUP BY t.id`,
			packID).Scan(&max); err != nil {
			if kernel.IsNoRows(err) {
				return httpx.NotFound("没有找到这个流量包")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM traffic_packs WHERE id = $1 FOR UPDATE`, packID); err != nil {
			return err
		}
		if err := insertTPV(ctx, tx, packID, max+1, in, adminID); err != nil {
			if store.IsUniqueViolation(err, "") {
				return httpx.Conflict("版本号冲突，请重试")
			}
			return err
		}
		if err := tx.Audit(ctx, adminID, "traffic_pack.publish_version", "traffic_pack:"+packID, map[string]any{"version": max + 1, "input": in}); err != nil {
			return err
		}
		var err error
		p, err = s.loadPack(ctx, tx, packID, true)
		return err
	})
	return p, err
}

// SetPackStatus 上架 / 下架流量包。
func (s *Service) SetPackStatus(ctx context.Context, adminID, packID, status string) (*AdminPack, error) {
	if !validSale(status) {
		return nil, httpx.Invalid("status 只能是 draft、on_sale、off_sale")
	}
	var p *AdminPack
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE traffic_packs SET status = $2 WHERE id = $1`, packID, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("没有找到这个流量包")
		}
		if err := tx.Audit(ctx, adminID, "traffic_pack.set_status", "traffic_pack:"+packID, map[string]any{"status": status}); err != nil {
			return err
		}
		p, err = s.loadPack(ctx, tx, packID, true)
		return err
	})
	return p, err
}

// ---- 管理端：规则版本 ----

// RulesVersion 是 OpenAPI RulesVersion（RulesInput + createdAt、createdBy）。
type RulesVersion struct {
	RulesInput
	CreatedAt time.Time        `json:"createdAt"`
	CreatedBy *kernel.AdminRef `json:"createdBy"`
}

// RulesInput 是 OpenAPI RulesInput。
type RulesInput struct {
	Version                  string    `json:"version"`
	EffectiveFrom            time.Time `json:"effectiveFrom"`
	PeriodDays               int       `json:"periodDays"`
	RetentionDays            int       `json:"retentionDays"`
	PortCooldownDays         int       `json:"portCooldownDays"`
	ResourceCooldownDays     int       `json:"resourceCooldownDays"`
	AccessChangesPerWindow   int       `json:"accessChangesPerWindow"`
	AccessChangeWindowHours  int       `json:"accessChangeWindowHours"`
	FreeExitChangesPerWindow int       `json:"freeExitChangesPerWindow"`
	ExitChangeWindowDays     int       `json:"exitChangeWindowDays"`
	UnpaidHoldHours          int       `json:"unpaidHoldHours"`
	MaxUnpaidNewOrders       int       `json:"maxUnpaidNewOrders"`
	MonthOptions             []int     `json:"monthOptions"`
	Payment                  Payment   `json:"payment"`
	SnapshotLines            []string  `json:"snapshotLines"`
}

var rulesVersionRe = regexp.MustCompile(`^R-[0-9]{4}-[0-9]{2}-[0-9]{2}(\.[0-9]+)?$`)

func rulesView(ctx context.Context, q kernel.Q, r *kernel.Rules) (RulesVersion, error) {
	v := RulesVersion{RulesInput: RulesInput{Version: r.Version, EffectiveFrom: r.EffectiveFrom, PeriodDays: r.PeriodDays,
		RetentionDays: r.RetentionDays, PortCooldownDays: r.PortCooldownDays, ResourceCooldownDays: r.ResourceCooldownDays,
		AccessChangesPerWindow: r.AccessChangesPerWindow, AccessChangeWindowHours: r.AccessChangeWindowHours,
		FreeExitChangesPerWindow: r.FreeExitChangesPerWindow, ExitChangeWindowDays: r.ExitChangeWindowDays,
		UnpaidHoldHours: r.UnpaidHoldHours, MaxUnpaidNewOrders: r.MaxUnpaidNewOrders, MonthOptions: []int{},
		Payment:       Payment{Payee: r.PaymentPayee, Bank: r.PaymentBank, Account: r.PaymentAccount, Note: r.PaymentNote},
		SnapshotLines: r.SnapshotLines}, CreatedAt: r.CreatedAt}
	if v.SnapshotLines == nil {
		v.SnapshotLines = []string{}
	}
	for _, m := range r.MonthOptions {
		v.MonthOptions = append(v.MonthOptions, int(m))
	}
	ref, err := kernel.LoadAdminRef(ctx, q, r.CreatedBy)
	v.CreatedBy = ref
	return v, err
}

// ListRules 列出规则版本（新的在前）。
func (s *Service) ListRules(ctx context.Context) ([]RulesVersion, error) {
	rows, err := s.env.Pool.Query(ctx, `SELECT `+kernel.RulesColumns()+` FROM rules ORDER BY effective_from DESC, version DESC`)
	if err != nil {
		return nil, err
	}
	var rs []*kernel.Rules
	for rows.Next() {
		r, err := kernel.ScanRules(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []RulesVersion{}
	for _, r := range rs {
		v, err := rulesView(ctx, s.env.Pool, r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// CreateRules 发布新规则版本（只追加；effectiveFrom 不早于当前时间）。
func (s *Service) CreateRules(ctx context.Context, adminID string, in RulesInput) (*RulesVersion, error) {
	now := s.env.Now()
	switch {
	case !rulesVersionRe.MatchString(in.Version):
		return nil, httpx.Invalid("规则版本号格式为 R-YYYY-MM-DD 或 R-YYYY-MM-DD.N")
	case in.EffectiveFrom.Before(now.Add(-time.Minute)):
		return nil, httpx.Invalid("生效时间不能早于当前时间")
	case in.PeriodDays < 1 || in.RetentionDays < 0 || in.PortCooldownDays < 0 || in.ResourceCooldownDays < 0 ||
		in.AccessChangesPerWindow < 0 || in.AccessChangeWindowHours < 1 || in.FreeExitChangesPerWindow < 0 ||
		in.ExitChangeWindowDays < 1 || in.UnpaidHoldHours < 1 || in.MaxUnpaidNewOrders < 1 || len(in.MonthOptions) == 0:
		return nil, httpx.Invalid("规则取值超出范围")
	}
	for _, m := range in.MonthOptions {
		if m < 1 {
			return nil, httpx.Invalid("月数选项必须为正")
		}
	}
	if in.SnapshotLines == nil {
		in.SnapshotLines = []string{}
	}
	if in.EffectiveFrom.Before(now) {
		in.EffectiveFrom = now
	}
	var v *RulesVersion
	err := s.env.InTx(ctx, func(tx *kernel.Tx) error {
		months := make([]int32, len(in.MonthOptions))
		for i, m := range in.MonthOptions {
			months[i] = int32(m)
		}
		_, err := tx.Exec(ctx, `INSERT INTO rules (version, effective_from, period_days, retention_days, port_cooldown_days,
		                          resource_cooldown_days, access_changes_per_window, access_change_window_hours,
		                          free_exit_changes_per_window, exit_change_window_days, unpaid_hold_hours, max_unpaid_new_orders,
		                          month_options, payment_payee, payment_bank, payment_account, payment_note, snapshot_lines,
		                          created_by, created_at)
		                        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
			in.Version, in.EffectiveFrom.UTC(), in.PeriodDays, in.RetentionDays, in.PortCooldownDays, in.ResourceCooldownDays,
			in.AccessChangesPerWindow, in.AccessChangeWindowHours, in.FreeExitChangesPerWindow, in.ExitChangeWindowDays,
			in.UnpaidHoldHours, in.MaxUnpaidNewOrders, months, in.Payment.Payee, in.Payment.Bank, in.Payment.Account,
			in.Payment.Note, in.SnapshotLines, adminID, tx.Now())
		if store.IsUniqueViolation(err, "") {
			return httpx.Conflict("规则版本号已存在")
		}
		if err != nil {
			return err
		}
		if err := tx.Audit(ctx, adminID, "rules.publish", "rules:"+in.Version, in); err != nil {
			return err
		}
		r, err := kernel.RulesByVersion(ctx, tx, in.Version)
		if err != nil {
			return err
		}
		rv, err := rulesView(ctx, tx, r)
		v = &rv
		return err
	})
	return v, err
}

// Routes 返回目录相关路由（用户端目录 + 管理端套餐、流量包、规则）。
func (s *Service) Routes() []httpx.Route {
	type createPlan struct {
		Code      string           `json:"code"`
		SortOrder int              `json:"sortOrder"`
		Version   PlanVersionInput `json:"version"`
	}
	type createPack struct {
		Code      string           `json:"code"`
		SortOrder int              `json:"sortOrder"`
		Version   PackVersionInput `json:"version"`
	}
	type sale struct {
		Status string `json:"status"`
	}
	ah := kernel.AdminH
	type ap = httpx.AdminPrincipal
	return []httpx.Route{
		{Method: "GET", Pattern: "/api/catalog", Auth: httpx.Public, Handler: func(w http.ResponseWriter, r *http.Request) {
			v, err := s.GetCatalog(r.Context())
			kernel.Reply(w, r, 200, v, err)
		}},
		{Method: "GET", Pattern: "/admin-api/plans", Auth: httpx.Admin, Handler: ah(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.ListPlans(r.Context())
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/plans", Auth: httpx.Admin, Handler: ah(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[createPlan](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.CreatePlan(r.Context(), a.AdminID, in.Code, in.SortOrder, in.Version)
			return 201, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/plans/{planId}", Auth: httpx.Admin, Handler: ah(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.loadPlan(r.Context(), s.env.Pool, r.PathValue("planId"), true)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/plans/{planId}/versions", Auth: httpx.Admin, Handler: ah(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[PlanVersionInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.CreatePlanVersion(r.Context(), a.AdminID, r.PathValue("planId"), in)
			return 201, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/plans/{planId}/status", Auth: httpx.Admin, Handler: ah(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[sale](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.SetPlanStatus(r.Context(), a.AdminID, r.PathValue("planId"), in.Status)
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/traffic-packs", Auth: httpx.Admin, Handler: ah(func(r *http.Request, _ ap) (int, any, error) {
			rows, err := s.env.Pool.Query(r.Context(), `SELECT id FROM traffic_packs ORDER BY sort_order, code`)
			if err != nil {
				return 0, nil, err
			}
			ids, err := collectStrings(rows)
			if err != nil {
				return 0, nil, err
			}
			out := []AdminPack{}
			for _, id := range ids {
				p, err := s.loadPack(r.Context(), s.env.Pool, id, false)
				if err != nil {
					return 0, nil, err
				}
				out = append(out, *p)
			}
			return 200, out, nil
		})},
		{Method: "POST", Pattern: "/admin-api/traffic-packs", Auth: httpx.Admin, Handler: ah(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[createPack](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.CreatePack(r.Context(), a.AdminID, in.Code, in.SortOrder, in.Version)
			return 201, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/traffic-packs/{trafficPackId}", Auth: httpx.Admin, Handler: ah(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.loadPack(r.Context(), s.env.Pool, r.PathValue("trafficPackId"), true)
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/traffic-packs/{trafficPackId}/versions", Auth: httpx.Admin, Handler: ah(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[PackVersionInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.CreatePackVersion(r.Context(), a.AdminID, r.PathValue("trafficPackId"), in)
			return 201, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/traffic-packs/{trafficPackId}/status", Auth: httpx.Admin, Handler: ah(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[sale](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.SetPackStatus(r.Context(), a.AdminID, r.PathValue("trafficPackId"), in.Status)
			return 200, v, err
		})},
		{Method: "GET", Pattern: "/admin-api/rules", Auth: httpx.Admin, Handler: ah(func(r *http.Request, _ ap) (int, any, error) {
			v, err := s.ListRules(r.Context())
			return 200, v, err
		})},
		{Method: "POST", Pattern: "/admin-api/rules", Auth: httpx.Admin, Handler: ah(func(r *http.Request, a ap) (int, any, error) {
			in, err := kernel.Body[RulesInput](r)
			if err != nil {
				return 0, nil, err
			}
			v, err := s.CreateRules(r.Context(), a.AdminID, in)
			return 201, v, err
		})},
	}
}
