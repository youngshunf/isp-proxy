package api_test

import (
	"context"
	"testing"
)

// ctl-core 的管理端路由经同一路由器鉴权，审计经 kernel.Auditor 适配写入 audit_logs；viewer 同样只读。
func TestCoreRoutesWiredWithAudit(t *testing.T) {
	e := newEnv(t)
	expect(t, e.client().do("GET", "/admin-api/nodes", nil), 401, "unauthorized")
	a, _ := e.newAdmin("ops@example.com", "admin")
	r := a.do("POST", "/admin-api/nodes", map[string]any{"name": "node-a", "publicHost": "proxy.example.test",
		"portRange": map[string]int{"start": 20000, "end": 20009}})
	expect(t, r, 201, "")
	var n struct {
		ID string `json:"id"`
	}
	r.json(t, &n)
	var actor, target string
	if err := e.pool.QueryRow(context.Background(), `SELECT l.actor_kind || ':' || a.email, l.target_type || ':' || l.target_id
		FROM audit_logs l JOIN admins a ON a.id = l.admin_id WHERE l.action = 'node.create'`).Scan(&actor, &target); err != nil {
		t.Fatal(err)
	}
	if actor != "admin:ops@example.com" || target != "node:"+n.ID {
		t.Fatalf("%s %s", actor, target)
	}
	v, _ := e.newAdmin("viewer@example.com", "viewer")
	expect(t, v.do("GET", "/admin-api/nodes", nil), 200, "")
	expect(t, v.do("POST", "/admin-api/nodes", map[string]any{"name": "node-b", "publicHost": "b.example.test"}), 403, "forbidden")
	// 目录访客可读（不要求登录；具体内容取决于当前规则版本，由 catalog 包自己的测试覆盖）。
	if r := e.client().do("GET", "/api/catalog", nil); r.status == 401 || r.status == 403 {
		t.Fatalf("目录应当公开: %d %s", r.status, r.body)
	}
}
