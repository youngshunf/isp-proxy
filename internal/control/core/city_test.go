package core_test

// 城市可选（D-27，docs/06 §10）：套餐与上游资源的城市可以为空；用户可在不限城市的套餐里选城市。
// 匹配规则：有效城市 = 套餐固定城市 > 用户选择 > 不限；不限匹配该国家全部资源（含没有城市的），城市 X 只匹配 city = X。

import (
	"strings"
	"testing"
	"time"
)

// cityResource 录入一个可售资源（绑定上限 1，只给独享）；city 为空串时资源没有城市。
func (x *h) cityResource(city string) string {
	x.t.Helper()
	id, _ := x.newResource(x.node, "us", "洛杉矶", "available")
	x.exec(`UPDATE upstream_resources SET max_tenants = 1 WHERE id = $1`, id)
	if city == "" {
		x.exec(`UPDATE upstream_resources SET city = NULL WHERE id = $1`, id)
	} else {
		x.exec(`UPDATE upstream_resources SET city = $2 WHERE id = $1`, id, city)
	}
	return id
}

type planResp struct {
	ID      string `json:"id"`
	Current struct {
		Name string  `json:"name"`
		City *string `json:"city"`
	} `json:"current"`
}

// anyCityPlan 经管理端接口新建并上架一个不限城市的套餐（不传 city）。
func (x *h) anyCityPlan(code, mode string) string {
	x.t.Helper()
	var p planResp
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/plans", map[string]any{"code": code, "exitMode": mode, "version": map[string]any{
		"name": "Pro · 美国不限城市", "countryCode": "us", "country": "美国", "price": map[string]any{"amount": 3000, "currency": "CNY"},
		"periodDays": 30, "includedBytes": 10 * gb, "concurrency": 10, "bandwidthMbps": 50}}), 201, &p)
	if p.Current.City != nil {
		x.t.Fatalf("不传城市应为不限城市（null），得到 %q", *p.Current.City)
	}
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/plans/"+p.ID+"/status", map[string]any{"status": "on_sale"}), 200, nil)
	return p.ID
}

// newOrder 用户新购下单（不确认收款）；city 为 nil 时不传。
func (x *h) newOrder(user, plan string, city *string) resp {
	in := map[string]any{"kind": "new", "planId": plan, "months": 1, "quantity": 1}
	if city != nil {
		in["city"] = *city
	}
	return x.do(asUser(user), "POST", "/api/orders", in, x.key())
}

type orderResp struct {
	ID     string `json:"id"`
	Amount struct {
		Amount int64 `json:"amount"`
	} `json:"amount"`
}

// reservedBy 返回订单预留的独享资源。
func (x *h) reservedBy(order string) string {
	return x.str(`SELECT id FROM upstream_resources WHERE reserved_order_id = $1`, order)
}

func (x *h) orderCity(order string) string {
	return x.str(`SELECT city FROM orders WHERE id = $1`, order)
}

// subOfOrder 返回确认收款后新购订单创建的订阅。
func (x *h) subOfOrder(order string) string {
	return x.str(`SELECT subscription_id FROM orders WHERE id = $1`, order)
}

func strp(s string) *string { return &s }

// boundRes 返回订阅当前出口所在的资源；没有出口绑定时为空串。
func (x *h) boundRes(sub string) string {
	return x.str(`SELECT COALESCE((SELECT resource_id FROM exit_bindings WHERE subscription_id = $1 AND state IN ('pending', 'active')
	               ORDER BY state = 'active' DESC LIMIT 1), '')`, sub)
}

