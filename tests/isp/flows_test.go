//go:build ispe2e

package isp

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 常用业务流程：开通、购买、等待可用、经入口访问。

// ProvisionOpts 调整代建开通。
type ProvisionOpts struct {
	Months     int
	ResourceID string
	Name       string
	PlanID     string
	Key        string
}

// AdminProvision 由管理员代建并开通（赠送，金额 0），返回结果（不等待就绪）。
func (e *Env) AdminProvision(u *User, o ProvisionOpts) (AdminCreateSubscriptionResult, error) {
	if o.Months == 0 {
		o.Months = 1
	}
	if o.PlanID == "" {
		o.PlanID = e.Plan.ID
	}
	if o.Key == "" {
		o.Key = NewKey()
	}
	body := map[string]any{"userId": u.ID, "planId": o.PlanID, "months": o.Months,
		"payment": ConfirmPaymentInput{Method: "admin_gift", Amount: CNY(0), ExternalRef: "E2E 赠送"}}
	if o.ResourceID != "" {
		body["resourceId"] = o.ResourceID
	}
	if o.Name != "" {
		body["name"] = o.Name
	}
	var res AdminCreateSubscriptionResult
	_, err := e.Admin.Do("POST", "/subscriptions", body, &res, Idem(o.Key))
	return res, err
}

// Provision 代建开通并等待可用，返回用户视图的订阅。
func (e *Env) Provision(u *User, o ProvisionOpts) Subscription {
	e.t.Helper()
	res, err := e.AdminProvision(u, o)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.WaitUsable(u, res.Subscription.ID)
}

// Purchase 用户自助下单新购 → 管理员确认收款 → 等待可用。
func (e *Env) Purchase(u *User, months int) (Subscription, Order) {
	e.t.Helper()
	o, err := u.Order(OrderInput{Kind: "new", PlanID: e.Plan.ID, Months: months, Quantity: 1}, "")
	if err != nil {
		e.t.Fatal(err)
	}
	e.Admin.Confirm(o.ID)
	var sid string
	e.Eventually(20*time.Second, func() bool {
		var got Order
		u.Get("/orders/"+o.ID, &got)
		if got.SubscriptionID != nil {
			sid = *got.SubscriptionID
			o = got
			return true
		}
		return false
	}, "订单 %s 确认后没有关联订阅", o.No)
	return e.WaitUsable(u, sid), o
}

