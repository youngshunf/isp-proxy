package kernel

import (
	"context"
	"time"

	"github.com/go-gost/gost/internal/control/httpx"
)

// Rules 是一个规则版本（D-14a、D-18、D-19、收款说明）。
type Rules struct {
	Version                  string
	EffectiveFrom            time.Time
	PeriodDays               int
	RetentionDays            int
	PortCooldownDays         int
	ResourceCooldownDays     int
	AccessChangesPerWindow   int
	AccessChangeWindowHours  int
	FreeExitChangesPerWindow int
	ExitChangeWindowDays     int
	UnpaidHoldHours          int
	MaxUnpaidNewOrders       int
	MonthOptions             []int32
	PaymentPayee             string
	PaymentBank              string
	PaymentAccount           string
	PaymentNote              string
	SnapshotLines            []string
	CreatedBy                *string
	CreatedAt                time.Time
}

const rulesCols = `version, effective_from, period_days, retention_days, port_cooldown_days, resource_cooldown_days,
	access_changes_per_window, access_change_window_hours, free_exit_changes_per_window, exit_change_window_days,
	unpaid_hold_hours, max_unpaid_new_orders, month_options, payment_payee, payment_bank, payment_account, payment_note,
	snapshot_lines, created_by, created_at`

// ScanRules 扫描一行 rulesCols。
func ScanRules(row interface{ Scan(...any) error }) (*Rules, error) {
	r := &Rules{}
	err := row.Scan(&r.Version, &r.EffectiveFrom, &r.PeriodDays, &r.RetentionDays, &r.PortCooldownDays, &r.ResourceCooldownDays,
		&r.AccessChangesPerWindow, &r.AccessChangeWindowHours, &r.FreeExitChangesPerWindow, &r.ExitChangeWindowDays,
		&r.UnpaidHoldHours, &r.MaxUnpaidNewOrders, &r.MonthOptions, &r.PaymentPayee, &r.PaymentBank, &r.PaymentAccount,
		&r.PaymentNote, &r.SnapshotLines, &r.CreatedBy, &r.CreatedAt)
	return r, err
}

// RulesColumns 返回规则表列清单（供 catalog 包列表查询）。
func RulesColumns() string { return rulesCols }

// CurrentRules 返回 now 时生效的规则版本（effective_from <= now 中最新的一条）。
func CurrentRules(ctx context.Context, q Q, now time.Time) (*Rules, error) {
	r, err := ScanRules(q.QueryRow(ctx, `SELECT `+rulesCols+` FROM rules WHERE effective_from <= $1
	                                       ORDER BY effective_from DESC, version DESC LIMIT 1`, now))
	if IsNoRows(err) {
		return nil, httpx.Precondition("尚未发布生效的规则版本")
	}
	return r, err
}

// RulesByVersion 读取指定规则版本。
func RulesByVersion(ctx context.Context, q Q, version string) (*Rules, error) {
	return ScanRules(q.QueryRow(ctx, `SELECT `+rulesCols+` FROM rules WHERE version = $1`, version))
}

// PaymentConfigured 报告收款说明是否已配置（docs/06 §8.1：为空时拒绝下单）。
func (r *Rules) PaymentConfigured() bool {
	return r.PaymentPayee != "" && r.PaymentBank != "" && r.PaymentAccount != ""
}

// MonthAllowed 报告 m 是否在月数选项内。
func (r *Rules) MonthAllowed(m int) bool {
	for _, v := range r.MonthOptions {
		if int(v) == m {
			return true
		}
	}
	return false
}