// A-53a：固定城市的套餐只用该城市的资源；选别的城市 422；同城无货 409（即使别的城市或没有城市的资源有货）。
func TestA53a_FixedCityPlanUsesOnlyItsCity(t *testing.T) {
	x := newH(t)
	u := x.newUser("a53a@example.com")
	x.cityResource("纽约")
	x.cityResource("")
	x.fail(x.newOrder(u, x.plan, nil), 409, "conflict")
	x.fail(x.newOrder(u, x.plan, strp("纽约")), 422, "invalid")
	x.fail(x.newOrder(u, x.plan, strp("洛杉矶")), 409, "conflict")
	la := x.cityResource("洛杉矶")
	var o orderResp
	x.ok(x.newOrder(u, x.plan, strp(" 洛杉矶 ")), 201, &o)
	if x.reservedBy(o.ID) != la || x.orderCity(o.ID) != "洛杉矶" {
		t.Fatalf("应预留洛杉矶资源并记下有效城市：%s %q", x.reservedBy(o.ID), x.orderCity(o.ID))
	}
	if d := x.str(`SELECT detail FROM order_lines WHERE order_id = $1 AND item_kind = 'plan'`, o.ID); strings.Contains(d, "城市：") {
		t.Fatalf("固定城市的套餐不另写所选城市：%s", d)
	}
	x.ok(x.confirm(o.ID, o.Amount.Amount, "REF-"+o.ID, false), 200, nil)
	sub := x.subOfOrder(o.ID)
	if x.str(`SELECT city FROM subscriptions WHERE id = $1`, sub) != "洛杉矶" || x.resOf(sub) != la {
		t.Fatal("订阅应记下有效城市并分到洛杉矶资源")
	}
	x.apply(x.node)
	v := x.sub(u, sub)
	if v.Plan == nil || v.Plan.City == nil || *v.Plan.City != "洛杉矶" {
		t.Fatalf("订阅视图的城市：%+v", v.Plan)
	}
}

