//go:build ispe2e

package isp

import (
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/go-gost/gost/internal/control/admin"
)

// Admin 是管理端客户端（/admin-api），自带 TOTP 计算（测试从绑定时拿到的密钥算验证码）。
type Admin struct {
	*Client
	e        *Env
	Email    string
	Password string
	ID       string
	secret   []byte
	mu       sync.Mutex
	lastStep int64
}

func (e *Env) newAdmin(email, pass string) *Admin {
	return &Admin{Client: newClient(e.t, e.Base, "/admin-api"), e: e, Email: email, Password: pass}
}

// totp 返回一个未用过的时间步的验证码（同一时间步不能重复使用；允许 +1 步）。
func (a *Admin) totp() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		now := a.e.Now()
		step := admin.TOTPStep(now)
		if step <= a.lastStep {
			step = a.lastStep + 1
		}
		if step > admin.TOTPStep(now)+1 {
			time.Sleep(time.Second)
			continue
		}
		a.lastStep = step
		return admin.HOTP(a.secret, uint64(step), 6)
	}
}

// Login 完成两步登录；首次登录绑定 TOTP。
func (a *Admin) Login() {
	a.t.Helper()
	jarReset(a.Client)
	var ch AdminLoginChallenge
	a.Must("POST", "/sessions", map[string]string{"email": a.Email, "password": a.Password}, &ch)
	if ch.Stage == "enrollment_required" {
		sec, err := admin.DecodeSecret(ch.Enrollment.Secret)
		if err != nil {
			a.t.Fatal(err)
		}
		a.secret = sec
	}
	if a.secret == nil {
		a.t.Fatalf("管理员 %s 没有 TOTP 密钥（stage %s）", a.Email, ch.Stage)
	}
	var s AdminSession
	a.Must("POST", "/sessions/current/mfa", map[string]string{"code": a.totp()}, &s)
	a.ID = s.Admin.ID
}

func jarReset(c *Client) {
	nc := newClient(c.t, c.base, c.prefix)
	c.hc = nc.hc
}

// PublishRules 发布规则版本（version 为空时自动编号）。
func (a *Admin) PublishRules(r *RulesInput) {
	a.t.Helper()
	r.EffectiveFrom = a.e.Now().UTC()
	if r.Version == "" || r.Version == "R-2026-11-08.1" {
		r.Version = fmt.Sprintf("R-2026-11-08.%d", time.Now().UnixNano()%1_000_000_000)
	}
	a.Must("POST", "/rules", r, nil)
	// 迁移内置规则 R-2026-11-08 的生效时间固定为 2026-11-08T00:00Z（收款说明为空）。在此之前发布的规则会在该时刻
	// 被内置规则覆盖（按 effective_from 取最新），拨时钟的场景就会突然“暂未开放购买”。这里在内置规则之后再发布一份同内容的规则。
	if seed := time.Date(2026, 11, 8, 0, 0, 1, 0, time.UTC); r.EffectiveFrom.Before(seed) {
		again := *r
		again.Version, again.EffectiveFrom = r.Version+"1", seed
		a.Must("POST", "/rules", &again, nil)
	}
}

// CreateUpstream 录入上游。
func (a *Admin) CreateUpstream(in UpstreamInput) Upstream {
	a.t.Helper()
	var u Upstream
	a.Must("POST", "/upstreams", in, &u)
	return u
}

// Upstream 读取上游。
func (a *Admin) Upstream(id string) Upstream {
	a.t.Helper()
	var u Upstream
	a.Get("/upstreams/"+id, &u)
	return u
}

// VerifyUpstream 下发验证并等待探测完成；wantAvailable 时还要求资源进入 available。
func (a *Admin) VerifyUpstream(id string, wantAvailable bool) Probe {
	a.t.Helper()
	var p Probe
	a.Must("POST", "/upstreams/"+id+"/verify", nil, &p)
	p = a.WaitProbe(p.ID)
	if wantAvailable {
		a.e.Eventually(10*time.Second, func() bool { return a.Upstream(id).Status == "available" },
			"上游 %s 验证后未进入 available（探测 %+v）", id, p)
	}
	return p
}

