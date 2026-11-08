//go:build ispe2e

package isp

import (
	"sort"
	"strings"
	"testing"
)

// 城市可选（D-27、docs/06 §10）：套餐与上游的城市可以不填；不限城市的套餐由用户选城市或交给系统分配。

// A-53 不限城市的套餐：目录给出套餐名与有货城市；选了城市只分到该城市的出口，该城市无货 409；
// 固定城市的套餐选别的城市 422；不选城市可分到没有城市的出口，经真实入口可用。
func TestA53_OptionalCityPurchase(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 1, Extra: []ExitSpec{
		{Label: "ny-1", City: "New York"},
		{Label: "nocity-1", City: "-"},
	}})
	if c := e.Admin.Upstream(e.Exit("nocity-1").ResourceID).City; c != nil {
		t.Fatalf("录入时不填城市应为 null：%q", *c)
	}
	pv := DefaultPlanVersion()
	pv.Name = "Pro · 美国住宅 E2E"
	pv.City = ""
	anyPlan := e.Admin.CreatePlan("us-any-"+randHex(2), pv)

	section(t, "目录：套餐名、不限城市与有货城市")
	c := e.NewUser().Catalog()
	p := catalogPlan(c, anyPlan.ID)
	var cities []string
	for _, x := range p.Cities {
		cities = append(cities, x.Name)
	}
	if p.Name != pv.Name || p.City != nil || strings.Join(cities, ",") != "Los Angeles,New York" || p.Stock == "out" {
		t.Fatalf("不限城市套餐：%+v", p)
	}
	if f := catalogPlan(c, e.Plan.ID); f.Name != DefaultPlanVersion().Name || f.City == nil || *f.City != "Los Angeles" || len(f.Cities) != 0 {
		t.Fatalf("固定城市套餐：%+v", f)
	}

	section(t, "选城市：只分到该城市的出口，订单明细与订阅带上城市")
	u1 := e.NewUser()
	s1, o1 := e.PurchaseWith(u1, OrderInput{Kind: "new", PlanID: anyPlan.ID, Months: 1, Quantity: 1, City: "New York"})
	if got := e.CurrentExitLabel(s1.ID); got != "ny-1" {
		t.Fatalf("选纽约应分到 ny-1，得到 %s", got)
	}
	if len(o1.Lines) == 0 || !strings.Contains(o1.Lines[0].Detail, "城市：New York") {
		t.Fatalf("订单明细应写上所选城市：%+v", o1.Lines)
	}
	if s1.Plan == nil || s1.Plan.City == nil || *s1.Plan.City != "New York" {
		t.Fatalf("订阅视图城市：%+v", s1.Plan)
	}
	if _, err := e.NewUser().Order(OrderInput{Kind: "new", PlanID: anyPlan.ID, Months: 1, Quantity: 1, City: "New York"}, ""); StatusOf(err) != 409 {
		t.Fatalf("纽约已无货应 409：%v", err)
	}
	if _, err := e.NewUser().Order(OrderInput{Kind: "new", PlanID: e.Plan.ID, Months: 1, Quantity: 1, City: "New York"}, ""); StatusOf(err) != 422 {
		t.Fatalf("固定城市套餐选别的城市应 422：%v", err)
	}

	section(t, "不选城市：可分到没有城市的出口，经入口可用")
	var labels []string
	for range 2 {
		s, _ := e.PurchaseWith(e.NewUser(), OrderInput{Kind: "new", PlanID: anyPlan.ID, Months: 1, Quantity: 1})
		if s.Plan == nil || s.Plan.City != nil {
			t.Fatalf("不选城市时订阅城市为 null：%+v", s.Plan)
		}
		labels = append(labels, e.CurrentExitLabel(s.ID))
	}
	sort.Strings(labels)
	if strings.Join(labels, ",") != "exit-A,nocity-1" {
		t.Fatalf("不限城市应用完该国家剩余出口（含没有城市的）：%v", labels)
	}
	if st := catalogPlan(e.NewUser().Catalog(), anyPlan.ID).Stock; st != "out" {
		t.Fatalf("全部用完后应无货：%s", st)
	}
	if e.CanaryHits() != 0 {
		t.Fatalf("不应发生直连：%d", e.CanaryHits())
	}
}
