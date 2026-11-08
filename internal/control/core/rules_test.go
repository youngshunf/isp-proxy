package core_test

import (
	"testing"
	"time"
)

// 规则版本：立即生效的版本马上成为当前版本；尚未生效的版本可以撤回，已生效的不能撤回也不能改（docs/06 §11）。
func TestRulesPublishNowAndWithdrawPending(t *testing.T) {
	x := newH(t)
	var list []map[string]any
	x.ok(x.do(x.asAdmin(), "GET", "/admin-api/rules", nil), 200, &list)
	base := list[0]
	if base["version"] != "R-2026-11-09" {
		t.Fatalf("种子规则不对：%v", base["version"])
	}
	body := func(version string, from time.Time) map[string]any {
		b := map[string]any{}
		for k, v := range base {
			if k != "createdAt" && k != "createdBy" {
				b[k] = v
			}
		}
		b["version"], b["effectiveFrom"] = version, from.Format(time.RFC3339Nano)
		return b
	}
	current := func() string {
		var c struct {
			Rules struct {
				Version string `json:"version"`
			} `json:"rules"`
		}
		x.ok(x.do(who{}, "GET", "/api/catalog", nil), 200, &c)
		return c.Rules.Version
	}

	// 立即生效：生效时间取发布时刻（略早于服务端时钟也按当前时刻处理）。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/rules", body("R-2026-11-10", t0.Add(-30*time.Second))), 201, nil)
	if v := current(); v != "R-2026-11-10" {
		t.Fatalf("立即生效的版本没有成为当前版本：%s", v)
	}

	// 定时生效（远期，避免与数据库真实时钟比较时过期）：不影响当前版本，可以撤回。
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/rules", body("R-2026-11-10.1", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))), 201, nil)
	if v := current(); v != "R-2026-11-10" {
		t.Fatalf("待生效版本不应成为当前版本：%s", v)
	}
	x.ok(x.do(x.asAdmin(), "DELETE", "/admin-api/rules/R-2026-11-10.1", nil), 204, nil)
	if n := x.int(`SELECT count(*) FROM rules WHERE version = 'R-2026-11-10.1'`); n != 0 {
		t.Fatal("撤回后版本仍在")
	}
	if !x.audit.has("rules.withdraw") {
		t.Fatal("撤回没有审计")
	}
	x.fail(x.do(x.asAdmin(), "DELETE", "/admin-api/rules/R-2026-11-10.1", nil), 404, "not_found")

	// 已生效的版本不能撤回；数据库层也拒绝改动和删除。
	x.fail(x.do(x.asAdmin(), "DELETE", "/admin-api/rules/R-2026-11-10", nil), 409, "conflict")
	if _, err := x.pool.Exec(x.ctx, `DELETE FROM rules WHERE version = 'R-2026-11-08'`); err == nil {
		t.Fatal("数据库应拒绝删除已生效的规则版本")
	}
	if _, err := x.pool.Exec(x.ctx, `UPDATE rules SET payment_note = 'x' WHERE version = 'R-2026-11-10'`); err == nil {
		t.Fatal("数据库应拒绝修改规则版本")
	}
}