// WaitProbe 等待探测完成（done 或 expired）。
func (a *Admin) WaitProbe(id string) Probe {
	a.t.Helper()
	var p Probe
	a.e.Eventually(30*time.Second, func() bool {
		a.Get("/probes/"+id, &p)
		return p.Status == "done" || p.Status == "expired"
	}, "探测 %s 未完成（%+v）", id, p)
	return p
}

// CreatePlan 新建并上架套餐。
func (a *Admin) CreatePlan(code string, v PlanVersionInput) AdminPlan {
	a.t.Helper()
	var p AdminPlan
	a.Must("POST", "/plans", map[string]any{"code": code, "version": v}, &p)
	a.Must("POST", "/plans/"+p.ID+"/status", map[string]string{"status": "on_sale"}, &p)
	return p
}

// CreatePack 新建并上架流量包。
func (a *Admin) CreatePack(code string, bytes, cents int64) AdminPack {
	a.t.Helper()
	var p AdminPack
	a.Must("POST", "/traffic-packs", map[string]any{"code": code, "version": map[string]any{"bytes": bytes, "price": CNY(cents)}}, &p)
	a.Must("POST", "/traffic-packs/"+p.ID+"/status", map[string]string{"status": "on_sale"}, &p)
	return p
}

// Sub 读取订阅详情。
func (a *Admin) Sub(id string) AdminSubscription {
	a.t.Helper()
	var s AdminSubscription
	a.Get("/subscriptions/"+id, &s)
	return s
}

// Ledger 读取账本。
func (a *Admin) Ledger(id string) Ledger {
	a.t.Helper()
	var l Ledger
	a.Get("/subscriptions/"+id+"/ledger", &l)
	return l
}

// Order 读取订单。
func (a *Admin) Order(id string) Order {
	a.t.Helper()
	var o Order
	a.Get("/orders/"+id, &o)
	return o
}

// Confirm 确认收款（金额取订单金额）。
func (a *Admin) Confirm(orderID string) Order {
	a.t.Helper()
	o := a.Order(orderID)
	var out Order
	a.Must("POST", "/orders/"+orderID+"/payment-confirmation", ConfirmPaymentInput{Method: "manual_transfer", Amount: o.Amount,
		ExternalRef: "BANK-" + o.No}, &out)
	return out
}

// Ledger 调整（adjust 可正可负；gift/compensation 为正）。
func (a *Admin) AddLedger(subID, source, kind string, amount int64, reason string) (LedgerEntry, error) {
	var le LedgerEntry
	_, err := a.Do("POST", "/subscriptions/"+subID+"/ledger-entries", map[string]any{"sourceType": source,
		"entitlementKind": kind, "amount": amount, "reason": reason}, &le, Idem(NewKey()))
	return le, err
}

// SetRemainingBytes 用 adjust 条目把剩余流量调到 n 字节（只追加账本，不覆盖余额）。
func (a *Admin) SetRemainingBytes(subID string, n int64) {
	a.t.Helper()
	s := a.Sub(subID)
	delta := n - (s.Traffic.TotalBytes - s.Traffic.UsedBytes)
	if delta == 0 {
		return
	}
	if _, err := a.AddLedger(subID, "adjust", "traffic", delta, "E2E 调整剩余流量"); err != nil {
		a.t.Fatal(err)
	}
}

