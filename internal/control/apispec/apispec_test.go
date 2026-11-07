package apispec

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/go-gost/gost/api"
	"gopkg.in/yaml.v3"
)

func loadDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatalf("openapi.yaml 不是有效的 YAML: %v", err)
	}
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Fatalf("应为 OpenAPI 3.1，得到 %q", v)
	}
	return doc
}

// resolve 按 JSON Pointer（RFC 6901）在文档内查找 ref。
func resolve(doc map[string]any, ref string) (any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("只允许文档内引用: %s", ref)
	}
	var cur any = doc
	for _, tok := range strings.Split(ref[2:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: %q 的上级不是对象", ref, tok)
		}
		if cur, ok = m[tok]; !ok {
			return nil, fmt.Errorf("%s: 找不到 %q", ref, tok)
		}
	}
	return cur, nil
}

func walk(v any, path string, fn func(path string, m map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(path, x)
		for k, c := range x {
			walk(c, path+"/"+k, fn)
		}
	case []any:
		for i, c := range x {
			walk(c, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}

func TestEveryRefResolves(t *testing.T) {
	doc := loadDoc(t)
	n := 0
	walk(doc, "", func(path string, m map[string]any) {
		ref, ok := m["$ref"]
		if !ok {
			return
		}
		n++
		s, _ := ref.(string)
		if _, err := resolve(doc, s); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	})
	if n < 100 {
		t.Fatalf("$ref 数量异常少（%d），检查是否解析到了正确的文件", n)
	}
	// 每个定义的 schema 都被引用（防止遗留死定义）。
	used := map[string]bool{}
	walk(doc, "", func(_ string, m map[string]any) {
		if s, ok := m["$ref"].(string); ok {
			used[s] = true
		}
	})
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for name := range schemas {
		if !used["#/components/schemas/"+name] {
			t.Errorf("schema %s 未被引用", name)
		}
	}
}

var pathParam = regexp.MustCompile(`\{([A-Za-z]+)\}`)

func TestOperationsAreWellFormed(t *testing.T) {
	doc := loadDoc(t)
	ids := map[string]string{}
	paths := doc["paths"].(map[string]any)
	for path, raw := range paths {
		item := raw.(map[string]any)
		if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/admin-api/") {
			t.Errorf("%s: 路径必须在 /api/ 或 /admin-api/ 下（agent 接口不在本文件）", path)
		}
		declared := map[string]bool{}
		collectParams := func(list any) {
			for _, p := range asList(list) {
				pm := deref(t, doc, p)
				if pm["in"] == "path" {
					declared[pm["name"].(string)] = true
					if pm["required"] != true {
						t.Errorf("%s: 路径参数 %v 必须 required", path, pm["name"])
					}
				}
			}
		}
		collectParams(item["parameters"])
		for _, m := range methods {
			raw, ok := item[m]
			if !ok {
				continue
			}
			op := raw.(map[string]any)
			where := strings.ToUpper(m) + " " + path
			id, _ := op["operationId"].(string)
			if id == "" {
				t.Errorf("%s: 缺少 operationId", where)
			} else if prev, dup := ids[id]; dup {
				t.Errorf("operationId %s 重复：%s 与 %s", id, prev, where)
			}
			ids[id] = where
			if _, ok := op["security"]; !ok {
				t.Errorf("%s: 必须显式声明 security（公开接口写 [{}]）", where)
			}
			if strings.HasPrefix(path, "/admin-api/") != strings.HasPrefix(id, "admin") {
				t.Errorf("%s: 管理端 operationId 以 admin 开头，用户端不以 admin 开头（%s）", where, id)
			}
			opDeclared := map[string]bool{}
			for k := range declared {
				opDeclared[k] = true
			}
			hasCSRF, hasIdem := false, false
			for _, p := range asList(op["parameters"]) {
				pm := deref(t, doc, p)
				switch {
				case pm["in"] == "path":
					opDeclared[pm["name"].(string)] = true
				case pm["name"] == "X-CSRF-Token":
					hasCSRF = true
				case pm["name"] == "Idempotency-Key":
					hasIdem = true
				}
			}
			for _, g := range pathParam.FindAllStringSubmatch(path, -1) {
				if !opDeclared[g[1]] {
					t.Errorf("%s: 路径参数 {%s} 未声明", where, g[1])
				}
			}
			if m != "get" && !hasCSRF {
				t.Errorf("%s: 非 GET 操作必须带 X-CSRF-Token 参数", where)
			}
			if m == "get" && hasCSRF {
				t.Errorf("%s: GET 不应要求 CSRF", where)
			}
			if mustIdem[id] && !hasIdem {
				t.Errorf("%s: 必须要求 Idempotency-Key", where)
			}
			resps, _ := op["responses"].(map[string]any)
			if len(resps) == 0 {
				t.Errorf("%s: 没有响应", where)
			}
			for code, r := range resps {
				rm := deref(t, doc, r)
				if rm["description"] == nil {
					t.Errorf("%s %s: 响应缺少 description", where, code)
				}
				if code >= "400" && !isErrorSchema(t, doc, rm) {
					t.Errorf("%s %s: 错误响应必须使用 Error schema（或以 allOf 扩展它）", where, code)
				}
			}
		}
	}
}

// 有账务或变更副作用、必须带幂等键的操作（docs/05 §5、docs/06 §3）。
var mustIdem = map[string]bool{
	"createOrder": true, "changeAccess": true, "createExitRequest": true,
	"adminCreateSubscription": true, "adminChangeAccess": true, "adminSubmitExitChange": true,
	"adminCreateLedgerEntry": true, "adminTransferBalance": true, "adminCreateOrder": true,
}

func isErrorSchema(t *testing.T, doc map[string]any, resp map[string]any) bool {
	content, _ := resp["content"].(map[string]any)
	js, _ := content["application/json"].(map[string]any)
	schema, _ := js["schema"].(map[string]any)
	ref, _ := schema["$ref"].(string)
	if ref == "#/components/schemas/Error" {
		return true
	}
	if ref == "" {
		return false
	}
	for _, part := range asList(deref(t, doc, schema)["allOf"]) {
		if pm, _ := part.(map[string]any); pm["$ref"] == "#/components/schemas/Error" {
			return true
		}
	}
	return false
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func deref(t *testing.T, doc map[string]any, v any) map[string]any {
	t.Helper()
	m, _ := v.(map[string]any)
	if ref, ok := m["$ref"].(string); ok {
		r, err := resolve(doc, ref)
		if err != nil {
			t.Fatal(err)
		}
		m, _ = r.(map[string]any)
	}
	return m
}

// 用户端与原型 PortalApi 一一对应（docs/05 §5）；额外的只有 confirmPasswordReset（原型缺失，F-W4 补）。
var portalAPI = []string{
	"session", "login", "register", "verifyEmail", "resendVerification", "requestPasswordReset", "changePassword", "logout",
	"listSubscriptions", "getSubscription", "listSubscriptionEvents", "renameSubscription", "changeAccess", "acknowledgeAccess",
	"getCatalog", "createOrder", "listOrders", "getOrder", "claimPaid", "cancelOrder",
	"listExitRequests", "createExitRequest",
}

func TestUserOperationsMatchPortalApi(t *testing.T) {
	rs, err := Routes()
	if err != nil {
		t.Fatal(err)
	}
	var user []string
	for _, r := range rs {
		if !r.Admin() {
			user = append(user, r.OperationID)
		}
	}
	want := append(slices.Clone(portalAPI), "confirmPasswordReset")
	slices.Sort(user)
	slices.Sort(want)
	if !slices.Equal(user, want) {
		t.Fatalf("用户端操作应与 PortalApi 一一对应\n得到: %v\n应为: %v", user, want)
	}
}

func TestBlockerOrderMatchesDisplayPriority(t *testing.T) {
	doc := loadDoc(t)
	b := deref(t, doc, map[string]any{"$ref": "#/components/schemas/Blocker"})
	got := fmt.Sprint(b["enum"])
	// docs/01 §7.2：封禁 > 已释放 > 暂停 > 待履约/履约异常 > 到期 > 耗尽 > 变更中 > 数据面异常。
	want := "[account_banned released admin_paused pending fulfill_failed expired traffic_exhausted changing dataplane]"
	if got != want {
		t.Fatalf("Blocker 枚举顺序即展示优先级\n得到 %s\n应为 %s", got, want)
	}
}

func TestRoutesHelper(t *testing.T) {
	ps, err := Paths()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"GET /api/me", "POST /api/sessions", "DELETE /api/sessions/current",
		"POST /api/subscriptions/{subscriptionId}/access-changes", "POST /api/exit-requests",
		"POST /admin-api/orders/{orderId}/payment-confirmation", "POST /admin-api/subscriptions/{subscriptionId}/exit-changes",
	} {
		if _, ok := ps[p]; !ok {
			t.Errorf("缺少 %s", p)
		}
	}
	// 模式能被 net/http ServeMux 接受且互不冲突（M2 直接用这些模式注册路由）。
	mux := http.NewServeMux()
	nop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ServeMux 拒绝契约路由: %v", r)
			}
		}()
		for p := range ps {
			mux.Handle(p, nop)
		}
	}()
	user, admin := 0, 0
	for _, r := range ps {
		if r.Admin() {
			admin++
		} else {
			user++
		}
	}
	t.Logf("契约操作：用户端 %d，管理端 %d", user, admin)
}
