//go:build ispe2e

package isp

import (
	"sync"
	"testing"
)

// A-01 录入有效上游并开通，生成唯一订阅、首个接入和三种连接；重复提交不多建资源。
func TestA01_ProvisionUniqueSubscription(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 2})
	u := e.NewUser()

	// 重复录入同一上游（协议 + 主机 + 端口 + 用户名）返回 409，不多建资源。
	if _, err := e.Admin.Do("POST", "/upstreams", e.UpstreamInputFor(e.Exits[0]), nil); StatusOf(err) != 409 {
		t.Fatalf("重复录入上游应 409：%v", err)
	}
	var ups Page[Upstream]
	e.Admin.Get("/upstreams?limit=200", &ups)
	if len(ups.Items) != len(e.Exits) {
		t.Fatalf("上游数 %d，期望 %d", len(ups.Items), len(e.Exits))
	}

	// 同一幂等键并发重复提交开通：只建一个订单、一个订阅。
	key := NewKey()
	res := make([]AdminCreateSubscriptionResult, 6)
	errs := make([]error, len(res))
	var wg sync.WaitGroup
	for i := range res {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = e.AdminProvision(u, ProvisionOpts{Key: key, ResourceID: e.Exits[0].ResourceID, Name: "A01"})
		}(i)
	}
	wg.Wait()
	var subID, orderID string
	for i := range res {
		if errs[i] != nil {
			// 并发同键请求允许其中一部分以 409（正在处理）返回，但不能建出第二个订阅。
			if StatusOf(errs[i]) != 409 {
				t.Fatalf("重复提交 %d：%v", i, errs[i])
			}
			continue
		}
		if subID == "" {
			subID, orderID = res[i].Subscription.ID, res[i].Order.ID
		}
		if res[i].Subscription.ID != subID || res[i].Order.ID != orderID {
			t.Fatalf("同一幂等键得到不同订阅/订单：%s/%s vs %s/%s", res[i].Subscription.ID, res[i].Order.ID, subID, orderID)
		}
	}
	if subID == "" {
		t.Fatal("没有一次开通成功")
	}
	// 重试（同键、同体）返回首次结果。
	again, err := e.AdminProvision(u, ProvisionOpts{Key: key, ResourceID: e.Exits[0].ResourceID, Name: "A01"})
	if err != nil || again.Subscription.ID != subID {
		t.Fatalf("同键重试：%v %s", err, again.Subscription.ID)
	}
	// 同键不同体返回 409。
	if _, err := e.AdminProvision(u, ProvisionOpts{Key: key, ResourceID: e.Exits[0].ResourceID, Name: "A01-other"}); StatusOf(err) != 409 {
		t.Fatalf("同键不同请求体应 409：%v", err)
	}

	s := e.WaitUsable(u, subID)
	if subs := u.Subs(); len(subs) != 1 {
		t.Fatalf("用户应只有 1 个订阅，得到 %d", len(subs))
	}
	if s.Access == nil || s.Access.Version != 1 {
		t.Fatalf("首个接入应为版本 1：%+v", s.Access)
	}
	p := s.Access.Ports
	if p.HTTP == 0 || p.HTTPS == 0 || p.SOCKS5 == 0 || p.HTTP == p.HTTPS || p.HTTP == p.SOCKS5 || p.HTTPS == p.SOCKS5 {
		t.Fatalf("三种连接应有三个不同端口：%+v", p)
	}
	for _, port := range []int{p.HTTP, p.HTTPS, p.SOCKS5} {
		if port < e.Ports[0] || port > e.Ports[1] {
			t.Fatalf("端口 %d 不在节点端口池 %v 内", port, e.Ports)
		}
	}
	var vers []map[string]any
	e.Admin.Get("/subscriptions/"+subID+"/access-versions", &vers)
	if len(vers) != 1 {
		t.Fatalf("接入版本应只有 1 个：%v", vers)
	}
	// 资源被该订阅占用，且只占用一个。
	up := e.Admin.Upstream(e.Exits[0].ResourceID)
	if up.Status != "bound" || up.BoundSub == nil || *up.BoundSub != subID {
		t.Fatalf("资源状态：%+v", up)
	}
	if other := e.Admin.Upstream(e.Exits[1].ResourceID); other.Status != "available" {
		t.Fatalf("未使用的资源不应被占用：%+v", other)
	}
	// 三种连接都可用并经绑定资源出口。
	e.MustReach(CredsOf(s.Access), e.Exits[0].Label)
	o := e.Admin.Order(orderID)
	if len(o.LedgerEntries) != 2 {
		t.Fatalf("新购订单应入账时长与流量各一条：%+v", o.LedgerEntries)
	}
}

// A-02 分别以 HTTP 普通转发、HTTP CONNECT、HTTPS 入口、SOCKS5 访问目标；出口均为绑定资源。
func TestA02_FourEntryModesUseBoundExit(t *testing.T) {
	t.Parallel()
	e := NewEnv(t, EnvOptions{Exits: 3}) // http、socks5、https 三种上游
	u := e.NewUser()
	for i, x := range e.Exits {
		s := e.Provision(u, ProvisionOpts{ResourceID: x.ResourceID})
		c := CredsOf(s.Access)
		for _, m := range AllModes {
			body, _, err := Fetch(m, c, e.Target(), "/ip")
			if err != nil || body != x.Label {
				t.Fatalf("订阅 %d（上游 %s）经 %s：%q %v", i, x.Protocol, m, body, err)
			}
		}
		if got := e.CurrentExitLabel(s.ID); got != x.Label {
			t.Fatalf("管理端出口资源 %s 与实际出口 %s 不符", got, x.Label)
		}
		// 用户视图的出口 IP 来自检测，等于该上游的回显 IP。
		e.WaitSub(u, s.ID, 15e9, func(s *Subscription) bool { return s.Exit != nil && s.Exit.IP == x.IP }, "出口检测 IP")
	}
	if e.CanaryHits() != 0 {
		t.Fatalf("目标被直连 %d 次（应经上游）", e.CanaryHits())
	}
}