// ExpireIn 用 adjust 条目把到期时间调到约 d 之后（d 可为负：已到期）。
func (a *Admin) ExpireIn(subID string, d time.Duration) time.Time {
	a.t.Helper()
	s := a.Sub(subID)
	if s.Time.ExpiresAt == nil {
		a.t.Fatalf("订阅 %s 尚未起算", subID)
	}
	target := a.e.Now().Add(d)
	delta := int64(target.Sub(*s.Time.ExpiresAt).Seconds())
	if delta != 0 {
		if _, err := a.AddLedger(subID, "adjust", "time", delta, "E2E 调整到期时间"); err != nil {
			a.t.Fatal(err)
		}
	}
	return *a.Sub(subID).Time.ExpiresAt
}

// Task 读取任务。
func (a *Admin) Task(id string) Task {
	a.t.Helper()
	var tk Task
	a.Get("/tasks/"+id, &tk)
	return tk
}

// WaitTask 等待任务结束。
func (a *Admin) WaitTask(id string) Task {
	a.t.Helper()
	var tk Task
	a.e.Eventually(45*time.Second, func() bool { tk = a.Task(id); return tk.Done() }, "任务 %s 未结束（%+v）", id, tk)
	return tk
}

// CompareCandidate 下发候选比较并等待结果。
func (a *Admin) CompareCandidate(subID, resourceID string) (Probe, error) {
	var p Probe
	if _, err := a.Do("POST", "/subscriptions/"+subID+"/exit-candidates", map[string]string{"resourceId": resourceID}, &p); err != nil {
		return p, err
	}
	return a.WaitProbe(p.ID), nil
}

// SubmitExitChange 提交换出口（可组合更换）。
func (a *Admin) SubmitExitChange(subID, resourceID, probeID string, alsoAccess bool, exitReq string) (Task, error) {
	var tk Task
	body := map[string]any{"resourceId": resourceID, "compareProbeId": probeID, "alsoChangeAccess": alsoAccess, "reason": "E2E 换出口"}
	if exitReq != "" {
		body["exitRequestId"] = exitReq
	}
	_, err := a.Do("POST", "/subscriptions/"+subID+"/exit-changes", body, &tk, Idem(NewKey()))
	return tk, err
}

// ChangeExit 完整的换出口向导：比较 → 提交 → 等任务成功。
func (a *Admin) ChangeExit(subID, resourceID string, alsoAccess bool) Task {
	a.t.Helper()
	p, err := a.CompareCandidate(subID, resourceID)
	if err != nil {
		a.t.Fatal(err)
	}
	if p.Comparison == nil || !p.Comparison.Passed {
		a.t.Fatalf("候选 %s 比较未通过：%+v %+v", resourceID, p, p.Comparison)
	}
	tk, err := a.SubmitExitChange(subID, resourceID, p.ID, alsoAccess, "")
	if err != nil {
		a.t.Fatal(err)
	}
	tk = a.WaitTask(tk.ID)
	if tk.State != "succeeded" {
		a.t.Fatalf("换出口任务失败：%+v %s %s", tk, deref(tk.FailureCode), deref(tk.FailureReason))
	}
	return tk
}

// Reveal 查看接入明文（审计）。
func (a *Admin) Reveal(subID string) Access {
	a.t.Helper()
	var acc Access
	a.Must("POST", "/subscriptions/"+subID+"/access/reveal", map[string]string{"reason": "E2E 核对"}, &acc)
	return acc
}

// Alerts 列出告警。
func (a *Admin) Alerts(q url.Values) []Alert {
	a.t.Helper()
	var p Page[Alert]
	a.Get("/alerts?"+q.Encode(), &p)
	return p.Items
}

// UserID 按邮箱查用户 ID。
func (a *Admin) UserID(email string) string {
	a.t.Helper()
	var p Page[AdminUser]
	a.Get("/users?q="+url.QueryEscape(email), &p)
	for _, u := range p.Items {
		if u.Email == email {
			return u.ID
		}
	}
	a.t.Fatalf("没有找到用户 %s", email)
	return ""
}

func deref[T any](p *T) T {
	var z T
	if p == nil {
		return z
	}
	return *p
}
