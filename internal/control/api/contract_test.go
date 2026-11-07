package api_test

import (
	"os"
	"sort"
	"testing"

	"github.com/go-gost/gost/internal/control/apispec"
	"github.com/go-gost/gost/internal/control/httpx"
)

// notYetImplemented 是契约中已定义、但实现尚未接入的路由（全部属于 ctl-core：订阅、订单、目录、换出口申请、
// 上游库存、套餐与规则、节点与告警、概览）。接入一个就删一个；控制面完成时必须为空
// （设置 ISP_REQUIRE_COMPLETE_API=1 时 TestContractComplete 强制检查）。
var notYetImplemented = map[string]bool{
	"GET /admin-api/alerts": true,
	"GET /admin-api/exit-requests": true,
	"GET /admin-api/exit-requests/{exitRequestId}": true,
	"GET /admin-api/nodes": true,
	"GET /admin-api/nodes/{nodeId}": true,
	"GET /admin-api/orders": true,
	"GET /admin-api/orders/{orderId}": true,
	"GET /admin-api/overview": true,
	"GET /admin-api/plans": true,
	"GET /admin-api/plans/{planId}": true,
	"GET /admin-api/probes/{probeId}": true,
	"GET /admin-api/refunds": true,
	"GET /admin-api/rules": true,
	"GET /admin-api/subscriptions": true,
	"GET /admin-api/subscriptions/{subscriptionId}": true,
	"GET /admin-api/subscriptions/{subscriptionId}/access-versions": true,
	"GET /admin-api/subscriptions/{subscriptionId}/events": true,
	"GET /admin-api/subscriptions/{subscriptionId}/exit-bindings": true,
	"GET /admin-api/subscriptions/{subscriptionId}/ledger": true,
	"GET /admin-api/subscriptions/{subscriptionId}/tasks": true,
	"GET /admin-api/subscriptions/{subscriptionId}/usage": true,
	"GET /admin-api/tasks/{taskId}": true,
	"GET /admin-api/traffic-packs": true,
	"GET /admin-api/traffic-packs/{trafficPackId}": true,
	"GET /admin-api/upstreams": true,
	"GET /admin-api/upstreams/{upstreamId}": true,
	"GET /api/catalog": true,
	"GET /api/exit-requests": true,
	"GET /api/orders": true,
	"GET /api/orders/{orderId}": true,
	"GET /api/subscriptions": true,
	"GET /api/subscriptions/{subscriptionId}": true,
	"GET /api/subscriptions/{subscriptionId}/events": true,
	"PATCH /admin-api/exit-requests/{exitRequestId}": true,
	"PATCH /admin-api/nodes/{nodeId}": true,
	"PATCH /admin-api/subscriptions/{subscriptionId}": true,
	"PATCH /admin-api/upstreams/{upstreamId}": true,
	"PATCH /api/subscriptions/{subscriptionId}": true,
	"POST /admin-api/alerts/{alertId}/ack": true,
	"POST /admin-api/exit-requests/{exitRequestId}/accept": true,
	"POST /admin-api/exit-requests/{exitRequestId}/complete": true,
	"POST /admin-api/exit-requests/{exitRequestId}/reject": true,
	"POST /admin-api/nodes": true,
	"POST /admin-api/orders": true,
	"POST /admin-api/orders/{orderId}/cancel": true,
	"POST /admin-api/orders/{orderId}/fulfillment-retry": true,
	"POST /admin-api/orders/{orderId}/payment-confirmation": true,
	"POST /admin-api/orders/{orderId}/refunds": true,
	"POST /admin-api/plans": true,
	"POST /admin-api/plans/{planId}/status": true,
	"POST /admin-api/plans/{planId}/versions": true,
	"POST /admin-api/refunds/{refundId}/decision": true,
	"POST /admin-api/rules": true,
	"POST /admin-api/subscriptions": true,
	"POST /admin-api/subscriptions/{subscriptionId}/access-changes": true,
	"POST /admin-api/subscriptions/{subscriptionId}/access/reveal": true,
	"POST /admin-api/subscriptions/{subscriptionId}/deletion": true,
	"POST /admin-api/subscriptions/{subscriptionId}/exit-candidates": true,
	"POST /admin-api/subscriptions/{subscriptionId}/exit-changes": true,
	"POST /admin-api/subscriptions/{subscriptionId}/exit-checks": true,
	"POST /admin-api/subscriptions/{subscriptionId}/fulfillment": true,
	"POST /admin-api/subscriptions/{subscriptionId}/ledger-entries": true,
	"POST /admin-api/subscriptions/{subscriptionId}/pause": true,
	"POST /admin-api/subscriptions/{subscriptionId}/resume": true,
	"POST /admin-api/traffic-packs": true,
	"POST /admin-api/traffic-packs/{trafficPackId}/status": true,
	"POST /admin-api/traffic-packs/{trafficPackId}/versions": true,
	"POST /admin-api/transfers": true,
	"POST /admin-api/upstreams": true,
	"POST /admin-api/upstreams/{upstreamId}/status": true,
	"POST /admin-api/upstreams/{upstreamId}/verify": true,
	"POST /api/exit-requests": true,
	"POST /api/orders": true,
	"POST /api/orders/{orderId}/cancel": true,
	"POST /api/orders/{orderId}/claim-paid": true,
	"POST /api/subscriptions/{subscriptionId}/access-changes": true,
	"POST /api/subscriptions/{subscriptionId}/access/ack": true,
}

