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
var notYetImplemented = map[string]bool{}

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