// A-53b：不限城市的套餐不选城市可分到任何城市的资源，包括没有城市的资源；选了城市只在该城市内预留，
// 永远不会分到没有城市的资源；该城市无货 409。下单明细写上所选城市，订阅保存有效城市。
func TestA53b_AnyCityPlanAndChosenCity(t *testing.T) {
	x := newH(t)
	anyPlan := x.anyCityPlan("us-any", "exclusive")
	u := x.newUser("a53b@example.com")
	nul := x.cityResource("")
	// 只有没有城市的资源：不选城市可买到它；选了城市就无货。
	x.fail(x.newOrder(u, anyPlan, strp("洛杉矶")), 409, "conflict")
	var o1 orderResp
	x.ok(x.newOrder(u, anyPlan, nil), 201, &o1)
	if x.reservedBy(o1.ID) != nul {
		t.Fatal("不限城市应能分到没有城市的资源")
	}
	if x.str(`SELECT (city IS NULL)::text FROM orders WHERE id = $1`, o1.ID) != "true" {
		t.Fatal("不选城市时订单城市为空")
	}
	x.ok(x.confirm(o1.ID, o1.Amount.Amount, "REF-"+o1.ID, false), 200, nil)
	s1 := x.subOfOrder(o1.ID)
	if x.str(`SELECT (city IS NULL)::text FROM subscriptions WHERE id = $1`, s1) != "true" || x.resOf(s1) != nul {
		t.Fatal("订阅城市应为空并用上没有城市的资源")
	}
	// 选城市：多个没有城市的资源与一个洛杉矶资源，只会预留洛杉矶的。
	for range 3 {
		x.cityResource("")
	}
	la := x.cityResource("洛杉矶")
	x.cityResource("纽约")
	u2 := x.newUser("a53b2@example.com")
	var o2 orderResp
	x.ok(x.newOrder(u2, anyPlan, strp("洛杉矶")), 201, &o2)
	if x.reservedBy(o2.ID) != la || x.orderCity(o2.ID) != "洛杉矶" {
		t.Fatal("选了洛杉矶应只预留洛杉矶资源")
	}
	if d := x.str(`SELECT detail FROM order_lines WHERE order_id = $1 AND item_kind = 'plan'`, o2.ID); !strings.Contains(d, "，城市：洛杉矶") {
		t.Fatalf("明细应写上所选城市：%s", d)
	}
	// 洛杉矶已被预留完：再选洛杉矶 409，不会落到没有城市或其他城市的资源。
	x.fail(x.newOrder(u2, anyPlan, strp("洛杉矶")), 409, "conflict")
	x.fail(x.newOrder(u2, anyPlan, strp("芝加哥")), 409, "conflict")
	x.ok(x.confirm(o2.ID, o2.Amount.Amount, "REF-"+o2.ID, false), 200, nil)
	s2 := x.subOfOrder(o2.ID)
	if x.str(`SELECT city FROM subscriptions WHERE id = $1`, s2) != "洛杉矶" || x.resOf(s2) != la {
		t.Fatal("订阅应保存所选城市并用上预留的资源")
	}
	// 预留过期后确认收款：履约按订阅城市重新挑选（只剩没有城市的资源和纽约，洛杉矶已占用）→ 交付异常而不是分错城市。
	u3 := x.newUser("a53b3@example.com")
	la2 := x.cityResource("洛杉矶")
	var o3 orderResp
	x.ok(x.newOrder(u3, anyPlan, strp("洛杉矶")), 201, &o3)
	if x.reservedBy(o3.ID) != la2 {
		t.Fatal("应预留新录入的洛杉矶资源")
	}
	x.exec(`UPDATE upstream_resources SET status = 'quarantine', status_reason = '测试', reserved_order_id = NULL, reserved_until = NULL
	         WHERE id = $1`, la2)
	x.ok(x.confirm(o3.ID, o3.Amount.Amount, "REF-"+o3.ID, false), 200, nil)
	s3 := x.subOfOrder(o3.ID)
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, s3); lc != "fulfill_failed" || x.boundRes(s3) != "" {
		t.Fatalf("所选城市无货应记为交付异常，不能分到其他城市：%s %s", lc, x.boundRes(s3))
	}
	// 管理员代建同样可以指定城市。
	var cs struct {
		Subscription struct{ ID string } `json:"subscription"`
	}
	x.cityResource("纽约")
	x.exec(`UPDATE upstream_resources SET status = 'retired' WHERE country_code = 'us' AND city IS DISTINCT FROM '纽约' AND status = 'available'`)
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions", map[string]any{"userId": u3, "planId": anyPlan, "months": 1, "city": "纽约",
		"payment": map[string]any{"method": "manual_transfer", "amount": map[string]any{"amount": 3000, "currency": "CNY"},
			"externalRef": "BANK-" + x.key()}}, x.key()), 201, &cs)
	if r := x.resOf(cs.Subscription.ID); r == "" || x.str(`SELECT city FROM upstream_resources WHERE id = $1`, r) != "纽约" {
		t.Fatal("代建指定纽约应分到纽约的资源")
	}
	x.fail(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions", map[string]any{"userId": u3, "planId": x.plan, "months": 1, "city": "纽约",
		"payment": map[string]any{"method": "manual_transfer", "amount": map[string]any{"amount": 3000, "currency": "CNY"},
			"externalRef": "BANK-" + x.key()}}, x.key()), 422, "invalid")
}

type catalogPlan struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	City   *string `json:"city"`
	Stock  string  `json:"stock"`
	Cities []struct {
		Name  string `json:"name"`
		Stock string `json:"stock"`
	} `json:"cities"`
}

func (x *h) catalogPlans() map[string]catalogPlan {
	x.t.Helper()
	var c struct {
		Plans []catalogPlan `json:"plans"`
	}
	x.ok(x.do(who{}, "GET", "/api/catalog", nil), 200, &c)
	m := map[string]catalogPlan{}
	for _, p := range c.Plans {
		m[p.ID] = p
	}
	return m
}