// TestContractRoutes 断言：已注册路由 ∪ 暂缺清单 = OpenAPI 操作集合，且两者不相交、暂缺清单里没有契约外的路由。
func TestContractRoutes(t *testing.T) {
	spec, err := apispec.Paths()
	if err != nil {
		t.Fatal(err)
	}
	p := newEnv(t).platform
	registered := map[string]httpx.Route{}
	for _, r := range p.Routes {
		registered[r.Method+" "+r.Pattern] = r
	}
	var extra, missing, stale []string
	for k := range registered {
		if _, ok := spec[k]; !ok {
			extra = append(extra, k)
		}
		if notYetImplemented[k] {
			stale = append(stale, k)
		}
	}
	for k := range spec {
		if _, ok := registered[k]; !ok && !notYetImplemented[k] {
			missing = append(missing, k)
		}
	}
	for k := range notYetImplemented {
		if _, ok := spec[k]; !ok {
			t.Errorf("暂缺清单中的 %s 不在契约里", k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	sort.Strings(stale)
	for _, k := range extra {
		t.Errorf("注册了契约外的路由: %s", k)
	}
	for _, k := range missing {
		t.Errorf("契约中的路由没有实现: %s", k)
	}
	for _, k := range stale {
		t.Errorf("%s 已实现，请从 notYetImplemented 删除", k)
	}
	// 授权级别与契约 security 一致：管理端只有登录两步是公开的。
	for k, r := range registered {
		sr := spec[k]
		if sr.Admin() && r.Auth == httpx.Public && k != "POST /admin-api/sessions" && k != "POST /admin-api/sessions/current/mfa" {
			t.Errorf("%s: 管理端路由不应公开", k)
		}
	}
	// 未验证用户不能购买或获取连接（R-15.1、A-23）：用户端除账号与目录外的路由都必须是 VerifiedUser。
	for k, r := range registered {
		sr := spec[k]
		if sr.Admin() {
			continue
		}
		switch {
		case hasTag(sr, "account"):
		case hasTag(sr, "catalog"):
			if r.Auth != httpx.Public {
				t.Errorf("%s: 目录访客可读，应为 Public", k)
			}
		default:
			if r.Auth != httpx.VerifiedUser {
				t.Errorf("%s: 应为 VerifiedUser（当前 %s）", k, r.Auth)
			}
		}
	}
	t.Logf("契约 %d 个操作：已实现 %d，暂缺 %d", len(spec), len(registered), len(notYetImplemented))
}

// TestContractComplete 在控制面完成时（ISP_REQUIRE_COMPLETE_API=1）要求暂缺清单为空。
func TestContractComplete(t *testing.T) {
	if len(notYetImplemented) == 0 {
		return
	}
	if os.Getenv("ISP_REQUIRE_COMPLETE_API") == "1" {
		t.Fatalf("还有 %d 个契约路由未实现", len(notYetImplemented))
	}
	t.Skipf("还有 %d 个契约路由未实现（ctl-core）；设置 ISP_REQUIRE_COMPLETE_API=1 时视为失败", len(notYetImplemented))
}

func hasTag(r apispec.Route, tag string) bool {
	for _, t := range r.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