// Renew 用户下续时长（或买流量）订单并由管理员确认。
func (e *Env) Renew(u *User, in OrderInput) Order {
	e.t.Helper()
	o, err := u.Order(in, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return e.Admin.Confirm(o.ID)
}

// WaitUsable 等待订阅可用：服务端 blockers 为空、接入已签发，且 HTTP CONNECT 经入口能到达出口。
func (e *Env) WaitUsable(u *User, id string) Subscription {
	e.t.Helper()
	var s Subscription
	e.Eventually(30*time.Second, func() bool {
		s = u.Sub(id)
		return len(s.Blockers) == 0 && s.Access != nil && s.Lifecycle == "active"
	}, "订阅 %s 未就绪：lifecycle=%s blockers=%v", id, s.Lifecycle, s.Blockers)
	deadline := time.Now().Add(15 * time.Second)
	for {
		body, _, err := Fetch(Connect, CredsOf(s.Access), e.Target(), "/ip")
		if err == nil && body != "DIRECT" {
			return s
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("订阅 %s 已显示可用，但入口不通：%q %v", id, body, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// WaitSub 轮询用户视图直到 cond 为真。
func (e *Env) WaitSub(u *User, id string, max time.Duration, cond func(*Subscription) bool, what string) Subscription {
	e.t.Helper()
	var s Subscription
	e.Eventually(max, func() bool { s = u.Sub(id); return cond(&s) }, "订阅 %s：等待 %s 超时（lifecycle=%s blockers=%v traffic=%+v）",
		id, what, s.Lifecycle, s.Blockers, s.Traffic)
	return s
}

// WaitAdminSub 轮询管理端视图直到 cond 为真。
func (e *Env) WaitAdminSub(id string, max time.Duration, cond func(*AdminSubscription) bool, what string) AdminSubscription {
	e.t.Helper()
	var s AdminSubscription
	e.Eventually(max, func() bool { s = e.Admin.Sub(id); return cond(&s) }, "订阅 %s：等待 %s 超时（lifecycle=%s blockers=%v traffic=%+v applied=%d desired=%d）",
		id, what, s.Lifecycle, s.Blockers, s.Traffic, s.AppliedVersion, s.DesiredVersion)
	return s
}

// WaitApplied 等待节点应用到订阅的当前期望版本。
func (e *Env) WaitApplied(id string) AdminSubscription {
	e.t.Helper()
	return e.WaitAdminSub(id, 20*time.Second, func(s *AdminSubscription) bool {
		return s.AppliedVersion >= s.DesiredVersion && s.DesiredVersion > 0
	}, "节点应用期望版本")
}

// Target 是客户请求的目标地址（金丝雀端点：只有直连才会真正到达它）。
func (e *Env) Target() string { return e.Canary.Addr }

// Reach 经入口请求 /ip，返回目标侧看到的出口标签。
func (e *Env) Reach(m Mode, c Creds) (string, error) {
	body, _, err := Fetch(m, c, e.Target(), "/ip")
	return body, err
}

// MustReach 断言四种接入方式都经 label 出口到达目标。
func (e *Env) MustReach(c Creds, label string, modes ...Mode) {
	e.t.Helper()
	if len(modes) == 0 {
		modes = AllModes
	}
	for _, m := range modes {
		var body string
		var err error
		// 节点刚应用新版本时可能有一次握手落在切换瞬间，短暂重试。
		for i := 0; i < 20; i++ {
			if body, err = e.Reach(m, c); err == nil && body == label {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil || body != label {
			e.t.Fatalf("%s 经 %s:%d 期望出口 %s，得到 %q（%v）", m, c.Host, portOf(c, m), label, body, err)
		}
	}
}

// MustReject 断言该接入在所有给定方式上被明确拒绝。
func (e *Env) MustReject(c Creds, why string, modes ...Mode) {
	e.t.Helper()
	if len(modes) == 0 {
		modes = AllModes
	}
	for _, m := range modes {
		body, err := e.Reach(m, c)
		if err == nil {
			e.t.Fatalf("%s：%s 应被拒绝，却到达 %q", why, m, body)
		}
		if !IsRejected(err) {
			e.t.Fatalf("%s：%s 没有明确拒绝（%v）", why, m, err)
		}
	}
}

func portOf(c Creds, m Mode) int {
	switch m {
	case HTTPSEntry:
		return c.Ports.HTTPS
	case SOCKS:
		return c.Ports.SOCKS5
	}
	return c.Ports.HTTP
}

// CurrentExitLabel 返回订阅当前出口资源对应的标签。
func (e *Env) CurrentExitLabel(id string) string {
	e.t.Helper()
	s := e.Admin.Sub(id)
	if s.Exit == nil {
		return ""
	}
	return e.LabelOf(s.Exit.ResourceID)
}

// FreeExit 返回一个当前 available、且不在 exclude 中的默认出口。
func (e *Env) FreeExit(exclude ...string) *FakeExit {
	e.t.Helper()
	skip := map[string]bool{}
	for _, x := range exclude {
		skip[x] = true
	}
	for _, x := range e.Exits {
		if skip[x.ResourceID] || skip[x.Label] {
			continue
		}
		if e.Admin.Upstream(x.ResourceID).Status == "available" {
			return x
		}
	}
	e.t.Fatal("没有空闲的可售出口")
	return nil
}

func hasEvent(evs []SubscriptionEvent, kind string) bool {
	for _, ev := range evs {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

// section 在日志里标出场景内的一段（场景内不用子测试：环境的辅助函数绑定在顶层测试上）。
func section(t *testing.T, name string) { t.Helper(); t.Logf("==== %s ====", name) }

func fmtBlockers(b []string) string { return "[" + strings.Join(b, ",") + "]" }

var _ = fmt.Sprintf