// A-53c：目录给出套餐名称；不限城市的套餐列出当前有货的城市（只含有城市的资源，按出口类型计），
// 整体库存按整个国家计；固定城市的套餐 cities 为空。库存告警对不限城市单独计。
func TestA53c_CatalogNameAndCities(t *testing.T) {
	x := newH(t)
	anyPlan := x.anyCityPlan("us-any", "exclusive")
	shared := x.anyCityPlan("us-any-shared", "shared")
	for range 4 {
		x.cityResource("洛杉矶")
	}
	x.cityResource("纽约")
	x.cityResource("")
	busy := x.cityResource("芝加哥")
	x.exec(`UPDATE upstream_resources SET status = 'quarantine', status_reason = '测试' WHERE id = $1`, busy)
	sh := x.cityResource("西雅图")
	x.exec(`UPDATE upstream_resources SET max_tenants = 5 WHERE id = $1`, sh)
	cat := x.catalogPlans()
	fixed, a, s := cat[x.plan], cat[anyPlan], cat[shared]
	if fixed.Name != "美国洛杉矶" || fixed.City == nil || *fixed.City != "洛杉矶" || len(fixed.Cities) != 0 || fixed.Stock != "plenty" {
		t.Fatalf("固定城市套餐：%+v", fixed)
	}
	if a.Name != "Pro · 美国不限城市" || a.City != nil || a.Stock != "plenty" {
		t.Fatalf("不限城市套餐：%+v", a)
	}
	got := []string{}
	for _, c := range a.Cities {
		got = append(got, c.Name+"="+c.Stock)
	}
	// 纽约只有 1 个（low），洛杉矶 4 个（plenty），西雅图上限 5 的共享资源空闲时也可整份给独享；芝加哥隔离不列。
	if want := []string{"洛杉矶=plenty", "纽约=low", "西雅图=low"}; !eq(got, want) {
		t.Fatalf("可选城市期望 %v，得到 %v", want, got)
	}
	if len(s.Cities) != 1 || s.Cities[0].Name != "西雅图" || s.Cities[0].Stock != "plenty" || s.Stock != "plenty" {
		t.Fatalf("共享不限城市只列有空位的共享资源城市：%+v", s)
	}
	// 库存告警：不限城市的套餐去重键城市段为 *，提示写“不限城市”。
	x.exec(`UPDATE upstream_resources SET status = 'retired' WHERE status = 'available'`)
	x.tick()
	if m := x.str(`SELECT message FROM alerts WHERE dedupe_key = 'stock_low:us:*' AND status = 'open'`); !strings.Contains(m, "不限城市") {
		t.Fatalf("不限城市套餐的库存告警：%q", m)
	}
	if n := x.int(`SELECT count(*) FROM alerts WHERE dedupe_key = 'stock_low:us:洛杉矶' AND status = 'open'`); n != 1 {
		t.Fatal("固定城市套餐的库存告警不变")
	}
}

// A-53d：选了城市的订阅释放后续费，重新履约只在保存的城市内挑选；存量订阅（订阅城市为空）回落到套餐城市，行为不变。
func TestA53d_RefulfillWithinStoredCity(t *testing.T) {
	x := newH(t)
	anyPlan := x.anyCityPlan("us-any", "exclusive")
	u := x.newUser("a53d@example.com")
	x.cityResource("纽约")
	o := x.buy(u, map[string]any{"kind": "new", "planId": anyPlan, "months": 1, "quantity": 1, "city": "纽约"})
	sub := x.subOfOrder(o)
	// 存量订阅：固定城市套餐，订阅城市置空模拟迁移前的数据。
	ul := x.newUser("a53d-legacy@example.com")
	x.resource()
	legacy, _ := x.createSub(ul, 1)
	x.exec(`UPDATE subscriptions SET city = NULL WHERE id = $1`, legacy)
	x.apply(x.node)
	for _, s := range []string{sub, legacy} {
		if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, s); lc != "active" {
			t.Fatalf("应已就绪：%s", lc)
		}
	}
	x.clk.Set(x.expires(sub).Add(time.Second))
	x.tick()
	x.clk.Advance(7*24*time.Hour + time.Second)
	x.tick()
	for _, s := range []string{sub, legacy} {
		if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, s); lc != "released" {
			t.Fatalf("应已释放：%s", lc)
		}
	}
	// 只有其他城市与没有城市的资源：续费后重新履约无货（交付异常），不会换城市。
	x.cityResource("洛杉矶")
	x.cityResource("")
	x.renew(u, sub)
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, sub); lc != "fulfill_failed" {
		t.Fatalf("纽约无货应交付异常：%s", lc)
	}
	ny := x.cityResource("纽约")
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+sub+"/fulfillment", map[string]any{}), 200, nil)
	if x.resOf(sub) != ny {
		t.Fatal("重新履约应分到纽约的资源")
	}
	// 存量订阅回落到套餐版本的城市（洛杉矶）：只剩没有城市的资源时无货。
	x.exec(`UPDATE upstream_resources SET status = 'retired' WHERE city = '洛杉矶' AND status = 'available'`)
	x.renew(ul, legacy)
	if lc := x.str(`SELECT lifecycle FROM subscriptions WHERE id = $1`, legacy); lc != "fulfill_failed" {
		t.Fatalf("存量订阅应仍按套餐城市挑选：%s", lc)
	}
	la := x.cityResource("洛杉矶")
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/subscriptions/"+legacy+"/fulfillment", map[string]any{}), 200, nil)
	if x.resOf(legacy) != la {
		t.Fatal("存量订阅应分到洛杉矶的资源")
	}
}

