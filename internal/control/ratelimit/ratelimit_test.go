package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/go-gost/gost/internal/control/store"
	"github.com/go-gost/gost/internal/control/store/pgtest"
	"github.com/jackc/pgx/v5"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestSlidingWindow(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	perMin := Rule{Scope: "mail_send", Limit: 1, Window: time.Minute}
	perHour := Rule{Scope: "mail_send_h", Limit: 5, Window: time.Hour}
	t0 := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)

	take := func(at time.Time) time.Duration {
		var w time.Duration
		if err := store.InTx(ctx, pool, func(tx pgx.Tx) error {
			var err error
			w, err = Take(ctx, tx, at, "a@example.com", perMin, perHour)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return w
	}
	if w := take(t0); w != 0 {
		t.Fatal(w)
	}
	if w := take(t0.Add(10 * time.Second)); w != 50*time.Second {
		t.Fatalf("60 秒内第二次应等待 50 秒，得到 %v", w)
	}
	for i := 1; i <= 4; i++ {
		if w := take(t0.Add(time.Duration(i) * 61 * time.Second)); w != 0 {
			t.Fatalf("第 %d 次: %v", i+1, w)
		}
	}
	// 一小时内已 5 次：第 6 次等第一条离开窗口。
	at := t0.Add(6 * 61 * time.Second)
	if w := take(at); w != t0.Add(time.Hour).Sub(at) {
		t.Fatalf("小时上限: %v", w)
	}
	if w := take(t0.Add(time.Hour + time.Second)); w != 0 {
		t.Fatalf("窗口滑出后应允许: %v", w)
	}
	// 其他 key 不受影响。
	var w time.Duration
	_ = store.InTx(ctx, pool, func(tx pgx.Tx) error {
		w, _ = Take(ctx, tx, at, "b@example.com", perMin, perHour)
		return nil
	})
	if w != 0 {
		t.Fatal(w)
	}
	n, err := Prune(ctx, pool, t0.Add(2*time.Hour))
	if err != nil || n != 14 {
		t.Fatalf("prune %d %v", n, err)
	}
}
