package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/go-gost/gost/internal/control/httpx"
	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/store/pgtest"
	"github.com/jackc/pgx/v5"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestRecordAndList(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	var adminID string
	if err := pool.QueryRow(ctx, `INSERT INTO admins (email, display_name, password_hash) VALUES ('a@example.com', 'A', 'x') RETURNING id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	actx := WithRequest(httpx.WithAdmin(ctx, httpx.AdminPrincipal{AdminID: adminID, Email: "a@example.com"}),
		RequestInfo{IP: "198.51.100.1", UserAgent: "ua", RequestID: "r1"})

	err := store.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := Record(actx, tx, Entry{Action: "subscription.pause", TargetType: "subscription", TargetID: "sub_x",
			Details: map[string]any{"reason": "滥用", "password": "p@ss", "nested": map[string]any{"totpSecret": "S"}}}); err != nil {
			return err
		}
		return Record(ctx, tx, Entry{Action: "admin.login_failed", Outcome: Denied, ActorKind: ActorAdmin, Details: map[string]any{"email": "x@example.com"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	// 业务事务回滚时审计一起回滚。
	_ = store.InTx(ctx, pool, func(tx pgx.Tx) error {
		_ = Record(actx, tx, Entry{Action: "order.confirm_payment"})
		return context.Canceled
	})
	if err := Record(ctx, pool, Entry{Action: "Bad Action"}); err == nil {
		t.Fatal("非法动作名应当被拒绝")
	}

	page, err := List(ctx, pool, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor != nil {
		t.Fatalf("%+v", page)
	}
	first, second := page.Items[0], page.Items[1]
	if first.Action != "admin.login_failed" || first.Actor.ID != nil || first.Outcome != Denied {
		t.Fatalf("%+v", first)
	}
	if second.Actor.Kind != ActorAdmin || *second.Actor.ID != adminID || second.Actor.Email != "a@example.com" ||
		*second.IP != "198.51.100.1" || *second.UserAgent != "ua" {
		t.Fatalf("%+v", second)
	}
	if d := string(second.Details); strings.Contains(d, "p@ss") || strings.Contains(d, `"S"`) || !strings.Contains(d, "滥用") {
		t.Fatalf("details 未脱敏: %s", d)
	}

	p1, err := List(ctx, pool, Filter{Limit: 1})
	if err != nil || len(p1.Items) != 1 || p1.NextCursor == nil {
		t.Fatalf("%+v %v", p1, err)
	}
	p2, err := List(ctx, pool, Filter{Limit: 1, Cursor: *p1.NextCursor})
	if err != nil || len(p2.Items) != 1 || p2.Items[0].Action != "subscription.pause" || p2.NextCursor != nil {
		t.Fatalf("%+v %v", p2, err)
	}
	p3, err := List(ctx, pool, Filter{Action: "subscription.", AdminID: adminID})
	if err != nil || len(p3.Items) != 1 {
		t.Fatalf("前缀筛选: %+v %v", p3, err)
	}
	if _, err := List(ctx, pool, Filter{Cursor: "zz"}); err != ErrBadCursor {
		t.Fatal(err)
	}
	// 只追加：库拒绝修改。
	if _, err := pool.Exec(ctx, `UPDATE audit_logs SET outcome = 'ok'`); err == nil {
		t.Fatal("audit_logs 应当只追加")
	}
}