type upstreamCity struct {
	ID         string  `json:"id"`
	City       *string `json:"city"`
	RowVersion int64   `json:"rowVersion"`
}

// A-53e：录入上游可以不填城市；修改时 null 或空串清除城市，缺省不改。套餐发布新版本时空城市即不限城市。
func TestA53e_UpstreamAndPlanCityOptional(t *testing.T) {
	x := newH(t)
	var u upstreamCity
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/upstreams", map[string]any{"url": "socks5://up:pw@127.0.0.1:41001", "nodeId": x.node,
		"countryCode": "us", "country": "美国"}), 201, &u)
	if u.City != nil {
		t.Fatalf("不填城市应为 null：%q", *u.City)
	}
	var u2 upstreamCity
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/upstreams", map[string]any{"url": "socks5://up:pw@127.0.0.1:41002", "nodeId": x.node,
		"countryCode": "us", "country": "美国", "city": "   "}), 201, &u2)
	if u2.City != nil {
		t.Fatal("空白城市应归一为 null")
	}
	patch := func(body map[string]any) upstreamCity {
		t.Helper()
		body["rowVersion"] = u.RowVersion
		x.ok(x.do(x.asAdmin(), "PATCH", "/admin-api/upstreams/"+u.ID, body), 200, &u)
		return u
	}
	if v := patch(map[string]any{"city": " 洛杉矶 "}); v.City == nil || *v.City != "洛杉矶" {
		t.Fatal("应设置城市")
	}
	if v := patch(map[string]any{"note": "改备注"}); v.City == nil || *v.City != "洛杉矶" {
		t.Fatal("缺省不改城市")
	}
	if v := patch(map[string]any{"city": nil}); v.City != nil {
		t.Fatal("null 应清除城市")
	}
	patch(map[string]any{"city": "纽约"})
	if v := patch(map[string]any{"city": ""}); v.City != nil {
		t.Fatal("空串应清除城市")
	}
	if n := x.int(`SELECT count(*) FROM upstream_resources WHERE id = $1 AND city IS NULL`, u.ID); n != 1 {
		t.Fatal("库里应为 NULL")
	}
	x.fail(x.do(x.asAdmin(), "PATCH", "/admin-api/upstreams/"+u.ID, map[string]any{"rowVersion": u.RowVersion, "city": 3}), 422, "invalid")
	// 发布新版本：空城市 = 不限城市；固定城市照旧。
	var p planResp
	x.ok(x.do(x.asAdmin(), "POST", "/admin-api/plans/"+x.plan+"/versions", map[string]any{"name": "美国不限", "countryCode": "us",
		"country": "美国", "city": "", "price": map[string]any{"amount": 3000, "currency": "CNY"}, "periodDays": 30, "includedBytes": 10 * gb,
		"concurrency": 10, "bandwidthMbps": 50}), 201, &p)
	if p.Current.City != nil {
		t.Fatal("空城市的新版本应为不限城市")
	}
	if x.catalogPlans()[x.plan].City != nil {
		t.Fatal("目录应显示当前版本不限城市")
	}
	// 数据库兜底：城市有值时不能是空白。
	if _, err := x.pool.Exec(x.ctx, `UPDATE upstream_resources SET city = ' ' WHERE id = $1`, u.ID); err == nil {
		t.Fatal("空白城市应被约束拒绝")
	}
}
